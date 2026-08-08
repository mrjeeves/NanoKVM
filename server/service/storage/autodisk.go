package storage

import (
	"os"
	"os/exec"
	"strings"

	log "github.com/sirupsen/logrus"
)

// The KVM's own USB drive is exported by S03usbdev at boot, and that is the
// problem this file exists to fix.
//
// S03usbdev runs at S03. It resolves the backing file once — "is
// /data/usbdisk.img a filesystem?" — and if the answer is no it composes no
// mass_storage function at all and says "no drive this boot". The server that
// WRITES that image runs at S95, ninety-two scripts later. So on the first boot
// after any update that installs or refreshes the drive, S03 has already
// decided there is no drive before the image exists, and nothing recomposes the
// gadget afterwards: the drive is missing until somebody reboots. On a device
// that has been up a while it works, which is exactly why this reads as an
// intermittent "USB failure" rather than the ordering bug it is.
//
// There is a second, quieter half. installUsbDisk stages and renames, so a
// refresh replaces the inode — but the gadget opened the OLD one when the LUN
// was configured and keeps using it. The host then sees the previous drive
// contents indefinitely, with every file on disk looking correct.
const (
	kvmDriveImage = "/data/usbdisk.img"
	// usbDiskFlag is S03usbdev's switch: present means "export a drive",
	// EMPTY-but-present means "use /data/usbdisk.img". Absent means the
	// operator turned the virtual disk off, which we must never override.
	usbDiskFlag = "/boot/usb.disk0"
	// lunFile is the function's backing-file attribute. Its absence is how we
	// tell that S03usbdev composed no mass_storage function this boot.
	lunFile = "/sys/kernel/config/usb_gadget/g0/functions/mass_storage.disk0/lun.0/file"
)

// Indirected for tests.
var (
	kvmDriveImageForTest = kvmDriveImage
	usbDiskFlagForTest   = usbDiskFlag
	lunFileForTest       = lunFile
	runShell             = func(cmd string) error { return exec.Command("sh", "-c", cmd).Run() }
)

// composeDiskCommands rebuilds the gadget so a mass_storage function that was
// skipped at boot gets composed. Mirrors the web UI's own "mount disk" path:
// `stop` unbinds the UDC and flips the OTG role, `start` re-runs S03usbdev's
// composition, which creates the function it skipped now that the image is
// there. Nothing else in the gadget is disturbed — every other mkdir/ln simply
// fails "File exists" and is stepped over.
const composeDiskCommands = "/etc/init.d/S03usbdev stop; /etc/init.d/S03usbdev start"

// EnsureDriveExported gives the attached machine its drive on the boot the
// image lands, instead of the one after. Called at startup, once the image the
// updater wrote is on disk.
//
// Conservative by construction: it does nothing unless the operator has the
// virtual disk switched on, the image is a real filesystem, and the gadget has
// no mass_storage function. Rebuilding the gadget re-enumerates HID, so doing it
// on a device that is already fine would cost a keyboard and mouse blip for no
// reason.
func EnsureDriveExported() {
	if _, err := os.Stat(usbDiskFlagForTest); err != nil {
		return // virtual disk switched off — the operator's call, not ours
	}
	if !driveImageReady(kvmDriveImageForTest) {
		return // no drive to export (or a half-written one, which is worse)
	}
	if _, err := os.Stat(lunFileForTest); err == nil {
		return // already composed; leave the gadget alone
	}

	log.Infof("usb drive: image is ready but no mass_storage function this boot — composing it")
	if err := runShell(composeDiskCommands); err != nil {
		log.Warnf("usb drive: compose failed: %s", err)
		return
	}
	if _, err := os.Stat(lunFileForTest); err != nil {
		log.Warnf("usb drive: gadget rebuilt but still no mass_storage function")
		return
	}
	log.Infof("usb drive attached (%s)", kvmDriveImageForTest)
}

// DetachDriveBacking releases the drive image so it can be replaced, and
// reports whether it did.
//
// This MUST happen before the updater swaps the image. installUsbDisk stages
// and renames, and the gadget has that exact path open as its backing store —
// replacing the inode underneath a live LUN leaves the host mid-transaction
// with a block device whose identity changed, which is what surfaces on Windows
// as "USB device not recognized". Detaching first turns that into an ordinary
// media-removed event, which every OS already knows how to handle.
//
// Returns false when there is nothing composed, or when the LUN points at
// something that is not ours — virtual media the operator mounted is theirs,
// and a drive refresh must never eject it.
func DetachDriveBacking() bool {
	if _, err := os.Stat(lunFileForTest); err != nil {
		return false
	}
	current, err := os.ReadFile(lunFileForTest)
	if err != nil {
		return false
	}
	if strings.TrimSpace(string(current)) != kvmDriveImageForTest {
		return false
	}
	if err := os.WriteFile(lunFileForTest, []byte("\n"), 0o666); err != nil {
		log.Warnf("usb drive: detach before refresh: %s", err)
		return false
	}
	return true
}

// ReattachDriveBacking points the LUN back at the drive image. Pair it with
// DetachDriveBacking on every path, including the ones where the install did
// nothing — leaving the host with no media because an update decided not to
// rewrite the drive would be a worse bug than the one this avoids.
func ReattachDriveBacking() {
	if _, err := os.Stat(lunFileForTest); err != nil {
		return
	}
	if !driveImageReady(kvmDriveImageForTest) {
		log.Warnf("usb drive: image is not a filesystem after refresh; leaving the LUN empty")
		return
	}
	if err := os.WriteFile(lunFileForTest, []byte(kvmDriveImageForTest), 0o666); err != nil {
		log.Warnf("usb drive: re-attach after refresh: %s", err)
		return
	}
	log.Infof("usb drive: re-attached after refresh")
}

// driveImageReady reports whether path is a file carrying a FAT boot signature.
// The same test S03usbdev makes, for the same reason: presence is not
// readiness, and handing a host a half-written volume makes Windows demand the
// customer format their KVM — worse than no drive at all.
func driveImageReady(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	var sig [2]byte
	if _, err := f.ReadAt(sig[:], 510); err != nil {
		return false
	}
	return sig[0] == 0x55 && sig[1] == 0xAA
}
