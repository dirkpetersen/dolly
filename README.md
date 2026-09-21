# 🐑 Dolly

> Hello, Dolly. Clones your Active Directory users and groups into OpenLDAP (or any LDAP server), one sheep at a time.

Dolly is a small, single-binary tool written in Go that replicates users and groups **one way** from Microsoft Active Directory to OpenLDAP or another standards-compliant LDAP directory. It exists for the common case where AD is the source of truth, but your Linux systems, HPC clusters, or legacy applications want a plain POSIX-friendly LDAP tree they can query without learning Microsoft's dialect.

It is deliberately simple. Dolly is not a bidirectional sync engine, an identity management platform, or a password bridge. It reads from AD, maps attributes, and writes to LDAP.

> **Status:** early development. Expect breaking changes to config and behavior before v1.0.

---

## Background and design requirements

Dolly is a new, simplified reimplementation of [ad2openldap](https://github.com/dirkpetersen/ad2openldap), a 15-year-old project that still runs reliably in production.

- **Learn from the old code's corner cases, not its issues.** ad2openldap has known problems, so don't copy its design wholesale. It does handle many real-world edge cases, though, and Dolly should handle them too.
- **No rebuilds.** Every run makes small, targeted changes to a live server. Normal operation never wipes the target and starts from scratch.
- **Native LDAP only.** Dolly talks to both servers over the LDAP protocol. It never shells out to `ldapmodify` or `slapadd`, writes no LDIF files, never restarts slapd, and needs no root.
- **Only touch what Dolly manages.** Target groups may contain extra members that were added directly on the LDAP server and don't exist in AD. Dolly only changes the members it previously replicated from AD:
  - A user added to the group in AD is added to the target group.
  - A user removed from the group in AD, or deleted from AD, is removed from the target group.
  - A member added directly on the LDAP server stays untouched.
- **Users and groups are independent.** Dolly can sync users only, groups only, or both, and each can run on its own.
- **Users and groups only.** No other object types are in scope.

---

## Features

- **One-way replication** of users and groups from AD to a target LDAP server
- **Non-destructive group membership.** Only members Dolly added are ever removed. Members added directly on the LDAP server are left alone.
- **Ownership records stored in the target LDAP**, so what Dolly manages survives host rebuilds and doesn't depend on a local file
- **Full comparison every run.** Dolly reads all in-scope users and groups each time, so deletions and nested-group changes are never missed. About 10,000 groups is a few seconds of reading.
- **Nested groups are flattened** into direct user members, with loop protection
- **Rename handling** by tracking AD's `objectGUID`, so a renamed user or group is renamed in place rather than deleted and re-created
- **Mass-deletion guard** that refuses runs removing more than a set share of managed memberships or users, and any run based on an incomplete read
- **Run lock stored in LDAP**, so two hosts never sync at once. Stale locks expire, and `dolly unlock` clears one by hand
- **Email notifications** without the flood: one mail on the first failure, a reminder at most every `remind_every`, and one on recovery
- **Local overrides survive.** Attributes listed as `create_only` are set when a user is created and then left to the LDAP admins
- **Several domain controllers** can be listed and are tried in order
- **Users and groups sync separately**, so you can run users only, groups only, or both
- **Configurable attribute mapping** with Go templates for defaults and derived values
- **Dry-run mode** that shows exactly what would change before anything is touched
- **Paged searches and ranged attribute retrieval**, so neither AD's 1000-result limit nor its 1500-value limit on large groups silently truncates data
- **LDAPS and StartTLS** support on both ends
- Single static binary with no runtime dependencies

## What Dolly does *not* do

- **Passwords.** AD doesn't expose password hashes over LDAP, and that's a good thing. Point your target systems at AD or Kerberos for authentication, or use SASL pass-through.
- **Write back to AD.** Changes flow in one direction only.
- **Replicate arbitrary object types.** It handles users and groups only, not computers, GPOs, contacts, NIS netgroups, or automount maps.
- **Delete groups.** A group removed from AD loses its AD-sourced members, but the group itself stays on the target.

---

## Quick start

```bash
# Install
go install github.com/dirkpetersen/dolly/cmd/dolly@latest

# Self-install for the current (unprivileged) user: binary, config, systemd --user units
dolly install
$EDITOR ~/.config/dolly/dolly.yaml
dolly check

# Taking over a tree written by ad2openldap? Adopt it once first:
dolly adopt --dry-run
dolly adopt

# See what would happen
dolly sync --dry-run

# Do it for real, then let the timer take over
dolly sync
systemctl --user enable --now dolly.timer
```

Dolly runs as a regular user by default and needs no root.

## Configuration

The defaults match the tree that ad2openldap produced, so existing clients keep working.

**One Dolly per target.** Each target LDAP server has its own ownership records and run lock, so independent LDAP servers each get their own Dolly instance and config. To serve several targets from one host, keep one config file per target and pass `--config`.

```yaml
source:
  urls:                               # tried in order
    - ldaps://dc01.example.edu:636
    - ldaps://dc02.example.edu:636
  ca_file: ad-ca.pem                  # optional, see "TLS certificates"
  bind_dn: CN=svc-dolly,OU=Service Accounts,DC=example,DC=edu   # a UPN such as svc-dolly@example.edu also works
  bind_password: ""                   # inline password, or use bind_password_file (set only one)
  bind_password_file: ad.secret       # relative paths resolve against the config file's directory
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
  ca_file: ldap-ca.pem                # optional, see "TLS certificates"
  bind_dn: cn=admin,dc=local
  bind_password: ""                   # inline password, or use bind_password_file (set only one)
  bind_password_file: ldap.secret
  users_base: ou=people,dc=local
  groups_base: ou=group,dc=local
  state_base: ou=dolly,dc=local       # Dolly's ownership records and run lock, see "Ownership records"
  empty_group_member: cn=empty,dc=local   # placeholder member for groups that would otherwise be empty

mapping:
  users:
    rdn: uid
    object_classes: [account, posixAccount]
    required: [uid, uidNumber, gidNumber]   # AD users missing any of these are ignored
    create_only: [loginShell, homeDirectory]  # set on creation, then never overwritten (local overrides survive)
    attributes:
      uid: uid
      cn: uid
      uidNumber: uidNumber
      gidNumber: gidNumber
      homeDirectory: '{{ or .unixHomeDirectory (printf "/home/%s" .uid) }}'
      loginShell: '{{ or .loginShell "/bin/bash" }}'
      gecos: gecos
  groups:
    rdn: cn
    object_classes: [groupOfNames, posixGroup]
    required: [name, gidNumber]       # AD groups missing any of these are ignored, also when flattening
    attributes:
      cn: name
      gidNumber: gidNumber
    membership:
      - attribute: member             # full DN of the target user
      - attribute: memberUid          # bare uid
    flatten_nested: true

sync:
  prune_users: false                  # delete Dolly-owned users that are gone from AD (groups are never deleted)
  prune_after_days: 30                # how long a user must be gone from AD before the entry is deleted
  disabled_shell: /sbin/nologin       # loginShell for disabled AD accounts; "" leaves the shell alone
  max_delete_percent: 10              # abort if a run would remove more than this share; --force overrides
  max_delete_min: 25                  # ...but never abort for this many removals or fewer
  lock_ttl: 60m                       # a run lock older than this is treated as stale and broken
  run_timeout: 45m                    # a run aborts itself after this long; must be shorter than lock_ttl
  network_timeout: 30s                # connect and per-operation timeout

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

**Passwords.** Each password can be given inline (`bind_password`, `password`) or in a separate file (`bind_password_file`, `password_file`). Setting both to a non-empty value is a config error; an empty value counts as unset. If `dolly.yaml` contains an inline password, Dolly refuses to run unless the file is readable only by its owner (mode `0600` or stricter), the same way `ssh` treats private keys. `dolly.yaml` is git-ignored, so an inline password never ends up in the repository.

Attribute values are plain AD attribute names or Go templates. Dolly only writes the attributes listed in the mapping, so other attributes on an entry are left alone. To exclude users or groups, use the search `filter`, for example `(!(memberOf=CN=ExcludedFromLDAPSync,OU=Groups,DC=example,DC=edu))`.

### File locations (XDG)

| What | Default path |
|---|---|
| Binary | `~/.local/bin/dolly` |
| Config | `$XDG_CONFIG_HOME/dolly/dolly.yaml` (`~/.config/dolly/`), created by `dolly install` with mode `0600` |
| Secrets and CA files | next to the config, mode `0600` |
| systemd units | `$XDG_CONFIG_HOME/systemd/user/dolly.{service,timer}` |
| Logs | journald (`journalctl --user -u dolly`) |

Config lookup order: `--config`, then `./dolly.yaml`, then `$XDG_CONFIG_HOME/dolly/dolly.yaml`. Dolly keeps no local state. Ownership records and the run lock live in the target LDAP.

## Commands

| Command | What it does |
|---|---|
| `dolly sync` | Reads all in-scope users and groups from AD and applies the differences |
| `dolly sync --users` | Syncs users only |
| `dolly sync --groups` | Syncs groups only (the default without either flag is both) |
| `dolly sync --dry-run` | Prints planned adds, modifies, renames, and removals without writing |
| `dolly sync --force` | Applies the run even if it trips the mass-deletion guard |
| `dolly adopt` | One-time takeover of an existing tree, such as one written by ad2openldap. See "Adopting an existing tree" |
| `dolly unlock` | Shows the current run lock and removes it after asking for confirmation (`--yes` skips the prompt). Use it after a crash |
| `dolly check` | Tests connectivity, binds, search scopes, the target's containers and size limit, and SMTP |
| `dolly install` | Copies the binary to `~/.local/bin`, creates the config from the built-in template if missing, and writes the `systemd --user` units |
| `dolly uninstall` | Removes the units and binary, and keeps the config |
| `dolly version` | Prints the version |

Exit codes: `0` for success, or when another host holds the lock. `1` for an error. `2` when the mass-deletion guard stopped the run.

## How it works

1. **Lock.** Dolly takes the run lock in the target LDAP. If another host holds it, Dolly exits quietly with code 0. `--dry-run` takes no lock. See "Run lock".
2. **Bleat.** Dolly binds to AD and runs paged searches for all in-scope users and groups. Large `member` attributes are fetched with ranged retrieval (`member;range=…`). Group members outside the configured search bases are followed and fetched by DN. A groups-only run still reads the AD users, read-only, to resolve members. If any read from AD or the target fails or comes back truncated (for example `sizeLimitExceeded`), the run aborts. Dolly never plans from partial data.
3. **Shear.** Each entry is mapped through the configured attribute rules. Entries missing a `required` attribute are ignored, and the run summary lists them once rather than warning every run. Nested groups are flattened. Members are resolved by DN to the user's `objectGUID` and then to the target DN and uid, never by CN, because CNs aren't unique and can contain escaped commas.
4. **Compare.** Dolly reads the target entries and its ownership records under `state_base`, then plans adds, attribute modifies, renames (`modrdn`), and member additions and removals.
5. **Guard.** Dolly counts removals across the whole run, separately for memberships and for users. If either count is above `max_delete_min` and above `max_delete_percent` of what Dolly owns, or if AD returned no users or no groups at all, Dolly aborts, sends a notification, and changes nothing. `--force` overrides the guard.
6. **Clone.** Dolly applies the plan in a crash-safe order: it writes the ownership record *before* adding an entry or member, and removes the entry or member *before* removing its record. A crash can then never leave a Dolly-added member looking like a local one. Finally it updates the status entry and releases the lock.

### Ownership records

Dolly records what it manages in the target LDAP under `state_base`, using only the standard core schema, so no schema changes are needed:

```text
ou=dolly,dc=local
├── ou=users
│   └── cn=<objectGUID>   objectClass: organizationalRole
│                         seeAlso: uid=jdoe,ou=people,dc=local
├── ou=groups
│   └── cn=<objectGUID>   objectClass: organizationalRole
│                         seeAlso: cn=hpc-users,ou=group,dc=local
│                         roleOccupant: uid=jdoe,ou=people,dc=local   (one per member Dolly added)
├── cn=lock                                                              (only while a run is active)
└── cn=status             last successful run, current failure, last notification
```

Dolly creates `state_base` and its children if they're missing. It doesn't create `users_base` or `groups_base`. If those are missing, it stops with a clear error. User records also keep small `description` values, such as the date a user went missing from AD and the shell to restore after a disabled account is re-enabled.

The rules:

- A user in the AD group but not in the target group is added and recorded as Dolly-owned.
- A Dolly-owned member who left the AD group, or was deleted from AD, is removed from the target group and from the record.
- A member who is already in the target group without a record was added locally. Dolly never removes that member, even if the same user is also in the AD group.
- A target user or group with the same `uid` or `cn` as an AD entry but no ownership record is a conflict. Dolly logs an error and skips it.
- A user or group renamed in AD (same `objectGUID`) is renamed in place, and its `member` and `memberUid` values are updated in every group.
- A user who is gone from AD (deleted, moved out of scope, or missing a `required` attribute) loses all Dolly-owned memberships right away. The user entry is deleted only when `prune_users` is on and the user has been gone for `prune_after_days`. At that point Dolly also removes the user from groups where they were added locally, so no group points at a user that no longer exists.
- A group deleted from AD loses its Dolly-owned members and its ownership record. The group itself stays.
- A new AD group with no resolvable members isn't created until it has one, because `groupOfNames` needs at least one `member`.
- If removing Dolly-owned members would leave a group with no members at all, Dolly adds the `empty_group_member` placeholder instead of breaking the `groupOfNames` schema. It removes the placeholder once the group has a real member again.
- A disabled AD account (`userAccountControl` bit `0x2`) keeps its user entry, but Dolly removes it from every group where it's Dolly-owned and sets `loginShell` to `disabled_shell`, because an SSH key would otherwise still work. Local memberships are untouched. When the account is re-enabled, Dolly restores the previous shell.
- Attributes in `create_only` are written when Dolly creates the user and never again, so a shell or home directory changed on the LDAP server stays changed. `disabled_shell` is the one exception.
- The AD primary group (`primaryGroupID`, usually Domain Users) is ignored, because AD doesn't list it in the group's `member` attribute.
- Users need `uid`, `uidNumber`, and `gidNumber`, and groups need `name` and `gidNumber`. Anything without them is ignored everywhere, inside or outside the search bases. An ignored child group isn't flattened into its parent either.
- A member outside the configured search bases that has the required attributes is followed. An out-of-scope user becomes a normal Dolly-managed user. An out-of-scope child group is only flattened into its parent and isn't created on the target.
- Members that are neither users nor groups (computers, contacts, foreign security principals) are skipped.
- The spelling of `uid` is kept exactly as it is in AD, including uppercase letters.
- Duplicate `uidNumber` or `gidNumber` values, between AD entries or against local entries, don't block the run. They're listed as warnings in the run summary.
- A changed `uidNumber` or `gidNumber` is applied, and it's always reported, because file ownership depends on it.
- Values for ASCII-only attributes such as `gecos` are transliterated to ASCII, because OpenLDAP rejects non-ASCII characters there.
- One bad entry doesn't stop the run. If the server rejects a change, Dolly logs it, carries on with the rest, reports it in the summary, and exits with code 1.

### Run lock

Before writing anything, Dolly creates `cn=lock,<state_base>` with an LDAP add. The add is atomic, so only one host can hold the lock. The entry records the host, PID, and start time. Dolly deletes it when the run ends, even after an error or a `SIGTERM`/`SIGINT`. A run also aborts itself after `run_timeout`, which must be shorter than `lock_ttl`, so a live run never has its lock broken.

If a run crashes and leaves the lock behind:

- A lock older than `lock_ttl` (by the server's `createTimestamp`) is treated as stale. The next run breaks it by first *renaming* it (`modrdn`), which only one host can win, and then deleting the renamed entry. It logs a warning and sends a notification.
- `dolly unlock` shows who holds the lock and since when, and removes it after confirmation.

### Adopting an existing tree

A tree written by ad2openldap has no ownership records. Run `dolly adopt` once before the first sync. For every AD user and group that matches a target entry by `uid` or `cn`, it creates an ownership record. In each group it marks the members who are also in the AD group as Dolly-owned, and treats every other member as local.

One caveat: a user removed from an AD group shortly before the adoption looks local, so Dolly will never remove them. Check `dolly adopt --dry-run` for surprises.

A second, intended difference from ad2openldap: users without a `gidNumber` (the old tool gave them 65534) and members that only arrived through a child group without a `gidNumber` are now ignored. `dolly adopt --dry-run` lists them. On the first sync they lose their memberships, the guard may ask for `--force`, and with `prune_users` on they're deleted after `prune_after_days`.

## Running it on a schedule

**systemd user timer (default)**

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

`dolly install` is safe to re-run. It never overwrites an existing config and only rewrites the units when they changed. It also handles a few common gaps in a service account's environment:

- **`XDG_RUNTIME_DIR` is unset** (typical after `su -` or `sudo -iu`, where `systemctl --user` fails with "Failed to connect to bus"). If `/run/user/<uid>` exists, Dolly points `XDG_RUNTIME_DIR` and `DBUS_SESSION_BUS_ADDRESS` at it for its own `systemctl --user` calls. If it doesn't exist, the user has no session and no linger, and Dolly stops and tells you to run `loginctl enable-linger <user>`. Dolly never edits your shell startup files.
- **`~/.local/bin` is missing or not on `PATH`.** Dolly creates the directory and warns if it isn't on `PATH`. The timer doesn't care, because the unit uses the absolute path `%h/.local/bin/dolly`.
- **Not Linux.** Without systemd (for example macOS), Dolly installs the binary and config, skips the units, and says so.

## Notifications

With `on: changes`, Dolly mails when a run made changes. Failures are always reported, whichever setting you choose. Dolly sends at most **one email per run**. It's a summary of everything that happened: counts, then the adds, removals, renames, changed ID numbers, warnings, and errors. A hundred changed users is still one mail.

Dolly keeps a `cn=status` entry under `state_base` with the last successful run, the current failure, and when it last sent mail. With `on: failure` it sends one mail when a failure first appears, a reminder at most every `remind_every` while it persists, and one mail when the run succeeds again. So an AD outage overnight is two or three mails, not 96. Ignored entries and conflicts are listed in the run summary and mailed only when the list changes.

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
- **Target LDAP:** the bind DN needs write access to `users_base`, `groups_base`, and `state_base`, and ideally nothing else. It must also be able to read *every* entry under them. OpenLDAP's default `olcSizeLimit` (500, or 10000 in the ad2openldap config) truncates searches for any DN except the rootdn, so either bind as the rootdn or raise the limit for Dolly's DN with `olcLimits`. Dolly aborts on a truncated read, and `dolly check` tests for it.

## Building from source

```bash
git clone https://github.com/dirkpetersen/dolly.git
cd dolly
go build -o dolly ./cmd/dolly
go test ./...
```

Requires Go 1.22 or later. Built on [go-ldap/ldap](https://github.com/go-ldap/ldap).

## Releasing

CI (`.github/workflows/ci.yml`) runs `gofmt`, `go vet`, `go test -race`, and a GoReleaser config check on every push and pull request that touches Go code.

To cut a release, push a semver tag:

```bash
git tag -a v0.1.0 -m "v0.1.0"
git push origin v0.1.0
```

`.github/workflows/release.yml` then runs the tests and [GoReleaser](https://goreleaser.com), which publishes static binaries for Linux and macOS (amd64 and arm64), `checksums.txt`, and a changelog to the GitHub release. Each archive also contains `LICENSE`, `README.md`, and `dolly.yaml.template`. `dolly version` prints the tag, commit, and build date.

## Contributing

Issues and pull requests are welcome. Please include a dry-run output or a minimal LDIF example when reporting mapping bugs, with anything sensitive redacted.

## License

MIT. See [LICENSE](LICENSE).

---

*No sheep were harmed in the replication of this directory.*
