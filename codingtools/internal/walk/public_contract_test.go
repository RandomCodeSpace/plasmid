package walk_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/plasmid-dev/plasmid/codingtools/internal/walk"
	"github.com/plasmid-dev/plasmid/warning"
	"github.com/plasmid-dev/plasmid/workspace"
)

func TestWalkValidatesPublicContract(t *testing.T) {
	t.Parallel()
	root, err := workspace.NewRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	callback := func(walk.Entry) error { return nil }
	var missingContext context.Context
	if err := walk.Walk(missingContext, &walk.Filter{Root: root}, callback); err == nil || !strings.Contains(err.Error(), "context is nil") {
		t.Fatalf("Walk() error = %v, want text %q", err, "context is nil")
	}
	tests := []struct {
		name     string
		filter   *walk.Filter
		callback func(walk.Entry) error
		want     string
	}{
		{name: "nil filter", callback: callback, want: "filter is nil"},
		{name: "nil root", filter: &walk.Filter{}, callback: callback, want: "root is nil"},
		{name: "nil callback", filter: &walk.Filter{Root: root}, want: "callback is nil"},
		{name: "invalid include", filter: &walk.Filter{Root: root, IncludeGlobs: []string{"["}}, callback: callback, want: "compile include globs"},
		{name: "invalid exclude", filter: &walk.Filter{Root: root, ExcludeGlobs: []string{"["}}, callback: callback, want: "compile exclude globs"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := walk.Walk(t.Context(), test.filter, test.callback)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Walk() error = %v, want text %q", err, test.want)
			}
		})
	}
}

func TestWalkAppliesFileFiltersAndWarningSink(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	for path, content := range map[string]string{
		".git":       "not a directory",
		"drop.txt":   "drop",
		"keep.go":    "keep",
		"nested.go":  "nested",
		".gitignore": "[invalid\n",
	} {
		if err := os.WriteFile(filepath.Join(directory, path), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	root, err := workspace.NewRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	sink := &warning.SliceSink{}
	var paths []string
	err = walk.Walk(t.Context(), &walk.Filter{
		Root:             root,
		WarningSink:      sink,
		IncludeGlobs:     []string{"*.go"},
		ExcludeGlobs:     []string{"nested.go"},
		SkipVCS:          true,
		RespectGitignore: true,
		MaxDepth:         -1,
	}, func(entry walk.Entry) error {
		paths = append(paths, entry.Path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(paths, []string{"keep.go"}) {
		t.Fatalf("Walk() paths = %#v, want [keep.go]", paths)
	}
	warnings := sink.Warnings()
	if len(warnings) != 2 || warnings[0].Code != warning.WarnWalkUnreadableIgnore || warnings[1].Code != warning.WarnWalkInvalidIgnorePattern {
		t.Fatalf("Walk() warnings = %#v", warnings)
	}
}

func TestWalkReturnsCallbackError(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := workspace.NewRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	want := errors.New("stop")
	err = walk.Walk(t.Context(), &walk.Filter{Root: root, MaxDepth: -1}, func(walk.Entry) error { return want })
	if !errors.Is(err, want) {
		t.Fatalf("Walk() error = %v, want %v", err, want)
	}
}

func TestWalkReportsOversizedAndEscapedIgnorePatterns(t *testing.T) {
	directory := t.TempDir()
	ignore := strings.Repeat("x", (1<<20)+1) + "\n"
	if err := os.WriteFile(filepath.Join(directory, ".gitignore"), []byte(ignore), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(directory, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "nested", ".gitignore"), []byte("foo\\/bar\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := workspace.NewRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	sink := &warning.SliceSink{}
	if err := walk.Walk(t.Context(), &walk.Filter{Root: root, WarningSink: sink, RespectGitignore: true, MaxDepth: -1}, func(walk.Entry) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if warnings := sink.Warnings(); len(warnings) != 1 || warnings[0].Code != warning.WarnWalkUnreadableIgnore {
		t.Fatalf("Walk() warnings = %#v", warnings)
	}
}

func TestWalkContainsPublicFilesystemAndCancellationFailures(t *testing.T) {
	t.Run("root removed", func(t *testing.T) {
		directory := t.TempDir()
		root, err := workspace.NewRoot(directory)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(directory); err != nil {
			t.Fatal(err)
		}
		if err := walk.Walk(t.Context(), &walk.Filter{Root: root, MaxDepth: -1}, func(walk.Entry) error { return nil }); err == nil {
			t.Fatal("Walk() succeeded after its root was removed")
		}
	})
	t.Run("callback cancellation", func(t *testing.T) {
		directory := t.TempDir()
		if err := os.WriteFile(filepath.Join(directory, "file"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		root, err := workspace.NewRoot(directory)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		err = walk.Walk(ctx, &walk.Filter{Root: root, MaxDepth: -1}, func(walk.Entry) error {
			cancel()
			return nil
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Walk() error = %v", err)
		}
	})
}
