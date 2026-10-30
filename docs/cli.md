# CLI reference — bucket-next-cli

`bucket-next-cli` is a sister binary for **offline** maintenance of a shard's state. It reads the same YAML config as
the service and reads or edits the state directory directly; it does not talk to a running service.

```bash
bucket-next-cli -config <path> [-tenant <tenant>] <command> [args]
```

## Golden rules

1. **The commands that change state take the state directory's lock** (`init-state`, `adopt-tenant`, `init`, `reset`,
   `delete`, `clear`, `rename`, `export-v2`): while the service runs they are refused — stop it first. The read-only
   commands (`check`, `info`, `tenants`, `list`, `status`) never write anything, not even on a pre-tenant directory the
   service has not carried over yet.
2. The CLI applies the **same checks** as the service: it refuses state that belongs to another `shard_id`,
   `total_shards`, uuid key or Snowflake epoch, a tenant file that holds another tenant, and a lost tenant file.
3. The changing commands refuse a pre-tenant `state.json`: the service carries it over at its next start (an older
   binary may still be writing it).
4. While the service is stopped, the CLI cannot know which reserved IDs were actually handed out, so it treats the
   stored mark as the next value. Moving below it needs `--force`.

## Shard commands

| Command | Effect |
|---|---|
| `check` | What the service's next start will do, changing nothing. Exit 0: it starts (the plan is printed); 1: it would refuse (the reason is printed); 3: no state yet. The deploy runs it with the NEW binary before installing anything. |
| `info` | The shard: identity, uuid key fingerprint, Snowflake epoch and mark, the carry-over, each tenant's entity counts, what the next start will do. |
| `tenants` | The declared tenants and the roster: served, entity count, on the roster since, file. |
| `init-state` | Create the state of a **brand-new** shard (`shard.json` + a file per declared tenant). Refuses if state exists. |
| `adopt-tenant <tenant>` | Put a tenant's existing file on the roster (after `shard.json` was restored from a backup older than that file). |

## Tenant commands (`-tenant <tenant>`, one of the config's tenants)

| Command | Effect |
|---|---|
| `list` | The tenant's entities: type, mark, next value. |
| `status <entity>` | One entity in detail. |
| `init <entity> <type> [start]` | Register an entity. `start` is `int`/`long` only, ≥ 1, snapped to the shard. |
| `reset <entity> <value> [--force]` | Move an `int`/`long` counter forward; `--force` to move it back. |
| `delete <entity> --force` | Remove one entity. If it is used again it restarts from its first value — IDs repeat. |
| `clear --force` | Remove every entity of the tenant (same warning). |
| `rename <from> <to>` | Carry a counter to a new name: the next ID under the new name is the one the old name would have issued. Refused for `uuid*` (the scramble is derived from the name) and while `legacy_tenant` is this tenant (a pre-tenant caller would register the old name afresh — two counters, the same IDs). |
| `export-v2` | The route back to a pre-tenant binary: writes `state.json` (format 2) from the CURRENT marks and retires `shard.json`, so the old binary carries on from here and a later start of this binary carries the file over again. Refused while another tenant has entities, or for a uuid entity with the tenant's scramble. |
| `help` | Usage. |

## Examples

```bash
# new shard: create its state
bucket-next-cli -config my.yaml init-state
# OK created the state of shard 1/2 in /var/lib/bucket-next/shard-1 for tenants btcl, acme

# what will the next start do?
bucket-next-cli -config my.yaml check
# OK: the next start will carry the pre-tenant state.json (format 2, 1 entities) over into tenant btcl, every mark kept
#   tenant btcl             1 entities

# register "order" in tenant btcl as long, starting at 1000 (service stopped)
bucket-next-cli -config my.yaml -tenant btcl init order long 1000
# OK initialised "order" as long for tenant btcl on shard 1; first value will be 1000

# move a counter forward
bucket-next-cli -config my.yaml -tenant btcl reset order 5000
# OK "order" of tenant btcl on shard 1/1: next value 1000 -> 5000

# while the service runs, every changing command is refused
bucket-next-cli -config my.yaml -tenant btcl reset order 9001
# error: state: another process holds the state in /var/lib/bucket-next/shard-1 (is bucket-next running on it?) — stop it first: ...

bucket-next-cli -config my.yaml -tenant btcl status order
bucket-next-cli -config my.yaml tenants
bucket-next-cli -config my.yaml info
```

## CLI or HTTP?

| Task | Tool |
|---|---|
| Mint IDs | HTTP `next-id` / `next-batch` |
| Create a new shard's state | `-init` on first start, or CLI `init-state` |
| Add a tenant | `tenants:` in the config + a restart (no command) |
| Seed an entity before launch | HTTP `POST /api/v2/{tenant}/init` (live) or CLI `init` (stopped) |
| Move a counter | HTTP `PUT /api/v2/{tenant}/reset` (live; knows exactly what was issued) or CLI `reset` |
| Inspect a live shard | HTTP `/api/v2/{tenant}/list`, `/status`; CLI `list` / `status` / `info` (read-only, safe while it runs) |
| Will the new binary accept this state? | CLI `check` |
| Delete or rename entities, the route back | CLI only |
| Recover when the service won't start | CLI `check` says why; `info` shows the identity |
