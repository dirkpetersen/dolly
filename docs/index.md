# 🐑 Dolly

> Hello, Dolly. Clones your Active Directory users and groups into OpenLDAP (or any LDAP server), one sheep at a time.

Dolly is a small, single-binary tool written in Go that replicates users and groups **one way** from Microsoft Active Directory to OpenLDAP or another standards-compliant LDAP directory. It exists for the common case where AD is the source of truth, but your Linux systems, HPC clusters, or legacy applications want a plain POSIX-friendly LDAP tree they can query without learning Microsoft's dialect.

It is deliberately simple. Dolly is not a bidirectional sync engine, an identity management platform, or a password bridge. It reads from AD, maps attributes, and writes to LDAP.

!!! note "Status"
    Dolly is in early development. Expect breaking changes to config and behavior before v1.0.

## Quick start

You need no Go and no root.

```bash
# 1. Download the binary for this machine (Linux or macOS, amd64 or arm64)
curl -fsSL "https://github.com/dirkpetersen/dolly/releases/latest/download/dolly_$(uname -s | tr A-Z a-z)_$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/').tar.gz" | tar -xz dolly

# 2. Install it: binary to ~/.local/bin, config from the template, systemd --user units
./dolly install

# 3. Set up servers, certificates, bind accounts and OUs interactively
~/.local/bin/dolly config

# 4. See what Dolly would do. This writes nothing.
~/.local/bin/dolly sync --dry-run
```

- Run these steps as a regular user, never as root.
- If `~/.local/bin` is on your `PATH`, you can type `dolly` instead of the full path.
- `dolly config` asks before it trusts each server certificate and before it writes any file.
- Read the plan. Then run `dolly sync`, and enable the timer with `systemctl --user enable --now dolly.timer`. `dolly install` does not enable it.
- To check the download, compare its SHA-256 with `checksums.txt` in the same release.
- Full documentation: [dirkpetersen.github.io/dolly](https://dirkpetersen.github.io/dolly/).

## Who it's for

Dolly is for administrators who run Active Directory as the identity source of truth but need a standard LDAP tree — for Linux authentication, HPC clusters, or applications that speak LDAP but not AD's dialect. It's a modern, simplified replacement for [ad2openldap](https://github.com/dirkpetersen/ad2openldap), a 15-year-old Python tool of the same kind.

## Key guarantees

- **Native LDAP only.** Dolly talks to both AD and the target over the LDAP protocol. It never shells out to `ldapmodify` or `slapadd`, and it never writes LDIF files.
- **Local members are never touched.** Target groups can hold members added directly on the LDAP server. Dolly only adds and removes the members it manages itself; everything else is left alone.
- **Ownership lives in LDAP, not in a local file.** What Dolly manages is recorded in the target directory itself, so it survives a host rebuild and doesn't depend on local state.
- **No rebuilds.** Every run reads the full current state from AD and the target, then applies minimal, targeted adds, modifies, renames, and removals. Normal operation never wipes the target and starts over.
- **Users and groups only.** No computers, contacts, NIS netgroups, or automount maps, even though its predecessor exported some of those.

## What Dolly does not do

- **Passwords.** AD doesn't expose password hashes over LDAP, and that's a good thing. Point your target systems at AD or Kerberos for authentication, or use SASL pass-through.
- **Write back to AD.** Changes flow in one direction only.
- **Replicate arbitrary object types.** Users and groups only — not computers, GPOs, contacts, NIS netgroups, or automount maps.
- **Delete groups.** A group removed from AD loses its AD-sourced members, but the group itself stays on the target.

## Where to go next

- [Getting started](getting-started.md) — install Dolly, write a config, and run your first sync.
- [Configuration](configuration.md) — the full annotated config file and a reference for every key.
- [Commands](commands.md) — every subcommand, flag, and exit code.
- [How it works](how-it-works/index.md) — the run algorithm, ownership rules, and the run lock.
- [Operations](operations/adopting.md) — adopting an existing tree, scheduling, notifications, TLS, and permissions.
- [Development](development.md) — building from source and contributing.
