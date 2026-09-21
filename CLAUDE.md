# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project state

Dolly is a Go tool (Go 1.22+, built on `github.com/go-ldap/ldap`) that replicates users and groups one way from Active Directory to OpenLDAP. The repo currently holds the spec (`README.md`), the user docs (`docs/`, `zensical.toml`), `dolly.yaml.template`, the CI, release, and docs workflows, and `.goreleaser.yaml`. No Go code exists yet. `README.md` is the spec: CLI surface, config schema (`dolly.yaml`), and sync algorithm. Keep it in sync when behavior changes.

Planned layout and commands (from README):

```bash
go build -o dolly ./cmd/dolly      # entry point lives in cmd/dolly
go test ./...
go test ./path/to/pkg -run TestName   # single test
```

CLI: `dolly sync [--users|--groups] [--dry-run] [--force]`, `dolly adopt [--dry-run]`, `dolly unlock [--yes]`, `dolly check`, `dolly install`, `dolly uninstall`, `dolly version`.

Releases: pushing a `v*` tag runs `.github/workflows/release.yml` (GoReleaser, config in `.goreleaser.yaml`, static `CGO_ENABLED=0` builds for linux/darwin × amd64/arm64). `cmd/dolly` must declare `var version, commit, date string`, which are set via `-ldflags -X main.…`. CI (`.github/workflows/ci.yml`) runs `gofmt -l`, `go vet`, `go test -race`, and `goreleaser check`, and triggers only on Go, module, or workflow changes. Local Go is the distro package (`/usr/bin/go`, 1.22.2). Keep `go.mod`'s `go` directive at 1.22 unless a dependency forces it higher. In that case `GOTOOLCHAIN=auto` fetches the newer toolchain, and the README's minimum version must be updated.

Exit codes: `0` success or lock held elsewhere, `1` error, `2` guard tripped. There is no `dolly diff` (use `sync --dry-run`).

## Architecture and testing

- **Native LDAP only.** All reads and writes go through `go-ldap`. Never shell out to `ldapmodify`, `slapadd`, or similar, and never write LDIF files. The only external command is `systemctl --user` in `dolly install`.
- **The planner is a pure function:** (AD snapshot, target snapshot, ownership records, config) → ordered plan of operations. It does no I/O. All the ownership rules live there, covered by table-driven tests, one per rule in the README's "Ownership records" list. `--dry-run` prints the same plan that a real run applies.
- **AD access sits behind a `Source` interface** with an in-memory fake for tests, because no test AD exists yet (a test OU and test LDAP server are TBD).
- **Integration tests** for the target side (apply, ownership records, lock, modrdn, size-limit detection) run against a real OpenLDAP container behind the `integration` build tag. Add the CI job together with the first code, not before. `ci.yml` fails without a `go.mod`, so its `paths` trigger excludes the workflow files for now. In the commit that adds `go.mod`, add `.github/workflows/ci.yml` and `release.yml` back to its `paths`.

## Documentation

User documentation is a [Zensical](https://zensical.org) site: config in `zensical.toml` (nav lives there), pages in `docs/`, and output in `site/` (git-ignored). `.github/workflows/docs.yml` builds it and publishes it to GitHub Pages (https://dirkpetersen.github.io/dolly/) on every push to `main` that touches `docs/` or `zensical.toml`. The repo's Pages source must be set to "GitHub Actions".

```bash
python3 -m venv .venv && .venv/bin/pip install zensical
.venv/bin/zensical serve          # preview at localhost:8000
.venv/bin/zensical build --clean --strict  # --strict fails on warnings; must pass before committing
```

`README.md` stays the spec. `docs/` is the user-facing version of it. When behavior changes, update both in the same commit. A new page must also be added to `nav` in `zensical.toml`.

## Model routing for agent work

- **Coding:** delegate to a background agent on **Opus** (`model: "opus"`).
- **Documentation** (`docs/`, README prose): delegate to an agent on **Sonnet** (`model: "sonnet"`).
- **Before every commit and push:** have an agent on **Fable** (`model: "fable"`) review the diff for correctness, spec consistency (README, `docs/`, template, CLAUDE.md), and anything that shouldn't be committed. Fix what it finds, then commit and push.

## Hard requirements

1. **Never wipe and rebuild the target.** Every run reads *all* in-scope users and groups from AD (no `uSNChanged` incremental mode, because it misses deletions and nested-group changes; about 10,000 groups is cheap). A groups-only run still reads the AD users to resolve members. **Never plan from partial data:** if any AD or target read errors or is truncated (`sizeLimitExceeded`; the old server config has `olcSizeLimit: 10000` and only the rootdn bypasses it), abort the run. `source.urls` is a list of DCs tried in order. Then Dolly applies minimal add, modify, modrdn, and delete operations against the running server.
2. **Only touch what Dolly owns.** Target groups can hold members added directly on the LDAP server. On sync, add members newly in the AD group, and remove members that Dolly previously replicated but that are no longer in the AD group or no longer in AD. Leave every other member alone. Never compute membership as "replace with AD's list", and never replace a whole `member` or `memberUid` attribute. Add and delete individual values. Only write the attributes listed in the mapping. The details are in the README sections "Ownership records" and "Adopting an existing tree":
   - **Ownership lives in the target LDAP**, not in a local file: `organizationalRole` entries under `state_base` (core schema only), named `cn=<objectGUID>`. `seeAlso` points to the managed entry, and `roleOccupant` lists the members Dolly added to a group. `memberUid` ownership is derived from those DNs. User records carry small `description` key=value notes (`missing-since`, `saved-shell`). `cn=status` holds the last success, current failure, and last notification. Dolly creates `state_base` and its children if missing, but never `users_base` or `groups_base`.
   - **Crash-safe write order.** Write the ownership record *before* adding an entry or member, and remove the entry or member *before* removing its record. The reverse order would make a Dolly-added member look local after a crash, and it would never be removed.
   - **Identity is `objectGUID`.** AD returns it as 16 mixed-endian bytes. Format it one consistent way (canonical hyphenated string). Renames in AD become `modrdn`, followed by updating `member` and `memberUid` values in every group and the `seeAlso` and `roleOccupant` values in the records. A rename that collides with an existing entry is a conflict: log it and skip.
   - **Local wins.** A member already present without a record is local and is never removed, even if the same user is also in AD. A target entry that matches an AD entry by name but has no record is a conflict: log an error and skip it. `dolly adopt`, run once, is the only thing that claims existing entries.
   - **Groups are never deleted.** A group deleted from AD loses its owned members and its record. A user gone from AD (deleted, out of scope, or missing a required attribute) loses owned memberships immediately. The entry is deleted only with `prune_users` and after `prune_after_days` (tracked as `missing-since` in the record). Pruning also removes the user's *local* memberships, the one exception to "local wins", to avoid dangling members.
   - **Per-entry errors don't abort the run** (the old tool used `ldapmodify -c`). Log, continue, report in the summary, and exit 1. Aborts are for incomplete reads, the guard, and the lock.
   - **ID numbers.** Duplicate `uidNumber`/`gidNumber` values are summary warnings and never block. A changed `uidNumber`/`gidNumber` is applied and always reported.
   - **`uid` case is preserved** as in AD. LDAP matching is case-insensitive, so compare names case-insensitively, and treat a case-only change as a rename (`modrdn`).
   - **ASCII-only attributes** (`gecos`, IA5String syntax) must be transliterated to ASCII, or OpenLDAP rejects the entry. The old tool escaped them with `unicode_escape`.
   - **`create_only` attributes** (default `loginShell`, `homeDirectory`) are written at creation and never again, so local overrides survive.
   - **Empty groups** get the `empty_group_member` placeholder rather than violating `groupOfNames`. Remove the placeholder once a real member exists.
   - **Disabled accounts** (`userAccountControl & 0x2`) keep their entry, lose all *owned* memberships, and get `loginShell` set to `disabled_shell` (this overrides `create_only`, because SSH keys would still work). Save the previous shell in the record and restore it on re-enable. **`primaryGroupID` is ignored.**
   - **Required attributes are absolute.** Users need `uid`, `uidNumber`, and `gidNumber` (no 65534 default any more). Groups need `name` and `gidNumber`. Anything without them is ignored everywhere, including for flattening, which differs from the old tool. List ignored entries in the run summary instead of warning on every run.
   - **Out-of-scope members** (outside the configured bases) with the required attributes are followed. Users become managed users, and child groups are flattened but not created. Non-user, non-group members are skipped.
   - **Run lock in LDAP:** `cn=lock,<state_base>`, taken with an atomic LDAP add and holding the host, PID, and start time. Release it in a `defer` *and* on `SIGTERM`/`SIGINT` (a `defer` doesn't run on a signal, so use `signal.NotifyContext`). If the lock is held, exit 0 quietly with no mail. `--dry-run` takes no lock. Every run has a hard `run_timeout` shorter than `lock_ttl`, plus `network_timeout` on connects and operations. A lock older than `lock_ttl` (judged by the server's `createTimestamp`, not the local clock) is broken by `modrdn` to a unique name first (only one host can win the rename), then deleted, with a warning and a notification. `dolly unlock` removes it by hand. There is no local lock file.
   - **Mass-deletion guard.** Count removals across the whole run, separately for memberships and users. Abort with no writes (exit 2, send a notification) if a count exceeds both `max_delete_min` and `max_delete_percent` of what Dolly owns, or if AD returned zero users or zero groups. `--force` overrides.
3. **One Dolly per target.** Each target server has its own `state_base`, lock, and config. There is no multi-target mode. Several targets from one host means several config files and `--config`. The source `bind_dn` may be a DN or a UPN.
4. **Users and groups sync independently.** `--users` or `--groups` alone must work, and neither may assume the other ran in the same invocation.
5. **Users and groups only.** No computers, contacts, NIS netgroups, or autofs maps, even though the predecessor exported the last two.
6. **Unprivileged, XDG, systemd --user.** Dolly runs as a regular user by default and must never require root. `dolly install` self-installs the binary to `~/.local/bin`, creates `$XDG_CONFIG_HOME/dolly/dolly.yaml` from the template (embed it with `go:embed`, and never overwrite an existing config), and writes `dolly.service` and `dolly.timer` to `$XDG_CONFIG_HOME/systemd/user/`. There is no local state or lock (both live in LDAP), and logs go to stdout/stderr for journald. Honor `XDG_CONFIG_HOME` and fall back to `~/.config`. Relative paths in the config resolve against the config file's directory. Secrets are written `0600`. No hardcoded `/etc` or `/var` paths. The README's "File locations (XDG)" table is the reference. Environment robustness (service accounts reached via `su -` or `sudo -iu` often lack a session environment):
   - **`XDG_RUNTIME_DIR` unset.** If `/run/user/<uid>` exists, set `XDG_RUNTIME_DIR` and `DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/<uid>/bus` in the environment of Dolly's own `systemctl --user` calls. If it doesn't exist, the user has no session and no linger: fail clearly and suggest `loginctl enable-linger <user>`.
   - **`~/.local/bin`.** Create it if missing, and warn (don't fail) if it isn't on `PATH`. Units reference `%h/.local/bin/dolly`, so they never depend on `PATH`. The timer has `RandomizedDelaySec=60` so two hosts don't fire in the same second.
   - **Directories.** Create the config directory on demand with `0700` (`os.MkdirAll`).
   - **Idempotent install.** Re-running `dolly install` is harmless: never overwrite an existing config, and rewrite units only when their content changed. `dolly uninstall` is the undo.
   - **Non-Linux.** Without systemd, install the binary and config, skip the units, and say so.
   - **Never edit the user's shell startup files** (`.bashrc`, `.profile`, and so on).
7. **Config and secrets.** Config lookup order is `--config`, then `./dolly.yaml`, then `$XDG_CONFIG_HOME/dolly/dolly.yaml`. A local `dolly.yaml` is git-ignored because it may contain passwords. The checked-in example is `dolly.yaml.template`. Keep the template, the README config block, and the config structs in sync, and never commit a real `dolly.yaml`.
8. **Email notifications.** The `notify` section configures SMTP (host, port, StartTLS, optional auth via `password_file`, from, to, subject prefix) and when to send: `failure`, `changes`, or `always`. **At most one email per run**, a summary of all changes, warnings, and errors, never one mail per entry. No flood: using `cn=status`, send one mail when a failure first appears, a reminder at most every `remind_every`, and one on recovery. `dolly check` also tests SMTP.
9. **Missing server certificate.** If an LDAPS or StartTLS connection fails because the server's CA isn't trusted locally, fetch the chain with `openssl s_client -connect host:636 -showcerts </dev/null | awk '/BEGIN CERTIFICATE/,/END CERTIFICATE/' > ca.pem` (add `-starttls ldap` for port 389). Show the fingerprint with `openssl x509 -noout -fingerprint -sha256`, and set `ca_file` in `dolly.yaml`. Code should load `ca_file` into the TLS root pool and never fall back to `InsecureSkipVerify`. See README "TLS certificates".

## Predecessor: ad2openldap

Dolly is a simplified rewrite of [ad2openldap](https://github.com/dirkpetersen/ad2openldap), which has run in production for about 15 years. The local checkout is at `../ad2openldap`. The reference implementation is `../ad2openldap/ad2openldap/ad2openldap3` (Python, ldap3). Sample config: `../ad2openldap/ad2openldap.conf`.

**Do not copy its architecture.** It exports AD to an LDIF file and diffs it against the previous export. It turns every modify into delete+add, which drops locally added group members. It falls back to a full sync that stops slapd, firewalls port 389, and deletes `/var/lib/ldap`. Dolly exists to avoid all of that.

**Do carry over its corner-case handling** (see `retrieve_ldap_userinfo`, `retrieve_ldap_groupinfo`, `flatten_groups`, `print_users`, `print_groups`):

- **Nested groups are flattened.** Members of child groups are added recursively to the parent. A cycle guard tracks the parent chain. Groups whose only members are other groups must still be emitted.
- **Resolve members by DN, not CN.** AD member DNs map to users through a `distinguishedName → objectGUID` map. CNs are not unique and often contain escaped commas (`CN=Gow\, Edward L,...`), so parse DNs properly and don't split on raw commas.
- **Required attributes:** skip, with a warning, users without `uid` or `uidNumber` and groups without `name` or `gidNumber` (the `required` lists in the mapping). Duplicate uids log a warning, and the first one wins.
- **User defaults:** a missing `unixHomeDirectory` becomes `/home/<uid>`. A missing `loginShell` gets `/bin/bash`. `gecos` is passed through. These are expressed as templates in the default mapping.
- **Empty groups:** don't create a group until it has at least one resolvable member (`groupOfNames` requires one).
- **Deliberately dropped:** the 65534 default for a missing `gidNumber` (such users are now ignored), the `-LS` group suffix skip, the hard-coded `ExcludedFromLDAPSync` group (use the search `filter` instead), and the `employeeID` fallback for `uidNumber`. Don't reintroduce them.
- **Group schema:** target groups are `groupOfNames` + `posixGroup` (rfc2307bis). Write members as both `member: uid=<uid>,ou=people,<base>` and `memberUid: <uid>`, sorted for stable comparison.
- **Target layout (the defaults match it):** users go under `ou=people,<base>` (RDN `uid`, `account` + `posixAccount`, `cn` = uid) and groups under `ou=group,<base>` (RDN `cn` from AD `name`). The source for uid is AD's `uid` attribute, not `sAMAccountName`.
- **Paged AD searches** use a page size of 500. Also use ranged retrieval (`member;range=0-1499`) for large groups, which the old code didn't handle.
- **Single instance:** the old tool used a pid file and broke stale locks after 20 minutes. Dolly replaces this with the LDAP run lock (see the hard requirements).
