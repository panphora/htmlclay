package platform

type SingleInstance interface {
	TryLock() (bool, error)
	SendFilePath(path string) error
	OnFileReceived(callback func(path string))
	Unlock() error
}
