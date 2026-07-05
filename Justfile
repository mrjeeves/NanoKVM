# NanoKVM — build & deploy the RISC-V device with the native AllMyStuff bridge.
#
# `just build-risc` produces a COMPLETE device build in one step:
#   server/NanoKVM-Server         the Go server (with the mesh bridge)
#   kvmapp/system/bin/myownmesh   the MyOwnMesh daemon, pinned in .myownmesh-rev
#   web/dist                      the web UI bundle the device serves (carries the
#                                 Mesh settings tab) — built here, not the firmware's
#   kvmapp/kvm_system/kvm_system  the OLED app (shows the joining-mesh/claim name
#                                 on the screen, both the pcie and cube layouts)
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
web_image := "nanokvm-web-builder"
daemon_dst := "kvmapp/system/bin/myownmesh"
oled_dst := "kvmapp/kvm_system/kvm_system"
oled_logo := "tools/logo_generator/allmystuff/logo.bin"
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

# Build a complete device image — server, web UI, OLED app, and the pinned
# daemon — in one step. The web bundle and the OLED app are part of the payload
# now: the device serves OUR web (with the Mesh tab), and its screen shows the
# joining-mesh name via OUR kvm_system, instead of whatever the firmware flashed.
build-risc: build-server daemon build-web build-oled
    @echo
    @echo "Device build complete:"
    @echo "  server/NanoKVM-Server"
    @echo "  {{daemon_dst}}"
    @echo "  web/dist"
    @echo "  {{oled_dst}}"
    @echo "Deploy: just deploy <device-ip>"

# Build just the NanoKVM server (Go, with the mesh bridge).
build-server:
    @echo "==> building NanoKVM-Server…"
    @make app
    @test -f server/NanoKVM-Server && echo "OK -> server/NanoKVM-Server"

# Build the web UI bundle (the React/vite SPA the device serves) into web/dist.
#
# WHY this is built and shipped: the device serves this SPA — and it carries the
# Mesh settings tab. The firmware flashes a STOCK web build with no Mesh tab, and
# neither build-risc nor deploy used to ship a web at all, so the tab never
# reached the device. We build OUR web (origin-relative, vite base '/') and ship
# it. Built in a node:22 image (vite 7 needs Node >=20) so a Mac without Node
# still builds it; the output is plain JS, so no amd64 pin (native = same bytes,
# faster). The web-builder image bakes node-gyp's toolchain — see
# docker/web.Dockerfile for why an optional `ws` addon forces that.
build-web:
    #!/usr/bin/env bash
    set -euo pipefail
    if ! docker info >/dev/null 2>&1; then
      echo "==> Docker not running — running setup-risc first (bootstraps Docker)…"
      just setup-risc
    fi
    if ! docker image inspect {{web_image}} >/dev/null 2>&1; then
      echo "==> building the web-builder image (node:22 + node-gyp toolchain)…"
      docker build -t {{web_image}} -f docker/web.Dockerfile docker
    fi
    echo "==> building the web bundle (vite) in {{web_image}}…"
    docker run --rm \
      -e HOST_UID="$(id -u)" -e HOST_GID="$(id -g)" \
      -v "$(pwd)/web:/web" -w /web {{web_image}} bash -c '
        set -e
        pnpm install --frozen-lockfile
        pnpm run build
        chown -R "${HOST_UID}:${HOST_GID}" dist node_modules 2>/dev/null || true
      '
    test -f web/dist/index.html && echo "OK -> web/dist"

# Build the OLED app (kvm_system) that draws the device screen — including the
# joining-mesh/claim name, in both the pcie (small) and cube (large) layouts.
#
# This is the MaixCDK-based hardware firmware, so it needs the FULL builder image
# (server + MaixCDK SDK) — heavier than the lean server image, and slow under
# emulation on a Mac, but it's how the on-device screen gets OUR kvm_system (with
# the mesh name) instead of the firmware's stock one. `make support` builds it
# and `add_to_kvmapp` stages the binary at {{oled_dst}}.
build-oled:
    #!/usr/bin/env bash
    set -euo pipefail
    if ! docker info >/dev/null 2>&1; then
      echo "==> Docker not running — running setup-risc first (bootstraps Docker)…"
      just setup-risc
    fi
    echo "==> building the OLED app (kvm_system) via MaixCDK — first run pulls the full SDK image…"
    make support
    test -f "{{oled_dst}}" && echo "OK -> {{oled_dst}}"

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
    rm -rf web/dist && mkdir -p web/dist && cp -a "$tmp/web/." web/dist/
    echo "OK -> server/NanoKVM-Server + {{daemon_dst}} + web/dist"
    echo "   (the OLED app isn't in the release bundle — build it with 'just build-oled' if you want the on-screen mesh name)"
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
    test -f server/NanoKVM-Server && test -f "{{daemon_dst}}" && test -d web/dist || { echo "❌ build first: just build-risc"; exit 1; }
    echo "==> deploying to {{ip}}…"
    ssh root@{{ip}} 'mkdir -p /kvmapp/system/bin'
    scp "{{daemon_dst}}"                   root@{{ip}}:/kvmapp/system/bin/myownmesh
    scp server/NanoKVM-Server              root@{{ip}}:/kvmapp/server/NanoKVM-Server
    # The init script must land in /etc/init.d — that's the dir Buildroot's rcS
    # runs at boot (run-parts … start). /kvmapp/system/init.d is only the in-image
    # source that the firmware build installs into /etc/init.d; on a running device
    # we have to place it there ourselves or it never autostarts.
    scp kvmapp/system/init.d/S94myownmesh  root@{{ip}}:/etc/init.d/S94myownmesh
    # Web UI: the server serves /tmp/server/web (S95nanokvm copies /kvmapp→/tmp at
    # boot), so replacing /kvmapp/server/web is safe — the reboot below re-copies
    # and serves OUR web (with the Mesh tab). Stage web.new and rename over web so
    # a partial scp never leaves a broken tree; same fs, so the rename is atomic.
    ssh root@{{ip}} 'rm -rf /kvmapp/server/web.new /kvmapp/server/web.old'
    scp -r web/dist                        root@{{ip}}:/kvmapp/server/web.new
    ssh root@{{ip}} 'set -e; [ -d /kvmapp/server/web ] && mv /kvmapp/server/web /kvmapp/server/web.old; mv /kvmapp/server/web.new /kvmapp/server/web; rm -rf /kvmapp/server/web.old; chmod +x /kvmapp/system/bin/myownmesh /etc/init.d/S94myownmesh /kvmapp/server/NanoKVM-Server'
    # OLED app: same /tmp-copy story. Optional — only the local `build-risc` build
    # produces it (the MaixCDK build is too heavy for the release CI), so the
    # download/`install` path won't have it; ship it when it's present.
    if [ -f "{{oled_dst}}" ]; then
      ssh root@{{ip}} 'mkdir -p /kvmapp/kvm_system'
      scp "{{oled_dst}}"                   root@{{ip}}:/kvmapp/kvm_system/kvm_system
      ssh root@{{ip}} 'chmod +x /kvmapp/kvm_system/kvm_system'
      echo "   + OLED app (kvm_system): joining-mesh name on screen"
    else
      echo "   (no {{oled_dst}} — skipping OLED; run 'just build-oled' to include it)"
    fi
    # AllMyStuff OLED boot logo: kvm_system reads /boot/logo.bin (32 bytes, 16x16
    # mono) and shows it instead of the built-in Sipeed logo. (The web logo is
    # embedded in the build as web/public/sipeed.ico — no /boot/logo.ico needed.)
    scp "{{oled_logo}}"                    root@{{ip}}:/boot/logo.bin
    echo "OK — just reboot {{ip}} && just verify {{ip}}"

reboot ip:
    @ssh root@{{ip}} reboot || true

# Daemon + bridge: processes, persisted state, and both logs on a device.
verify ip:
    @ssh root@{{ip}} 'echo "--- daemon proc ---"; ps | grep -i myownmesh | grep -v grep || echo "(no daemon serving)"; echo "--- server proc ---"; ps | grep -i nanokvm-server | grep -v grep || echo "(no server)"; echo "--- state (/data/myownmesh) ---"; ls -la /data/myownmesh 2>/dev/null || echo "(none yet)"; echo "--- daemon log ---"; tail -n 30 /var/log/myownmesh.log 2>/dev/null || echo "(none yet)"; echo "--- bridge log ---"; tail -n 30 /var/log/nanokvm-mesh.log 2>/dev/null || echo "(none yet)"'

# Reversible undo on a device: stop the daemon, remove the init script + reboot.
undeploy ip:
    @ssh root@{{ip}} '/etc/init.d/S94myownmesh stop 2>/dev/null; rm -f /etc/init.d/S94myownmesh && reboot' || true

clean-risc:
    @rm -rf server/NanoKVM-Server {{daemon_dst}} web/dist {{oled_dst}}
    @echo "removed build outputs (Docker builder images kept)"
