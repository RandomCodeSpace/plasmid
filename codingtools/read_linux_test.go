package codingtools

import (
	"context"
	"errors"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/plasmid-dev/plasmid/workspace"
)

func TestReadRejectsNonRegularFileWithoutOpeningIt(t *testing.T) {
	rootDir := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(rootDir, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	harness := newReadHarness(t, rootDir, nil)
	result, err := harness.tool(context.Background(), "session", map[string]any{"path": "pipe"})
	if !errors.Is(err, workspace.ErrNotRegularFile) || result != nil {
		t.Fatalf("Call() = %#v, %v", result, err)
	}
	if touches := harness.observer.snapshot(); len(touches) != 0 {
		t.Fatalf("non-regular read published touches: %#v", touches)
	}
}
