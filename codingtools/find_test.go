package codingtools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/plasmid-dev/plasmid/outputlimit"
	"github.com/plasmid-dev/plasmid/workspace"
)

func TestFindToolContract(t *testing.T) {
	if _, err := newFindHandler(Config{}); err == nil {
		t.Fatal("NewFindTool without root succeeded")
	}
	root, err := workspace.NewRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tool, err := NewFindTool(Config{Root: root, Touch: workspace.NewTouchBus(), Budget: outputlimit.NewBudget(10000)})
	if err != nil {
		t.Fatal(err)
	}
	if tool.Name() != findToolName || tool.Description() != FindDescription || tool.IsLongRunning() {
		t.Fatalf("tool contract = %#v", tool)
	}
}

func TestFindGlobTypePathSortAndTruncation(t *testing.T) {
	directory := t.TempDir()
	writeFindFile(t, directory, "root.go", "root")
	writeFindFile(t, directory, "nested/old.go", "old")
	writeFindFile(t, directory, "nested/new.go", "new")
	writeFindFile(t, directory, "nested/file.txt", "text")
	if err := os.Symlink("nested/new.go", filepath.Join(directory, "link.go")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	old := time.Unix(10, 0)
	newer := time.Unix(20, 0)
	for _, item := range []struct {
		path string
		time time.Time
	}{{"root.go", newer}, {"nested/old.go", old}, {"nested/new.go", newer}} {
		if err := os.Chtimes(filepath.Join(directory, item.path), item.time, item.time); err != nil {
			t.Fatal(err)
		}
	}
	tool := newFindTestTool(t, directory)
	tests := []struct {
		name      string
		args      map[string]any
		want      []string
		truncated bool
	}{
		{"glob", map[string]any{"glob": "*.go", "type": entryTypeFile}, []string{"nested/new.go", "nested/old.go", "root.go"}, false},
		{"directory", map[string]any{"glob": "nested", "type": "dir"}, []string{"nested"}, false},
		{"symlink", map[string]any{"glob": "*.go", "type": "symlink"}, []string{"link.go"}, false},
		{"any", map[string]any{"glob": "*", "type": "any"}, []string{"link.go", "nested", "nested/file.txt", "nested/new.go", "nested/old.go", "root.go"}, false},
		{"nested path", map[string]any{"path": "nested", "glob": "*.go", "type": entryTypeFile}, []string{"nested/new.go", "nested/old.go"}, false},
		{"modified tie", map[string]any{"glob": "*.go", "type": entryTypeFile, "sort_by": "modified"}, []string{"nested/new.go", "root.go", "nested/old.go"}, false},
		{"truncated", map[string]any{"glob": "*.go", "type": entryTypeFile, "max_results": 2}, []string{"nested/new.go", "nested/old.go"}, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := callFind(t, tool, context.Background(), test.args)
			if !reflect.DeepEqual(got.Paths, test.want) || got.Truncated != test.truncated {
				t.Fatalf("result = %#v, want paths %#v truncated %v", got, test.want, test.truncated)
			}
		})
	}
}

func TestFindRejectsNonDirectoryCancellationAndEscape(t *testing.T) {
	directory := t.TempDir()
	writeFindFile(t, directory, "file.txt", "x")
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(directory, "outside")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	tool := newFindTestTool(t, directory)
	for name, args := range map[string]map[string]any{
		"file":           {"path": "file.txt", "glob": "*"},
		"dot dot escape": {"path": "../outside", "glob": "*"},
		"symlink escape": {"path": "outside", "glob": "*"},
	} {
		t.Run(name, func(t *testing.T) {
			result, err := adaptTestHandler(t, tool.call)(context.Background(), "", args)
			if err == nil || result != nil {
				t.Fatalf("result = %#v, error = %v", result, err)
			}
			if name == "file" && !errors.Is(err, workspace.ErrNotDirectory) {
				t.Fatalf("file error = %v, want ErrNotDirectory", err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := adaptTestHandler(t, tool.call)(ctx, "", map[string]any{"glob": "*"})
	if !errors.Is(err, context.Canceled) || result != nil {
		t.Fatalf("result = %#v, error = %v", result, err)
	}
}

func TestFindSortsAllMatchesBeforeModifiedLimit(t *testing.T) {
	directory := t.TempDir()
	for index := range findWalkMaxRegressionEntries {
		writeFindFile(t, directory, fmt.Sprintf("a-%05d.txt", index), "x")
	}
	writeFindFile(t, directory, "z-newest.txt", "x")
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(directory, "z-newest.txt"), future, future); err != nil {
		t.Fatal(err)
	}
	got := callFind(t, newFindTestTool(t, directory), context.Background(), map[string]any{
		"glob": "*.txt", "type": entryTypeFile, "sort_by": "modified", "max_results": 1,
	})
	if !reflect.DeepEqual(got.Paths, []string{"z-newest.txt"}) || !got.Truncated {
		t.Fatalf("modified result = %#v", got)
	}
}

const findWalkMaxRegressionEntries = 20000

func TestFindBoundsOutputAndPublishesSortedFileTouches(t *testing.T) {
	directory := t.TempDir()
	for _, path := range []string{"z.txt", "a.txt", "directory/child.txt"} {
		writeFindFile(t, directory, path, "x")
	}
	root, err := workspace.NewRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	bus := workspace.NewTouchBus()
	observer := &listObserver{}
	bus.Subscribe(observer)
	budget := outputlimit.NewBudget(200)
	tool, err := newFindHandler(Config{
		Root: root, Touch: bus, Budget: budget,
		Output: outputlimit.Policy{MaxBytes: 1000, MaxLines: 100, MaxLineBytes: 1000, HeadFraction: 0.6},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := adaptTestHandler(t, tool.call)(context.Background(), "budget", map[string]any{"glob": "*", "type": "any"})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > 200 {
		t.Fatalf("encoded output = %d bytes, grant 200", len(encoded))
	}
	touches := observer.snapshot()
	paths := make([]string, len(touches))
	for index, touch := range touches {
		if touch.Kind != workspace.TouchSearch {
			t.Fatalf("touch = %#v", touch)
		}
		paths[index] = touch.Path
	}
	if want := []string{"a.txt", "directory/child.txt", "z.txt"}; !reflect.DeepEqual(paths, want) {
		t.Fatalf("touch paths = %#v, want %#v", paths, want)
	}
}

func newFindTestTool(t *testing.T, directory string) *findHandler {
	t.Helper()
	root, err := workspace.NewRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	tool, err := newFindHandler(Config{
		Root: root, Touch: workspace.NewTouchBus(), Budget: outputlimit.NewBudget(10000),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return tool
}

func callFind(t *testing.T, tool *findHandler, ctx context.Context, args map[string]any) FindResult {
	t.Helper()
	result, err := adaptTestHandler(t, tool.call)(ctx, findToolName, args)
	if err != nil {
		t.Fatal(err)
	}
	var decoded FindResult
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func writeFindFile(t *testing.T, root, path, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
