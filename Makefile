# Makefile for NanoKVM Project

# Configuration
IMAGE_NAME := nanokvm-builder            # lean: Go + host-tools (builds the server)
FULL_IMAGE := nanokvm-builder-full       # adds the MaixCDK SDK (regenerates libkvm)
UID := $(shell id -u)
GID := $(shell id -g)
PWD := $(shell pwd)

# The Sophgo host-tools cross-gcc is x86_64-only, so the builder image is amd64
# and runs under Rosetta/QEMU on Apple Silicon. Pin every build/run to it (no-op
# on a native amd64 host). Override on other arches with `make PLATFORM=... app`.
PLATFORM := linux/amd64

# Force BuildKit. The legacy builder can't build a cross-platform multi-stage
# image (it builds the base stage for the host arch, then later `FROM base AS …`
# stages demand --platform and fail with "does not provide the specified
# platform"). BuildKit builds every stage for --platform consistently.
export DOCKER_BUILDKIT := 1

# Docker run common parameters
DOCKER_RUN_BASE := docker run --platform=$(PLATFORM) -e UID=$(UID) -e GID=$(GID) -v $(PWD):/home/build/NanoKVM --rm

# Build commands
# NOTE: -ldflags=-extldflags=-Wl,-rpath,$$ORIGIN/dl_lib bakes the runpath so the
# binary finds libkvm.so (and the C906 libs) in ./dl_lib at runtime. S95nanokvm
# runs the server from /tmp/server with no LD_LIBRARY_PATH, so without this the
# binary can't load libkvm.so and exits immediately (the stock build sets the
# same runpath; build.sh/CI use patchelf for it). $$ escapes for make; $ORIGIN
# must reach the linker literally.
GO_BUILD_CMD := cd /home/build/NanoKVM/server && go mod tidy && CGO_ENABLED=1 GOOS=linux GOARCH=riscv64 CC=riscv64-unknown-linux-musl-gcc CGO_CFLAGS="-mcpu=c906fdv -march=rv64imafdcv0p7xthead -mcmodel=medany -mabi=lp64d" go build -ldflags=-extldflags=-Wl,-rpath,$$ORIGIN/dl_lib
SUPPORT_BUILD_CMD := . ./home/build/MaixCDK/bin/activate && cd /home/build/NanoKVM/support/sg2002 && ./build kvm_system && ./build kvm_system add_to_kvmapp

.PHONY: help check-root builder-image full-image rebuild-image check-image shell app support all clean

# Default target
all: app support

# Help target
help:
	@echo "NanoKVM Build System"
	@echo ""
	@echo "Available targets:"
	@echo "  help          - Show this help message"
	@echo "  check-image   - Check builder Docker image and show versions"
	@echo "  builder-image - Build lean server image (Go + host-tools) if not exists"
	@echo "  full-image    - Build full image (adds MaixCDK SDK) if not exists"
	@echo "  rebuild-image - Force rebuild the lean server image"
	@echo "  shell         - Enter interactive builder environment (full image)"
	@echo "  app           - Build Go application server (lean image)"
	@echo "  support       - Build hardware support libraries (full image)"
	@echo "  all           - Build both app and support (default)"
	@echo "  clean         - Clean build artifacts"
	@echo ""
	@echo "Prerequisites:"
	@echo "  - Docker must be installed and running"
	@echo "  - Must not run as root user"

# Security check - prevent running as root
check-root:
	@if [ "$$(id -u)" -eq 0 ]; then \
		echo "Can't run as root"; \
		exit 1; \
	fi

# Check if builder image exists and show versions
check-image: check-root
	@echo "Checking builder image..."
	@echo "Golang version: " && \
		docker run --platform=$(PLATFORM) --rm -i $(IMAGE_NAME) go version && \
		echo "" && \
		echo "Host-tools version:" && \
		docker run --platform=$(PLATFORM) --rm -i $(IMAGE_NAME) riscv64-unknown-linux-musl-gcc -v && \
		echo ""

# Build the lean server builder image if it doesn't exist (Go + host-tools,
# `--target server`; BuildKit skips the MaixCDK stage). This is what setup-risc
# and the server build use.
builder-image: check-root
	@if ! docker image inspect $(IMAGE_NAME) >/dev/null 2>&1; then \
		echo "Building Docker image..."; \
		docker build --platform=$(PLATFORM) --target server -t $(IMAGE_NAME) -f docker/Dockerfile ./; \
	else \
		echo "Docker image $(IMAGE_NAME) already exists."; \
	fi

# Build the full builder image (server + MaixCDK SDK). Only `support`/`shell`
# need it; it's slow under emulation, so it is built on demand, not by setup.
full-image: check-root
	@if ! docker image inspect $(FULL_IMAGE) >/dev/null 2>&1; then \
		echo "Building full Docker image (with MaixCDK)..."; \
		docker build --platform=$(PLATFORM) --target full -t $(FULL_IMAGE) -f docker/Dockerfile ./; \
	else \
		echo "Docker image $(FULL_IMAGE) already exists."; \
	fi

# Force rebuild the lean server builder image
rebuild-image: check-root
	@echo "Force rebuilding Docker image..."
	@docker build --platform=$(PLATFORM) --no-cache --target server -t $(IMAGE_NAME) -f docker/Dockerfile ./

# Enter interactive shell (full image, MaixCDK activated)
shell: check-root full-image
	@echo "Switching into builder..."
	@$(DOCKER_RUN_BASE) -it $(FULL_IMAGE) /bin/bash -c ". ./home/build/MaixCDK/bin/activate && cd /home/build/NanoKVM ; exec bash"

# Build Go application (lean server image — no MaixCDK needed)
app: check-root builder-image
	@echo "Building app..."
	@$(DOCKER_RUN_BASE) -it $(IMAGE_NAME) /bin/bash -c '$(GO_BUILD_CMD)'

# Build hardware support libraries (full image — regenerates libkvm)
support: check-root full-image
	@echo "Building support..."
	@$(DOCKER_RUN_BASE) -it $(FULL_IMAGE) /bin/bash -c '$(SUPPORT_BUILD_CMD)'

# Clean build artifacts
clean:
	@echo "Cleaning build artifacts..."
	@if [ -f server/NanoKVM-Server ]; then \
		rm -f server/NanoKVM-Server; \
		echo "Removed server/NanoKVM-Server"; \
	fi
	@if [ -d support/sg2002/build ]; then \
		rm -rf support/sg2002/build; \
		echo "Removed support/sg2002/build"; \
	fi
	@echo "Clean completed."