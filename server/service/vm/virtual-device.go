package vm

import (
	"errors"
	"os"
	"os/exec"

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

		exist, _ := isDeviceExist(device)
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
