package storage

import (
	"os"
	"strings"

	log "github.com/sirupsen/logrus"
)

// Keeping the KVM's own USB drive attached across an image swap.
//
// installUsbDisk stages and renames, so a refresh replaces the inode — but the
// gadget opened the OLD one when the LUN was configured and keeps using it.
// Swapping it underneath leaves the host mid-transaction with a block device
// whose identity changed; detaching first and re-pointing after turns that into
// an ordinary media change.
//
// This file used to also REBUILD the gadget at startup, to export a drive
// S03usbdev had skipped. That is gone: rebuilding unbinds and rebinds the UDC,
// which tears down and recreates /dev/hidg0-2 — and every other gadget change
// in this server closes HID around that (service/vm/virtual-device.go), which
// this did not. A drive that appears one boot later is a trivial cost; keyboard
// and mouse that stop working are not.
const (
	kvmDriveImage = "/data/usbdisk.img"
	// lunFile is the function's backing-file attribute. Its absence means
	// S03usbdev composed no mass_storage function this boot, and there is
	// nothing for the detach/re-attach pair to do.
	lunFile = "/sys/kernel/config/usb_gadget/g0/functions/mass_storage.disk0/lun.0/file"
)

// Indirected for tests.
var (
	kvmDriveImageForTest = kvmDriveImage
	lunFileForTest       = lunFile
)

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
