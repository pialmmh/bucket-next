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
state_dir: ./data/shard-1
tenants: [btcl]              # the tenants this installation serves; every call names one
EOF
```

See [configuration.md](configuration.md) for every key (including `uuid_key`, which you
should set before using `uuid*` types in production).

## 3. First start — with `-init`

A brand-new shard has no state yet. Start it **once** with `-init` to create it:

```bash
./bin/bucket-next -config my.yaml -init
```

```
state ./data/shard-1: create the state of a brand-new shard 1/1
state ./data/shard-1: create tenant btcl: declared, not on the roster yet
bucket-next listening on 127.0.0.1:7001  shard=1/1  tenants=btcl  legacy_tenant=""  state=./data/shard-1
```

Every later start is **without** `-init`. If the state is ever missing, the service refuses to start instead of
starting from zero (which would re-issue old IDs):

```
state not found: state_dir ./data/shard-1 does not exist
Refusing to start: starting from zero would re-issue IDs.
If this is a brand-new shard, start it once with -init (or run: bucket-next-cli -config my.yaml init-state).
If the state was lost, restore it from backup instead.
```

`-init` only creates state that does not exist; it never touches existing state. A tenant added to `tenants` later
needs no `-init`: the shard creates its file at the next start.

> A `WARNING: uuid_key is not set` line is expected in a quick test. It means uuid IDs are
> unique but predictable. Set `uuid_key` before production use.

## 4. Get IDs

In a second terminal:

```bash
curl http://localhost:7001/health

# scalar — one sequential integer for entity "order"
curl "http://localhost:7001/api/v2/btcl/next-id/order?dataType=int"
# {"dataType":"int","entityName":"order","shard":1,"tenant":"btcl","value":1}

curl "http://localhost:7001/api/v2/btcl/next-id/order?dataType=int"
# {"dataType":"int","entityName":"order","shard":1,"tenant":"btcl","value":2}

# vector — a range of 5 in one call
curl "http://localhost:7001/api/v2/btcl/next-batch/order?dataType=int&batchSize=5"
# {"batchSize":5,"dataType":"int","endValue":7,"entityName":"order","range":{"first":3,"last":7,"step":1,"count":5},
#  "shard":1,"startValue":3,"tenant":"btcl","values":[3,4,5,6,7]}

# a tenant this installation does not serve is refused, in words — never a default tenant
curl "http://localhost:7001/api/v2/nobody/next-id/order?dataType=int"
# {"error":"Unknown tenant","message":"tenant 'nobody' is not served by this installation (served tenants: btcl)"}

# a unique 12-character string for entity "token"
curl "http://localhost:7001/api/v2/btcl/next-id/token?dataType=uuid12"
# {"dataType":"uuid12","entityName":"token","shard":1,"tenant":"btcl","value":"3KpZ8qWbT0xa"}

# a time-ordered 64-bit Snowflake for entity "event"
curl "http://localhost:7001/api/v2/btcl/next-id/event?dataType=snowflake"
# {"dataType":"snowflake","entityName":"event","shard":1,"tenant":"btcl","value":"315240570203148288"}
```

Each entity registered itself in tenant `btcl` on the first call and is now bound to that data type.

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
