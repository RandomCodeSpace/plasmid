package codingtools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/plasmid-dev/plasmid/loop"
	"github.com/plasmid-dev/plasmid/outputlimit"
	"github.com/plasmid-dev/plasmid/workspace"
)

func newGrepTool(t *testing.T, dir string) loop.Tool {
	t.Helper()
	root, err := workspace.NewRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	tool, err := NewGrepTool(Config{Root: root, Touch: workspace.NewTouchBus(), Budget: outputlimit.NewBudget(100000)})
	if err != nil {
		t.Fatal(err)
	}
	return tool
}

func TestGrepToolSearchesAndSorts(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{"z.txt": "no\nneedle z\nend\n", "a.txt": "before\nneedle a\nafter\n", "bin": "\x00needle"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	result, err := newGrepTool(t, dir).Call(context.Background(), loop.ToolCall{ID: "call", SessionID: "s", Args: map[string]any{"pattern": "needle", "context_lines": 1}})
	if err != nil {
		t.Fatal(err)
	}
	if result.CallID != "call" || result.Content["skipped_binary"] != float64(1) {
		t.Fatalf("result = %#v", result)
	}
	matches := result.Content["matches"].([]any)
	if len(matches) != 2 || matches[0].(map[string]any)["path"] != "a.txt" {
		t.Fatalf("matches = %#v", matches)
	}
	if got := matches[0].(map[string]any)["before"].([]any); len(got) != 1 || got[0] != "before" {
		t.Fatalf("before = %#v", got)
	}
}

func TestGrepToolStrictAndPortable(t *testing.T) {
	tool := newGrepTool(t, t.TempDir())
	for _, args := range []map[string]any{{}, {"pattern": 1}, {"pattern": "x", "extra": true}, {"pattern": "(?=x)"}, {"pattern": "\\1"}} {
		_, err := tool.Call(context.Background(), loop.ToolCall{Args: args})
		if err == nil {
			t.Fatalf("args %#v accepted", args)
		}
	}
	_, err := tool.Call(context.Background(), loop.ToolCall{Args: map[string]any{"pattern": "(?=x)"}})
	if !errors.Is(err, ErrUnsupportedPattern) {
		t.Fatalf("error = %v", err)
	}
}

func TestGrepPublishesSortedDeduplicatedMatchedFileTouches(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"z.txt":  "needle\nneedle\n",
		"a.txt":  "needle\n",
		"no.txt": "other\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	root, err := workspace.NewRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	bus := workspace.NewTouchBus()
	observer := &listObserver{}
	bus.Subscribe(observer)
	tool, err := NewGrepTool(Config{Root: root, Touch: bus, Budget: outputlimit.NewBudget(100000)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Call(context.Background(), loop.ToolCall{SessionID: "search", Args: map[string]any{"pattern": "needle"}}); err != nil {
		t.Fatal(err)
	}
	touches := observer.snapshot()
	paths := make([]string, len(touches))
	for index, touch := range touches {
		if touch.Kind != workspace.TouchSearch || touch.SessionID != "search" {
			t.Fatalf("touch = %#v", touch)
		}
		paths[index] = touch.Path
	}
	if want := []string{"a.txt", "z.txt"}; !reflect.DeepEqual(paths, want) {
		t.Fatalf("touch paths = %#v, want %#v", paths, want)
	}
}

func TestGrepReportsWalkTruncation(t *testing.T) {
	dir := t.TempDir()
	root, err := workspace.NewRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	tool := &GrepTool{root: root, touch: workspace.NewTouchBus(), output: outputlimit.Defaults(), logger: slog.Default()}
	emitted := 0
	result, err := tool.finish(context.Background(), loop.ToolCall{ID: "call"}, root.Dir(), 10, grepState{walkTruncated: true}, 1000, &emitted, loop.ToolResult{CallID: "call"})
	if err != nil {
		t.Fatal(err)
	}
	if result.CallID != "call" || result.Content["truncated"] != true {
		t.Fatalf("truncated result = %#v", result)
	}
}

func TestSearchTouchesAreCappedAndWarned(t *testing.T) {
	bus := workspace.NewTouchBus()
	observer := &listObserver{}
	bus.Subscribe(observer)
	var logOutput bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logOutput, nil))
	paths := make([]string, 0, 600)
	for index := 299; index >= 0; index-- {
		path := fmt.Sprintf("p-%03d.txt", index)
		paths = append(paths, path, path)
	}
	publishSearchTouches(context.Background(), bus, logger, "session", paths)
	touches := observer.snapshot()
	if len(touches) != MaxTouchEvents || touches[0].Path != "p-000.txt" || touches[len(touches)-1].Path != "p-255.txt" {
		t.Fatalf("touch boundary = %d, %q, %q", len(touches), touches[0].Path, touches[len(touches)-1].Path)
	}
	if !strings.Contains(logOutput.String(), loop.WarnContextTouchOverflow) {
		t.Fatalf("warning log = %q", logOutput.String())
	}
}
