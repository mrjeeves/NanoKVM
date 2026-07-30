package vm

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	"NanoKVM-Server/proto"
	"NanoKVM-Server/service/hid"
)

const (
	// virtualNetwork is the flag this toggle WRITES: RNDIS, what this model has
	// always used. S03usbdev also understands /boot/usb.ncm and prefers it when
	// both exist (see virtualNetworkNcm) — NCM being the variant macOS speaks,
	// where RNDIS is Windows-oriented. Switching the default is a deliberate
	// change for another day; what matters here is that the read and teardown
	// paths below know about both, so a hand-placed NCM flag (an SD card edited
	// on a PC, the only way to configure a KVM that has never had a network)
	// can't leave this toggle reporting "off" for a gadget that is plainly up,
	// nor surviving a turn-off that only ever removed the RNDIS half.
	virtualNetwork    = "/boot/usb.rndis0"
	virtualNetworkNcm = "/boot/usb.ncm"
	virtualDisk       = "/boot/usb.disk0"
)

var (
	mountNetworkCommands = []string{
		"touch /boot/usb.rndis0",
		"/etc/init.d/S03usbdev stop",
		"/etc/init.d/S03usbdev start",
		// Hand the tethered host an address. Without this the gadget comes up
		// and the host's USB adapter sits unaddressed, so the KVM's own usb0
		// address is unreachable from the one machine that should always be
		// able to see it.
		"/etc/init.d/S32usbdhcp start",
		// Share the KVM's uplink internet with the tethered host, so the
		// virtual network extends the host's connectivity instead of
		// black-holing its default route. Best-effort (the script always
		// exits 0), so it can't fail the toggle.
		"/etc/init.d/S31usbnet start",
	}

	// Off means off, whichever variant is up. Removing only the RNDIS half left
	// an NCM gadget running with the UI reporting it gone — and S03usbdev
	// prefers NCM, so the next start would rebuild it. `rm -f` so a missing
	// flag isn't an error: normally only one of the two is present.
	unmountNetworkCommands = []string{
		"/etc/init.d/S31usbnet stop",
		"/etc/init.d/S32usbdhcp stop",
		"/etc/init.d/S03usbdev stop",
		"rm -rf /sys/kernel/config/usb_gadget/g0/configs/c.1/rndis.usb0",
		"rm -rf /sys/kernel/config/usb_gadget/g0/configs/c.1/ncm.usb0",
		"rm -f /boot/usb.rndis0",
		"rm -f /boot/usb.ncm",
		"/etc/init.d/S03usbdev start",
	}

	mountDiskCommands = []string{
		"touch /boot/usb.disk0",
		"/etc/init.d/S03usbdev stop",
		"/etc/init.d/S03usbdev start",
	}

	unmountDiskCommands = []string{
		"/etc/init.d/S03usbdev stop",
		"rm -rf /sys/kernel/config/usb_gadget/g0/configs/c.1/mass_storage.disk0",
		"rm /boot/usb.disk0",
		"/etc/init.d/S03usbdev start",
	}
)

func (s *Service) GetVirtualDevice(c *gin.Context) {
	var rsp proto.Response

	// Either flag means the gadget is up — S03usbdev builds NCM from one and
	// RNDIS from the other, and reading only RNDIS reported "off" for a live
	// NCM link the user could see on their machine.
	network, _ := isDeviceExist(virtualNetwork)
	if !network {
		network, _ = isDeviceExist(virtualNetworkNcm)
	}
	disk, _ := isDeviceExist(virtualDisk)

	rsp.OkRspWithData(c, &proto.GetVirtualDeviceRsp{
		Network: network,
		Disk:    disk,
	})
	log.Debugf("get virtual device success")
}

func (s *Service) UpdateVirtualDevice(c *gin.Context) {
	var req proto.UpdateVirtualDeviceReq
	var rsp proto.Response

	if err := proto.ParseFormRequest(c, &req); err != nil {
		rsp.ErrRsp(c, -1, "invalid argument")
		return
	}

	var device string
	var commands []string

	switch req.Device {
	case "network":
		device = virtualNetwork

		// On means on under either flag. Checking only RNDIS meant a live NCM
		// gadget read as off, so the toggle tried to MOUNT what was already up
		// — writing a second flag rather than turning anything off.
		exist, _ := isDeviceExist(virtualNetwork)
		if !exist {
			exist, _ = isDeviceExist(virtualNetworkNcm)
		}
		if !exist {
			commands = mountNetworkCommands
		} else {
			commands = unmountNetworkCommands
		}
	case "disk":
		device = virtualDisk

		exist, _ := isDeviceExist(device)
		if !exist {
			commands = mountDiskCommands
		} else {
			commands = unmountDiskCommands
		}
	default:
		rsp.ErrRsp(c, -2, "invalid arguments")
		return
	}

	h := hid.GetHid()
	h.Lock()
	h.CloseNoLock()
	defer func() {
		h.OpenNoLock()
		h.Unlock()
	}()

	for _, command := range commands {
		err := exec.Command("sh", "-c", command).Run()
		if err != nil {
			rsp.ErrRsp(c, -3, "operation failed")
			return
		}
	}

	on, _ := isDeviceExist(device)
	rsp.OkRspWithData(c, &proto.UpdateVirtualDeviceRsp{
		On: on,
	})

	log.Debugf("update virtual device %s success", req.Device)
}

func isDeviceExist(device string) (bool, error) {
	_, err := os.Stat(device)

	if err == nil {
		return true, nil
	}

	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}

	log.Errorf("check file %s err: %s", device, err)
	return false, err
}

// usbNetAutoMarker records, under the mesh home dir, that the USB network was
// auto-enabled for first claim. Its presence — not the flag file's — is what
// makes that a one-time act.
const usbNetAutoMarker = ".usbnet-auto"

// EnsureUsbNetworkForClaim brings the USB network gadget up on a device nobody
// has claimed yet, so the machine it is physically plugged into can reach it
// over the cable and claim it there.
//
// This is the setup path for an appliance with no network: the USB gadget needs
// no LAN, no router, no DHCP server and no Wi-Fi credentials, and the KVM's web
// server already binds every interface, so the full API answers on it. Without
// this a factory-fresh device is only reachable by first getting it onto a
// network — the thing you often need the KVM to help you do.
//
// It writes the flag and stops there — it does NOT recompose the running
// gadget. See the body: tearing one down isn't something S03usbdev can do, so
// "enabling" live mutates a composite the attached host is using and can take
// its keyboard and mouse out. The next boot builds the gadget cleanly from the
// flags, and a factory-fresh device should carry the flag in its image and
// never reach this at all.
//
// Deliberately once per device, tracked by a marker rather than by the flag
// file. The flag's absence can't distinguish "never configured" from
// "deliberately turned off", so keying on it would put the gadget back every
// boot and overrule an operator who switched it off. It lives under the mesh
// home dir, which a reflash wipes — a re-imaged device should get the setup
// path again.
//
// It never turns the gadget OFF. Claiming over the USB link is the whole point,
// and disabling on claim would cut the very connection the claim arrived on.
//
// Best-effort and self-silencing: every failure is logged and returns.
func EnsureUsbNetworkForClaim(claimed bool, stateDir string) {
	if claimed || stateDir == "" {
		return
	}
	marker := filepath.Join(stateDir, usbNetAutoMarker)
	if _, err := os.Stat(marker); err == nil {
		return
	}

	// Already on (an image that ships it, or a flag placed by hand on the SD
	// card): nothing to do, but record it so we never reconsider.
	on, _ := isDeviceExist(virtualNetwork)
	if !on {
		on, _ = isDeviceExist(virtualNetworkNcm)
	}
	if !on {
		// Write the flag ONLY. S03usbdev's "stop" is start_usb_host — it
		// unbinds the UDC and flips the OTG role, and deliberately leaves the
		// configfs tree standing. So a "stop; start" against a running gadget
		// doesn't rebuild it: every mkdir fails File exists, every descriptor
		// write fails Resource busy, and the composite is mutated in place
		// under a host that is actively using it. Doing that here took the
		// keyboard and mouse out on a live device — breaking the KVM's whole
		// reason for existing to enable a convenience.
		//
		// The gadget is built cleanly exactly once per boot, from these flags,
		// before this server starts. So the safe way to add a function is to
		// write the flag and let the next boot compose it. A factory-fresh
		// device should have the flag in its image and never reach this at all.
		if err := os.WriteFile(virtualNetwork, nil, 0o644); err != nil {
			log.Warnf("usb network auto-enable: write %s: %s", virtualNetwork, err)
			return
		}
		log.Infof("usb network enabled for first claim — takes effect on the next boot (the running gadget is left alone on purpose)")
	}

	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		log.Warnf("usb network auto-enable: create %s: %s", stateDir, err)
		return
	}
	if err := os.WriteFile(marker, []byte("1\n"), 0o644); err != nil {
		log.Warnf("usb network auto-enable: write %s: %s", marker, err)
	}
}
