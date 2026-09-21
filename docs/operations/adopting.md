# Adopting an existing tree

If you're migrating from [ad2openldap](https://github.com/dirkpetersen/ad2openldap), or otherwise already have a target tree with users and groups but no Dolly ownership records, run `dolly adopt` once before your first `dolly sync`.

```bash
dolly adopt --dry-run
dolly adopt
```

## What it does

A tree written by ad2openldap (or created by hand) has no ownership records under `state_base`. For every AD user and group that matches a target entry by `uid` or `cn`, `dolly adopt`:

- creates an ownership record for that entry, and
- for each group, marks the members that are also currently in the AD group as Dolly-owned, and treats every other existing member as local.

Adopt claims every AD user that has a `uid` — even one missing `uidNumber` or `gidNumber`, or with an invalid one — rather than applying the full [required-attributes](../how-it-works/ownership.md#required-attributes) check that `sync` uses for user entries. That way, the following `dolly sync` can treat those user entries as gone (pruning them if `sync.prune_users` is on) instead of never claiming them in the first place. Until then they stay group members, since a member needs only a uid. Groups still need to pass their own required-attribute check (`name` and `gidNumber`) to be adopted. Adopt flattens through every child group exactly as `sync` does, and counts disabled AD accounts (`userAccountControl` bit `0x2`) as members when deciding which existing target members are Dolly-owned.

After adoption, `dolly sync` behaves normally: it only adds and removes membership it manages, and leaves everything marked local alone. See [Ownership](../how-it-works/ownership.md) for the full rule set adoption sets up.

Always review `dolly adopt --dry-run` before running it for real — adoption only happens once, and the decisions it makes (Dolly-owned vs. local) are hard to undo cleanly afterward.

## Caveat: recent removals look local

If a user was removed from an AD group shortly before you run `dolly adopt`, that membership already looks purely local at adoption time — Dolly has no record that it was ever AD-sourced, so it will never remove that member. Check `dolly adopt --dry-run` for any group memberships you expect to have already been removed, and clean those up by hand if needed.

## Caveat: users without `gidNumber` get no user entry

This is an intended behavior change from ad2openldap, not a bug. The old tool defaulted a missing `gidNumber` to `65534`. Dolly has no such default: users without `gidNumber` get no user entry from `sync`, per the [required attributes](../how-it-works/ownership.md#required-attributes) rule, and any member that only arrived through a child group whose own `gidNumber` is missing isn't a member at all, because that child group isn't flattened. `dolly adopt` itself still claims these users, as described above.

`dolly adopt --dry-run` lists these entries. On the first real sync:

- members that only came through a child group without `gidNumber` lose those memberships, and the [mass-deletion guard](../how-it-works/index.md#guard) may trip and ask for `--force` if there are enough of them,
- users without `gidNumber` keep their memberships, but their user entries count as gone from AD: with `sync.prune_users` enabled, they're deleted once they've been gone for `sync.prune_after_days`, which removes their memberships too.

Fix `gidNumber` in AD before adopting if you want these users to keep syncing normally.
