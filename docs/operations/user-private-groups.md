# User private groups

Many sites give every user a *user private group* (UPG): a group of its own, often with the same name as the user, whose `gidNumber` is the user's primary group. Dolly can sync these groups as a second group set, apart from the shared groups:

- the **shared groups**: `source.groups` in AD, `target.groups_base` on the target
- the **user private groups** (optional): `source.upg`, `target.upg_base`

Without a `source.upg` section, nothing changes. If `source.upg` is removed later, Dolly leaves the existing UPG records and groups alone and lists one warning: "N user private group records exist under ou=upg but source.upg is not set; they are left alone". The UPG set has its own target container and its own ownership records (`ou=upg,<state_base>`), so the same group name can exist in both sets.

## Configuration

```yaml
source:
  upg:
    from: ad                          # or: users
    base: OU=UPG,DC=example,DC=edu    # from: ad only
    filter: (objectClass=group)       # from: ad only
    # gid_number: gidNumber           # from: users only
target:
  upg_base: ou=upg,dc=local
```

`source.upg` and `target.upg_base` are set together. `upg_base` must be a base of its own: it must differ from `users_base`, `groups_base`, and `state_base`, it must not lie inside any of them, and it must not contain `users_base` or `groups_base`. `state_base` may still lie inside `users_base`, `groups_base`, or `upg_base`. A real run creates a missing `upg_base` (with an `ou=` RDN) like the other bases, and stops if it exists but `target.bind_dn` can't see it.

## `from: ad`: the groups come from AD

Dolly reads the AD groups under `source.upg.base` that match `source.upg.filter` and replicates them to `upg_base`. They use the same group mapping and the same rules as the shared groups: ownership records, local members win, groups are never deleted, members must exist on the target, and nested groups are flattened.

- A group that both AD searches return belongs to the UPG set.
- An AD UPG that is a member of a shared group is **not** flattened into that group. Dolly skips it and counts it with the non-user members in the summary.
- [`gid_from_upg`](gid-from-upg.md) looks up the user's group in the UPG set, not in the shared groups.
- If the UPG search returns no groups at all (for example after the OU was renamed in AD, or with a wrong filter), the mass-deletion guard stops the run.
- A group that moves between the sets in AD (into or out of the UPG search) keeps a record of the old set. Dolly reports a conflict and skips it. `dolly adopt` claims only an entry at the group's DN in its new base. So delete its record under `state_base`, move the entry to the new base (or delete the old entry), then run `dolly adopt`.

## `from: users`: Dolly makes one group per user

Dolly makes a `posixGroup` for each AD user that gets a user entry:

| Attribute | Value |
|---|---|
| `cn` (the group RDN) | the user's uid |
| `gidNumber` | the user's mapped `gid_number` attribute: `gidNumber` by default (after `gid_from_upg`), or for example `uidNumber` |
| `memberUid` | the uid |
| `member` | the user's DN, if `member` is in `mapping.groups.membership` |

The object classes come from the group mapping. Dolly manages only `cn` and `gidNumber` on these groups; it leaves other attributes and local members alone. The group's ownership record is keyed by the user's `objectGUID`.

- **Which runs.** These groups are planned only in runs that sync users (`dolly sync`, `dolly sync --users`). A `--groups` run leaves them alone and never treats them as gone.
- **Renames.** When the uid changes, the user's rename updates `memberUid` and `member` in every group. Then the UPG gets a `modrdn` to `cn=<new uid>`, and its record follows, with the same crash-safe rename note as any rename.
- **Gone or disabled users.** A user that is gone from AD, or disabled, loses only its Dolly-owned membership in its UPG. A disabled user always loses it, also when its `gid_number` value is invalid. With `member`, the `empty_group_member` placeholder keeps the group valid. The group is never deleted. Its record stays while the user's own record does, so a user that comes back gets its membership back. When the user is pruned, the UPG record goes too, and the group entry stays.
- **Duplicate gidNumbers.** These UPGs take part in the duplicate `gidNumber` check with both group sets, and each duplicate is a summary warning. So a `--users` run also reports duplicate `gidNumber` values among the shared groups.

    !!! warning
        With the default `gid_number: gidNumber`, users that all share one primary gid (for example 100) give many UPGs with the same `gidNumber`. Set `gid_number: uidNumber`, or use [`gid_from_upg`](gid-from-upg.md), to give each UPG its own gid.

- **Bad gidNumber.** A user whose `gid_number` value is missing or not a valid ID (0 to 4294967294) gets no new UPG, with an `invalid` warning. The user entry is not affected, and a UPG the user already has stays as it is, with its member, unless the user is disabled.

## Adopting

`dolly adopt` also claims existing UPGs under `upg_base`. With `from: ad`, it matches the AD UPGs by `cn`. With `from: users`, it matches the groups named after the uid of a synced user. Members that are also in the AD group (for `from: users`, the user itself) become Dolly-owned; all other members stay local.

## Guard, output, and check

- UPG membership removals count toward the [mass-deletion guard](../how-it-works/index.md) together with the shared group memberships.
- The dry-run output shows the UPG set separately: an `AD: N user private groups` line, the UPG entry count on the `Target:` line, a `User private groups` section, and an `upgs:` summary line.
- `dolly check` searches `source.upg.base` (with `from: ad`) and checks that `upg_base` exists and can be read in one search.
