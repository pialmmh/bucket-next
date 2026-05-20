.PHONY: build run test vet tidy clean

BIN_DIR := bin
SERVICE := $(BIN_DIR)/bucket-next
CLI     := $(BIN_DIR)/bucket-next-cli

build: $(SERVICE) $(CLI)

$(SERVICE):
	mkdir -p $(BIN_DIR)
	go build -o $(SERVICE) ./cmd/bucket-next

$(CLI):
	mkdir -p $(BIN_DIR)
	go build -o $(CLI) ./cmd/bucket-next-cli

run: $(SERVICE)
	$(SERVICE) -config configs/sample.yaml

tidy:
	go mod tidy

vet:
	go vet ./...

test:
	go test ./...

clean:
	rm -rf $(BIN_DIR) data
