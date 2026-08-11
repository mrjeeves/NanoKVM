package storage

type Service struct {
	remote *remoteMediaManager
}

func NewService() *Service {
	return &Service{remote: newRemoteMediaManager()}
}
