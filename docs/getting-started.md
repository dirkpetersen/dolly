# Getting started

This page walks through installing Dolly, configuring it, and running your first sync.

## Install

Install with Go:

```bash
go install github.com/dirkpetersen/dolly/cmd/dolly@latest
```

Or download a static binary from the project's [GitHub Releases](https://github.com/dirkpetersen/dolly/releases). Releases are built for Linux and macOS, on amd64 and arm64, with no runtime dependencies. Each release archive also contains `LICENSE`, `README.md`, and `dolly.yaml.template`.

Dolly runs as a regular user by default and needs no root.

## Self-install

```bash
dolly install
```

This copies the binary to `~/.local/bin`, creates `$XDG_CONFIG_HOME/dolly/dolly.yaml` from the built-in template if one doesn't already exist, and writes the `systemd --user` units. Re-running `dolly install` is always safe: it never overwrites an existing config, and it only rewrites the systemd units when their content has changed. See [Scheduling](operations/scheduling.md) for what the units look like and how to handle service-account environments.

## Edit the configuration

```bash
$EDITOR ~/.config/dolly/dolly.yaml
```

At minimum, fill in your AD source (URLs, bind DN, bind password, user and group bases and filters) and your target LDAP server (URL, bind DN, bind password, and the base DNs for users, groups, and Dolly's own state). Each bind password can be set inline (`bind_password`) or in a separate file (`bind_password_file`) — see [Passwords](configuration.md#passwords). If you use an inline password, run `chmod 600 ~/.config/dolly/dolly.yaml` so Dolly will run with it. See [Configuration](configuration.md) for the full annotated file and a reference table for every key.

## Check connectivity

```bash
dolly check
```

`dolly check` tests connectivity and binds to both AD and the target, verifies the target's containers exist, checks for a truncated search (a sign that `olcSizeLimit` is too low for Dolly's bind DN), and tests SMTP if notifications are configured. Fix anything it reports before syncing.

## Adopt an existing tree

If you're migrating from [ad2openldap](https://github.com/dirkpetersen/ad2openldap) or otherwise already have a target tree with no Dolly ownership records, adopt it once before your first sync:

```bash
dolly adopt --dry-run
dolly adopt
```

See [Adopting an existing tree](operations/adopting.md) for what this does and its caveats. Skip this step if the target is empty or new.

## Dry run

```bash
dolly sync --dry-run
```

This prints the full plan — every add, modify, rename, and removal — without writing anything to the target. Review it before running for real, especially the first time.

## First sync

```bash
dolly sync
```

This syncs both users and groups (the default when neither `--users` nor `--groups` is given): it reads every in-scope user and group from AD, then applies the plan. Per-entry errors are logged and don't abort the run; see [How it works](how-it-works/index.md) for the full algorithm.

## Enable the timer

```bash
systemctl --user enable --now dolly.timer
loginctl enable-linger "$USER"   # keep the timer running while you're logged out
```

The timer runs `dolly sync` on a schedule (every 15 minutes by default). `loginctl enable-linger` keeps your systemd `--user` session alive across logouts, which a service account usually needs. See [Scheduling](operations/scheduling.md) for details, including what happens when `XDG_RUNTIME_DIR` isn't set.
