# bucket-next

A sharded, multi-type unique-ID service. Single Go binary, one YAML config, segment-cached for
high-TPS bursts, no coordination between shards.

- **Types, per entity:** `int`, `long`, `snowflake`, `uuid8`, `uuid12`, `uuid16`, `uuid22`.
- **Two API versions for every type:** *scalar* (`next-id` — one ID) and *vector*
  (`next-batch` — a range of up to 10 000 IDs).
- **Never repeats within an entity** — across shards, under concurrency, after a crash or a
  power loss.
- **Shard identity is enforced:** every process needs a `shard_id`, and its state file is bound
  to it.
- **Binds where you tell it:** `listen_address` keeps the unauthenticated API on loopback or a
  private IP.

Repository: `git@github.com:pialmmh/bucket-next.git` · Go 1.21+ · only dependency `gopkg.in/yaml.v3`

## Contents

- [Quick start](#quick-start)
- [Data types](#data-types)
- [API reference](#api-reference)
- [Examples](#examples)
- [Configuration](#configuration)
- [CLI](#cli)
- [Deployment](#deployment)
- [How uniqueness is guaranteed](#how-uniqueness-is-guaranteed)
- [Project layout and testing](#project-layout-and-testing)
- [More documentation](#more-documentation)

---

## Quick start

```bash
git clone git@github.com:pialmmh/bucket-next.git && cd bucket-next
make build                                   # bin/bucket-next + bin/bucket-next-cli

cat > my.yaml <<'EOF'
shard_id: 1
total_shards: 1
listen_address: 127.0.0.1                    # dev box: loopback only
listen_port: 7001
state_path: ./data/state.json
EOF

./bin/bucket-next -config my.yaml -init      # -init ONLY on the very first start
```

```bash
curl "http://localhost:7001/api/next-id/order?dataType=int"                  # one ID
curl "http://localhost:7001/api/next-batch/sms?dataType=uuid12&batchSize=100" # 100 IDs
```

`-init` creates the state file of a brand-new shard. Every later start runs without it; if the
state file is ever missing the service refuses to start rather than restart from zero.

---

## Data types

An entity is bound to one type on its first call, for life.

| Type | Wire form | Capacity per entity (whole cluster) | Example |
|---|---|---|---|
| `int` | JSON number | up to 2 147 483 647 | `42` |
| `long` | decimal **string** | up to 9.2 × 10^18 | `"1000004"` |
| `snowflake` | decimal string | ~69 years, 4096 per ms per shard | `"315240570203148288"` |
| `uuid8` | 8 chars `[0-9a-zA-Z]` | 218 trillion | `"0CIOE46z"` |
| `uuid12` / `uuid16` / `uuid22` | 12 / 16 / 22 chars | 9.2 × 10^18 | `"oKs4845oCMOV3Qhm"` |

- `long` is a string because JSON numbers lose precision above 2^53.
- `uuid*` are **not random**: each is the entity's counter passed through a keyed permutation.
  They are fixed-length, look random, and never repeat within an entity.

---

## API reference

Base URL `http://<listen_address>:<listen_port>` (default port `7001`). JSON everywhere. **No
authentication** — bind it to loopback or a private IP with `listen_address` (anyone who can reach
the port can mint IDs and reset counters). Examples are real responses from shard 2 of 3.

### Endpoints

| Method | Path | Purpose |
|---|---|---|
| GET | [`/api/next-id/{entity}`](#get-apinext-identity--scalar) | **Scalar** — mint one ID |
| GET | [`/api/next-batch/{entity}`](#get-apinext-batchentity--vector) | **Vector** — mint up to 10 000 IDs |
| POST | [`/api/init/{entity}`](#post-apiinitentity) | Register an entity, optional start value |
| PUT | [`/api/reset/{entity}`](#put-apiresetentity) | Move an `int`/`long` counter |
| GET | [`/api/status/{entity}`](#get-apistatusentity) | One entity's snapshot |
| GET | [`/api/list`](#get-apilist) | Every entity on this shard |
| GET | [`/api/segment-state/{entity}`](#get-apisegment-stateentity) | RAM reservation of a counter entity |
| GET | [`/api/parse-snowflake/{id}`](#get-apiparse-snowflakeid) | Decode a Snowflake ID |
| GET | [`/health`](#get-health) | Liveness + Snowflake stats |
| GET | [`/shard-info`](#get-shard-info) | Shard identity + cluster-wide values |
| GET | [`/api/types`](#get-apitypes) | Types and API versions |

### Errors

Every error body is `{"error": "<short code>", "message": "<detail>"}`.

| Status | When | `error` |
|---|---|---|
| 400 | unknown/missing `dataType` | `Valid dataType is required` |
| 400 | `batchSize` missing or outside 1..10000 | `Valid batchSize is required` |
| 400 | entity bound to another type (body adds `registeredType`) | `Type mismatch` |
| 400 | start/reset value `< 1`, too large, or not allowed for the type | `Invalid startValue` / `Invalid value` |
| 404 | entity not registered (`status`, `reset`, `segment-state`) | `Entity not found` |
| 409 | the type has no values left for this entity on this shard | `Range exhausted` |
| 409 | `init` on an existing entity | `Entity already exists` |
| 409 | `reset` would move backward without `force` | `Backward reset refused` |
| 500 | server fault, e.g. the state file could not be written | `Generation failed` / `Failed to …` |

---

### GET /api/next-id/{entity} — scalar

Mint **one** ID. The entity registers itself on its first call.

| Param | In | Notes |
|---|---|---|
| `entity` | path | any non-empty name |
| `dataType` | query | required — one of the 7 types |

```bash
curl "http://localhost:7001/api/next-id/order?dataType=int"
```
```json
{"dataType":"int","entityName":"order","shard":2,"value":2}
```

```bash
curl "http://localhost:7001/api/next-id/session?dataType=uuid16"
```
```json
{"dataType":"uuid16","entityName":"session","shard":2,"value":"oKs4845oCMOV3Qhm"}
```

Type mismatch:
```json
{"error":"Type mismatch","message":"Entity 'order' is registered as 'int', cannot use 'long'","registeredType":"int"}
```

Exhausted (e.g. an `int` entity past 2 147 483 647):
```json
{"error":"Range exhausted","message":"Entity 'x' has no int values left on this shard (max 2147483647)"}
```

---

### GET /api/next-batch/{entity} — vector

Mint a **range** of IDs in one round trip — the high-TPS path. It uses the same counter as
`next-id`, so the two never overlap.

| Param | In | Notes |
|---|---|---|
| `entity` | path | any non-empty name |
| `dataType` | query | required |
| `batchSize` | query | required, `1..10000` |

```bash
curl "http://localhost:7001/api/next-batch/cdr?dataType=long&batchSize=4"
```
```json
{"batchSize":4,"dataType":"long","entityName":"cdr","shard":2,
 "startValue":"2","endValue":"11","values":["2","5","8","11"],
 "range":{"first":"2","last":"11","step":3,"count":4}}
```

For `int` and `long` the batch is one contiguous slice of this shard's sequence, and `range`
describes it fully: value *i* = `first + i × step` (`step` = `total_shards`).

```bash
curl "http://localhost:7001/api/next-batch/token?dataType=uuid8&batchSize=3"
```
```json
{"batchSize":3,"dataType":"uuid8","entityName":"token","shard":2,
 "startValue":"0CIOE46z","endValue":"LrdDXHX4","values":["0CIOE46z","gvV6EFAx","LrdDXHX4"]}
```

`snowflake` and `uuid*` batches have `values` but no `range`. On `409` nothing is handed out —
there are no partial batches.

---

### POST /api/init/{entity}

Register an entity before traffic, optionally choosing where an `int`/`long` counter starts.

| Body field | Notes |
|---|---|
| `dataType` | required |
| `startValue` | optional; `int`/`long` only; `1..max`; snapped **forward** to this shard |

```bash
curl -X POST http://localhost:7001/api/init/invoice \
  -H 'Content-Type: application/json' \
  -d '{"dataType":"long","startValue":1000}'
```
```json
{"entityName":"invoice","dataType":"long","shard":2,"initialized":true,
 "message":"Entity 'invoice' initialized successfully with type 'long'",
 "requestedStartValue":1000,"actualStartValue":"1001",
 "adjusted":true,"adjustmentReason":"Value adjusted to match shard 2 pattern",
 "nextValue":"1001","pattern":"2, 5, 8, ..."}
```

`1000 mod 3 = 1`, but shard 2 owns residue 2, so the start moves up to `1001`.

Errors: `409 Entity already exists`; `400 Invalid startValue`:
```json
{"error":"Invalid startValue","message":"startValue must be between 1 and 9223372036854775807 for type long"}
```

---

### PUT /api/reset/{entity}

Move an `int`/`long` counter. The value is snapped forward to this shard. Moving **backward**
— below IDs that may already be in use — is refused unless `"force": true`.

| Body field | Notes |
|---|---|
| `value` | required, `1..max` |
| `force` | optional, default `false` — allow a backward move |

```bash
curl -X PUT http://localhost:7001/api/reset/order \
  -H 'Content-Type: application/json' -d '{"value":5000}'
```
```json
{"entityName":"order","dataType":"int","shard":2,
 "reset":{"previousNextValue":14,"requestedValue":5000,"actualValue":5000,
          "previousIteration":4,"newIteration":1666,"forced":false},
 "nextValue":5000,"message":"Counter reset successfully. Next value will be 5000"}
```

Backward without `force` → `409`:
```json
{"error":"Backward reset refused","message":"Next value is 14; moving to 2 could re-issue IDs that are already in use. Send \"force\": true to do it anyway.","nextValue":14}
```

With `{"value":2,"force":true}` it succeeds and reports `"forced":true`.
Errors: `404` unknown entity, `400` not `int`/`long` or bad value, `409` as above.

---

### GET /api/status/{entity}

```bash
curl http://localhost:7001/api/status/cdr
```
```json
{"entityName":"cdr","dataType":"long","currentIteration":1000,"currentValue":"11","nextValue":"14","shard":2}
```

| Field | Meaning |
|---|---|
| `currentValue` | last value this process handed out (`null` if none yet) |
| `nextValue` | value it will hand out next |
| `currentIteration` | durable mark in the state file (runs ahead because of reservations) |

For `uuid*` both values are `null` (opaque strings). For `snowflake`, `currentValue` is
`"N/A (Snowflake IDs are not sequential)"`. `404` if unknown.

---

### GET /api/list

Every entity on this shard, each in the `status` shape:

```json
{"entities":[{"entityName":"cdr","dataType":"long","currentIteration":1000,"currentValue":"11","nextValue":"14","shard":2}],
 "shardInfo":{"shardId":2,"totalShards":3}}
```

Per shard only — for a cluster view, call each shard and merge.

---

### GET /api/segment-state/{entity}

Diagnostic view of a counter entity's RAM reservation (`int`, `long`, `uuid*`).

```bash
curl http://localhost:7001/api/segment-state/cdr
```
```json
{"entityName":"cdr","dataType":"long","shard":2,
 "segment":{"size":1000,"watermark":0.5,"loaded":true,"issued":4,"remaining":996,
            "refillInFlight":false,"nextIteration":4,"reservedEnd":1000,"diskMark":1000,
            "nextValue":"14","diskHighWaterMark":"3002"}}
```

| Field | Meaning |
|---|---|
| `issued` | IDs handed out by this process since it loaded the entity |
| `remaining` | reserved in RAM, not yet handed out |
| `refillInFlight` | the next block is being written to disk |
| `nextIteration`, `reservedEnd` | the window `[next, end)` this process owns |
| `diskMark` | durable mark — always above every ID handed out |

`404` unknown entity, `400` for `snowflake` (no reservations).

---

### GET /api/parse-snowflake/{id}

Decode a Snowflake minted by any shard:

```json
{"id":"315240570203148288","timestamp":1779226511668,"date":"2026-05-19T21:35:11.668Z",
 "shardId":2,"sequence":0,"binary":"0000010001011111..."}
```

`400` if the id is not a valid 64-bit decimal.

### GET /health

```json
{"status":"healthy","uptime":12345.67,"shard":2,"totalShards":3,
 "snowflakeStats":{"generated":184213,"collisions":4,"waits":0,"shardId":2,"totalShards":3,"maxIdsPerMs":4096,"maxShards":1024}}
```

### GET /shard-info

Identity plus the values that **must be identical on every shard** — compare them across
nodes after a deploy:

```json
{"shardId":2,"totalShards":3,"status":"active","uuidKeyFingerprint":"4179529caf32c8cc","snowflakeEpochMs":1704067200000}
```

### GET /api/types

```json
{"availableTypes":["int","long","snowflake","uuid8","uuid12","uuid16","uuid22"],
 "apiVersions":{"scalar":"GET /api/next-id/{entity}?dataType=T — one ID",
                "vector":"GET /api/next-batch/{entity}?dataType=T&batchSize=N — N IDs (1..10000)"},
 "description":{"int":"Sequential 32-bit integer (shard 2: 2, 5, 8, ...)", "...":"..."},
 "shardInfo":{"shardId":2,"totalShards":3},
 "snowflakeInfo":{"epoch":"2024-01-01T00:00:00Z","maxIdsPerMs":4096,"maxShards":1024,
                  "bitsAllocation":{"timestamp":41,"shardId":10,"sequence":12,"total":64}}}
```

---

## Examples

All examples use a single shard on `localhost:7001`.

### One ID per type (scalar)

```bash
curl "localhost:7001/api/next-id/order?dataType=int"         # {"value":1, ...}
curl "localhost:7001/api/next-id/cdr?dataType=long"          # {"value":"1", ...}
curl "localhost:7001/api/next-id/event?dataType=snowflake"   # {"value":"315240570203148288", ...}
curl "localhost:7001/api/next-id/call?dataType=uuid8"        # {"value":"0CIOE46z", ...}
curl "localhost:7001/api/next-id/session?dataType=uuid22"    # {"value":"<22 chars>", ...}
```

### A range per type (vector)

```bash
curl "localhost:7001/api/next-batch/order?dataType=int&batchSize=1000"
curl "localhost:7001/api/next-batch/sms-id?dataType=uuid12&batchSize=10000"
curl "localhost:7001/api/next-batch/event?dataType=snowflake&batchSize=500"
```

### Scalar and vector share one sequence

```bash
curl "localhost:7001/api/next-id/x?dataType=int"                  # 1
curl "localhost:7001/api/next-batch/x?dataType=int&batchSize=3"   # 2, 3, 4
curl "localhost:7001/api/next-id/x?dataType=int"                  # 5
```

### Keep only the range of a numeric batch

```bash
curl -s "localhost:7001/api/next-batch/cdr?dataType=long&batchSize=5000" | jq .range
# {"first":"1","last":"5000","step":1,"count":5000}   value i = first + i*step
```

### Seed, move forward, move back on purpose

**Counters are per shard.** `init`, `reset` and `status` act only on the shard you call. To put an
entity's counter above IDs that already exist, do it on **every** shard that mints for that entity
(each one snaps the value to its own residue).

```bash
curl -X POST localhost:7001/api/init/invoice -H 'Content-Type: application/json' \
     -d '{"dataType":"long","startValue":1000}'                        # next = 1000
curl -X PUT  localhost:7001/api/reset/invoice -H 'Content-Type: application/json' \
     -d '{"value":5000}'                                               # forward: OK
curl -X PUT  localhost:7001/api/reset/invoice -H 'Content-Type: application/json' \
     -d '{"value":10}'                                                 # backward: 409
curl -X PUT  localhost:7001/api/reset/invoice -H 'Content-Type: application/json' \
     -d '{"value":10,"force":true}'                                    # backward, on purpose
```

### Multi-shard numbering

Each shard owns one residue class — no coordination, no collisions:

| Shard | `int` / `long` values |
|---|---|
| 1 of 3 | 1, 4, 7, 10, … |
| 2 of 3 | 2, 5, 8, 11, … |
| 3 of 3 | 3, 6, 9, 12, … |

A client may call any shard (round-robin, nearest, …); every ID stays unique for its entity
across the cluster. `uuid*` follow the same rule; `snowflake` carries the shard in its bits.

### Client snippets

**bash + jq**
```bash
next_id()  { curl -s "http://localhost:7001/api/next-id/$1?dataType=$2" | jq -r .value; }
next_ids() { curl -s "http://localhost:7001/api/next-batch/$1?dataType=$2&batchSize=$3" | jq -r '.values[]'; }

order_id=$(next_id order long)
next_ids sms-id uuid12 1000 > ids.txt
```

**Java (JDK 21, Jackson)**
```java
import com.fasterxml.jackson.databind.DeserializationFeature;
import com.fasterxml.jackson.databind.ObjectMapper;
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.util.List;

public final class BucketNextClient {
    record One(String entityName, String dataType, String value) {}          // int arrives as a number; Jackson coerces it
    record Many(String entityName, String dataType, List<String> values) {}

    private static final ObjectMapper JSON = new ObjectMapper()
            .configure(DeserializationFeature.FAIL_ON_UNKNOWN_PROPERTIES, false);
    private final HttpClient http = HttpClient.newHttpClient();
    private final String base;

    public BucketNextClient(String base) { this.base = base; }

    public String nextId(String entity, String dataType) throws Exception {
        return get("/api/next-id/" + entity + "?dataType=" + dataType, One.class).value();
    }

    public List<String> nextIds(String entity, String dataType, int n) throws Exception {
        return get("/api/next-batch/" + entity + "?dataType=" + dataType + "&batchSize=" + n, Many.class).values();
    }

    private <T> T get(String path, Class<T> type) throws Exception {
        HttpResponse<String> r = http.send(HttpRequest.newBuilder(URI.create(base + path)).GET().build(),
                HttpResponse.BodyHandlers.ofString());
        if (r.statusCode() != 200) throw new IllegalStateException("bucket-next " + r.statusCode() + ": " + r.body());
        return JSON.readValue(r.body(), type);
    }
}
// new BucketNextClient("http://10.10.199.11:7001").nextIds("sms-id", "uuid12", 5000);
```

**Python**
```python
import requests

BASE = "http://localhost:7001"

def next_id(entity, dtype):
    r = requests.get(f"{BASE}/api/next-id/{entity}", params={"dataType": dtype})
    r.raise_for_status()
    return r.json()["value"]      # int for "int"; str for long / snowflake / uuid*

def next_ids(entity, dtype, n):
    r = requests.get(f"{BASE}/api/next-batch/{entity}", params={"dataType": dtype, "batchSize": n})
    r.raise_for_status()
    return r.json()["values"]

order_id = next_id("order", "long")
sms_ids = next_ids("sms-id", "uuid12", 5000)
```

**Go**
```go
func nextIDs(base, entity, dtype string, n int) ([]any, error) {
	u := fmt.Sprintf("%s/api/next-batch/%s?dataType=%s&batchSize=%d", base, entity, url.QueryEscape(dtype), n)
	resp, err := http.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bucket-next: %s", resp.Status)
	}
	var out struct{ Values []any `json:"values"` }
	return out.Values, json.NewDecoder(resp.Body).Decode(&out)
}
```

**Node.js**
```js
async function nextIds(entity, dtype, n, base = "http://localhost:7001") {
  const r = await fetch(`${base}/api/next-batch/${entity}?dataType=${dtype}&batchSize=${n}`);
  if (!r.ok) throw new Error(`bucket-next ${r.status}: ${await r.text()}`);
  return (await r.json()).values;   // strings for long / snowflake / uuid*
}
```

---

## Configuration

One YAML file per process (`-config <path>`). All keys:

| Key | Required | Default | Notes |
|---|---|---|---|
| `shard_id` | yes | — | `1..total_shards`, unique in the cluster |
| `total_shards` | yes | — | `1..1024`, identical on every node |
| `state_path` | yes | — | this shard's own state file |
| `listen_address` | no | `""` (all interfaces) | IP to bind — `127.0.0.1` on a dev box, the private IP on a server; hostnames are refused |
| `listen_port` | no | `7001` | |
| `segment_size` | no | `1000` | counter IDs reserved per durable write; use `10000` for high TPS |
| `segment_refill_watermark` | no | `0.5` | start the background refill when this fraction is used |
| `clock_drift_tolerance_ms` | no | `0` | extra backward clock jump Snowflake waits out |
| `snowflake_epoch_ms` | no | `1704067200000` | identical on every node; never change |
| `uuid_key` | no | built-in | 32/48/64 hex chars (`openssl rand -hex 16`); identical on every node; set before the first uuid ID; never change |

The service **refuses to start** when the config is invalid (e.g. `listen_address` is not an IP),
the state file is missing (without `-init`), or the state file belongs to another `shard_id`,
`total_shards`, uuid key or epoch:

```
state: state file ./a.json does not belong to this config (shard_id: file=1 config=2);
using it would issue duplicate IDs — point state_path at this shard's own file, or fix the config
```

Details: [docs/configuration.md](docs/configuration.md).

---

## CLI

`bin/bucket-next-cli -config <path> <command>` edits a shard's state file offline. Stop the
service before changing anything.

| Command | Effect |
|---|---|
| `init-state` | create the state file of a brand-new shard |
| `list` / `status <entity>` / `info` | inspect |
| `init <entity> <type> [start]` | register an entity |
| `reset <entity> <value> [--force]` | move an `int`/`long` counter (`--force` to go back) |
| `delete <entity> --force` | remove an entity (its IDs restart if it is reused) |
| `clear --force` | remove all entities |

Details: [docs/cli.md](docs/cli.md).

---

## Deployment

The routesphere convention: the service's config per tenant and profile (`config/tenants/<tenant>/<profile>/`), a
tenant conf saying where each profile runs (`tools/deploy/tenant-conf-v2/<tenant>.conf`), the shared ssh inventory by
reference, and one command for every deployed shard of a profile over SSH. Each shard runs as systemd unit `bucket-next@<shard_id>` with its
own config and state file, so several shards can share a host.

```bash
tools/deploy/remote-deploy-v2.sh --list
tools/deploy/remote-deploy-v2.sh btcl bed --dry-run        # plan + rendered configs, no SSH
tools/deploy/remote-deploy-v2.sh btcl bed --init-state     # first deploy: create the state file
tools/deploy/remote-deploy-v2.sh btcl bed                  # later deploys
tools/deploy/remote-deploy-v2.sh btcl bed --shard 1        # one shard
```

Before touching any host the script checks shard numbering, host files, port and state-file
clashes, and the uuid key file. After each start it checks that `/shard-info` reports the
deployed `shardId` and `totalShards`. Target hosts need passwordless sudo, `systemd` and `curl`.

In a profile's YAML, `listen_address: "{private_ip}"` makes every shard bind its own host's `private_ip` from the
inventory; a literal IP (e.g. `127.0.0.1`) applies to all shards. A cluster that will grow is declared at its final
`total_shards` and deploys only the shards that run today (`shards = 1`): the count is fixed for life.

Details: [docs/deployment.md](docs/deployment.md) · [tools/deploy/README.md](tools/deploy/README.md).

---

## How uniqueness is guaranteed

| Type | Mechanism |
|---|---|
| `int`, `long` | Shard *k* of *N* emits `k, k+N, k+2N, …`. Counters are reserved in blocks with an fsynced disk write; one reservation per entity at a time, so a block is never handed out twice. |
| `uuid*` | The same counter, passed through a bijection keyed by `uuid_key` + entity name. Different counters → different strings. |
| `snowflake` | Timestamp + shard id + per-ms sequence. A time lease is saved ahead of use; after a restart the service waits until the clock passes it. |

| Scope | Guarantee |
|---|---|
| same entity, same shard | never repeats — concurrency, crash and power loss included |
| same entity, all shards | never repeats, if every shard has its own `shard_id` and shares `total_shards`, `uuid_key`, `snowflake_epoch_ms` |
| different entities | `int`/`long` overlap on purpose (each entity counts alone); `uuid*` sequences are unrelated (statistically unique); `snowflake` is unique cluster-wide |

A crash forfeits the unused tail of a reservation: the sequence jumps forward (gap ≤
`segment_size`) but never repeats.

---

## Project layout and testing

```
cmd/bucket-next/         service entry
cmd/bucket-next-cli/     offline maintenance CLI
internal/config/         YAML loader + validation
internal/datatype/       type tags and limits
internal/shardmath/      value = shard + iteration × total_shards
internal/state/          state file: identity check, fsync + atomic rename
internal/allocator/      RAM reservations, one in flight per entity
internal/uuidgen/        counter → fixed-length string (keyed Feistel permutation)
internal/forge/          Snowflake with a persisted time lease
internal/server/         HTTP handlers
configs/sample.yaml      every config key with its default
config/tenants/          the service's config per tenant and profile
tools/deploy/            tenant-conf-v2 (where it runs), remote-deploy-v2.sh, the unit
docs/                    detailed guides
```

```bash
make test    # go test -race ./... — includes concurrency stress tests for duplicates
make vet
```

---

## More documentation

- [docs/getting-started.md](docs/getting-started.md) — step-by-step first run
- [docs/usage-examples.md](docs/usage-examples.md) — more recipes
- [docs/api-reference.md](docs/api-reference.md) — the API on its own page
- [docs/configuration.md](docs/configuration.md) — every key, startup checks, tuning
- [docs/cli.md](docs/cli.md) — the CLI in detail
- [docs/deployment.md](docs/deployment.md) — deploy walkthrough, backups, troubleshooting

Design notes (feature story) live in Trilium under `ID-Land`.
