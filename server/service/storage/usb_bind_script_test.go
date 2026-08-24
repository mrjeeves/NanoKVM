//go:build linux

package storage

// usb_bind_script_test.go RUNS S03usbdev's bind_udc against a fake sysfs,
// rather than asserting on the script's text like its neighbours do.
//
// That distinction is the point. The bug this function replaces was not a
// missing string, it was a write that reported success and left the gadget
// bound to nothing — and no amount of grepping the script proves the retry and
// read-back actually behave. The device this ships to cannot be brought into
// CI, so executing the logic against a directory we control is the closest
// thing to a real test available, and it caught the "write does not stick" case
// that matters most.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const initScript = "../../../kvmapp/system/init.d/S03usbdev"

// extractBindUDC pulls the helper out of the init script so it can be sourced
// on its own. The rest of the script cds into /sys/kernel/config and sources
// /etc/profile, neither of which exists off-device.
func extractBindUDC(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(initScript)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	start := strings.Index(text, "UDC_CLASS_DIR=")
	if start < 0 {
		t.Fatal("could not find bind_udc's preamble in S03usbdev")
	}
	end := strings.Index(text[start:], "\nstop_usb_dev(){")
	if end < 0 {
		t.Fatal("could not find the end of bind_udc in S03usbdev")
	}
	fn := filepath.Join(t.TempDir(), "bind_udc.sh")
	if err := os.WriteFile(fn, []byte(text[start:start+end]), 0o644); err != nil {
		t.Fatal(err)
	}
	return fn
}

// runBindUDC sources the helper and calls it against a fake /sys/class/udc,
// returning whether it reported success.
func runBindUDC(t *testing.T, helper, classDir, udcFile string) (bool, string) {
	t.Helper()
	script := ". " + helper + "\nbind_udc " + udcFile + "\n"
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = append(os.Environ(),
		"UDC_CLASS_DIR="+classDir,
		"BIND_UDC_RETRIES=3",
		"BIND_UDC_DELAY=0", // no real sleeping in tests
	)
	out, err := cmd.CombinedOutput()
	return err == nil, string(out)
}

// The ordinary case: a controller is present and the write sticks.
func TestBindUDCBindsWhenControllerIsPresent(t *testing.T) {
	helper := extractBindUDC(t)
	root := t.TempDir()
	classDir := filepath.Join(root, "udc")
	if err := os.MkdirAll(filepath.Join(classDir, "4340000.usb"), 0o755); err != nil {
		t.Fatal(err)
	}
	udcFile := filepath.Join(root, "UDC")
	if err := os.WriteFile(udcFile, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	ok, out := runBindUDC(t, helper, classDir, udcFile)
	if !ok {
		t.Fatalf("bind_udc failed with a controller present:\n%s", out)
	}
	bound, err := os.ReadFile(udcFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(bound)) != "4340000.usb" {
		t.Fatalf("UDC = %q, want the controller name", strings.TrimSpace(string(bound)))
	}
}

// No controller at all: must give up and say so, not hang the boot. S03 runs
// ahead of the network and the server on a slow single core, so an unbounded
// wait here would cost the whole device, not just USB.
func TestBindUDCFailsWhenNoControllerExists(t *testing.T) {
	helper := extractBindUDC(t)
	root := t.TempDir()
	classDir := filepath.Join(root, "empty")
	if err := os.MkdirAll(classDir, 0o755); err != nil {
		t.Fatal(err)
	}
	udcFile := filepath.Join(root, "UDC")
	if err := os.WriteFile(udcFile, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	ok, out := runBindUDC(t, helper, classDir, udcFile)
	if ok {
		t.Fatalf("bind_udc reported success with no controller:\n%s", out)
	}
	if !strings.Contains(out, "FAILED to bind the gadget") {
		t.Errorf("bind_udc gave up without saying so:\n%s", out)
	}
}

// The case the read-back exists for, and the one the old code could not see: a
// write that succeeds and does not stick. /dev/null accepts every write and
// always reads back empty — exactly the shape of the failure that left devices
// in the field with a composed gadget attached to no controller.
func TestBindUDCDetectsAWriteThatDoesNotStick(t *testing.T) {
	helper := extractBindUDC(t)
	classDir := filepath.Join(t.TempDir(), "udc")
	if err := os.MkdirAll(filepath.Join(classDir, "4340000.usb"), 0o755); err != nil {
		t.Fatal(err)
	}

	ok, out := runBindUDC(t, helper, classDir, "/dev/null")
	if ok {
		t.Fatalf("bind_udc accepted a bind that did not stick — the read-back is not working:\n%s", out)
	}
}

// A controller that appears late must still be picked up: at boot the KVM and
// the host initialise together, and the first look at /sys/class/udc is
// expected to come up empty. The unchecked `sleep 1` this replaced is exactly
// where that race was lost.
func TestBindUDCRetriesUntilTheControllerAppears(t *testing.T) {
	helper := extractBindUDC(t)
	root := t.TempDir()
	classDir := filepath.Join(root, "udc")
	if err := os.MkdirAll(classDir, 0o755); err != nil {
		t.Fatal(err)
	}
	udcFile := filepath.Join(root, "UDC")
	if err := os.WriteFile(udcFile, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	// Appear on the second attempt, driven by the helper's own retry loop
	// rather than by a timer, so the test cannot flake. The counter lives in a
	// file because bind_udc calls ls inside a command substitution — a shell
	// variable would be incremented in a subshell and reset every attempt.
	counter := filepath.Join(root, "attempts")
	script := ". " + helper + `
_orig_ls_dir="$UDC_CLASS_DIR"
_counter="` + counter + `"
: > "$_counter"
ls() {
    echo x >> "$_counter"
    if [ "$(wc -l < "$_counter")" -ge 2 ]; then
        mkdir -p "$_orig_ls_dir/4340000.usb"
    fi
    command ls "$@"
}
bind_udc ` + udcFile + `
`
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = append(os.Environ(),
		"UDC_CLASS_DIR="+classDir,
		"BIND_UDC_RETRIES=5",
		"BIND_UDC_DELAY=0",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bind_udc gave up on a controller that appeared late:\n%s", out)
	}
	bound, readErr := os.ReadFile(udcFile)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.TrimSpace(string(bound)) != "4340000.usb" {
		t.Fatalf("UDC = %q, want the late-appearing controller", strings.TrimSpace(string(bound)))
	}
}
