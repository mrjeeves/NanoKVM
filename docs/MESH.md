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

NanoKVM builds **one** artifact — `server/NanoKVM-Server` (Go, with the bridge) —
inside the Docker builder image, so a dev box needs only Docker. The MyOwnMesh
**daemon is not built here**; the bridge connects to an existing `myownmesh
serve` control socket (the same control-socket reuse AllMyStuff relies on), and
the MyOwnMesh version this server targets is pinned in `.myownmesh-rev` (the same
model AllMyStuff uses for the daemon it ships with).

```sh
just setup-risc            # one-time: build the Docker builder image
just build-risc            # build server/NanoKVM-Server   (alias: build-server)
just deploy <device-ip>    # scp the server + init script to a device
just reboot <device-ip>
just verify <device-ip>    # confirm a daemon is serving + its log
just undeploy <device-ip>  # reversible: remove the init script + reboot
```

### Testing against an existing daemon

The bridge dials `$MYOWNMESH_HOME/daemon.sock` and reuses whatever `myownmesh
serve` is already running — it never spawns or builds a daemon. To test:

1. Run a `myownmesh` daemon you already have (built/installed the normal MyOwnMesh
   way — **no cross-compile needed for a non-device daemon**): `myownmesh serve`.
2. Point the bridge at it: set `mesh.home` in `server.yaml` (or `MYOWNMESH_HOME`)
   to that daemon's home, so `<home>/daemon.sock` resolves to the running socket.
3. Start `NanoKVM-Server`; the bridge connects, joins `cec-backend-client-mesh`,
   and advertises the KVM. `just verify` shows the daemon side.

On a device, the daemon is installed separately at `.myownmesh-rev` (build it
with MyOwnMesh's `just build-risc`, or install a release ≥ that rev), and
`S94myownmesh` starts it at boot.

## Packaging

`kvmapp/system/init.d/S94myownmesh` starts the MyOwnMesh daemon with
`MYOWNMESH_HOME=/data/myownmesh` **before** the NanoKVM server (`S95nanokvm`),
following the same copy-to-`/tmp`-and-launch pattern. The daemon binary is
expected at `/kvmapp/system/bin/myownmesh` (matching `mesh.daemonBin`), installed
separately at the `.myownmesh-rev` version — NanoKVM does not build or ship it.
The init script no-ops cleanly if the binary is absent (the bridge then just
keeps retrying the socket, so the device stays a normal KVM).

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
