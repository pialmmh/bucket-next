# Configuration reference

The service reads exactly one YAML file, passed with `-config <path>`. There is no
environment-variable surface — every knob lives in the file.

```bash
./bin/bucket-next -config /path/to/config.yaml
# positional form also works:
./bin/bucket-next /path/to/config.yaml
```

## Required keys

| Key | Type | Description |
|---|---|---|
| `shard_id` | int | This node's shard, in `[1, total_shards]`. Unique within the cluster. |
| `total_shards` | int | Cluster size, in `[1, 1024]`. Identical on every node. |
| `state_path` | string | Path to the JSON state file. Absolute or relative; parent dir auto-created. |

## Optional keys (defaults shown)

| Key | Default | Description |
|---|---|---|
| `listen_port` | `7001` | HTTP port to bind. |
| `segment_size` | `1000` | Numeric IDs reserved per disk write. Higher = fewer writes, larger crash gap. |
| `segment_refill_watermark` | `0.9` | Fraction of a segment consumed before the next is fetched in the background. Must be in `(0, 1)`. |
| `clock_drift_tolerance_ms` | `0` | Max backward wall-clock jump the Snowflake forge waits out before erroring. |
| `snowflake_epoch_ms` | `1704067200000` | Unix-millis epoch for Snowflake timestamps. Default = 2024-01-01 UTC. Must match across the cluster. |

## Full example

```yaml
shard_id: 2
total_shards: 3
listen_port: 7001
state_path: /var/lib/bucket-next/state.json

segment_size: 1000
segment_refill_watermark: 0.9
clock_drift_tolerance_ms: 0
snowflake_epoch_ms: 1704067200000
```

## Validation — the service refuses to start if

- The config file is missing or unparseable.
- `shard_id` is outside `[1, total_shards]`.
- `total_shards` is outside `[1, 1024]`.
- `listen_port` is outside `[1, 65535]`.
- `state_path` is empty.
- `segment_size` is less than 1.
- `segment_refill_watermark` is not strictly between 0 and 1.

On any of these it prints the reason to stderr and exits non-zero — it never half-starts.

## Notes on specific keys

### `total_shards` and re-sharding

`total_shards` defines the residue partition for numeric IDs: shard *k* emits
`k, k+N, k+2N, …`. Changing `total_shards` after IDs have been issued **breaks the
guarantee** — previously-issued values keep their residue under the old N, not the new one.
Re-sharding is a manual migration, not a config edit. Pick the cluster size up front, with
headroom.

### `segment_size` trade-off

- Larger → fewer disk writes per ID issued → higher throughput, but a hard crash forfeits up
  to `segment_size` IDs (gap, never duplicate).
- Smaller → more disk writes, tighter crash gap.
- For call/SMS burst workloads the default 1000 is a good balance. For low-volume entities
  where gaps look ugly, drop it to e.g. 10.

### `snowflake_epoch_ms` must be uniform

Every node decodes Snowflake timestamps relative to this epoch. If two nodes disagree,
`/api/parse-snowflake` returns mismatched timestamps. Set it once and keep it identical
across the whole cluster.

## Multi-shard configs

When deploying more than one shard, each shard gets its **own** config file with its own
`shard_id` but the **same** `total_shards`. The deploy tooling generates these per-shard
files for you — see [deployment.md](deployment.md). You do not hand-write N config files.
