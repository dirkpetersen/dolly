# How it works

Every `dolly sync` run follows the same six steps, in order. There's no incremental mode: each run reads the full current state from AD and the target, then applies the minimum set of changes needed to reconcile them.

## 1. Lock

Dolly takes the run lock in the target LDAP, at `cn=lock,<state_base>`. If another host already holds it, Dolly exits quietly with code `0` — no error, no mail. `--dry-run` takes no lock at all. See [Run lock](run-lock.md) for how the lock works and how stale locks are broken.

## 2. Bleat

Dolly binds to AD, trying `source.urls` in order, and runs paged searches for all in-scope groups on every run, and for all in-scope users in every run that syncs users. Large `member` attributes are fetched with ranged retrieval (`member;range=…`), so AD's 1500-value limit on a single attribute read never silently truncates a large group. Group members the searches didn't return, such as members outside the configured search bases, are fetched individually by DN (base-scope lookups, several at a time). They must match `source.users.filter` or `source.groups.filter`, just like the searches. A member that matches neither is skipped as filtered, as if it were out of scope.

A groups-only run (`--groups`) doesn't search the AD users base at all. It fetches every group member by DN and follows nested groups the same way, so its cost depends on the size of the synced groups, not of the users base. On the target it reads `groups_base` and `state_base`, but not `users_base`. Instead it looks up only the uids of the members it would add, 50 per search (`(|(uid=a)(uid=b)…)`), so a `users_base` larger than the server's size limit is no problem. A users-only run (`--users`) still reads AD groups, because Dolly needs them to know which out-of-scope users are still referenced by a group and must therefore still be followed. In a `--users` run, the only group values that change are `member`/`memberUid` fix-ups for a user rename or prune; removing owned memberships happens only in a run that includes groups (`dolly sync` or `--groups`).

!!! warning "Never plan from partial data"
    If any read from AD or the target fails, or comes back truncated (for example `sizeLimitExceeded`), the run aborts immediately. Dolly never computes a plan from an incomplete read. OpenLDAP's `olcSizeLimit` (500 by default) applies to every bind DN except the rootdn, so a non-rootdn bind DN that hits it will trip this abort — see [Permissions](../operations/permissions.md).

Every run reads *all* in-scope groups, and all in-scope users when it syncs users. There's no `uSNChanged` incremental mode, because it would miss deletions and nested-group membership changes. Reading around 10,000 groups is cheap, on the order of seconds.

## 3. Shear

Each AD entry is mapped through the configured [attribute rules](../configuration.md#mapping). Entries missing a `required` attribute, or with a `uidNumber` or `gidNumber` outside 0 to 4294967294, are ignored — not just skipped with a warning every run, but listed once in the run summary. A user missing `uidNumber` or `gidNumber` only gets no entry of its own; it can still be a group member (see [Required attributes](ownership.md#required-attributes)). Nested groups are flattened into direct user members, with a cycle guard against loops in the parent chain. A group whose only members are other groups is still emitted once flattening resolves it to actual users.

Members are resolved by DN, never by CN: AD member DNs are mapped through a `distinguishedName → objectGUID` map to the correct user, because CNs aren't unique and can contain escaped commas (`CN=Gow\, Edward L,...`).

## 4. Compare

Dolly reads the target's current entries and its own ownership records under `state_base`, then computes a plan: adds, attribute modifications, renames (`modrdn`), and member additions and removals. See [Ownership](ownership.md) for the full set of rules the comparison follows.

`--dry-run` prints exactly this plan and stops — the same plan a real run would apply.

## 5. Guard {: #guard }

Before writing anything, Dolly counts planned removals across the whole run, separately for group memberships and for users. The run aborts with **no writes**, exits with code `2`, and sends a notification if:

- either count exceeds **both** `max_delete_min` and `max_delete_percent` of what Dolly owns, or
- AD returned zero users or zero groups at all. (A `--groups` run doesn't read the users base, so only zero groups counts there.)

Local memberships removed by a prune are reported in the summary but don't count toward the guard.

`--force` overrides the guard and applies the run anyway. This is the safety net against a misconfigured filter, an AD outage that looks like "everyone got deleted," or a search base typo.

This guard runs on every `dolly sync`, including `--dry-run`: a dry run that would trip it still reports what it would have tripped on and exits `2`, even though it writes nothing regardless. `--force` makes that case exit `0` instead. The "AD returned zero users or zero groups" check applies to every sync, dry-run or not — but not to `dolly adopt`, which has no guard of its own.

## 6. Clone

Dolly applies the plan against the running server, using individual add, modify, modrdn, and delete operations — never a full rebuild, never LDIF, never `ldapmodify`.

Writes happen in a crash-safe order:

- **Adding** an entry or membership: the ownership record is written *before* the entry or member is added.
- **Removing** an entry or membership: the entry or member is removed *before* its ownership record is removed.

This order matters because a crash partway through must never leave a Dolly-added member looking like a local one (which would make it un-removable later), and must never remove an ownership record while the thing it owns still exists (which would leak an entry Dolly can no longer track).

Per-entry errors — a rejected modify, an unreachable entry — don't abort the run. Dolly logs them, continues with the rest, reports them in the summary, and exits with code `1`. Aborts are reserved for incomplete reads (step 2), the mass-deletion guard (step 5), and lock failures.

Finally, Dolly updates the `cn=status` entry under `state_base` and releases the lock.

## Next

- [Ownership](ownership.md) — the full rule set for what Dolly manages and what it leaves alone.
- [Run lock](run-lock.md) — locking, staleness, and `dolly unlock`.
