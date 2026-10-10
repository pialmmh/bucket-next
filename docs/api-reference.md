# API reference

All endpoints speak JSON. **No authentication** — bind the service to loopback or a private IP
with `listen_address` (see [configuration.md](configuration.md#listen_address--keep-the-api-private)).
Base URL is `http://<listen_address>:<listen_port>` (default port `7001`). Examples below are real
responses from shard 2 of 3.

## Conventions

- **Two versions for every ID type** — *scalar* `next-id` returns one ID; *vector*
  `next-batch` returns a range of 1–10 000 IDs. Both use the same counter, so they never overlap.
- **`long` values are decimal strings**, not JSON numbers (JSON loses precision above 2^53).
  `int` values are JSON numbers.
- **Errors** look like `{"error": "<short code>", "message": "<detail>"}`.
  - `400` — bad input (unknown type, bad batch size, type mismatch, value out of range)
  - `404` — unknown entity
  - `409` — a state conflict: range exhausted, entity already exists, backward reset refused
  - `500` — server fault (e.g. the state file could not be written)
- **Data types**: `int`, `long`, `snowflake`, `uuid8`, `uuid12`, `uuid16`, `uuid22`.

## Endpoint index

| Method | Path | Purpose |
|---|---|---|
| GET | `/api/next-id/{entity}` | **Scalar** — mint one ID |
| GET | `/api/next-batch/{entity}` | **Vector** — mint a range of up to 10 000 IDs |
| POST | `/api/init/{entity}` | Register an entity, optionally with a start value |
| PUT | `/api/reset/{entity}` | Move an `int`/`long` counter |
| GET | `/api/status/{entity}` | One entity's snapshot |
| GET | `/api/list` | Every entity on this shard |
| GET | `/api/segment-state/{entity}` | RAM reservation of a counter entity |
| GET | `/api/parse-snowflake/{id}` | Decode a Snowflake ID |
| GET | `/health` | Liveness + Snowflake stats |
| GET | `/shard-info` | Shard identity + values every shard must share |
| GET | `/api/types` | Supported types and API versions |

## Data types at a glance

| Type | Wire form | Capacity per entity (cluster-wide) | How it's made |
|---|---|---|---|
| `int` | JSON number | up to 2 147 483 647 | shard-interleaved counter |
| `long` | decimal string | up to 9.2 × 10^18 | shard-interleaved counter |
| `snowflake` | decimal string | ~69 years from the epoch, 4096/ms/shard | 41-bit time + 10-bit shard + 12-bit sequence |
| `uuid8` | 8 chars `[0-9a-zA-Z]` | 218 trillion | counter, scrambled by a keyed permutation |
| `uuid12`/`uuid16`/`uuid22` | 12/16/22 chars | 9.2 × 10^18 | counter, scrambled by a keyed permutation |

`uuid*` strings are fixed-length and look random, but they are **guaranteed unique per
entity** — they are the entity's counter run through a bijection keyed by `uuid_key` and the
entity name.

---

## GET /api/next-id/{entity} — scalar

Mint one ID. **Auto-registers** the entity with the requested type on its first call.

| Param | In | Notes |
|---|---|---|
| `entity` | path | Any non-empty string. |
| `dataType` | query | Required. One of the 7 types. |

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

**Errors**
- `400` `Valid dataType is required` — missing or unknown `dataType`.
- `400` `Type mismatch` — entity is bound to another type:
  ```json
  {"error":"Type mismatch","message":"Entity 'order' is registered as 'int', cannot use 'long'","registeredType":"int"}
  ```
- `409` `Range exhausted` — the entity's type has no values left on this shard:
  ```json
  {"error":"Range exhausted","message":"Entity 'x' has no int values left on this shard (max 2147483647)"}
  ```

---

## GET /api/next-batch/{entity} — vector

Mint a range of IDs in one round trip. The high-TPS path.

| Param | In | Notes |
|---|---|---|
| `entity` | path | Any non-empty string. |
| `dataType` | query | Required. |
| `batchSize` | query | Required. Integer `1..10000`. |

```bash
curl "http://localhost:7001/api/next-batch/cdr?dataType=long&batchSize=4"
```
```json
{"batchSize":4,"dataType":"long","entityName":"cdr","shard":2,
 "startValue":"2","endValue":"11","values":["2","5","8","11"],
 "range":{"first":"2","last":"11","step":3,"count":4}}
```

For `int` and `long` the batch is one contiguous slice of this shard's sequence, so `range`
describes it completely: value *i* is `first + i × step` (`step` = `total_shards`).

```bash
curl "http://localhost:7001/api/next-batch/token?dataType=uuid8&batchSize=3"
```
```json
{"batchSize":3,"dataType":"uuid8","entityName":"token","shard":2,
 "startValue":"0CIOE46z","endValue":"LrdDXHX4","values":["0CIOE46z","gvV6EFAx","LrdDXHX4"]}
```

`snowflake` and `uuid*` batches carry `values` only (no `range`).

**Errors** — as for `next-id`, plus `400` `Valid batchSize is required` for a missing,
non-numeric, `< 1` or `> 10000` size. On `409` no IDs are handed out (no partial batch).

---

## POST /api/init/{entity}

Register an entity before any traffic, optionally choosing where an `int`/`long` counter
starts. Refuses if the entity exists.

**Counters are per shard.** `init`, `reset` and `status` act only on the shard you call. To put an
entity's counter above IDs that already exist, do it on **every** shard that mints for that entity
(each one snaps the value to its own residue).

Body (`application/json`):

| Field | Notes |
|---|---|
| `dataType` | Required. |
| `startValue` | Optional; `int`/`long` only; `1..max`. Snapped **forward** to this shard's residue. |

```bash
curl -X POST http://localhost:7001/api/init/invoice \
  -H 'Content-Type: application/json' -d '{"dataType":"long","startValue":1000}'
```
```json
{"entityName":"invoice","dataType":"long","shard":2,"initialized":true,
 "message":"Entity 'invoice' initialized successfully with type 'long'",
 "requestedStartValue":1000,"actualStartValue":"1001",
 "adjusted":true,"adjustmentReason":"Value adjusted to match shard 2 pattern",
 "nextValue":"1001","pattern":"2, 5, 8, ..."}
```

**Errors**
- `409` `Entity already exists` (body includes its `dataType` and `currentIteration`).
- `400` `Invalid startValue` — `< 1`, above the type's maximum, no slot for this shard at or
  below the maximum, or given for a non-numeric type:
  ```json
  {"error":"Invalid startValue","message":"startValue must be between 1 and 9223372036854775807 for type long"}
  ```

---

## PUT /api/reset/{entity}

Move an `int`/`long` counter. The value is snapped forward to this shard's residue. It takes
effect immediately. Like `init`, it acts on this shard only — repeat it on every shard.

Moving the counter **backward** — below IDs that may already be in use — is refused unless
you send `"force": true`, because it re-issues IDs.

Body (`application/json`): `{ "value": <number>, "force": <bool, optional> }`

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

Backward without `force`:
```json
{"error":"Backward reset refused","message":"Next value is 14; moving to 2 could re-issue IDs that are already in use. Send \"force\": true to do it anyway.","nextValue":14}
```

Backward with `"force": true` succeeds and reports `"forced": true`.

**Errors**
- `404` — entity not registered (reset never creates one).
- `400` — entity is not `int`/`long`, or `value` is missing, `< 1` or above the maximum.
- `409` `Backward reset refused` — see above.

---

## GET /api/status/{entity}

```bash
curl http://localhost:7001/api/status/cdr
```
```json
{"entityName":"cdr","dataType":"long","currentIteration":1000,"currentValue":"11","nextValue":"14","shard":2}
```

- `currentValue` — the last value this process handed out (`null` if none yet).
- `nextValue` — the value it will hand out next.
- `currentIteration` — the durable mark in the state file (it runs ahead of `nextValue`
  because of reservations).
- For `uuid*`, `currentValue` and `nextValue` are `null` (the strings are opaque). For
  `snowflake`, `currentValue` reads `"N/A (Snowflake IDs are not sequential)"`.

**Errors**: `404` if the entity does not exist.

---

## GET /api/list

Every entity on this shard, each in the `status` shape, plus `shardInfo`.

```json
{"entities":[{"entityName":"cdr","dataType":"long","currentIteration":1000,"currentValue":"11","nextValue":"14","shard":2}, ...],
 "shardInfo":{"shardId":2,"totalShards":3}}
```

Per shard only. For a cluster view, call every shard and merge.

---

## GET /api/segment-state/{entity}

Diagnostic view of a counter entity's RAM reservation (`int`, `long`, `uuid*`). Not on the hot
path.

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
| `remaining` | IDs reserved in RAM, not yet handed out |
| `refillInFlight` | the background reservation is writing the next block |
| `nextIteration` / `reservedEnd` | the RAM window `[next, end)` this process owns |
| `diskMark` | the durable mark; always above every ID handed out |
| `nextValue`, `diskHighWaterMark` | the same positions as values (`int`/`long` only) |

**Errors**: `404` unknown entity; `400` for `snowflake` (no reservations).

---

## GET /api/parse-snowflake/{id}

Decode a Snowflake ID minted by any shard (pure arithmetic, uses `snowflake_epoch_ms`).

```json
{"id":"315240570203148288","timestamp":1779226511668,"date":"2026-05-19T21:35:11.668Z",
 "shardId":2,"sequence":0,"binary":"0000010001011111..."}
```

**Errors**: `400` if the path is not a valid 64-bit decimal.

---

## GET /health

```json
{"status":"healthy","uptime":12345.67,"shard":2,"totalShards":3,
 "snowflakeStats":{"generated":184213,"collisions":4,"waits":0,"shardId":2,"totalShards":3,"maxIdsPerMs":4096,"maxShards":1024}}
```

## GET /shard-info

Identity plus the values that **must be identical on every shard** of a cluster. Compare them
across nodes after a deploy.

```json
{"shardId":2,"totalShards":3,"status":"active","uuidKeyFingerprint":"4179529caf32c8cc","snowflakeEpochMs":1704067200000}
```

## GET /api/types

Lists the types, a description of each, the two API versions and the Snowflake layout.

```json
{"availableTypes":["int","long","snowflake","uuid8","uuid12","uuid16","uuid22"],
 "apiVersions":{"scalar":"GET /api/next-id/{entity}?dataType=T — one ID",
                "vector":"GET /api/next-batch/{entity}?dataType=T&batchSize=N — N IDs (1..10000)"},
 "description":{"uuid8":"Unique 8-character alphanumeric string (shard-interleaved counter, keyed scramble)", "...":"..."},
 "shardInfo":{"shardId":2,"totalShards":3},
 "snowflakeInfo":{"epoch":"2024-01-01T00:00:00Z","maxIdsPerMs":4096,"maxShards":1024,
                  "bitsAllocation":{"timestamp":41,"shardId":10,"sequence":12,"total":64}}}
```
