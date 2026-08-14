package storage

import (
	"os"
	"strings"
	"testing"
)

func TestEnsureUSBGadgetLeavesBoundControllerAloneWhenPeripheralFlagIsZero(t *testing.T) {
	oldRead, oldWrite := usbReadFile, usbWriteFile
	defer func() { usbReadFile, usbWriteFile = oldRead, oldWrite }()

	peripheralFlagRead := false
	usbReadFile = func(path string) ([]byte, error) {
		if path == usbGadgetUDC {
			return []byte("4340000.usb\n"), nil
		}
		if strings.HasSuffix(path, "is_a_peripheral") {
			peripheralFlagRead = true
			return []byte("0\n"), nil
		}
		t.Fatalf("unexpected USB health read %q", path)
		return nil, nil
	}
	usbWriteFile = func(string, []byte, os.FileMode) error {
		t.Fatal("bound USB gadget was modified")
		return nil
	}

	if err := ensureUSBGadgetBound(); err != nil {
		t.Fatal(err)
	}
	if peripheralFlagRead {
		t.Fatal("bound gadget health consulted the unreliable is_a_peripheral flag")
	}
}

func TestUSBInitScriptSelectsDeviceRoleBeforeBinding(t *testing.T) {
	script, err := os.ReadFile("../../../kvmapp/system/init.d/S03usbdev")
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
	assertOrder := func(name, block, next string) {
		t.Helper()
		start := strings.Index(text, block)
		if start < 0 {
			t.Fatalf("could not find %s function", name)
		}
		end := strings.Index(text[start+len(block):], next)
		if end < 0 {
			t.Fatalf("could not find end of %s function", name)
		}
		body := text[start : start+len(block)+end]
		role := strings.Index(body, "echo device > /proc/cviusb/otg_role")
		bind := strings.Index(body, "ls /sys/class/udc/")
		if role < 0 || bind < 0 || role > bind {
			t.Fatalf("%s must select device role before binding UDC", name)
		}
	}
	assertOrder("start_usb_dev", "start_usb_dev(){", "start_usb_host(){")
	assertOrder("restart_usb_dev", "restart_usb_dev(){", "case \"$1\" in")
}
