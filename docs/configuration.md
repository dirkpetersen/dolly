# Configuration

The defaults match the tree that ad2openldap produced, so existing clients keep working.

!!! note "One Dolly per target"
    Each target LDAP server has its own ownership records and run lock, so independent LDAP servers each get their own Dolly instance and config. To serve several targets from one host, keep one config file per target and pass `--config`.

## Config lookup order

Dolly looks for a config file in this order:

1. `--config <path>`, if given
2. `./dolly.yaml` in the current directory
3. `$XDG_CONFIG_HOME/dolly/dolly.yaml` (falls back to `~/.config/dolly/dolly.yaml`)

A local `dolly.yaml` is git-ignored, because it may contain passwords. The checked-in example is `dolly.yaml.template`. Relative paths inside the config — such as `bind_password_file` or `ca_file` — resolve against the directory containing the config file, not the current working directory.

Dolly keeps no local state of its own. Ownership records and the run lock live in the target LDAP; see [How it works](how-it-works/index.md).

## Full example

This is `dolly.yaml.template`, copied verbatim. `dolly install` writes this file to `$XDG_CONFIG_HOME/dolly/dolly.yaml` if one doesn't already exist.

```yaml
# Dolly configuration template.
# `dolly install` copies this to ~/.config/dolly/dolly.yaml ($XDG_CONFIG_HOME).
# A ./dolly.yaml in the working directory also works; it is git-ignored. Never commit it.
# Defaults match the tree produced by ad2openldap.

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
  empty_group_member: cn=empty,dc=local   # placeholder for groupOfNames groups that would otherwise be empty; unused with memberUid only

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
    object_classes: [groupOfNames, posixGroup]   # RFC 2307 servers (structural posixGroup): [posixGroup] only
    required: [name, gidNumber]       # AD groups missing any of these are ignored, also when flattening
    attributes:
      cn: name
      gidNumber: gidNumber
    membership:
      - attribute: member             # full DN of the target user
      - attribute: memberUid          # bare uid; RFC 2307 servers: list only this one
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

## Passwords

Each password can be given inline (`bind_password`, `password`) or in a separate file (`bind_password_file`, `password_file`). Setting both to a non-empty value is a config error; an empty value counts as unset. If `dolly.yaml` contains an inline password, Dolly refuses to run unless the file is readable only by its owner (mode `0600` or stricter), the same way `ssh` treats private keys. `dolly.yaml` is git-ignored, so an inline password never ends up in the repository.

## Group schema: rfc2307bis or RFC 2307

The defaults assume rfc2307bis, where `posixGroup` is auxiliary and can be combined with `groupOfNames`, so members are written as both `member` DNs and `memberUid` values. Many older servers use RFC 2307 (`nis.schema`), where `posixGroup` is structural and can't be combined with `groupOfNames`. For those, configure `memberUid` only:

```yaml
mapping:
  groups:
    object_classes: [posixGroup]
    membership:
      - attribute: memberUid
```

To check which one a server uses, look up `posixGroup` in its schema (`ldapsearch -x -o ldif-wrap=no -b cn=Subschema -s base objectClasses | grep -i posixGroup`). `STRUCTURAL` means RFC 2307, and `AUXILIARY` means rfc2307bis. With `memberUid` only, Dolly doesn't need to read or write user entries on the target to manage groups, and `empty_group_member` isn't used.

## Config validation

Dolly validates `dolly.yaml` before connecting to anything:

- Unknown keys are a config error.
- `mapping.users.required` and `mapping.groups.required` must each include at least the spec's minimum attributes (`uid`, `uidNumber`, `gidNumber` for users; `name`, `gidNumber` for groups).
- `mapping.users.rdn` must map to the same value as `uid`.
- Every `create_only` entry must also be a mapped attribute.
- An empty `notify.smtp_host` disables notifications.
- `source.page_size` defaults to `500` if omitted.
- Listing `member` in `mapping.groups.membership` requires `target.empty_group_member` to be set.
- `target.users_base` and `target.groups_base` must differ, since Dolly tells users from groups by their container.

## `source`

Connection and search settings for Active Directory. Dolly only ever reads from this side.

| Key | Default / example | Meaning |
|---|---|---|
| `urls` | `[ldaps://dc01.example.edu:636, ldaps://dc02.example.edu:636]` | Domain controllers, tried in order. Use `ldaps://` URLs for encrypted binds. |
| `ca_file` | `ad-ca.pem` (optional) | CA certificate to trust for AD's TLS chain. See [TLS certificates](operations/tls.md). |
| `bind_dn` | `CN=svc-dolly,OU=Service Accounts,DC=example,DC=edu` | The AD service account to bind as. A UPN such as `svc-dolly@example.edu` also works. |
| `bind_password` | `""` | Inline bind password. Set only one of `bind_password` / `bind_password_file`. See [Passwords](#passwords). |
| `bind_password_file` | `ad.secret` | Path to a file holding the bind password. Resolved against the config file's directory if relative. |
| `users.base` | `OU=People,DC=example,DC=edu` | Search base for users. |
| `users.filter` | `(&(objectClass=user)(objectCategory=person))` | LDAP filter selecting in-scope users. See [Excluding entries](#excluding-entries-with-filters). |
| `groups.base` | `OU=Groups,DC=example,DC=edu` | Search base for groups. |
| `groups.filter` | `(objectClass=group)` | LDAP filter selecting in-scope groups. |
| `page_size` | `500` | Page size for paged AD searches. Large `member` attributes are also fetched with ranged retrieval regardless of this setting. |

## `target`

Connection settings and base DNs for the target LDAP server. Dolly reads and writes here.

| Key | Default / example | Meaning |
|---|---|---|
| `url` | `ldap://ldap.example.edu:389` | The target server. |
| `start_tls` | `true` | Upgrade the connection with StartTLS. |
| `ca_file` | `ldap-ca.pem` (optional) | CA certificate to trust for the target's TLS chain. |
| `bind_dn` | `cn=admin,dc=local` | The bind DN used to read and write the target. See [Permissions](operations/permissions.md) for the access it needs. |
| `bind_password` | `""` | Inline bind password. Set only one of `bind_password` / `bind_password_file`. See [Passwords](#passwords). |
| `bind_password_file` | `ldap.secret` | Path to a file holding the bind password. |
| `users_base` | `ou=people,dc=local` | Where user entries live. Dolly never creates this container. |
| `groups_base` | `ou=group,dc=local` | Where group entries live. Dolly never creates this container. |
| `state_base` | `ou=dolly,dc=local` | Where Dolly's ownership records, run lock, and status entry live. Dolly creates this container and its children if missing. See [Ownership](how-it-works/ownership.md). |
| `empty_group_member` | `cn=empty,dc=local` | Placeholder member DN used so a `groupOfNames` group never has zero members. Only used when `member` is in `mapping.groups.membership`; unused with `memberUid` only. See [Group schema](#group-schema-rfc2307bis-or-rfc-2307). |

## `mapping`

How AD attributes become target entries. Attribute values are plain AD attribute names or Go templates (`text/template`), evaluated against the AD entry's attributes. Dolly only writes the attributes listed here, so anything else already present on a target entry is left alone.

### `mapping.users`

| Key | Default / example | Meaning |
|---|---|---|
| `rdn` | `uid` | The attribute used as the RDN for user entries. |
| `object_classes` | `[account, posixAccount]` | Object classes written on new user entries. |
| `required` | `[uid, uidNumber, gidNumber]` | AD users missing any of these attributes are ignored everywhere, including for group flattening. |
| `create_only` | `[loginShell, homeDirectory]` | Written only when the user is created; never overwritten afterward, so local admin overrides survive. `disabled_shell` is the one exception (see [Ownership](how-it-works/ownership.md)). |
| `attributes.uid` | `uid` | Source is AD's `uid` attribute, not `sAMAccountName`. |
| `attributes.cn` | `uid` | |
| `attributes.uidNumber` | `uidNumber` | |
| `attributes.gidNumber` | `gidNumber` | |
| `attributes.homeDirectory` | `` `{{ or .unixHomeDirectory (printf "/home/%s" .uid) }}` `` | Falls back to `/home/<uid>` when `unixHomeDirectory` is unset. |
| `attributes.loginShell` | `` `{{ or .loginShell "/bin/bash" }}` `` | Falls back to `/bin/bash`. |
| `attributes.gecos` | `gecos` | Transliterated to ASCII on write; see [Ownership](how-it-works/ownership.md). |

### `mapping.groups`

| Key | Default / example | Meaning |
|---|---|---|
| `rdn` | `cn` | The attribute used as the RDN for group entries, sourced from AD's `name`. |
| `object_classes` | `[groupOfNames, posixGroup]` | rfc2307bis-style schema: `groupOfNames` for `member`, auxiliary `posixGroup` for `memberUid`. RFC 2307 servers use `[posixGroup]` alone (structural). See [Group schema](#group-schema-rfc2307bis-or-rfc-2307). |
| `required` | `[name, gidNumber]` | AD groups missing either are ignored everywhere, including when flattening nested groups. |
| `attributes.cn` | `name` | |
| `attributes.gidNumber` | `gidNumber` | |
| `membership` | `[{attribute: member}, {attribute: memberUid}]` | Both attributes are maintained: `member` as the full target user DN, `memberUid` as the bare `uid`. Values are sorted for stable comparison. RFC 2307 servers list only `memberUid`. |
| `flatten_nested` | `true` | Members of child (nested) groups are added recursively to the parent, with cycle protection. |

## `sync`

Run behavior: pruning, the mass-deletion guard, and timeouts.

| Key | Default | Meaning |
|---|---|---|
| `prune_users` | `false` | Delete Dolly-owned users that are gone from AD, once they've been gone for `prune_after_days`. Groups are never deleted. |
| `prune_after_days` | `30` | How long a user must be gone from AD before its entry is deleted (tracked as `missing-since` in its ownership record). |
| `disabled_shell` | `/sbin/nologin` | `loginShell` set on disabled AD accounts (`userAccountControl` bit `0x2`). Empty string leaves the shell alone. |
| `max_delete_percent` | `10` | Part of the mass-deletion guard: abort if a run would remove more than this percentage of what Dolly owns. `--force` overrides. |
| `max_delete_min` | `25` | The guard never trips for this many removals or fewer, regardless of percentage. |
| `lock_ttl` | `60m` | A run lock older than this (by the server's `createTimestamp`) is treated as stale and broken. |
| `run_timeout` | `45m` | A run aborts itself after this long. Must be shorter than `lock_ttl` so a live run's lock is never mistaken for stale. |
| `network_timeout` | `30s` | Connect and per-operation timeout against both AD and the target. |

See [How it works](how-it-works/index.md#guard) for the mass-deletion guard in detail, and [Run lock](how-it-works/run-lock.md) for locking.

## `notify`

SMTP settings and when to send mail. See [Notifications](operations/notifications.md) for the flood-control behavior.

| Key | Default / example | Meaning |
|---|---|---|
| `smtp_host` | `mx.example.edu` | SMTP relay host. |
| `smtp_port` | `25` | SMTP port. |
| `start_tls` | `true` | Upgrade the SMTP connection with StartTLS. |
| `username` | `""` | Optional SMTP auth username. |
| `password` | `""` | Inline SMTP auth password. Set only one of `password` / `password_file`. See [Passwords](#passwords). |
| `password_file` | `""` | Optional path to a file holding the SMTP auth password. |
| `from` | `"Dolly <dolly-noreply@example.edu>"` | Envelope and header `From` address. |
| `to` | `[ldap-admins@example.edu]` | Recipient list. |
| `subject_prefix` | `"[dolly]"` | Prefix on every subject line. |
| `on` | `failure` | When to send mail: `failure`, `changes`, or `always`. |
| `remind_every` | `24h` | While a failure persists, send at most one reminder per this interval. |

## Templates

Any attribute value under `mapping` can be a Go `text/template` string instead of a plain AD attribute name. Templates are evaluated against the AD entry's attributes, so `.uid`, `.unixHomeDirectory`, and so on refer to the raw AD values. The default mapping uses templates for `homeDirectory` and `loginShell` to supply fallbacks when AD doesn't set `unixHomeDirectory` or `loginShell`.

## Excluding entries with filters

There's no separate exclude list. To keep users or groups out of scope entirely, extend `source.users.filter` or `source.groups.filter`. For example, to exclude a group of accounts that shouldn't be replicated:

```text
(!(memberOf=CN=ExcludedFromLDAPSync,OU=Groups,DC=example,DC=edu))
```

Combine it with the existing filter using `(&...)`.

## File locations (XDG)

| What | Default path |
|---|---|
| Binary | `~/.local/bin/dolly` |
| Config | `$XDG_CONFIG_HOME/dolly/dolly.yaml` (`~/.config/dolly/`), created by `dolly install` with mode `0600` |
| Secrets and CA files | next to the config, mode `0600` |
| systemd units | `$XDG_CONFIG_HOME/systemd/user/dolly.{service,timer}` |
| Logs | journald (`journalctl --user -u dolly`) |

Dolly honors `XDG_CONFIG_HOME` and falls back to `~/.config` when it's unset.
