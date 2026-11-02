package taskengine

import (
	"errors"
	"os"
	"syscall"
)

func (r *ModelRegistry) acquireLock() error {
	f, err := os.OpenFile(r.path+".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return errors.New("registry lock unavailable")
	}
	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		f.Close()
		return errors.New("registry already owned")
	}
	r.lock = f
	return nil
}
