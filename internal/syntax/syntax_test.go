package syntax

import (
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/plasmid-dev/plasmid/warning"
)

func TestParseYAML(t *testing.T) {
	t.Parallel()
	value, err := ParseYAML("name: demo\nmetadata:\n  owner: 'a team'\ntools: [Read, \"Bash(git *)\"]\nnotes: |\n  first\n  second\n")
	if err != nil {
		t.Fatal(err)
	}
	if value.Kind != YAMLMapping || len(value.Mapping) != 4 {
		t.Fatalf("value = %#v", value)
	}
	if got := value.Mapping[1].Value.Mapping[0].Value.Scalar; got != "a team" {
		t.Fatalf("metadata owner = %q", got)
	}
	if got := value.Mapping[2].Value.Sequence[1].Scalar; got != "Bash(git *)" {
		t.Fatalf("tool = %q", got)
	}
	if got := value.Mapping[3].Value.Scalar; got != "first\nsecond\n" {
		t.Fatalf("notes = %q", got)
	}
}

func TestParseYAMLBlockScalarPreservesBlankAndCommentLines(t *testing.T) {
	t.Parallel()
	value, err := ParseYAML("description: |\n  first\n\n  # literal comment\n  second\nname: demo\n")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := value.Mapping[0].Value.Scalar, "first\n\n# literal comment\nsecond\n"; got != want {
		t.Fatalf("block scalar = %q, want %q", got, want)
	}
}

func TestParseYAMLRejectsUnsupportedSyntax(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		source string
		line   int
	}{
		{name: "tab", source: "name: ok\n\tbad: value", line: 2},
		{name: "root sequence", source: "- one", line: 1},
		{name: "alias", source: "name: *shared", line: 1},
		{name: "single quote interior", source: "name: 'secret'value'", line: 1},
		{name: "nested sequence", source: "items:\n  - one\n    - two", line: 3},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseYAML(test.source)
			var yamlErr *YAMLError
			if !errors.As(err, &yamlErr) || yamlErr.Line != test.line {
				t.Fatalf("error = %v, want YAML line %d", err, test.line)
			}
		})
	}
}

func TestParseDocumentWarnsPerEntry(t *testing.T) {
	t.Parallel()
	source := `---
name: Demo
description: first
description: second
model: opus
unknown: value
disable-model-invocation: maybe
allowed-tools: "Read Broken("
globs: "{a,b}"
---
body
`
	document, warnings := ParseDocument(source, "skills/demo/SKILL.md", HostClaude)
	if document.Description != "first" || document.Body != "body\n" {
		t.Fatalf("document = %#v", document)
	}
	wantCodes := []string{
		warning.WarnSyntaxDuplicateField,
		warning.WarnSyntaxUnsupportedField,
		warning.WarnSyntaxUnknownField,
		warning.WarnSyntaxInvalidField,
		warning.WarnSyntaxInvalidToolPattern,
		warning.WarnSyntaxInvalidGlob,
		warning.WarnSyntaxInvalidField,
		warning.WarnSyntaxDocumentNotInvocable,
	}
	gotCodes := make([]string, len(warnings))
	for index, item := range warnings {
		gotCodes[index] = item.Code
	}
	if !reflect.DeepEqual(gotCodes, wantCodes) {
		t.Fatalf("warning codes = %#v, want %#v", gotCodes, wantCodes)
	}
}

func TestParseDocumentMalformedFrontmatterDoesNotPanic(t *testing.T) {
	t.Parallel()
	document, warnings := ParseDocument("---\nname: [broken\n---\nbody", "broken.md", HostClaude)
	if document.Body != "body" || len(warnings) != 3 || warnings[0].Code != warning.WarnSyntaxInvalidField ||
		warnings[2].Code != warning.WarnSyntaxDocumentNotInvocable || document.Exposure != (Exposure{}) {
		t.Fatalf("ParseDocument() = %#v, %#v", document, warnings)
	}
}

func TestDescriptionValidationEmitsOneInvalidFieldWarning(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		description string
	}{
		{name: "invalid type", description: "description: [invalid]"},
		{name: "empty string", description: `description: ""`},
		{name: "missing"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			source := "---\nname: demo\n" + test.description + "\n---\nbody"
			_, warnings := ParseDocument(source, "fixture.md", HostPortable)
			invalidFields := 0
			for _, item := range warnings {
				if item.Code == warning.WarnSyntaxInvalidField {
					invalidFields++
				}
			}
			if invalidFields != 1 {
				t.Fatalf("invalid-field warnings = %d, all warnings = %#v", invalidFields, warnings)
			}
		})
	}
}

func TestPresentEmptyNameEmitsOneInvalidFieldWarning(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"name:", `name:""`} {
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			source := "---\n" + field + "\ndescription: safe\n---\nbody"
			_, warnings := ParseDocument(source, "fixture.md", HostPortable)
			if len(warnings) != 2 || warnings[0].Code != warning.WarnSyntaxInvalidField ||
				warnings[1].Code != warning.WarnSyntaxDocumentNotInvocable {
				t.Fatalf("warnings = %#v", warnings)
			}
		})
	}
}

func TestParseDocumentFailsSoftForUnknownAndUnsupportedSyntax(t *testing.T) {
	t.Parallel()
	source := `---
name: demo
description: safe
hooks:
  - type: command
    command: TOPSECRET
mystery: &TOPSECRET
allowed-tools: Read
---
body`
	document, warnings := ParseDocument(source, "fixture.md", HostClaude)
	if document.Name != "demo" || len(document.AllowedTools) != 1 || document.Exposure != DefaultExposure() {
		t.Fatalf("document = %#v", document)
	}
	if len(warnings) != 2 || warnings[0].Code != warning.WarnSyntaxUnsupportedField || warnings[1].Code != warning.WarnSyntaxUnknownField {
		t.Fatalf("warnings = %#v", warnings)
	}
	for _, item := range warnings {
		if strings.Contains(item.Message, "TOPSECRET") {
			t.Fatalf("warning leaked source data: %#v", item)
		}
	}
}

func TestParseDocumentRejectsMalformedBlockIndentation(t *testing.T) {
	t.Parallel()
	source := "---\nname: demo\ndescription: |\n    safe\n   malformed\nallowed-tools: Read\n---\nbody"
	document, warnings := ParseDocument(source, "fixture.md", HostClaude)
	if document.Exposure != (Exposure{}) || len(document.AllowedTools) != 1 {
		t.Fatalf("document = %#v", document)
	}
	if len(warnings) < 2 || warnings[0].Code != warning.WarnSyntaxInvalidField {
		t.Fatalf("warnings = %#v", warnings)
	}
}

func TestDocumentSequenceWarningsUseItemLines(t *testing.T) {
	t.Parallel()
	source := `---
name: demo
description: safe
arguments:
  - valid
  - bad-name
globs:
  - "*.go"
  - "{secret}"
allowed-tools:
  - Read
  - Broken(
---
body`
	_, warnings := ParseDocument(source, "fixture.md", HostClaude)
	want := []struct {
		code string
		line int
	}{
		{code: warning.WarnSyntaxInvalidField, line: 6},
		{code: warning.WarnSyntaxInvalidGlob, line: 9},
		{code: warning.WarnSyntaxInvalidToolPattern, line: 12},
	}
	if len(warnings) != len(want) {
		t.Fatalf("warnings = %#v", warnings)
	}
	for index, expected := range want {
		if warnings[index].Code != expected.code || warnings[index].Line != expected.line {
			t.Errorf("warning[%d] = %#v, want code %q line %d", index, warnings[index], expected.code, expected.line)
		}
	}
}

func TestScanCodeRegions(t *testing.T) {
	t.Parallel()
	source := "outside `inline`\n```go\n@inside\n```\nafter"
	regions := ScanCodeRegions(source)
	want := []CodeRegion{
		{Kind: CodeRegionInline, Start: 8, End: 16, ContentStart: 9, ContentEnd: 15},
		{Kind: CodeRegionFence, Start: 17, End: 35, ContentStart: 23, ContentEnd: 31},
	}
	if !reflect.DeepEqual(regions, want) {
		t.Fatalf("regions = %#v, want %#v", regions, want)
	}
	if !IsCodeOffset(regions, 10) || IsCodeOffset(regions, 16) || !IsCodeOffset(regions, 30) {
		t.Fatalf("code offset classification failed: %#v", regions)
	}
}

func TestArgumentsAndSubstitution(t *testing.T) {
	t.Parallel()
	arguments, err := ParseArguments(`first "two words" target=prod literal=$1`, []string{"target"})
	if err != nil {
		t.Fatal(err)
	}
	got, warnings := Substitute(
		"$1|$2|${target}|${SESSION_ID}|$ARGUMENTS|${missing}", "prompt.md",
		Substitutions{Arguments: arguments, Variables: Variables{SessionID: "$1"}},
	)
	want := `first|two words|prod|$1|first "two words" target=prod literal=$1|${missing}`
	if got != want || len(warnings) != 1 || warnings[0].Code != warning.WarnSyntaxUnresolvedVariable {
		t.Fatalf("Substitute() = %q, %#v", got, warnings)
	}
}

func TestToolPolicyDenyWinsAndIntersects(t *testing.T) {
	t.Parallel()
	outer := NewToolPolicy(
		[]ToolPattern{{Tool: "Read"}, {Tool: "Bash", Argument: "git *"}},
		[]ToolPattern{{Tool: "Read", Argument: "secret/*"}},
	)
	inner := NewToolPolicy([]ToolPattern{{Tool: "Read"}}, nil)
	policy := outer.Intersect(inner)
	tests := []struct {
		tool     string
		argument string
		want     bool
	}{
		{tool: "Read", argument: "docs/readme.md", want: true},
		{tool: "Read", argument: "secret/key"},
		{tool: "Bash", argument: "git status"},
		{tool: "Write", argument: "docs/readme.md"},
	}
	for _, test := range tests {
		if got := policy.Allows(test.tool, test.argument); got != test.want {
			t.Errorf("Allows(%q, %q) = %v, want %v", test.tool, test.argument, got, test.want)
		}
	}
}

func TestToolPatternClaudePrefix(t *testing.T) {
	t.Parallel()
	pattern, err := ParseToolPattern("Bash(git diff:*)")
	if err != nil {
		t.Fatal(err)
	}
	if !pattern.matches("Bash", "git diff --stat") || pattern.matches("Bash", "git status") {
		t.Fatalf("Claude prefix pattern matched incorrectly: %#v", pattern)
	}
}

func TestParseToolPatternsRetainsValidSiblings(t *testing.T) {
	t.Parallel()
	patterns, parseErrors := ParseToolPatterns("Read Broken( Write")
	want := []ToolPattern{{Tool: "Read"}}
	if !reflect.DeepEqual(patterns, want) || len(parseErrors) != 1 {
		t.Fatalf("ParseToolPatterns() = %#v, %v", patterns, parseErrors)
	}
}

func TestParseToolPatternsDoesNotRecoverInsideUnterminatedArgument(t *testing.T) {
	t.Parallel()
	patterns, parseErrors := ParseToolPatterns("Broken(Read Write")
	if len(patterns) != 0 || len(parseErrors) != 1 {
		t.Fatalf("ParseToolPatterns() = %#v, %v", patterns, parseErrors)
	}
}

func TestMalformedConfiguredAllowListFailsClosed(t *testing.T) {
	t.Parallel()
	malformed := "---\nname: demo\ndescription: safe\nallowed-tools: Broken(Read Write\n---\nbody"
	document, warnings := ParseDocument(malformed, "fixture.md", HostClaude)
	if len(warnings) != 1 || warnings[0].Code != warning.WarnSyntaxInvalidToolPattern {
		t.Fatalf("warnings = %#v", warnings)
	}
	if document.ToolPolicy().Allows("Read", "anything") {
		t.Fatal("entirely malformed configured allow-list failed open")
	}

	absent := "---\nname: demo\ndescription: safe\n---\nbody"
	document, warnings = ParseDocument(absent, "fixture.md", HostClaude)
	if len(warnings) != 0 || !document.ToolPolicy().Allows("Read", "anything") {
		t.Fatalf("absent allow-list became restrictive: %#v, %#v", document, warnings)
	}

	empty := "---\nname: demo\ndescription: safe\nallowed-tools: []\n---\nbody"
	document, warnings = ParseDocument(empty, "fixture.md", HostClaude)
	nested := document.ToolPolicy().Intersect(NewToolPolicy([]ToolPattern{{Tool: "Read"}}, nil))
	if len(warnings) != 0 || document.ToolPolicy().Allows("Read", "anything") || nested.Allows("Read", "anything") {
		t.Fatalf("configured empty allow-list failed open: %#v, %#v", document, warnings)
	}
}

func TestConfiguredAllowListRetainsOnlyValidPrefix(t *testing.T) {
	t.Parallel()
	source := "---\nname: demo\ndescription: safe\nallowed-tools: Read Broken(Edit Write\n---\nbody"
	document, warnings := ParseDocument(source, "fixture.md", HostClaude)
	if len(warnings) != 1 || !document.ToolPolicy().Allows("Read", "file") ||
		document.ToolPolicy().Allows("Write", "file") || document.ToolPolicy().Allows("Edit", "file") {
		t.Fatalf("policy = %#v, warnings = %#v", document.AllowedTools, warnings)
	}
}

func TestWarningsDoNotIncludeSubstitutionTokens(t *testing.T) {
	t.Parallel()
	_, warnings := Substitute("${TOPSECRET}\n$2", "fixture.md", Substitutions{})
	if len(warnings) != 2 || warnings[0].Line != 1 || warnings[1].Line != 2 {
		t.Fatalf("warnings = %#v", warnings)
	}
	for _, item := range warnings {
		if strings.Contains(item.Message, "TOPSECRET") || strings.Contains(item.Message, "$2") {
			t.Fatalf("warning leaked substitution token: %#v", item)
		}
	}
}

func TestExposureDoesNotGrantTrust(t *testing.T) {
	t.Parallel()
	exposure := DefaultExposure()
	if exposure.Allows(InvocationModel, true, false) {
		t.Fatal("untrusted repository document became model-invocable")
	}
	if !exposure.Allows(InvocationModel, true, true) || !exposure.Allows(InvocationUser, true, false) {
		t.Fatal("valid exposure was rejected")
	}
}

func TestScopeStoreConcurrent(t *testing.T) {
	t.Parallel()
	var store ScopeStore
	const count = 128
	var group sync.WaitGroup
	for index := range count {
		group.Add(1)
		go func() {
			defer group.Done()
			key := ScopeKey{SessionID: "session", InvocationID: string(rune(index + 1))}
			if err := store.Begin(key, TurnScope{Arguments: Arguments{Positionals: []string{"original"}}}); err != nil {
				t.Errorf("Begin() error = %v", err)
				return
			}
			scope, ok := store.Get(key)
			if !ok {
				t.Error("Get() missed active scope")
				return
			}
			scope.Arguments.Positionals[0] = "mutated"
			if !store.Release(key) {
				t.Error("Release() missed active scope")
			}
		}()
	}
	group.Wait()
	if got := store.Len(); got != 0 {
		t.Fatalf("Len() = %d", got)
	}
}
