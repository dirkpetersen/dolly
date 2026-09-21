# Run lock

Dolly has no local lock file. The run lock lives entirely in the target LDAP, so two hosts syncing the same target never race each other.

## Taking the lock

Before writing anything, Dolly creates `cn=lock,<state_base>` with a plain LDAP add. The add is atomic at the server, so only one host can ever succeed in creating it. The entry records the host, PID, and start time of the run that holds it.

`--dry-run` never takes the lock — it only reads.

## Releasing the lock

Dolly deletes the lock entry when the run ends, whether it succeeded or failed, via a `defer`. Because a `defer` does not run when the process is killed by a signal, Dolly also releases the lock on `SIGTERM` and `SIGINT`, using `signal.NotifyContext`, so a normal service stop or manual `Ctrl-C` still cleans up.

If another host already holds the lock when a run starts, Dolly exits quietly with code `0`: no error, no mail. This is the expected outcome of two overlapping scheduled runs, not a failure.

## `run_timeout` vs. `lock_ttl`

Every run enforces a hard `sync.run_timeout`, which must be configured shorter than `sync.lock_ttl`. This ordering guarantees that a run which is still legitimately in progress never has its own lock mistaken for stale and broken out from under it. `sync.network_timeout` additionally bounds each individual connect and operation against AD and the target, so a single hung network call can't stall a run indefinitely.

## Stale locks

If a run crashes (killed, host rebooted, and so on) and leaves the lock behind, the *next* run detects staleness by comparing the lock entry's `createTimestamp` — the server's clock, not the local host's — against `sync.lock_ttl`. If it's older than that, the lock is treated as stale and broken:

1. The stale lock is renamed (`modrdn`) to a unique name first. Because `modrdn` is also atomic, only one competing host can win this rename.
2. The winner then deletes the renamed entry and proceeds to take a fresh lock.
3. Dolly logs a warning and sends a notification that a stale lock was broken.

## `dolly unlock`

For a faster recovery than waiting out `lock_ttl`, run `dolly unlock`. It shows who currently holds the lock and since when, then removes it after you confirm (or immediately with `--yes`). Use this right after you know a run has crashed and won't clean up after itself. See [Commands](../commands.md#dolly-unlock).

## One Dolly per target

Each target server has its own `state_base`, its own lock, and its own config file — there is no multi-target mode within a single Dolly process. If one host needs to sync several independent target servers, run separate config files and pass `--config` for each; see [Configuration](../configuration.md).
