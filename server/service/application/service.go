package application

// Firmware install paths on the device. Updates come from our own release
// channel (see update.go), never cdn.sipeed.com — the stock Sipeed update
// service was removed so a stock build can't clobber our mesh server.
const (
	AppDir   = "/kvmapp"
	CacheDir = "/root/.kvmcache"
)

type Service struct{}

func NewService() *Service {
	return &Service{}
}
