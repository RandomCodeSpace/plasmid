//go:build unix

package mcp

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/plasmid-dev/plasmid/config"
)

func TestStdioProcessTreeClosesDescendantsAndFailedHandshake(t *testing.T) {
	tests := []struct {
		name      string
		invalid   bool
		wantError bool
	}{
		{name: "session close"},
		{name: "failed handshake", invalid: true, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) { runStdioProcessTreeCase(t, test.invalid, test.wantError) })
	}
}

func runStdioProcessTreeCase(t *testing.T, invalid, wantError bool) {
	t.Helper()
	pidFile := filepath.Join(t.TempDir(), "descendant.pid")
	environment := map[string]string{"PLASMID_MCP_HELPER": "1", "PLASMID_MCP_DESCENDANT_PID": pidFile}
	if invalid {
		environment["PLASMID_MCP_INVALID_HANDSHAKE"] = "1"
	}
	manager, catalog := configuredManager(t, config.MCPServer{
		ID: "stdio-tree", Transport: config.MCPStdio, Command: os.Args[0],
		Args: []string{"-test.run=^TestMCPStdioHelper$"}, Env: environment,
	})
	_, err := manager.connection(t.Context(), "session", "plasmid:configured:stdio-tree", catalog)
	if (err != nil) != wantError {
		t.Fatalf("connection error = %v", err)
	}
	pid := waitForDescendantPID(t, pidFile)
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	waitForProcessExit(t, pid)
}

func waitForProcessExit(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for processExists(pid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if processExists(pid) {
		t.Fatalf("descendant process %d survived MCP close", pid)
	}
}

func waitForDescendantPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
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

func processExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
