.PHONY: build run check-cyclonedds idl-gen test tidy lint

BIN       := om1-telemetry
CMD       := ./cmd/main
BUILD_DIR := bin
IDL_DIR   := idl
IDL_GEN_DIR := internal/ddsgen

export CGO_ENABLED=1

# CycloneDDS (libddsc) is a build/runtime dependency of internal/ddscore and
# every stream's dds_reader.go — unlike zenoh-c there is no prebuilt
# per-platform archive to fetch, so it must be installed via package manager
# or built from source before `make build`/`make test` will work:
#
#   Debian/Ubuntu: apt install libcyclonedds-dev cyclonedds-tools
#   macOS (brew):  brew install cyclonedds       (formula may need --HEAD)
#   from source:   https://github.com/eclipse-cyclonedds/cyclonedds
#
# `cyclonedds-tools` / the from-source build both provide `idlc`, needed by
# `idl-gen`. See README's "Build prerequisites" section.
check-cyclonedds:
	@command -v pkg-config >/dev/null 2>&1 || { echo "pkg-config not found"; exit 1; }
	@pkg-config --exists CycloneDDS || { \
		echo "CycloneDDS not found via pkg-config. Install libddsc (see Makefile comments) before building."; \
		exit 1; \
	}

# Regenerates the idlc type-support C sources for every message type this
# recorder subscribes to, from idl/*.idl, into $(IDL_GEN_DIR). Must be rerun
# whenever an .idl file changes; the generated *.c/*.h are gitignored.
idl-gen: check-cyclonedds
	@command -v idlc >/dev/null 2>&1 || { echo "idlc not found (part of cyclonedds-tools / a from-source build)"; exit 1; }
	@mkdir -p $(IDL_GEN_DIR)
	@for f in $(IDL_DIR)/*.idl; do \
		echo "idlc $$f"; \
		idlc -l c -I $(IDL_DIR) -o $(IDL_GEN_DIR) "$$f" || exit 1; \
	done

build: idl-gen
	@mkdir -p $(BUILD_DIR)
	go build -o $(BUILD_DIR)/$(BIN) $(CMD)

run: idl-gen
	go run $(CMD)

test: idl-gen
	go test -p 8 -v ./...

lint: idl-gen
	golangci-lint run --timeout=5m

tidy:
	go mod tidy
	go mod verify
