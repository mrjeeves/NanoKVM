package router

import (
	"github.com/gin-gonic/gin"

	"NanoKVM-Server/middleware"
	"NanoKVM-Server/service/storage"
)

func storageRouter(r *gin.Engine) {
	service := storage.NewService()
	api := r.Group("/api").Use(middleware.CheckToken())

	api.GET("/storage/image", service.GetImages)               // get image list
	api.GET("/storage/image/mounted", service.GetMountedImage) // get mounted image
	api.POST("/storage/image/mount", service.MountImage)       // mount image
	api.GET("/storage/cdrom", service.GetCdRom)                // get CD-ROM flag
	api.POST("/storage/image/delete", service.DeleteImage)     // delete image
	api.GET("/storage/remote-media/enabled", service.RemoteMediaEnabled)
	api.GET("/storage/remote-media", service.RemoteMedia)
	api.POST("/storage/remote-media/session", service.RemoteMediaPollOpen)
	api.GET("/storage/remote-media/session/next", service.RemoteMediaPollNext)
	api.POST("/storage/remote-media/session/reply", service.RemoteMediaPollReply)
	api.DELETE("/storage/remote-media/session", service.RemoteMediaPollClose)
}
