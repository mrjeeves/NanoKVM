package router

import (
	"NanoKVM-Server/middleware"
	"NanoKVM-Server/service/application"

	"github.com/gin-gonic/gin"
)

func applicationRouter(r *gin.Engine) {
	service := application.NewService()
	api := r.Group("/api").Use(middleware.CheckToken())

	// Firmware version + update, pointed at our own release channel (never
	// cdn.sipeed.com). Sipeed's stock update service — online + offline install
	// and the preview channel — was removed: a stock update installs its build
	// over /kvmapp and clobbers our mesh server (see docs/MESH.md).
	//
	// Both sit behind the normal CheckToken gate, so the AllMyStuff mesh tunnel
	// authorizes them with NO device password (mesh-roster membership is the
	// auth — the point of reaching a KVM over the mesh), while a direct LAN
	// caller still needs the KVM login.
	api.GET("/application/version", service.GetVersion) // current build + our channel's latest
	api.POST("/application/update", service.Update)     // install our latest firmware
}
