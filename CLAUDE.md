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

CLI: `dolly sync [--full] [--users|--groups] [--dry-run]`, `dolly diff`, `dolly check`, `dolly version`.

## Hard requirements

1. **Never wipe and rebuild the target.** All changes are incremental LDAP add/modify/delete against a running server.
2. **Only touch what Dolly owns.** Target groups can hold members added directly on the LDAP server. On sync, add members newly in the AD group, and remove members that Dolly previously replicated but that are no longer in the AD group or no longer in AD. Leave every other member alone. So Dolly must persist which entries and group members it wrote. The README puts this in the state file, and `prune` also only deletes Dolly-created entries. Never compute membership as "replace with AD's list".
3. **Users and groups sync independently.** `--users` or `--groups` alone must work, and neither may assume the other ran in the same invocation.
4. **Users and groups only.** No computers, contacts, NIS netgroups, or autofs maps, even though the predecessor exported the last two.
5. **Config and secrets.** The default config is `dolly.yaml` in the working directory. It is git-ignored because it may contain passwords. The checked-in example is `dolly.yaml.template`. Keep the template, the README config block, and the config structs in sync, and never commit a real `dolly.yaml`.
6. **Missing server certificate.** If an LDAPS or StartTLS connection fails because the server's CA isn't trusted locally, fetch the chain with `openssl s_client -connect host:636 -showcerts </dev/null | awk '/BEGIN CERTIFICATE/,/END CERTIFICATE/' > ca.pem` (add `-starttls ldap` for port 389). Show the fingerprint with `openssl x509 -noout -fingerprint -sha256`, and set `ca_file` in `dolly.yaml`. Code should load `ca_file` into the TLS root pool and never fall back to `InsecureSkipVerify`. See README "TLS certificates".

## Predecessor: ad2openldap

Dolly is a simplified rewrite of [ad2openldap](https://github.com/dirkpetersen/ad2openldap), which has run in production for about 15 years. The local checkout is at `../ad2openldap`. The reference implementation is `../ad2openldap/ad2openldap/ad2openldap3` (Python, ldap3). Sample config: `../ad2openldap/ad2openldap.conf`.

**Do not copy its architecture.** It exports AD to an LDIF file and diffs it against the previous export. It turns every modify into delete+add, which drops locally added group members. It falls back to a full sync that stops slapd, firewalls port 389, and deletes `/var/lib/ldap`. Dolly exists to avoid all of that.

**Do carry over its corner-case handling** (see `retrieve_ldap_userinfo`, `retrieve_ldap_groupinfo`, `flatten_groups`, `print_users`, `print_groups`):

- **Nested groups are flattened.** Members of child groups are added recursively to the parent. A cycle guard tracks the parent chain. Groups whose only members are other groups must still be emitted.
- **Resolve members by DN, not CN.** AD member DNs map to uids through a `distinguishedName → uid` map. CNs are not unique and often contain escaped commas (`CN=Gow\, Edward L,...`), so parse DNs properly and don't split on raw commas.
- **Required user attributes:** skip users without `uid`, or without `uidNumber` (fall back to `employeeID` for `uidNumber`). Duplicate uids log a warning, and the first one wins.
- **User defaults:** missing `gidNumber` gets `default_gid` (65534). A missing `unixHomeDirectory` becomes `/home/<uid>`. A missing `loginShell` gets `default_shell`. Optional passthrough attrs include `loginShell` and `gecos`.
- **Group filtering:** skip groups without `gidNumber` and groups with no resolvable members (`groupOfNames` requires at least one `member`). Skip groups whose name ends in `-LS`. Skip groups that are members of the AD group named by `ad_excluded_group` (default `ExcludedFromLDAPSync`).
- **Group schema:** target groups are `groupOfNames` + `posixGroup` (rfc2307bis). Write members as both `member: uid=<uid>,ou=people,<base>` and `memberUid: <uid>`, sorted for stable comparison.
- **Target layout:** the old tool put users under `ou=people,<base>` (RDN `uid`, objectClasses `account` + `posixAccount`) and groups under `ou=group,<base>` (RDN `cn`). Dolly makes these configurable, and the README example uses `ou=groups` and `inetOrgPerson`, so defaults must be chosen deliberately if they need to match an existing ad2openldap tree.
- **Paged AD searches** use a page size of 500.
- **Single instance:** a pid-file lock prevents overlapping runs, and a stale lock older than 20 minutes is broken.
