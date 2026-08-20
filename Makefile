.PHONY: build run install-cyclonedds check-cyclonedds install-draco idl-gen test tidy lint

BIN       := om1-telemetry
CMD       := ./cmd/main
BUILD_DIR := bin
IDL_DIR   := idl
IDL_GEN_DIR := internal/ddsgen

export CGO_ENABLED=1

CYCLONEDDS_VERSION := releases/0.10.x
CYCLONEDDS_DIR      := .cyclonedds
CYCLONEDDS_SRC       := $(CYCLONEDDS_DIR)/src
CYCLONEDDS_INSTALL   := $(shell pwd)/$(CYCLONEDDS_DIR)/install

export PATH := $(CYCLONEDDS_INSTALL)/bin:$(PATH)
export PKG_CONFIG_PATH := $(CYCLONEDDS_INSTALL)/lib/pkgconfig:$(PKG_CONFIG_PATH)
export LD_LIBRARY_PATH := $(CYCLONEDDS_INSTALL)/lib:$(LD_LIBRARY_PATH)
export DYLD_LIBRARY_PATH := $(CYCLONEDDS_INSTALL)/lib:$(DYLD_LIBRARY_PATH)

install-cyclonedds:
	@if [ ! -f "$(CYCLONEDDS_INSTALL)/lib/pkgconfig/CycloneDDS.pc" ]; then \
		set -e; \
		echo "Building CycloneDDS ($(CYCLONEDDS_VERSION)) into $(CYCLONEDDS_INSTALL)..."; \
		rm -rf $(CYCLONEDDS_SRC); \
		git clone --branch $(CYCLONEDDS_VERSION) --depth 1 \
			https://github.com/eclipse-cyclonedds/cyclonedds.git $(CYCLONEDDS_SRC); \
		mkdir -p $(CYCLONEDDS_SRC)/build; \
		cd $(CYCLONEDDS_SRC)/build && \
			cmake -DBUILD_EXAMPLES=OFF -DBUILD_TESTING=OFF -DENABLE_ICEORYX=NO \
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

export CGO_LDFLAGS := -Wl,-rpath,$(CYCLONEDDS_INSTALL)/lib

# draco_encoder is a *runtime* dependency (internal/upload's pointcloud
# compression step shells out to it -- see compress_pointcloud.go), not a
# build/cgo one like CycloneDDS: nothing here links against it, the
# recorder just needs to find it on PATH when it runs. So, unlike
# CycloneDDS, no build/run/test target depends on this one -- run it
# yourself if you want to exercise pointcloud compression locally; the
# Dockerfile always builds it into the image.
DRACO_VERSION := 1.5.7
DRACO_DIR     := .draco
# Absolute, unlike CYCLONEDDS_SRC: install-draco's recipe cd's into
# $(DRACO_SRC)/build to run cmake (Draco's build has no "install" target
# that places draco_encoder into a prefix the way CycloneDDS's does, so the
# recipe cp's it out manually afterward) -- a relative path here would
# resolve against that new cwd instead of the original one for anything
# referencing it after the cd.
DRACO_SRC     := $(shell pwd)/$(DRACO_DIR)/src
DRACO_INSTALL := $(shell pwd)/$(DRACO_DIR)/install

export PATH := $(DRACO_INSTALL)/bin:$(PATH)

install-draco:
	@if [ ! -x "$(DRACO_INSTALL)/bin/draco_encoder" ]; then \
		set -e; \
		echo "Building draco_encoder ($(DRACO_VERSION)) into $(DRACO_INSTALL)..."; \
		rm -rf $(DRACO_SRC); \
		git clone --branch $(DRACO_VERSION) --depth 1 \
			https://github.com/google/draco.git $(DRACO_SRC); \
		mkdir -p $(DRACO_SRC)/build; \
		cd $(DRACO_SRC)/build && cmake -DCMAKE_BUILD_TYPE=Release .. && cmake --build . --target draco_encoder -j; \
		mkdir -p $(DRACO_INSTALL)/bin; \
		cp $(DRACO_SRC)/build/draco_encoder $(DRACO_INSTALL)/bin/; \
		echo "draco_encoder installed to $(DRACO_INSTALL)/bin"; \
	else \
		echo "draco_encoder already installed in $(DRACO_INSTALL)/bin"; \
	fi

idl-gen: check-cyclonedds
	@mkdir -p $(IDL_GEN_DIR)
	@for f in $(IDL_DIR)/*.idl; do \
		echo "idlc $$f"; \
		idlc -l c -x final -I $(IDL_DIR) -o $(IDL_GEN_DIR) "$$f" || exit 1; \
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
