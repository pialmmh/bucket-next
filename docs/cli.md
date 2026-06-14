# CLI reference — bucket-next-cli

`bucket-next-cli` is a sister binary for **offline** maintenance of the state store. It reads
the same YAML config as the service and edits the JSON state file directly — it does not talk
to a running service over HTTP.

```bash
bucket-next-cli -config <path> <command> [args]
```

## Golden rule

**Stop the service before any mutating command** (`init`, `reset`, `delete`, `clear`). Both
processes write the same file; concurrent writes have undefined ordering. Read-only commands
(`list`, `status`, `info`) are safe while the service runs.

## Commands

| Command | Effect |
|---|---|
| `list` | One line per entity: type, iteration, next value. |
| `status <entity>` | Detailed snapshot of one entity. |
| `init <entity> <type> [start]` | Register a new entity. Optional numeric `start` snapped to the shard's residue. |
| `reset <entity> <value>` | Move a numeric counter. Same residue-snapping as the HTTP endpoint. |
| `delete <entity>` | Remove one entity. Irreversible. |
| `clear --force` | Remove ALL entities. Refuses without `--force`. |
| `info` | Shard id, state path, entity counts by type, file size, last modified. |
| `help [command]` | Usage for one command, or the full list. |

## Examples

```bash
# List everything
bucket-next-cli -config my.yaml list

# Register "order" as long, starting at 1000
bucket-next-cli -config my.yaml init order long 1000
# OK initialised "order" as long; first value will be 1000

# On a multi-shard config, the start value snaps to the shard residue:
#   snapped start-value 1000 -> 1001 (shard 2 residue)

# Move an existing counter
bucket-next-cli -config my.yaml reset order 5000
# OK "order" reset: iteration 0 -> 1666; next value will be 5000

# Show one entity
bucket-next-cli -config my.yaml status order

# System info
bucket-next-cli -config my.yaml info

# Wipe everything (requires --force)
bucket-next-cli -config my.yaml clear --force
```

## When to reach for the CLI vs the HTTP API

| Task | Tool |
|---|---|
| Mint IDs in production | HTTP `/api/next-id`, `/api/next-batch` |
| Seed an entity before launch | either `init` (CLI offline, or HTTP `POST /api/init`) |
| Bulk-edit many entities during migration | CLI (service stopped) |
| Recover when the service won't start | CLI |
| Inspect live state | HTTP `/api/list`, `/api/status` (no downtime) |

## Notes

- The CLI prints the shard id it is using before every mutation, so pointing it at the wrong
  shard's config is visible in the output.
- It does not mint IDs — generation belongs to the service. The CLI only edits the counters
  the service will use next.
- It operates only on its own shard's state file; it does not contact peers.
