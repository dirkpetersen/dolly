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

A groups-only run still reads AD users (read-only) to resolve group members. `--users` and `--groups` each work standalone and never assume the other ran in the same invocation.

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
| `2` | The [mass-deletion guard](how-it-works/index.md#guard) stopped the run. Use `--force` to override if the removals are expected. |
