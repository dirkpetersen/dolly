# Scheduling

Dolly is designed to run unprivileged, under `systemd --user`, with all paths following the XDG base directory spec. There is no local state or lock file to manage — both live in the target LDAP — so scheduling is just "run `dolly sync` periodically."

## systemd user timer (default)

`dolly install` writes these units, so you normally don't create them by hand. This is the default, a full sync of users and groups:

```ini
# ~/.config/systemd/user/dolly.service
[Unit]
Description=Dolly AD to LDAP sync
After=network-online.target

[Service]
Type=oneshot
TimeoutStartSec=1h
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

`dolly install --groups` (or `--users`) bakes the flag into the service's `ExecStart`, for example for a groups-only deployment, and `--config FILE` adds the config's absolute path (a relative path is made absolute), which is also the path `dolly install` creates from the template if it's missing. Without `--config`, the service relies on the default [config lookup order](../configuration.md#config-lookup-order), since its working directory isn't where you ran `dolly install`:

```bash
dolly install --groups
# dolly.service then has:
# ExecStart=%h/.local/bin/dolly sync --groups

dolly install --groups --config ~/targets/ldap-a.yaml
# ExecStart=%h/.local/bin/dolly sync --groups --config /home/svc-dolly/targets/ldap-a.yaml
```

`dolly install` prints the resulting `ExecStart=` line, and re-running it with different flags rewrites the service. It doesn't enable the timer itself — it ends by printing the next steps (edit the config, `dolly check`, `dolly sync --dry-run`, then):

```bash
systemctl --user enable --now dolly.timer
loginctl enable-linger "$USER"   # keep the timer running while you're logged out (may need an admin)
```

`TimeoutStartSec=1h` on the service keeps a hung run from blocking the timer forever. It is above the default `run_timeout` (45m), and `dolly install` warns if your config's `run_timeout` isn't below it. When it expires, systemd stops the service with `SIGTERM` (the default `KillMode`), which Dolly handles like `run_timeout`: it stops between operations, records the failure in `cn=status`, and releases the lock.

`RandomizedDelaySec=60` on the timer spreads out runs when several hosts are on the same schedule, so they don't all fire in the same second.

## Idempotent install

`dolly install` is safe to re-run:

- it never overwrites an existing `dolly.yaml` (created with mode `0600`, in a `0700` directory),
- it replaces the binary only when it differs from what's already installed (written to a temp file and renamed into place, mode `0755`),
- it only rewrites the systemd units when their content has actually changed,
- it runs `systemctl --user daemon-reload` on every install, so a re-run after a failed reload fixes it.

`dolly uninstall` is the undo: it stops and disables `dolly.timer` (fine if it isn't loaded), removes both units (and the links to them, when the user manager has another home, see below), reloads systemd, and removes `~/.local/bin/dolly` — the config and any secrets next to it are kept.

## Environment gaps in service-account setups

Service accounts reached via `su -` or `sudo -iu` often lack a full session environment. `dolly install` handles the common gaps:

- **`XDG_RUNTIME_DIR` unset.** This is typical after `su -` or `sudo -iu`, and normally makes `systemctl --user` fail with "Failed to connect to bus." If `/run/user/<uid>` exists, Dolly sets `XDG_RUNTIME_DIR` and `DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/<uid>/bus` for its own `systemctl --user` calls. If `/run/user/<uid>` doesn't exist, the account has no active session and no linger enabled: Dolly still installs the binary, config, and units, then exits `1` and tells you to run `loginctl enable-linger <user>` (may need an admin to run it for the service account) and re-run `dolly install`.
- **`~/.local/bin` missing or not on `PATH`.** Dolly creates the directory if needed, and warns (without failing) if it isn't on `PATH`. This doesn't matter for the timer itself, since the unit references the absolute path `%h/.local/bin/dolly` and never depends on `PATH`.
- **Login `$HOME` differs from the passwd home.** On AD/SSSD hosts the login shell often has `HOME=/home/<user>` while the passwd entry, and so the `systemd --user` manager, uses another directory such as `/home/<DOMAIN>/<user>`. The manager then looks for units in its own `~/.config/systemd/user/` and expands `%h` to its own home, so units written only under your `$HOME` would be "not found". `$HOME` always wins: `dolly install` asks the manager for its `HOME` and `XDG_CONFIG_HOME` (`systemctl --user show-environment`), keeps the binary, config, and units under your `$HOME` as usual, and registers the two units with `systemctl --user link`, so systemd creates symlinks in its own unit directory (Dolly writes nothing there itself). `ExecStart` then uses absolute paths, the binary and `--config` (the default config's path if you didn't pass one), for example `ExecStart=/home/peter/.local/bin/dolly sync --config /home/peter/.config/dolly/dolly.yaml`, and the install output has a note explaining the split. Re-running `dolly install` rewrites the files in place (the links follow), re-creates a missing link, and leaves an existing regular `dolly.service` or `dolly.timer` in the manager's directory alone with a warning. `systemctl --user enable --now dolly.timer` works on linked units as usual. `dolly uninstall` removes the links only if they point at Dolly's files. If `show-environment` fails, Dolly assumes the manager uses your `$HOME`.
- **Non-Linux hosts.** Without systemd, `dolly install` installs the binary and config, skips writing any units, and says so.

Dolly never edits shell startup files (`.bashrc`, `.profile`, and so on) to fix any of this.

## Several targets from one host

There is no multi-target mode — each target LDAP server needs its own config file, state base, and run lock. To sync more than one target from a single host, keep one `dolly.yaml` per target and pass `--config` explicitly for each, either as separate `dolly.service` units (one per target, each with its own `--config` baked in by `dolly install --config`) or by wrapping calls to `dolly sync --config /path/to/target-a.yaml` and `--config /path/to/target-b.yaml` in your own scheduling. See [Configuration](../configuration.md#config-lookup-order).
