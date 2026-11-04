# gidNumber from the user private group

`mapping.users.gid_from_upg` tells Dolly to take a user's `gidNumber` from the user's *user private group* (UPG) in AD, instead of from the AD user object.

## Why this exists

Many sites give every user a private group. The group has the same name as the user, or a name that follows a rule (for example `u-jdoe`). The `gidNumber` of that group is the user's primary group on Unix hosts. On the target, the user's `gidNumber` must be the `gidNumber` of the UPG.

A common problem: an Enterprise IAM team owns the AD user objects. They do not fill in `gidNumber` on the users, or they set a placeholder. But the same team (or you) can create the UPG groups, for example `u-<username>`, each with its own `gidNumber`.

With `gid_from_upg`, Dolly builds a correct target from this AD data. It finds the UPG by a naming rule and uses the group's `gidNumber` for the user.

## Configuration

```yaml
mapping:
  users:
    required: [sAMAccountName, uidNumber]  # gidNumber's source may be left out with gid_from_upg
    attributes:
      uid: sAMAccountName
      gidNumber: gidNumber            # keep it mapped: it is the attribute Dolly writes
      # ... (a present mapping.users must list every attribute: cn, homeDirectory, and so on)
    gid_from_upg:
      group_name: 'u-{{ .uid }}'      # the UPG's AD name for this user
      when: missing                   # or: always
```

| Key | Meaning |
|---|---|
| `group_name` | Required. A Go template. The data are the user's AD attributes (first values). `.uid` is the user's mapped uid, which is the target uid (also when `uid` is mapped from `sAMAccountName`). The result is compared with the AD `name` of each group. A plain name without `{{ }}` is a config error. |
| `when` | `missing` (default) or `always`. See below. |

Keep `gidNumber` in `mapping.users.attributes`. It is the attribute Dolly writes on the target user entry. The UPG only supplies the value.

## `when`: missing or always

- **`missing`**: Dolly uses the group's `gidNumber` only when the AD user has no valid `gidNumber`. A valid `gidNumber` on the user always wins. "Not valid" means: no value, more than one value, or a value that is not an integer from 0 to 4294967294.
- **`always`**: the group's `gidNumber` wins whenever the group exists. A user without a group keeps its own value.

Precedence, for each user:

| `when` | First choice | Second choice |
|---|---|---|
| `missing` | valid `gidNumber` of the AD user | `gidNumber` of the UPG |
| `always` | `gidNumber` of the UPG | valid `gidNumber` of the AD user |

If neither gives a value, the user has no usable `gidNumber`. See [No group exists](#no-group-exists).

## Which groups are searched

With `source.upg.from: ad` (see [User private groups](user-private-groups.md)), the UPG groups are the groups of that search, and the shared groups are not used. Otherwise, the UPG groups are the AD groups that Dolly already reads:

- they are in scope: under `source.groups.base` and matching `source.groups.filter`
- the group mapping accepts them: they have every attribute in `mapping.groups.required`, and a mapped `gidNumber`

The value used is the group's mapped `gidNumber`, the same value its target group gets. So the UPG groups must be inside the groups search. They are also synced as target groups like any other group. Out-of-scope groups that Dolly finds only as members are not used.

## Matching is case-insensitive

Dolly renders `group_name` for the user and compares the result with the AD group `name` without regard to case. `U-JDoe` matches a template result of `u-jdoe`.

## Duplicates

If several AD groups have the same name, the first one by DN wins. Dolly lists each other group once as a `duplicate` warning:

```text
another AD group is also named "u-jdoe"; gid_from_upg uses the first by DN, CN=u-jdoe,OU=Groups,...
```

This is the same rule as for duplicate uids.

## Out-of-range values

A UPG `gidNumber` that Dolly would use must be an integer from 0 to 4294967294, like any ID. If it is not, the user is ignored. It is counted once in the summary, and listed with `-v`:

```text
gets its gidNumber from AD group CN=u-jdoe,..., but gidNumber "4294967295" is not a valid ID
```

Dolly does not fall back to the user's own value in this case. A broken UPG stays visible.

## No group exists

- `when: missing`, user has no valid `gidNumber`, no group found: the user is ignored.
- `when: always`, user has a valid `gidNumber`, no group found: the user keeps its own value.
- `when: always`, user has no valid `gidNumber`, no group found: the user is ignored.

An ignored user is a user that lacks a required attribute. It gets no user entry. It is counted once in the summary, and with `-v` it is listed under "Warnings" as `ignored`, for example:

```text
has no gidNumber, and no AD group is named "u-jdoe" (gid_from_upg)
```

The user stays a valid group member, as for any user without `gidNumber`.

## Which runs it affects

Only the `gidNumber` that Dolly writes on user entries changes. So `gid_from_upg` matters only in runs that sync users: `dolly sync` and `dolly sync --users`. These runs always read the AD groups.

- A `--groups` run writes no user entries and is not affected.
- `dolly adopt` claims users by uid as before.

If a UPG's `gidNumber` changes, every user who takes it from that group gets the new value in the next run that syncs users. Dolly applies the change and reports it as an `id-changed` warning, as for any `gidNumber` change.

## Config validation

- `group_name` must be a Go template.
- `when` must be `missing` or `always`.
- With `gid_from_upg`, `mapping.users.required` may leave out the source of `gidNumber`. Without it, the source must be listed.
- `mapping.users.required` must still list the source of `uid` and `uidNumber`.
- `gidNumber` must still be mapped.

## Summary line

A run that syncs users prints one line when the `gidNumber` of any user came from a UPG:

```text
gidNumber:   12 users take it from their UPG group (gid_from_upg)
```

## Worked example

The AD data:

| AD object | Attributes |
|---|---|
| user `jdoe` | `uid: jdoe`, `uidNumber: 51234`, no `gidNumber` |
| user `asmith` | `uid: asmith`, `uidNumber: 51235`, `gidNumber: 100` (a placeholder from IAM) |
| user `nobody1` | `uid: nobody1`, `uidNumber: 51236`, no `gidNumber` |
| group `u-jdoe` | `gidNumber: 51234` |
| group `u-asmith` | `gidNumber: 51235` |

The config uses `group_name: 'u-{{ .uid }}'` and `when: missing`. The target entries after a sync:

- `uid=jdoe,ou=people,dc=local` with `uidNumber: 51234` and `gidNumber: 51234`. The user has no `gidNumber`, so Dolly takes it from `u-jdoe`.
- `uid=asmith,ou=people,dc=local` with `uidNumber: 51235` and `gidNumber: 100`. The user has a valid value, and `missing` lets it win.
- `nobody1` gets no entry. It is listed under "Warnings": `has no gidNumber, and no AD group is named "u-nobody1" (gid_from_upg)`.
- `cn=u-jdoe,ou=group,dc=local` and `cn=u-asmith,ou=group,dc=local` are created as normal groups.
- The summary shows `gidNumber:   1 users take it from their UPG group (gid_from_upg)`.

Now change to `when: always`. In the next run, `asmith` gets `gidNumber: 51235` from `u-asmith`. Dolly reports the change as `id-changed`. If IAM later sets a placeholder `gidNumber` on every user, `when: always` keeps the UPG values for all users whose group exists. With `when: missing`, a user with a valid placeholder keeps the placeholder.

## Troubleshooting

- **A user has no entry, and the warning says "no AD group is named ...".** Check the rendered name in the message against the real group name in AD. Check that the group is under `source.groups.base`, matches `source.groups.filter`, and has `name` and `gidNumber` set. A group that the group mapping ignores is not a UPG candidate.
- **The user has the wrong `gidNumber` with `when: missing`.** The user has a valid value of its own, and it wins. Use `when: always` if the group must win.
- **`gets its gidNumber from AD group ..., but ... is not a valid ID`.** Fix the `gidNumber` of the group in AD. It must be an integer from 0 to 4294967294.
- **A `duplicate` warning for a UPG name.** Two AD groups have the same name. Dolly uses the first by DN. Rename or remove the extra group.
- **Config error about `required`.** The source of `uid` and `uidNumber` must stay in `mapping.users.required`. Only the source of `gidNumber` may be left out.
- **No change in a `--groups` run.** This is correct. Only runs that sync users write `gidNumber`.
