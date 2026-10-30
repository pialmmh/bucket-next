# Configuration reference

The service reads exactly one YAML file, passed with `-config <path>`. There is no
environment-variable surface — every knob lives in the file.

```bash
./bin/bucket-next -config /path/to/config.yaml          # normal start
./bin/bucket-next -config /path/to/config.yaml -init    # first start of a brand-new shard
```

## Required keys

| Key | Type | Description |
|---|---|---|
| `shard_id` | int | This node's shard, in `[1, total_shards]`. Unique within the cluster. |
| `total_shards` | int | Cluster size, in `[1, 1024]`. Identical on every node. |
| `state_dir` | string | This shard's own state directory: `shard.json` + `tenants/<tenant>.json`. Created by `-init`. (The pre-tenant `state_path` is refused, naming `state_dir`.) |
| `tenants` | list | The tenants this installation serves, e.g. `[btcl]` — tenant-master's tenant ids (`^[a-z][a-z0-9]{1,15}$`), no repeats. Identical on every node. A call never creates a tenant. |

## Optional keys (defaults shown)

| Key | Default | Description |
|---|---|---|
| `legacy_tenant` | none | One of `tenants`. The pre-tenant roads (`/api/next-id/{entity}` …, no tenant) act on it, and a pre-tenant `state.json` found in `state_dir` is carried over into it. Unset = those roads answer `400 Tenant is required`, and a pre-tenant `state.json` is refused. See [tenancy.md](tenancy.md). |
| `listen_address` | `""` | IP to bind. Empty = all interfaces. Use `127.0.0.1` on a dev box and the host's private IP on a server. Must be an IP literal (IPv4 or IPv6); hostnames are refused. |
| `listen_port` | `7001` | HTTP port to bind. |
| `segment_size` | `1000` | Counter IDs reserved per durable disk write. Bigger = fewer writes, bigger gap after a crash. |
| `segment_refill_watermark` | `0.5` | Fraction of a reservation used before the next one is fetched in the background. Must be in `(0, 1)`. |
| `clock_drift_tolerance_ms` | `0` | Extra backward wall-clock jump the Snowflake forge waits out before erroring. |
| `snowflake_epoch_ms` | `1704067200000` | Unix-millis epoch for Snowflake timestamps (2024-01-01 UTC). Identical on every node; never change. |
| `uuid_key` | *(built-in)* | Hex AES key — 32, 48 or 64 hex chars — that scrambles `uuid*` IDs. Identical on every node; never change. |

## Full example

```yaml
shard_id: 2
total_shards: 3
listen_address: 10.10.199.12
listen_port: 7001
state_dir: /var/lib/bucket-next/shard-2
tenants: [btcl, acme]
legacy_tenant: btcl      # only while a caller still uses the pre-tenant roads

segment_size: 1000
segment_refill_watermark: 0.5
clock_drift_tolerance_ms: 0
snowflake_epoch_ms: 1704067200000
uuid_key: 0f1e2d3c4b5a69788796a5b4c3d2e1f0
```

## Startup checks — the service refuses to start if

Config:
- the file is missing or unparseable;
- `shard_id` is outside `[1, total_shards]`, or `total_shards` outside `[1, 1024]`;
- `listen_port` is outside `[1, 65535]`, `state_dir` is empty, or `state_path` is set (it is replaced by `state_dir`);
- `tenants` is missing or empty, a tenant breaks `^[a-z][a-z0-9]{1,15}$`, a tenant is listed twice, or `legacy_tenant`
  is not one of `tenants`;
- `listen_address` is set but is not an IP address;
- `segment_size < 1`, or `segment_refill_watermark` is not strictly between 0 and 1;
- `uuid_key` is set but is not 32/48/64 hex chars.

State:
- **`state_dir` holds no state** and `-init` was not given (starting from zero would re-issue IDs);
- a file is empty or unparseable;
- **a file belongs to another identity** — the `shard_id`, `total_shards`, uuid key fingerprint or
  `snowflake_epoch_ms` recorded in `shard.json` or a tenant file differ from the config. Example:

  ```
  state: state file /var/lib/bucket-next/shard-1/shard.json does not belong to this config (total_shards: file=2 config=3);
  using it would issue duplicate IDs — point state_dir at this shard's own directory, or fix the config
  ```
- a tenant file holds another tenant (renamed or copied);
- a tenant on the shard's roster has **lost its file** (restore it from backup);
- a tenant not on the roster has a file with entities (`bucket-next-cli adopt-tenant <t>` after `shard.json` was
  restored from an older backup);
- `shard.json` is missing while tenant files exist (restore `shard.json`);
- a pre-tenant `state.json` is present and no tenant owns it (`legacy_tenant`).

Clock:
- the clock is more than `1000 + clock_drift_tolerance_ms` ms behind the last Snowflake mark
  saved by the previous run. Within that window the service just waits (up to ~1 s).

On any of these it prints the reason and exits non-zero — it never half-starts.

## The state binds a shard's identity

```
<state_dir>/shard.json          identity · Snowflake mark · the tenant roster · the last carry-over
<state_dir>/tenants/<t>.json    tenant t · the same identity · its entities' marks (and each uuid entity's scramble)
<state_dir>/.lock               held by the running service
```

On creation every file records `shardId`, `totalShards`, `uuidKeyFingerprint` and `snowflakeEpochMs`. These four must
never change for the life of the shard, because every ID already issued depends on them. To change any of them you need a
planned migration (new state and new entity names), not a config edit.

A new tenant needs no step of its own: a tenant declared in `tenants` and not on the shard's roster gets its file at the
next start, then joins the roster. A tenant on the roster whose file is missing is refused (lost), never begun afresh.

A pre-tenant state file (`state_dir/state.json`, format 1 or 2) is carried over at the first start into
`legacy_tenant`, every mark kept, then renamed `state.json.migrated-v<N>`: [tenancy.md](tenancy.md).

## Notes on specific keys

### `listen_address` — keep the API private

The API has no authentication: anyone who reaches the port can mint IDs, register entities and
reset counters (`force` included). Bind it where only your services can reach it:

| Where | `listen_address` | Bound socket |
|---|---|---|
| dev box | `127.0.0.1` | `127.0.0.1:7001` |
| server | its private IP, e.g. `10.10.199.12` | `10.10.199.12:7001` |
| (default) | `""` | every interface, `*:7001` |

IPv6 works too (`::1` binds `[::1]:7001`). Firewall the port as well.

### `total_shards` — pick it once, with headroom

Shard *k* emits `k, k+N, k+2N, …`. Values issued under one *N* keep their residue, so changing
*N* later would let shards collide. The service refuses a state file created under a different
`total_shards`. Unused shard numbers cost nothing, so leave room to grow.

### `segment_size` and `segment_refill_watermark` — throughput vs gap

Every reservation is an fsynced disk write (durable against power loss). It takes from well
under 1 ms on a server SSD to ~25 ms on a slow or full disk.

- Throughput per entity is roughly `segment_size / disk-write-time`. With a 25 ms disk and
  `segment_size: 1000`, that is ~40 000 IDs/s per entity; `10000` gives ~400 000/s.
- The refill starts when `(1 − watermark) × segment_size` IDs are left. At `0.5` there is half
  a block of headroom to hide the disk write.
- A hard crash forfeits at most one reservation's unused tail (≤ `segment_size` IDs) — a gap,
  never a duplicate.

For call/SMS burst traffic use `segment_size: 10000`. For a low-volume entity where gaps look
untidy, use a small value such as 10.

### `uuid_key` — set before the first uuid ID

uuid IDs are a shard-interleaved counter scrambled by a keyed permutation. Uniqueness comes
from the counter; the key only makes them unpredictable. Without `uuid_key` a built-in key
is used and the service logs a warning: IDs are still unique, but anyone with the source can
predict them. Generate a key with `openssl rand -hex 16` and use the **same** key on every
shard. Changing it later is refused at startup (fingerprint mismatch).

## Multi-shard configs

Each shard gets its **own** config with its own `shard_id` and `state_dir`, and the **same**
`total_shards`, `tenants`, `uuid_key` and `snowflake_epoch_ms`. The deploy tooling renders these per-shard
files for you — see [deployment.md](deployment.md).
