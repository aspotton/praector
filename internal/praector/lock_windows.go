//go:build windows

package praector

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

const (
	lockfileExclusiveLock   = 0x00000002
	lockfileFailImmediately = 0x00000001
)

var (
	kernel32Lock     = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = kernel32Lock.NewProc("LockFileEx")
	procUnlockFileEx = kernel32Lock.NewProc("UnlockFileEx")
)

type winOverlapped struct {
	Internal     uintptr
	InternalHigh uintptr
	Offset       uint32
	OffsetHigh   uint32
	HEvent       uintptr
}

type FileLock struct {
	f  *os.File
	ov winOverlapped
}

func AcquireFileLock(path string, nonblock bool) (*FileLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, err
	}
	flags := uint32(lockfileExclusiveLock)
	if nonblock {
		flags |= lockfileFailImmediately
	}
	l := &FileLock{f: f}
	r, _, e := procLockFileEx.Call(f.Fd(), uintptr(flags), 0, 1, 0, uintptr(unsafe.Pointer(&l.ov)))
	if r == 0 {
		f.Close()
		if nonblock {
			return nil, fmt.Errorf("lock busy: %v", e)
		}
		return nil, e
	}
	return l, nil
}
func (l *FileLock) Close() error {
	if l == nil || l.f == nil {
		return nil
	}
	procUnlockFileEx.Call(l.f.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(&l.ov)))
	return l.f.Close()
}
