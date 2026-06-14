# API reference

All endpoints speak JSON. **No authentication** — the service is meant to live on a private
network. Base URL is `http://<host>:<listen_port>` (default port `7001`).

Every response carries a top-level `shard` (or `shardId`) field so a caller can confirm which
node answered.

## Conventions

- **`long` values are decimal strings**, not JSON numbers — JSON numbers lose precision above
  2^53. `int` values stay JSON numbers.
- **Errors** use `{"error": "<short code>", "message": "<detail>"}` with HTTP 4xx for client
  mistakes and 5xx for server faults.
- **Data types**: `int`, `long`, `snowflake`, `uuid8`, `uuid12`, `uuid16`, `uuid22`.

## Endpoint index

| Method | Path | Purpose |
|---|---|---|
| GET | `/health` | Liveness + Snowflake stats |
| GET | `/shard-info` | Shard identity |
| GET | `/api/types` | Supported types + layout |
| GET | `/api/next-id/:entity` | Mint one ID |
| GET | `/api/next-batch/:entity` | Mint up to 10 000 IDs |
| POST | `/api/init/:entity` | Explicit registration |
| PUT | `/api/reset/:entity` | Move a numeric counter |
| GET | `/api/status/:entity` | One-entity snapshot |
| GET | `/api/list` | All entities on this shard |
| GET | `/api/segment-state/:entity` | RAM cache fill state |
| GET | `/api/parse-snowflake/:id` | Decode a Snowflake ID |

---

## GET /health

Liveness probe. Returns shard identity, uptime, and running Snowflake forge counters.

```bash
curl http://localhost:7001/health
```
```json
{
  "status": "healthy",
  "uptime": 12345.67,
  "shard": 2,
  "totalShards": 3,
  "snowflakeStats": {
    "generated": 184213, "collisions": 4, "waits": 0,
    "shardId": 2, "totalShards": 3, "maxIdsPerMs": 4096, "maxShards": 1024
  }
}
```

---

## GET /shard-info

Bare shard identity. The smallest payload for load-balancer / registry checks.

```bash
curl http://localhost:7001/shard-info
```
```json
{ "shardId": 2, "totalShards": 3, "status": "active" }
```

---

## GET /api/types

Lists supported data types with descriptions and the Snowflake bit layout.

```bash
curl http://localhost:7001/api/types
```
```json
{
  "availableTypes": ["int","long","snowflake","uuid8","uuid12","uuid16","uuid22"],
  "description": {
    "int": "Sequential 32-bit integer (shard 2: 2, 5, 8, ...)",
    "long": "Sequential 64-bit integer (shard 2: 2, 5, 8, ...)",
    "snowflake": "64-bit time-ordered unique ID (timestamp + shard + sequence)",
    "uuid8": "Random 8-character alphanumeric string"
  },
  "shardInfo": { "shardId": 2, "totalShards": 3 },
  "snowflakeInfo": {
    "shardId": 2, "totalShards": 3, "epoch": "2024-01-01T00:00:00Z",
    "maxIdsPerMs": 4096, "maxShards": 1024,
    "bitsAllocation": { "timestamp": 41, "shardId": 10, "sequence": 12, "total": 64 }
  }
}
```

---

## GET /api/next-id/:entity

Mint one ID. **Auto-registers** the entity with the requested type on first call.

| Param | In | Notes |
|---|---|---|
| `entity` | path | Any non-empty string. |
| `dataType` | query | Required. One of the supported types. |

```bash
curl "http://localhost:7001/api/next-id/order?dataType=int"
```
```json
{ "entityName": "order", "dataType": "int", "value": 4, "shard": 2 }
```

**Errors**
- `400` — `dataType` missing or unrecognised.
- `400` — entity already registered with a different type (body includes `registeredType`).
- `500` — numeric entity has hit its type's maximum on this shard.

---

## GET /api/next-batch/:entity

Mint a batch in one round trip. The high-TPS path.

| Param | In | Notes |
|---|---|---|
| `entity` | path | Any non-empty string. |
| `dataType` | query | Required. |
| `batchSize` | query | Required. Integer `[1, 10000]`. |

```bash
curl "http://localhost:7001/api/next-batch/order?dataType=int&batchSize=5"
```
```json
{
  "entityName": "order", "dataType": "int", "batchSize": 5,
  "startValue": 6, "endValue": 10,
  "values": [6, 7, 8, 9, 10], "shard": 2
}
```

For numeric types the batch is a contiguous slice; the allocator reserves enough segments
inline (single durable write) to cover it. For `snowflake` the forge mints in a loop; for
`uuid*` each value is independently random.

**Errors**
- `400` — `batchSize` missing, non-numeric, `< 1`, or `> 10000`.
- `400` — type mismatch.
- `500` — batch would exceed a numeric type's maximum (no partial batch is returned).

---

## POST /api/init/:entity

Register an entity explicitly, optionally with a numeric start value. Refuses if it exists.
Use when you want to seed numbering or guarantee the type before any traffic.

Body (`application/json`):

| Field | Notes |
|---|---|
| `dataType` | Required. |
| `startValue` | Optional. Numeric types only. Snapped forward to the shard's residue. |

```bash
curl -X POST http://localhost:7001/api/init/invoice \
  -H 'Content-Type: application/json' \
  -d '{"dataType":"long","startValue":1000}'
```
```json
{
  "entityName": "invoice", "dataType": "long", "shard": 2, "initialized": true,
  "message": "Entity 'invoice' initialized successfully with type 'long'",
  "requestedStartValue": 1000, "actualStartValue": "1001",
  "adjusted": true, "adjustmentReason": "Value adjusted to match shard 2 pattern",
  "nextValue": "1001", "pattern": "2, 5, 8, ..."
}
```

**Errors**
- `409` — entity already exists (body includes its `dataType` and `currentIteration`).
- `400` — invalid `dataType`, or out-of-range `startValue`.

---

## PUT /api/reset/:entity

Move a numeric counter to a new value. Snaps the value to this shard's residue. **Numeric
types only.** Takes effect immediately — any in-RAM segment is dropped so the next ID comes
from the new mark.

Body (`application/json`): `{ "value": <number> }`

```bash
curl -X PUT http://localhost:7001/api/reset/order \
  -H 'Content-Type: application/json' \
  -d '{"value":5000}'
```
```json
{
  "entityName": "order", "dataType": "int", "shard": 2,
  "reset": {
    "previousValue": 11, "requestedValue": 5000, "actualValue": 5000,
    "previousIteration": 4, "newIteration": 1666
  },
  "nextValue": 5000,
  "message": "Counter reset successfully. Next value will be 5000"
}
```

**Errors**
- `404` — entity not registered (reset does not auto-create).
- `400` — entity is not numeric, or `value` missing / out of range.

---

## GET /api/status/:entity

Snapshot of one entity: type, current iteration, last emitted value, next value.

```bash
curl http://localhost:7001/api/status/order
```
```json
{
  "entityName": "order", "dataType": "int",
  "currentValue": 11, "nextValue": 14,
  "currentIteration": 333334, "shard": 2
}
```

For `snowflake`, `currentValue` reads `"N/A (Snowflake IDs are not sequential)"` and
`nextValue` is `null`. For `uuid*`, both are `null`.

**Errors**: `404` if the entity does not exist.

---

## GET /api/list

Every entity registered on this shard.

```bash
curl http://localhost:7001/api/list
```
```json
{
  "entities": [
    { "entityName": "order", "dataType": "int", "currentValue": 11, "currentIteration": 333334, "shard": 2 },
    { "entityName": "token", "dataType": "uuid8", "currentValue": null, "currentIteration": 0, "shard": 2 }
  ],
  "shardInfo": { "shardId": 2, "totalShards": 3 }
}
```

This is per-shard. For a cluster-wide view, query every shard's `/api/list` and merge.

---

## GET /api/segment-state/:entity

Diagnostic view of the in-RAM segment cache for a numeric entity. Not on the hot path.

```bash
curl http://localhost:7001/api/segment-state/order
```
```json
{
  "entityName": "order", "dataType": "int",
  "segment": {
    "size": 1000, "cursor": 742, "remaining": 258, "watermark": 0.9,
    "refillInFlight": false, "diskHighWaterMark": "2002"
  },
  "shard": 2
}
```

- `cursor` — IDs handed out from the current segment.
- `remaining` — IDs left before the next reservation.
- `refillInFlight` — true while the background refill is writing the next segment.
- `diskHighWaterMark` — the value most recently persisted; always ≥ any value handed out.

**Errors**: `404` if the entity does not exist; `400` if it is not a numeric type.

---

## GET /api/parse-snowflake/:id

Decode a 64-bit Snowflake ID into its parts. Works for IDs minted by any shard (pure
arithmetic).

```bash
curl http://localhost:7001/api/parse-snowflake/315240570203148288
```
```json
{
  "id": "315240570203148288",
  "timestamp": 1779226511668,
  "date": "2026-05-19T21:35:11.668Z",
  "shardId": 2,
  "sequence": 0,
  "binary": "0000010001011111..."
}
```

The decode uses the configured `snowflake_epoch_ms`; all nodes must share it or timestamps
disagree.

**Errors**: `400` if the path is not a valid 64-bit decimal string.
