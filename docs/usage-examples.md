# Usage examples

Practical recipes. Assumes a service running on `localhost:7001` (see
[getting-started.md](getting-started.md)).

## Pick a data type

| You need… | Use | Example value |
|---|---|---|
| Human-readable sequential key, fits 32-bit | `int` | `42` |
| Human-readable sequential key, large | `long` | `"1000004"` |
| Time-ordered opaque 64-bit key (DB primary key at scale) | `snowflake` | `"315240570203148288"` |
| Short opaque token | `uuid8` / `uuid12` / `uuid16` / `uuid22` | `"36xCpcN1"` |

Each entity is bound to **one** type on first use. That binding is permanent.

## Sequential integers

```bash
curl "http://localhost:7001/api/next-id/order?dataType=int"
# {"dataType":"int","entityName":"order","shard":1,"value":1}
curl "http://localhost:7001/api/next-id/order?dataType=int"
# value: 2
```

On a single shard (`total_shards: 1`) you get `1, 2, 3, …`. On shard *k* of *N* you get
`k, k+N, k+2N, …` — see [Multi-shard numbering](#multi-shard-numbering).

## Large sequential (long)

```bash
curl "http://localhost:7001/api/next-id/cdr?dataType=long"
# {"dataType":"long","entityName":"cdr","shard":1,"value":"1"}
```

Note `value` is a **string** for `long`. Parse it as a 64-bit integer client-side.

## Random short strings (uuid)

```bash
curl "http://localhost:7001/api/next-id/session?dataType=uuid16"
# {"dataType":"uuid16","entityName":"session","shard":1,"value":"oQeLIGjRPZGKCo5G"}
```

`uuid8`, `uuid12`, `uuid16`, `uuid22` differ only in length. They are crypto-random, not
shard-embedded; uniqueness is statistical.

## Time-ordered Snowflake

```bash
ID=$(curl -s "http://localhost:7001/api/next-id/event?dataType=snowflake" | jq -r .value)
echo "$ID"
# 315240570203148288

# Decode it back
curl -s "http://localhost:7001/api/parse-snowflake/$ID"
# {"id":"...","timestamp":...,"date":"2026-05-19T21:35:11.668Z","shardId":1,"sequence":0,...}
```

## Batches for high throughput

One request, up to 10 000 IDs:

```bash
curl "http://localhost:7001/api/next-batch/sms?dataType=long&batchSize=5000"
# {"batchSize":5000,"startValue":"1","endValue":"5000","values":["1","2",...],"shard":1}
```

For numeric types the batch is contiguous (`startValue`..`endValue`). Issue ten requests of
10 000 to get 100 000.

## Seed a starting value

Use `init` to register before any traffic and set where numbering starts:

```bash
curl -X POST http://localhost:7001/api/init/invoice \
  -H 'Content-Type: application/json' \
  -d '{"dataType":"long","startValue":1000}'
```

On a single shard the next value is exactly `1000`. On a multi-shard cluster the value is
snapped forward to the shard's residue (response shows `adjusted: true`).

## Reset a counter

```bash
curl -X PUT http://localhost:7001/api/reset/order \
  -H 'Content-Type: application/json' \
  -d '{"value":5000}'
```

Numeric types only. Takes effect immediately.

## Inspect state

```bash
curl http://localhost:7001/api/status/order      # one entity
curl http://localhost:7001/api/list              # all entities
curl http://localhost:7001/api/segment-state/order   # RAM cache fill (numeric only)
```

## Multi-shard numbering

With three shards, each node owns a residue class — no coordination, no collisions:

| Shard | `int`/`long` values |
|---|---|
| 1 of 3 | 1, 4, 7, 10, … |
| 2 of 3 | 2, 5, 8, 11, … |
| 3 of 3 | 3, 6, 9, 12, … |

Your client picks any shard (round-robin, nearest, whatever); every ID is globally unique.
Snowflake and uuid types are also collision-free across shards. See
[deployment.md](deployment.md) to stand up the cluster.

## Gaps and crash behaviour

For `int`/`long`, the service reserves a block of `segment_size` IDs (default 1000) on disk
in one write, then serves from RAM. Consequences:

- After handing out 10 IDs, `/api/list` may show `currentIteration: 1000`. That is the
  reservation — expected, not a leak.
- A hard crash (kill -9, power loss) forfeits the **unused tail** of the current block. Max
  gap = `segment_size`. The sequence jumps forward; it never repeats a value.
- If gaps bother you for a low-volume entity, lower `segment_size` (e.g. to 10) in the config.

Snowflake and uuid types have no such reservation — they compute each value on demand.

## Language snippets

### bash + jq

```bash
next_id() { curl -s "http://localhost:7001/api/next-id/$1?dataType=$2" | jq -r .value; }
order_id=$(next_id order long)
```

### Python

```python
import requests

def next_id(entity, dtype, base="http://localhost:7001"):
    r = requests.get(f"{base}/api/next-id/{entity}", params={"dataType": dtype})
    r.raise_for_status()
    return r.json()["value"]   # str for long/snowflake/uuid, int for int

order_id = next_id("order", "long")

def next_batch(entity, dtype, n, base="http://localhost:7001"):
    r = requests.get(f"{base}/api/next-batch/{entity}",
                     params={"dataType": dtype, "batchSize": n})
    r.raise_for_status()
    return r.json()["values"]

ids = next_batch("sms", "long", 5000)
```

### Go

```go
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

func nextID(base, entity, dtype string) (any, error) {
	u := fmt.Sprintf("%s/api/next-id/%s?dataType=%s", base, entity, url.QueryEscape(dtype))
	resp, err := http.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct{ Value any `json:"value"` }
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Value, nil
}
```

### Node.js

```js
async function nextId(entity, dtype, base = "http://localhost:7001") {
  const r = await fetch(`${base}/api/next-id/${entity}?dataType=${dtype}`);
  if (!r.ok) throw new Error(`bucket-next ${r.status}`);
  return (await r.json()).value;   // string for long/snowflake/uuid
}
```
