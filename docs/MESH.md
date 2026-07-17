# Native AllMyStuff mesh integration

The NanoKVM can join an [AllMyStuff](https://github.com/) cloud mesh as a
first-class **KVM appliance** node. Once joined it:

- advertises its presence (hardware thumbnail, ownership, fleet, mesh
  memberships) so it shows up in the AllMyStuff graph with the KVM controls;
- tunnels its **own web UI** over the mesh "sites" plane, with the KVM login
  bypassed — mesh roster membership is the authentication;
- supports **claim** (adoption), **fleet** join, **attach/detach** (binding the
  KVM to the machine it controls), owner-curated **mesh membership**
  (`mesh_add`/`mesh_remove`), and **unclaim** (`Release` → factory-reset of the
  mesh identity, back to the joining mesh in claim mode).

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
2. `identity_show` → learns this device's node id and derives its **joining
   mesh** (`cec-kvm-xxxxx-xxxxx`, `joining.go` — deterministic from the
   identity, shown on the OLED and in the web UI's Mesh tab);
3. reconciles network membership (`ensureMemberships`): the retired shared
   `cec-backend-client-mesh` is left, an **unclaimed** device joins its joining
   mesh, a **claimed** one its fleet mesh (never the joining mesh — unclaim is
   what returns it there), and owner-added meshes are kept;
4. `channel_subscribe`s the presence / control / media planes on **every**
   joined network and `capabilities_set`s the AllMyStuff marker (`allmystuff`,
   `kvm`, `sites` tags, with the inventory summary + endpoints nested under
   `extra`);
5. broadcasts a `NodeProfile` on the presence plane of every network (and
   re-broadcasts on every state change and on a slow heartbeat). The advert's
   `kvm` block carries `joining_mesh` + `meshes` (the full membership list),
   so the AllMyStuff drawer can render and curate it.

Inbound **control** messages (claim, fleet-key, release, attach/detach,
mesh add/remove, site-route offer) are handled in `control.go`; inbound
**media** `SiteFrame`s are demuxed per route/connection in `sites.go`, each
tunneled browser connection served as in-process HTTP through the gin engine
with `middleware.WithMeshAuth`.

The bridge is **non-fatal**: if the daemon isn't up yet it logs and retries, so
the KVM is never blocked from serving its LAN.

## Ownership, claim, fleets, and the joining mesh

- **Claiming is LAN-first, and public-mesh claiming is off by default.**
  While claimable, the device sits on two claim rendezvous meshes:
  - the **LAN claim mesh** (`allmystuff-local-claim-v1`, frozen — mirrored
    from `allmystuff-protocol`): mDNS-only signaling (`strategy: "none",
    mdns: true`, no relays/STUN/TURN), which every AllMyStuff node always
    joins too. A claimable KVM therefore simply **appears in the claim sheet
    of any AllMyStuff machine on the same LAN** — no id transcription, no
    mesh joining, and it works during the pre-NTP boot window (mDNS needs no
    wall clock, unlike relay TLS).
  - its own **joining mesh** (below) — by default also pinned to LAN-only
    signaling, so nothing about an unclaimed device touches public relays.
- Every device has its own **joining mesh** — `cec-kvm-<5>-<5>`, derived
  deterministically (and frozen, `joining.go`) from the daemon identity. It's
  the mesh an unclaimed/reset device waits on, and the name is surfaced on the
  device's OLED (rotating with the IPs) and in the web UI's **Mesh** settings
  tab — nothing is printed on a box. To adopt a device: read the name, join
  that mesh from AllMyStuff, claim it (same LAN by default; over the internet
  only with `publicClaims` enabled).
- **`publicClaims: true`** (config file only — see Configuration) re-opens
  the WAN paths: the joining mesh keeps its relay venue (claim by on-screen
  id over the internet), and the device additionally mints a random **claim
  code** — a rotating rendezvous secret (`amsclaim-<code>`, frozen mirror of
  AllMyStuff's derivation, `claimcode.go`) shown in the web UI's Mesh tab —
  for AllMyStuff's **Fleet → "Claim a remote device"** one-field flow. The
  code rotates after every successful claim; the only HTTP mutation exposed
  is *rotate* (which can only invalidate a code, never enable claiming).
  Claims arriving over any non-rendezvous mesh are declined
  (`claimNetworkAllowed`, defense in depth on top of the membership policy).
- A fresh device is **claimable**. An `Ownership Claim{owner}` (only honored
  while claimable) records the owner, ends claim mode, and — because a KVM is
  physically wired to the machine that claims it — **auto-attaches** to the
  owner. The device replies `Ownership Claimed{owner}` and re-advertises.
- `Ownership FleetKey{key,name,venue}` hands down the shared fleet credential.
  The bridge derives the fleet's closed-network id from the key (a byte-for-byte
  port of AllMyStuff's `derive_fleet_network_id`, see `fleet.go`) and **joins
  that network**, so the KVM truly becomes a fleet member. If a `venue`
  transport config is supplied it's used verbatim. Once the fleet mesh is
  carrying the device (roster converged, bounded wait), it **leaves the
  joining mesh** — an auto-approve venue has no business hosting a claimed
  appliance.
- `Ownership Release` is the **unclaim**: the device forgets its owner,
  attachment, and fleet credential, leaves every mesh (purging local state),
  returns to its joining mesh, and offers itself for adoption again.
- `Kvm Attach{node,label}` / `Kvm Detach` re-point or clear the binding. The
  target's display label rides the attach so the device renames itself
  **`KVM-<label>`** — on its presence advert *and* on the daemon identity
  (`identity_set_label`); detach reverts to the brand name. A label-less
  attach (older sender) falls back to the label cached from the target's own
  presence.
- `Kvm MeshAdd{network_id}` / `Kvm MeshRemove{network_id}` curate the device's
  mesh memberships (validated, purged on remove). The **fleet mesh is
  refused** — that membership is governed by the fleet key. Confirmation is
  the re-advertised `kvm.meshes` list, exactly like attach.

All of these are gated on the sender being the device's owner or a fleet
co-member (the mesh authenticates the sender).

State (owner, claimable, attached_to, attached_label, fleet_key, fleet_name)
is persisted to `$MYOWNMESH_HOME/kvm-state.json`.

## Auth bypass

`middleware/jwt.go` exposes `WithMeshAuth(r)` (marks a request context
mesh-authenticated) and the token check passes for such requests. The site
tunnel wraps every request with it, so mesh-tunneled requests are authenticated
**without a token** while normal LAN/direct requests are unaffected. Mesh roster
membership replaces the KVM login.

## The SSH site (remote shell + remote update)

Alongside the web UI, presence advertises a second site: **SSH** (`tcp:22`,
scheme `ssh`). The site host proxies that tunnel straight to the device's own
`sshd` as raw TCP — **no auth bypass rides this path**; sshd still runs its
own authentication on top of the mesh roster gate, so it is *stricter* than
the web tunnel. The advertised set stays the allow-list: any other port is
refused.

To reach a KVM's shell from anywhere on the fleet:

1. In AllMyStuff, open the **Sites** tab, find the KVM, and **Map** its
   `SSH` entry. **Copy** gives you the local address (`localhost:<port>`).
2. `ssh root@localhost -p <port>` — or update the device in place:

   ```sh
   just deploy localhost <port>     # full mesh build re-deploy over the tunnel
   just verify localhost <port>
   ```

Every device-touching recipe (`deploy`, `install`, `reboot`, `verify`,
`undeploy`) takes the port as an optional trailing argument, defaulting to
plain LAN `22`. If SSH is disabled on the device (the web UI toggle /
`/etc/kvm/ssh_stop`), the tunnel dial fails and the connection just closes —
enable SSH first.

## Configuration

Add a `mesh` block to `/etc/kvm/server.yaml` (defaults shown):

```yaml
mesh:
  enabled: true
  home: /data/myownmesh          # identity, rosters, kvm-state.json (persistent)
  socket: /tmp/myownmesh/daemon.sock  # control socket — MUST be on tmpfs (see below)
  networkId: ""                  # empty = this device's own joining mesh (cec-kvm-…)
  label: CEC KVM Joining Mesh
  relays: []                     # empty = public venue default
  publicClaims: false            # claims over the public mesh (see below)
  daemonBin: /kvmapp/system/bin/myownmesh
```

`networkId` pins a **custom joining mesh** — leave it empty for the derived
per-device `cec-kvm-…` id. A config still carrying the retired shared default
(`cec-backend-client-mesh`) is migrated to empty on load, and a device that
still holds that network leaves it on the next connect.

`publicClaims` is the device's **claims-over-the-public-mesh policy**, and it
is **strictly device-local**: this config file is the only place it can be
set. There is deliberately no HTTP endpoint and no mesh control message that
mutates it — a remote system (including a mesh-tunneled browser session
riding the auth bypass) must never be able to open a device to public
claiming. Off (the default), an unclaimed KVM's rendezvous meshes are all
LAN-only and the device can only be claimed from the same local network; on,
the joining mesh keeps its relay venue and a claim code is minted (shown in
the web UI's Mesh tab). Flipping it takes effect on the next bridge connect —
the bridge re-joins the joining mesh under the new signaling policy (tracked
in `kvm-state.json` as `joining_public`).

**Why `socket` is separate from `home`.** The data partition (`/data`) is
typically **exFAT/FAT**, which can hold regular files (identity, rosters, state)
but **cannot hold a Unix socket** — `bind()` returns `EPERM`. So the daemon's
control socket lives on **tmpfs** (`/tmp`). The init script pins the daemon to
the same path via `$home/config.json` (`{"daemon":{"control_socket":"…"}}`), and
the bridge dials `mesh.socket`; the two must match. Empty `socket` falls back to
`$home/daemon.sock` (fine only if `home` is on a socket-capable filesystem).

## Deploying a prebuilt release (no local build)

The everyday path needs **no Docker and no toolchain** — it downloads a single
prebuilt **device bundle** and copies it to the device:

```sh
just install <device-ip>          # fetch the bundle, then deploy
# or, in two steps:
just fetch                        # download the latest device bundle (server + daemon)
just deploy <device-ip>
```

`fetch` pulls **one** asset from this repo's GitHub release —
`nanokvm-mesh-riscv64.tar.gz`, built by `.github/workflows/release.yml` — which
bundles **both** the NanoKVM server **and** the MyOwnMesh daemon pinned in
`.myownmesh-rev` (the workflow downloads `myownmesh-linux-riscv64.tar.gz` and
packs it in, the way AllMyStuff bundles the daemon into its app). The `.sha256`
is verified. Pass a tag to pin a version: `just fetch v1.2.3` / `just install
<ip> v1.2.3`.

### Cutting a release

`just release X.Y.Z` bumps the advertised version (`appVersion` in
`server/service/mesh/bridge.go` + `web/package.json`), commits, and pushes the
`vX.Y.Z` tag. That triggers the release workflow, which builds the server,
bundles the `.myownmesh-rev` daemon, and publishes `nanokvm-mesh-riscv64.tar.gz`.
Mirrors MyOwnMesh / AllMyStuff `just release`.

## Building from source

`just build-risc` produces a **complete device build in one step** — the Go
server (`server/NanoKVM-Server`, built in the Docker builder image) **and** the
MyOwnMesh daemon (`kvmapp/system/bin/myownmesh`). The daemon is never compiled
here: it's downloaded from the MyOwnMesh release pinned in `.myownmesh-rev`
(MyOwnMesh cross-compiles it with cargo-zigbuild — a NanoKVM never builds Rust).
A dev box needs only Docker, and only for the server.

```sh
just setup-risc            # one-time: build the Docker builder image
just build-risc            # server (Docker) + pinned daemon (download), one step
just deploy <device-ip>    # scp the server + daemon + init script to a device
just reboot <device-ip>
just verify <device-ip>    # confirm the daemon is serving + its log
just undeploy <device-ip>  # reversible: remove the init script + reboot
```

(`just build-server` builds only the server; `just daemon` only downloads the
daemon. If the pinned MyOwnMesh release has no riscv asset yet, `daemon`/`fetch`
fail with a clear pointer rather than building the wrong thing.)

### Testing against an existing daemon

The bridge dials `$MYOWNMESH_HOME/daemon.sock` and reuses whatever `myownmesh
serve` is already running — it never spawns or builds a daemon. To test:

1. Run a `myownmesh` daemon you already have (built/installed the normal MyOwnMesh
   way — **no cross-compile needed for a non-device daemon**): `myownmesh serve`.
2. Point the bridge at it: set `mesh.home` in `server.yaml` (or `MYOWNMESH_HOME`)
   to that daemon's home, so `<home>/daemon.sock` resolves to the running socket.
3. Start `NanoKVM-Server`; the bridge connects, joins the device's joining
   mesh (logged as `mesh: node id …, joining mesh cec-kvm-…`), and advertises
   the KVM. `just verify` shows the daemon side.

On a device, the daemon is installed separately at `.myownmesh-rev` (build it
with MyOwnMesh's `just build-risc`, or install a release ≥ that rev), and
`S94myownmesh` starts it at boot.

## Packaging

`S94myownmesh` starts the MyOwnMesh daemon with `MYOWNMESH_HOME=/data/myownmesh`
**before** the NanoKVM server (`S95nanokvm`), following the same
copy-to-`/tmp`-and-launch pattern. It also writes the tmpfs control-socket
override into `$home/config.json` on first start (see *Configuration* above).
`just build-risc` stages the pinned daemon at `kvmapp/system/bin/myownmesh`
(matching `mesh.daemonBin`), so it ships in the image. The script no-ops cleanly
if the binary is absent (the bridge then just keeps retrying, so the device stays
a normal KVM).

**Boot dir.** Buildroot's `rcS` runs init scripts from **`/etc/init.d/`** (via
`run-parts … start`). In the repo the script lives in `kvmapp/system/init.d/`,
which the firmware build installs into `/etc/init.d/`. When deploying to an
already-running device, `just deploy` therefore copies `S94myownmesh` straight
into `/etc/init.d/` — copying it only into `/kvmapp/system/init.d/` would not
autostart it.

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
allow-list. `joining_test.go` freezes the joining-mesh derivation;
`membership_test.go` drives mesh add/remove, unclaim-via-Release, the
KVM-<label> renames, and the legacy-mesh migration against a fake daemon.
