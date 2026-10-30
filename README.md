# bucket-next

A sharded, multi-tenant, multi-type unique-ID service. Single Go binary, one YAML config, segment-cached for
high-TPS bursts, no coordination between shards.

- **One service per installation, serving every tenant.** Every call names its tenant —
  `/api/v2/{tenant}/next-id/{entity}` — and counters, state and status are per **(tenant, entity)**. A call without a
  tenant, or for a tenant this installation does not serve, is refused in words; there is never a default tenant.
- **Types, per entity:** `int`, `long`, `snowflake`, `uuid8`, `uuid12`, `uuid16`, `uuid22`.
- **Two API versions for every type:** *scalar* (`next-id` — one ID) and *vector*
  (`next-batch` — a range of up to 10 000 IDs).
- **Never repeats within a tenant's entity** — across shards, under concurrency, after a crash or a
  power loss.
- **Shard identity is enforced:** every process needs a `shard_id`, and every state file is bound to it.
- **Binds where you tell it:** `listen_address` keeps the unauthenticated API on loopback or a
  private IP.

Repository: `git@github.com:pialmmh/bucket-next.git` · Go 1.21+ · only dependency `gopkg.in/yaml.v3`

## Contents

- [Quick start](#quick-start)
- [Tenants](#tenants)
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
state_dir: ./data/shard-1
tenants: [btcl]
EOF

./bin/bucket-next -config my.yaml -init      # -init ONLY on the very first start
```

```bash
curl "http://localhost:7001/api/v2/btcl/next-id/order?dataType=int"                  # one ID
curl "http://localhost:7001/api/v2/btcl/next-batch/sms?dataType=uuid12&batchSize=100" # 100 IDs
```

`-init` creates the state of a brand-new shard. Every later start runs without it; if the state is ever missing the
service refuses to start rather than restart from zero.

---

## Tenants

```
one installation = one bucket-next (its shards = its own scale-out across boxes)
   │
   ├── tenant btcl ── entity partner.btcl (int)   partner ids     /api/v2/btcl/next-id/partner.btcl
   │                └ entity packageaccount (long) …              /api/v2/btcl/next-id/packageaccount
   └── tenant acme ── its own entities, its own counters          /api/v2/acme/…
```

- A **tenant** is tenant-master's tenant id — the word prime-context's roots carry (`btcl` = the root schema = the
  topic suffix = the tenant id). Spelling: `^[a-z][a-z0-9]{1,15}$` — lowercase letters and digits, a letter first, 2–16
  characters, never case-folded (`BTCL` is refused, not mapped). Not the facade's wider tenant code, not Odoo's company
  number.
- The tenants an installation serves are **declared** in its config (`tenants: [btcl]`); a call never creates one. A
  second tenant = one more name in `tenants` + a restart: each shard creates the tenant's file then. No code.
- Two tenants may use the same entity name: they are two counters. `int`/`long` values of different tenants overlap on
  purpose; `uuid*` strings of different tenants are unrelated (the scramble includes the tenant); `snowflake` IDs are
  unique across the whole installation.
- The record ids of the money and master tables now live in Odoo's database (`odoo_btcl`, the tier schemas inside,
  `company_id` per row). Nothing changes here: an id is an id.

The move from the pre-tenant service — the state carried over, the pre-tenant roads kept by one config line until every
caller has moved, the route back: [docs/tenancy.md](docs/tenancy.md).

---

## Data types

An entity is bound to one type on its first call, for life.

| Type | Wire form | Capacity per entity (whole cluster) | Example |
|---|---|---|---|
| `int` | JSON number | up to 2 147 483 647 | `42` |
| `long` | decimal **string** | up to 9.2 × 10^18 | `"1000004"` |
| `snowflake` | decimal string | ~69 years, 4096 per ms per shard | `"374611854007209984"` |
| `uuid8` | 8 chars `[0-9a-zA-Z]` | 218 trillion | `"0CIOE46z"` |
| `uuid12` / `uuid16` / `uuid22` | 12 / 16 / 22 chars | 9.2 × 10^18 | `"HNNP8mpF5JLzEljJ"` |

- `long` is a string because JSON numbers lose precision above 2^53.
- `uuid*` are **not random**: each is the entity's counter passed through a keyed permutation.
  They are fixed-length, look random, and never repeat within a tenant's entity.

---

## API reference

Base URL `http://<listen_address>:<listen_port>` (default port `7001`). JSON everywhere. **No
authentication** — bind it to loopback or a private IP with `listen_address` (anyone who can reach
the port can mint IDs and reset counters). Examples are real responses from shard 2 of 3 serving `btcl` and `acme`.

**Everything under `/api/` names its tenant, first:** `/api/v2/{tenant}/{road}[/{entity or id}]`. Only the two probes
outside `/api/` are global.

### Endpoints

| Method | Path | Purpose |
|---|---|---|
| GET | [`/api/v2/{tenant}/next-id/{entity}`](#get-apiv2tenantnext-identity--scalar) | **Scalar** — mint one ID |
| GET | [`/api/v2/{tenant}/next-batch/{entity}`](#get-apiv2tenantnext-batchentity--vector) | **Vector** — mint up to 10 000 IDs |
| POST | [`/api/v2/{tenant}/init/{entity}`](#post-apiv2tenantinitentity) | Register an entity, optional start value |
| PUT | [`/api/v2/{tenant}/reset/{entity}`](#put-apiv2tenantresetentity) | Move an `int`/`long` counter |
| GET | [`/api/v2/{tenant}/status/{entity}`](#get-apiv2tenantstatusentity) | One entity's snapshot |
| GET | [`/api/v2/{tenant}/list`](#get-apiv2tenantlist) | Every entity of the tenant on this shard |
| GET | [`/api/v2/{tenant}/segment-state/{entity}`](#get-apiv2tenantsegment-stateentity) | RAM reservation of a counter entity |
| GET | [`/api/v2/{tenant}/parse-snowflake/{id}`](#get-apiv2tenantparse-snowflakeid) | Decode a Snowflake ID |
| GET | [`/api/v2/{tenant}/types`](#get-apiv2tenanttypes) | Types and API versions |
| GET | [`/health`](#get-health) | Liveness + Snowflake stats (global) |
| GET | [`/shard-info`](#get-shard-info) | Shard identity, served tenants, cluster-wide values (global) |

`types` and `parse-snowflake` read no tenant data, but they take the tenant too, so the rule has no exceptions and a
caller's wrong tenant shows on its first call, whatever the call. The pre-tenant roads (`/api/next-id/{entity}` …,
no tenant) are kept by one config line during the move: [Pre-tenant roads](#pre-tenant-roads).

### Errors

Every error body is `{"error": "<word>", "message": "<detail>"}`; once the tenant is known it also names it
(`"tenant": "btcl"`). **The word is the contract** — match the word, not the status alone.

| Status | When | `error` |
|---|---|---|
| 400 | no tenant segment (`/api/v2/next-id/x`), or a pre-tenant road while they are switched off | `Tenant is required` |
| 400 | the tenant breaks `^[a-z][a-z0-9]{1,15}$` (`BTCL`, `bt_cl`, `b`) | `Invalid tenant` |
| 400 | a well-formed tenant this installation does not serve | `Unknown tenant` |
| 400 | the entity breaks `^[A-Za-z0-9._-]{1,64}$` | `Invalid entity name` |
| 400 | no entity in the path | `Entity name is required` |
| 400 | unknown/missing `dataType` | `Valid dataType is required` |
| 400 | `batchSize` missing or outside 1..10000 | `Valid batchSize is required` |
| 400 | entity bound to another type (body adds `registeredType`) | `Type mismatch` |
| 400 | start/reset value `< 1`, too large, or not allowed for the type | `Invalid startValue` / `Invalid value` / `Invalid start value` |
| 400 | `reset` without `value` · on a non-counter · `segment-state` of a snowflake | `value is required` · `Invalid operation` · `Invalid type` |
| 400 | a bad Snowflake id | `Invalid Snowflake ID` |
| 404 | entity not registered in this tenant (`status`, `reset`, `segment-state`) | `Entity not found` |
| 404 | no such road under `/api/v2/{tenant}/` | `Not found` |
| 405 | wrong method (the `Allow` header names the right one) | `Method not allowed` |
| 409 | the type has no values left for this entity on this shard | `Range exhausted` |
| 409 | `init` on an existing entity | `Entity already exists` |
| 409 | `reset` would move backward without `force` | `Backward reset refused` |
| 500 | server fault, e.g. the state file could not be written | `Generation failed` / `Failed to …` |

**Every tenant refusal is 400, never 404, on purpose:** 404 means "Entity not found", and a client that registers an
entity when `status` answers 404 (seed-idgen's `mintAbove` does) must never take an unknown tenant for an unregistered
entity.

```json
{"error":"Tenant is required","message":"the tenant comes first: /api/v2/{tenant}/next-id/{entity} (served tenants: acme, btcl)"}
{"error":"Invalid tenant","message":"tenant 'BTCL' breaks the rule ^[a-z][a-z0-9]{1,15}$ (tenant-master's tenant id: lowercase letters and digits, a letter first, 2-16 characters; never case-folded)"}
{"error":"Unknown tenant","message":"tenant 'nobody' is not served by this installation (served tenants: acme, btcl)"}
{"error":"Invalid entity name","message":"entity 'a b' breaks the rule ^[A-Za-z0-9._-]{1,64}$ (letters, digits, '.', '_', '-'; 1-64 characters)","tenant":"btcl"}
{"error":"Entity not found","tenant":"btcl"}
```

---

### GET /api/v2/{tenant}/next-id/{entity} — scalar

Mint **one** ID. The entity registers itself in the tenant on its first call.

| Param | In | Notes |
|---|---|---|
| `tenant` | path | one of the installation's tenants |
| `entity` | path | `^[A-Za-z0-9._-]{1,64}$` |
| `dataType` | query | required — one of the 7 types |

```bash
curl "http://localhost:7001/api/v2/btcl/next-id/order?dataType=int"
```
```json
{"dataType":"int","entityName":"order","shard":2,"tenant":"btcl","value":2}
```

```bash
curl "http://localhost:7001/api/v2/btcl/next-id/token?dataType=uuid12"
```
```json
{"dataType":"uuid12","entityName":"token","shard":2,"tenant":"btcl","value":"x24M8NOk49QE"}
```

The same entity in another tenant is another counter:
```bash
curl "http://localhost:7001/api/v2/acme/next-id/order?dataType=int"
```
```json
{"dataType":"int","entityName":"order","shard":2,"tenant":"acme","value":2}
```

Type mismatch:
```json
{"error":"Type mismatch","message":"Entity 'order' is registered as 'int', cannot use 'long'","registeredType":"int","tenant":"btcl"}
```

Exhausted (e.g. an `int` entity past 2 147 483 647):
```json
{"error":"Range exhausted","message":"Entity 'x' has no int values left on this shard (max 2147483647)","tenant":"btcl"}
```

---

### GET /api/v2/{tenant}/next-batch/{entity} — vector

Mint a **range** of IDs in one round trip — the high-TPS path. It uses the same counter as
`next-id`, so the two never overlap.

| Param | In | Notes |
|---|---|---|
| `tenant`, `entity` | path | as for `next-id` |
| `dataType` | query | required |
| `batchSize` | query | required, `1..10000` |

```bash
curl "http://localhost:7001/api/v2/btcl/next-batch/sms?dataType=long&batchSize=5"
```
```json
{"batchSize":5,"dataType":"long","endValue":"14","entityName":"sms",
 "range":{"first":"2","last":"14","step":3,"count":5},
 "shard":2,"startValue":"2","tenant":"btcl","values":["2","5","8","11","14"]}
```

For `int` and `long` the batch is one contiguous slice of this shard's sequence, and `range`
describes it fully: value *i* = `first + i × step` (`step` = `total_shards`).

```bash
curl "http://localhost:7001/api/v2/btcl/next-batch/session?dataType=uuid16&batchSize=3"
```
```json
{"batchSize":3,"dataType":"uuid16","endValue":"36f9gl3kSkMFRVjv","entityName":"session","shard":2,
 "startValue":"HNNP8mpF5JLzEljJ","tenant":"btcl","values":["HNNP8mpF5JLzEljJ","0BBfDAXuQOgF3YMu","36f9gl3kSkMFRVjv"]}
```

`snowflake` and `uuid*` batches have `values` but no `range`. On `409` nothing is handed out —
there are no partial batches.

---

### POST /api/v2/{tenant}/init/{entity}

Register an entity before traffic, optionally choosing where an `int`/`long` counter starts.

| Body field | Notes |
|---|---|
| `dataType` | required |
| `startValue` | optional; `int`/`long` only; `1..max`; snapped **forward** to this shard |

```bash
curl -X POST http://localhost:7001/api/v2/btcl/init/invoice \
  -H 'Content-Type: application/json' \
  -d '{"dataType":"long","startValue":1000}'
```
```json
{"actualStartValue":"1001","adjusted":true,"adjustmentReason":"Value adjusted to match shard 2 pattern",
 "dataType":"long","entityName":"invoice","initialized":true,
 "message":"Entity 'invoice' initialized successfully with type 'long'",
 "nextValue":"1001","pattern":"2, 5, 8, ...","requestedStartValue":1000,"shard":2,"tenant":"btcl"}
```

`1000 mod 3 = 1`, but shard 2 owns residue 2, so the start moves up to `1001`.

Again → `409`:
```json
{"currentStatus":{"currentIteration":333,"dataType":"long","shard":2},"error":"Entity already exists",
 "message":"Entity 'invoice' is already registered with type 'long'","tenant":"btcl"}
```
`400 Invalid startValue`:
```json
{"error":"Invalid startValue","message":"startValue must be between 1 and 9223372036854775807 for type long","tenant":"btcl"}
```

---

### PUT /api/v2/{tenant}/reset/{entity}

Move an `int`/`long` counter. The value is snapped forward to this shard. Moving **backward**
— below IDs that may already be in use — is refused unless `"force": true`.

| Body field | Notes |
|---|---|
| `value` | required, `1..max` |
| `force` | optional, default `false` — allow a backward move |

```bash
curl -X PUT http://localhost:7001/api/v2/btcl/reset/invoice \
  -H 'Content-Type: application/json' -d '{"value":5000}'
```
```json
{"dataType":"long","entityName":"invoice","message":"Counter reset successfully. Next value will be 5000","nextValue":"5000",
 "reset":{"actualValue":"5000","forced":false,"newIteration":1666,"previousIteration":334,"previousNextValue":"1004","requestedValue":"5000"},
 "shard":2,"tenant":"btcl"}
```

Backward without `force` → `409`:
```json
{"error":"Backward reset refused","message":"Next value is 5000; moving to 101 could re-issue IDs that are already in use. Send \"force\": true to do it anyway.","nextValue":"5000","tenant":"btcl"}
```

With `{"value":100,"force":true}` it succeeds and reports `"forced":true`.
Errors: `404` unknown entity, `400` not `int`/`long` or bad value, `409` as above.

---

### GET /api/v2/{tenant}/status/{entity}

```bash
curl http://localhost:7001/api/v2/btcl/status/cdr
```
```json
{"currentIteration":1000,"currentValue":"2","dataType":"long","entityName":"cdr","nextValue":"5","shard":2,"tenant":"btcl"}
```

| Field | Meaning |
|---|---|
| `currentValue` | last value this process handed out (`null` if none yet) |
| `nextValue` | value it will hand out next |
| `currentIteration` | durable mark in the state file (runs ahead because of reservations) |

For `uuid*` both values are `null` (opaque strings). For `snowflake`, `currentValue` is
`"N/A (Snowflake IDs are not sequential)"`. `404 Entity not found` if the tenant has no such entity.

---

### GET /api/v2/{tenant}/list

Every entity of the tenant on this shard, each in the `status` shape:

```json
{"entities":[{"currentIteration":1000,"currentValue":"2","dataType":"long","entityName":"cdr","nextValue":"5","shard":2},
             {"currentIteration":1000,"currentValue":2,"dataType":"int","entityName":"order","nextValue":5,"shard":2}],
 "shardInfo":{"shardId":2,"totalShards":3},"tenant":"btcl"}
```

Per tenant and per shard only — for a cluster view, call each shard and merge.

---

### GET /api/v2/{tenant}/segment-state/{entity}

Diagnostic view of a counter entity's RAM reservation (`int`, `long`, `uuid*`).

```bash
curl http://localhost:7001/api/v2/btcl/segment-state/cdr
```
```json
{"dataType":"long","entityName":"cdr","shard":2,"tenant":"btcl",
 "segment":{"diskHighWaterMark":"3002","diskMark":1000,"issued":1,"loaded":true,"nextIteration":1,"nextValue":"5",
            "refillInFlight":false,"remaining":999,"reservedEnd":1000,"size":1000,"watermark":0.5}}
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

### GET /api/v2/{tenant}/parse-snowflake/{id}

Decode a Snowflake minted by any shard (Snowflake IDs are unique across the installation, whichever tenant asked):

```json
{"binary":"0000010100110010111000110111111110110100010000000001000000000000","date":"2026-10-30T17:33:46.577Z",
 "id":"374611856649621504","sequence":0,"shardId":2,"tenant":"btcl","timestamp":1793381626577}
```

`400` if the id is not a valid 64-bit decimal.

### GET /api/v2/{tenant}/types

```json
{"apiVersions":{"scalar":"GET /api/v2/{tenant}/next-id/{entity}?dataType=T — one ID",
                "vector":"GET /api/v2/{tenant}/next-batch/{entity}?dataType=T&batchSize=N — N IDs (1..10000)"},
 "availableTypes":["int","long","snowflake","uuid8","uuid12","uuid16","uuid22"],
 "description":{"int":"Sequential 32-bit integer (shard 2: 2, 5, 8, ...)", "...":"..."},
 "shardInfo":{"shardId":2,"totalShards":3},
 "snowflakeInfo":{"epoch":"2024-01-01T00:00:00Z","maxIdsPerMs":4096,"maxShards":1024,
                  "bitsAllocation":{"timestamp":41,"shardId":10,"sequence":12,"total":64}},"tenant":"btcl"}
```

### GET /health

Global (no tenant).

```json
{"shard":2,"snowflakeStats":{"collisions":0,"generated":1,"maxIdsPerMs":4096,"maxShards":1024,"shardId":2,"totalShards":3,"waits":0},
 "status":"healthy","tenants":["acme","btcl"],"totalShards":3,"uptime":1.194490396}
```

### GET /shard-info

Global. Identity plus the values that **must be identical on every shard** — compare them across nodes after a deploy:
`totalShards`, `uuidKeyFingerprint`, `snowflakeEpochMs` and `tenants` (a caller rotates across shards; a tenant missing
on one would be refused there). `legacyTenant`, `v1Calls` and `v1Callers` show the pre-tenant roads during the move.

```json
{"legacyTenant":"btcl","shardId":2,"snowflakeEpochMs":1704067200000,"status":"active","tenants":["acme","btcl"],
 "totalShards":3,"uuidKeyFingerprint":"987a907e1b019511","v1Callers":{"127.0.0.1":1},"v1Calls":1}
```

### Pre-tenant roads

The roads of the service before tenancy — `/api/next-id/{entity}`, `/api/next-batch/{entity}`, `/api/init/{entity}`,
`/api/reset/{entity}`, `/api/status/{entity}`, `/api/segment-state/{entity}`, `/api/list`, `/api/types`,
`/api/parse-snowflake/{id}` — stay while callers move, **switched by one config line**:

| Config | The pre-tenant roads |
|---|---|
| `legacy_tenant: btcl` | act on tenant `btcl` and answer exactly as before tenancy (no `tenant` field); the same counters as `/api/v2/btcl/…` |
| no `legacy_tenant` | answer `400 Tenant is required`, naming the v2 road to call; nothing is minted |

```json
{"dataType":"int","entityName":"order","shard":2,"value":5}
{"error":"Tenant is required","message":"this installation serves tenants by name: call /api/v2/{tenant}/next-id/partner.btcl"}
```

Every pre-tenant call, served or refused, is counted per caller address (`v1Calls`, `v1Callers` on `/shard-info`) and
logged once per caller per hour, so an operator sees who has not moved. One difference from before tenancy: a `uuid*`
entity registered after the move gets the tenant's scramble; entities carried over keep theirs.

---

## Examples

All examples use a single shard on `localhost:7001` serving tenant `btcl`.

### One ID per type (scalar)

```bash
curl "localhost:7001/api/v2/btcl/next-id/order?dataType=int"         # {"value":1, ...}
curl "localhost:7001/api/v2/btcl/next-id/cdr?dataType=long"          # {"value":"1", ...}
curl "localhost:7001/api/v2/btcl/next-id/event?dataType=snowflake"   # {"value":"374611854007209984", ...}
curl "localhost:7001/api/v2/btcl/next-id/call?dataType=uuid8"        # {"value":"0CIOE46z", ...}
curl "localhost:7001/api/v2/btcl/next-id/session?dataType=uuid22"    # {"value":"<22 chars>", ...}
```

### A range per type (vector)

```bash
curl "localhost:7001/api/v2/btcl/next-batch/order?dataType=int&batchSize=1000"
curl "localhost:7001/api/v2/btcl/next-batch/sms-id?dataType=uuid12&batchSize=10000"
curl "localhost:7001/api/v2/btcl/next-batch/event?dataType=snowflake&batchSize=500"
```

### Scalar and vector share one sequence

```bash
curl "localhost:7001/api/v2/btcl/next-id/x?dataType=int"                  # 1
curl "localhost:7001/api/v2/btcl/next-batch/x?dataType=int&batchSize=3"   # 2, 3, 4
curl "localhost:7001/api/v2/btcl/next-id/x?dataType=int"                  # 5
```

### Keep only the range of a numeric batch

```bash
curl -s "localhost:7001/api/v2/btcl/next-batch/cdr?dataType=long&batchSize=5000" | jq .range
# {"first":"1","last":"5000","step":1,"count":5000}   value i = first + i*step
```

### Seed, move forward, move back on purpose

**Counters are per shard.** `init`, `reset` and `status` act only on the shard you call. To put an
entity's counter above IDs that already exist, do it on **every** shard that mints for that entity
(each one snaps the value to its own residue).

```bash
curl -X POST localhost:7001/api/v2/btcl/init/invoice -H 'Content-Type: application/json' \
     -d '{"dataType":"long","startValue":1000}'                        # next = 1000
curl -X PUT  localhost:7001/api/v2/btcl/reset/invoice -H 'Content-Type: application/json' \
     -d '{"value":5000}'                                               # forward: OK
curl -X PUT  localhost:7001/api/v2/btcl/reset/invoice -H 'Content-Type: application/json' \
     -d '{"value":10}'                                                 # backward: 409
curl -X PUT  localhost:7001/api/v2/btcl/reset/invoice -H 'Content-Type: application/json' \
     -d '{"value":10,"force":true}'                                    # backward, on purpose
```

### Multi-shard numbering

Each shard owns one residue class — no coordination, no collisions — for every tenant's every counter:

| Shard | `int` / `long` values |
|---|---|
| 1 of 3 | 1, 4, 7, 10, … |
| 2 of 3 | 2, 5, 8, 11, … |
| 3 of 3 | 3, 6, 9, 12, … |

A client may call any shard (round-robin, nearest, …); every ID stays unique for its tenant's entity
across the cluster. `uuid*` follow the same rule; `snowflake` carries the shard in its bits.

### Client snippets

Every snippet takes the tenant from its caller: there is no default tenant in a client either.

**bash + jq**
```bash
next_id()  { curl -s "http://localhost:7001/api/v2/$1/next-id/$2?dataType=$3" | jq -r .value; }
next_ids() { curl -s "http://localhost:7001/api/v2/$1/next-batch/$2?dataType=$3&batchSize=$4" | jq -r '.values[]'; }

order_id=$(next_id btcl order long)
next_ids btcl sms-id uuid12 1000 > ids.txt
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
    record One(String tenant, String entityName, String dataType, String value) {}   // int arrives as a number; Jackson coerces it
    record Many(String tenant, String entityName, String dataType, List<String> values) {}

    private static final ObjectMapper JSON = new ObjectMapper()
            .configure(DeserializationFeature.FAIL_ON_UNKNOWN_PROPERTIES, false);
    private final HttpClient http = HttpClient.newHttpClient();
    private final String base;

    public BucketNextClient(String base) { this.base = base; }

    public String nextId(String tenant, String entity, String dataType) throws Exception {
        return get("/api/v2/" + tenant + "/next-id/" + entity + "?dataType=" + dataType, One.class).value();
    }

    public List<String> nextIds(String tenant, String entity, String dataType, int n) throws Exception {
        return get("/api/v2/" + tenant + "/next-batch/" + entity + "?dataType=" + dataType + "&batchSize=" + n, Many.class).values();
    }

    private <T> T get(String path, Class<T> type) throws Exception {
        HttpResponse<String> r = http.send(HttpRequest.newBuilder(URI.create(base + path)).GET().build(),
                HttpResponse.BodyHandlers.ofString());
        if (r.statusCode() != 200) throw new IllegalStateException("bucket-next " + r.statusCode() + ": " + r.body());
        return JSON.readValue(r.body(), type);
    }
}
// new BucketNextClient("http://10.10.188.2:7001").nextIds("btcl", "sms-id", "uuid12", 5000);
```

**Python**
```python
import requests

BASE = "http://localhost:7001"

def next_id(tenant, entity, dtype):
    r = requests.get(f"{BASE}/api/v2/{tenant}/next-id/{entity}", params={"dataType": dtype})
    r.raise_for_status()
    return r.json()["value"]      # int for "int"; str for long / snowflake / uuid*

def next_ids(tenant, entity, dtype, n):
    r = requests.get(f"{BASE}/api/v2/{tenant}/next-batch/{entity}", params={"dataType": dtype, "batchSize": n})
    r.raise_for_status()
    return r.json()["values"]

order_id = next_id("btcl", "order", "long")
sms_ids = next_ids("btcl", "sms-id", "uuid12", 5000)
```

**Go**
```go
func nextIDs(base, tenant, entity, dtype string, n int) ([]any, error) {
	u := fmt.Sprintf("%s/api/v2/%s/next-batch/%s?dataType=%s&batchSize=%d", base, tenant, entity, url.QueryEscape(dtype), n)
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
async function nextIds(tenant, entity, dtype, n, base = "http://localhost:7001") {
  const r = await fetch(`${base}/api/v2/${tenant}/next-batch/${entity}?dataType=${dtype}&batchSize=${n}`);
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
| `state_dir` | yes | — | this shard's own directory: `shard.json` + `tenants/<tenant>.json` (replaces `state_path`, which is refused naming it) |
| `tenants` | yes | — | the tenants this installation serves, e.g. `[btcl]`; identical on every node |
| `legacy_tenant` | no | none | the pre-tenant roads act on it, and a pre-tenant `state.json` in `state_dir` is carried over into it; unset = those roads answer 400 |
| `listen_address` | no | `""` (all interfaces) | IP to bind — `127.0.0.1` on a dev box, the private IP on a server; hostnames are refused |
| `listen_port` | no | `7001` | |
| `segment_size` | no | `1000` | counter IDs reserved per durable write; use `10000` for high TPS |
| `segment_refill_watermark` | no | `0.5` | start the background refill when this fraction is used |
| `clock_drift_tolerance_ms` | no | `0` | extra backward clock jump Snowflake waits out |
| `snowflake_epoch_ms` | no | `1704067200000` | identical on every node; never change |
| `uuid_key` | no | built-in | 32/48/64 hex chars (`openssl rand -hex 16`); identical on every node; set before the first uuid ID; never change |

The service **refuses to start**, in words, when the config is invalid, the shard has no state (without `-init`), a
file belongs to another `shard_id`, `total_shards`, uuid key or epoch, a tenant file holds another tenant, a tenant on
the shard's roster has lost its file, or a pre-tenant `state.json` has no tenant to own it:

```
state: state file /var/lib/bucket-next/shard-1/shard.json does not belong to this config (total_shards: file=2 config=3);
using it would issue duplicate IDs — point state_dir at this shard's own directory, or fix the config
```

Details: [docs/configuration.md](docs/configuration.md).

---

## CLI

`bin/bucket-next-cli -config <path> [-tenant <tenant>] <command>` reads and edits a shard's state offline. The commands
that change state take the state directory's lock, so they are refused while the service runs; the read-only ones
(`check`, `info`, `tenants`, `list`, `status`) never write.

| Command | Effect |
|---|---|
| `check` | what the service's next start will do; exit 0 it starts, 1 it would refuse (why), 3 no state yet |
| `info` / `tenants` | the shard: identity, Snowflake mark, carry-over, roster, tenants |
| `init-state` | create the state of a brand-new shard |
| `adopt-tenant <tenant>` | put a tenant's existing file on the roster (after `shard.json` was restored from an older backup) |
| `-tenant t list` / `status <entity>` | inspect a tenant |
| `-tenant t init <entity> <type> [start]` | register an entity |
| `-tenant t reset <entity> <value> [--force]` | move an `int`/`long` counter (`--force` to go back) |
| `-tenant t delete <entity> --force` / `clear --force` | remove entities (their IDs restart if reused) |
| `-tenant t rename <from> <to>` | carry a counter to a new name (not uuid; not while `legacy_tenant` is that tenant) |
| `-tenant t export-v2` | the route back to a pre-tenant binary: `state.json` from the current marks |

Details: [docs/cli.md](docs/cli.md).

---

## Deployment

The routesphere convention: the service's config per installation and profile (`config/tenants/<tenant>/<profile>/` —
here `<tenant>` is the installation, whose boxes; the data tenants it serves are the profile's `tenants:` key), a tenant
conf saying where each profile runs (`tools/deploy/tenant-conf-v2/<tenant>.conf`), the shared ssh inventory by reference,
and one command for every deployed shard of a profile over SSH. Each shard runs as systemd unit `bucket-next@<shard_id>`
with its own config and state directory, so several shards can share a host.

```bash
tools/deploy/remote-deploy-v2.sh --list
tools/deploy/remote-deploy-v2.sh btcl bed --dry-run        # plan + rendered configs, no SSH
tools/deploy/remote-deploy-v2.sh btcl bed --init-state     # first deploy: create the state
tools/deploy/remote-deploy-v2.sh btcl bed                  # later deploys
tools/deploy/remote-deploy-v2.sh btcl bed --shard 1        # one shard
```

Before touching any host the script checks shard numbering, the tenants (one line, tenant ids, `legacy_tenant` among
them), host files, port and state-directory clashes, and the uuid key file. On each host the NEW binary first reads the
shard's state read-only (`bucket-next-cli check`): a refusal stops the deploy before anything is installed, and the
running shard keeps serving. After each start the script checks that `/shard-info` reports the deployed `shardId`,
`totalShards` and `tenants`. Target hosts need passwordless sudo, `systemd` and `curl`.

Details: [docs/deployment.md](docs/deployment.md) · [docs/tenancy.md](docs/tenancy.md) (the bed's move) ·
[tools/deploy/README.md](tools/deploy/README.md).

---

## How uniqueness is guaranteed

| Type | Mechanism |
|---|---|
| `int`, `long` | Shard *k* of *N* emits `k, k+N, k+2N, …` for each (tenant, entity). Counters are reserved in blocks with an fsynced write of the tenant's file; one reservation per entity at a time, so a block is never handed out twice. |
| `uuid*` | The same counter, passed through a bijection keyed by `uuid_key` + tenant + entity name (an entity carried over from before tenancy keeps its entity-only scramble; the scramble is stored with the record, never re-derived). Different counters → different strings. |
| `snowflake` | Timestamp + shard id + per-ms sequence, one forge per shard for every tenant. A time lease is saved ahead of use; after a restart the service waits until the clock passes it. |

| Scope | Guarantee |
|---|---|
| same tenant, same entity, same shard | never repeats — concurrency, crash and power loss included |
| same tenant, same entity, all shards | never repeats, if every shard has its own `shard_id` and shares `total_shards`, `uuid_key`, `snowflake_epoch_ms` |
| different entities or tenants | `int`/`long` overlap on purpose (each counter counts alone); `uuid*` sequences are unrelated (statistically unique); `snowflake` is unique installation-wide |

A crash forfeits the unused tail of a reservation: the sequence jumps forward (gap ≤
`segment_size`) but never repeats.

---

## Project layout and testing

```
cmd/bucket-next/         service entry
cmd/bucket-next-cli/     offline maintenance CLI
internal/config/         YAML loader + validation
internal/names/          the tenant and entity spelling rules
internal/datatype/       type tags and limits
internal/shardmath/      value = shard + iteration × total_shards
internal/state/          shard.json + tenants/<t>.json: identity check, roster, carry-over, lock, fsync + atomic rename
internal/allocator/      RAM reservations, one in flight per entity (one allocator per tenant)
internal/uuidgen/        counter → fixed-length string (keyed Feistel permutation)
internal/forge/          Snowflake with a persisted time lease
internal/server/         HTTP: v2.go (tenant-first roads), v1.go (pre-tenant roads), handlers.go
configs/sample.yaml      every config key with its default
config/tenants/          the service's config per installation and profile
tools/deploy/            tenant-conf-v2 (where it runs), remote-deploy-v2.sh, the unit
tools/rehearsal/         the bed's move rehearsed: tenant-first.sh (local, SIGKILL loop), deploy-in-lxc.sh (systemd)
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
- [docs/tenancy.md](docs/tenancy.md) — tenants, the state carried over, the switch-over, the route back
- [docs/configuration.md](docs/configuration.md) — every key, startup checks, tuning
- [docs/cli.md](docs/cli.md) — the CLI in detail
- [docs/deployment.md](docs/deployment.md) — deploy walkthrough, backups, troubleshooting

Design notes (feature story) live in Trilium under `ID-Land`.
