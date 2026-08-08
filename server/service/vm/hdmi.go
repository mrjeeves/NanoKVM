package vm

import (
	"time"

	"NanoKVM-Server/common"
	"NanoKVM-Server/proto"
	"NanoKVM-Server/service/viewer"
	"NanoKVM-Server/utils"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// These handlers drive the receiver directly, so each one tells the viewer
// lease what it did. Without that the lease's cached view of the hardware and
// the hardware itself drift apart, and the next lease transition is skipped as
// a redundant no-op — the case where a manual reset left the screen dark until
// every viewer disconnected and came back.

func (s *Service) ResetHdmi(c *gin.Context) {
	var rsp proto.Response

	vision := common.GetKvmVision()

	vision.SetHDMI(false)
	time.Sleep(1 * time.Second)
	vision.SetHDMI(true)
	utils.PersistHDMIEnabled()
	viewer.Note(true)
	viewer.SetAllowed(true)

	rsp.OkRsp(c)
	log.Debug("reset hdmi")
}

func (s *Service) EnableHdmi(c *gin.Context) {
	var rsp proto.Response

	vision := common.GetKvmVision()

	vision.SetHDMI(true)
	utils.PersistHDMIEnabled()
	viewer.Note(true)
	// Re-arms the lease: with a viewer already connected the receiver stays up,
	// otherwise it powers down again after the idle grace.
	viewer.SetAllowed(true)

	rsp.OkRsp(c)
	log.Debug("enable hdmi")
}

func (s *Service) DisableHdmi(c *gin.Context) {
	var rsp proto.Response

	vision := common.GetKvmVision()

	vision.SetHDMI(false)
	utils.PersistHDMIDisabled()
	viewer.Note(false)
	// Pins the receiver off: an operator who switched HDMI off must not have it
	// switched back on by the next viewer that connects.
	viewer.SetAllowed(false)

	rsp.OkRsp(c)
	log.Debug("disable hdmi")
}

func (s *Service) GetHdmiState(c *gin.Context) {
	var rsp proto.Response

	rsp.OkRspWithData(c, &proto.GetGetHdmiStateRsp{
		Enabled: !utils.IsHdmiDisabled(),
	})

	log.Debug("get hdmi state")
}
