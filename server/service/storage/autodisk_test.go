package storage

import (
	"os"
	"path/filepath"
	"testing"
)

// fatImage is a file that passes the same boot-signature test S03usbdev makes.
func fatImage(t *testing.T, path string) {
	t.Helper()
	img := make([]byte, 1024)
	img[510], img[511] = 0x55, 0xAA
	if err := os.WriteFile(path, img, 0o644); err != nil {
		t.Fatal(err)
	}
}

// stage points the package at a temp dir and records shell invocations instead
// of running them. Returns the paths and a pointer to the recorded commands.
func stage(t *testing.T) (image, flag, lun string, ran *[]string) {
	t.Helper()
	dir := t.TempDir()
	image = filepath.Join(dir, "usbdisk.img")
	flag = filepath.Join(dir, "usb.disk0")
	lun = filepath.Join(dir, "lun.file")

	oi, of, ol, or := kvmDriveImageForTest, usbDiskFlagForTest, lunFileForTest, runShell
	var calls []string
	kvmDriveImageForTest, usbDiskFlagForTest, lunFileForTest = image, flag, lun
	runShell = func(cmd string) error {
		calls = append(calls, cmd)
		// Model S03usbdev: composing the gadget creates the LUN attribute.
		return os.WriteFile(lun, []byte("\n"), 0o644)
	}
	t.Cleanup(func() {
		kvmDriveImageForTest, usbDiskFlagForTest, lunFileForTest, runShell = oi, of, ol, or
	})
	return image, flag, lun, &calls
}

// The reported failure: the image lands after S03usbdev already decided there
// was no drive, so the gadget carries no mass_storage function and the drive is
// missing until somebody reboots.
func TestEnsureDriveExportedComposesAMissingFunction(t *testing.T) {
	image, flag, lun, ran := stage(t)
	fatImage(t, image)
	if err := os.WriteFile(flag, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	EnsureDriveExported()

	if len(*ran) != 1 || (*ran)[0] != composeDiskCommands {
		t.Fatalf("expected one compose, got %v", *ran)
	}
	if _, err := os.Stat(lun); err != nil {
		t.Fatal("the LUN was not composed")
	}
}

// Rebuilding the gadget re-enumerates HID, so a device that is already fine
// must not be touched — that would cost a keyboard and mouse blip for nothing.
func TestEnsureDriveExportedLeavesAWorkingGadgetAlone(t *testing.T) {
	image, flag, lun, ran := stage(t)
	fatImage(t, image)
	_ = os.WriteFile(flag, nil, 0o644)
	_ = os.WriteFile(lun, []byte(image), 0o644)

	EnsureDriveExported()

	if len(*ran) != 0 {
		t.Fatalf("a composed gadget was rebuilt anyway: %v", *ran)
	}
}

func TestEnsureDriveExportedRespectsTheOperatorsSwitch(t *testing.T) {
	image, _, _, ran := stage(t)
	fatImage(t, image)
	// No /boot/usb.disk0: the operator turned the virtual disk off.

	EnsureDriveExported()

	if len(*ran) != 0 {
		t.Fatalf("the disabled switch was overridden: %v", *ran)
	}
}

// A half-written image is a drive Windows asks the customer to format, which is
// worse than no drive — the same rule S03usbdev applies.
func TestEnsureDriveExportedSkipsAnUnformattedImage(t *testing.T) {
	image, flag, _, ran := stage(t)
	if err := os.WriteFile(image, make([]byte, 1024), 0o644); err != nil { // no 55AA
		t.Fatal(err)
	}
	_ = os.WriteFile(flag, nil, 0o644)

	EnsureDriveExported()

	if len(*ran) != 0 {
		t.Fatalf("an unformatted image was exported: %v", *ran)
	}
}

// installUsbDisk stages and renames, so a refresh swaps the inode while the
// gadget still holds the old one open. Re-pointing the LUN is the only way the
// host sees the new drive.
func TestDetachThenReattachRestoresOurImage(t *testing.T) {
	image, _, lun, _ := stage(t)
	fatImage(t, image)
	_ = os.WriteFile(lun, []byte(image+"\n"), 0o644)

	if !DetachDriveBacking() {
		t.Fatal("our own image should have been detached")
	}
	ReattachDriveBacking()

	got, err := os.ReadFile(lun)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != image {
		t.Fatalf("LUN = %q, want it re-pointed at %q", got, image)
	}
}

// Virtual media shares this LUN. If the operator has an ISO mounted, that is
// what they are looking at — a refresh must not yank it out from under them.
func TestDetachLeavesMountedMediaAlone(t *testing.T) {
	image, _, lun, _ := stage(t)
	fatImage(t, image)
	_ = os.WriteFile(lun, []byte("/data/windows-11.iso\n"), 0o644)

	if DetachDriveBacking() {
		t.Fatal("mounted media must never be detached by a drive refresh")
	}

	got, _ := os.ReadFile(lun)
	if string(got) != "/data/windows-11.iso\n" {
		t.Fatalf("mounted media was displaced: %q", got)
	}
}
