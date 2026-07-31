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

It also lets the drive carry files, which is the point of the two below.

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
