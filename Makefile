.PHONY: build service cli run test vet tidy clean

BIN_DIR := bin

# Always rebuild (go build is incremental). A file target here would skip the build when
# bin/ already exists and leave a stale binary for `remote-deploy-v2.sh --skip-build` to ship.
build: service cli

service:
	mkdir -p $(BIN_DIR)
	go build -o $(BIN_DIR)/bucket-next ./cmd/bucket-next

cli:
	mkdir -p $(BIN_DIR)
	go build -o $(BIN_DIR)/bucket-next-cli ./cmd/bucket-next-cli

# Local single-shard run; -init creates ./data/state.json on the first start only.
run: service
	$(BIN_DIR)/bucket-next -config configs/sample.yaml -init

tidy:
	go mod tidy

vet:
	go vet ./...

test:
	go test -race ./...

clean:
	rm -rf $(BIN_DIR) data
