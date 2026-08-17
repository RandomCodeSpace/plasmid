//go:build windows

package lsp

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"
)

const (
	windowsParentHelperEnv     = "PLASMID_LSP_WINDOWS_PARENT_HELPER"
	windowsDescendantHelperEnv = "PLASMID_LSP_WINDOWS_DESCENDANT_HELPER"
	windowsDescendantPIDEnv    = "PLASMID_LSP_WINDOWS_DESCENDANT_PID"
	windowsSynchronize         = 0x00100000
	windowsWaitTimeout         = 0x00000102
)

var waitForSingleObject = kernel32.NewProc("WaitForSingleObject")

const (
	wantSchedulingClassOffset          = 20 + 5*unsafe.Sizeof(uintptr(0))
	wantIOCountersOffset               = 32 + 4*unsafe.Sizeof(uintptr(0))
	wantJobObjectExtendedLimitInfoSize = 80 + 8*unsafe.Sizeof(uintptr(0))
)

var (
	_ [wantSchedulingClassOffset - unsafe.Offsetof(jobObjectBasicLimitInformation{}.schedulingClass)]byte
	_ [unsafe.Offsetof(jobObjectBasicLimitInformation{}.schedulingClass) - wantSchedulingClassOffset]byte
	_ [wantIOCountersOffset - unsafe.Offsetof(jobObjectExtendedLimitInfo{}.ioInfo)]byte
	_ [unsafe.Offsetof(jobObjectExtendedLimitInfo{}.ioInfo) - wantIOCountersOffset]byte
	_ [wantJobObjectExtendedLimitInfoSize - unsafe.Sizeof(jobObjectExtendedLimitInfo{})]byte
	_ [unsafe.Sizeof(jobObjectExtendedLimitInfo{}) - wantJobObjectExtendedLimitInfoSize]byte
)

func TestProcessTransportStopsWindowsDescendants(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		stop func(context.CancelFunc, Transport) error
	}{
		{name: "close", stop: func(_ context.CancelFunc, transport Transport) error { return transport.Close() }},
		{name: "parent cancellation", stop: func(cancel context.CancelFunc, transport Transport) error {
			cancel()
			select {
			case <-transport.Done():
				return transport.Close()
			case <-time.After(processWaitBound):
				return context.DeadlineExceeded
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "descendant.pid")
			t.Setenv(windowsParentHelperEnv, "1")
			t.Setenv(windowsDescendantPIDEnv, pidFile)
			ctx, cancel := context.WithCancel(context.Background())
			transport, err := startStdioProcess(
				ctx, executable, []string{"-test.run=^TestLSPWindowsParentHelper$"},
				t.TempDir(), 1024, nil,
			)
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			pid := waitForWindowsPID(t, pidFile)
			if err := test.stop(cancel, transport); err != nil {
				t.Fatal(err)
			}
			cancel()
			deadline := time.Now().Add(2 * time.Second)
			for windowsProcessExists(pid) && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if windowsProcessExists(pid) {
				t.Fatalf("descendant process %d survived Windows job cleanup", pid)
			}
		})
	}
}

func TestLSPWindowsParentHelper(t *testing.T) {
	if os.Getenv(windowsParentHelperEnv) != "1" {
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(executable, "-test.run=^TestLSPWindowsDescendantHelper$")
	child.Env = append(os.Environ(), windowsDescendantHelperEnv+"=1")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv(windowsDescendantPIDEnv), []byte(strconv.Itoa(child.Process.Pid)), 0o644); err != nil {
		_ = child.Process.Kill()
		t.Fatal(err)
	}
	_ = child.Wait()
}

func TestLSPWindowsDescendantHelper(t *testing.T) {
	if os.Getenv(windowsDescendantHelperEnv) != "1" {
		return
	}
	time.Sleep(30 * time.Second)
}

func waitForWindowsPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			return pid
		}
		if !errors.Is(err, os.ErrNotExist) || time.Now().After(deadline) {
			t.Fatalf("read descendant PID: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func windowsProcessExists(pid int) bool {
	handle, _, _ := openProcess.Call(windowsSynchronize, 0, uintptr(uint32(pid)))
	if handle == 0 {
		return false
	}
	defer closeProcessTreeHandle.Call(handle)
	result, _, _ := waitForSingleObject.Call(handle, 0)
	return result == windowsWaitTimeout
}
