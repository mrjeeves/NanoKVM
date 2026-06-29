# NanoKVM — build & deploy the RISC-V server (with the native AllMyStuff bridge).
#
# NanoKVM builds ONE artifact: server/NanoKVM-Server (Go, incl. the mesh bridge),
# inside the Docker builder image — so a dev box needs only Docker (no Go/Rust/
# RISC-V toolchain on the host).
#
# The MyOwnMesh DAEMON is NOT built here. The bridge connects to an existing
# `myownmesh serve` control socket at $MYOWNMESH_HOME/daemon.sock:
#   * testing — point it at a daemon you already run (set `mesh.home` /
#     MYOWNMESH_HOME to that daemon's home); the bridge reuses the socket.
#   * device  — the daemon is installed separately at the MyOwnMesh version
#     pinned in .myownmesh-rev (same model AllMyStuff uses), and the init
#     script (kvmapp/system/init.d/S94myownmesh) starts it at boot.

set shell := ["bash", "-uc"]

image := "nanokvm-builder"

default: help

help:
    @just --list

# One-time: build the Docker builder image (Go + riscv64 musl toolchain).
setup-risc:
    @make builder-image

# Build the NanoKVM server (Go, with the mesh bridge). Alias: build-server.
build-risc:
    @echo "==> building NanoKVM-Server…"
    make app
    @test -f server/NanoKVM-Server && echo "OK -> server/NanoKVM-Server"

alias build-server := build-risc

# Copy the server + the daemon init script to a running device. The myownmesh
# daemon itself is installed separately (the version pinned in .myownmesh-rev)
# or is already serving — NanoKVM doesn't build or ship it.
deploy ip:
    #!/usr/bin/env bash
    set -euo pipefail
    test -f server/NanoKVM-Server || { echo "❌ build first: just build-risc"; exit 1; }
    echo "==> deploying server to {{ip}}…"
    scp server/NanoKVM-Server              root@{{ip}}:/kvmapp/server/NanoKVM-Server
    scp kvmapp/system/init.d/S94myownmesh  root@{{ip}}:/kvmapp/system/init.d/S94myownmesh
    ssh root@{{ip}} 'chmod +x /kvmapp/server/NanoKVM-Server /kvmapp/system/init.d/S94myownmesh'
    echo "OK — ensure a myownmesh daemon (>= .myownmesh-rev) is installed/serving, then: just reboot {{ip}}"

# Print the pinned MyOwnMesh daemon revision this server is built against.
daemon-rev:
    @cat .myownmesh-rev

reboot ip:
    @ssh root@{{ip}} reboot || true

# Show the daemon's process, persisted state, and log on a device (or wherever
# it's serving) — confirms the bridge has a control socket to talk to.
verify ip:
    @ssh root@{{ip}} 'echo "--- daemon ---"; ps | grep -i myownmesh | grep -v grep || echo "(no daemon serving)"; echo "--- state (/data/myownmesh) ---"; ls -la /data/myownmesh 2>/dev/null || echo "(none yet)"; echo "--- log ---"; tail -n 40 /var/log/myownmesh.log 2>/dev/null || echo "(none yet)"'

# Reversible undo on a device: remove the daemon init script + reboot.
undeploy ip:
    @ssh root@{{ip}} 'rm -f /kvmapp/system/init.d/S94myownmesh && reboot' || true

clean-risc:
    @rm -f server/NanoKVM-Server
