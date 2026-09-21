# 🐑 Dolly

> Hello, Dolly. Clones your Active Directory users and groups into OpenLDAP (or any LDAP server), one sheep at a time.

Dolly is a small, single-binary tool written in Go that replicates users and groups **one way** from Microsoft Active Directory to OpenLDAP or another standards-compliant LDAP directory. It exists for the common case where AD is the source of truth, but your Linux systems, HPC clusters, or legacy applications want a plain POSIX-friendly LDAP tree they can query without learning Microsoft's dialect.

It is deliberately simple. Dolly is not a bidirectional sync engine, an identity management platform, or a password bridge. It reads from AD, maps attributes, and writes to LDAP.

> **Status:** early development. Expect breaking changes to config and behavior before v1.0.

---

## Features

- **One-way replication** of users and groups from AD to a target LDAP server
- **Configurable attribute mapping**, e.g. `sAMAccountName` → `uid` and `user` → `inetOrgPerson` + `posixAccount`
- **Group membership rewriting**, which translates AD member DNs into target DNs (`member`, `uniqueMember`, or `memberUid`)
- **Incremental sync** using `uSNChanged`, so only changed entries are fetched after the first run
- **Full reconcile mode** that detects and optionally prunes entries removed from AD
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

# Create a config
cp config.example.yaml dolly.yaml
$EDITOR dolly.yaml

# See what would happen
dolly sync --config dolly.yaml --dry-run

# Do it for real
dolly sync --config dolly.yaml
```

## Configuration

```yaml
source:
  url: ldaps://dc01.example.edu:636
  bind_dn: CN=svc-dolly,OU=Service Accounts,DC=example,DC=edu
  bind_password_file: /etc/dolly/ad.secret
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
  bind_dn: cn=dolly,dc=example,dc=edu
  bind_password_file: /etc/dolly/ldap.secret
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
  prune: false                      # delete target entries missing from AD
  state_file: /var/lib/dolly/state.json
```

Attribute values can be plain AD attribute names or Go templates for derived values.

## Commands

| Command | What it does |
|---|---|
| `dolly sync` | Incremental sync based on the last recorded `uSNChanged` |
| `dolly sync --full` | Full reconcile of every in-scope entry |
| `dolly sync --dry-run` | Prints planned adds, modifies, and deletes without writing |
| `dolly diff` | Compares source and target and reports differences |
| `dolly check` | Tests connectivity, binds, and search scopes |
| `dolly version` | Prints the version |

## How it works

1. **Bleat.** Dolly binds to AD and runs paged searches for in-scope users and groups. In incremental mode it only asks for entries with `uSNChanged` greater than the last high-water mark.
2. **Shear.** Each entry is mapped through the configured attribute rules. Group members are resolved and rewritten as target DNs or bare uids.
3. **Clone.** Dolly compares mapped entries against the target and issues the minimal set of LDAP add, modify, and delete operations.
4. **Remember.** The new high-water mark is saved so the next run picks up where this one left off.

> **Note on `uSNChanged`:** USNs are local to each domain controller. Dolly tracks the DC's `invocationId`, and if it connects to a different DC it falls back to a full sync automatically.

## Running it on a schedule

**systemd timer**

```ini
# /etc/systemd/system/dolly.service
[Service]
Type=oneshot
ExecStart=/usr/local/bin/dolly sync --config /etc/dolly/dolly.yaml

# /etc/systemd/system/dolly.timer
[Timer]
OnCalendar=*:0/15
Persistent=true

[Install]
WantedBy=timers.target
```

**Container**

```bash
docker run --rm -v /etc/dolly:/etc/dolly:ro -v dolly-state:/var/lib/dolly \
  ghcr.io/<your-org>/dolly sync --config /etc/dolly/dolly.yaml
```

A nightly `dolly sync --full` alongside frequent incremental runs is a good default, because it catches deletions and anything the incremental pass missed.

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
