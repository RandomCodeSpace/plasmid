//go:build unix

package walk

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
	"time"

	"github.com/plasmid-dev/plasmid/warning"
	"github.com/plasmid-dev/plasmid/workspace"
)

func TestWalkRejectsFIFOIgnoreFileWithoutBlocking(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(directory, ".gitignore"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "visible.txt"), []byte("visible"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := workspace.NewRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		paths    []string
		warnings []warning.Warning
		err      error
	}
	done := make(chan result, 1)
	go func() {
		var got result
		var warnings warning.SliceSink
		got.err = walk(context.Background(), &Filter{Root: root, MaxDepth: -1, RespectGitignore: true, SkipHidden: true}, func(entry Entry) error {
			got.paths = append(got.paths, entry.Path)
			return nil
		}, &warnings)
		got.warnings = warnings.Warnings()
		done <- got
	}()
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatal(got.err)
		}
		if want := []string{"visible.txt"}; !reflect.DeepEqual(got.paths, want) {
			t.Fatalf("paths = %#v, want %#v", got.paths, want)
		}
		if len(got.warnings) != 1 || got.warnings[0].Code != warning.WarnWalkUnreadableIgnore {
			t.Fatalf("warnings = %#v", got.warnings)
		}
	case <-time.After(time.Second):
		t.Fatal("walk blocked opening a FIFO ignore file")
	}
}
