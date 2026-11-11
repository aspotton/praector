//go:build !windows

package praector

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

type processTree struct{}

func configureChildProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func attachProcessTree(cmd *exec.Cmd) (*processTree, error) { return &processTree{}, nil }

func terminateProcessTree(cmd *exec.Cmd, _ *processTree) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	time.Sleep(100 * time.Millisecond)
	if err := syscall.Kill(-pid, 0); err == nil || errors.Is(err, syscall.EPERM) {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
}

func terminateStalePID(pid int) {
	if pid <= 0 {
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	time.Sleep(100 * time.Millisecond)
	if err := syscall.Kill(-pid, 0); err == nil || errors.Is(err, syscall.EPERM) {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
}

func closeProcessTree(_ *processTree) error { return nil }

func executableMode() os.FileMode { return 0755 }
