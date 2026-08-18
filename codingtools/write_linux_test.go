//go:build linux

package codingtools

import (
	"context"
	"errors"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/plasmid-dev/plasmid/workspace"
)

func TestWriteRejectsNonRegularTarget(t *testing.T) {
	h := newWriteHarness(t, t.TempDir(), nil)
	if err := syscall.Mkfifo(filepath.Join(h.root, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := h.tool(context.Background(), "s", map[string]any{"path": "pipe", "content": "value"})
	if !errors.Is(err, workspace.ErrNotRegularFile) {
		t.Fatalf("non-regular target error = %v", err)
	}
	if len(h.observer.snapshot()) != 0 {
		t.Fatal("failed write published a touch")
	}
}
