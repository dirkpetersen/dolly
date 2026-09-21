# 🐑 Dolly

> Hello, Dolly. Clones your Active Directory users and groups into OpenLDAP (or any LDAP server), one sheep at a time.

Dolly is a small, single-binary tool written in Go that replicates users and groups **one way** from Microsoft Active Directory to OpenLDAP or another standards-compliant LDAP directory. It exists for the common case where AD is the source of truth, but your Linux systems, HPC clusters, or legacy applications want a plain POSIX-friendly LDAP tree they can query without learning Microsoft's dialect.

It is deliberately simple. Dolly is not a bidirectional sync engine, an identity management platform, or a password bridge. It reads from AD, maps attributes, and writes to LDAP.

> **Status:** early development. Expect breaking changes to config and behavior before v1.0.

---

## Background and design requirements

Dolly is a new, simplified reimplementation of [ad2openldap](https://github.com/dirkpetersen/ad2openldap), a 15-year-old project that still runs reliably in production. A local checkout lives at `../ad2openldap`. Use `ad2openldap3` (`../ad2openldap/ad2openldap/ad2openldap3`) as the main reference.

- **Learn from the old code's corner cases, not its issues.** ad2openldap has known problems, so don't copy its design wholesale. It does handle many real-world edge cases, though, and Dolly should handle them too.
- **Reliable incremental replication.** Normal operation must never require wiping the target and rebuilding it from scratch.
- **Only touch what Dolly manages.** Target groups may contain extra members that were added directly on the LDAP server and don't exist in AD. Dolly only changes the members it previously replicated from AD:
  - A user added to the group in AD is added to the target group.
  - A user removed from the group in AD, or deleted from AD, is removed from the target group.
  - A member added directly on the LDAP server stays untouched.
- **Users and groups are independent.** Dolly can sync users only, groups only, or both, and each can run on its own.
- **Users and groups only.** No other object types are in scope.

---

## Features

- **One-way replication** of users and groups from AD to a target LDAP server
- **Configurable attribute mapping**, e.g. `sAMAccountName` → `uid` and `user` → `inetOrgPerson` + `posixAccount`
- **Group membership rewriting**, which translates AD member DNs into target DNs (`member`, `uniqueMember`, or `memberUid`)
- **Non-destructive group membership**, where only members Dolly replicated from AD are added or removed, and members added directly on the LDAP server are left alone
- **Users and groups sync separately**, so you can run users only, groups only, or both
- **Incremental sync** using `uSNChanged`, so only changed entries are fetched after the first run
- **Full reconcile mode** that detects entries removed from AD and can optionally prune them. It only ever deletes entries that Dolly itself created.
- **Scoped sync** through search bases and LDAP filters, so you replicate only the OUs and groups you want
- **Disabled-account handling**, which skips, flags, or locks accounts based on `userAccountControl`
- **Dry-run mode** that shows exactly what would change before anything is touched
- **Paged searches**, so AD's 1000-result limit doesn't silently truncate your directory
- **LDAPS and StartTLS** support on both ends
- Single static binary with no runtime dependencies

## What Dolly does *not* do

- **Passwords.** AD doesn't expose password hashes over LDAP, and that's a good thing. Point your target systems at AD or Kerberos for authentication, or use SASL pass-through.
- **Write back to AD.** Changes flow in one direction only.
- **Replicate arbitrary object types.** It handles users and groups only, not computers, GPOs, or contacts.

---

## Quick start

```bash
# Install
go install github.com/dirkpetersen/dolly/cmd/dolly@latest

# Self-install for the current (unprivileged) user: binary, config, systemd --user units
dolly install
$EDITOR ~/.config/dolly/dolly.yaml

# See what would happen
dolly sync --dry-run

# Do it for real, then let the timer take over
dolly sync
systemctl --user enable --now dolly.timer
```

Dolly runs as a regular user by default and needs no root.

## Configuration

```yaml
source:
  url: ldaps://dc01.example.edu:636
  ca_file: ad-ca.pem                   # optional, see "TLS certificates"
  bind_dn: CN=svc-dolly,OU=Service Accounts,DC=example,DC=edu
  bind_password_file: ad.secret         # relative paths resolve against the config file's directory
  users:
    base: OU=People,DC=example,DC=edu
    filter: (&(objectClass=user)(objectCategory=person))
  groups:
    base: OU=Groups,DC=example,DC=edu
    filter: (objectClass=group)
  page_size: 500

target:
  url: ldap://ldap.example.edu:389
  start_tls: true
  ca_file: ldap-ca.pem                 # optional, see "TLS certificates"
  bind_dn: cn=dolly,dc=example,dc=edu
  bind_password_file: ldap.secret
  users_base: ou=people,dc=example,dc=edu
  groups_base: ou=groups,dc=example,dc=edu

mapping:
  users:
    rdn: uid
    object_classes: [inetOrgPerson, posixAccount, shadowAccount]
    attributes:
      uid: sAMAccountName
      cn: displayName
      givenName: givenName
      sn: sn
      mail: mail
      uidNumber: uidNumber          # from AD's RFC2307 attributes
      gidNumber: gidNumber
      homeDirectory: "/home/{{ .sAMAccountName }}"
      loginShell: "/bin/bash"
  groups:
    rdn: cn
    object_classes: [posixGroup, groupOfNames]
    attributes:
      cn: sAMAccountName
      gidNumber: gidNumber
      description: description
    membership:
      - attribute: member           # full DN, rewritten to target tree
      - attribute: memberUid        # bare uid, for posixGroup

sync:
  disabled_accounts: skip           # skip | include | lock
  prune: false                      # delete Dolly-managed target entries missing from AD
                                    # (group members removed in AD are always removed)
  # state_file: ~/.local/state/dolly/state.json   # default: $XDG_STATE_HOME/dolly/state.json
```

Attribute values can be plain AD attribute names or Go templates for derived values. Relative paths (`ca_file`, `bind_password_file`, `state_file`) resolve against the config file's directory.

### File locations (XDG)

| What | Default path |
|---|---|
| Binary | `~/.local/bin/dolly` |
| Config | `$XDG_CONFIG_HOME/dolly/dolly.yaml` (`~/.config/dolly/`) |
| Secrets and CA files | next to the config, mode `0600` |
| State (high-water mark, owned entries and members) | `$XDG_STATE_HOME/dolly/state.json` (`~/.local/state/dolly/`) |
| Lock file | `$XDG_RUNTIME_DIR/dolly.lock` (falls back to `/run/user/<uid>`, then `$XDG_STATE_HOME/dolly/`) |
| systemd units | `$XDG_CONFIG_HOME/systemd/user/dolly.{service,timer}` |
| Logs | journald (`journalctl --user -u dolly`) |

Config lookup order: `--config`, then `./dolly.yaml`, then `$XDG_CONFIG_HOME/dolly/dolly.yaml`.

## Commands

| Command | What it does |
|---|---|
| `dolly sync` | Incremental sync based on the last recorded `uSNChanged` |
| `dolly sync --full` | Full reconcile of every in-scope entry |
| `dolly sync --users` | Syncs users only |
| `dolly sync --groups` | Syncs groups only (the default without either flag is both) |
| `dolly sync --dry-run` | Prints planned adds, modifies, and deletes without writing |
| `dolly diff` | Compares source and target and reports differences |
| `dolly check` | Tests connectivity, binds, and search scopes |
| `dolly install` | Copies the binary to `~/.local/bin`, creates the config from the built-in template if missing, and writes the `systemd --user` units |
| `dolly uninstall` | Removes the units and binary, and keeps config and state |
| `dolly version` | Prints the version |

## How it works

1. **Bleat.** Dolly binds to AD and runs paged searches for in-scope users and groups. In incremental mode it only asks for entries with `uSNChanged` greater than the last high-water mark.
2. **Shear.** Each entry is mapped through the configured attribute rules. Group members are resolved and rewritten as target DNs or bare uids.
3. **Clone.** Dolly compares mapped entries against the target and issues the minimal set of LDAP add, modify, and delete operations. For group membership it only adds or removes members it knows came from AD. Members added directly on the target are never touched.
4. **Remember.** The new high-water mark and the set of AD-sourced entries and group members are saved, so the next run picks up where this one left off and knows exactly what it owns.

> **Note on `uSNChanged`:** USNs are local to each domain controller. Dolly tracks the DC's `invocationId`, and if it connects to a different DC it falls back to a full sync automatically.

## Running it on a schedule

**systemd user timer (default)**

`dolly install` writes these units, so you normally don't create them by hand:

```ini
# ~/.config/systemd/user/dolly.service
[Service]
Type=oneshot
ExecStart=%h/.local/bin/dolly sync

# ~/.config/systemd/user/dolly.timer
[Timer]
OnCalendar=*:0/15
Persistent=true

[Install]
WantedBy=timers.target
```

```bash
systemctl --user enable --now dolly.timer
loginctl enable-linger "$USER"   # keep the timer running while you're logged out (may need an admin)
```

`dolly install` is safe to re-run. It never overwrites an existing config and only rewrites the units when they changed. It also handles a few common gaps in a service account's environment:

- **`XDG_RUNTIME_DIR` is unset** (typical after `su -` or `sudo -iu`, where `systemctl --user` fails with "Failed to connect to bus"). If `/run/user/<uid>` exists, Dolly points `XDG_RUNTIME_DIR` and `DBUS_SESSION_BUS_ADDRESS` at it for its own `systemctl --user` calls. If it doesn't exist, the user has no session and no linger, and Dolly stops and tells you to run `loginctl enable-linger <user>`. Dolly never edits your shell startup files.
- **`~/.local/bin` is missing or not on `PATH`.** Dolly creates the directory and warns if it isn't on `PATH`. The timer doesn't care, because the unit uses the absolute path `%h/.local/bin/dolly`.
- **Not Linux.** Without systemd (for example macOS), Dolly installs the binary and config, skips the units, and says so.

**Container**

```bash
docker run --rm -v ~/.config/dolly:/config:ro -v dolly-state:/state -e XDG_STATE_HOME=/state \
  ghcr.io/<your-org>/dolly sync --config /config/dolly.yaml
```

A nightly `dolly sync --full` alongside frequent incremental runs is a good default, because it catches deletions and anything the incremental pass missed.

## TLS certificates

If the TLS handshake fails because the server's CA isn't trusted locally (common with an internal AD CA), pull the server's certificate chain and point `ca_file` at it:

```bash
openssl s_client -connect dc01.example.edu:636 -showcerts </dev/null 2>/dev/null \
  | awk '/BEGIN CERTIFICATE/,/END CERTIFICATE/' > ~/.config/dolly/ad-ca.pem
# For StartTLS on 389: openssl s_client -connect ldap.example.edu:389 -starttls ldap -showcerts
openssl x509 -in ~/.config/dolly/ad-ca.pem -noout -subject -issuer -fingerprint -sha256
```

Check the fingerprint against a trusted source before relying on it. Dolly never silently disables certificate verification.

## Permissions

- **AD:** a regular read-only service account is enough. Dolly never writes to AD.
- **Target LDAP:** the bind DN needs write access to the configured users and groups subtrees, and ideally nothing else.

## Building from source

```bash
git clone https://github.com/dirkpetersen/dolly.git
cd dolly
go build -o dolly ./cmd/dolly
go test ./...
```

Requires Go 1.22 or later. Built on [go-ldap/ldap](https://github.com/go-ldap/ldap).

## Contributing

Issues and pull requests are welcome. Please include a dry-run output or a minimal LDIF example when reporting mapping bugs, with anything sensitive redacted.

## License

MIT. See [LICENSE](LICENSE).

---

*No sheep were harmed in the replication of this directory.*
