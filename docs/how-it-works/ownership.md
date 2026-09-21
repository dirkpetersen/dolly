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
│                         roleOccupant: uid=jdoe,ou=people,dc=local   (one per member Dolly added)
├── cn=lock                                                              (only while a run is active)
└── cn=status             last successful run, current failure, last notification
```

Each record's RDN is `cn=<objectGUID>` — the AD object's `objectGUID`, formatted as a canonical hyphenated string (AD returns it as 16 mixed-endian bytes, so Dolly normalizes it once to a single consistent format). `seeAlso` points at the managed target entry. On group records, `roleOccupant` lists the members Dolly itself added to that group; `memberUid` ownership is derived from those DNs rather than tracked separately. User records also carry small `description` values in `key=value` form, such as `missing-since=2024-01-01` or a saved shell to restore later.

Dolly creates `state_base` and its children if they're missing. It never creates `users_base` or `groups_base` — if those don't exist, it stops with a clear error.

## Group membership

- A user in the AD group but not in the target group is added to the target group and recorded as Dolly-owned.
- A Dolly-owned member who left the AD group, or was deleted from AD, is removed from the target group and from the record.
- Membership is never replaced wholesale. Dolly never computes "the target group's members = AD's list"; it adds and removes individual `member` and `memberUid` values, and only ever touches the attributes listed in the mapping.

## Local wins

A member already present in a target group without an ownership record was added locally, on the LDAP server directly. Dolly never removes that member — even if the same user also happens to be in the AD group. Local additions are permanent until removed by hand, with one exception: [pruning](#deleted-and-pruned-users) a user removes their local memberships too, to avoid a dangling reference to a deleted entry.

## Conflicts

A target user or group that matches an AD entry by name (`uid` or `cn`) but has no ownership record is a conflict: something with that name already existed on the target before Dolly touched it, or was created outside Dolly. Dolly logs an error and skips it rather than guessing. `dolly adopt`, run once, is the only thing that claims existing unowned entries — see [Adopting an existing tree](../operations/adopting.md).

## Renames

Identity is `objectGUID`, not name. A user or group renamed in AD (same `objectGUID`, different name) becomes a `modrdn` on the target, followed by updating every `member` and `memberUid` value that referenced the old DN, and the `seeAlso` and `roleOccupant` values in its own and any group's ownership records.

If a rename would collide with an entry that already exists at the new name, that's a conflict: Dolly logs it and skips the rename.

## Deleted and pruned users

A user gone from AD — deleted, moved out of scope, or now missing a `required` attribute — immediately loses all Dolly-owned group memberships. The user entry itself is deleted only when `sync.prune_users` is enabled and the user has been gone for `sync.prune_after_days` (tracked as `missing-since` in the user's ownership record). At prune time, Dolly also removes the user from any group where they were added *locally* — the one exception to "local wins" — so no group is left pointing at an entry that no longer exists.

## Disabled accounts

A disabled AD account (`userAccountControl` bit `0x2`) keeps its target entry. Dolly:

- removes it from every group where it's Dolly-owned (local memberships are untouched),
- sets `loginShell` to `sync.disabled_shell` — this overrides `create_only` protection, because an SSH key would otherwise still let the account in,
- saves the previous shell in the user's ownership record, and restores it when the account is re-enabled.

## Groups are never deleted

A group deleted from AD loses its Dolly-owned members and its ownership record, but the group entry itself stays on the target.

## `create_only` attributes

Attributes listed in `mapping.users.create_only` (by default `loginShell` and `homeDirectory`) are written only when Dolly creates the user, and never again — so a shell or home directory changed by an admin directly on the LDAP server survives future syncs. `disabled_shell` is the one attribute Dolly still overwrites regardless.

## Primary group

`primaryGroupID` is ignored entirely; AD doesn't list the primary group in its `member` attribute, so Dolly can't reconcile it and doesn't try.

## Empty group placeholder

`groupOfNames` requires at least one `member`. A new AD group with no resolvable members isn't created on the target until it has one. If removing Dolly-owned members would leave an existing target group with none at all, Dolly adds `target.empty_group_member` as a placeholder instead of violating the schema, and removes the placeholder again once a real member exists.

## Required attributes

Users need `uid`, `uidNumber`, and `gidNumber`. Groups need `name` and `gidNumber` (`mapping.users.required` / `mapping.groups.required`). An AD entry missing any of its required attributes is ignored everywhere — including as a source for flattening a nested group — not just skipped with output on every run. Ignored entries are listed once in the run summary instead.

## Out-of-scope members

A group member outside the configured `users.base` / `groups.base` search bases, but with the required attributes, is still followed:

- an out-of-scope **user** becomes a normal Dolly-managed user,
- an out-of-scope **child group** is flattened into its parent but is not itself created on the target.

Members that are neither users nor groups — computers, contacts, foreign security principals — are skipped.

## `uid` case

The spelling of `uid` is preserved exactly as it appears in AD, including uppercase letters. Because LDAP matching is case-insensitive, Dolly compares names case-insensitively and treats a case-only change as a rename (`modrdn`), the same as any other rename.

## ID numbers

Duplicate `uidNumber` or `gidNumber` values — between AD entries, or against an existing local entry — never block a run; they're listed as warnings in the run summary. A *changed* `uidNumber` or `gidNumber` is always applied and always reported, because file ownership on disk depends on it.

## ASCII-only attributes

Attributes with IA5String syntax, such as `gecos`, must be ASCII. Values are transliterated to ASCII before being written, because OpenLDAP rejects non-ASCII characters in these attributes outright.

## Per-entry errors

A rejected change on one entry doesn't abort the run. Dolly logs it, continues with everything else, reports it in the run summary, and exits with code `1`. This mirrors the predecessor's use of `ldapmodify -c` (continue on error), without shelling out to `ldapmodify` itself. Aborts are reserved for incomplete reads, the mass-deletion guard, and lock failures — see [How it works](index.md).
