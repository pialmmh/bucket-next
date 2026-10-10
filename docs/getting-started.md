# Getting started

From a clean checkout to a running service handing out IDs.

## Prerequisites

- **Go 1.21+** to build (`go version`). The binary itself has no runtime dependency.
- `curl` for testing.

## 1. Clone and build

```bash
git clone git@github.com:pialmmh/bucket-next.git
cd bucket-next
make build
```

`make build` always rebuilds and produces:

- `bin/bucket-next` — the HTTP service
- `bin/bucket-next-cli` — offline maintenance tool

## 2. Write a config

The service reads exactly one YAML file. On a single box, one shard is all you need:

```bash
cat > my.yaml <<'EOF'
shard_id: 1
total_shards: 1
listen_address: 127.0.0.1    # loopback only — the API has no authentication
listen_port: 7001
state_path: ./data/state.json
EOF
```

See [configuration.md](configuration.md) for every key (including `uuid_key`, which you
should set before using `uuid*` types in production).

## 3. First start — with `-init`

A brand-new shard has no state file yet. Start it **once** with `-init` to create one:

```bash
./bin/bucket-next -config my.yaml -init
```

```
created new state file ./data/state.json for shard 1/1
bucket-next listening on 127.0.0.1:7001  shard=1/1  state=./data/state.json
```

Every later start is **without** `-init`. If the state file is ever missing, the service
refuses to start instead of starting from zero (which would re-issue old IDs). `-init` only
creates a file that does not exist; it never touches an existing one.

> A `WARNING: uuid_key is not set` line is expected in a quick test. It means uuid IDs are
> unique but predictable. Set `uuid_key` before production use.

## 4. Get IDs

In a second terminal:

```bash
curl http://localhost:7001/health

# scalar — one sequential integer for entity "order"
curl "http://localhost:7001/api/next-id/order?dataType=int"
# {"dataType":"int","entityName":"order","shard":1,"value":1}

curl "http://localhost:7001/api/next-id/order?dataType=int"
# {"dataType":"int","entityName":"order","shard":1,"value":2}

# vector — a range of 5 in one call
curl "http://localhost:7001/api/next-batch/order?dataType=int&batchSize=5"
# {"batchSize":5,"startValue":3,"endValue":7,"values":[3,4,5,6,7],
#  "range":{"first":3,"last":7,"step":1,"count":5}, ...}

# a unique 12-character string for entity "token"
curl "http://localhost:7001/api/next-id/token?dataType=uuid12"
# {"dataType":"uuid12","entityName":"token","shard":1,"value":"3KpZ8qWbT0xa"}

# a time-ordered 64-bit Snowflake for entity "event"
curl "http://localhost:7001/api/next-id/event?dataType=snowflake"
# {"dataType":"snowflake","entityName":"event","shard":1,"value":"315240570203148288"}
```

Each entity registered itself on the first call and is now bound to that data type.

## 5. Stop it

`Ctrl-C`. The service drains in-flight requests and waits for any pending disk reservation.

## Next steps

- More recipes: [usage-examples.md](usage-examples.md)
- Full endpoint list: [api-reference.md](api-reference.md)
- Deploy to real servers: [deployment.md](deployment.md)

## Run the test suite

```bash
make test     # go test -race ./...  (includes concurrency stress tests)
make vet
```
