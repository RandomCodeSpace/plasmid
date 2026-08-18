package codingtools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/plasmid-dev/plasmid/outputlimit"
	"github.com/plasmid-dev/plasmid/workspace"
)

type listObserver struct {
	mu      sync.Mutex
	touches []workspace.Touch
}

func (o *listObserver) ObserveTouch(_ context.Context, touch workspace.Touch) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.touches = append(o.touches, touch)
}

func (o *listObserver) snapshot() []workspace.Touch {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]workspace.Touch(nil), o.touches...)
}

type listHarness struct {
	tool     testNativeHandler
	root     string
	observer *listObserver
}

func newListHarness(t *testing.T, rootDir string) listHarness {
	t.Helper()
	root, err := workspace.NewRoot(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	bus := workspace.NewTouchBus()
	observer := &listObserver{}
	bus.Subscribe(observer)
	tool, err := newListHandler(Config{Root: root, Touch: bus, Budget: outputlimit.NewBudget(10000)})
	if err != nil {
		t.Fatal(err)
	}
	return listHarness{tool: adaptTestHandler(t, tool.call), root: rootDir, observer: observer}
}

func decodeListResult(t *testing.T, content map[string]any) ListResult {
	t.Helper()
	encoded, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}
	var result ListResult
	if err := json.Unmarshal(encoded, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestNewListToolContractAndDependencies(t *testing.T) {
	root, err := workspace.NewRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, cfg := range []Config{
		{Touch: workspace.NewTouchBus(), Budget: outputlimit.NewBudget(10000)},
		{Root: root, Budget: outputlimit.NewBudget(10000)},
		{Root: root, Touch: workspace.NewTouchBus()},
	} {
		if _, err := newListHandler(cfg); err == nil {
			t.Fatal("constructor accepted a missing dependency")
		}
	}
	tool, err := NewListTool(Config{Root: root, Touch: workspace.NewTouchBus(), Budget: outputlimit.NewBudget(10000)})
	if err != nil {
		t.Fatal(err)
	}
	if tool.Name() != "ls" || tool.Description() != ListDescription || tool.IsLongRunning() {
		t.Fatal("ls metadata drifted")
	}
	first := ListInputSchema()
	first[0] ^= 0xff
	if string(first) == string(ListInputSchema()) {
		t.Fatal("input schema aliases tool state")
	}
}

func TestListDirectoryDepthHiddenTypesAndTimes(t *testing.T) {
	h := newListHarness(t, t.TempDir())
	for path, content := range map[string]string{
		"a-dir/deep.txt": "deep",
		"a-dir/file.txt": entryTypeFile,
		"z-file.txt":     "z",
		".hidden/seen":   "hidden",
		".dot":           "dot",
	} {
		full := filepath.Join(h.root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	stamp := time.Date(2025, 2, 3, 4, 5, 6, 0, time.FixedZone("other", 2*60*60))
	if err := os.Chtimes(filepath.Join(h.root, "z-file.txt"), stamp, stamp); err != nil {
		t.Fatal(err)
	}
	result, err := h.tool(context.Background(), "session", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	got := decodeListResult(t, result)
	if got.Truncated {
		t.Fatal("unexpected truncation")
	}
	if paths := listPaths(got.Entries); !reflect.DeepEqual(paths, []string{"a-dir", "z-file.txt"}) {
		t.Fatalf("paths = %#v", paths)
	}
	if got.Entries[0].Type != entryTypeDirectory || got.Entries[1].Type != entryTypeFile || got.Entries[1].Size != 1 || got.Entries[1].ModTime != "2025-02-03T02:05:06Z" {
		t.Fatalf("entries = %#v", got.Entries)
	}
	visible, err := h.tool(context.Background(), "session", map[string]any{"max_depth": 2, "show_hidden": true})
	if err != nil {
		t.Fatal(err)
	}
	if paths := listPaths(decodeListResult(t, visible).Entries); !reflect.DeepEqual(paths, []string{".hidden", "a-dir", ".dot", ".hidden/seen", "a-dir/deep.txt", "a-dir/file.txt", "z-file.txt"}) {
		t.Fatalf("visible paths = %#v", paths)
	}
}

func TestListSymlinkDoesNotDescend(t *testing.T) {
	h := newListHarness(t, t.TempDir())
	if err := os.Mkdir(filepath.Join(h.root, "target"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.root, "target", "child"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target", filepath.Join(h.root, "link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	result, err := h.tool(context.Background(), "session", map[string]any{"max_depth": 2})
	if err != nil {
		t.Fatal(err)
	}
	entries := decodeListResult(t, result).Entries
	if paths := listPaths(entries); !reflect.DeepEqual(paths, []string{"target", "link", "target/child"}) {
		t.Fatalf("paths = %#v", paths)
	}
	if entries[1].Type != entryTypeSymlink {
		t.Fatalf("link type = %q", entries[1].Type)
	}
}

func TestListTruncationSandboxCancellationAndTouch(t *testing.T) {
	h := newListHarness(t, t.TempDir())
	if err := os.WriteFile(filepath.Join(h.root, "b"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(h.root, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.root, "c"), []byte("c"), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := h.tool(context.Background(), "session", map[string]any{"max_results": 1})
	if err != nil {
		t.Fatal(err)
	}
	listed := decodeListResult(t, result)
	if !listed.Truncated || !reflect.DeepEqual(listPaths(listed.Entries), []string{"a"}) {
		t.Fatalf("truncated result = %#v", listed)
	}
	touches := h.observer.snapshot()
	if len(touches) != 1 || touches[0].Kind != workspace.TouchList || touches[0].Path != "." || touches[0].SessionID != "session" {
		t.Fatalf("touches = %#v", touches)
	}
	_, err = h.tool(context.Background(), "", map[string]any{"path": "../outside"})
	if !errors.Is(err, ErrPathOutsideRoot) {
		t.Fatalf("outside error = %v", err)
	}
	_, err = h.tool(context.Background(), "", map[string]any{"path": "b"})
	if !errors.Is(err, workspace.ErrNotDirectory) {
		t.Fatalf("file error = %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = h.tool(cancelled, "", map[string]any{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled error = %v", err)
	}
	if got := h.observer.snapshot(); len(got) != 1 {
		t.Fatalf("failed calls published touches: %#v", got)
	}
}

func TestListSortsBeforeApplyingResultLimit(t *testing.T) {
	h := newListHarness(t, t.TempDir())
	for _, name := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(h.root, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(h.root, "z"), 0o755); err != nil {
		t.Fatal(err)
	}
	result, err := h.tool(context.Background(), "", map[string]any{"max_results": 1})
	if err != nil {
		t.Fatal(err)
	}
	if got := listPaths(decodeListResult(t, result).Entries); !reflect.DeepEqual(got, []string{"z"}) {
		t.Fatalf("globally sorted entries = %#v, want directory z", got)
	}
}

func TestListBoundsJSONOutputWithSessionBudget(t *testing.T) {
	directory := t.TempDir()
	for index := range 20 {
		name := fmt.Sprintf("entry-%02d-with-a-deliberately-long-name", index)
		if err := os.WriteFile(filepath.Join(directory, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	root, err := workspace.NewRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	budget := outputlimit.NewBudget(200)
	tool, err := newListHandler(Config{
		Root: root, Touch: workspace.NewTouchBus(), Budget: budget,
		Output: outputlimit.Policy{MaxBytes: 1000, MaxLines: 100, MaxLineBytes: 1000, HeadFraction: 0.6},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := adaptTestHandler(t, tool.call)(context.Background(), "budget", map[string]any{})
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
	if got := decodeListResult(t, result); !got.Truncated {
		t.Fatalf("bounded result = %#v", got)
	}
}

func listPaths(entries []ListEntry) []string {
	paths := make([]string, len(entries))
	for index, entry := range entries {
		paths[index] = entry.Path
	}
	return paths
}
