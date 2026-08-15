# CEC KVM changelog

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

- **Virtual-device and install-media changes no longer switch the KVM's USB
  port into host mode while it is connected to a computer.** `S03usbdev stop`
  is the live gadget-recomposition path, not an instruction to make the KVM a
  USB host. It now disconnects configfs while remaining in peripheral mode,
  and PHY recovery disconnects the gadget before resetting DWC2. This prevents
  disk/network toggles and recovery from applying host signaling and
  termination to a port physically wired to another host, which can strand
  keyboard, mouse, network, and media together.

- **Server startup no longer resets a healthy USB gadget.** The CVI driver's
  `is_a_peripheral` file reads `0` on working, host-visible NanoKVMs, so it is
  not a valid health check. Startup now trusts configfs' populated `UDC`
  binding and repairs only an actually unbound gadget. This keeps keyboard,
  mouse, network, and storage enumerated across an app update or restart.

- **A failed or interrupted install-media mount can no longer take down USB.**
  Media changes are transactional now: the prior LUN state is restored on an
  error, UDC rebinding is retried, and keyboard/mouse handles are reopened only
  after the composite gadget is live. Unmounting remote media returns to the
  validated `/data/usbdisk.img` instead of exporting the raw `/data` partition.
  Server startup also rebinds a blank UDC and ejects dead FUSE media left by an
  interrupted stream, so installing this update repairs a KVM that an older
  build already left without USB.

- **The USB drive comes formatted, named "CEC KVM", and carrying our icon.** It
  is built in CI now (`support/usbdisk/`) and shipped in the release bundle,
  rather than being made on the device at first boot. Building it there was
  wrong twice over: `mkfs` on a multi-gigabyte file ran at `S03`, in the boot
  path of a slow single-core board ahead of the network and the server; and the
  "is it already there?" test was `[ -s image ]`, which a file created but not
  successfully formatted passes. One interrupted first boot — a reboot landing
  mid-format, so the cleanup never ran — left an unformatted volume that every
  boot after happily exported, and Windows asked the customer to format their
  KVM. Forever, because the next boot saw a non-empty file and asked no further
  questions. `S03usbdev` now only checks the FAT boot signature before handing
  the image to a host, so a half-made one is never exported again, and the
  server installs the real one off the boot path. A formatted image already on
  the device is the customer's drive and is never replaced. The bundled
  `autorun.inf` is not for running anything — Windows disabled AutoRun for
  removable drives in 2011 — but on a removable volume it still sets the drive's
  icon and name.

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

First CEC KVM release — the NanoKVM as a first-class AllMyStuff mesh appliance:

- Pure-Go **MyOwnMesh bridge** (`server/service/mesh/`) with a bundled daemon
  pinned in `.myownmesh-rev`.
- **LAN-first claiming** over the mDNS rendezvous mesh; **zero-login** web
  access tunnelled over the mesh "sites" plane.
- Full **KVM-node lifecycle**: presence advertising, fleet membership,
  attach/detach to the machine it controls, remote restart, and unclaim.
- **CEC hand-raise** on the CEC Support help queue — a beacon on the
  `cecsupport-clients` mesh, raised from the web UI or the **BOOT button**.

_Upstream baseline: Sipeed NanoKVM **2.4.3**._
