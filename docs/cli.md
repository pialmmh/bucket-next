# CLI reference — bucket-next-cli

`bucket-next-cli` is a sister binary for **offline** maintenance of a shard's state file. It
reads the same YAML config as the service and edits the state file directly; it does not talk
to a running service.

```bash
bucket-next-cli -config <path> <command> [args]
```

## Golden rules

1. **Stop the service before any command that changes state** (`init`, `reset`, `delete`,
   `clear`). Both processes write the same file.
2. The CLI applies the **same identity check** as the service: it refuses a state file that
   belongs to another `shard_id`, `total_shards`, uuid key or Snowflake epoch.
3. While the service is stopped, the CLI cannot know which reserved IDs were actually handed
   out, so it treats the stored mark as the next value. Moving below it needs `--force`.

## Commands

| Command | Effect |
|---|---|
| `init-state` | Create the state file for a **brand-new** shard. Refuses if it exists. |
| `list` | Every entity: type, mark, next value. |
| `status <entity>` | One entity in detail. |
| `init <entity> <type> [start]` | Register an entity. `start` is `int`/`long` only, ≥ 1, snapped to the shard. |
| `reset <entity> <value> [--force]` | Move an `int`/`long` counter forward; `--force` to move it back. |
| `delete <entity> --force` | Remove one entity. If it is used again it restarts from its first value — IDs repeat. |
| `clear --force` | Remove every entity (same warning). |
| `info` | Shard identity, uuid key fingerprint, Snowflake epoch and mark, counts, file size. |
| `help` | Usage. |

## Examples

```bash
# new shard: create its state file
bucket-next-cli -config my.yaml init-state
# OK created ./data/state.json for shard 1/1

# register "order" as long, starting at 1000 (service stopped)
bucket-next-cli -config my.yaml init order long 1000
# OK initialised "order" as long on shard 1; first value will be 1000

# on a multi-shard config the start value snaps to the shard:
#   snapped 1000 -> 1001 (shard 2 residue)

# move a counter forward
bucket-next-cli -config my.yaml reset order 5000
# OK "order" on shard 1/1: next value 1000 -> 5000

# move it back — refused without --force
bucket-next-cli -config my.yaml reset order 10
# error: refused: next value is 5000 and the service may have issued anything below it; ...

bucket-next-cli -config my.yaml status order
bucket-next-cli -config my.yaml info
bucket-next-cli -config my.yaml delete order --force
```

## CLI or HTTP?

| Task | Tool |
|---|---|
| Mint IDs | HTTP `next-id` / `next-batch` |
| Create a new shard's state file | `-init` on first start, or CLI `init-state` |
| Seed an entity before launch | HTTP `POST /api/init` (live) or CLI `init` (stopped) |
| Move a counter | HTTP `PUT /api/reset` (live; knows exactly what was issued) or CLI `reset` |
| Inspect a live shard | HTTP `/api/list`, `/api/status` |
| Delete entities | CLI only, with `--force` |
| Recover when the service won't start | CLI `info` shows the file's identity |
