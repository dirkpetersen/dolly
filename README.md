# 🐑 Dolly

[![CI](https://github.com/dirkpetersen/dolly/actions/workflows/ci.yml/badge.svg)](https://github.com/dirkpetersen/dolly/actions/workflows/ci.yml)
[![Docs](https://github.com/dirkpetersen/dolly/actions/workflows/docs.yml/badge.svg)](https://dirkpetersen.github.io/dolly/)
[![Release](https://img.shields.io/github/v/release/dirkpetersen/dolly?sort=semver)](https://github.com/dirkpetersen/dolly/releases/latest)
[![Go version](https://img.shields.io/github/go-mod/go-version/dirkpetersen/dolly)](go.mod)
[![Platforms](https://img.shields.io/badge/platforms-linux%20%7C%20macOS-lightgrey)](https://github.com/dirkpetersen/dolly/releases/latest)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

> Hello, Dolly. Clones your Active Directory users and groups into OpenLDAP (or any LDAP server), one sheep at a time.

**📖 Documentation: [dirkpetersen.github.io/dolly](https://dirkpetersen.github.io/dolly/)** · [Getting started](https://dirkpetersen.github.io/dolly/getting-started/) · [Configuration](https://dirkpetersen.github.io/dolly/configuration/) · [Releases](https://github.com/dirkpetersen/dolly/releases) · [Contributing](CONTRIBUTING.md)

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
- **Full comparison every run.** Dolly reads all in-scope groups (and, when it syncs users, all in-scope users) each time, so deletions and nested-group changes are never missed. About 10,000 groups is a few seconds of reading.
- **Groups-only deployments.** `dolly sync --groups` works with a read-only, arbitrarily large `users_base` and with AD users that have no Unix numbers: it reads neither users base in full, only the members it needs
- **Nested groups are flattened** into direct user members, with loop protection
- **Rename handling** by tracking AD's `objectGUID`, so a renamed user or group is renamed in place rather than deleted and re-created
- **Mass-deletion guard** that refuses runs removing more than a set share of managed memberships or users, and any run based on an incomplete read
- **Run lock stored in LDAP**, so two hosts never sync at once. Stale locks expire, and `dolly unlock` clears one by hand
- **Email notifications** without the flood: one mail on the first failure, a reminder at most every `remind_every`, and one on recovery
- **Local overrides survive.** Attributes listed as `create_only` are set when a user is created and then left to the LDAP admins
- **Several domain controllers** can be listed; each run tries them in random order, which spreads the load, and falls over to the next one on a connection error
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
dolly install            # groups-only deployment: dolly install --groups
$EDITOR ~/.config/dolly/dolly.yaml
dolly check              # groups-only: dolly check --groups

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
  urls:                               # tried in random order each run
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
    required: [uid, uidNumber, gidNumber]   # needed for a user entry; a group member needs only uid's source
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

**Passwords.** Each password can be given inline (`bind_password`, `password`) or in a separate file (`bind_password_file`, `password_file`). Setting both to a non-empty value is a config error; an empty value counts as unset. If `dolly.yaml` contains an inline password, Dolly refuses to run unless the file is readable only by its owner (mode `0600` or stricter), the same way `ssh` treats private keys. `dolly.yaml` is git-ignored, so an inline password never ends up in the repository.

**Group schema: rfc2307bis or RFC 2307.** The defaults assume rfc2307bis, where `posixGroup` is auxiliary and can be combined with `groupOfNames`, so members are written as both `member` DNs and `memberUid` values. Many older servers use RFC 2307 (`nis.schema`), where `posixGroup` is structural and can't be combined with `groupOfNames`. For those, configure `memberUid` only:

```yaml
mapping:
  groups:
    object_classes: [posixGroup]
    membership:
      - attribute: memberUid
```

To check which one a server uses, look up `posixGroup` in its schema (`ldapsearch -x -o ldif-wrap=no -b cn=Subschema -s base objectClasses | grep -i posixGroup`). `STRUCTURAL` means RFC 2307, and `AUXILIARY` means rfc2307bis. With `memberUid` only, Dolly doesn't need to read or write user entries on the target to manage groups, and `empty_group_member` isn't used.

Attribute values are plain AD attribute names or Go templates. Dolly only writes the attributes listed in the mapping, so other attributes on an entry are left alone. To exclude users or groups, use the search `filter`, for example `(!(memberOf=CN=ExcludedFromLDAPSync,OU=Groups,DC=example,DC=edu))`.

**Groups only.** Dolly can manage groups without ever creating users: run `dolly sync --groups` (for example in the systemd unit). The user entries then come from elsewhere, and `users_base` can be read-only for Dolly and larger than the server's search size limit, because a groups-only run never reads it in full (see "How it works"). In a groups-only deployment, the users that are supposed to be added to a target group must already exist on the target: an AD user is added to a group only if an entry with its uid exists under `users_base` (with `member` in the membership list: an entry at the member DN). If it doesn't exist, Dolly skips that member. Skipped members are not warnings: the run summary shows one count line (`members skipped: N not on the target`), and `--debug` lists each one. A skipped member is checked again on every run and added as soon as its entry appears. Neither side needs `uidNumber`: AD users need only the attribute `uid` is mapped from (for example `uid: sAMAccountName`), and the target entry needs only a `uid`. Only the groups need `gidNumber` (and `name`). `uidNumber` and `gidNumber` on AD users are needed only for user entries Dolly creates.

**Config validation.** Dolly validates `dolly.yaml` before connecting to anything:

- Unknown keys are a config error. The removed key `sync.require_member_on_target` gets an explanation: its rule (group members must exist on the target) is now always on, so delete the line.
- `mapping.users.required` must include the AD attributes that `uid`, `uidNumber`, and `gidNumber` are mapped from, when they're mapped from a plain attribute (with the defaults: `uid`, `uidNumber`, `gidNumber`; with `uid: sAMAccountName`, `sAMAccountName` instead of `uid`). `uidNumber` and `gidNumber` must be mapped. `mapping.groups.required` must include `name` and `gidNumber`.
- `mapping.users.rdn` must map to the same value as `uid`.
- Every `create_only` entry must also be a mapped attribute.
- An empty `notify.smtp_host` disables notifications.
- SMTP without authentication is allowed: with `notify.username`, `password`, and `password_file` all empty, Dolly never sends `AUTH` (plain relay, with or without `start_tls`). A username without a password, or a password without a username, is a config error. A username also requires TLS, since `AUTH` is never sent over an unencrypted connection: `start_tls: true`, or `smtp_port: 465` (implicit TLS, where `start_tls` must be `false`). `notify.from` and each `notify.to` entry must be valid mail addresses.
- `page_size` defaults to `500` if omitted.
- Listing `member` in `mapping.groups.membership` requires `empty_group_member` to be set.
- `users_base` and `groups_base` must differ, since Dolly tells users from groups by their container.
- `state_base` may live inside `groups_base` or `users_base` (for example `ou=dolly,ou=group,dc=local`), and Dolly ignores that subtree when it reads users and groups. It must not equal either base, and neither base may live inside `state_base`.

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
| `dolly sync --users` | Syncs users only. AD groups are still read, since Dolly needs them to resolve out-of-scope users; the only group values that change are `member`/`memberUid` fix-ups for a renamed or pruned user. Removing owned memberships happens only in a run that includes groups |
| `dolly sync --groups` | Syncs groups only (the default without either flag is both). Neither the AD users base nor the target's `users_base` is read in full: members are fetched from AD by DN, and only their uids are looked up on the target. A member's target DN comes from the user's ownership record (`seeAlso`), not a fresh AD lookup, so a pending user rename causes no churn. A member is added only if its entry already exists under `users_base` (see "Ownership records"); otherwise it is skipped, counted in the summary, and listed with `--debug`. Neither AD users nor target users need `uidNumber`; only groups need `gidNumber`. |
| `dolly sync --dry-run` | Prints planned adds, modifies, renames, and removals without writing |
| `dolly sync --force` | Applies the run even if it trips the mass-deletion guard |
| `--debug` | Accepted by every command that plans (`sync`, `adopt`). Prints debug details to stderr, one line per group member skipped because it has no entry on the target: `debug: skip <uid> in <group DN>: no entry on the target under <users_base>` (with `member` in the membership list: `no entry on the target at <member DN>`). Without it, the summary shows only the count |
| `dolly adopt` | One-time takeover of an existing tree, such as one written by ad2openldap. See "Adopting an existing tree" |
| `dolly unlock` | Shows the current run lock (holder, and age by the server's `createTimestamp`) and removes it after asking for confirmation. Without `--yes`, stdin must be a terminal: Dolly refuses (exit `1`) rather than guess when it isn't, for example under cron. `--yes` skips the prompt. Use it after a crash, when no run is active |
| `dolly check` | Tests the config, every domain controller, the target's containers and size limit, and SMTP, one `✓`/`✗`/`!` line each; exits `1` if any check fails. Writes nothing. `--groups` for a groups-only deployment; `--send-test-mail` also sends one test message to `notify.to`. See "Checking a config" |
| `dolly install` | Copies the binary to `~/.local/bin`, creates the config from the built-in template if missing, and writes the `systemd --user` units. `--users` or `--groups` and `--config FILE` are baked into the unit's `ExecStart`. See "Running it on a schedule" |
| `dolly uninstall` | Stops and disables the timer, removes the units and the binary, and keeps the config |
| `dolly version` | Prints the version, commit, build date, and the Go version it was built with |

### Checking a config

`dolly check` runs every check it can, even after one fails, and never writes to AD or the target:

- **Config:** loads and validates it (including the password rules), and reads every password file: it must exist and not be empty, and a file readable by group or others is a warning.
- **AD:** connects to *each* DC in `source.urls` (LDAPS, or StartTLS for `ldap://`) and binds. A DC that fails while another answers is a warning, since runs fail over; none answering is a failure. After an invalid-credentials error the remaining DCs aren't tried, because every attempt counts toward the account's lockout. On the first DC that answered, it runs a paged search (page size 1, first page only) of the users base and the groups base with their filters, and names that DC. A base with no matching entry is a failure (the guard stops every sync when AD returns no users or no groups), except the users base with `--groups`, which groups-only runs never search.
- **Target:** TLS and bind (plain `ldap://` without StartTLS is a warning). `groups_base` must exist, and a full unpaged read of it (DNs only, no client size limit) must not hit the server's size limit, since every run reads it. `users_base` must exist and answer a `(uid=*)` lookup; the same full read tests its size limit. A truncated `users_base` read is a failure, or with `--groups` a warning, because groups-only runs never read `users_base` in full. Both print the `olcLimits` fix (see "Permissions"). `state_base` must exist, or have an `ou=`/`cn=` RDN and an existing parent for the first real run to create it under (write access there isn't tested, since `check` never writes). A run lock held longer than `lock_ttl` (by `createTimestamp`), or one with a timestamp in the future, is a warning suggesting `dolly unlock` if no run is active.
- **SMTP** (if `notify.smtp_host` is set): connects, says `EHLO`, does StartTLS if configured, and authenticates with `AUTH PLAIN` if a username is set, or reports `SMTP: no authentication (username empty)`. No mail is sent unless `--send-test-mail` is given. Connects and operations are bounded by `network_timeout`, the whole check by `run_timeout`.

```text
Config /home/svc-dolly/.config/dolly/dolly.yaml
  ✓ loaded and valid
  ✓ source.bind_password_file /home/svc-dolly/.config/dolly/ad.secret: readable
  ✓ target.bind_password_file /home/svc-dolly/.config/dolly/ldap.secret: readable

AD (source.urls; sync tries them in random order)
  ! ldaps://dc01.example.edu:636: dial tcp: connection refused (runs fail over to ldaps://dc02.example.edu:636)
  ✓ ldaps://dc02.example.edu:636: LDAPS and bind as CN=svc-dolly,OU=Service Accounts,DC=example,DC=edu (41ms)
  ✓ users base OU=People,DC=example,DC=edu: paged search with (&(objectClass=user)(objectCategory=person)) returns entries (answered by ldaps://dc02.example.edu:636)
  ✓ groups base OU=Groups,DC=example,DC=edu: paged search with (objectClass=group) returns entries (answered by ldaps://dc02.example.edu:636)

Target ldap://ldap.example.edu:389
  ✓ StartTLS and bind as cn=admin,dc=local (12ms)
  ✓ groups_base ou=group,dc=local exists; all 9412 entries readable in one search
  ✓ users_base ou=people,dc=local: readable (uid lookup found uid=jdoe,ou=people,dc=local)
  ✗ users_base ou=people,dc=local: the server truncated a full read after 10000 entries (sizeLimitExceeded); runs that sync users read it in full and would abort (for a groups-only deployment, run dolly check --groups)
      bind as the rootdn, or raise the limit for Dolly's DN on the database entry: olcLimits: dn.exact="cn=admin,dc=local" size=unlimited time=unlimited (README "Permissions")
  ✓ state_base ou=dolly,dc=local exists

SMTP mx.example.edu:25
  ✓ connected to mx.example.edu:25
  ✓ EHLO ldap-sync01
  ✓ STARTTLS (certificate verified for mx.example.edu)
  ✓ SMTP: no authentication (username empty)
  - no mail sent (use --send-test-mail to send one to ldap-admins@example.edu)

1 check failed, 14 passed, 1 warning.
```

Exit codes: `0` for success, or when another host holds the lock. `1` for an error, including a single rejected operation (see "Applying the plan") and a run stopped by a signal or `run_timeout`. `2` when the mass-deletion guard stopped (or, for `--dry-run`, would have stopped) the run — `--force` makes that case exit `0` instead. The "AD returned zero users or zero groups" guard applies to every `dolly sync`, dry-run or not, but not to `dolly adopt`.

## How it works

1. **Lock.** Dolly takes the run lock in the target LDAP, creating `state_base` first if it's missing. If another host holds it, Dolly exits quietly with code 0. `--dry-run` takes no lock. See "Run lock".
2. **Bleat.** Dolly binds to AD (trying `source.urls` in random order) and runs paged searches for all in-scope groups on every run, and for all in-scope users in every run that syncs users — a `--users` run still reads AD groups, because Dolly needs them to know which out-of-scope users are still referenced by a group. A `--groups` run doesn't search the users base at all: it fetches each group member by DN (base-scope lookups, several at a time), following nested groups the same way, so its cost follows the size of the synced groups, not of the users base. Large `member` attributes are fetched with ranged retrieval (`member;range=…`). Group members that the searches didn't return — outside the configured bases, or every member in a `--groups` run — are fetched by DN, and must match `source.users.filter` (users) or `source.groups.filter` (groups), exactly like the searches; a member that matches neither is skipped as filtered, as if it were out of scope. On the target, a run that syncs users reads all of `users_base`; a `--groups` run reads only `groups_base` and `state_base`, and looks up the uids of the members it would add in batches (`(|(uid=a)(uid=b)…)`, 50 per search). If any read from AD or the target fails or comes back truncated (for example `sizeLimitExceeded`), the run aborts. Dolly never plans from partial data.
3. **Shear.** Each entry is mapped through the configured attribute rules. Entries missing a `required` attribute are ignored (a user without `uidNumber` or `gidNumber` only for its own entry; it can still be a group member), and the run summary lists them once rather than warning every run. Nested groups are flattened. Members are resolved by DN to the user's `objectGUID` and then to the target DN and uid, never by CN, because CNs aren't unique and can contain escaped commas.
4. **Compare.** Dolly reads the target entries and its ownership records under `state_base`, then plans adds, attribute modifies, renames (`modrdn`), and member additions and removals.
5. **Guard.** Dolly counts removals across the whole run, separately for memberships and for users. If either count is above `max_delete_min` and above `max_delete_percent` of what Dolly owns, or if AD returned no users or no groups at all, Dolly aborts, sends a notification, and changes nothing. Local memberships removed by a prune are reported in the summary but don't count toward the guard. This applies to every `dolly sync`, including `--dry-run` (which reports what the guard would trip on, exits `2`, and writes nothing regardless). `--force` overrides the guard and makes a would-be trip exit `0`. `dolly adopt` has no guard.
6. **Clone.** Dolly applies the plan in a crash-safe order: it writes the ownership record *before* adding an entry or member, and removes the entry or member *before* removing its record. A crash can then never leave a Dolly-added member looking like a local one. The operations run exactly in plan order (the step numbers `--dry-run` prints); Dolly never reorders them. Finally it updates the status entry and releases the lock. See "Applying the plan".

### Applying the plan

A rejected operation doesn't stop the run. Dolly logs it to stderr with its step number and DN, carries on, lists it in the result, and exits `1`. What it skips are the later operations that *depend* on the failed one, because running them would break the crash-safe order:

- A failed ownership record skips the entry add it protects, and a user whose creation failed (or was skipped) this run isn't added to any group. A new group whose initial members include such a user waits for the next run.
- Each user's and each group's record update, entry add, rename (`modrdn`), reference fix-ups, and final "rename complete" note form one sequence. The first failure skips the rest of that sequence, exactly as if the run had crashed there: after a failed `modrdn` the `renaming-from` note stays, so the next run finishes the rename. A failed attribute change or a rejected member doesn't stop the group's other members.
- Each membership is its own sequence: a failed `roleOccupant` add skips the member values; a failed member value delete skips the `roleOccupant` delete, so the member stays Dolly-owned and is removed on the next run instead of turning into a local member. A group gone from AD keeps its record until all its owned members are removed; a pruned user keeps its entry and record until every membership is removed.
- If the `empty_group_member` placeholder can't be added, the member deletes on that group are skipped; if a member add fails, the placeholder stays.

Skipped operations are listed with the step that caused them, and are retried by the next run. On a re-run after a crash, an add whose entry already exists with the planned content, a value add that finds the value present, and a value delete that finds it gone count as "already in place". So does a rename fix-up (a `member`, `memberUid`, or `roleOccupant` value changed from the old user DN or uid to the new one) that the server already carried out itself, as OpenLDAP's `refint` overlay does for DN values right after a `modrdn`: when such a fix-up is rejected, Dolly re-reads the attribute and counts it as done if the old value is gone and the new one present, deletes only the old value if both are present, and leaves the value alone if both are gone (it may have been a local member removed in the meantime; an owned one is re-added by the next run). Any other server error is a failure. Each operation is bounded by `network_timeout`. On `SIGTERM`, `SIGINT`, or `run_timeout`, Dolly stops before the next operation, reports the rest as not run, and still writes `cn=status` and releases the lock.

A real run prints the plan (as `--dry-run` does) and then the result:

```text
Result: 41 applied, 2 already in place, 3 skipped because an earlier step failed, 1 failed

Failed (1)
  step 12 cn=hpc-users,ou=group,dc=local: add member uid=jdoe,ou=people,dc=local
    LDAP Result Code 65 "Object Class Violation": ...

Skipped (3)
  step 13 cn=hpc-users,ou=group,dc=local: add memberUid jdoe
    needs step 12, which failed (pair:...)

Created groups (1)
  lab (3 members)

Changed groups (2)
  hpc-users +2 -1: +asmith +bwong -cdavis
  staff +0 -0 (1 renamed)
```

`--debug` also prints one line per applied operation to stderr (`debug: step 7 applied: ...`).

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
│                         roleOccupant: uid=jdoe,ou=people,dc=local   (one per member Dolly added, always a DN)
├── cn=lock                                                              (only while a run is active)
└── cn=status             last successful run, current failure, last notification
```

Dolly creates `state_base` and its children if they're missing (`state_base` needs an `ou=` or `cn=` RDN for that; otherwise create it by hand). It doesn't create `users_base` or `groups_base`. If those are missing, it stops with a clear error. User records also keep small `description` key=value notes: `missing-since`, an RFC 3339 UTC timestamp (for example `2026-09-21T19:00:00Z`) recording when a user went missing from AD; `saved-shell`, the shell to restore after a disabled account is re-enabled; and `renaming-from=<old DN>`, present only while a rename is in progress (see below).

The rules:

- A user in the AD group but not in the target group is added and recorded as Dolly-owned.
- A Dolly-owned member who left the AD group, or was deleted from AD, is removed from the target group and from the record.
- A member who is already in the target group without a record was added locally. Dolly never removes that member, even if the same user is also in the AD group. One race: a member that a local admin adds between Dolly's read and its write can end up Dolly-owned, because Dolly writes the ownership record before the member value. The window is one run.
- A member value removed by hand on the target is re-added on the next run only if Dolly owns that membership (the group's record lists it in `roleOccupant`, because it came from AD) and the AD group still lists the user: for Dolly-owned members, AD wins. A local member removed by hand is never re-added as a repair; that's the admin's business. Known consequence: a local member who is *also* in the AD group, once removed by hand, comes back as a Dolly-owned member, because Dolly can't tell that from a new AD membership without keeping extra state.
- A target user or group with the same `uid` or `cn` as an AD entry but no ownership record is a conflict. Dolly logs an error and leaves it untouched; a conflict user can still be added as a group member by DN, since that doesn't require an owned user entry.
- When two AD users map to the same `uid`, the winner is chosen in order: the current owner (an ownership record already points at this AD user), then any entry with an ownership record, then an in-scope entry, then the lowest DN. The other user is a warning, nothing more.
- A user or group renamed in AD (same `objectGUID`) is renamed in place, and its `member` and `memberUid` values are updated in every group. The record gets a `renaming-from=<old DN>` note first; only then does Dolly perform the `modrdn` and fix up `member`, `memberUid`, `seeAlso`, and `roleOccupant` values; the note is removed last. A crash mid-rename is recoverable from the note alone. Renames happen in place — Dolly never moves an entry to a different parent. An owned entry found somewhere other than directly under its configured base is reported as a conflict, not moved.
- A user who is gone from AD (deleted, moved out of scope, filtered out, or missing the attribute `uid` is mapped from) loses all Dolly-owned memberships right away. The user entry is deleted only when `prune_users` is on and the user has been gone for `prune_after_days`. At that point Dolly also removes the user from groups where they were added locally, so no group points at a user that no longer exists. A user who still has a uid but lost another `required` attribute (for example `uidNumber`) counts as gone for its user entry (the prune clock starts), but keeps its memberships.
- A group deleted from AD loses its Dolly-owned members and its ownership record. The group itself stays.
- Ownership records always name members by DN in `roleOccupant`, built as `<rdn>=<uid>,<users_base>`, even when groups use `memberUid` only. With `memberUid` only, that DN is just an identifier: the user entry must exist somewhere under `users_base` with that uid (see below), but not necessarily at that DN.
- With `member` in the membership list (`groupOfNames`), a new AD group with no resolvable members isn't created until it has one, because `groupOfNames` needs at least one `member`. With `memberUid` only, `posixGroup` may be empty, so the group is created right away.
- With `member` in the membership list, if removing Dolly-owned members would leave a group with no members at all, Dolly adds the `empty_group_member` placeholder instead of breaking the `groupOfNames` schema. It removes the placeholder once the group has a real member again.
- A disabled AD account (`userAccountControl` bit `0x2`) keeps its user entry, but Dolly removes it from every group where it's Dolly-owned and sets `loginShell` to `disabled_shell`, because an SSH key would otherwise still work. Local memberships are untouched. When the account is re-enabled, Dolly restores the saved shell only if `loginShell` is still `disabled_shell`; if an admin changed it in the meantime, the admin's value stays and the `saved-shell` note is dropped. A user who is already disabled the first time Dolly creates them is created with `disabled_shell` directly, and their mapped shell is saved in the record for a later re-enable.
- Attributes in `create_only` are written when Dolly creates the user and never again, so a shell or home directory changed on the LDAP server stays changed. `disabled_shell` is the one exception.
- The AD primary group (`primaryGroupID`, usually Domain Users) is ignored, because AD doesn't list it in the group's `member` attribute.
- To be a group member, an AD user needs only the attribute its `uid` is mapped from. A user entry needs every attribute in `mapping.users.required` (`uid`, `uidNumber`, and `gidNumber` by default): a user missing one gets no entry and is listed once in the run summary (only in runs that sync users), but can still be a member. Groups need `name` and `gidNumber`. A group or a user without a uid is ignored everywhere, inside or outside the search bases, and an ignored child group isn't flattened into its parent either.
- A `uidNumber` or `gidNumber` Dolly would write must be an integer from 0 to 4294967294 (`uid_t` and `gid_t` are unsigned 32-bit, and 4294967295 is reserved as -1). An entry with any other value (for example an 11-digit university ID) is ignored and listed once in the summary, like one missing a required attribute.
- The users that are supposed to be added to a target group must exist on the target. This is always on; there is no setting for it. An AD user is added to a target group only if an entry with its uid exists under `users_base`, Dolly-owned or local (with `member` in the membership list: an entry at the member DN). A user created earlier in the same run counts; a user pruned in the same run doesn't, so a pruned user's former memberships are never re-added. If the user doesn't exist, Dolly skips that member. This matters most for groups-only replication, where Dolly creates no users: there, neither the AD users nor the target users need `uidNumber` (the lookup matches on `uid` only), and only the groups need `gidNumber`. Skipped members are not warnings and never trigger a notification: the run summary shows a single line, `members skipped: N not on the target (use --debug to list them)`, omitted when zero, and `--debug` prints one line per skipped (group, user) pair to stderr. A skipped member is re-evaluated every run and added (and recorded as Dolly-owned) once its entry exists. A Dolly-owned member whose entry disappeared is left in place, neither re-added nor removed, until the user leaves the AD group.
- A member outside the configured search bases that matches the search filter and has a uid is followed. An out-of-scope user with every required attribute becomes a normal Dolly-managed user. An out-of-scope child group is only flattened into its parent and isn't created on the target.
- A member that exists in AD but matches neither `source.users.filter` nor `source.groups.filter` is skipped and counted as filtered in the summary, as if it were outside the search bases.
- Members that are neither users nor groups (computers, contacts, foreign security principals) are skipped.
- The spelling of `uid` is kept exactly as it is in AD, including uppercase letters.
- Duplicate `uidNumber` or `gidNumber` values, between AD entries or against local entries, don't block the run. They're listed as warnings in the run summary.
- A changed `uidNumber` or `gidNumber` is applied, and it's always reported, because file ownership depends on it.
- Values for ASCII-only attributes — `gecos`, `homeDirectory`, and `loginShell` — are transliterated to ASCII, because OpenLDAP rejects non-ASCII characters there.
- One bad entry doesn't stop the run. If the server rejects a change, Dolly logs it, carries on with the rest, reports it in the summary, and exits with code 1.

### Run lock

Before reading or writing anything else, Dolly creates `state_base` if it's missing (only `state_base` itself; the record containers come with the plan), then creates `cn=lock,<state_base>` with an LDAP add. The add is atomic, so only one host can hold the lock. The entry (an `organizationalRole`) records the host, PID, start time, and command as `description` values. If another run holds a fresh lock, Dolly prints one line to stderr and exits `0` without mail and without touching `cn=status`. Dolly deletes its lock when the run ends, even after an error or a `SIGTERM`/`SIGINT`, using a fresh short timeout, and only if the lock is still its own. `dolly sync` and `dolly adopt` take the lock; `--dry-run` doesn't. A run also aborts itself after `run_timeout`, which must be shorter than `lock_ttl`, so a live run never has its lock broken.

If a run crashes and leaves the lock behind:

- A lock is treated as stale only when *both* its server `createTimestamp` and the holder's `started=` note are at least `lock_ttl` old by the local clock (a lock without a `started=` note is judged by `createTimestamp` alone), so one wrong clock can't break a live run's lock. If `createTimestamp` or the `started=` note is more than 5 minutes in the local future, the clocks disagree: Dolly refuses to judge the lock, leaves it alone, records the error naming the skew in `cn=status` as a failure (so it is mailed), and exits `1`; a future `started=` would otherwise keep the lock fresh forever. Keep the Dolly hosts and the LDAP server NTP-synced.
- The next run breaks a stale lock by first *renaming* it (`modrdn` to `cn=lock-stale-<short host>-<unix time>-<pid>`), which only one host can win, and then deleting the renamed entry and taking the lock (one retry). A host that loses the rename (the lock is gone, or the stale name already exists) treats the lock as held. If the entry it renamed turns out to be fresh (another host broke the stale lock and took a new one in between), it renames it back. Breaking a lock logs a warning and sends a notification.
- `dolly unlock` shows who holds the lock and since when (flagging clock skew), and removes it after confirmation. `dolly check` warns about a lock held longer than `lock_ttl` by its `createTimestamp`, or one with clock skew.

### Adopting an existing tree

A tree written by ad2openldap has no ownership records. Run `dolly adopt` once before the first sync. For every AD user and group that matches a target entry by `uid` or `cn`, it creates an ownership record. In each group it marks the members who are also in the AD group as Dolly-owned, and treats every other member as local.

Adopt claims every AD user that has a `uid`, even one missing `uidNumber` or `gidNumber` (or with an invalid one), so the following `dolly sync` can then treat those user entries as gone — pruning them if `prune_users` is enabled — instead of never claiming them at all. Such users stay group members until then, since a member needs only a uid. Groups still need to pass the full required-attribute check (`name` and `gidNumber`) to be adopted. Adopt flattens through every child group, the same way `sync` does, and counts disabled AD accounts as members when deciding which existing target members are Dolly-owned.

One caveat: a user removed from an AD group shortly before the adoption looks local, so Dolly will never remove them. Check `dolly adopt --dry-run` for surprises.

A second, intended difference from ad2openldap: users without a `gidNumber` (the old tool gave them 65534) get no user entry from `sync`, per the required-attributes rule, and members that only arrived through a child group without a `gidNumber` aren't members at all. Adopt still claims them, as above, so `dolly adopt --dry-run` lists them. On the first sync the members of such child groups lose their memberships (the guard may ask for `--force`), and with `prune_users` on, users without a `gidNumber` are deleted after `prune_after_days`, which removes their memberships too.

## Running it on a schedule

**systemd user timer (default)**

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

`TimeoutStartSec=1h` keeps a hung run from blocking the timer forever: it is above the default `run_timeout` (45m), and `dolly install` warns if the config's `run_timeout` isn't below it. systemd stops a timed-out service with `SIGTERM` (the default `KillMode`), which Dolly handles like `run_timeout`: it stops between operations, records `cn=status`, and releases the lock.

`dolly install --groups` (or `--users`) bakes the flag into the service, for example for a groups-only deployment, and `--config FILE` adds the config's absolute path (a relative path is made absolute), which also becomes the file `dolly install` creates from the template if it's missing. Without `--config` the service relies on the default lookup (`$XDG_CONFIG_HOME/dolly/dolly.yaml`), because its working directory isn't where you ran `dolly install`:

```bash
dolly install --groups
# dolly.service then has:
# ExecStart=%h/.local/bin/dolly sync --groups
dolly install --groups --config ~/targets/ldap-a.yaml
# ExecStart=%h/.local/bin/dolly sync --groups --config /home/svc-dolly/targets/ldap-a.yaml
```

`dolly install` prints the resulting `ExecStart=` line. Re-running it with different flags rewrites the service. It doesn't enable the timer; it ends with the next steps (edit the config, `dolly check`, `dolly sync --dry-run`, then):

```bash
systemctl --user enable --now dolly.timer
loginctl enable-linger "$USER"   # keep the timer running while you're logged out (may need an admin)
```

`dolly install` is safe to re-run. It never overwrites an existing config (it creates it with mode `0600`, in a `0700` directory), replaces the binary atomically (mode `0755`) only when it differs, and only rewrites the units when their content changed. It runs `systemctl --user daemon-reload` on every install, so a re-run after a failed reload fixes it. `dolly uninstall` stops and disables the timer (a timer that isn't loaded is fine), removes both units and reloads, removes `~/.local/bin/dolly`, and keeps the config and any secrets next to it. It also handles a few common gaps in a service account's environment:

- **`XDG_RUNTIME_DIR` is unset** (typical after `su -` or `sudo -iu`, where `systemctl --user` fails with "Failed to connect to bus"). If `/run/user/<uid>` exists, Dolly points `XDG_RUNTIME_DIR` and `DBUS_SESSION_BUS_ADDRESS` at it for its own `systemctl --user` calls. If it doesn't exist, the user has no session and no linger: Dolly still installs the binary, config, and units, then stops with exit `1` and tells you to run `loginctl enable-linger <user>` and `dolly install` again. Dolly never edits your shell startup files.
- **`~/.local/bin` is missing or not on `PATH`.** Dolly creates the directory and warns if it isn't on `PATH`. The timer doesn't care, because the unit uses the absolute path `%h/.local/bin/dolly`.
- **The login `$HOME` differs from the passwd home** (common on AD/SSSD hosts: the shell has `HOME=/home/<user>`, while the passwd entry and so the `systemd --user` manager use `/home/<DOMAIN>/<user>`). `$HOME` always wins: Dolly reads the manager's `HOME` and `XDG_CONFIG_HOME` with `systemctl --user show-environment`, keeps the binary, config, and units under the shell's `$HOME` as usual, and registers the units with `systemctl --user link`, so systemd puts symlinks in its own unit directory (Dolly writes nothing there itself). Because the manager would expand `%h` to its own home, `ExecStart` then uses the absolute binary path and has `--config` (unless the manager's own default lookup already finds the config) (the default config's absolute path if you gave none), and `dolly install` prints a note explaining this. An existing regular `dolly.service` or `dolly.timer` in the manager's directory is left alone with a warning. `dolly uninstall` removes those links only when they point at Dolly's files. If `show-environment` fails, Dolly assumes the manager uses `$HOME`.
- **Not Linux.** Without systemd (for example macOS), Dolly installs the binary and config, skips the units, and says so.

## Notifications

With `on: changes`, Dolly mails when a run made changes. Failures are always reported, whichever setting you choose. Dolly sends at most **one email per run**, in plain text. It's a summary of everything that happened: a short header (host, command, start and end time, outcome, and why this mail was sent), the failure if any, then the run's output exactly as the journal has it: the plan with its counts, adds, removals, renames, changed ID numbers, and warnings, the result with the created and changed groups and their members, and the errors. A hundred changed users is still one mail. A body longer than 2,000 lines is cut, with a note pointing at `journalctl --user -u dolly`.

Dolly keeps a `cn=status` entry (an `organizationalRole`) under `state_base` with the last successful run, the current failure, and when it last sent mail, as `description` key=value notes: `last-run`, `last-success`, `last-result` (a one-line count summary), `failure-since` and `failure` (only while a failure persists), `last-notified` (the last successful send), `failure-notified` (when the last failure mail was sent, with a short hash of the failure it reported; only while a failure persists), `notify-error` (the last failed send, cleared by a successful one), and `warnings-hash` (see below). Every real run that got the lock writes it, whether it succeeded, failed to read, tripped the guard, had per-entry errors, or was stopped.

Each run that got the lock decides from those notes, before writing them, whether to mail:

- **Failures, whatever `on` says.** A read error, a tripped guard, a rejected operation, and a stop by signal or `run_timeout` are failures. Dolly mails when a failure first appears (or when it was never mailed, for example because the send failed), then a reminder at most every `remind_every` (by `failure-notified`) while it persists, and one "recovered" mail when a run succeeds again. So an AD outage overnight is two or three mails, not 96. While a failure persists, a *different* failure (its text differs from the failure last mailed, ignoring counts, timestamps, and durations) is mailed right away as "failure changed" (at most once an hour, or once per `remind_every` if that is shorter, so a failure flapping between two texts can't mail every run), and only failure mails reset the reminder clock, so a changes or warnings mail sent during the failure doesn't postpone the reminder.
- **A broken stale lock** (see "Run lock") is mailed, even if the run then succeeds.
- **`on: changes`** also mails when the run applied at least one operation; **`on: always`** mails every run.
- **Warnings** (ignored entries, conflicts, duplicates, groups waiting for members; not changed ID numbers, which are changes) are mailed only when the list changes, whatever `on` says. Dolly stores a short hash of the list in `warnings-hash`, one note per run scope (`warnings-hash`, `warnings-hash-users`, `warnings-hash-groups`, `warnings-hash-adopt`), so alternating `--users` and `--groups` runs don't flip it. A run that failed before planning leaves the hash alone.
- A run that finds the lock held never mails, and neither does `--dry-run`. A run that can't reach the target at all can't read or write `cn=status`, so it isn't mailed either; it logs the error and exits `1` (watch the unit's result in systemd).

`last-notified`, `failure-notified`, and `warnings-hash` are updated only after a successful send, so a failed send is retried by the next run. A mail failure never changes the run's exit code or stops the run: Dolly logs it to stderr and records `notify-error`. Each SMTP step is bounded by `network_timeout`.

SMTP without authentication (plain relay, the usual campus relay on port 25) needs only `smtp_host`, `smtp_port`, `from`, and `to`: with no username Dolly never sends `AUTH`, with or without StartTLS. With a username it authenticates with `AUTH PLAIN`, only over TLS (StartTLS, or implicit TLS on port 465). StartTLS and implicit TLS verify the server's certificate against the system trust store; there is no way to skip verification. An empty `smtp_host` disables notifications.

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
- **Target LDAP:** the bind DN needs write access to `users_base`, `groups_base`, and `state_base`, and ideally nothing else (a groups-only deployment needs only read access to `users_base`). It must also be able to read *every* entry under `groups_base` and `state_base`, and under `users_base` in runs that sync users. OpenLDAP's default `olcSizeLimit` (500, or 10000 in the ad2openldap config) truncates searches for any DN except the rootdn, so either bind as the rootdn or raise the limit for Dolly's DN with `olcLimits` on the database entry, for example `olcLimits: dn.exact="cn=dolly,dc=local" size=unlimited time=unlimited`. Dolly aborts on a truncated read, and `dolly check` tests for it. A `--groups` run never reads all of `users_base`, so a large `users_base` behind a hard limit is fine there.

## Building from source

```bash
git clone https://github.com/dirkpetersen/dolly.git
cd dolly
go build -o dolly ./cmd/dolly
go test ./...
```

Requires Go 1.22 or later to build (CI checks that the code still compiles and vets with 1.22). Release binaries are built with the latest stable Go, for its current `crypto/tls` and `crypto/x509` fixes. Built on [go-ldap/ldap](https://github.com/go-ldap/ldap).

## Releasing

CI (`.github/workflows/ci.yml`) runs `gofmt`, `go vet`, `go test -race`, and a GoReleaser config check on every push and pull request that touches Go code. An `integration` job starts an OpenLDAP service container (`osixia/openldap` with rfc2307bis) and runs `go test -tags integration ./...`, which tests the target writer (apply, run lock, `cn=status`) against it. To run those tests locally, point `DOLLY_IT_URL`, `DOLLY_IT_BIND_DN`, `DOLLY_IT_PASSWORD`, and `DOLLY_IT_BASE` at a scratch server (`DOLLY_IT_SCHEMA=rfc2307` for one with `nis.schema`); each test works in its own subtree and deletes it afterwards.

To cut a release, push a semver tag:

```bash
git tag -a v0.1.0 -m "v0.1.0"
git push origin v0.1.0
```

`.github/workflows/release.yml` then runs the tests and [GoReleaser](https://goreleaser.com), which publishes static binaries for Linux and macOS (amd64 and arm64), `checksums.txt`, and a changelog to the GitHub release. Each archive also contains `LICENSE`, `README.md`, and `dolly.yaml.template`. The release workflow uses the latest stable Go (`actions/setup-go` with `go-version: stable`), not the 1.22 minimum from `go.mod`. `dolly version` prints the tag, commit, build date, and Go version.

## Contributing

Issues and pull requests are welcome. Please include a dry-run output or a minimal LDIF example when reporting mapping bugs, with anything sensitive redacted. See [CONTRIBUTING.md](CONTRIBUTING.md) for how to set up a fork and how this project is developed with Claude Code.

## License

MIT. See [LICENSE](LICENSE).

---

*No sheep were harmed in the replication of this directory.*
