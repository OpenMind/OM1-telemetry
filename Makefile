BIN       := rom1-telemetry
CMD       := ./cmd/main
BUILD_DIR := bin

.PHONY: all build run start test

all: build

build:
	@mkdir -p $(BUILD_DIR)
	go build -o $(BUILD_DIR)/$(BIN) $(CMD)

run:
	go run $(CMD)

start: build
	./$(BUILD_DIR)/$(BIN)

test:
	go test ./...

lint:
	golangci-lint run
