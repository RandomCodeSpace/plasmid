package syntax

import (
	"reflect"
	"testing"

	"github.com/RandomCodeSpace/plasmid/warning"
)

func TestParseInstructionFrontmatter(t *testing.T) {
	t.Parallel()
	source := `---
applyTo: "**/*.go, cmd/**"
allowed-tools: [Read, "Bash(git *)"]
disallowed-tools: Read(secret/*)
---
Follow the local rules.
`
	got, warnings := ParseInstruction(source, "rules.instructions.md", HostCopilot)
	if len(warnings) != 0 {
		t.Fatalf("warnings = %#v", warnings)
	}
	if got.Body != "Follow the local rules.\n" || !got.PathScopeDeclared || !reflect.DeepEqual(got.Globs, []string{"**/*.go", "cmd/**"}) {
		t.Fatalf("instruction = %#v", got)
	}
	if !got.Policy.Allows("read", "main.go") || got.Policy.Allows("read", "secret/key") || got.Policy.Allows("write", "main.go") {
		t.Fatalf("policy did not map host tool names to native names")
	}
}

func TestParseInstructionWarnsPerInvalidEntry(t *testing.T) {
	t.Parallel()
	source := "---\npaths: [\"**/*.go\", \"{bad}\"]\nunknown: value\n---\nbody"
	got, warnings := ParseInstruction(source, "rule.md", HostClaude)
	if got.Body != "body" || !reflect.DeepEqual(got.Globs, []string{"**/*.go"}) {
		t.Fatalf("instruction = %#v", got)
	}
	want := []string{warning.WarnContextGlobInvalid, warning.WarnContextFrontmatterUnsupported}
	if len(warnings) != len(want) {
		t.Fatalf("warnings = %#v", warnings)
	}
	for index, code := range want {
		if warnings[index].Code != code {
			t.Fatalf("warning[%d] = %#v, want %q", index, warnings[index], code)
		}
	}
}

func TestParseInstructionMalformedAllowPolicyFailsClosed(t *testing.T) {
	t.Parallel()
	for _, source := range []string{
		"---\nallowed-tools: [Read\n---\nbody",
		"---\nallowed-tools: [Read\nbody",
	} {
		instruction, warnings := ParseInstruction(source, "AGENTS.md", HostCodex)
		if instruction.Policy.Allows("read", "x") || instruction.Policy.Allows("write", "x") {
			t.Fatalf("malformed allow policy was permissive for %q", source)
		}
		if len(warnings) == 0 {
			t.Fatalf("warnings = %#v", warnings)
		}
	}
}

func TestScanCommandDirectivesSkipsOrdinaryCode(t *testing.T) {
	t.Parallel()
	source := "before !`printf inline` and `literal`\n```!\nprintf fenced\n```\n```sh\nprintf ordinary\n```\n"
	got := ScanCommandDirectives(source)
	want := []CommandDirective{
		{Start: 7, End: 23, ContentStart: 9, ContentEnd: 22, Line: 1, Command: "printf inline"},
		{Start: 38, End: 61, ContentStart: 43, ContentEnd: 57, Line: 2, Command: "printf fenced\n"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("directives = %#v, want %#v", got, want)
	}
}

func TestNativeToolInvocationCompatibility(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		args         map[string]any
		wantName     string
		wantArgument string
	}{
		{name: "Read", args: map[string]any{"path": "README.md"}, wantName: "read", wantArgument: "README.md"},
		{name: "Write", args: map[string]any{"path": "out.txt"}, wantName: "write", wantArgument: "out.txt"},
		{name: "Edit", args: map[string]any{"path": "edit.txt"}, wantName: "edit", wantArgument: "edit.txt"},
		{name: "Bash", args: map[string]any{"command": "go test ./..."}, wantName: "bash", wantArgument: "go test ./..."},
		{name: "Grep", args: map[string]any{"pattern": "needle"}, wantName: "grep", wantArgument: "needle"},
		{name: "Glob", args: map[string]any{"glob": "**/*.go"}, wantName: "find", wantArgument: "**/*.go"},
		{name: "Find", args: map[string]any{"glob": "*.md"}, wantName: "find", wantArgument: "*.md"},
		{name: "LS", args: map[string]any{"path": "."}, wantName: "ls", wantArgument: "."},
		{name: "List", args: map[string]any{"path": "pkg"}, wantName: "ls", wantArgument: "pkg"},
		{name: "custom", args: map[string]any{"z": 2, "a": 1}, wantName: "custom", wantArgument: `{"a":1,"z":2}`},
	}
	for _, test := range tests {
		name, argument := NativeToolInvocation(test.name, test.args)
		if name != test.wantName || argument != test.wantArgument {
			t.Errorf("NativeToolInvocation(%q) = %q, %q, want %q, %q", test.name, name, argument, test.wantName, test.wantArgument)
		}
	}
}

func TestToolPolicyVisibilityUsesNameLevelIntersection(t *testing.T) {
	t.Parallel()
	outer := NewToolPolicy(
		[]ToolPattern{{Tool: "Read"}, {Tool: "Bash", Argument: "git *"}},
		[]ToolPattern{{Tool: "Read", Argument: "secret/*"}},
	)
	inner := NewToolPolicy([]ToolPattern{{Tool: "Read"}}, nil)
	policy := outer.Intersect(inner)
	if !policy.Visible("read") || policy.Visible("bash") || policy.Visible("write") {
		t.Fatalf("unexpected name visibility")
	}
	if NewToolPolicy(nil, []ToolPattern{{Tool: "Read"}}).Visible("read") {
		t.Fatal("name-wide deny remained visible")
	}
}
