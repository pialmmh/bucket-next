# bucket-next

A sharded, multi-type unique-ID service. Single Go binary, one YAML config, segment-cached for high-TPS bursts.

See the design notes in Trilium (`ID-Land` and its children) for the full feature story. This file is the quick-start.

## Documentation

Full guides live in [`docs/`](docs/) — followable cold by a human or another agent:

- [docs/getting-started.md](docs/getting-started.md) — clone, build, run, first ID
- [docs/usage-examples.md](docs/usage-examples.md) — recipes per data type, batches, language snippets
- [docs/api-reference.md](docs/api-reference.md) — all 11 HTTP endpoints
- [docs/configuration.md](docs/configuration.md) — every config key
- [docs/cli.md](docs/cli.md) — `bucket-next-cli`
- [docs/deployment.md](docs/deployment.md) — multi-node deploy with `deploy.sh`

## Build

```
make build
```

Produces:
- `bin/bucket-next` — the HTTP service
- `bin/bucket-next-cli` — offline maintenance tool

## Run

```
cp configs/sample.yaml configs/dev.yaml
# edit shard_id / total_shards / listen_port / state_path
bin/bucket-next -config configs/dev.yaml
```

## Quick API smoke test

```
curl http://localhost:7001/health
curl "http://localhost:7001/api/next-id/cdr?dataType=long"
curl "http://localhost:7001/api/next-batch/sms?dataType=long&batchSize=100"
curl "http://localhost:7001/api/next-id/sub?dataType=snowflake"
```

## CLI

```
bin/bucket-next-cli -config configs/dev.yaml list
bin/bucket-next-cli -config configs/dev.yaml init cdr long 1000
bin/bucket-next-cli -config configs/dev.yaml reset cdr 5000
bin/bucket-next-cli -config configs/dev.yaml info
```

Stop the service before running CLI mutations — both processes write the same state file.

## Layout

```
cmd/
  bucket-next/         main service entry
  bucket-next-cli/     offline maintenance CLI
internal/
  config/              YAML loader + validation
  datatype/            int/long/snowflake/uuid* type tags
  state/               JSON state store, atomic write
  forge/               Snowflake (41 ts | 10 shard | 12 seq)
  shortid/             random uuid8/12/16/22
  allocator/           segment-cached interleaved counters
  server/              HTTP handlers
configs/
  sample.yaml          all config keys with defaults
```

## Data type behaviour

| Type | Wire form | Stateful | Shard-aware |
|---|---|---|---|
| `int` | JSON number | yes (segments on disk) | yes (interleaved `k, k+N, …`) |
| `long` | decimal string | yes | yes (interleaved) |
| `snowflake` | decimal string | no | yes (10 bits in payload) |
| `uuid8`–`uuid22` | string | no | shard-flavored entropy |

## Segment caching

Numeric IDs serve from an in-RAM segment of `segment_size` IDs (default 1000). Disk is touched once per segment, not once per ID. When `segment_refill_watermark` (default 0.9) of the segment has been handed out, a background goroutine reserves the next segment. The next segment swap is seamless — no Caller waits.

On crash, the tail of the in-flight segment is forfeited. Maximum gap equals `segment_size`. Duplicate IDs are impossible by construction.

## Dependencies

- `gopkg.in/yaml.v3` — config parsing.

Everything else is stdlib. Build is a single binary per platform.
