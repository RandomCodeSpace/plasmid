package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRootResolution(t *testing.T) {
	base := t.TempDir()
	rootDir := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	mustMkdir(t, filepath.Join(rootDir, "nested"))
	mustMkdir(t, outside)
	mustWriteFile(t, filepath.Join(rootDir, "nested", "file.txt"), "data")
	mustWriteFile(t, filepath.Join(outside, "secret.txt"), "secret")
	mustSymlink(t, filepath.Join(rootDir, "nested"), filepath.Join(rootDir, "inside-link"))
	mustSymlink(t, outside, filepath.Join(rootDir, "outside-link"))
	mustSymlink(t, filepath.Join(rootDir, "missing-target"), filepath.Join(rootDir, "dangling-link"))

	root, err := NewRoot(rootDir)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		path string
		want string
		err  error
	}{
		{name: "relative existing", path: "nested/file.txt", want: filepath.Join(rootDir, "nested", "file.txt")},
		{name: "absolute existing", path: filepath.Join(rootDir, "nested", "file.txt"), want: filepath.Join(rootDir, "nested", "file.txt")},
		{name: "root", path: ".", want: rootDir},
		{name: "lexical escape", path: "../outside/secret.txt", err: ErrOutsideRoot},
		{name: "outside absolute", path: filepath.Join(outside, "secret.txt"), err: ErrOutsideRoot},
		{name: "in root symlink", path: "inside-link/file.txt", want: filepath.Join(rootDir, "nested", "file.txt")},
		{name: "escaping symlink", path: "outside-link/secret.txt", err: ErrOutsideRoot},
		{name: "missing tail", path: "nested/new/child.txt", want: filepath.Join(rootDir, "nested", "new", "child.txt")},
		{name: "missing tail through symlink", path: "inside-link/new/child.txt", want: filepath.Join(rootDir, "nested", "new", "child.txt")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := root.Resolve(test.path)
			if !errors.Is(err, test.err) {
				t.Fatalf("Resolve(%q) error = %v, want %v", test.path, err, test.err)
			}
			if got != test.want {
				t.Fatalf("Resolve(%q) = %q, want %q", test.path, got, test.want)
			}
		})
	}

	if _, err := root.Resolve(""); !errors.Is(err, ErrPathEmpty) {
		t.Fatalf("empty path error = %v", err)
	}
	if _, err := root.ResolveExisting("nested/nope.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing read error = %v", err)
	}
	if _, err := root.ResolveExisting("dangling-link"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("dangling symlink read error = %v", err)
	}
	if _, err := root.ResolveExisting("outside-link/secret.txt"); !errors.Is(err, ErrOutsideRoot) {
		t.Fatalf("escaping read error = %v", err)
	}
	if _, err := root.ResolveForWrite("outside-link/new.txt"); !errors.Is(err, ErrOutsideRoot) {
		t.Fatalf("escaping write error = %v", err)
	}
}

func TestRootCWDIndependenceAndRel(t *testing.T) {
	rootDir := t.TempDir()
	mustMkdir(t, filepath.Join(rootDir, "dir"))
	root, err := NewRoot(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(rootDir, "dir", "future.txt")
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(old) }()
	got, err := root.ResolveForWrite("dir/future.txt")
	if err != nil || got != want {
		t.Fatalf("cwd-independent ResolveForWrite = %q, %v; want %q, nil", got, err, want)
	}
	if rel := root.Rel(want); rel != "dir/future.txt" {
		t.Fatalf("Rel = %q", rel)
	}
	if rel := root.Rel(root.Dir()); rel != "." {
		t.Fatalf("root Rel = %q", rel)
	}
}

func TestNewRootRejectsFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	mustWriteFile(t, file, "x")
	if _, err := NewRoot(file); !errors.Is(err, ErrNotDirectory) {
		t.Fatalf("NewRoot file error = %v", err)
	}
}

func TestRootFilesystemRootContainsDescendants(t *testing.T) {
	root, err := NewRoot(string(filepath.Separator))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := root.Resolve(filepath.Join("tmp", "plasmid-root-test")); err != nil {
		t.Fatalf("Resolve descendant of filesystem root: %v", err)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}
