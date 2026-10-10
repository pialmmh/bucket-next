# Usage examples

Practical recipes. Assumes a service on `localhost:7001` (see
[getting-started.md](getting-started.md)).

## Pick a data type

| You need… | Use | Example value |
|---|---|---|
| Human-readable sequential key, fits 32-bit | `int` | `42` |
| Human-readable sequential key, large | `long` | `"1000004"` |
| Time-ordered 64-bit key | `snowflake` | `"315240570203148288"` |
| Short opaque string (call ID, SMS ID, token) | `uuid8` / `uuid12` / `uuid16` / `uuid22` | `"0CIOE46z"` |

Each entity is bound to **one** type on first use, for life.

## Scalar vs vector — the two versions of every type

| Version | Endpoint | Returns | Use when |
|---|---|---|---|
| Scalar | `GET /api/next-id/{entity}?dataType=T` | one ID | one record at a time |
| Vector | `GET /api/next-batch/{entity}?dataType=T&batchSize=N` | N IDs (1..10 000) | bulk / high TPS — one round trip per block |

Both draw from the same counter, so mixing them never repeats an ID:

```bash
curl "localhost:7001/api/next-id/x?dataType=int"                  # 1
curl "localhost:7001/api/next-batch/x?dataType=int&batchSize=3"   # 2, 3, 4
curl "localhost:7001/api/next-id/x?dataType=int"                  # 5
```

## Sequential integers

```bash
curl "localhost:7001/api/next-id/order?dataType=int"
# {"dataType":"int","entityName":"order","shard":1,"value":1}
```

Single shard (`total_shards: 1`): `1, 2, 3, …`. Shard *k* of *N*: `k, k+N, k+2N, …` — see
[Multi-shard numbering](#multi-shard-numbering).

## Large sequential (long)

```bash
curl "localhost:7001/api/next-id/cdr?dataType=long"
# {"dataType":"long","entityName":"cdr","shard":1,"value":"1"}
```

`value` is a **string** for `long`; parse it as a 64-bit integer.

## A range of numbers in one call

```bash
curl "localhost:7001/api/next-batch/cdr?dataType=long&batchSize=5000"
# {..., "startValue":"2","endValue":"14999",
#  "range":{"first":"2","last":"14999","step":3,"count":5000}, "values":[...]}
```

For `int`/`long` the vector is contiguous on this shard. A client that only needs the block can
store `range` alone: value *i* = `first + i × step`.

## Unique short strings (uuid)

```bash
curl "localhost:7001/api/next-id/session?dataType=uuid16"
# {"dataType":"uuid16","entityName":"session","shard":1,"value":"oKs4845oCMOV3Qhm"}
curl "localhost:7001/api/next-batch/sms-id?dataType=uuid8&batchSize=1000"
```

`uuid8/12/16/22` differ only in length. They are **not random**: each is the entity's counter
(shard-interleaved, like `long`) passed through a keyed permutation. So they are fixed-length,
look random, and **never repeat within an entity** — across shards, under load and after
crashes.

- Set the same `uuid_key` on every shard before the first uuid ID (see
  [configuration.md](configuration.md#uuid_key--set-before-the-first-uuid-id)).
- Different entities get unrelated sequences. If strings must be unique across several
  entities, draw them from **one** entity (guaranteed); across entities it is only statistical.
- `uuid8` holds 218 trillion values per entity; the longer ones 9.2 × 10^18.

## Time-ordered Snowflake

```bash
ID=$(curl -s "localhost:7001/api/next-id/event?dataType=snowflake" | jq -r .value)
curl -s "localhost:7001/api/parse-snowflake/$ID"
# {"id":"...","timestamp":...,"date":"2026-05-19T21:35:11.668Z","shardId":1,"sequence":0,...}
```

## Seed a starting value

```bash
curl -X POST localhost:7001/api/init/invoice \
  -H 'Content-Type: application/json' -d '{"dataType":"long","startValue":1000}'
```

Single shard: the next value is exactly `1000`. Multi-shard: snapped forward to the shard's
residue (`"adjusted": true`). Allowed for `int`/`long` only; the value must be ≥ 1.

**Counters are per shard.** `init`, `reset` and `status` act only on the shard you call. To put an
entity's counter above IDs that already exist, do it on **every** shard that mints for that entity
(each one snaps the value to its own residue).

## Move a counter

```bash
# forward — always allowed
curl -X PUT localhost:7001/api/reset/order -H 'Content-Type: application/json' -d '{"value":5000}'

# backward — refused (409), because IDs could repeat
curl -X PUT localhost:7001/api/reset/order -H 'Content-Type: application/json' -d '{"value":2}'

# backward on purpose (you know those IDs are unused)
curl -X PUT localhost:7001/api/reset/order -H 'Content-Type: application/json' -d '{"value":2,"force":true}'
```

## Inspect state

```bash
curl localhost:7001/api/status/order          # one entity
curl localhost:7001/api/list                  # all entities
curl localhost:7001/api/segment-state/order   # RAM reservation (int/long/uuid*)
curl localhost:7001/shard-info                # identity; compare across shards
```

## Multi-shard numbering

Three shards, each owning a residue class — no coordination, no collisions:

| Shard | `int`/`long` values |
|---|---|
| 1 of 3 | 1, 4, 7, 10, … |
| 2 of 3 | 2, 5, 8, 11, … |
| 3 of 3 | 3, 6, 9, 12, … |

A client may call any shard (round-robin, nearest…); every ID is unique for its entity across
the cluster. `uuid*` follow the same rule; `snowflake` carries the shard id in its bits.

## Gaps and crash behaviour

Counter types reserve `segment_size` IDs (default 1000) on disk in one durable write
(fsync), then serve them from RAM:

- After 10 IDs, `currentIteration` may already read 1000 — that is the reservation.
- A hard crash (kill -9, power loss) forfeits the **unused tail** of the reservation. The
  sequence jumps forward (gap ≤ `segment_size`); it never repeats. Writes are fsynced, so a
  power loss cannot roll the counter back.
- `snowflake` saves a time mark ~1 s ahead; after a restart the service waits until the clock
  passes it (≤ ~1 s), so no Snowflake repeats either.
- Lower `segment_size` for a low-volume entity if gaps look untidy.

## Language snippets

### bash + jq

```bash
next_id()  { curl -s "http://localhost:7001/api/next-id/$1?dataType=$2" | jq -r .value; }
next_ids() { curl -s "http://localhost:7001/api/next-batch/$1?dataType=$2&batchSize=$3" | jq -r '.values[]'; }
order_id=$(next_id order long)
```

### Python

```python
import requests

BASE = "http://localhost:7001"

def next_id(entity, dtype):
    r = requests.get(f"{BASE}/api/next-id/{entity}", params={"dataType": dtype})
    r.raise_for_status()
    return r.json()["value"]   # str for long/snowflake/uuid*, int for int

def next_ids(entity, dtype, n):
    r = requests.get(f"{BASE}/api/next-batch/{entity}", params={"dataType": dtype, "batchSize": n})
    r.raise_for_status()
    return r.json()["values"]

order_id = next_id("order", "long")
sms_ids = next_ids("sms-id", "uuid12", 5000)
```

### Go

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

### Node.js

```js
async function nextId(entity, dtype, base = "http://localhost:7001") {
  const r = await fetch(`${base}/api/next-id/${entity}?dataType=${dtype}`);
  if (!r.ok) throw new Error(`bucket-next ${r.status}`);
  return (await r.json()).value;   // string for long/snowflake/uuid*
}
```
