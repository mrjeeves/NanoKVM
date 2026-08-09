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

// stage points the package at a temp dir.
func stage(t *testing.T) (image, lun string) {
	t.Helper()
	dir := t.TempDir()
	image, lun = filepath.Join(dir, "usbdisk.img"), filepath.Join(dir, "lun.file")
	oi, ol := kvmDriveImageForTest, lunFileForTest
	kvmDriveImageForTest, lunFileForTest = image, lun
	t.Cleanup(func() { kvmDriveImageForTest, lunFileForTest = oi, ol })
	return image, lun
}

// installUsbDisk stages and renames, so a refresh swaps the inode while the
// gadget still holds the old one open. Re-pointing the LUN is the only way the
// host sees the new drive.
func TestDetachThenReattachRestoresOurImage(t *testing.T) {
	image, lun := stage(t)
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
	image, lun := stage(t)
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
