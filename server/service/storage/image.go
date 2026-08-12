package storage

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
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
	imageNone            = "/dev/mmcblk0p3"
	cdromFlag            = "/sys/kernel/config/usb_gadget/g0/functions/mass_storage.disk0/lun.0/cdrom"
	mountDevice          = "/sys/kernel/config/usb_gadget/g0/functions/mass_storage.disk0/lun.0/file"
	inquiryString        = "/sys/kernel/config/usb_gadget/g0/functions/mass_storage.disk0/lun.0/inquiry_string"
	roFlag               = "/sys/kernel/config/usb_gadget/g0/functions/mass_storage.disk0/lun.0/ro"
	virtualMediaMetadata = "/data/.allmystuff-virtual-media.json"
)

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

func mountImage(req proto.MountImageReq) error {
	imageMountMu.Lock()
	defer imageMountMu.Unlock()

	// The gadget flags are only writable while the previous backing file is
	// detached. Do this for every mount, not just CD-ROM images: a raw USB disk
	// arriving after an ISO must clear cdrom while remaining read-only.
	if err := os.WriteFile(mountDevice, []byte("\n"), 0o666); err != nil {
		return fmt.Errorf("unmount image failed: %w", err)
	}
	ro := "0"
	if req.File != "" && (req.Cdrom || req.ReadOnly) {
		ro = "1"
	}
	cdrom := "0"
	if req.File != "" && req.Cdrom {
		cdrom = "1"
	}
	if err := os.WriteFile(roFlag, []byte(ro), 0o666); err != nil {
		return fmt.Errorf("set ro flag failed: %w", err)
	}
	if err := os.WriteFile(cdromFlag, []byte(cdrom), 0o666); err != nil {
		return fmt.Errorf("set cdrom flag failed: %w", err)
	}

	inquiryVen := "NanoKVM"
	inquiryPrd := "USB Mass Storage"
	inquiryVer := 0x0520
	if req.Cdrom {
		inquiryPrd = "USB CD/DVD-ROM"
	}
	inquiryData := fmt.Sprintf("%-8s%-16s%04x", inquiryVen, inquiryPrd, inquiryVer)

	if err := os.WriteFile(inquiryString, []byte(inquiryData), 0o666); err != nil {
		return fmt.Errorf("set inquiry failed: %w", err)
	}

	// mount
	image := req.File
	if image == "" {
		image = imageNone
	}

	if err := os.WriteFile(mountDevice, []byte(image), 0o666); err != nil {
		return fmt.Errorf("mount image failed: %w", err)
	}

	h := hid.GetHid()
	h.Lock()
	h.CloseNoLock()
	defer func() {
		h.OpenNoLock()
		h.Unlock()
	}()

	// reset usb
	commands := []string{
		"echo > /sys/kernel/config/usb_gadget/g0/UDC",
		"ls /sys/class/udc/ | cat > /sys/kernel/config/usb_gadget/g0/UDC",
	}

	for _, command := range commands {
		err := exec.Command("sh", "-c", command).Run()
		if err != nil {
			return fmt.Errorf("reset USB gadget failed: %w", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil
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
