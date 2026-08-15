.PHONY: build run install-cyclonedds check-cyclonedds idl-gen test tidy lint

BIN       := om1-telemetry
CMD       := ./cmd/main
BUILD_DIR := bin
IDL_DIR   := idl
IDL_GEN_DIR := internal/ddsgen

export CGO_ENABLED=1

# CycloneDDS (libddsc + idlc) is a build/runtime dependency of
# internal/ddscore and every stream's dds_reader.go — unlike zenoh-c there is
# no prebuilt per-platform archive, so it's built from source here (same
# eclipse-cyclonedds/cyclonedds repo everyone on the team builds locally),
# rather than relying on distro packages that vary release-to-release.
CYCLONEDDS_VERSION := releases/0.10.x
CYCLONEDDS_DIR      := .cyclonedds
CYCLONEDDS_SRC       := $(CYCLONEDDS_DIR)/src
CYCLONEDDS_INSTALL   := $(shell pwd)/$(CYCLONEDDS_DIR)/install

export PATH := $(CYCLONEDDS_INSTALL)/bin:$(PATH)
export PKG_CONFIG_PATH := $(CYCLONEDDS_INSTALL)/lib/pkgconfig:$(PKG_CONFIG_PATH)

install-cyclonedds:
	@if [ ! -f "$(CYCLONEDDS_INSTALL)/lib/pkgconfig/CycloneDDS.pc" ]; then \
		set -e; \
		echo "Building CycloneDDS ($(CYCLONEDDS_VERSION)) into $(CYCLONEDDS_INSTALL)..."; \
		rm -rf $(CYCLONEDDS_SRC); \
		git clone --branch $(CYCLONEDDS_VERSION) --depth 1 \
			https://github.com/eclipse-cyclonedds/cyclonedds.git $(CYCLONEDDS_SRC); \
		mkdir -p $(CYCLONEDDS_SRC)/build; \
		cd $(CYCLONEDDS_SRC)/build && \
			cmake -DBUILD_EXAMPLES=OFF -DBUILD_TESTING=OFF \
			      -DCMAKE_INSTALL_PREFIX=$(CYCLONEDDS_INSTALL) .. && \
			cmake --build . --target install; \
		echo "CycloneDDS installed to $(CYCLONEDDS_INSTALL)"; \
	else \
		echo "CycloneDDS already installed in $(CYCLONEDDS_INSTALL)"; \
	fi

check-cyclonedds: install-cyclonedds
	@command -v pkg-config >/dev/null 2>&1 || { echo "pkg-config not found"; exit 1; }
	@pkg-config --exists CycloneDDS || { \
		echo "CycloneDDS not found via pkg-config after install-cyclonedds — something went wrong."; \
		exit 1; \
	}

# libddsc's own .pc file doesn't embed an rpath, so binaries built against
# this non-system install prefix fail at runtime with "Library not loaded:
# @rpath/libddsc..." unless one is added explicitly.
export CGO_LDFLAGS := -Wl,-rpath,$(CYCLONEDDS_INSTALL)/lib

# Regenerates the idlc type-support C sources for every message type this
# recorder subscribes to, from idl/*.idl, into $(IDL_GEN_DIR). Must be rerun
# whenever an .idl file changes; the generated *.c/*.h are gitignored.
idl-gen: check-cyclonedds
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
