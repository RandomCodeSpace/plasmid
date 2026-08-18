package codingtools

import "testing"

func TestToolSpecifier(t *testing.T) {
	for _, test := range []struct {
		name     string
		toolName string
		input    map[string]any
		want     string
	}{
		{"bash", "bash", map[string]any{"command": "go test ./..."}, "go test ./..."},
		{"read", "read", map[string]any{"path": "a.go"}, "a.go"},
		{"write", "write", map[string]any{"path": "a.go"}, "a.go"},
		{"edit", "edit", map[string]any{"path": "a.go"}, "a.go"},
		{"ls", "ls", map[string]any{"path": "."}, "."},
		{"find", "find", map[string]any{"path": "dir"}, "dir"},
		{"grep", "grep", map[string]any{"pattern": "needle"}, "needle"},
		{"unknown", "unknown", map[string]any{"path": "a.go"}, ""},
		{"wrong type", "read", map[string]any{"path": 1}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := ToolSpecifier(test.toolName, test.input); got != test.want {
				t.Fatalf("ToolSpecifier() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestToolSpecifierFunc(t *testing.T) {
	var nilFunc ToolSpecifierFunc
	if nilFunc != nil {
		t.Fatal("zero function is non-nil")
	}
	funcValue := ToolSpecifierFunc(func(name string, _ map[string]any) string { return name })
	if got := funcValue("read", nil); got != "read" {
		t.Fatalf("specifier function = %q", got)
	}
}
