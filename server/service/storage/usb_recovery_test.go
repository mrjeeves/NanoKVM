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
	assertOrder("start_usb_dev", "start_usb_dev(){", "stop_usb_dev(){")
	assertOrder("restart_usb_dev", "restart_usb_dev(){", "case \"$1\" in")
}

func TestUSBInitScriptNeverEntersHostModeForGadgetLifecycle(t *testing.T) {
	script, err := os.ReadFile("../../../kvmapp/system/init.d/S03usbdev")
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
	if strings.Contains(text, "echo host > /proc/cviusb/otg_role") {
		t.Fatal("gadget lifecycle must not switch the KVM port to host mode while attached to a computer")
	}

	stop := strings.Index(text, "stop_usb_dev(){")
	restart := strings.Index(text, "restart_usb_dev(){")
	if stop < 0 || restart < 0 || stop > restart {
		t.Fatal("could not locate stop_usb_dev")
	}
	stopBody := text[stop:restart]
	unbind := strings.Index(stopBody, "echo > /sys/kernel/config/usb_gadget/g0/UDC")
	device := strings.Index(stopBody, "echo device > /proc/cviusb/otg_role")
	if unbind < 0 || device < 0 || unbind > device {
		t.Fatal("stop_usb_dev must unbind configfs before reaffirming device role")
	}

	restartPHY := strings.Index(text, "restart_phy)")
	if restartPHY < 0 {
		t.Fatal("could not locate restart_phy")
	}
	restartBody := text[restartPHY:]
	stopCall := strings.Index(restartBody, "stop_usb_dev")
	driverUnbind := strings.Index(restartBody, "/sys/bus/platform/drivers/dwc2/unbind")
	if stopCall < 0 || driverUnbind < 0 || stopCall > driverUnbind {
		t.Fatal("restart_phy must disconnect configfs before unbinding the DWC2 controller")
	}
}
