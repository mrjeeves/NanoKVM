package storage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEnsureUSBGadgetRepairsBoundControllerInHostMode(t *testing.T) {
	oldRead, oldWrite, oldSleep := usbReadFile, usbWriteFile, usbSleep
	defer func() { usbReadFile, usbWriteFile, usbSleep = oldRead, oldWrite, oldSleep }()

	usbReadFile = func(path string) ([]byte, error) {
		switch path {
		case usbGadgetUDC:
			return []byte("4340000.usb\n"), nil
		case filepath.Join(usbUDCClass, "4340000.usb", "is_a_peripheral"):
			return []byte("0\n"), nil
		default:
			t.Fatalf("unexpected read %q", path)
			return nil, nil
		}
	}
	var writes []string
	usbWriteFile = func(path string, data []byte, _ os.FileMode) error {
		writes = append(writes, path+"="+strings.TrimSpace(string(data)))
		return nil
	}
	usbSleep = func(time.Duration) {}

	if err := ensureUSBGadgetBound(); err != nil {
		t.Fatal(err)
	}
	want := []string{
		usbGadgetUDC + "=",
		usbOTGRole + "=device",
		usbGadgetUDC + "=4340000.usb",
	}
	if strings.Join(writes, "|") != strings.Join(want, "|") {
		t.Fatalf("USB recovery writes = %q, want safe order %q", writes, want)
	}
}

func TestEnsureUSBGadgetLeavesHealthyPeripheralBound(t *testing.T) {
	oldRead, oldWrite := usbReadFile, usbWriteFile
	defer func() { usbReadFile, usbWriteFile = oldRead, oldWrite }()

	usbReadFile = func(path string) ([]byte, error) {
		if path == usbGadgetUDC {
			return []byte("4340000.usb\n"), nil
		}
		return []byte("1\n"), nil
	}
	usbWriteFile = func(string, []byte, os.FileMode) error {
		t.Fatal("healthy USB gadget was modified")
		return nil
	}

	if err := ensureUSBGadgetBound(); err != nil {
		t.Fatal(err)
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
