//go:build windows

package processtree

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
	closeHandle              = kernel32.NewProc("CloseHandle")
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
	_                     [8 - unsafe.Sizeof(uintptr(0))]byte
	ioInfo                ioCounters
	processMemoryLimit    uintptr
	jobMemoryLimit        uintptr
	peakProcessMemoryUsed uintptr
	peakJobMemoryUsed     uintptr
}

type windowsTree struct {
	job  syscall.Handle
	once sync.Once
	err  error
}

func configure(command *exec.Cmd) error {
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createSuspended}
	return nil
}

func attach(process *os.Process) (Terminator, error) {
	jobValue, _, callErr := createJobObject.Call(0, 0)
	if jobValue == 0 {
		return nil, windowsError("create job object", callErr)
	}
	job := syscall.Handle(jobValue)
	info := jobObjectExtendedLimitInfo{}
	info.basicLimitInformation.limitFlags = jobObjectLimitKillOnJobClose
	set, _, callErr := setInformationJobObject.Call(uintptr(job), jobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info))
	if set == 0 {
		_, _, _ = closeHandle.Call(uintptr(job))
		return nil, windowsError("configure job object", callErr)
	}
	processValue, _, callErr := openProcess.Call(processSetQuota|processSuspendResume|processTerminate, 0, uintptr(uint32(process.Pid)))
	if processValue == 0 {
		_, _, _ = closeHandle.Call(uintptr(job))
		return nil, windowsError("open process", callErr)
	}
	assigned, _, assignErr := assignProcessToJobObject.Call(uintptr(job), processValue)
	if assigned == 0 {
		_, _, _ = closeHandle.Call(processValue)
		_, _, _ = closeHandle.Call(uintptr(job))
		return nil, windowsError("assign process to job", assignErr)
	}
	resumeStatus, _, _ := ntResumeProcess.Call(processValue)
	_, _, _ = closeHandle.Call(processValue)
	if resumeStatus != 0 {
		_, _, _ = closeHandle.Call(uintptr(job))
		return nil, fmt.Errorf("resume process: NTSTATUS 0x%x", resumeStatus)
	}
	return &windowsTree{job: job}, nil
}

func (tree *windowsTree) Terminate() error {
	tree.once.Do(func() {
		closed, _, callErr := closeHandle.Call(uintptr(tree.job))
		if closed == 0 {
			tree.err = windowsError("close process job object", callErr)
		}
	})
	return tree.err
}

func windowsError(operation string, err error) error {
	if err == nil || err == syscall.Errno(0) {
		err = syscall.EINVAL
	}
	return fmt.Errorf("%s: %w", operation, err)
}
