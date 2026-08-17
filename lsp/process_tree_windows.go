//go:build windows

package lsp

import (
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"
)

const (
	jobObjectExtendedLimitInformation = 9
	jobObjectLimitKillOnJobClose      = 0x00002000
	createSuspended                   = 0x00000004
	processSetQuota                   = 0x0100
	processSuspendResume              = 0x0800
	processTerminate                  = 0x0001
)

var (
	kernel32                 = syscall.NewLazyDLL("kernel32.dll")
	createJobObject          = kernel32.NewProc("CreateJobObjectW")
	setInformationJobObject  = kernel32.NewProc("SetInformationJobObject")
	openProcess              = kernel32.NewProc("OpenProcess")
	assignProcessToJobObject = kernel32.NewProc("AssignProcessToJobObject")
	closeProcessTreeHandle   = kernel32.NewProc("CloseHandle")
	ntResumeProcess          = syscall.NewLazyDLL("ntdll.dll").NewProc("NtResumeProcess")
)

type jobObjectBasicLimitInformation struct {
	perProcessUserTimeLimit int64
	perJobUserTimeLimit     int64
	limitFlags              uint32
	minimumWorkingSetSize   uintptr
	maximumWorkingSetSize   uintptr
	activeProcessLimit      uint32
	affinity                uintptr
	priorityClass           uint32
	schedulingClass         uint32
}

type ioCounters struct {
	readOperationCount  uint64
	writeOperationCount uint64
	otherOperationCount uint64
	readTransferCount   uint64
	writeTransferCount  uint64
	otherTransferCount  uint64
}

type jobObjectExtendedLimitInfo struct {
	basicLimitInformation jobObjectBasicLimitInformation
	// The Windows x86 ABI gives the preceding LARGE_INTEGER-containing
	// structure four bytes of tail padding; Go's 386 ABI does not.
	_                     [8 - unsafe.Sizeof(uintptr(0))]byte
	ioInfo                ioCounters
	processMemoryLimit    uintptr
	jobMemoryLimit        uintptr
	peakProcessMemoryUsed uintptr
	peakJobMemoryUsed     uintptr
}

type windowsProcessTree struct {
	job  syscall.Handle
	once sync.Once
	err  error
}

func configureProcessTree(command *exec.Cmd) error {
	// Suspension closes the spawn-before-assignment gap: the server cannot
	// create descendants until the containing job owns it.
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createSuspended}
	return nil
}

func attachProcessTree(process *os.Process) (processTree, error) {
	jobValue, _, callErr := createJobObject.Call(0, 0)
	if jobValue == 0 {
		return nil, windowsProcessError("create job object", callErr)
	}
	job := syscall.Handle(jobValue)
	info := jobObjectExtendedLimitInfo{}
	info.basicLimitInformation.limitFlags = jobObjectLimitKillOnJobClose
	set, _, callErr := setInformationJobObject.Call(
		uintptr(job),
		jobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		unsafe.Sizeof(info),
	)
	if set == 0 {
		_, _, _ = closeProcessTreeHandle.Call(uintptr(job))
		return nil, windowsProcessError("configure job object", callErr)
	}
	processValue, _, callErr := openProcess.Call(processSetQuota|processSuspendResume|processTerminate, 0, uintptr(uint32(process.Pid)))
	if processValue == 0 {
		_, _, _ = closeProcessTreeHandle.Call(uintptr(job))
		return nil, windowsProcessError("open LSP process", callErr)
	}
	assigned, _, assignErr := assignProcessToJobObject.Call(uintptr(job), processValue)
	if assigned == 0 {
		_, _, _ = closeProcessTreeHandle.Call(processValue)
		_, _, _ = closeProcessTreeHandle.Call(uintptr(job))
		return nil, windowsProcessError("assign LSP process to job", assignErr)
	}
	resumeStatus, _, _ := ntResumeProcess.Call(processValue)
	_, _, _ = closeProcessTreeHandle.Call(processValue)
	if resumeStatus != 0 {
		_, _, _ = closeProcessTreeHandle.Call(uintptr(job))
		return nil, fmt.Errorf("resume LSP process: NTSTATUS 0x%x", resumeStatus)
	}
	return &windowsProcessTree{job: job}, nil
}

func (tree *windowsProcessTree) terminate() error {
	tree.once.Do(func() {
		closed, _, callErr := closeProcessTreeHandle.Call(uintptr(tree.job))
		if closed == 0 {
			tree.err = windowsProcessError("close LSP job object", callErr)
		}
	})
	return tree.err
}

func windowsProcessError(operation string, err error) error {
	if err == nil || err == syscall.Errno(0) {
		err = syscall.EINVAL
	}
	return fmt.Errorf("%s: %w", operation, err)
}
