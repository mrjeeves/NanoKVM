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
# toolchain). The daemon is NOT built here — it's the prebuilt
# `myownmesh-linux-riscv64.tar.gz` from the MyOwnMesh release pinned in
# .myownmesh-rev, downloaded and staged for you. (MyOwnMesh cross-compiles it
# with cargo-zigbuild; a NanoKVM never builds Rust.)
#
# Don't want to build the server either? `just install <device-ip>` downloads a
# prebuilt server (from THIS repo's release) AND the pinned daemon and deploys
# them — zero local Docker, zero toolchain.
#
# For local testing you don't need the daemon here at all: run a `myownmesh
# serve` you already have and point the bridge at its control socket (set
# mesh.home / MYOWNMESH_HOME) — see docs/MESH.md.

set shell := ["bash", "-uc"]

image := "nanokvm-builder"
daemon_dst := "kvmapp/system/bin/myownmesh"
mom_repo := "https://github.com/mrjeeves/MyOwnMesh"
nanokvm_repo := "https://github.com/mrjeeves/NanoKVM"

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
    # 2. Ensure the buildx plugin. BuildKit needs it to build the cross-platform
    #    multi-stage amd64 image (the legacy builder can't), and the Homebrew
    #    `docker` CLI doesn't bundle it. Checked every run — a Colima that was
    #    already up skips step 1 entirely, so this can't live inside that block.
    if ! docker buildx version >/dev/null 2>&1; then
      case "$(uname -s)" in
        Darwin)
          command -v brew >/dev/null || { echo "❌ Install Homebrew first: https://brew.sh"; exit 1; }
          echo "==> installing docker-buildx…"
          brew list docker-buildx >/dev/null 2>&1 || brew install docker-buildx
          # Homebrew installs the binary but leaves the CLI-plugin symlink to you.
          mkdir -p "${DOCKER_CONFIG:-$HOME/.docker}/cli-plugins"
          ln -sfn "$(brew --prefix)/opt/docker-buildx/bin/docker-buildx" \
                  "${DOCKER_CONFIG:-$HOME/.docker}/cli-plugins/docker-buildx"
          ;;
        Linux)
          echo "❌ docker buildx plugin missing. Install it (e.g. 'sudo apt-get install -y docker-buildx-plugin')"
          echo "   or see https://docs.docker.com/go/buildx/, then re-run."
          exit 1 ;;
        *) echo "❌ docker buildx missing and no auto-install for this OS — install it and re-run."; exit 1 ;;
      esac
      docker buildx version >/dev/null 2>&1 || { echo "❌ buildx still not available after install."; exit 1; }
    fi
    echo "==> buildx OK ($(docker buildx version | head -1))"
    # 3. Build the NanoKVM builder image (Go + riscv64-unknown-linux-musl-gcc).
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

# The daemon is never built here — MyOwnMesh cross-compiles + publishes it, and
# this fails with a clear pointer (not a wrong build) if the pinned release has
# no riscv asset yet.
#
# Download the pinned MyOwnMesh daemon release and stage it for deploy.
daemon:
    #!/usr/bin/env bash
    set -euo pipefail
    rev="$(cat .myownmesh-rev)"
    dst="{{daemon_dst}}"; mkdir -p "$(dirname "$dst")"
    asset="myownmesh-linux-riscv64.tar.gz"
    url="{{mom_repo}}/releases/download/${rev}/${asset}"
    sha() { if command -v sha256sum >/dev/null; then sha256sum -c "$1"; else shasum -a 256 -c "$1"; fi; }
    tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
    echo "==> daemon pinned at ${rev}: ${url}"
    if ! curl -fsSL "$url" -o "$tmp/$asset"; then
      echo "❌ no ${asset} published at ${rev}." >&2
      echo "   Cut a MyOwnMesh release that includes the riscv64 daemon asset (just release <ver>)," >&2
      echo "   then set .myownmesh-rev to that tag. Or build it yourself: in a MyOwnMesh checkout run" >&2
      echo "   'just build-risc' and copy target/riscv64gc-unknown-linux-musl/release/myownmesh to ${dst}." >&2
      exit 1
    fi
    if curl -fsSL "$url.sha256" -o "$tmp/$asset.sha256"; then
      echo "    verifying sha256…"; ( cd "$tmp" && sha "$asset.sha256" )
    else
      echo "    (no .sha256 published; skipping integrity check)"
    fi
    tar -xzf "$tmp/$asset" -C "$(dirname "$dst")"
    chmod +x "$dst"
    echo "OK (release ${rev}) -> $dst"

# Print the pinned MyOwnMesh daemon revision.
daemon-rev:
    @cat .myownmesh-rev

# ── Download-only path: deploy a release with NO local build (no Docker) ───────
#
# `just install <device-ip>` fetches the prebuilt device bundle (server + the
# pinned daemon, in one NanoKVM release asset) and deploys it. Nothing is
# compiled locally. This is the everyday path once releases are published; use
# `build-risc` only to build from source.

# One NanoKVM release asset carries the whole device payload: CI bundles the
# .myownmesh-rev daemon into it (like AllMyStuff bundles the daemon into its
# app), so this single download has both binaries and no build is needed.
#
# Download the device bundle (latest release, or VERSION): server + daemon.
fetch VERSION="latest":
    #!/usr/bin/env bash
    set -euo pipefail
    sha() { if command -v sha256sum >/dev/null; then sha256sum -c "$1"; else shasum -a 256 -c "$1"; fi; }
    asset="nanokvm-mesh-riscv64.tar.gz"
    if [ "{{VERSION}}" = "latest" ]; then
      url="{{nanokvm_repo}}/releases/latest/download/${asset}"
    else
      url="{{nanokvm_repo}}/releases/download/{{VERSION}}/${asset}"
    fi
    tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
    echo "==> device bundle ({{VERSION}}): ${url}"
    if ! curl -fsSL "$url" -o "$tmp/$asset"; then
      echo "❌ no ${asset} at {{VERSION}}. Cut a NanoKVM release (just release X.Y.Z) so CI publishes it," >&2
      echo "   or build locally with 'just build-risc'." >&2
      exit 1
    fi
    if curl -fsSL "$url.sha256" -o "$tmp/$asset.sha256"; then
      echo "    verifying sha256…"; ( cd "$tmp" && sha "$asset.sha256" )
    else
      echo "    (no .sha256 published; skipping integrity check)"
    fi
    mkdir -p server "$(dirname "{{daemon_dst}}")"
    tar -xzf "$tmp/$asset" -C "$tmp"
    cp "$tmp/NanoKVM-Server" server/NanoKVM-Server
    cp "$tmp/myownmesh"      "{{daemon_dst}}"
    chmod +x server/NanoKVM-Server "{{daemon_dst}}"
    echo "OK -> server/NanoKVM-Server + {{daemon_dst}}"
    echo "Now: just deploy <device-ip>   (or use 'just install <device-ip>')"

# Fetch the prebuilt device bundle (server + daemon) and deploy to a device.
install ip VERSION="latest": (fetch VERSION)
    @just deploy {{ip}}

# Bump the advertised version, commit, push, then push the `vX.Y.Z` tag to
# trigger the release workflow — which builds the server, bundles the
# .myownmesh-rev daemon, and publishes nanokvm-mesh-riscv64.tar.gz. Mirrors
# MyOwnMesh / AllMyStuff.
#
# Cut a release (bump version + tag) so CI publishes the device bundle.
release VERSION:
    #!/usr/bin/env bash
    set -euo pipefail
    ./scripts/bump-version.sh "{{VERSION}}"
    if ! git diff --quiet server/service/mesh/bridge.go web/package.json; then
      git add server/service/mesh/bridge.go web/package.json
      git commit -m "chore(release): {{VERSION}}"
    fi
    git push
    git tag "v{{VERSION}}"
    git push origin "v{{VERSION}}"
    echo ""
    echo "✓ pushed tag v{{VERSION}} — the release workflow is building the device bundle."
    echo "  It publishes nanokvm-mesh-riscv64.tar.gz (server + pinned daemon)."
    echo "  Then: just install <device-ip>   (downloads that bundle and deploys)"

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
    @rm -rf server/NanoKVM-Server {{daemon_dst}}
    @echo "removed build outputs (Docker builder image kept)"
