package storage

type Service struct {
	remote *remoteMediaManager
}

func NewService() *Service {
	recoverUSBAtStartup()
	return &Service{remote: newRemoteMediaManager()}
}
