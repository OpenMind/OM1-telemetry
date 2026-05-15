.PHONY: build run download-zenohc test tidy

BIN       := rom1-telemetry
CMD       := ./cmd/main
BUILD_DIR := bin

ZENOH_C_VERSION=1.9.0
ZENOH_C_DIR=.zenoh-c
ZENOH_C_ABS_DIR=$(shell pwd)/$(ZENOH_C_DIR)
UNAME_S := $(shell uname -s)
UNAME_M := $(shell uname -m)

ifeq ($(UNAME_S),Linux)
	ifeq ($(UNAME_M),x86_64)
		ZENOH_PLATFORM=x86_64-unknown-linux-gnu
	else ifeq ($(UNAME_M),aarch64)
		ZENOH_PLATFORM=aarch64-unknown-linux-gnu
	endif
	DYLD_VAR=LD_LIBRARY_PATH
else ifeq ($(UNAME_S),Darwin)
	ifeq ($(UNAME_M),arm64)
		ZENOH_PLATFORM=aarch64-apple-darwin
	else
		ZENOH_PLATFORM=x86_64-apple-darwin
	endif
	DYLD_VAR=DYLD_LIBRARY_PATH
endif

ZENOH_URL=https://github.com/eclipse-zenoh/zenoh-c/releases/download/$(ZENOH_C_VERSION)/zenoh-c-$(ZENOH_C_VERSION)-$(ZENOH_PLATFORM)-standalone.zip

export CGO_ENABLED=1
export CGO_CFLAGS=-I$(ZENOH_C_ABS_DIR)/include
export CGO_LDFLAGS=-L$(ZENOH_C_ABS_DIR)/lib -lzenohc -Wl,-rpath,$(ZENOH_C_ABS_DIR)/lib


build:
	@mkdir -p $(BUILD_DIR)
	go build -o $(BUILD_DIR)/$(BIN) $(CMD)

run:
	go run $(CMD)

download-zenohc:
	@echo "Downloading zenoh-c $(ZENOH_C_VERSION) for $(ZENOH_PLATFORM)..."
	@mkdir -p $(ZENOH_C_DIR)
	@if [ ! -f "$(ZENOH_C_DIR)/lib/libzenohc.dylib" ] && [ ! -f "$(ZENOH_C_DIR)/lib/libzenohc.so" ]; then \
		echo "Fetching $(ZENOH_URL)..."; \
		curl -sSL -o /tmp/zenoh-c.zip $(ZENOH_URL); \
		unzip -q /tmp/zenoh-c.zip -d $(ZENOH_C_DIR); \
		rm /tmp/zenoh-c.zip; \
		echo "zenoh-c installed to $(ZENOH_C_DIR)"; \
		if [ "$(UNAME_S)" = "Darwin" ]; then \
			echo "Patching dylib install names..."; \
			if [ -f "$(ZENOH_C_ABS_DIR)/lib/libzenohc.dylib" ]; then \
				install_name_tool -id "@rpath/libzenohc.dylib" "$(ZENOH_C_ABS_DIR)/lib/libzenohc.dylib"; \
			fi; \
		fi; \
	else \
		echo "zenoh-c already installed in $(ZENOH_C_DIR)"; \
	fi

test: download-zenohc
	$(DYLD_VAR)=$(ZENOH_C_ABS_DIR)/lib go test -p 8 -v ./...

lint: download-zenohc
	$(DYLD_VAR)=$(ZENOH_C_ABS_DIR)/lib golangci-lint run --timeout=5m

tidy:
	go mod tidy
	go mod verify
