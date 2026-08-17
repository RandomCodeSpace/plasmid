// Package workspace provides framework-free workspace state and coordination.
package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var (
	ErrPathEmpty      = errors.New("workspace path is empty")
	ErrOutsideRoot    = errors.New("workspace path is outside root")
	ErrNotFound       = errors.New("workspace path not found")
	ErrNotDirectory   = errors.New("workspace root is not a directory")
	ErrIsDirectory    = errors.New("workspace path is a directory")
	ErrNotRegularFile = errors.New("workspace path is not a regular file")
	ErrTooLarge       = errors.New("workspace file is too large")
	ErrBinaryFile     = errors.New("workspace file is binary")
)

// Root is an immutable, symlink-resolved workspace directory.
type Root struct {
	dir string
}

// NewRoot creates a root from an existing directory.
func NewRoot(dir string) (*Root, error) {
	if dir == "" {
		return nil, fmt.Errorf("new root: %w", ErrPathEmpty)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("new root: %w", err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("new root: %w", err)
	}
	info, err := os.Stat(real)
	if err != nil {
		return nil, fmt.Errorf("new root: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("new root: %w", ErrNotDirectory)
	}
	return &Root{dir: real}, nil
}

// Dir returns the fully resolved absolute root directory.
func (r *Root) Dir() string { return r.dir }

// Rel returns a deterministic slash-separated path relative to the root.
// Callers should pass paths returned by Resolve, ResolveExisting, or ResolveForWrite.
func (r *Root) Rel(abs string) string {
	rel, err := filepath.Rel(r.dir, abs)
	if err != nil {
		return filepath.ToSlash(abs)
	}
	if rel == "." {
		return "."
	}
	return filepath.ToSlash(rel)
}

// Resolve resolves a path inside the root. It works for existing paths and
// prospective write paths whose final components do not yet exist.
func (r *Root) Resolve(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("resolve path: %w", ErrPathEmpty)
	}
	if r == nil || r.dir == "" {
		return "", fmt.Errorf("resolve path: %w", ErrOutsideRoot)
	}
	candidate := path
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(r.dir, candidate)
	}
	candidate = filepath.Clean(candidate)
	resolved, err := resolveExistingAncestor(candidate)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	if !Contains(r.dir, resolved) {
		return "", fmt.Errorf("resolve path %q: %w", path, ErrOutsideRoot)
	}
	return resolved, nil
}

// ResolveExisting resolves a path and requires it to exist.
func (r *Root) ResolveExisting(path string) (string, error) {
	resolved, err := r.Resolve(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("resolve existing path %q: %w", path, ErrNotFound)
		}
		return "", err
	}
	if _, err := os.Stat(resolved); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("resolve existing path %q: %w", path, ErrNotFound)
		}
		return "", fmt.Errorf("resolve existing path %q: %w", path, err)
	}
	return resolved, nil
}

// ResolveForWrite resolves a path and verifies its existing parent chain stays
// in the root. It intentionally does not require the target to exist.
func (r *Root) ResolveForWrite(path string) (string, error) {
	resolved, err := r.Resolve(path)
	if err != nil {
		return "", err
	}
	parent, err := resolveExistingAncestor(filepath.Dir(resolved))
	if err != nil {
		return "", fmt.Errorf("resolve write path: %w", err)
	}
	if !Contains(r.dir, parent) {
		return "", fmt.Errorf("resolve write path %q: %w", path, ErrOutsideRoot)
	}
	return resolved, nil
}

func resolveExistingAncestor(path string) (string, error) {
	path = filepath.Clean(path)
	var tail []string
	ancestor := path
	for {
		_, err := os.Lstat(ancestor)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return "", err
		}
		tail = append(tail, filepath.Base(ancestor))
		ancestor = parent
	}
	real, err := filepath.EvalSymlinks(ancestor)
	if err != nil {
		return "", err
	}
	for i := len(tail) - 1; i >= 0; i-- {
		real = filepath.Join(real, tail[i])
	}
	return filepath.Clean(real), nil
}

// Contains reports whether path is lexically contained by root. Callers that
// require symlink-aware containment must resolve both paths first.
func Contains(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
