//go:build windows

package praector

import (
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

const (
	createNewProcessGroup        = 0x00000200
	jobObjectExtendedLimitInfo   = 9
	jobObjectLimitKillOnJobClose = 0x00002000
	processSetQuota              = 0x0100
	processTerminate             = 0x0001
)

type ioCounters struct {
	ReadOperationCount, WriteOperationCount, OtherOperationCount uint64
	ReadTransferCount, WriteTransferCount, OtherTransferCount    uint64
}

type basicLimitInformation struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type extendedLimitInformation struct {
	BasicLimitInformation basicLimitInformation
	IoInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

type processTree struct{ job syscall.Handle }

var (
	kernel32               = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW   = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJob  = kernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJob = kernel32.NewProc("AssignProcessToJobObject")
	procOpenProcess        = kernel32.NewProc("OpenProcess")
	procTerminateProcess   = kernel32.NewProc("TerminateProcess")
)

func configureChildProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewProcessGroup}
}

func attachProcessTree(cmd *exec.Cmd) (*processTree, error) {
	if cmd == nil || cmd.Process == nil {
		return &processTree{}, nil
	}
	job, _, e := procCreateJobObjectW.Call(0, 0)
	if job == 0 {
		return nil, e
	}
	info := extendedLimitInformation{}
	info.BasicLimitInformation.LimitFlags = jobObjectLimitKillOnJobClose
	r, _, e := procSetInformationJob.Call(job, jobObjectExtendedLimitInfo, uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info))
	if r == 0 {
		_ = syscall.CloseHandle(syscall.Handle(job))
		return nil, e
	}
	ph, _, e := procOpenProcess.Call(processSetQuota|processTerminate, 0, uintptr(cmd.Process.Pid))
	if ph == 0 {
		_ = syscall.CloseHandle(syscall.Handle(job))
		return nil, e
	}
	defer syscall.CloseHandle(syscall.Handle(ph))
	r, _, e = procAssignProcessToJob.Call(job, ph)
	if r == 0 {
		_ = syscall.CloseHandle(syscall.Handle(job))
		return nil, e
	}
	return &processTree{job: syscall.Handle(job)}, nil
}

func terminateProcessTree(cmd *exec.Cmd, tree *processTree) {
	if tree != nil && tree.job != 0 {
		_ = syscall.CloseHandle(tree.job) // KILL_ON_JOB_CLOSE terminates descendants.
		tree.job = 0
		return
	}
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func terminateStalePID(pid int) {
	if pid <= 0 {
		return
	}
	ph, _, _ := procOpenProcess.Call(processTerminate, 0, uintptr(pid))
	if ph == 0 {
		return
	}
	defer syscall.CloseHandle(syscall.Handle(ph))
	_, _, _ = procTerminateProcess.Call(ph, 1)
}

func closeProcessTree(tree *processTree) error {
	if tree != nil && tree.job != 0 {
		err := syscall.CloseHandle(tree.job)
		tree.job = 0
		return err
	}
	return nil
}

func executableMode() os.FileMode { return 0644 }
