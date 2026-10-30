# bucket-next documentation

`bucket-next` is a sharded, multi-tenant unique-ID HTTP service. One static Go binary per installation mints IDs of
several shapes (`int`, `long`, `snowflake`, `uuid8/12/16/22`) for every tenant, keyed by (tenant, entity), and splits
the ID space across N nodes with **no inter-node coordination**.

Repository: `git@github.com:pialmmh/bucket-next.git`

## Read in this order

| Doc | What it covers |
|---|---|
| [getting-started.md](getting-started.md) | Clone, build, first start (`-init`), first ID. |
| [usage-examples.md](usage-examples.md) | Recipes per data type, scalar vs vector, reset, language snippets. |
| [api-reference.md](api-reference.md) | The tenant-first roads `/api/v2/{tenant}/…`, the two global probes, the error words, the pre-tenant roads. |
| [tenancy.md](tenancy.md) | Tenants: the model, the state on disk, the carry-over of a pre-tenant state, the bed's switch-over, the route back, the callers. |
| [configuration.md](configuration.md) | Every YAML key, defaults, validation, the state-file identity check. |
| [cli.md](cli.md) | `bucket-next-cli` for offline state maintenance. |
| [deployment.md](deployment.md) | Multi-node deploy by the routesphere convention: config per tenant and profile, the tenant conf, the shared SSH inventory, `tools/deploy/remote-deploy-v2.sh`. |

## 30-second mental model

- **Tenant** — tenant-master's tenant id (`btcl`). One service per installation serves every tenant; every call names
  its tenant first (`/api/v2/btcl/next-id/order`); a call without one, or for one not declared, is refused in words.
- **Entity** — a named ID stream within a tenant (`order`, `cdr`, `sms`, `session`). You pick the name; it
  registers itself on first use, bound to one data type for life.
- **Data type** — `int`/`long` are sequential numbers, `snowflake` is time-ordered, `uuid*`
  are fixed-length strings that look random.
- **Scalar and vector** — every type has two APIs: `next-id` returns **one** ID,
  `next-batch` returns a **range** of up to 10 000 IDs in one call.
- **Shard** — one running process with its own `shard_id` out of `total_shards`. Shard *k*
  of *N* owns the values `k, k+N, k+2N, …`, so shards never collide without talking.
- **State** — one directory per shard: `shard.json` + one file per tenant. Every file records which shard owns it; a
  wrong file, a changed `shard_id` or `total_shards`, or a missing file stops the service from starting.

## The uniqueness guarantee

| Scope | Guarantee |
|---|---|
| Same tenant + entity, same shard | Never repeats — including under heavy concurrency and after a crash or power loss. |
| Same tenant + entity, different shards | Never repeats, as long as each shard has its own `shard_id` and they share `total_shards`, `uuid_key` and `snowflake_epoch_ms`. |
| Different entities or tenants | `int`/`long` streams overlap on purpose (each counter counts on its own). `uuid*` streams use a different scramble per (tenant, entity), so they are unrelated; cross-entity uniqueness is statistical, not guaranteed. `snowflake` is unique across all entities and tenants of the installation. |

## The one thing that surprises people

For counter types (`int`, `long`, `uuid*`) the service reserves a **block** of IDs on disk at
once (default 1000) and serves them from RAM. After you take 10 IDs, the tenant's state file may
already say 1000 — that is the reservation, not a leak. A crash forfeits the unused rest of
the block (a gap of at most `segment_size`); it never hands out the same ID twice. See
[usage-examples.md](usage-examples.md#gaps-and-crash-behaviour).
