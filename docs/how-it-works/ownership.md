# Ownership

Dolly records what it manages in the target LDAP itself, under `target.state_base`, using only the standard core schema — no schema changes are needed on the target server.

```text
ou=dolly,dc=local
├── ou=users
│   └── cn=<objectGUID>   objectClass: organizationalRole
│                         seeAlso: uid=jdoe,ou=people,dc=local
├── ou=groups
│   └── cn=<objectGUID>   objectClass: organizationalRole
│                         seeAlso: cn=hpc-users,ou=group,dc=local
│                         roleOccupant: uid=jdoe,ou=people,dc=local   (one per member Dolly added, always a DN)
├── cn=lock                                                              (only while a run is active)
└── cn=status             last successful run, current failure, last notification
```

Each record's RDN is `cn=<objectGUID>` — the AD object's `objectGUID`, formatted as a canonical hyphenated string (AD returns it as 16 mixed-endian bytes, so Dolly normalizes it once to a single consistent format). `seeAlso` points at the managed target entry. On group records, `roleOccupant` lists the members Dolly itself added to that group, always as a DN built as `<rdn>=<uid>,<users_base>` (for example `uid=jdoe,ou=people,dc=local`), even when the group's schema uses `memberUid` only; `memberUid` ownership is derived from those DNs rather than tracked separately. With `memberUid` only, that DN is just an identifier: the user entry must exist somewhere under `users_base` with that uid (see [Required attributes](#required-attributes) below), but not necessarily at that DN. User records also carry small `description` values in `key=value` form:

- `missing-since` — an RFC 3339 UTC timestamp (for example `missing-since=2026-09-21T19:00:00Z`) recording when a user went missing from AD.
- `saved-shell` — the shell to restore after a disabled account is re-enabled.
- `renaming-from=<old DN>` — present only while a rename is in progress; see [Renames](#renames).

Dolly creates `state_base` and its children if they're missing. It never creates `users_base` or `groups_base` — if those don't exist, it stops with a clear error.

## Group membership

- A user in the AD group but not in the target group is added to the target group and recorded as Dolly-owned.
- A Dolly-owned member who left the AD group, or was deleted from AD, is removed from the target group and from the record.
- Membership is never replaced wholesale. Dolly never computes "the target group's members = AD's list"; it adds and removes individual `member` and `memberUid` values, and only ever touches the attributes listed in the mapping.
- With `member` in `mapping.groups.membership` (`groupOfNames`), a new AD group with no resolvable members isn't created until it has one. With `memberUid` only, `posixGroup` may be empty, so the group is created right away — see [Group schema](../configuration.md#group-schema-rfc2307bis-or-rfc-2307).
- A groups-only run (`--groups`) never writes user entries, so `users_base` may be read-only for Dolly.
- **Members must exist on the target.** This is always on; there's no setting for it. An AD user is added to a target group only if an entry with its uid exists under `users_base`, Dolly-owned or local (with `member` in the membership list, the entry must be at the member DN). A user created earlier in the same run counts; a user pruned in the same run doesn't, so a pruned user's former memberships are never re-added. If no such entry exists, Dolly skips that member. This matters most in a groups-only deployment (see [Groups-only deployments](../configuration.md#groups-only-deployments)), where Dolly never creates users and the users that go into a target group must already exist there: neither the AD user nor the target entry needs `uidNumber` for this check (the lookup matches on `uid` only), and only groups need `gidNumber`. Skipped members aren't warnings and never trigger a notification: the run summary shows one count line, `members skipped: N not on the target (use --debug to list them)` (omitted when zero), and `--debug` prints one `debug: skip <uid> in <group>: …` line per skipped (group, user) pair to stderr — see [Commands](../commands.md#the-debug-flag). A skipped member is re-evaluated every run and added (and recorded as Dolly-owned) once its entry exists. A Dolly-owned member whose entry has disappeared is left in place, neither re-added nor removed, until the user leaves the AD group.
- In a `--groups` run, a member's target DN is read from the user's own ownership record (`seeAlso`), not recomputed from a fresh AD-to-target mapping. A user rename that's still pending (its own `--users` sync hasn't run yet) therefore causes no group churn — the group keeps pointing at the DN the record already has.
- **Manual removal.** A member value removed by hand from the target is re-added on the next run only if Dolly owns that membership (the group's ownership record lists it in `roleOccupant`, because it came from AD) and the AD group still lists the user — for a Dolly-owned member, AD wins. A local member removed by hand is never restored; that's the admin's business, not Dolly's. Known consequence, kept without extra state: a local member who is *also* in the AD group, once removed by hand, comes back as a Dolly-owned member on the next run, because Dolly can't distinguish that from a fresh AD membership.

## Local wins

A member already present in a target group without an ownership record was added locally, on the LDAP server directly. Dolly never removes that member — even if the same user also happens to be in the AD group. Local additions are permanent until removed by hand, with one exception: [pruning](#deleted-and-pruned-users) a user removes their local memberships too, to avoid a dangling reference to a deleted entry.

## Conflicts

A target user or group that matches an AD entry by name (`uid` or `cn`) but has no ownership record is a conflict: something with that name already existed on the target before Dolly touched it, or was created outside Dolly. Dolly logs an error and leaves the entry untouched rather than guessing. A conflicting user is still eligible to be a group member by DN — group membership doesn't require an owned user entry — so it can appear in `member`/`roleOccupant` lists even though its own entry is left alone. `dolly adopt`, run once, is the only thing that claims existing unowned entries — see [Adopting an existing tree](../operations/adopting.md).

## Duplicate uid

When two AD users map to the same `uid`, Dolly picks one winner and logs the other as a warning. The order of preference:

1. the current owner — an existing ownership record already points at this AD user;
2. failing that, any AD user with an ownership record;
3. failing that, an in-scope AD user;
4. failing that, the one with the lowest DN.

## Renames

Identity is `objectGUID`, not name. A user or group renamed in AD (same `objectGUID`, different name) becomes a `modrdn` on the target, followed by updating every `member` and `memberUid` value that referenced the old DN, and the `seeAlso` and `roleOccupant` values in its own and any group's ownership records.

This happens in a crash-safe order: the ownership record is updated first, adding a `renaming-from=<old DN>` note; only then does Dolly perform the `modrdn` and the `member`/`memberUid`/`seeAlso`/`roleOccupant` fix-ups; the note is removed last. If Dolly crashes mid-rename, the note alone is enough to resume or recover on the next run.

Renames happen in place — Dolly never moves an entry to a different parent under the target tree. An owned entry that turns up somewhere other than directly under its configured base (`users_base` or `groups_base`) is reported as a conflict, not moved.

If a rename would collide with an entry that already exists at the new name, that's also a conflict: Dolly logs it and skips the rename.

## Deleted and pruned users

A user gone from AD — deleted, moved out of scope, filtered out, or now missing the attribute `uid` is mapped from — immediately loses all Dolly-owned group memberships. A user that still has a uid but lost another `required` attribute (for example `uidNumber`) counts as gone only for its user entry: the prune clock starts, but its memberships stay. The user entry itself is deleted only when `sync.prune_users` is enabled and the user has been gone for `sync.prune_after_days` (tracked as `missing-since` in the user's ownership record). At prune time, Dolly also removes the user from any group where they were added *locally* — the one exception to "local wins" — so no group is left pointing at an entry that no longer exists.

## Disabled accounts

A disabled AD account (`userAccountControl` bit `0x2`) keeps its target entry. Dolly:

- removes it from every group where it's Dolly-owned (local memberships are untouched),
- sets `loginShell` to `sync.disabled_shell` — this overrides `create_only` protection, because an SSH key would otherwise still let the account in,
- saves the previous shell in the user's ownership record as `saved-shell`, and restores it when the account is re-enabled — but only if `loginShell` is still `disabled_shell` at that point. If an admin changed the shell directly while the account was disabled, the admin's value stays and the `saved-shell` note is dropped instead of being applied.

A user who is already disabled the first time Dolly creates their entry is created with `disabled_shell` right away, and their mapped shell (what the mapping would otherwise have written) is saved as `saved-shell` for a later re-enable.

## Groups are never deleted

A group deleted from AD loses its Dolly-owned members and its ownership record, but the group entry itself stays on the target.

## `create_only` attributes

Attributes listed in `mapping.users.create_only` (by default `loginShell` and `homeDirectory`) are written only when Dolly creates the user, and never again — so a shell or home directory changed by an admin directly on the LDAP server survives future syncs. `disabled_shell` is the one attribute Dolly still overwrites regardless.

## Primary group

`primaryGroupID` is ignored entirely; AD doesn't list the primary group in its `member` attribute, so Dolly can't reconcile it and doesn't try.

## Empty group placeholder

This only applies when `member` is in `mapping.groups.membership` (the rfc2307bis default). `groupOfNames` requires at least one `member`, so a new AD group with no resolvable members isn't created on the target until it has one, and if removing Dolly-owned members would leave an existing target group with none at all, Dolly adds `target.empty_group_member` as a placeholder instead of violating the schema, removing the placeholder again once a real member exists.

With `memberUid` only (RFC 2307, structural `posixGroup`), none of this applies: `empty_group_member` isn't used, and groups may be empty and are created right away. See [Group schema](../configuration.md#group-schema-rfc2307bis-or-rfc-2307).

## Required attributes

To be a group member, an AD user needs only the attribute its `uid` is mapped from (`uid` by default, `sAMAccountName` in many groups-only setups). A user entry needs every attribute in `mapping.users.required` (`uid`, `uidNumber`, and `gidNumber` by default): a user missing one gets no entry, and in runs that sync users it's listed once in the run summary, but it can still be a member. Groups need `name` and `gidNumber` (`mapping.groups.required`). A group missing a required attribute, or a user without a uid, is ignored everywhere — including as a source for flattening a nested group — not just skipped with output on every run. Ignored entries are listed once in the run summary instead.

A `uidNumber` or `gidNumber` Dolly would write must be an integer from 0 to 4294967294: Linux `uid_t` and `gid_t` are unsigned 32-bit, and 4294967295 is reserved as -1. An entry with any other value, such as an 11-digit university ID, is ignored and listed once in the summary, like one missing a required attribute.

## Out-of-scope members

A group member outside the configured `users.base` / `groups.base` search bases is still followed if it matches `source.users.filter` or `source.groups.filter` and has the required attributes:

- an out-of-scope **user** becomes a normal Dolly-managed user (a member, and, with every required attribute, a user entry),
- an out-of-scope **child group** is flattened into its parent but is not itself created on the target.

Members that are neither users nor groups — computers, contacts, foreign security principals — are skipped. So is a member that exists in AD but matches neither search filter; the run summary counts it as filtered.

## `uid` case

The spelling of `uid` is preserved exactly as it appears in AD, including uppercase letters. Because LDAP matching is case-insensitive, Dolly compares names case-insensitively and treats a case-only change as a rename (`modrdn`), the same as any other rename.

## ID numbers

Duplicate `uidNumber` or `gidNumber` values — between AD entries, or against an existing local entry — never block a run; they're listed as warnings in the run summary. A *changed* `uidNumber` or `gidNumber` is always applied and always reported, because file ownership on disk depends on it.

## ASCII-only attributes

Attributes with IA5String syntax — `gecos`, `homeDirectory`, and `loginShell` — must be ASCII. Values are transliterated to ASCII before being written, because OpenLDAP rejects non-ASCII characters in these attributes outright.

## Per-entry errors

A rejected change on one entry doesn't abort the run. Dolly logs it, continues with everything else, reports it in the run summary, and exits with code `1`. This mirrors the predecessor's use of `ldapmodify -c` (continue on error), without shelling out to `ldapmodify` itself. Aborts are reserved for incomplete reads, the mass-deletion guard, and lock failures — see [How it works](index.md).
