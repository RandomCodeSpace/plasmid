package loop

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestNoADKInCore(t *testing.T) {
	moduleRoot := filepath.Clean("..")
	allowed := map[string]bool{".": true, "adkloop": true}
	err := filepath.WalkDir(moduleRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		relative, err := filepath.Rel(moduleRoot, filepath.Dir(path))
		if err != nil || allowed[relative] {
			return err
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, spec := range file.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			if strings.HasPrefix(importPath, "google.golang.org/adk") || strings.HasPrefix(importPath, "google.golang.org/genai") {
				t.Errorf("%s imports forbidden framework package %s", path, importPath)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
