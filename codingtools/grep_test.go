package codingtools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/plasmid-dev/plasmid/outputlimit"
	"github.com/plasmid-dev/plasmid/warning"
	"github.com/plasmid-dev/plasmid/workspace"
)

func newGrepTool(t *testing.T, dir string) *grepHandler {
	t.Helper()
	root, err := workspace.NewRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	tool, err := newGrepHandler(Config{Root: root, Touch: workspace.NewTouchBus(), Budget: outputlimit.NewBudget(100000)})
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
	tool := newGrepTool(t, dir)
	result, err := adaptTestHandler(t, tool.call)(context.Background(), "s", map[string]any{"pattern": "needle", "context_lines": 1})
	if err != nil {
		t.Fatal(err)
	}
	if result["skipped_binary"] != float64(1) {
		t.Fatalf("result = %#v", result)
	}
	matches := result["matches"].([]any)
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
		_, err := adaptTestHandler(t, tool.call)(context.Background(), "", args)
		if err == nil {
			t.Fatalf("args %#v accepted", args)
		}
	}
	_, err := adaptTestHandler(t, tool.call)(context.Background(), "", map[string]any{"pattern": "(?=x)"})
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
	tool, err := newGrepHandler(Config{Root: root, Touch: bus, Budget: outputlimit.NewBudget(100000)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adaptTestHandler(t, tool.call)(context.Background(), "search", map[string]any{"pattern": "needle"}); err != nil {
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

func TestSearchTouchesAreCappedAndWarned(t *testing.T) {
	bus := workspace.NewTouchBus()
	observer := &listObserver{}
	bus.Subscribe(observer)
	var warnings warning.SliceSink
	paths := make([]string, 0, 600)
	for index := 299; index >= 0; index-- {
		path := fmt.Sprintf("p-%03d.txt", index)
		paths = append(paths, path, path)
	}
	publishSearchTouches(context.Background(), bus, &warnings, "session", paths, MaxTouchEvents)
	touches := observer.snapshot()
	if len(touches) != MaxTouchEvents || touches[0].Path != "p-000.txt" || touches[len(touches)-1].Path != "p-255.txt" {
		t.Fatalf("touch boundary = %d, %q, %q", len(touches), touches[0].Path, touches[len(touches)-1].Path)
	}
	gotWarnings := warnings.Warnings()
	if len(gotWarnings) != 1 || gotWarnings[0].Code != warning.WarnContextTouchOverflow || gotWarnings[0].Source != "codingtools" || gotWarnings[0].Path != "" {
		t.Fatalf("warnings = %#v", gotWarnings)
	}
}

func TestSearchTouchesUseConfiguredCap(t *testing.T) {
	bus := workspace.NewTouchBus()
	observer := &listObserver{}
	bus.Subscribe(observer)
	var warnings warning.SliceSink
	publishSearchTouches(context.Background(), bus, &warnings, "session", []string{"d", "c", "b", "a"}, 3)
	touches := observer.snapshot()
	if len(touches) != 3 || touches[0].Path != "a" || touches[2].Path != "c" {
		t.Fatalf("touches = %#v", touches)
	}
	if got := warnings.Warnings(); len(got) != 1 || got[0].Code != warning.WarnContextTouchOverflow {
		t.Fatalf("warnings = %#v", got)
	}
}

func TestSearchToolsDefaultWarningSinkUsesConfiguredLogger(t *testing.T) {
	root, err := workspace.NewRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	paths := make([]string, MaxTouchEvents+1)
	for index := range paths {
		paths[index] = fmt.Sprintf("p-%03d.txt", index)
	}
	tests := []struct {
		name      string
		construct func(Config) (warning.Sink, error)
	}{
		{"grep", func(cfg Config) (warning.Sink, error) {
			tool, err := newGrepHandler(cfg)
			if err != nil {
				return nil, err
			}
			return tool.warnings, nil
		}},
		{"find", func(cfg Config) (warning.Sink, error) {
			tool, err := newFindHandler(cfg)
			if err != nil {
				return nil, err
			}
			return tool.warnings, nil
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertSearchToolDefaultWarning(t, root, paths, test.construct)
		})
	}
}

func assertSearchToolDefaultWarning(t *testing.T, root *workspace.Root, paths []string, construct func(Config) (warning.Sink, error)) {
	t.Helper()
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	sink, err := construct(Config{Root: root, Touch: workspace.NewTouchBus(), Budget: outputlimit.NewBudget(100000), Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	publishSearchTouches(context.Background(), workspace.NewTouchBus(), sink, "session", paths, MaxTouchEvents)
	var got map[string]any
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["code"] != warning.WarnContextTouchOverflow || got["source"] != "codingtools" || got["path"] != "" || got["line"] != float64(0) {
		t.Fatalf("warning log = %#v", got)
	}
}

func TestGrepRoutesNestedWalkWarningsToConfiguredSink(t *testing.T) {
	directory := t.TempDir()
	if err := os.MkdirAll(filepath.Join(directory, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "nested", ".gitignore"), []byte("[broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "nested", "file.txt"), []byte("needle\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := workspace.NewRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	var warnings warning.SliceSink
	tool, err := newGrepHandler(Config{
		Root:        root,
		Touch:       workspace.NewTouchBus(),
		Budget:      outputlimit.NewBudget(100000),
		WarningSink: &warnings,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adaptTestHandler(t, tool.call)(context.Background(), "", map[string]any{"pattern": "needle"}); err != nil {
		t.Fatal(err)
	}
	got := warnings.Warnings()
	if len(got) != 1 || got[0].Code != warning.WarnWalkInvalidIgnorePattern || got[0].Source != "walk" || got[0].Path != "nested/.gitignore" || got[0].Line != 1 {
		t.Fatalf("warnings = %#v", got)
	}
}
