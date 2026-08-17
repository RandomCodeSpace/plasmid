//go:build unix

package lsp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestProcessTransportStopsDescendants(t *testing.T) {
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not available")
	}
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not available")
	}
	for _, test := range []struct {
		name string
		stop func(context.CancelFunc, Transport) error
	}{
		{name: "close", stop: func(_ context.CancelFunc, transport Transport) error { return transport.Close() }},
		{name: "cancel", stop: func(cancel context.CancelFunc, transport Transport) error {
			cancel()
			select {
			case <-transport.Done():
				return nil
			case <-time.After(processWaitBound):
				return context.DeadlineExceeded
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			pidFile := filepath.Join(root, "child.pid")
			script := fmt.Sprintf("sleep 30 >/dev/null 2>&1 & child=$!; printf '%%s' \"$child\" > %s; wait", shellQuote(pidFile))
			ctx, cancel := context.WithCancel(context.Background())
			transport, err := startStdioProcess(ctx, shell, []string{"-c", script}, root, 1024, nil)
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			pid := waitForPID(t, pidFile)
			if err := test.stop(cancel, transport); err != nil {
				t.Fatal(err)
			}
			cancel()
			deadline := time.Now().Add(2 * time.Second)
			for processExists(pid) && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if processExists(pid) {
				t.Fatalf("descendant process %d survived process-tree cleanup", pid)
			}
		})
	}
}

func TestProcessTransportStopsOnRPCDisconnect(t *testing.T) {
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not available")
	}
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not available")
	}
	root := t.TempDir()
	pidFile := filepath.Join(root, "child.pid")
	script := fmt.Sprintf("sleep 30 >/dev/null 2>&1 & child=$!; printf '%%s' \"$child\" > %s; exec 1>&-; wait", shellQuote(pidFile))
	ctx, cancel := context.WithCancel(context.Background())
	transport, err := startStdioProcess(ctx, shell, []string{"-c", script}, root, 1024, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		_ = transport.Close()
	})
	pid := waitForPID(t, pidFile)
	select {
	case <-transport.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("disconnected RPC transport left the process tree alive")
	}
	deadline := time.Now().Add(2 * time.Second)
	for processExists(pid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if processExists(pid) {
		t.Fatalf("descendant process %d survived RPC disconnect", pid)
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func waitForPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	var lastErr error
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if parseErr == nil && pid > 0 {
				return pid
			}
			lastErr = parseErr
			if parseErr == nil {
				lastErr = fmt.Errorf("invalid child PID %d", pid)
			}
		} else if errors.Is(err, os.ErrNotExist) {
			lastErr = err
		} else {
			t.Fatalf("read child PID: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("read child PID: %v", lastErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func processExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
