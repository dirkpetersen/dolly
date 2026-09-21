# Notifications

Dolly sends at most **one email per run**. There's no per-entry mail — a run that changes a hundred users still produces exactly one message.

## Configuring SMTP

Notifications are configured under `notify` in `dolly.yaml`:

```yaml
notify:
  smtp_host: mx.example.edu
  smtp_port: 25
  start_tls: true
  username: ""                        # optional SMTP auth
  password_file: ""
  from: "Dolly <dolly-noreply@example.edu>"
  to: [ldap-admins@example.edu]
  subject_prefix: "[dolly]"
  on: failure                         # failure | changes | always
  remind_every: 24h                   # while a failure persists, remind at most this often
```

See [Configuration](../configuration.md#notify) for the full key reference. `username` and `password_file` are optional — leave them empty for an SMTP relay that doesn't require auth. `dolly check` also tests the SMTP connection, so you can validate this section without waiting for a real run; see [Commands](../commands.md#dolly-check).

## What a mail contains

Each mail is a summary of the whole run: counts first, then the adds, removals, renames, changed ID numbers, warnings, and errors. Ignored entries and conflicts are listed in the run summary too, but only mailed when that list changes from the previous run, so a long-standing conflict doesn't nag forever.

## When mail is sent

The `on` setting controls the trigger:

| Value | Sends mail |
|---|---|
| `failure` | Only around failures (see below). |
| `changes` | When the run made changes. Failures are always reported. |
| `always` | On every run. |

With `on: failure` (the default), Dolly avoids flooding your inbox during an extended outage by tracking state in the `cn=status` entry:

- one mail when a failure **first appears**,
- at most one reminder per `remind_every` while it **persists**,
- one mail on **recovery**, when the run next succeeds.

So an AD outage that lasts overnight produces two or three mails total, not one for every 15-minute run.

## `cn=status`

Dolly keeps a `cn=status` entry under `state_base` (alongside the ownership records and run lock) recording:

- the last successful run,
- the current failure, if any,
- when a notification was last sent.

This is what makes the flood-control behavior possible without any local state — a fresh host or a rebuilt Dolly instance picks up exactly where the last one left off, because the status lives in the target LDAP too. See [Ownership](../how-it-works/ownership.md) for the rest of what lives under `state_base`.
