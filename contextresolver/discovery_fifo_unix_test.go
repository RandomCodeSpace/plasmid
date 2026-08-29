//go:build !windows

package contextresolver

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/RandomCodeSpace/plasmid/warning"
)

func TestInstructionSymlinkToFIFOIsRejectedWithoutBlocking(t *testing.T) {
	t.Parallel()
	rootDir := t.TempDir()
	fifo := filepath.Join(rootDir, "instruction.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("create FIFO: %v", err)
	}
	if err := os.Symlink(fifo, filepath.Join(rootDir, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	sink := &warning.SliceSink{}
	resolver := newTestResolver(t, rootDir, Options{WarningSink: sink})
	done := make(chan error, 1)
	go func() { done <- resolver.StartSession(t.Context(), "session") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("instruction discovery blocked opening a FIFO")
	}
	if !hasWarning(sink.Warnings(), warning.WarnContextReadError) {
		t.Fatalf("warnings = %#v", sink.Warnings())
	}
}
