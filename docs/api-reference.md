# API reference

Base URL `http://<listen_address>:<listen_port>` (default port `7001`). JSON everywhere. **No
authentication** — bind it to loopback or a private IP with `listen_address` (anyone who can reach
the port can mint IDs and reset counters). Examples are real responses from shard 2 of 3 serving `btcl` and `acme`.

**Everything under `/api/` names its tenant, first:** `/api/v2/{tenant}/{road}[/{entity or id}]`. Only the two probes
outside `/api/` are global.

## Endpoints

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

## Errors

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

## GET /api/v2/{tenant}/next-id/{entity} — scalar

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

## GET /api/v2/{tenant}/next-batch/{entity} — vector

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

## POST /api/v2/{tenant}/init/{entity}

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

## PUT /api/v2/{tenant}/reset/{entity}

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

## GET /api/v2/{tenant}/status/{entity}

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

## GET /api/v2/{tenant}/list

Every entity of the tenant on this shard, each in the `status` shape:

```json
{"entities":[{"currentIteration":1000,"currentValue":"2","dataType":"long","entityName":"cdr","nextValue":"5","shard":2},
             {"currentIteration":1000,"currentValue":2,"dataType":"int","entityName":"order","nextValue":5,"shard":2}],
 "shardInfo":{"shardId":2,"totalShards":3},"tenant":"btcl"}
```

Per tenant and per shard only — for a cluster view, call each shard and merge.

---

## GET /api/v2/{tenant}/segment-state/{entity}

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

## GET /api/v2/{tenant}/parse-snowflake/{id}

Decode a Snowflake minted by any shard (Snowflake IDs are unique across the installation, whichever tenant asked):

```json
{"binary":"0000010100110010111000110111111110110100010000000001000000000000","date":"2026-10-30T17:33:46.577Z",
 "id":"374611856649621504","sequence":0,"shardId":2,"tenant":"btcl","timestamp":1793381626577}
```

`400` if the id is not a valid 64-bit decimal.

## GET /api/v2/{tenant}/types

```json
{"apiVersions":{"scalar":"GET /api/v2/{tenant}/next-id/{entity}?dataType=T — one ID",
                "vector":"GET /api/v2/{tenant}/next-batch/{entity}?dataType=T&batchSize=N — N IDs (1..10000)"},
 "availableTypes":["int","long","snowflake","uuid8","uuid12","uuid16","uuid22"],
 "description":{"int":"Sequential 32-bit integer (shard 2: 2, 5, 8, ...)", "...":"..."},
 "shardInfo":{"shardId":2,"totalShards":3},
 "snowflakeInfo":{"epoch":"2024-01-01T00:00:00Z","maxIdsPerMs":4096,"maxShards":1024,
                  "bitsAllocation":{"timestamp":41,"shardId":10,"sequence":12,"total":64}},"tenant":"btcl"}
```

## GET /health

Global (no tenant).

```json
{"shard":2,"snowflakeStats":{"collisions":0,"generated":1,"maxIdsPerMs":4096,"maxShards":1024,"shardId":2,"totalShards":3,"waits":0},
 "status":"healthy","tenants":["acme","btcl"],"totalShards":3,"uptime":1.194490396}
```

## GET /shard-info

Global. Identity plus the values that **must be identical on every shard** — compare them across nodes after a deploy:
`totalShards`, `uuidKeyFingerprint`, `snowflakeEpochMs` and `tenants` (a caller rotates across shards; a tenant missing
on one would be refused there). `legacyTenant`, `v1Calls` and `v1Callers` show the pre-tenant roads during the move.

```json
{"legacyTenant":"btcl","shardId":2,"snowflakeEpochMs":1704067200000,"status":"active","tenants":["acme","btcl"],
 "totalShards":3,"uuidKeyFingerprint":"987a907e1b019511","v1Callers":{"127.0.0.1":1},"v1Calls":1}
```

## Pre-tenant roads

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

More: [usage-examples.md](usage-examples.md) · tenancy and the move: [tenancy.md](tenancy.md)
