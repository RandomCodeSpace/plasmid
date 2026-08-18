//go:build windows

package plasmid_test

import "syscall"

const (
	releaseSynchronize = 0x00100000
	releaseWaitTimeout = 0x00000102
)

var (
	releaseKernel32     = syscall.NewLazyDLL("kernel32.dll")
	releaseOpenProcess  = releaseKernel32.NewProc("OpenProcess")
	releaseWaitProcess  = releaseKernel32.NewProc("WaitForSingleObject")
	releaseCloseProcess = releaseKernel32.NewProc("CloseHandle")
)

func releaseProcessExists(pid int) bool {
	handle, _, _ := releaseOpenProcess.Call(releaseSynchronize, 0, uintptr(uint32(pid)))
	if handle == 0 {
		return false
	}
	defer releaseCloseProcess.Call(handle)
	result, _, _ := releaseWaitProcess.Call(handle, 0)
	return result == releaseWaitTimeout
}
