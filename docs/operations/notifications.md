# Notifications

Dolly sends at most **one email per run**, in plain text. There's no per-entry mail — a run that changes a hundred users still produces exactly one message.

## Configuring SMTP

Notifications are configured under `notify` in `dolly.yaml`:

```yaml
notify:
  smtp_host: mx.example.edu
  smtp_port: 25
  start_tls: true
  username: ""                        # optional SMTP auth
  password: ""                        # inline, or use password_file (set only one)
  password_file: ""
  from: "Dolly <dolly-noreply@example.edu>"
  to: [ldap-admins@example.edu]
  subject_prefix: "[dolly]"
  on: failure                         # failure | changes | always
  remind_every: 24h                   # while a failure persists, remind at most this often
```

See [Configuration](../configuration.md#notify) for the full key reference, and [Passwords](../configuration.md#passwords) for the rules on inline vs. file passwords. An empty `notify.smtp_host` disables notifications entirely.

### Authentication

`username` and `password`/`password_file` are optional, for a relay that doesn't require auth (the usual campus relay on port 25): leave `username` empty and Dolly never sends `AUTH`, with or without StartTLS. Setting a username requires a password too, and vice versa — a config error otherwise. A username also requires TLS, since `AUTH` is never sent over an unencrypted connection: either `start_tls: true`, or `smtp_port: 465` (implicit TLS, where `start_tls` must then be `false`). StartTLS and implicit TLS both verify the server's certificate against the system trust store — there is no way to skip verification. `notify.from` and each `notify.to` entry must be valid mail addresses.

`dolly check` also tests this section: it connects, sends `EHLO`, does StartTLS if configured, and authenticates with `AUTH PLAIN` if a username is set (otherwise it reports that no authentication will be used). Add `--send-test-mail` to actually send one message to `notify.to`; without it, `dolly check` never sends mail. See [Commands](../commands.md#dolly-check).

## What a mail contains

Each mail is a plain-text summary of the whole run: a short header (host, command, start and end time, outcome, and why this mail was sent), the failure if any, then the run's output exactly as the journal has it — the plan with its counts, adds, removals, renames, changed ID numbers, and warnings, the result with the created and changed groups and their members, and the errors. A hundred changed users is still one mail. A body longer than 2,000 lines is cut, with a note pointing at `journalctl --user -u dolly` for the rest.

## When mail is sent

The `on` setting controls the trigger for non-failure mail:

| Value | Sends mail |
|---|---|
| `failure` | Only around failures (see below). |
| `changes` | When the run made changes. Failures are always reported, whatever `on` says. |
| `always` | On every run. |

Failures are mailed whatever `on` says. Dolly avoids flooding your inbox during an extended outage by tracking state in `cn=status`:

- one mail when a failure **first appears** (or was never successfully mailed — for example because an earlier send attempt failed),
- at most one reminder per `remind_every` while it **persists**,
- one mail on **recovery**, when the run next succeeds.

So an AD outage that lasts overnight produces two or three mails total, not one for every 15-minute run. A read error, a tripped [mass-deletion guard](../how-it-works/index.md#guard), a rejected operation, and a run stopped by a signal or `run_timeout` all count as failures.

A **broken stale lock** (see [Run lock](../how-it-works/run-lock.md#stale-locks)) is always mailed too, even if the run that broke it then goes on to succeed.

**Warnings** — ignored entries, conflicts, duplicate IDs, groups still waiting for their first member — are mailed only when the list changes from the previous run, whatever `on` says, so a long-standing conflict doesn't nag forever. A *changed* `uidNumber`/`gidNumber` is a change, not a warning, and follows the `changes`/`always` rule above like any other change.

Two cases never send mail at all:

- a run that finds the lock already held by another host — it exits `0` quietly and never touches `cn=status`;
- `--dry-run` — it takes no lock and never writes `cn=status`.

A run that can't reach the target at all can't read or write `cn=status` either, so it can't be mailed either way — it logs the error and exits `1`. Watch the systemd unit's result for that case (see `journalctl --user -u dolly`).

## `cn=status`

Dolly keeps a `cn=status` entry (an `organizationalRole`) under `state_base` (alongside the ownership records and run lock), recording, as `description` key=value notes:

- `last-run` — when the last real run happened,
- `last-success` — when a run last completed without aborting,
- `last-result` — a one-line count summary of that run,
- `failure-since` and `failure` — present only while a failure persists,
- `last-notified` — when a notification mail was last **successfully sent**,
- `notify-error` — the last failed send attempt, cleared by the next successful send,
- `warnings-hash`, `warnings-hash-users`, `warnings-hash-groups`, `warnings-hash-adopt` — a short hash of the current warnings list, one note per run scope, so alternating `--users` and `--groups` runs don't keep re-triggering a warnings mail off each other's hash.

Every real run that acquires the lock writes this entry, whether it succeeded, failed to read AD or the target, tripped the mass-deletion guard, had per-entry errors while applying the plan, or was stopped by a signal or `run_timeout`. `--dry-run` never writes it, since it never takes the lock.

`last-notified` and the warnings hashes are updated only *after* a successful send, so a failed send is retried by the next run instead of being silently treated as delivered. A mail failure never changes the run's own exit code — it's only logged (to stderr) and recorded in `notify-error`.

This is what makes the flood-control behavior possible without any local state — a fresh host or a rebuilt Dolly instance picks up exactly where the last one left off, because the status lives in the target LDAP too. See [Ownership](../how-it-works/ownership.md) for the rest of what lives under `state_base`.
