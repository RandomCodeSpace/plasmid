//go:build unix

package codingtools_test

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/RandomCodeSpace/plasmid/codingtools"
	"github.com/RandomCodeSpace/plasmid/outputlimit"
	"github.com/RandomCodeSpace/plasmid/warning"
	"github.com/RandomCodeSpace/plasmid/workspace"
)

func TestPublicToolsContainPermissionFailures(t *testing.T) {
	directory := t.TempDir()
	writeFile(t, directory, "file.txt", "value\n")
	writeFile(t, directory, "search.txt", "needle\n")
	writeFile(t, directory, "parent-file", "content\n")
	if err := os.Mkdir(filepath.Join(directory, "locked"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(directory, "noaccess"), 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := workspace.NewRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	set, err := codingtools.New(codingtools.Config{
		Root: root, Queue: workspace.NewMutationQueue(), Ledger: workspace.NewLedger(), Touch: workspace.NewTouchBus(),
		Budget: outputlimit.NewBudget(1 << 20), WarningSink: warning.DiscardSink{},
	})
	if err != nil {
		t.Fatal(err)
	}
	invokeTool(t, set, "read", map[string]any{"path": "file.txt"})
	for _, path := range []string{"file.txt", "search.txt"} {
		absolute := filepath.Join(directory, path)
		if err := os.Chmod(absolute, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(absolute, 0o600) })
	}
	locked := filepath.Join(directory, "locked")
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	noaccess := filepath.Join(directory, "noaccess")
	if err := os.Chmod(noaccess, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(noaccess, 0o700) })

	for _, test := range []struct {
		tool string
		args map[string]any
	}{
		{tool: "read", args: map[string]any{"path": "file.txt"}},
		{tool: "edit", args: map[string]any{"path": "file.txt", "old_text": "value", "new_text": "changed"}},
		{tool: "write", args: map[string]any{"path": "file.txt", "content": "new"}},
		{tool: "write", args: map[string]any{"path": "locked/new.txt", "content": "new"}},
		{tool: "write", args: map[string]any{"path": "noaccess/new.txt", "content": "new"}},
		{tool: "grep", args: map[string]any{"path": "search.txt", "pattern": "needle"}},
		{tool: "find", args: map[string]any{"path": "noaccess", "glob": "*"}},
		{tool: "ls", args: map[string]any{"path": "noaccess"}},
	} {
		response := invokeTool(t, set, test.tool, test.args)
		message, _ := response["error"].(string)
		if !strings.Contains(strings.ToLower(message), "permission denied") {
			t.Fatalf("%s response = %#v", test.tool, response)
		}
	}
	response := invokeTool(t, set, "write", map[string]any{"path": "parent-file/child.txt", "content": "new"})
	if message, _ := response["error"].(string); !strings.Contains(strings.ToLower(message), "not a directory") {
		t.Fatalf("write through file response = %#v", response)
	}
}

func TestPublicToolsRejectNamedPipesWithoutOpeningThem(t *testing.T) {
	directory := t.TempDir()
	pipe := filepath.Join(directory, "pipe")
	if err := syscall.Mkfifo(pipe, 0o600); err != nil {
		t.Skipf("named pipe unavailable: %v", err)
	}
	root, err := workspace.NewRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	set, err := codingtools.New(codingtools.Config{
		Root: root, Queue: workspace.NewMutationQueue(), Ledger: workspace.NewLedger(), Touch: workspace.NewTouchBus(),
		Budget: outputlimit.NewBudget(1 << 20), WarningSink: warning.DiscardSink{},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		tool string
		args map[string]any
	}{
		{tool: "read", args: map[string]any{"path": "pipe"}},
		{tool: "write", args: map[string]any{"path": "pipe", "content": "x"}},
		{tool: "edit", args: map[string]any{"path": "pipe", "old_text": "x", "new_text": "y"}},
		{tool: "grep", args: map[string]any{"path": "pipe", "pattern": "x"}},
	} {
		response := invokeTool(t, set, test.tool, test.args)
		if message, _ := response["error"].(string); !strings.Contains(message, "regular") {
			t.Fatalf("%s response = %#v", test.tool, response)
		}
	}
	directoryResult := invokeTool(t, set, "grep", map[string]any{"path": ".", "pattern": "x"})
	if directoryResult["match_count"] != float64(0) {
		t.Fatalf("directory grep response = %#v", directoryResult)
	}
}
