# Run lock

Dolly has no local lock file. The run lock lives entirely in the target LDAP, so two hosts syncing the same target never race each other.

## Taking the lock

Before reading or writing anything else, Dolly creates `state_base` itself if it's missing (just that entry — the ownership-record containers come with the plan), then creates `cn=lock,<state_base>` with a plain LDAP add. The add is atomic at the server, so only one host can ever succeed in creating it. The entry is an `organizationalRole` recording the host, PID, start time, and command as `description` values.

`dolly sync` and `dolly adopt` take the lock; `--dry-run` never does — it only reads.

## Releasing the lock

Dolly deletes the lock entry when the run ends, whether it succeeded or failed, via a `defer`, using a fresh short timeout and only if the lock is still its own. Because a `defer` does not run when the process is killed by a signal, Dolly also releases the lock on `SIGTERM` and `SIGINT`, using `signal.NotifyContext`, so a normal service stop or manual `Ctrl-C` still cleans up.

If another host already holds a fresh lock when a run starts, Dolly prints one line to stderr and exits quietly with code `0`: no error, no mail, and `cn=status` is left untouched. This is the expected outcome of two overlapping scheduled runs, not a failure.

## `run_timeout` vs. `lock_ttl`

Every run enforces a hard `sync.run_timeout`, which must be configured shorter than `sync.lock_ttl`. This ordering guarantees that a run which is still legitimately in progress never has its own lock mistaken for stale and broken out from under it. `sync.network_timeout` additionally bounds each individual connect and operation against AD and the target, so a single hung network call can't stall a run indefinitely.

## Stale locks

If a run crashes (killed, host rebooted, and so on) and leaves the lock behind, the *next* run checks it against `sync.lock_ttl`. The lock is stale only when *both* its server `createTimestamp` and the holder's `started=` note are at least `lock_ttl` old by the local clock (a lock without a `started=` note is judged by `createTimestamp` alone), so one wrong clock can't break a live run's lock. If `createTimestamp` is more than 5 minutes in the local future, the clocks disagree: Dolly leaves the lock alone and exits `1` with an error naming the skew.

!!! warning
    Keep the Dolly hosts and the LDAP server NTP-synced.

A stale lock is broken like this:

1. The stale lock is renamed (`modrdn`) to `cn=lock-stale-<short host>-<unix time>-<pid>` first. Because `modrdn` is also atomic, only one competing host can win this rename; a host that loses it (the lock is gone, or the stale name already exists) treats the lock as still held.
2. The winner then deletes the renamed entry and takes a fresh lock (one retry). If the entry it renamed turns out to have been fresh after all — another host broke the same stale lock and took a new one in between — the winner renames it back instead of deleting it.
3. Breaking a lock logs a warning and sends a notification — even if the run that broke it then goes on to succeed. See [Notifications](../operations/notifications.md#when-mail-is-sent).

## `dolly unlock`

For a faster recovery than waiting out `lock_ttl`, run `dolly unlock`. It shows who currently holds the lock and its age by `createTimestamp`, then removes it after you confirm. Without `--yes`, Dolly requires stdin to be a terminal and refuses (exit `1`) otherwise, rather than guess — for example, it won't act unattended under cron. `--yes` skips the prompt and works non-interactively. Use this right after you know a run has crashed and won't clean up after itself, when no run is active. See [Commands](../commands.md#dolly-unlock).

## One Dolly per target

Each target server has its own `state_base`, its own lock, and its own config file — there is no multi-target mode within a single Dolly process. If one host needs to sync several independent target servers, run separate config files and pass `--config` for each; see [Configuration](../configuration.md).
