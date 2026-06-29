# NanoKVM — build & deploy the RISC-V device with the native AllMyStuff bridge.
#
# Two artifacts ship to the Sophgo SG2002 (riscv64 + musl):
#   server/NanoKVM-Server        the Go server (with the mesh bridge)
#   kvmapp/system/bin/myownmesh  the MyOwnMesh daemon (Rust)
#
# Both build INSIDE the Docker builder image (it carries the Go compiler AND
# riscv64-unknown-linux-musl-gcc), so a dev box needs only Docker — no Go, Rust,
# or RISC-V toolchain on the host. The daemon is built from a sibling MyOwnMesh
# checkout (override with `just mom=/path …`).
#
# Typical flow:
#   just setup-risc            # one-time: builder image + Rust toolchain (cached)
#   just build-risc            # build both artifacts
#   just deploy 192.168.1.50   # copy them to a running device
#   just verify 192.168.1.50   # confirm the daemon came up
#
# (The Go server can also be built the upstream way with `make app`; these
# recipes wrap it and add the daemon + deploy so the whole device is one flow.)

set shell := ["bash", "-uc"]

# A MyOwnMesh checkout to build the daemon from (sibling by default).
mom := "../MyOwnMesh"
# The Docker builder image name (matches the Makefile).
image := "nanokvm-builder"
# Where the daemon is staged so it ships in the device image / gets deployed.
daemon_dst := "kvmapp/system/bin/myownmesh"
# riscv64 musl cross toolchain dir inside the builder image (Sophgo host-tools).
toolchain_bin := "/usr/local/host-tools/gcc/riscv64-linux-musl-x86_64/bin"

default: help

help:
    @just --list

# One-time: build the Docker builder image (Go + riscv64 musl toolchain) and
# prime the Rust toolchain + target into a cached volume. Safe to re-run.
setup-risc:
    #!/usr/bin/env bash
    set -euo pipefail
    echo "==> ensuring Docker builder image ({{image}})…"
    make builder-image
    echo "==> priming Rust + the riscv64 musl target (cached in volume nanokvm-daemon-home)…"
    docker run --rm -v nanokvm-daemon-home:/root --entrypoint bash {{image}} -c '
      set -e
      if ! . "$HOME/.cargo/env" 2>/dev/null; then
        wget -qO- https://sh.rustup.rs | sh -s -- -y --profile minimal --default-toolchain 1.88.0
        . "$HOME/.cargo/env"
      fi
      rustup target add riscv64gc-unknown-linux-musl
      echo "toolchain: $(riscv64-unknown-linux-musl-gcc --version | head -1)"'
    echo "OK — now: just build-risc"

# Build BOTH device artifacts.
build-risc: build-server build-daemon
    @echo
    @echo "Built for the device:"
    @echo "  server/NanoKVM-Server"
    @echo "  {{daemon_dst}}"
    @echo "Next: just deploy <device-ip>"

# Build the NanoKVM server (Go, with the mesh bridge) — wraps `make app`.
build-server:
    @echo "==> building NanoKVM-Server…"
    make app
    @test -f server/NanoKVM-Server && echo "OK -> server/NanoKVM-Server"

# Cross-build the MyOwnMesh daemon and stage it into {{daemon_dst}}.
build-daemon:
    #!/usr/bin/env bash
    set -euo pipefail
    mom="{{mom}}"
    [ -d "$mom" ] || { echo "❌ MyOwnMesh not found at '$mom' (override: just mom=/path build-daemon)"; exit 1; }
    if ! grep -q linux-riscv64 "$mom/crates/myownmesh-updater/src/lib.rs" 2>/dev/null; then
      echo "❌ MyOwnMesh lacks the riscv64 updater fix — update it to latest main (git -C $mom pull)."; exit 1
    fi
    mom_abs="$(cd "$mom" && pwd)"
    echo "==> cross-building the myownmesh daemon (riscv64 musl) in {{image}}…"
    docker run --rm \
      -v "$mom_abs":/src -w /src \
      -v nanokvm-daemon-home:/root \
      --entrypoint bash {{image}} -c '
        set -e
        export PATH="$PATH:{{toolchain_bin}}"
        if ! . "$HOME/.cargo/env" 2>/dev/null; then
          wget -qO- https://sh.rustup.rs | sh -s -- -y --profile minimal --default-toolchain 1.88.0
          . "$HOME/.cargo/env"
        fi
        rustup target add riscv64gc-unknown-linux-musl
        cargo build --release --bin myownmesh --target riscv64gc-unknown-linux-musl'
    mkdir -p "$(dirname "{{daemon_dst}}")"
    cp "$mom_abs/target/riscv64gc-unknown-linux-musl/release/myownmesh" "{{daemon_dst}}"
    echo "OK -> {{daemon_dst}}"

# Copy freshly-built artifacts to a running device. Usage: just deploy 192.168.1.50
deploy ip:
    #!/usr/bin/env bash
    set -euo pipefail
    test -f "{{daemon_dst}}"        || { echo "❌ daemon not built — run: just build-risc"; exit 1; }
    test -f server/NanoKVM-Server   || { echo "❌ server not built — run: just build-risc"; exit 1; }
    echo "==> deploying to {{ip}}…"
    ssh root@{{ip}} 'mkdir -p /kvmapp/system/bin'
    scp "{{daemon_dst}}"                     root@{{ip}}:/kvmapp/system/bin/myownmesh
    scp kvmapp/system/init.d/S94myownmesh    root@{{ip}}:/kvmapp/system/init.d/S94myownmesh
    scp server/NanoKVM-Server                root@{{ip}}:/kvmapp/server/NanoKVM-Server
    ssh root@{{ip}} 'chmod +x /kvmapp/system/bin/myownmesh /kvmapp/system/init.d/S94myownmesh /kvmapp/server/NanoKVM-Server'
    echo "OK — reboot: just reboot {{ip}}   then: just verify {{ip}}"

# Reboot a device. Usage: just reboot 192.168.1.50
reboot ip:
    @ssh root@{{ip}} reboot || true

# Show the daemon's process, persisted state, and log on a device.
verify ip:
    @ssh root@{{ip}} 'echo "--- process ---"; ps | grep -i myownmesh | grep -v grep || echo "(daemon not running)"; echo "--- state (/data/myownmesh) ---"; ls -la /data/myownmesh 2>/dev/null || echo "(none yet)"; echo "--- log (tail) ---"; tail -n 40 /var/log/myownmesh.log 2>/dev/null || echo "(no log yet)"'

# Reversible undo: stop shipping the daemon on a device (removes the init script) + reboot.
undeploy ip:
    @ssh root@{{ip}} 'rm -f /kvmapp/system/init.d/S94myownmesh && reboot' || true

# Remove locally-built artifacts (keeps the Docker image + cargo cache volume).
clean-risc:
    @rm -f server/NanoKVM-Server {{daemon_dst}}
    @echo "removed built artifacts. To purge the cargo cache too: docker volume rm nanokvm-daemon-home"
