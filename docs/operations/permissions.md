# Permissions

## Active Directory

A regular, read-only service account is enough for `source.bind_dn`. Dolly never writes to AD — it only binds and searches. The account needs read access to the configured `users.base` and `groups.base`, and to any out-of-scope entries referenced as group members (see [out-of-scope members](../how-it-works/ownership.md#out-of-scope-members)).

## Target LDAP

The `target.bind_dn` needs:

- **Write access** to `users_base`, `groups_base`, and `state_base`, and ideally nothing else.
- **Read access to every entry** under those bases — not a filtered or size-limited view.

## `olcSizeLimit` and `olcLimits`

OpenLDAP's default `olcSizeLimit` (500, or `10000` in the ad2openldap-era config) truncates search results for any bind DN except the server's rootdn. Since Dolly [aborts the whole run](../how-it-works/index.md#2-bleat) on a truncated read rather than planning from partial data, a bind DN that hits this limit will block every sync once the target tree grows past it.

You have two options:

1. **Bind as the rootdn.** Simplest, but grants Dolly's bind DN full access to the whole directory, not just the bases it needs.
2. **Raise the limit for Dolly's specific DN** with `olcLimits`, leaving the global `olcSizeLimit` untouched for everyone else. Add it to the database entry (for example `olcDatabase={1}mdb,cn=config`) and adjust the DN to your `target.bind_dn`:

```text
olcLimits: dn.exact="cn=dolly,dc=local" size=unlimited time=unlimited
```

Adjust `dn.exact` to match the actual `target.bind_dn` in your config. `dolly check` tests for a truncated search using the configured bind DN, so run it after changing either the account or the limit — see [Commands](../commands.md#dolly-check).

!!! note
    A `size=unlimited` grant only expands what this one DN can retrieve in a search; it doesn't grant it write access anywhere. Access control (`olcAccess`) and size limits (`olcLimits`) are configured separately.
