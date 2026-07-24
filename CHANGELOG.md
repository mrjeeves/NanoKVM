# AllMyKVM changelog

The release history of **this fork** — Critical Error Computing's AllMyStuff /
CEC integration built on top of Sipeed's NanoKVM. Entries below are our own
`vX.Y.Z` releases (the version the device advertises on the mesh and the one the
Update tab installs — `server/buildinfo`, never the Sipeed base image's
`/kvmapp/version`).

When a release re-bases onto a newer upstream Sipeed firmware, the new upstream
baseline is called out inline, so our version and the Sipeed version underneath
it never drift silently apart. Sipeed's full upstream changelog is preserved
verbatim in [`CHANGELOG.upstream.md`](CHANGELOG.upstream.md).

## Unreleased

- **Firmware updates run off our own version and release channel.** The stock
  Sipeed updater — which installs over `/kvmapp` and clobbers our mesh server —
  is removed, both the web UI and the server routes. Settings → Update now
  installs our own GitHub-released bundle (`nanokvm-mesh-riscv64.tar.gz`),
  verified by sha256, and it's password-free over the AllMyStuff mesh. The
  version the updater compares is our fork's number (`server/buildinfo`), so a
  device no longer reads as the unrelated upstream `2.x` from `/kvmapp/version`.
- **MyOwnMesh daemon pinned to v0.3.2** (`.myownmesh-rev`) — picks up the
  0.3.2 mesh-connectivity fixes.
- **Over-the-air updates now ship the pinned daemon too.** Settings → Update
  installs the bundled myownmesh and restarts it whenever the pinned binary
  actually changed (a sha256 compare), so a daemon-side fix reaches the fleet
  over the mesh — previously an update swapped only the server + web and a
  daemon bump needed an on-site `just deploy`. An unchanged daemon is left
  completely untouched, so an ordinary update never disturbs the mesh tunnel;
  when the daemon does change it is bounced after the update response is sent
  and just before the server restart.

## 0.1.0

First AllMyKVM release — the NanoKVM as a first-class AllMyStuff mesh appliance:

- Pure-Go **MyOwnMesh bridge** (`server/service/mesh/`) with a bundled daemon
  pinned in `.myownmesh-rev`.
- **LAN-first claiming** over the mDNS rendezvous mesh; **zero-login** web
  access tunnelled over the mesh "sites" plane.
- Full **KVM-node lifecycle**: presence advertising, fleet membership,
  attach/detach to the machine it controls, remote restart, and unclaim.
- **CEC hand-raise** on the CEC Support help queue — a beacon on the
  `cecsupport-clients` mesh, raised from the web UI or the **BOOT button**.

_Upstream baseline: Sipeed NanoKVM **2.4.3**._
