package vm

import (
	"net"
	"testing"
)

// The USB gadget interface must classify as USB, not Other.
//
// This is the whole point of reporting it: getInterfaceInfo drops anything
// typed Other, so before this the device published every address it had except
// the one that needs no LAN — leaving a host with the appliance plugged into it
// unable to learn where to reach it. That regression would be silent (one
// address quietly missing from /api/vm/info), so it's pinned here.
func TestUsbGadgetInterfaceIsReported(t *testing.T) {
	for _, name := range []string{"usb0", "usb1"} {
		if got := getInterfaceType(net.Interface{Name: name}); got != USB {
			t.Errorf("getInterfaceType(%q) = %q, want %q", name, got, USB)
		}
	}
}

// The existing classifications must keep working — USB is an addition, not a
// reshuffle, and `usb` must not shadow a wired or wireless name.
func TestInterfaceTypesAreUnchangedForWiredAndWireless(t *testing.T) {
	cases := map[string]string{
		"eth0":   Wired,
		"enp3s0": Wired,
		"wlan0":  Wireless,
		"wlp2s0": Wireless,
		// Not a network interface we report: loopback and the bridges a
		// container runtime leaves lying around stay Other, and Other is
		// dropped by getInterfaceInfo.
		"lo":      Other,
		"docker0": Other,
	}
	for name, want := range cases {
		if got := getInterfaceType(net.Interface{Name: name}); got != want {
			t.Errorf("getInterfaceType(%q) = %q, want %q", name, got, want)
		}
	}
}
