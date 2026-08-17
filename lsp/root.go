package lsp

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/plasmid-dev/plasmid/workspace"
)

// SelectWorkspaceRoot checks markers in precedence order and returns the
// nearest ancestor containing the first marker found anywhere beneath the
// workspace boundary. If none match, it returns workspaceDir.
func SelectWorkspaceRoot(workspaceDir, path string, markers []string) (string, error) {
	root, err := workspace.NewRoot(workspaceDir)
	if err != nil {
		return "", fmt.Errorf("select LSP root: %w", err)
	}
	resolved, err := root.ResolveExisting(path)
	if err != nil {
		return "", fmt.Errorf("select LSP root: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("select LSP root: %w", err)
	}
	directory := resolved
	if !info.IsDir() {
		directory = filepath.Dir(resolved)
	}
	for _, marker := range markers {
		if !validRootMarker(marker) {
			return "", fmt.Errorf("select LSP root: invalid marker %q", marker)
		}
	}
	for _, marker := range markers {
		for candidate := directory; ; candidate = filepath.Dir(candidate) {
			_, err := os.Lstat(filepath.Join(candidate, marker))
			if err == nil {
				return candidate, nil
			}
			if !errors.Is(err, os.ErrNotExist) {
				return "", fmt.Errorf("select LSP root marker: %w", err)
			}
			if samePath(candidate, root.Dir()) {
				break
			}
			parent := filepath.Dir(candidate)
			if parent == candidate {
				return "", fmt.Errorf("select LSP root: path escaped workspace")
			}
		}
	}
	return root.Dir(), nil
}

func validRootMarker(marker string) bool {
	return marker != "" && marker != "." && !filepath.IsAbs(marker) && filepath.Base(marker) == marker && !strings.ContainsAny(marker, `/\\`) && !strings.ContainsRune(marker, 0)
}

func samePath(left, right string) bool {
	if filepath.Clean(left) == filepath.Clean(right) {
		return true
	}
	leftInfo, leftErr := os.Stat(left)
	rightInfo, rightErr := os.Stat(right)
	return leftErr == nil && rightErr == nil && os.SameFile(leftInfo, rightInfo)
}
