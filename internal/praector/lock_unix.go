//go:build !windows

package praector

import (
	"fmt"
	"os"
	"syscall"
)

type FileLock struct{ f *os.File }

func AcquireFileLock(path string, nonblock bool) (*FileLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, err
	}
	flags := syscall.LOCK_EX
	if nonblock {
		flags |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(f.Fd()), flags); err != nil {
		f.Close()
		if nonblock {
			return nil, fmt.Errorf("lock busy: %w", err)
		}
		return nil, err
	}
	return &FileLock{f: f}, nil
}
func (l *FileLock) Close() error {
	if l == nil || l.f == nil {
		return nil
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	return l.f.Close()
}
