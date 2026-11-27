# Only group members

By default, a run that syncs users reads every user under `source.users.base` and gives each valid user an entry on the target. Sometimes you only want entries for the members of the groups you replicate. For example, you replicate one OU of groups, and the users live somewhere else in a large directory.

## Configuration

```yaml
source:
  users:
    base: OU=People,DC=example,DC=edu
    filter: (&(objectClass=user)(objectCategory=person))
    only_group_members: true
```

The default is `false`. `dolly sync --only-group-members` sets it to `true` for one run.

## Which users get an entry

A user gets an entry when all of these are true:

- It is a member of a synced shared group (`source.groups`). Nested groups are flattened first, so members of child groups count.
- It matches `source.users.filter`.
- It has every attribute in `mapping.users.required`.

These users do **not** get an entry:

- A user in the users base that is in no synced group.
- A user whose only group is a user private group (`source.upg`). UPG members do not count.

Nothing happens to a user that has no entry and is not a member.

## How Dolly finds the users

Dolly doesn't search `source.users.base`. It fetches each group member by DN, with base-scope lookups, several at a time. This is the same path that a `--groups` run uses. Each lookup must still match `source.users.filter` or `source.groups.filter`. A member that matches neither is skipped and counted as filtered. Because of this, members outside the users base get entries too.

`source.users.base` is still required. It gives a bare `bind_dn` its domain, and `dolly adopt` reads it. On the target, a run that syncs users still reads all of `users_base`.

## Users that leave their groups

A Dolly-managed user that is no longer a member of a synced group is treated like a user that is gone from AD:

- It loses its Dolly-owned memberships at once.
- Its entry is deleted only by the usual prune rules: `sync.prune_users` and `sync.prune_after_days`.

## Guard

The [mass-deletion guard](../how-it-works/index.md#guard) checks that AD returned users. In this mode it counts the users found through the groups, not the users in the users base. If no group member can get a user entry, the guard trips:

```text
no member of the synced groups can get a user entry (only_group_members)
```

A run with zero groups trips the guard as always.

## Which runs it affects

- `dolly sync` and `dolly sync --users`: yes.
- `dolly sync --groups`: no. A groups-only run reads no users base anyway.
- `dolly adopt`: no. It still claims the users of the users base. The first sync then treats the users that are in no group as gone.
- `dolly check`: an empty or failing search of the users base is a warning (`!`), not a failure.

## Summary line

The plan header has one more line, where N is the number of users that get or keep an entry:

```text
Users: only group members (N found by DN lookup)
```
