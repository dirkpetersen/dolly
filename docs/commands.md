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
dolly sync --debug        # also print per-entry debug lines to stderr
```

| Flag | Effect |
|---|---|
| `--users` | Sync users only. |
| `--groups` | Sync groups only. The default with neither flag is both. |
| `--dry-run` | Print the planned adds, modifies, renames, and removals without writing anything. Takes no run lock. |
| `--force` | Apply the run even if it trips the [mass-deletion guard](how-it-works/index.md#guard). |
| `--debug` | Print debug details to stderr. See [The `--debug` flag](#the-debug-flag). |

A real run (also `dolly adopt`) prints a progress line to stderr 10 seconds after it starts to apply the plan, and then one every 60 seconds, for example `dolly: done 41200 of 194661 operations (21%), 3 failed, 0 skipped, 12m10s elapsed`. A run that finishes within 10 seconds prints none, and these lines are not part of the mail or `cn=status`.

There is no `dolly diff` — use `dolly sync --dry-run` instead.

A groups-only run doesn't read the AD users base or the target's `users_base` in full: it fetches each group member from AD by DN and looks up only the uids it needs on the target, so it works with a huge or size-limited `users_base`. See [Groups-only deployments](configuration.md#groups-only-deployments). A users-only run still reads AD groups, because Dolly needs them to know which out-of-scope users are still referenced; the only group values a `--users` run changes are `member`/`memberUid` fix-ups for a renamed or pruned user. Removing owned memberships from a group happens only in a run that includes groups.

In a `--groups` run, a member's target DN comes from the user's own ownership record (`seeAlso`), not a fresh AD lookup, so a pending user rename causes no group churn. A member is added only if its entry already exists under `users_base` (see [Ownership](how-it-works/ownership.md)); otherwise it is skipped, counted in the summary, and listed with `--debug`. Neither AD users nor target users need `uidNumber`; only groups need `gidNumber`.

`--users` and `--groups` each work standalone and never assume the other ran in the same invocation.

## The `--debug` flag

`--debug` is accepted by every command that plans (`sync` and `adopt`). Without it, a group member skipped because it has no matching entry under `users_base` is only counted, in a single run-summary line (`members skipped: N not on the target (use --debug to list them)`, omitted when the count is zero) — it's never a per-entry warning and never triggers a notification. With `--debug`, Dolly also prints one line per skipped member to stderr:

```text
debug: skip <uid> in <group DN>: no entry on the target under <users_base>
```

With `member` in `mapping.groups.membership`, the message instead names the expected member DN:

```text
debug: skip <uid> in <group DN>: no entry on the target at <member DN>
```

A skipped member is re-evaluated on every run and added (and recorded as Dolly-owned) as soon as a matching entry exists — see [Group membership](how-it-works/ownership.md#group-membership).

## `dolly adopt`

One-time takeover of an existing target tree that has no Dolly ownership records, such as one written by [ad2openldap](https://github.com/dirkpetersen/ad2openldap).

```bash
dolly adopt --dry-run
dolly adopt
dolly adopt --debug      # also print per-entry debug lines to stderr
```

See [Adopting an existing tree](operations/adopting.md) for what it does and its caveats.

## `dolly unlock`

Shows the current run lock — who holds it, its age by the server's `createTimestamp`, the holder's `started=` time, and a clock-skew warning if either is in the future — and removes it after confirmation.

```bash
dolly unlock          # prompts for confirmation
dolly unlock --yes    # skips the prompt
```

Without `--yes`, Dolly requires stdin to be a terminal: if it isn't (for example under cron), Dolly refuses and exits `1` rather than guess. `--yes` skips the prompt and works non-interactively.

Use this after a crash that left the lock behind, when no run is active, instead of waiting for `lock_ttl` to expire it. See [Run lock](how-it-works/run-lock.md).

## `dolly check`

Runs every check it can, even after one has failed, and never writes to AD or the target — safe to run at any time.

```bash
dolly check                    # full deployment (users and groups)
dolly check --groups           # groups-only deployment
dolly check --send-test-mail   # also sends one test message to notify.to
```

Each check prints one `✓` / `✗` / `!` line, and `dolly check` exits `1` if any check failed:

- **Config** — loads and validates `dolly.yaml` (including the SMTP validation rules; see [Configuration](configuration.md#config-validation)), and reads every password file: it must exist and be non-empty, and one readable by group or others is a warning.
- **AD** — connects to each DC in `source.urls` (LDAPS, or StartTLS for `ldap://`), and binds. A DC that fails while another answers is a warning, since real runs fail over the same way; none answering is a failure. After an invalid-credentials error the remaining DCs aren't tried, since every attempt counts toward the account's lockout. On the first DC that answered, it runs a one-page search of the users base and the groups base (and, with `source.upg.from: ad`, the UPG base) with their filters. A base with no matching entries is a failure (the guard stops every sync when AD returns zero users, zero groups, or zero user private groups) — except the users base with `--groups`, which a groups-only run never searches.
- **Target** — TLS and bind (plain `ldap://` without StartTLS is a warning). If `groups_base` exists, a full unpaged read of it must not hit the server's size limit, since every run reads it. If `users_base` exists, it must answer a `(uid=*)` lookup; the same full-read size-limit test applies, but a truncated `users_base` is a failure only without `--groups` — with it, it's a warning, since groups-only runs never read `users_base` in full. Both truncation checks print the `olcLimits` fix; see [Permissions](operations/permissions.md). A missing `users_base`, `groups_base`, or `state_base` is a warning (`!`), `does not exist, or target.bind_dn cannot see it; the first real run creates it, or aborts if it is hidden`, if Dolly can create it: an `ou=` RDN (for `state_base` also `cn=`), and a parent that exists (or is itself a base the run creates first). It is a warning, not a pass, because the server gives the same answer (`noSuchObject`) for a base the bind DN isn't allowed to see. Otherwise it is a failure. Write access there isn't tested, since `check` never writes. A run lock held longer than `lock_ttl` (by `createTimestamp`), or one with a timestamp in the future, is a warning that suggests `dolly unlock` if no run is active.
- **SMTP** (if `notify.smtp_host` is set) — connects, sends `EHLO`, does StartTLS if configured, and authenticates with `AUTH PLAIN` if a username is set, otherwise reports that no authentication will be used. No mail is sent unless `--send-test-mail` is given. See [Notifications](operations/notifications.md#authentication).

Run this after editing the config, and whenever `dolly sync` reports connection or truncation errors.

## `dolly install`

Copies the binary to `~/.local/bin`, creates the config from the built-in template if one doesn't already exist, and writes the `systemd --user` units.

```bash
dolly install
dolly install --groups                            # bake sync --groups into the unit
dolly install --users --config ~/targets/a.yaml    # bake sync --users --config <absolute path>
```

`--users` or `--groups` and `--config FILE` are baked into the unit's `ExecStart` (a relative `--config` path is made absolute; that same path is what `dolly install` creates from the template if it's missing). Without `--config`, the unit relies on the default [config lookup order](configuration.md#config-lookup-order). `dolly install` prints the resulting `ExecStart=` line and ends with the next steps (edit the config, `dolly check`, `dolly sync --dry-run`, then enable the timer) — it never enables the timer itself.

Safe to re-run: it never overwrites an existing config, replaces the binary only when it differs, and only rewrites the systemd units when their content changed (it runs `systemctl --user daemon-reload` every time). See [Scheduling](operations/scheduling.md) for the units it writes and how it handles service-account environments.

## `dolly uninstall`

Stops and disables the timer, removes the systemd units and the binary, and reloads systemd. The config is kept.

```bash
dolly uninstall
```

## `dolly fetch-cert`

Shows the certificate chain a server sends, so you can set `ca_file` or `tls_fingerprint`. With a URL as argument it reads no config; with `--source`/`--target` it writes the URLs and pins into the config (see below).

```bash
dolly fetch-cert ldaps://dc01.example.edu            # port 636
dolly fetch-cert ldap://ldap.example.edu:389         # StartTLS
dolly fetch-cert ldaps://dc01.example.edu --out ~/.config/dolly/ad-ca.pem
```

Dolly connects to `ldaps://host[:636]`, or to `ldap://host[:389]` with StartTLS. It does not verify the server and sends no credentials. It prints each certificate of the chain (subject, issuer, validity with expired ones flagged, SHA-256 fingerprint), then a ready-to-paste `tls_fingerprint` line for the leaf. `--out FILE` also writes the chain as PEM (mode `0644`, usable as `ca_file`). An existing file is never overwritten. See [TLS certificates](operations/tls.md).

```bash
dolly fetch-cert --source ldaps://dc01.example.edu --source ldaps://dc02.example.edu --target ldap://ldap.example.edu:389
```

With `--source URL` (once per DC) and `--target URL` it fetches each chain, shows the changes (old and new URLs, the new pins, and `ca_file` and `start_tls` changes), and after `[y/N]` (or `--yes`; without a terminal, `--yes` is required) writes them: `--source` replaces `source.urls` and `source.tls_fingerprint` with the given URLs and their leaf fingerprints, `--target` sets `target.url`, `target.tls_fingerprint`, and `start_tls` (`true` for `ldap://`), and `ca_file` is removed on that side. Comments are kept, the write is atomic with mode `0600`, and an invalid result is rolled back. Flags: `--source`, `--target`, `--config FILE`, `--yes`. A URL argument can't be combined with `--source`/`--target`, and `--out` only works with a URL argument.

## `dolly version`

Prints the version, commit, build date, and the Go version the binary was built with.

```bash
dolly version
```

## Exit codes

| Code | Meaning |
|---|---|
| `0` | Success, or another host already holds the run lock (a quiet, expected outcome). |
| `1` | An error occurred, including a single rejected operation while applying the plan (see [Applying the plan](how-it-works/index.md#applying-the-plan)) or a run stopped by a signal or `run_timeout`. |
| `2` | The [mass-deletion guard](how-it-works/index.md#guard) stopped the run, or would have on a `--dry-run`. Use `--force` to override if the removals are expected — this also turns a would-be `2` from `--dry-run` into `0`. |

The "AD returned zero users or zero groups" guard (and its user private groups case) applies to every `dolly sync`, dry-run or not, but not to `dolly adopt`, which has no guard.
