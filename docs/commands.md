# Commands

All commands accept `--config <path>` to override the [config lookup order](configuration.md#config-lookup-order).

## `dolly sync`

Reads all in-scope users and groups from AD and applies the differences to the target.

```bash
dolly sync                # both users and groups (default)
dolly sync --users        # users only
dolly sync --groups       # groups only
dolly sync --dry-run      # print the plan, write nothing
dolly sync --force        # apply even if the mass-deletion guard trips
```

| Flag | Effect |
|---|---|
| `--users` | Sync users only. |
| `--groups` | Sync groups only. The default with neither flag is both. |
| `--dry-run` | Print the planned adds, modifies, renames, and removals without writing anything. Takes no run lock. |
| `--force` | Apply the run even if it trips the [mass-deletion guard](how-it-works/index.md#guard). |

There is no `dolly diff` — use `dolly sync --dry-run` instead.

A groups-only run doesn't read the AD users base or the target's `users_base` in full: it fetches each group member from AD by DN and looks up only the uids it needs on the target, so it works with a huge or size-limited `users_base`. See [Groups-only deployments](configuration.md#groups-only-deployments). A users-only run still reads AD groups, because Dolly needs them to know which out-of-scope users are still referenced; the only group values a `--users` run changes are `member`/`memberUid` fix-ups for a renamed or pruned user. Removing owned memberships from a group happens only in a run that includes groups.

In a `--groups` run, a member's target DN comes from the user's own ownership record (`seeAlso`), not a fresh AD lookup, so a user rename still pending its own `--users` sync causes no group churn. With `sync.require_member_on_target` (the default), a member is added only if its entry exists under `users_base`. With it off and `member` in the membership list, an AD user that has neither an ownership record nor a target entry yet is skipped in a `--groups` run and listed as pending until a users sync creates it; with `memberUid` only, no user entry is needed.

`--users` and `--groups` each work standalone and never assume the other ran in the same invocation.

## `dolly adopt`

One-time takeover of an existing target tree that has no Dolly ownership records, such as one written by [ad2openldap](https://github.com/dirkpetersen/ad2openldap).

```bash
dolly adopt --dry-run
dolly adopt
```

See [Adopting an existing tree](operations/adopting.md) for what it does and its caveats.

## `dolly unlock`

Shows the current run lock — who holds it, and since when — and removes it after confirmation.

```bash
dolly unlock          # prompts for confirmation
dolly unlock --yes    # skips the prompt
```

Use this after a crash that left the lock behind, instead of waiting for `lock_ttl` to expire it. See [Run lock](how-it-works/run-lock.md).

## `dolly check`

Tests connectivity, binds, search scopes, the target's containers and size limit, and SMTP.

```bash
dolly check
```

Run this after editing the config, and whenever `dolly sync` reports connection or truncation errors.

## `dolly install`

Copies the binary to `~/.local/bin`, creates the config from the built-in template if one doesn't already exist, and writes the `systemd --user` units.

```bash
dolly install
```

Safe to re-run: it never overwrites an existing config, and it only rewrites the systemd units when their content changed. See [Scheduling](operations/scheduling.md) for the units it writes and how it handles service-account environments.

## `dolly uninstall`

Removes the systemd units and the binary. The config is kept.

```bash
dolly uninstall
```

## `dolly version`

Prints the version, commit, and build date.

```bash
dolly version
```

## Exit codes

| Code | Meaning |
|---|---|
| `0` | Success, or another host already holds the run lock (a quiet, expected outcome). |
| `1` | An error occurred. |
| `2` | The [mass-deletion guard](how-it-works/index.md#guard) stopped the run, or would have on a `--dry-run`. Use `--force` to override if the removals are expected — this also turns a would-be `2` from `--dry-run` into `0`. |

The "AD returned zero users or zero groups" guard applies to every `dolly sync`, dry-run or not, but not to `dolly adopt`, which has no guard.
