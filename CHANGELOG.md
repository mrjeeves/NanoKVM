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

- **The startup reconcile now converges boot scripts too, not just the daemon.**
  The code that performs an update is the code already on the device, so a
  device updating _from_ an older server is updated by that server's updater —
  which copies only the parts it knows about and silently ignores an `init.d/`
  it has never heard of. The build that added init scripts to the bundle
  therefore couldn't deliver them during the very update that installed it:
  `S03usbdev`, `S32usbdhcp` and `S31usbnet` would have waited for the release
  *after* the one that carries them. The startup reconcile — which already
  fetches this exact version's bundle to heal the identical daemon gap — now
  installs any boot script that differs as well, closing it in one hop. Scripts
  are written only when they actually differ (no rewriting identical files onto
  flash, and "installed" in the log means something changed) and are never run:
  their effects belong to a boot, and one of them composes the USB gadget, which
  under a host that's using it is how a KVM loses its keyboard. They take effect
  at the device's next restart.
- **The RNDIS → NCM migration runs at every startup**, not only inside an
  update. Same reasoning inverted: an update only ever reaches a device whose
  current server already knew to migrate, which is precisely the device that
  doesn't need it. It needs no network, no release tag and no marker, so it also
  converges dev builds and devices whose reconcile has already run — a
  stat-and-return once migrated. The absence of both flags still means "off",
  which is a real choice, and is left alone.
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
  and just before the server restart. Because that logic lives in the server, a
  device updating _from_ an older server can't get the daemon during the very
  update that installs this build — so the new server also **reconciles its
  daemon once on startup**: if the installed binary doesn't match the one this
  release pins, it fetches and swaps it in and restarts the daemon, converging
  the fleet with no on-site deploy. The check runs once per version (a marker
  under the mesh home dir), so ordinary boots do nothing.

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
