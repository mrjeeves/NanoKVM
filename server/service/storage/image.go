package storage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	"NanoKVM-Server/proto"
	"NanoKVM-Server/service/hid"
)

var imageMountMu sync.Mutex

const (
	imageDirectory       = "/data"
	imageNone            = kvmDriveImage
	cdromFlag            = "/sys/kernel/config/usb_gadget/g0/functions/mass_storage.disk0/lun.0/cdrom"
	mountDevice          = "/sys/kernel/config/usb_gadget/g0/functions/mass_storage.disk0/lun.0/file"
	inquiryString        = "/sys/kernel/config/usb_gadget/g0/functions/mass_storage.disk0/lun.0/inquiry_string"
	roFlag               = "/sys/kernel/config/usb_gadget/g0/functions/mass_storage.disk0/lun.0/ro"
	virtualMediaMetadata = "/data/.allmystuff-virtual-media.json"
	usbGadgetUDC         = "/sys/kernel/config/usb_gadget/g0/UDC"
	usbUDCClass          = "/sys/class/udc"
	usbOTGRole           = "/proc/cviusb/otg_role"
)

var (
	usbReadFile  = os.ReadFile
	usbWriteFile = os.WriteFile
	usbSleep     = time.Sleep
)

type lunState struct {
	file     string
	readOnly string
	cdrom    string
	inquiry  string
}

type virtualMediaState struct {
	Source string `json:"source"`
	Label  string `json:"label"`
	File   string `json:"file"`
}

func persistVirtualMedia(req proto.MountImageReq) {
	if req.File == "" {
		_ = os.Remove(virtualMediaMetadata)
		return
	}
	if req.Source == "" {
		return
	}
	data, err := json.Marshal(virtualMediaState{Source: req.Source, Label: req.Label, File: req.File})
	if err == nil {
		_ = os.WriteFile(virtualMediaMetadata, data, 0o600)
	}
}

func (s *Service) GetImages(c *gin.Context) {
	var rsp proto.Response
	var images []string

	err := filepath.Walk(imageDirectory, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if !info.IsDir() {
			name := strings.ToLower(info.Name())
			if strings.HasSuffix(name, ".iso") || strings.HasSuffix(name, ".img") {
				images = append(images, path)
			}
		}

		return nil
	})
	if err != nil {
		rsp.ErrRsp(c, -2, "get images failed")
		return
	}

	rsp.OkRspWithData(c, &proto.GetImagesRsp{
		Files: images,
	})
	log.Debugf("get images success, total %d", len(images))
}

func (s *Service) MountImage(c *gin.Context) {
	var req proto.MountImageReq
	var rsp proto.Response

	if err := proto.ParseFormRequest(c, &req); err != nil {
		rsp.ErrRsp(c, -1, "invalid arguments")
		return
	}

	// A direct mount/unmount is an explicit replacement for any lazy provider.
	// Its shutdown performs this requested mount in the same gadget reset, then
	// tears FUSE down only after the kernel no longer has the virtual file open.
	handled, err := s.remote.replaceActive(req)
	if !handled {
		err = mountImage(req)
	}
	if err != nil {
		log.Errorf("mount image %s failed: %s", req.File, err)
		rsp.ErrRsp(c, -2, err.Error())
		return
	}

	if !handled {
		persistVirtualMedia(req)
	}
	rsp.OkRsp(c)
	log.Debugf("mount image %s success", req.File)
}

func mountImage(req proto.MountImageReq) (retErr error) {
	imageMountMu.Lock()
	defer imageMountMu.Unlock()
	hidNeedsRecovery := false

	previous, err := readLUNState()
	if err != nil {
		return fmt.Errorf("read current USB media state: %w", err)
	}
	defer func() {
		if retErr == nil {
			return
		}
		if err := writeLUNState(previous); err != nil {
			retErr = fmt.Errorf("%v; restore previous USB media: %w", retErr, err)
		}
		if err := ensureUSBGadgetBound(); err != nil {
			retErr = fmt.Errorf("%v; recover USB gadget: %w", retErr, err)
		}
		if hidNeedsRecovery {
			h := hid.GetHid()
			h.Lock()
			if err := h.OpenNoLockWithRetry(2*time.Second, 100*time.Millisecond); err != nil {
				retErr = fmt.Errorf("%v; recover USB input devices: %w", retErr, err)
			}
			h.Unlock()
		}
	}()

	ro := "0"
	if req.File != "" && (req.Cdrom || req.ReadOnly) {
		ro = "1"
	}
	cdrom := "0"
	if req.File != "" && req.Cdrom {
		cdrom = "1"
	}
	inquiryVen := "NanoKVM"
	inquiryPrd := "USB Mass Storage"
	inquiryVer := 0x0520
	if req.Cdrom {
		inquiryPrd = "USB CD/DVD-ROM"
	}
	inquiryData := fmt.Sprintf("%-8s%-16s%04x", inquiryVen, inquiryPrd, inquiryVer)

	image := req.File
	if image == "" && driveImageReady(imageNone) {
		image = imageNone
	}
	target := lunState{file: image, readOnly: ro, cdrom: cdrom, inquiry: inquiryData}
	if err := writeLUNState(target); err != nil {
		return err
	}

	h := hid.GetHid()
	h.Lock()
	h.CloseNoLock()
	hidNeedsRecovery = true
	resetErr := resetUSBGadget()
	openErr := h.OpenNoLockWithRetry(2*time.Second, 100*time.Millisecond)
	if openErr == nil {
		hidNeedsRecovery = false
	}
	h.Unlock()
	if resetErr != nil {
		return fmt.Errorf("reset USB gadget failed: %w", resetErr)
	}
	if openErr != nil {
		return fmt.Errorf("reopen USB input devices: %w", openErr)
	}
	return nil
}

func readLUNState() (lunState, error) {
	read := func(path string) (string, error) {
		data, err := os.ReadFile(path)
		return strings.TrimSpace(string(data)), err
	}
	state := lunState{}
	var err error
	if state.file, err = read(mountDevice); err != nil {
		return state, err
	}
	if state.readOnly, err = read(roFlag); err != nil {
		return state, err
	}
	if state.cdrom, err = read(cdromFlag); err != nil {
		return state, err
	}
	if state.inquiry, err = read(inquiryString); err != nil {
		return state, err
	}
	return state, nil
}

func writeLUNState(state lunState) error {
	if err := os.WriteFile(mountDevice, []byte("\n"), 0o666); err != nil {
		return fmt.Errorf("detach USB media: %w", err)
	}
	if err := os.WriteFile(roFlag, []byte(state.readOnly), 0o666); err != nil {
		return fmt.Errorf("set ro flag: %w", err)
	}
	if err := os.WriteFile(cdromFlag, []byte(state.cdrom), 0o666); err != nil {
		return fmt.Errorf("set cdrom flag: %w", err)
	}
	if err := os.WriteFile(inquiryString, []byte(state.inquiry), 0o666); err != nil {
		return fmt.Errorf("set inquiry: %w", err)
	}
	if state.file != "" {
		if err := os.WriteFile(mountDevice, []byte(state.file), 0o666); err != nil {
			return fmt.Errorf("attach USB media: %w", err)
		}
	}
	return nil
}

func resetUSBGadget() error {
	controller, err := usbController()
	if err != nil {
		return err
	}
	if err := usbWriteFile(usbGadgetUDC, []byte("\n"), 0o666); err != nil {
		return fmt.Errorf("unbind USB gadget: %w", err)
	}
	if err := switchUSBToDeviceRole(); err != nil {
		return err
	}
	usbSleep(100 * time.Millisecond)
	return bindUSBGadget(controller)
}

func ensureUSBGadgetBound() error {
	data, err := usbReadFile(usbGadgetUDC)
	if err != nil {
		return err
	}
	boundController := strings.TrimSpace(string(data))
	// UDC is configfs' authoritative binding state. On the CVI controller,
	// is_a_peripheral reports 0 on healthy, host-visible NanoKVMs as well, so it
	// cannot be used as a role/health signal. Resetting a gadget because of that
	// value disconnects every composed function (HID, network, and storage) at
	// each server start and can leave the controller unenumerated.
	if boundController != "" {
		return nil
	}
	controller, err := usbController()
	if err != nil {
		return err
	}
	if err := switchUSBToDeviceRole(); err != nil {
		return err
	}
	usbSleep(100 * time.Millisecond)
	return bindUSBGadget(controller)
}

func switchUSBToDeviceRole() error {
	if err := usbWriteFile(usbOTGRole, []byte("device\n"), 0o666); err != nil {
		return fmt.Errorf("switch USB controller to device role: %w", err)
	}
	return nil
}

func usbController() (string, error) {
	if data, err := usbReadFile(usbGadgetUDC); err == nil {
		if controller := strings.TrimSpace(string(data)); controller != "" {
			return controller, nil
		}
	}
	controllers, err := os.ReadDir(usbUDCClass)
	if err != nil {
		return "", fmt.Errorf("list USB controllers: %w", err)
	}
	if len(controllers) == 0 {
		return "", fmt.Errorf("no USB device controller is available")
	}
	return controllers[0].Name(), nil
}

func bindUSBGadget(controller string) error {
	var lastErr error
	for attempt := 0; attempt < 10; attempt++ {
		if err := usbWriteFile(usbGadgetUDC, []byte(controller), 0o666); err == nil {
			return nil
		} else {
			lastErr = err
		}
		usbSleep(100 * time.Millisecond)
	}
	return fmt.Errorf("bind USB gadget to %s: %w", controller, lastErr)
}

func (s *Service) GetMountedImage(c *gin.Context) {
	var rsp proto.Response

	content, err := os.ReadFile(mountDevice)
	if err != nil {
		rsp.ErrRsp(c, -2, "read failed")
		return
	}

	image := strings.ReplaceAll(string(content), "\n", "")
	if image == imageNone {
		image = ""
	}

	data := &proto.GetMountedImageRsp{
		File: image,
	}

	rsp.OkRspWithData(c, data)
}

func (s *Service) GetCdRom(c *gin.Context) {
	var rsp proto.Response

	content, err := os.ReadFile(cdromFlag)
	if err != nil {
		rsp.ErrRsp(c, -1, "read failed")
		return
	}

	flag := strings.ReplaceAll(string(content), "\n", "")
	flatInt, err := strconv.ParseInt(flag, 10, 64)
	if err != nil {
		rsp.ErrRsp(c, -2, "parse failed")
		return
	}

	data := &proto.GetCdRomRsp{
		Cdrom: flatInt,
	}

	rsp.OkRspWithData(c, data)
}

func (s *Service) DeleteImage(c *gin.Context) {
	var req proto.DeleteImageReq
	var rsp proto.Response

	if err := proto.ParseFormRequest(c, &req); err != nil {
		rsp.ErrRsp(c, -1, "invalid arguments")
		return
	}

	filename := strings.ToLower(req.File)
	validPrefix := strings.HasPrefix(filename, imageDirectory)
	validSuffix := strings.HasSuffix(filename, ".iso") || strings.HasSuffix(filename, ".img")

	if !validPrefix || !validSuffix {
		rsp.ErrRsp(c, -2, "invalid arguments")
		return
	}

	if err := os.Remove(req.File); err != nil {
		rsp.ErrRsp(c, -3, "remove file failed")
		log.Errorf("failed to remove file %s: %s", req.File, err)
		return
	}

	rsp.OkRsp(c)
	log.Debugf("delete image %s success", req.File)
}
