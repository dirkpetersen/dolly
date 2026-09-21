# Scheduling

Dolly is designed to run unprivileged, under `systemd --user`, with all paths following the XDG base directory spec. There is no local state or lock file to manage — both live in the target LDAP — so scheduling is just "run `dolly sync` periodically."

## systemd user timer (default)

`dolly install` writes these units, so you normally don't create them by hand:

```ini
# ~/.config/systemd/user/dolly.service
[Unit]
Description=Dolly AD to LDAP sync
After=network-online.target

[Service]
Type=oneshot
ExecStart=%h/.local/bin/dolly sync

# ~/.config/systemd/user/dolly.timer
[Unit]
Description=Run Dolly every 15 minutes

[Timer]
OnCalendar=*:0/15
RandomizedDelaySec=60
Persistent=true

[Install]
WantedBy=timers.target
```

```bash
systemctl --user enable --now dolly.timer
loginctl enable-linger "$USER"   # keep the timer running while you're logged out (may need an admin)
```

`RandomizedDelaySec=60` on the timer spreads out runs when several hosts are on the same schedule, so they don't all fire in the same second.

## Idempotent install

`dolly install` is safe to re-run:

- it never overwrites an existing `dolly.yaml`,
- it only rewrites the systemd units when their content has actually changed.

`dolly uninstall` removes the units and the binary, and leaves the config in place.

## Environment gaps in service-account setups

Service accounts reached via `su -` or `sudo -iu` often lack a full session environment. `dolly install` handles the common gaps:

- **`XDG_RUNTIME_DIR` unset.** This is typical after `su -` or `sudo -iu`, and normally makes `systemctl --user` fail with "Failed to connect to bus." If `/run/user/<uid>` exists, Dolly sets `XDG_RUNTIME_DIR` and `DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/<uid>/bus` for its own `systemctl --user` calls. If `/run/user/<uid>` doesn't exist, the account has no active session and no linger enabled — Dolly fails clearly and tells you to run `loginctl enable-linger <user>` (may need an admin to run it for the service account).
- **`~/.local/bin` missing or not on `PATH`.** Dolly creates the directory if needed, and warns (without failing) if it isn't on `PATH`. This doesn't matter for the timer itself, since the unit references the absolute path `%h/.local/bin/dolly` and never depends on `PATH`.
- **Non-Linux hosts.** Without systemd, `dolly install` installs the binary and config, skips writing any units, and says so.

Dolly never edits shell startup files (`.bashrc`, `.profile`, and so on) to fix any of this.

## Several targets from one host

There is no multi-target mode — each target LDAP server needs its own config file, state base, and run lock. To sync more than one target from a single host, keep one `dolly.yaml` per target and pass `--config` explicitly for each, either as separate `dolly.service` units (one per target) or by wrapping calls to `dolly sync --config /path/to/target-a.yaml` and `--config /path/to/target-b.yaml` in your own scheduling. See [Configuration](../configuration.md#config-lookup-order).
