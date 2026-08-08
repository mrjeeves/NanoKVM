# The KVM's USB drive

What the customer sees in Explorer or Finder when a CEC KVM is plugged in.

The image is built **in CI**, not on the device (`.github/workflows/release.yml`),
and shipped in the release bundle. That is deliberate on two counts.

It used to be made on first boot, by `S03usbdev`, which put `mkfs` in the boot
path of a slow single-core board — ahead of the network, the mesh and the
server. Worse, the "is it already there?" check was `[ -s image ]`: a file that
had been created but not successfully formatted (a boot cut short by a reboot,
say) passed it, so from then on every boot exported an unformatted volume and
Windows asked the customer to format their KVM. Building it once, in a place
with a real toolchain, removes both the boot-time work and the half-made state.

It also lets the drive carry files, which is the point of everything below.

## autorun.inf

**Not for running anything.** Windows disabled AutoRun for removable drives in
2011 (KB971029, after Conficker) and macOS never had it; an installer here would
never launch itself, and shouldn't. What `autorun.inf` still does on a removable
volume is set the drive's **icon and name** in Explorer — so the customer sees
*CEC KVM* with our mark rather than a nameless removable disk.

CRLF line endings and 8.3-safe filenames, because Windows parses this file with
very old code.

## cec.ico

The CEC Support app icon, so the drive and the app the customer runs look like
the same thing. Kept in sync by hand with `gui/src-tauri/icons/icon.ico` in the
CECSupport repo.

## cecsupport.ps1 + CEC-Support.cmd

The route to the desktop app from the machine the KVM is plugged into. Before
this the only route was the web UI's "Desktop App" button pointing at
support.cec.direct, which requires the customer to already know where to go.

`cecsupport.ps1` asks GitHub for the **latest** CEC Support release, downloads
its x64 installer, checks the download against the digest GitHub reports for
that asset, and runs it. Nothing is pinned: the app updates itself on first run,
so a pinned install would be a stale install for about a minute, and a KVM
flashed a year ago would otherwise hand out a year-old installer.

Carrying the launcher rather than the ~37 MB installer keeps the whole drive
image under 100 KB gzipped — it rides inside every over-the-air update, and
paying 37 MB on each one for a file used once is the wrong trade.

The digest check is integrity, not provenance: the hash comes from the same
place as the file, so it proves the download was not truncated or corrupted,
and nothing more. That is the failure that actually happens on a flaky
connection, and running a half-downloaded installer is worth refusing.

`CEC-Support.cmd` is what the customer double-clicks. A `.ps1` on removable
media is not double-clickable — Windows opens it in Notepad, and the default
execution policy blocks scripts that came off removable media. The `.cmd`
invokes PowerShell with `-ExecutionPolicy Bypass` for that one call, changing
nothing on the machine.

Both files are written to the drive with CRLF endings by the build step, for
the same reason `autorun.inf` needs them.

## Building the drive

`scripts/build-usbdisk.sh` — one build step, called by both `just usbdisk` /
`just deploy` and release CI. The mkfs/mcopy sequence used to be copy-pasted
between the Justfile and `.github/workflows/release.yml`, which was survivable
while the drive held two static files and stops being survivable the moment its
contents matter: a release quietly shipping a different drive than a dev deploy
is the kind of bug nobody finds until a customer is on the phone.

It fails if any expected file did not land, for the same reason the device
checks the FAT signature before exporting the drive.

## This does not reach devices already in the field

`installUsbDisk` (server/service/application/update.go) never replaces a drive
that is already formatted — it is the customer's drive, their files are theirs,
and an update that silently emptied it would be a worse bug than any it fixed.
So the launcher reaches devices provisioned from this release onward, plus any
whose drive is missing or unformatted. Getting it onto a drive already out there
needs a deliberate, customer-initiated re-provision, which does not exist yet —
a "restore the KVM drive" action in the web UI is the obvious shape, since
wiping is then their choice rather than an update's side effect.
