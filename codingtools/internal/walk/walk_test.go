package walk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"sync"
	"testing"

	"github.com/RandomCodeSpace/plasmid/warning"
	"github.com/RandomCodeSpace/plasmid/workspace"
)

func TestWalkDepthAndRootEntry(t *testing.T) {
	t.Parallel()
	root := fixtureRoot(t, map[string]string{
		"root.txt":         "root",
		"one/file.txt":     "one",
		"one/two/file.txt": "two",
	})
	tests := []struct {
		name  string
		depth int
		want  []string
	}{
		{name: "zero", depth: 0, want: []string{}},
		{name: "one", depth: 1, want: []string{"one", "root.txt"}},
		{name: "two", depth: 2, want: []string{"one", "one/file.txt", "one/two", "root.txt"}},
		{name: "unlimited", depth: -1, want: []string{"one", "one/file.txt", "one/two", "one/two/file.txt", "root.txt"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			entries, err := collect(t, root, Filter{MaxDepth: test.depth})
			if err != nil {
				t.Fatal(err)
			}
			if paths := entryPaths(entries); !reflect.DeepEqual(paths, test.want) {
				t.Fatalf("paths = %#v, want %#v", paths, test.want)
			}
			for _, entry := range entries {
				if entry.Path == "." {
					t.Fatal("walk emitted the root entry")
				}
			}
		})
	}
}

func TestWalkHiddenVCSAndGlobs(t *testing.T) {
	t.Parallel()
	root := fixtureRoot(t, map[string]string{
		".git/config":      "ignored",
		".hg/store":        "hg",
		".hidden/file.txt": "hidden",
		"Name.txt":         "upper",
		"drop/file.go":     "drop",
		"keep/file.go":     "keep",
		"keep/file.txt":    "txt",
		"name.txt":         "lower",
	})
	tests := []struct {
		name   string
		filter Filter
		want   []string
	}{
		{
			name:   "hidden and vcs",
			filter: Filter{MaxDepth: -1, SkipHidden: true, SkipVCS: true},
			want:   []string{"Name.txt", "drop", "drop/file.go", "keep", "keep/file.go", "keep/file.txt", "name.txt"},
		},
		{
			name:   "git is unconditional",
			filter: Filter{MaxDepth: -1},
			want:   []string{".hg", ".hg/store", ".hidden", ".hidden/file.txt", "Name.txt", "drop", "drop/file.go", "keep", "keep/file.go", "keep/file.txt", "name.txt"},
		},
		{
			name:   "include files and exclude directory",
			filter: Filter{MaxDepth: -1, IncludeGlobs: []string{"*.go"}, ExcludeGlobs: []string{"drop/"}},
			want:   []string{".hg", ".hidden", "keep", "keep/file.go"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			entries, err := collect(t, root, test.filter)
			if err != nil {
				t.Fatal(err)
			}
			if got := entryPaths(entries); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("paths = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestWalkGitignorePrecedenceAndWarnings(t *testing.T) {
	t.Parallel()
	root := fixtureRoot(t, map[string]string{
		".git/info/exclude":     "info.txt\n!.git/\n",
		".git/secret":           "secret",
		".gitignore":            "*.log\n!keep.log\n/build\n[broken\nblocked/\nnested/*.tmp\nnested/drop/\n",
		"a.log":                 "drop",
		"blocked/.gitignore":    "!file.txt\n",
		"blocked/file.txt":      "still blocked",
		"build/file.txt":        "root build",
		"info.txt":              "excluded by info",
		"keep.log":              "keep",
		"nested/.gitignore":     "!keep.tmp\n!drop/\n",
		"nested/build/file.txt": "nested build",
		"nested/drop/file.txt":  "reincluded directory",
		"nested/drop.tmp":       "drop",
		"nested/keep.tmp":       "keep",
	})
	var warnings warning.SliceSink
	var entries []Entry
	err := walk(context.Background(), &Filter{
		Root: root, MaxDepth: -1, SkipHidden: true, RespectGitignore: true,
	}, func(entry Entry) error {
		entries = append(entries, entry)
		return nil
	}, &warnings)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"keep.log",
		"nested",
		"nested/build",
		"nested/build/file.txt",
		"nested/drop",
		"nested/drop/file.txt",
		"nested/keep.tmp",
	}
	if got := entryPaths(entries); !reflect.DeepEqual(got, want) {
		t.Fatalf("paths = %#v, want %#v", got, want)
	}
	gotWarnings := warnings.Warnings()
	if len(gotWarnings) != 1 || gotWarnings[0].Code != warning.WarnWalkInvalidIgnorePattern || gotWarnings[0].Path != ".gitignore" || gotWarnings[0].Line != 4 {
		t.Fatalf("warnings = %#v", gotWarnings)
	}
}

func TestWalkGitignoreCanBeDisabled(t *testing.T) {
	t.Parallel()
	root := fixtureRoot(t, map[string]string{
		".git/info/exclude": "info.txt\n",
		".gitignore":        "*.log\n",
		"a.log":             "visible",
		"info.txt":          "visible",
	})
	entries, err := collect(t, root, Filter{MaxDepth: -1})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{".gitignore", "a.log", "info.txt"}
	if got := entryPaths(entries); !reflect.DeepEqual(got, want) {
		t.Fatalf("paths = %#v, want %#v", got, want)
	}
}

func TestWalkDefaultWarningSinkUsesSlogDefault(t *testing.T) {
	root := fixtureRoot(t, map[string]string{
		"nested/.gitignore": "[broken\n",
		"nested/file.txt":   "visible",
	})
	var output bytes.Buffer
	prior := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(prior) })

	err := Walk(context.Background(), &Filter{
		Root: root, MaxDepth: -1, SkipHidden: true, RespectGitignore: true,
	}, func(Entry) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["code"] != warning.WarnWalkInvalidIgnorePattern || got["source"] != "walk" || got["path"] != "nested/.gitignore" || got["line"] != float64(1) {
		t.Fatalf("warning log = %#v", got)
	}
}

func TestWalkRejectsSymlinkedIgnoreFiles(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		linkPath   string
		linkTarget string
	}{
		{name: "gitignore", linkPath: ".gitignore", linkTarget: "outside-ignore"},
		{name: "git info parent", linkPath: ".git", linkTarget: "outside-git"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			assertSymlinkedIgnoreRejected(t, test.linkPath, test.linkTarget)
		})
	}
}

func assertSymlinkedIgnoreRejected(t *testing.T, linkPath, linkTarget string) {
	t.Helper()
	directory := t.TempDir()
	target := prepareSymlinkedIgnoreTarget(t, linkPath, linkTarget)
	if err := os.WriteFile(filepath.Join(directory, "outside.txt"), []byte("visible"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(directory, linkPath)); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	root, err := workspace.NewRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	var warnings warning.SliceSink
	var entries []Entry
	err = walk(context.Background(), &Filter{Root: root, MaxDepth: -1, RespectGitignore: true, SkipHidden: true}, func(entry Entry) error {
		entries = append(entries, entry)
		return nil
	}, &warnings)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := entryPaths(entries), []string{"outside.txt"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("paths = %#v, want %#v", got, want)
	}
	gotWarnings := warnings.Warnings()
	if len(gotWarnings) != 1 || gotWarnings[0].Code != warning.WarnWalkUnreadableIgnore {
		t.Fatalf("warnings = %#v", gotWarnings)
	}
}

func prepareSymlinkedIgnoreTarget(t *testing.T, linkPath, linkTarget string) string {
	t.Helper()
	target := filepath.Join(t.TempDir(), linkTarget)
	if linkPath == ".gitignore" {
		if err := os.WriteFile(target, []byte("outside.txt\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return target
	}
	if err := os.MkdirAll(filepath.Join(target, "info"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "info", "exclude"), []byte("outside.txt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return target
}

func TestWalkCapsAndErrors(t *testing.T) {
	t.Parallel()
	root := fixtureRoot(t, map[string]string{"a": "a", "b": "b"})
	tests := []walkCapCase{
		{name: "visited includes root", filter: Filter{MaxDepth: -1, MaxVisited: 1}, wantPaths: nil, wantError: ErrWalkTruncated},
		{name: "result boundary emitted", filter: Filter{MaxDepth: -1, MaxResults: 1}, wantPaths: []string{"a"}, wantError: ErrWalkTruncated},
		{name: "zero caps use defaults", filter: Filter{MaxDepth: -1}, wantPaths: []string{"a", "b"}},
		{name: "negative caps unlimited", filter: Filter{MaxDepth: -1, MaxVisited: -1, MaxResults: -1}, wantPaths: []string{"a", "b"}},
		{name: "callback", filter: Filter{MaxDepth: -1}, callbackError: errors.New("callback failed"), wantPaths: []string{"a"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertWalkCapCase(t, root, test)
		})
	}
}

type walkCapCase struct {
	name          string
	filter        Filter
	callbackError error
	wantPaths     []string
	wantError     error
}

func assertWalkCapCase(t *testing.T, root *workspace.Root, test walkCapCase) {
	t.Helper()
	var paths []string
	err := Walk(context.Background(), withRoot(root, test.filter), func(entry Entry) error {
		paths = append(paths, entry.Path)
		return test.callbackError
	})
	if !reflect.DeepEqual(paths, test.wantPaths) {
		t.Fatalf("paths = %#v, want %#v", paths, test.wantPaths)
	}
	if test.callbackError != nil {
		if err != test.callbackError {
			t.Fatalf("error = %v, want callback error", err)
		}
		return
	}
	if test.wantError == nil && err != nil || test.wantError != nil && !errors.Is(err, test.wantError) {
		t.Fatalf("error = %v, want %v", err, test.wantError)
	}
}

func TestWalkCancellation(t *testing.T) {
	t.Parallel()
	root := fixtureRoot(t, map[string]string{"a": "a", "b": "b"})
	t.Run("before", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		called := false
		err := Walk(ctx, &Filter{Root: root, MaxDepth: -1}, func(Entry) error {
			called = true
			return nil
		})
		if !errors.Is(err, context.Canceled) || called {
			t.Fatalf("error = %v, called = %v", err, called)
		}
	})
	t.Run("from callback wins over cap", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		err := Walk(ctx, &Filter{Root: root, MaxDepth: -1, MaxResults: 1}, func(Entry) error {
			cancel()
			return nil
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context cancellation", err)
		}
	})
	t.Run("callback cancellation error", func(t *testing.T) {
		err := Walk(context.Background(), &Filter{Root: root, MaxDepth: -1}, func(Entry) error {
			return context.Canceled
		})
		if err != context.Canceled {
			t.Fatalf("error = %v, want callback's context error", err)
		}
	})
	t.Run("another goroutine", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ready := make(chan struct{})
		cancelled := make(chan struct{})
		go func() {
			<-ready
			cancel()
			close(cancelled)
		}()
		var once sync.Once
		err := Walk(ctx, &Filter{Root: root, MaxDepth: -1}, func(Entry) error {
			once.Do(func() { close(ready) })
			<-cancelled
			return nil
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context cancellation", err)
		}
	})
}

func TestWalkSymlinksNeverDescend(t *testing.T) {
	t.Parallel()
	directory := symlinkWalkFixture(t)
	root, err := workspace.NewRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, follow := range []bool{false, true} {
		entries, err := collect(t, root, Filter{MaxDepth: -1, FollowSymlinks: follow})
		if err != nil {
			t.Fatal(err)
		}
		paths := entryPaths(entries)
		want := []string{"dir-link", "escape-link", "file-link", "file.txt", "loop", "target", "target/inside.txt"}
		if !reflect.DeepEqual(paths, want) {
			t.Fatalf("follow %v paths = %#v, want %#v", follow, paths, want)
		}
		assertSymlinkEntries(t, directory, follow, entries)
	}
}

func symlinkWalkFixture(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	writeWalkFile(t, filepath.Join(directory, "file.txt"), "file")
	if err := os.Mkdir(filepath.Join(directory, "target"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeWalkFile(t, filepath.Join(directory, "target", "inside.txt"), "inside")
	outside := t.TempDir()
	writeWalkFile(t, filepath.Join(outside, "outside.txt"), "outside")
	for name, target := range map[string]string{"file-link": "file.txt", "dir-link": "target", "escape-link": outside, "loop": "."} {
		if err := os.Symlink(target, filepath.Join(directory, name)); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
	}
	return directory
}

func writeWalkFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertSymlinkEntries(t *testing.T, directory string, follow bool, entries []Entry) {
	t.Helper()
	for _, entry := range entries {
		if entry.Path != "dir-link" && entry.Path != "escape-link" && entry.Path != "file-link" && entry.Path != "loop" {
			continue
		}
		if !entry.IsSymlink || entry.IsDir || entry.Mode&os.ModeSymlink == 0 {
			t.Errorf("follow %v symlink entry = %#v", follow, entry)
		}
		info, err := os.Lstat(filepath.Join(directory, entry.Path))
		if err != nil {
			t.Fatal(err)
		}
		if entry.Size != info.Size() {
			t.Errorf("follow %v symlink size = %d, want lstat size %d", follow, entry.Size, info.Size())
		}
	}
}

func TestWalkPreservesSpecialNamesAndHiddenRoot(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	directory := filepath.Join(parent, ".selected")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	names := []string{"!bang", "#hash", "Name", "café", "name", "space name"}
	if runtime.GOOS != "windows" {
		names = append(names, string([]byte{'b', 'a', 'd', 0xff}))
	}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	root, err := workspace.NewRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := collect(t, root, Filter{MaxDepth: -1, SkipHidden: true})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(names)
	if got := entryPaths(entries); !reflect.DeepEqual(got, names) {
		t.Fatalf("paths = %#v, want %#v", got, names)
	}
}

func TestWalkDeterministic(t *testing.T) {
	t.Parallel()
	root := fixtureRoot(t, map[string]string{"z": "z", "a/x": "x", "a/A": "upper", "a/a": "lower"})
	first, err := collect(t, root, Filter{MaxDepth: -1})
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		next, err := collect(t, root, Filter{MaxDepth: -1})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(first, next) {
			t.Fatal("walk result was nondeterministic")
		}
	}
}

func fixtureRoot(t *testing.T, files map[string]string) *workspace.Root {
	t.Helper()
	directory := t.TempDir()
	for path, content := range files {
		absolute := filepath.Join(directory, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(absolute, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	root, err := workspace.NewRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func collect(t *testing.T, root *workspace.Root, filter Filter) ([]Entry, error) {
	t.Helper()
	filter.Root = root
	var entries []Entry
	err := Walk(context.Background(), &filter, func(entry Entry) error {
		entries = append(entries, entry)
		return nil
	})
	return entries, err
}

func withRoot(root *workspace.Root, filter Filter) *Filter {
	filter.Root = root
	return &filter
}

func entryPaths(entries []Entry) []string {
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		paths = append(paths, entry.Path)
	}
	return paths
}
