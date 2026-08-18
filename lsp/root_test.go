package lsp

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSelectWorkspaceRootNearestMarker(t *testing.T) {
	workspaceDir := t.TempDir()
	project := filepath.Join(workspaceDir, "project")
	packageDir := filepath.Join(project, "pkg")
	if err := os.MkdirAll(packageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "go.mod"), []byte("module example\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(packageDir, "main.go")
	if err := os.WriteFile(file, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := SelectWorkspaceRoot(workspaceDir, file, []string{"go.work", "go.mod"})
	if err != nil || got != project {
		t.Fatalf("SelectWorkspaceRoot = %q, %v; want %q", got, err, project)
	}
}

func TestSelectWorkspaceRootFallsBackAndConfines(t *testing.T) {
	workspaceDir := t.TempDir()
	file := filepath.Join(workspaceDir, "main.go")
	if err := os.WriteFile(file, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := SelectWorkspaceRoot(workspaceDir, file, []string{"go.mod"})
	if err != nil || got != workspaceDir {
		t.Fatalf("fallback = %q, %v", got, err)
	}
	outside := filepath.Join(t.TempDir(), "outside.go")
	if err := os.WriteFile(outside, []byte("package outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := SelectWorkspaceRoot(workspaceDir, outside, []string{"go.mod"}); err == nil {
		t.Fatal("outside path accepted")
	}
	if _, err := SelectWorkspaceRoot(workspaceDir, file, []string{"../go.mod"}); err == nil {
		t.Fatal("invalid marker accepted")
	}
	if _, err := SelectWorkspaceRoot(workspaceDir, file, []string{`dir\marker`}); err == nil {
		t.Fatal("non-portable marker accepted")
	}
}

func TestSelectWorkspaceRootHonorsMarkerPrecedenceBeforeProximity(t *testing.T) {
	workspaceDir := t.TempDir()
	moduleDir := filepath.Join(workspaceDir, "module")
	packageDir := filepath.Join(moduleDir, "pkg")
	if err := os.MkdirAll(packageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspaceDir, "go.work"), []byte("go 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(moduleDir, "go.mod"), []byte("module example\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(packageDir, "main.go")
	if err := os.WriteFile(file, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		markers []string
		want    string
	}{
		{name: "go work first", markers: []string{"go.work", "go.mod"}, want: workspaceDir},
		{name: "go mod first", markers: []string{"go.mod", "go.work"}, want: moduleDir},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := SelectWorkspaceRoot(workspaceDir, file, test.markers)
			if err != nil || got != test.want {
				t.Fatalf("SelectWorkspaceRoot = %q, %v; want %q", got, err, test.want)
			}
		})
	}
}
