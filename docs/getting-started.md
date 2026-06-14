# Getting started

This gets you from a clean checkout to a running service handing out IDs.

## Prerequisites

- **Go 1.21+** to build (`go version` to check). The built binary has no runtime
  dependency — it is a single static executable.
- A POSIX shell, `curl` for testing.

## 1. Clone and build

```bash
git clone git@github.com:pialmmh/bucket-next.git
cd bucket-next
make build
```

`make build` produces two binaries:

- `bin/bucket-next` — the HTTP service
- `bin/bucket-next-cli` — offline maintenance tool

(If you do not have `make`, run `go build -o bin/bucket-next ./cmd/bucket-next` and
`go build -o bin/bucket-next-cli ./cmd/bucket-next-cli`.)

## 2. Write a config

The service reads exactly one YAML file. For a single-box setup, one shard is all you need:

```bash
cat > my.yaml <<'EOF'
shard_id: 1
total_shards: 1
listen_port: 7001
state_path: ./data/state.json
EOF
```

`state_path`'s parent directory is created automatically. See
[configuration.md](configuration.md) for every key.

## 3. Run it

```bash
./bin/bucket-next -config my.yaml
```

You will see:

```
bucket-next listening on :7001  shard=1/1  state=./data/state.json
```

The service runs in the foreground. Leave it running; open a second terminal for the next step.

## 4. Get IDs

```bash
# Health check
curl http://localhost:7001/health

# A sequential integer for entity "order"
curl "http://localhost:7001/api/next-id/order?dataType=int"
# {"dataType":"int","entityName":"order","shard":1,"value":1}

# Call it again — the counter advances
curl "http://localhost:7001/api/next-id/order?dataType=int"
# {"dataType":"int","entityName":"order","shard":1,"value":2}

# A random short string for entity "token"
curl "http://localhost:7001/api/next-id/token?dataType=uuid8"
# {"dataType":"uuid8","entityName":"token","shard":1,"value":"36xCpcN1"}

# A time-ordered 64-bit Snowflake for entity "event"
curl "http://localhost:7001/api/next-id/event?dataType=snowflake"
# {"dataType":"snowflake","entityName":"event","shard":1,"value":"315240570203148288"}
```

That is it — the service is usable. The entity auto-registered on the first call and is now
bound to its data type.

## 5. Stop it

Press `Ctrl-C` in the service terminal. It shuts down gracefully (drains in-flight requests,
flushes any pending state reservation).

## Next steps

- More recipes: [usage-examples.md](usage-examples.md)
- Full endpoint list: [api-reference.md](api-reference.md)
- Deploy to real servers: [deployment.md](deployment.md)

## Run the test suite (optional)

```bash
go test ./...     # all 7 internal packages
go vet ./...      # static checks
```
