//go:build !linux

package storage

import (
	"net/http"

	"NanoKVM-Server/proto"
	"github.com/gin-gonic/gin"
)

type remoteMediaManager struct{}

func recoverUSBAtStartup() {}

func newRemoteMediaManager() *remoteMediaManager { return &remoteMediaManager{} }

func (m *remoteMediaManager) replaceActive(proto.MountImageReq) (bool, error) {
	return false, nil
}

func (s *Service) RemoteMediaEnabled(c *gin.Context) {
	var rsp proto.Response
	rsp.OkRspWithData(c, gin.H{"enabled": false, "reason": "remote media requires Linux"})
}

func (s *Service) RemoteMedia(c *gin.Context) {
	c.AbortWithStatus(http.StatusNotImplemented)
}

func (s *Service) RemoteMediaPollOpen(c *gin.Context) {
	c.AbortWithStatus(http.StatusNotImplemented)
}

func (s *Service) RemoteMediaPollNext(c *gin.Context) {
	c.AbortWithStatus(http.StatusNotImplemented)
}

func (s *Service) RemoteMediaPollReply(c *gin.Context) {
	c.AbortWithStatus(http.StatusNotImplemented)
}

func (s *Service) RemoteMediaPollClose(c *gin.Context) {
	c.AbortWithStatus(http.StatusNotImplemented)
}
