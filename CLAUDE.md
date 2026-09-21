# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project state

Dolly is a Go tool (Go 1.22+, built on `github.com/go-ldap/ldap`) that replicates users and groups one way from Active Directory to OpenLDAP. The repo currently holds only `README.md`, `LICENSE` (MIT), `dolly.yaml.template`, and a Go `.gitignore`. No code exists yet. `README.md` is the spec: CLI surface, config schema (`dolly.yaml`), and sync algorithm. Keep it in sync when behavior changes.

Planned layout and commands (from README):

```bash
go build -o dolly ./cmd/dolly      # entry point lives in cmd/dolly
go test ./...
go test ./path/to/pkg -run TestName   # single test
```

CLI: `dolly sync [--users|--groups] [--dry-run] [--force]`, `dolly adopt [--dry-run]`, `dolly unlock [--yes]`, `dolly diff`, `dolly check`, `dolly install`, `dolly uninstall`, `dolly version`.

Releases: pushing a `v*` tag runs `.github/workflows/release.yml` (GoReleaser, config in `.goreleaser.yaml`, static `CGO_ENABLED=0` builds for linux/darwin × amd64/arm64). `cmd/dolly` must declare `var version, commit, date string`, which are set via `-ldflags -X main.…`. CI (`.github/workflows/ci.yml`) runs `gofmt -l`, `go vet`, `go test -race`, and `goreleaser check`, and triggers only on Go, module, or workflow changes. Keep `go.mod`'s `go` directive at or below the local toolchain.

Local Go is the distro package (`/usr/bin/go`, 1.22.2). If a dependency needs a newer Go, rely on `GOTOOLCHAIN=auto` rather than raising the floor casually.

## Hard requirements

1. **Never wipe and rebuild the target.** Every run reads *all* in-scope users and groups from AD (no `uSNChanged` incremental mode, because it misses deletions and nested-group changes; about 10,000 groups is cheap), then applies minimal add, modify, modrdn, and delete operations against the running server.
2. **Only touch what Dolly owns.** Target groups can hold members added directly on the LDAP server. On sync, add members newly in the AD group, and remove members that Dolly previously replicated but that are no longer in the AD group or no longer in AD. Leave every other member alone. Never compute membership as "replace with AD's list", and never replace a whole `member` or `memberUid` attribute. Add and delete individual values. Only write the attributes listed in the mapping. The details are in the README sections "Ownership records" and "Adopting an existing tree":
   - **Ownership lives in the target LDAP**, not in a local file: `organizationalRole` entries under `state_base` (core schema only), named `cn=<objectGUID>`. `seeAlso` points to the managed entry, and `roleOccupant` lists the members Dolly added to a group. `memberUid` ownership is derived from those DNs. Update the records in the same pass as the changes.
   - **Identity is `objectGUID`.** Renames in AD become `modrdn`, followed by updating `member` and `memberUid` values in every group.
   - **Local wins.** A member already present without a record is local and is never removed, even if the same user is also in AD. A target entry that matches an AD entry by name but has no record is a conflict: log an error and skip it. `dolly adopt`, run once, is the only thing that claims existing entries.
   - **Groups are never deleted.** A group deleted from AD loses its owned members and its record. Users are deleted only with `prune_users`, and users deleted from AD are always removed from groups.
   - **Empty groups** get the `empty_group_member` placeholder rather than violating `groupOfNames`. Remove the placeholder once a real member exists.
   - **Disabled accounts** (`userAccountControl & 0x2`) keep their entry but lose all *owned* memberships. **`primaryGroupID` is ignored.**
   - **Out-of-scope members** (outside the configured bases) are followed only if they have a `gidNumber`. Users become managed users, and child groups are flattened but not created.
   - **Run lock in LDAP:** `cn=lock,<state_base>`, taken with an atomic LDAP add and holding the host, PID, and start time. Release it in a `defer`. A lock older than `lock_ttl` (judged by the server's `createTimestamp`, not the local clock) is broken with a warning and a notification. `dolly unlock` removes it by hand. There is no local lock file.
   - **Mass-deletion guard.** Abort with no writes and send a notification if removals exceed `max_delete_percent` of owned members or users, unless `--force` is given.
3. **Users and groups sync independently.** `--users` or `--groups` alone must work, and neither may assume the other ran in the same invocation.
4. **Users and groups only.** No computers, contacts, NIS netgroups, or autofs maps, even though the predecessor exported the last two.
5. **Unprivileged, XDG, systemd --user.** Dolly runs as a regular user by default and must never require root. `dolly install` self-installs the binary to `~/.local/bin`, creates `$XDG_CONFIG_HOME/dolly/dolly.yaml` from the template (embed it with `go:embed`, and never overwrite an existing config), and writes `dolly.service` and `dolly.timer` to `$XDG_CONFIG_HOME/systemd/user/`. There is no local state or lock (both live in LDAP), and logs go to stdout/stderr for journald. Honor the XDG env vars and fall back to the spec defaults (`~/.config`, `~/.local/state`). Relative paths in the config resolve against the config file's directory. Secrets are written `0600`. No hardcoded `/etc` or `/var` paths. The README's "File locations (XDG)" table is the reference. Environment robustness (service accounts reached via `su -` or `sudo -iu` often lack a session environment):
   - **`XDG_RUNTIME_DIR` unset.** If `/run/user/<uid>` exists, set `XDG_RUNTIME_DIR` and `DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/<uid>/bus` in the environment of Dolly's own `systemctl --user` calls. If it doesn't exist, the user has no session and no linger: fail clearly and suggest `loginctl enable-linger <user>`.
   - **`~/.local/bin`.** Create it if missing, and warn (don't fail) if it isn't on `PATH`. Units reference `%h/.local/bin/dolly`, so they never depend on `PATH`.
   - **Directories.** Create config and state directories on demand with `0700` (`os.MkdirAll`).
   - **Idempotent install.** Re-running `dolly install` is harmless: never overwrite an existing config, and rewrite units only when their content changed. `dolly uninstall` is the undo.
   - **Non-Linux.** Without systemd, install the binary and config, skip the units, and say so.
   - **Never edit the user's shell startup files** (`.bashrc`, `.profile`, and so on).
6. **Config and secrets.** Config lookup order is `--config`, then `./dolly.yaml`, then `$XDG_CONFIG_HOME/dolly/dolly.yaml`. A local `dolly.yaml` is git-ignored because it may contain passwords. The checked-in example is `dolly.yaml.template`. Keep the template, the README config block, and the config structs in sync, and never commit a real `dolly.yaml`.
7. **Email notifications.** The `notify` section configures SMTP (host, port, StartTLS, optional auth via `password_file`, from, to, subject prefix) and when to send: `failure`, `changes`, or `always`. `dolly check` also tests SMTP.
8. **Missing server certificate.** If an LDAPS or StartTLS connection fails because the server's CA isn't trusted locally, fetch the chain with `openssl s_client -connect host:636 -showcerts </dev/null | awk '/BEGIN CERTIFICATE/,/END CERTIFICATE/' > ca.pem` (add `-starttls ldap` for port 389). Show the fingerprint with `openssl x509 -noout -fingerprint -sha256`, and set `ca_file` in `dolly.yaml`. Code should load `ca_file` into the TLS root pool and never fall back to `InsecureSkipVerify`. See README "TLS certificates".

## Predecessor: ad2openldap

Dolly is a simplified rewrite of [ad2openldap](https://github.com/dirkpetersen/ad2openldap), which has run in production for about 15 years. The local checkout is at `../ad2openldap`. The reference implementation is `../ad2openldap/ad2openldap/ad2openldap3` (Python, ldap3). Sample config: `../ad2openldap/ad2openldap.conf`.

**Do not copy its architecture.** It exports AD to an LDIF file and diffs it against the previous export. It turns every modify into delete+add, which drops locally added group members. It falls back to a full sync that stops slapd, firewalls port 389, and deletes `/var/lib/ldap`. Dolly exists to avoid all of that.

**Do carry over its corner-case handling** (see `retrieve_ldap_userinfo`, `retrieve_ldap_groupinfo`, `flatten_groups`, `print_users`, `print_groups`):

- **Nested groups are flattened.** Members of child groups are added recursively to the parent. A cycle guard tracks the parent chain. Groups whose only members are other groups must still be emitted.
- **Resolve members by DN, not CN.** AD member DNs map to users through a `distinguishedName → objectGUID` map. CNs are not unique and often contain escaped commas (`CN=Gow\, Edward L,...`), so parse DNs properly and don't split on raw commas.
- **Required attributes:** skip, with a warning, users without `uid` or `uidNumber` and groups without `name` or `gidNumber` (the `required` lists in the mapping). Duplicate uids log a warning, and the first one wins.
- **User defaults:** missing `gidNumber` gets 65534. A missing `unixHomeDirectory` becomes `/home/<uid>`. A missing `loginShell` gets `/bin/bash`. `gecos` is passed through. These are expressed as templates in the default mapping.
- **Empty groups:** don't create a group until it has at least one resolvable member (`groupOfNames` requires one).
- **Deliberately dropped:** the `-LS` group suffix skip, the hard-coded `ExcludedFromLDAPSync` group (use the search `filter` instead), and the `employeeID` fallback for `uidNumber`. Don't reintroduce them.
- **Group schema:** target groups are `groupOfNames` + `posixGroup` (rfc2307bis). Write members as both `member: uid=<uid>,ou=people,<base>` and `memberUid: <uid>`, sorted for stable comparison.
- **Target layout (the defaults match it):** users go under `ou=people,<base>` (RDN `uid`, `account` + `posixAccount`, `cn` = uid) and groups under `ou=group,<base>` (RDN `cn` from AD `name`). The source for uid is AD's `uid` attribute, not `sAMAccountName`.
- **Paged AD searches** use a page size of 500. Also use ranged retrieval (`member;range=0-1499`) for large groups, which the old code didn't handle.
- **Single instance:** the old tool used a pid file and broke stale locks after 20 minutes. Dolly replaces this with the LDAP run lock (see the hard requirements).
