# NanoKVM — build & deploy the RISC-V device with the native AllMyStuff bridge.
#
# `just build-risc` produces a COMPLETE device build in one step:
#   server/NanoKVM-Server         the Go server (with the mesh bridge)
#   kvmapp/system/bin/myownmesh   the MyOwnMesh daemon, pinned in .myownmesh-rev
#
# `just setup-risc` bootstraps everything: it installs + starts a Docker runtime
# (Colima — lightweight, no Docker Desktop) if you don't have one, then builds
# the builder image. The cross-toolchain is Linux-only, so a Linux container is
# how a Mac cross-compiles; setup-risc just makes that painless.
#
# The server builds inside the Docker builder image (Go + riscv64 musl
# toolchain). The daemon is the version pinned in .myownmesh-rev: `build-risc`
# downloads the published `myownmesh-linux-riscv64.tar.gz` release asset when one
# exists (fast), or builds it from the pinned source in the same builder image
# otherwise — either way you run ONE command. No sibling checkout.
#
# For local testing you don't need the daemon here at all: run a `myownmesh
# serve` you already have and point the bridge at its control socket (set
# mesh.home / MYOWNMESH_HOME) — see docs/MESH.md.

set shell := ["bash", "-uc"]

image := "nanokvm-builder"
daemon_dst := "kvmapp/system/bin/myownmesh"
mom_repo := "https://github.com/mrjeeves/MyOwnMesh"

default: help

help:
    @just --list

# One-time: get a Docker-compatible runtime going (installs + starts Colima on a
# Mac if you don't already have one — no Docker Desktop needed), then build the
# builder image (Go + riscv64 musl toolchain). Idempotent: re-run any time.
setup-risc:
    #!/usr/bin/env bash
    set -euo pipefail
    # 1. Ensure a working Docker daemon. If `docker info` already succeeds (any
    #    runtime — Colima, Docker Desktop, Linux dockerd), use it as-is.
    if ! docker info >/dev/null 2>&1; then
      case "$(uname -s)" in
        Darwin)
          command -v brew >/dev/null || { echo "❌ Install Homebrew first: https://brew.sh"; exit 1; }
          command -v colima >/dev/null || { echo "==> installing colima (lightweight Linux VM)…"; brew install colima; }
          command -v docker >/dev/null || { echo "==> installing the docker CLI…"; brew install docker; }
          if ! colima status >/dev/null 2>&1; then
            echo "==> starting colima (first boot takes a minute)…"
            # vz + Rosetta runs the amd64 toolchain image fast on Apple Silicon;
            # falls back to a plain start (qemu emulation) on older macOS/Intel.
            colima start --vm-type=vz --vz-rosetta 2>/dev/null || colima start
          fi
          # Make sure linux/amd64 images (the Sophgo toolchain) can run.
          docker run --privileged --rm tonistiigi/binfmt --install amd64 >/dev/null 2>&1 || true
          ;;
        Linux)
          echo "❌ Docker isn't available. Install it (e.g. 'sudo apt-get install -y docker.io',"
          echo "   add yourself to the 'docker' group) or see https://docs.docker.com/engine/install/, then re-run."
          exit 1 ;;
        *)
          echo "❌ Unsupported OS for auto-setup — install a Docker-compatible runtime and re-run."; exit 1 ;;
      esac
      docker info >/dev/null 2>&1 || { echo "❌ Docker still not reachable after setup."; exit 1; }
    fi
    echo "==> Docker runtime OK"
    # 2. Build the NanoKVM builder image (Go + riscv64-unknown-linux-musl-gcc).
    echo "==> building the builder image…"
    make builder-image
    echo "OK — now: just build-risc"

# Build a complete device image — the server AND the pinned daemon — in one step.
build-risc: build-server daemon
    @echo
    @echo "Device build complete:"
    @echo "  server/NanoKVM-Server"
    @echo "  {{daemon_dst}}"
    @echo "Deploy: just deploy <device-ip>"

# Build just the NanoKVM server (Go, with the mesh bridge).
build-server:
    @echo "==> building NanoKVM-Server…"
    @make app
    @test -f server/NanoKVM-Server && echo "OK -> server/NanoKVM-Server"

# Obtain the daemon pinned in .myownmesh-rev and stage it at {{daemon_dst}}:
# prefer the published release asset; fall back to building it from the pinned
# source in the Docker builder if no release exists at that rev yet.
daemon:
    #!/usr/bin/env bash
    set -euo pipefail
    rev="$(cat .myownmesh-rev)"
    dst="{{daemon_dst}}"; mkdir -p "$(dirname "$dst")"
    asset="myownmesh-linux-riscv64.tar.gz"
    url="{{mom_repo}}/releases/download/${rev}/${asset}"
    sha() { if command -v sha256sum >/dev/null; then sha256sum -c "$1"; else shasum -a 256 -c "$1"; fi; }
    tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
    echo "==> daemon pinned at ${rev}"
    if curl -fsSL "$url" -o "$tmp/$asset" && curl -fsSL "$url.sha256" -o "$tmp/$asset.sha256"; then
      echo "    fetched the published release asset; verifying sha256…"
      ( cd "$tmp" && sha "$asset.sha256" )
      tar -xzf "$tmp/$asset" -C "$(dirname "$dst")"
      chmod +x "$dst"
      echo "OK (release ${rev}) -> $dst"
    else
      echo "    no published ${asset} at ${rev} yet — building it from the pinned source…"
      src=".myownmesh-src"
      [ -d "$src/.git" ] || git clone --filter=blob:none "{{mom_repo}}" "$src"
      git -C "$src" fetch --tags --filter=blob:none origin
      git -C "$src" checkout --quiet --detach "$rev"
      src_abs="$(cd "$src" && pwd)"
      docker run --rm \
        -v "$src_abs":/s -w /s \
        -v nanokvm-daemon-home:/root \
        --entrypoint bash {{image}} -c '
          set -e
          export PATH="$PATH:/usr/local/host-tools/gcc/riscv64-linux-musl-x86_64/bin"
          if ! . "$HOME/.cargo/env" 2>/dev/null; then
            wget -qO- https://sh.rustup.rs | sh -s -- -y --profile minimal --default-toolchain 1.88.0
            . "$HOME/.cargo/env"
          fi
          rustup target add riscv64gc-unknown-linux-musl
          cargo build --release --bin myownmesh --target riscv64gc-unknown-linux-musl'
      cp "$src_abs/target/riscv64gc-unknown-linux-musl/release/myownmesh" "$dst"
      echo "OK (built ${rev}) -> $dst"
    fi

# Print the pinned MyOwnMesh daemon revision.
daemon-rev:
    @cat .myownmesh-rev

# Copy the complete device build (server + daemon + init script) to a device.
deploy ip:
    #!/usr/bin/env bash
    set -euo pipefail
    test -f server/NanoKVM-Server && test -f "{{daemon_dst}}" || { echo "❌ build first: just build-risc"; exit 1; }
    echo "==> deploying to {{ip}}…"
    ssh root@{{ip}} 'mkdir -p /kvmapp/system/bin'
    scp "{{daemon_dst}}"                   root@{{ip}}:/kvmapp/system/bin/myownmesh
    scp kvmapp/system/init.d/S94myownmesh  root@{{ip}}:/kvmapp/system/init.d/S94myownmesh
    scp server/NanoKVM-Server              root@{{ip}}:/kvmapp/server/NanoKVM-Server
    ssh root@{{ip}} 'chmod +x /kvmapp/system/bin/myownmesh /kvmapp/system/init.d/S94myownmesh /kvmapp/server/NanoKVM-Server'
    echo "OK — just reboot {{ip}} && just verify {{ip}}"

reboot ip:
    @ssh root@{{ip}} reboot || true

# Daemon process + persisted state + log on a device.
verify ip:
    @ssh root@{{ip}} 'echo "--- daemon ---"; ps | grep -i myownmesh | grep -v grep || echo "(no daemon serving)"; echo "--- state (/data/myownmesh) ---"; ls -la /data/myownmesh 2>/dev/null || echo "(none yet)"; echo "--- log ---"; tail -n 40 /var/log/myownmesh.log 2>/dev/null || echo "(none yet)"'

# Reversible undo on a device: remove the daemon init script + reboot.
undeploy ip:
    @ssh root@{{ip}} 'rm -f /kvmapp/system/init.d/S94myownmesh && reboot' || true

clean-risc:
    @rm -rf server/NanoKVM-Server {{daemon_dst}} .myownmesh-src
    @echo "removed build outputs (Docker image + cargo cache volume kept)"
