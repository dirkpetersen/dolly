# Getting started

This page walks through installing Dolly, configuring it, and running your first sync. The short version is the [Quick start](index.md#quick-start) on the home page. The sections below give the same steps in more detail.

## Install

The quick start downloads a release binary with `curl`. Or install with Go:

```bash
go install github.com/dirkpetersen/dolly/cmd/dolly@latest
```

Or download a static binary by hand from the project's [GitHub Releases](https://github.com/dirkpetersen/dolly/releases). Releases are built for Linux and macOS, on amd64 and arm64, with no runtime dependencies. Each release archive also contains `LICENSE`, `README.md`, and `dolly.yaml.template`.

Dolly runs as a regular user by default and needs no root.

## Self-install

```bash
dolly install
```

This copies the binary to `~/.local/bin`, creates `$XDG_CONFIG_HOME/dolly/dolly.yaml` from the built-in template if one doesn't already exist, and writes the `systemd --user` units. For a groups-only deployment, use `dolly install --groups` instead, which bakes `--groups` into the unit's `ExecStart` (see [Groups-only deployments](configuration.md#groups-only-deployments)). Re-running `dolly install` is always safe: it never overwrites an existing config, replaces the binary only when it differs, and only rewrites the systemd units when their content has changed. It prints the next steps and doesn't enable the timer itself. See [Scheduling](operations/scheduling.md) for what the units look like and how to handle service-account environments.

## Quick start with `dolly config`

```bash
dolly config
```

`dolly config` asks for the AD and target servers, shows each server's certificate and pins it once you trust it, tests each bind right away, checks the bases, and asks for the group schema. The passwords go to `ad.secret` and `ldap.secret` next to the config (mode `0600`). Nothing is written until you confirm the summary, and then it runs `dolly check`. See [Commands](commands.md#dolly-config). Edit `notify` by hand afterwards.

## Edit the configuration

```bash
$EDITOR ~/.config/dolly/dolly.yaml
```

At minimum, fill in your AD source (URLs, bind DN, bind password, user and group bases and filters) and your target LDAP server (URL, bind DN, bind password, and the base DNs for users, groups, and Dolly's own state). Each bind password can be set inline (`bind_password`) or in a separate file (`bind_password_file`) — see [Passwords](configuration.md#passwords). If you use an inline password, run `chmod 600 ~/.config/dolly/dolly.yaml` so Dolly will run with it. See [Configuration](configuration.md) for the full annotated file and a reference table for every key.

## Check connectivity

```bash
dolly check
dolly check --groups   # for a groups-only deployment: a truncated users_base is a warning, not a failure
```

`dolly check` runs every check it can, even after one fails, and never writes to AD or the target. It prints a `✓`/`✗`/`!` line per check: the config (including password files), each AD domain controller and its search bases, the target's TLS/bind and containers (with a size-limit check for a truncated read — a sign that `olcSizeLimit` is too low for Dolly's bind DN), and SMTP if `notify` is configured (add `--send-test-mail` to actually send a test message). Fix anything it reports before syncing; see [Commands](commands.md#dolly-check).

## Adopt an existing tree

If you're migrating from [ad2openldap](https://github.com/dirkpetersen/ad2openldap) or otherwise already have a target tree with no Dolly ownership records, adopt it once before your first sync:

```bash
dolly adopt --dry-run -v
dolly adopt
```

See [Adopting an existing tree](operations/adopting.md) for what this does and its caveats. Skip this step if the target is empty or new.

## Dry run

```bash
dolly sync --dry-run -v
```

This prints the full plan (every add, modify, rename, and removal) without writing anything to the target. Without `-v`, it prints the summary, base creation, and one count line for the warnings. Review it before running for real, especially the first time.

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
