# bucket-next documentation

`bucket-next` is a sharded unique-ID HTTP service. One static Go binary mints IDs of
several shapes (`int`, `long`, `snowflake`, `uuid8/12/16/22`) keyed by entity name, and
partitions the numeric space across N nodes with **no inter-node coordination**.

Repository: `git@github.com:pialmmh/bucket-next.git`

## Read in this order

| Doc | What it covers |
|---|---|
| [getting-started.md](getting-started.md) | Clone, build, run, get your first ID in under a minute. |
| [usage-examples.md](usage-examples.md) | Copy-paste recipes for every data type, batches, init/reset, language snippets. |
| [api-reference.md](api-reference.md) | All 11 HTTP endpoints — params, example responses, errors. |
| [configuration.md](configuration.md) | Every YAML config key, defaults, validation rules. |
| [cli.md](cli.md) | `bucket-next-cli` for offline state maintenance. |
| [deployment.md](deployment.md) | Multi-node deploy: tenants, profiles, SSH inventory, `deploy.sh`. |

## 30-second mental model

- **Entity** — a named counter or ID stream (`order`, `cdr`, `sms`, `user-token`). You pick the name; it auto-registers on first use.
- **Data type** — chosen per entity on first use, then fixed for life. `int`/`long` are sequential; `snowflake` is time-ordered; `uuid*` is random.
- **Shard** — one running process, identified by `shard_id` of `total_shards`. For a single box, use `total_shards: 1` and you get plain `1, 2, 3, …`.
- **State** — one JSON file on disk per shard. No database.

## The one thing that surprises people

For `int`/`long`, the service reserves a **block** of IDs on disk at once (default 1000) and
serves them from RAM. So after you hand out 10 IDs, the on-disk counter may read 1000 — that
is the reservation, not a bug. A hard crash forfeits the unused tail of the current block
(max gap = `segment_size`); it never hands out a duplicate. See
[usage-examples.md](usage-examples.md#gaps-and-crash-behaviour).
