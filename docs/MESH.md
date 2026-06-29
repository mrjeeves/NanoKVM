# Native AllMyStuff mesh integration

The NanoKVM can join an [AllMyStuff](https://github.com/) cloud mesh as a
first-class **KVM appliance** node. Once joined it:

- advertises its presence (hardware thumbnail, ownership, fleet) so it shows up
  in the AllMyStuff graph with the KVM drawer;
- tunnels its **own web UI** over the mesh "sites" plane, with the KVM login
  bypassed — mesh roster membership is the authentication;
- supports **claim** (adoption), **fleet** join, and **attach/detach** (binding
  the KVM to the machine it controls).

This is a pure-Go bridge living in `server/service/mesh`. **v1 does not** do
native screen/HID streaming — the tunneled web UI delivers the full KVM
experience — so the bridge imports none of the CGO/libkvm packages and builds &
tests on a host (`go test ./service/mesh/...`).

## How it works

A separate **MyOwnMesh daemon** runs on the device and owns the WebRTC mesh
transport. The bridge talks to it over a local control socket
(`$MYOWNMESH_HOME/daemon.sock`, line-delimited JSON). The daemon authenticates
every peer (ed25519 handshake) before any byte reaches the bridge.

On start (when `mesh.enabled`), the bridge:

1. connects to the daemon socket and `events_subscribe`s (capturing its
   `client_id`);
2. `identity_show` → learns this device's node id;
3. `networks_list` → joins the configured network with `network_add` if absent;
4. `channel_subscribe`s the presence / control / media planes and
   `capabilities_set`s the AllMyStuff marker (`allmystuff`, `kvm`, `sites` tags,
   with the inventory summary + endpoints nested under `extra`);
5. broadcasts a `NodeProfile` on the presence plane (and re-broadcasts on every
   state change and on a slow heartbeat).

Inbound **control** messages (claim, fleet-key, attach/detach, site-route
offer) are handled in `control.go`; inbound **media** `SiteFrame`s are demuxed
per route/connection in `sites.go`, each tunneled browser connection served as
in-process HTTP through the gin engine with `middleware.WithMeshAuth`.

The bridge is **non-fatal**: if the daemon isn't up yet it logs and retries, so
the KVM is never blocked from serving its LAN.

## Ownership, claim, and fleets

- A fresh device is **claimable**. An `Ownership Claim{owner}` (only honored
  while claimable) records the owner, ends claim mode, and — because a KVM is
  physically wired to the machine that claims it — **auto-attaches** to the
  owner. The device replies `Ownership Claimed{owner}` and re-advertises.
- `Ownership FleetKey{key,name,venue}` hands down the shared fleet credential.
  The bridge derives the fleet's closed-network id from the key (a byte-for-byte
  port of AllMyStuff's `derive_fleet_network_id`, see `fleet.go`) and **joins
  that network**, so the KVM truly becomes a fleet member. If a `venue`
  transport config is supplied it's used verbatim.
- `Kvm Attach{node}` / `Kvm Detach` re-point or clear the binding. Both are
  gated on the sender being the device's owner (the mesh authenticates the
  sender).

State (owner, claimable, attached_to, fleet_key, fleet_name) is persisted to
`$MYOWNMESH_HOME/kvm-state.json`.

## Auth bypass

`middleware/jwt.go` exposes `WithMeshAuth(r)` (marks a request context
mesh-authenticated) and the token check passes for such requests. The site
tunnel wraps every request with it, so mesh-tunneled requests are authenticated
**without a token** while normal LAN/direct requests are unaffected. Mesh roster
membership replaces the KVM login.

## Configuration

Add a `mesh` block to `/etc/kvm/server.yaml` (defaults shown):

```yaml
mesh:
  enabled: true
  home: /data/myownmesh
  networkId: cec-backend-client-mesh
  label: CEC Backend Client Mesh
  relays: []            # empty = public venue default
  daemonBin: /kvmapp/system/bin/myownmesh
```

## Building & deploying

Both device artifacts — the Go server (with the bridge) and the Rust
`myownmesh` daemon — build inside the Docker builder image, which carries the Go
compiler **and** `riscv64-unknown-linux-musl-gcc`. A dev box needs only Docker;
no Go, Rust, or RISC-V toolchain on the host. The daemon is cross-compiled from a
sibling MyOwnMesh checkout (override with `just mom=/path …`).

```sh
just setup-risc            # one-time: builder image + Rust toolchain (cached)
just build-risc            # build server/NanoKVM-Server + kvmapp/system/bin/myownmesh
just deploy <device-ip>    # scp both + the init script to a running device
just reboot <device-ip>
just verify <device-ip>    # daemon process + /data/myownmesh + /var/log/myownmesh.log
just undeploy <device-ip>  # reversible: remove the init script + reboot
```

`just build-server` / `just build-daemon` build either half alone; `make app`
still builds the server the upstream way. The daemon's own cross-build lives in
MyOwnMesh (`just build-risc` there); see MyOwnMesh `docs/NANOKVM.md`.

## Packaging

`kvmapp/system/init.d/S94myownmesh` starts the MyOwnMesh daemon with
`MYOWNMESH_HOME=/data/myownmesh` **before** the NanoKVM server (`S95nanokvm`),
following the same copy-to-`/tmp`-and-launch pattern. `just build-daemon` stages
the daemon binary at `/kvmapp/system/bin/myownmesh` (matching `mesh.daemonBin`)
so it ships in the image. The init script no-ops cleanly if the binary is absent.

## Tests

```
cd server
go vet ./service/mesh/... ./middleware/...
go test ./service/mesh/...
```

`protocol_test.go` round-trips every wire type (NodeProfile with kvm,
ControlMessage ownership/kvm/route, SiteFrame/SiteEvent, control
Request/Response, ClientId `c<n>`, fleet-id derivation). `sites_test.go` covers
the `meshConn` `net.Conn` framing and a Data/Close round trip plus the host
allow-list.
