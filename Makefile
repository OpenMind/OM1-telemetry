BIN       := rom1-telemetry
CMD       := ./cmd/main
BUILD_DIR := bin

.PHONY: all build run start clean fmt vet test

all: build

build:
	@mkdir -p $(BUILD_DIR)
	go build -o $(BUILD_DIR)/$(BIN) $(CMD)

## Run with go run. Override any value via environment variables, e.g.:
##   make run VIDEO_RTSP_URL=rtsp://192.168.1.10:8554/cam
run:
	go run $(CMD)

start: build
	./$(BUILD_DIR)/$(BIN)

fmt:
	go fmt ./...

vet:
	go vet ./...

test:
	go test ./...

clean:
	rm -rf $(BUILD_DIR) recordings
