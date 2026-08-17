package lsp

import (
	"os"
	"path/filepath"
	"testing"

	"go.lsp.dev/protocol"
)

func TestNormalizeDiagnostics(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "pkg", "main.go")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	uri, err := PathToFileURI(file)
	if err != nil {
		t.Fatal(err)
	}
	source := protocol.NewOptional("gopls")
	values := []protocol.Diagnostic{
		{Range: protocol.Range{Start: protocol.Position{Line: 2}, End: protocol.Position{Line: 2, Character: 1}}, Severity: protocol.DiagnosticSeverityWarning, Code: protocol.String("B"), Source: source, Message: protocol.String("second\r\nline")},
		{Range: protocol.Range{Start: protocol.Position{Line: 1}, End: protocol.Position{Line: 1, Character: 1}}, Severity: protocol.DiagnosticSeverityError, Code: protocol.Integer(7), Source: source, Message: protocol.String("first")},
		{Range: protocol.Range{Start: protocol.Position{Line: 1}, End: protocol.Position{Line: 1, Character: 1}}, Severity: protocol.DiagnosticSeverityError, Code: protocol.Integer(7), Source: source, Message: protocol.String("first")},
		{Range: protocol.Range{Start: protocol.Position{Line: 3}, End: protocol.Position{Line: 2}}, Message: protocol.String("invalid")},
	}
	got, err := NormalizeDiagnostics(root, uri, values, 2)
	if err != nil || len(got) != 2 {
		t.Fatalf("NormalizeDiagnostics = %#v, %v", got, err)
	}
	if got[0].Path != "pkg/main.go" || got[0].Code != "7" || got[1].Message != "second\nline" {
		t.Fatalf("normalized = %#v", got)
	}
}

func TestNormalizeDiagnosticsRejectsEscape(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.go")
	if err := os.WriteFile(outside, []byte("package outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	uri, _ := PathToFileURI(outside)
	if _, err := NormalizeDiagnostics(root, uri, nil, 1); err == nil {
		t.Fatal("outside diagnostic URI accepted")
	}
}
