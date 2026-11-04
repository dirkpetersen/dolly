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
  urls:                               # tried in random order each run
    - ldaps://dc01.example.edu:636
    - ldaps://dc02.example.edu:636
  ca_file: ad-ca.pem                  # optional, see "TLS certificates"
  # tls_fingerprint: ["sha256:AB:CD:..."]   # instead of ca_file: pin each DC's certificate (dolly fetch-cert URL)
  bind_dn: CN=svc-dolly,OU=Service Accounts,DC=example,DC=edu   # or a UPN (svc-dolly@example.edu), or a bare name (svc-dolly: the domain comes from the base's DC= parts; EXAMPLE\svc-dolly stays as it is)
  bind_password: ""                   # inline password, or use bind_password_file (set only one)
  bind_password_file: ad.secret       # relative paths resolve against the config file's directory
  users:
    base: OU=People,DC=example,DC=edu
    filter: (&(objectClass=user)(objectCategory=person))
  groups:
    base: OU=Groups,DC=example,DC=edu
    filter: (objectClass=group)
  # upg:                              # optional second group set: user private groups, see "User private groups"
  #   from: ad                        # ad: AD groups under base that match filter; users: one group per synced user
  #   base: OU=UPG,DC=example,DC=edu  # from: ad only
  #   filter: (&(objectClass=group)(cn=u-*))   # from: ad only
  #   gid_number: gidNumber           # from: users only: the mapped user attribute that gives the group's gidNumber
  page_size: 500

target:
  url: ldap://ldap.example.edu:389
  start_tls: true
  ca_file: ldap-ca.pem                # optional, see "TLS certificates"
  # tls_fingerprint: ["sha256:AB:CD:..."]   # instead of ca_file: pin the server's certificate (dolly fetch-cert URL)
  bind_dn: cn=admin,dc=local
  bind_password: ""                   # inline password, or use bind_password_file (set only one)
  bind_password_file: ldap.secret
  users_base: ou=people,dc=local      # users_base and groups_base are created (ou= only) if missing
  groups_base: ou=group,dc=local
  state_base: ou=dolly,dc=local       # Dolly's ownership records and run lock, see "Ownership records"
  # upg_base: ou=upg,dc=local         # with source.upg: where the user private groups go (created if missing, ou= only)
  empty_group_member: cn=empty,dc=local   # placeholder for groupOfNames groups that would otherwise be empty; unused with memberUid only

# mapping and sync: leave a section (or mapping.users / mapping.groups) out to
# use the values below. In a present sync section only lock_ttl, run_timeout, and
# network_timeout default; other missing sync keys are zero (as before).
mapping:
  users:
    rdn: uid
    object_classes: [account, posixAccount]
    required: [sAMAccountName, uidNumber, gidNumber]   # needed for a user entry; a group member needs only uid's source
    create_only: [loginShell, homeDirectory]  # set on creation, then never overwritten (local overrides survive)
    attributes:                       # AD attribute names; for AD's uid attribute set uid, cn, required, and homeDirectory to uid
      uid: sAMAccountName
      cn: sAMAccountName
      uidNumber: uidNumber
      gidNumber: gidNumber
      homeDirectory: '{{ or .unixHomeDirectory (printf "/home/%s" .sAMAccountName) }}'
      loginShell: '{{ or .loginShell "/bin/bash" }}'
      gecos: gecos
    # gid_from_upg:                   # take gidNumber from the user's private group in AD, see "gidNumber from the user private group"
    #   group_name: 'u-{{ .uid }}'    # Go template; matched case-insensitively against AD group names
    #   when: missing                 # missing: only if the AD user has no valid gidNumber; always: the group wins whenever it exists
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
  network_timeout: 30s                # connection setup (dial, TLS, bind) and per-operation timeout

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

## Defaults for `mapping` and `sync`

A config may leave out the whole `sync` section or the whole `mapping` section. Dolly then uses the values in the full example above (they come from the built-in `dolly.yaml.template`).

- In a `sync` section that is present, only `lock_ttl`, `run_timeout`, and `network_timeout` get their defaults when missing (they used to be required, so this changes no existing config). Every other missing key keeps its zero value, as before: `prune_users: false`, `prune_after_days: 0`, `disabled_shell: ""` (the shell is left alone), `max_delete_percent: 0`, `max_delete_min: 0`.
- For `mapping`, only a whole missing subsection gets its default. Without `mapping.users`, the default user mapping applies. Without `mapping.groups`, the default group mapping applies.
- Keys inside a `mapping.users` or `mapping.groups` that is present never get defaults. A mapping is always exactly what is written.
- The default group mapping lists `member`, so it needs `target.empty_group_member`.

## AD bind name

`source.bind_dn` can be one of three forms:

- a DN: `CN=svc-dolly,OU=Service Accounts,DC=example,DC=edu`
- a UPN: `svc-dolly@example.edu`
- a bare account name: `svc-dolly` (no `=`, `@`, or `\`)

A down-level name such as `EXAMPLE\svc-dolly` is passed to AD unchanged.

Dolly turns a bare name into the UPN `<name>@<domain>`. The domain comes from the `DC=` parts of `source.users.base`, or of `source.groups.base` if the users base has none. For example, with `DC=ad,DC=example,DC=edu`, the name `svc-dolly` binds as `svc-dolly@ad.example.edu`. `dolly check` and the progress lines of a run show the UPN. If neither base has `DC=` parts, a bare name is a config error.

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

To check which one a server uses, look up `posixGroup` in its schema (`ldapsearch -x -o ldif-wrap=no -b cn=Subschema -s base objectClasses | grep -i posixGroup`). `STRUCTURAL` means RFC 2307, and `AUXILIARY` means rfc2307bis. `dolly config` and `dolly check` read it for you (from the root DSE's `subschemaSubentry`): `dolly config` sets the default from it and warns if you choose the other schema, and `dolly check` warns if `mapping.groups.object_classes` doesn't match. With `memberUid` only, Dolly doesn't need to write user entries on the target to manage groups, and `empty_group_member` isn't used.

## Groups-only deployments

Dolly can manage groups without ever creating users. Run `dolly sync --groups` (for example in the systemd unit): the user entries then come from somewhere else, and Dolly only adds and removes group members.

- `target.users_base` can be read-only for Dolly, and larger than the server's search size limit. A `--groups` run never reads it in full; it looks up only the uids of the members it would add, 50 per search.
- AD users need only the attribute `uid` is mapped from to be group members. By default `uid` comes from `sAMAccountName`. If your Unix identity is AD's `uid` attribute, map `uid: uid` and `cn: uid`, put `uid` in `mapping.users.required` instead of `sAMAccountName`, and use `.uid` in the `homeDirectory` template. `uidNumber` and `gidNumber` stay in the list; they only matter for user entries, which a groups-only run never creates.
- A member is added only if an entry with its uid already exists under `users_base` (this is always on; there's no setting for it). Neither the AD user nor the target entry needs `uidNumber` for this check; only groups need `gidNumber`. Members without a target entry are skipped, not warned about: the run summary shows one count line, and `--debug` lists each skipped (group, user) pair. A skipped member is picked up automatically once its entry exists.
- The AD users base isn't searched either: every member is fetched from AD by DN, and must still match `source.users.filter`.

## Config validation

Dolly validates `dolly.yaml` before connecting to anything:

- Unknown keys are a config error. The removed key `sync.require_member_on_target` gets an explanation: its rule (group members must exist on the target) is now always on, so delete the line.
- `mapping.users.required` must include the AD attributes that `uid`, `uidNumber`, and `gidNumber` are mapped from, when they're mapped from a plain attribute. With the defaults that's `sAMAccountName`, `uidNumber`, `gidNumber`; with `uid: uid` it's `uid` instead of `sAMAccountName`. `uidNumber` and `gidNumber` must be mapped.
- `mapping.groups.required` must include `name` and `gidNumber`.
- `mapping.users.rdn` must map to the same value as `uid`.
- Every `create_only` entry must also be a mapped attribute.
- An empty `notify.smtp_host` disables notifications.
- SMTP without authentication is allowed: with `notify.username`, `notify.password`, and `notify.password_file` all empty, Dolly never sends `AUTH` (plain relay, with or without `notify.start_tls`). A username without a password, or a password without a username, is a config error. A username also requires TLS, since `AUTH` is never sent over an unencrypted connection: `notify.start_tls: true`, or `notify.smtp_port: 465` (implicit TLS, where `notify.start_tls` must be `false`). `notify.from` and each `notify.to` entry must be valid mail addresses.
- `source.page_size` defaults to `500` if omitted.
- `mapping.users.gid_from_upg.group_name` must be a Go template, and `when` must be `missing` or `always` (default `missing`). With `gid_from_upg`, `mapping.users.required` may leave out the source of `gidNumber`. See [gidNumber from the user private group](operations/gid-from-upg.md).
- `source.bind_dn` as a bare account name needs `DC=` parts in `source.users.base` or `source.groups.base`.
- Each `tls_fingerprint` entry must be a SHA-256 fingerprint: 64 hex digits, with or without a `sha256:` prefix and colons, in any case. `ca_file` and `tls_fingerprint` can't both be set on the same side. On the target, `ca_file` or `tls_fingerprint` with a plaintext `ldap://` URL and `start_tls: false` is a config error, because no TLS would be used and the setting would be silently ignored. (The source always uses TLS.) See [TLS certificates](operations/tls.md).
- Listing `member` in `mapping.groups.membership` requires `target.empty_group_member` to be set.
- `target.users_base` and `target.groups_base` must differ, since Dolly tells users from groups by their container.
- `state_base` may live inside `groups_base` or `users_base` (for example `ou=dolly,ou=group,dc=local`), and Dolly ignores that subtree when it reads users and groups. It must not equal either base, and neither base may live inside `state_base`.
- `source.upg` and `target.upg_base` are set together or not at all. `upg_base` must differ from the other three bases, must not lie inside any of them, and must not contain `users_base` or `groups_base`. With `from: ad`, `base` and `filter` are required and `gid_number` is not allowed. With `from: users`, `base` and `filter` are not allowed, and `gid_number` must be a key of `mapping.users.attributes`. See [User private groups](operations/user-private-groups.md).

## `source`

Connection and search settings for Active Directory. Dolly only ever reads from this side.

| Key | Default / example | Meaning |
|---|---|---|
| `urls` | `[ldaps://dc01.example.edu:636, ldaps://dc02.example.edu:636]` | Domain controllers, tried in random order each run to spread the load: a connect or bind failure moves on to the next one (invalid credentials don't, to spare the account's lockout counter). An `ldap://` URL is always upgraded with StartTLS. |
| `ca_file` | `ad-ca.pem` (optional) | CA certificate to trust for AD's TLS chain. See [TLS certificates](operations/tls.md). Not with `tls_fingerprint`. |
| `tls_fingerprint` | `["sha256:AB:CD:..."]` (optional) | Pin the leaf certificate of each DC by SHA-256 fingerprint. One entry per DC. CA, expiry, and host name are then not checked. Not with `ca_file`. Get the value with `dolly fetch-cert`. See [TLS certificates](operations/tls.md). |
| `bind_dn` | `CN=svc-dolly,OU=Service Accounts,DC=example,DC=edu` | The AD service account to bind as. A DN, a UPN such as `svc-dolly@example.edu`, or a bare account name such as `svc-dolly`. See [AD bind name](#ad-bind-name). |
| `bind_password` | `""` | Inline bind password. Set only one of `bind_password` / `bind_password_file`. See [Passwords](#passwords). |
| `bind_password_file` | `ad.secret` | Path to a file holding the bind password. Resolved against the config file's directory if relative. |
| `users.base` | `OU=People,DC=example,DC=edu` | Search base for users. |
| `users.filter` | `(&(objectClass=user)(objectCategory=person))` | LDAP filter selecting in-scope users. It also applies to users fetched by DN as group members. See [Excluding entries](#excluding-entries-with-filters). |
| `groups.base` | `OU=Groups,DC=example,DC=edu` | Search base for groups. |
| `groups.filter` | `(objectClass=group)` | LDAP filter selecting in-scope groups. It also applies to nested groups fetched by DN. |
| `upg.from` | `ad` or `users` (optional) | Turns on the user private group set. See [User private groups](operations/user-private-groups.md). |
| `upg.base`, `upg.filter` | `OU=UPG,DC=example,DC=edu`, `(objectClass=group)` | With `from: ad`: the search for the AD user private groups. |
| `upg.gid_number` | `gidNumber` | With `from: users`: the mapped user attribute that gives each group's `gidNumber` (for example `uidNumber`). |
| `page_size` | `500` | Page size for paged AD searches. Large `member` attributes are also fetched with ranged retrieval regardless of this setting. |

## `target`

Connection settings and base DNs for the target LDAP server. Dolly reads and writes here.

| Key | Default / example | Meaning |
|---|---|---|
| `url` | `ldap://ldap.example.edu:389` | The target server. |
| `start_tls` | `true` | Upgrade the connection with StartTLS. |
| `ca_file` | `ldap-ca.pem` (optional) | CA certificate to trust for the target's TLS chain. Not with `tls_fingerprint`. |
| `tls_fingerprint` | `["sha256:AB:CD:..."]` (optional) | Pin the target server's leaf certificate by SHA-256 fingerprint. CA, expiry, and host name are then not checked. Not with `ca_file`. See [TLS certificates](operations/tls.md). |
| `bind_dn` | `cn=admin,dc=local` | The bind DN used to read and write the target. See [Permissions](operations/permissions.md) for the access it needs. |
| `bind_password` | `""` | Inline bind password. Set only one of `bind_password` / `bind_password_file`. See [Passwords](#passwords). |
| `bind_password_file` | `ldap.secret` | Path to a file holding the bind password. |
| `users_base` | `ou=people,dc=local` | Where user entries live. A real run creates it if it is missing, but only with an `ou=` RDN, and its parent must exist. See [Ownership](how-it-works/ownership.md). |
| `groups_base` | `ou=group,dc=local` | Where group entries live. A real run creates it if it is missing, with the same rules as `users_base`. |
| `state_base` | `ou=dolly,dc=local` | Where Dolly's ownership records, run lock, and status entry live. Dolly creates this container and its children if missing. See [Ownership](how-it-works/ownership.md). |
| `upg_base` | `ou=upg,dc=local` (optional) | With `source.upg`: where the user private groups live. Created if missing, with the same rules as `users_base`. |
| `empty_group_member` | `cn=empty,dc=local` | Placeholder member DN used so a `groupOfNames` group never has zero members. Only used when `member` is in `mapping.groups.membership`; unused with `memberUid` only. See [Group schema](#group-schema-rfc2307bis-or-rfc-2307). |

## `mapping`

How AD attributes become target entries. Attribute values are plain AD attribute names or Go templates (`text/template`), evaluated against the AD entry's attributes. Dolly only writes the attributes listed here, so anything else already present on a target entry is left alone.

### `mapping.users`

| Key | Default / example | Meaning |
|---|---|---|
| `rdn` | `uid` | The attribute used as the RDN for user entries. |
| `object_classes` | `[account, posixAccount]` | Object classes written on new user entries. |
| `required` | `[sAMAccountName, uidNumber, gidNumber]` | Needed for a user entry. A user missing one gets no entry, but can still be a group member as long as it has the attribute `uid` is mapped from. |
| `create_only` | `[loginShell, homeDirectory]` | Written only when the user is created; never overwritten afterward, so local admin overrides survive. `disabled_shell` is the one exception (see [Ownership](how-it-works/ownership.md)). |
| `attributes.uid` | `sAMAccountName` | Source is `sAMAccountName` by default (since v0.0.5). Map AD's `uid` attribute instead (as ad2openldap did) if that's your Unix identity, and change `cn`, `required`, and `homeDirectory` to match. |
| `attributes.cn` | `sAMAccountName` | |
| `attributes.uidNumber` | `uidNumber` | |
| `attributes.gidNumber` | `gidNumber` | |
| `attributes.homeDirectory` | `` `{{ or .unixHomeDirectory (printf "/home/%s" .sAMAccountName) }}` `` | Falls back to `/home/<uid>` when `unixHomeDirectory` is unset. |
| `attributes.loginShell` | `` `{{ or .loginShell "/bin/bash" }}` `` | Falls back to `/bin/bash`. |
| `attributes.gecos` | `gecos` | Transliterated to ASCII on write; see [Ownership](how-it-works/ownership.md). |
| `gid_from_upg.group_name` | `u-{{ .uid }}` (optional) | Go template for the name of the user's private group in AD. Dolly takes `gidNumber` from that group. See [gidNumber from the user private group](operations/gid-from-upg.md). |
| `gid_from_upg.when` | `missing` | `missing`: use the group only if the AD user has no valid `gidNumber`. `always`: the group wins whenever it exists. |

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
| `lock_ttl` | `60m` | A run lock whose `createTimestamp` and holder `started=` time are both older than this is treated as stale and broken. See [Run lock](how-it-works/run-lock.md). |
| `run_timeout` | `45m` | A run aborts itself after this long. Must be shorter than `lock_ttl` so a live run's lock is never mistaken for stale. |
| `network_timeout` | `30s` | Timeout for each connection setup (TCP connect, StartTLS or the LDAPS handshake, and the bind, together) and for each operation, against both AD and the target. |

See [How it works](how-it-works/index.md#guard) for the mass-deletion guard in detail, and [Run lock](how-it-works/run-lock.md) for locking.

## `notify`

SMTP settings and when to send mail. See [Notifications](operations/notifications.md) for the flood-control behavior, and [Authentication](operations/notifications.md#authentication) for the `username`/`password`/TLS rules.

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

There's no separate exclude list. To keep users or groups out of scope entirely, extend `source.users.filter` or `source.groups.filter`. The filters also apply to group members Dolly fetches by DN, so a member that doesn't match is skipped (and counted as filtered in the summary), wherever it lives in AD. For example, to exclude a group of accounts that shouldn't be replicated:

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
