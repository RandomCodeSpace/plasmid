package syntax_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/RandomCodeSpace/plasmid/internal/syntax"
)

const coverageSyntaxPath = "coverage.md"

func TestParseYAMLPublicScalarEdges(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		source string
		want   string
	}{
		{name: "doubled single quote", source: "value: 'it''s # literal' # comment", want: "it's # literal"},
		{name: "leading blank block line", source: "value: |\n\n  text\n", want: "\ntext\n"},
		{name: "empty block", source: "value: |", want: ""},
		{name: "escaped double quote in flow", source: `value: ["say \"hi\""]`, want: `say "hi"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value, err := syntax.ParseYAML(test.source)
			if err != nil {
				t.Fatal(err)
			}
			got := value.Mapping[0].Value
			if got.Kind == syntax.YAMLSequence {
				got = got.Sequence[0]
			}
			if got.Scalar != test.want {
				t.Fatalf("scalar = %q, want %q", got.Scalar, test.want)
			}
		})
	}
}

func TestParseYAMLPublicStructuralErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		source  string
		message string
	}{
		{name: "mixed mapping sequence", source: "root:\n  key: value\n  - item", message: "cannot mix sequence and mapping entries"},
		{name: "quoted key", source: "'bad:key': value", message: "unsupported mapping key"},
		{name: "escaped quoted key", source: `"bad\"key": value`, message: "unsupported mapping key"},
		{name: "unterminated single scalar", source: "value: 'broken", message: "unterminated single-quoted scalar"},
		{name: "excessive nesting", source: deeplyNestedYAML(34), message: "mapping nesting exceeds 32 levels"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := syntax.ParseYAML(test.source)
			var yamlErr *syntax.YAMLError
			if !errors.As(err, &yamlErr) || !strings.Contains(yamlErr.Message, test.message) {
				t.Fatalf("error = %v, want YAML error containing %q", err, test.message)
			}
		})
	}
}

func deeplyNestedYAML(levels int) string {
	var source strings.Builder
	for level := range levels {
		source.WriteString(strings.Repeat("  ", level))
		source.WriteString("level")
		source.WriteString(strings.Repeat("x", level))
		source.WriteString(":\n")
	}
	return source.String()
}

func TestPublicSyntaxEdgeBehavior(t *testing.T) {
	t.Parallel()

	arguments, err := syntax.ParseArguments("   ", nil)
	if err != nil || len(arguments.Positionals) != 0 {
		t.Fatalf("arguments = %#v, error = %v", arguments, err)
	}
	if _, err := syntax.ParseToolPattern("(argument)"); err == nil {
		t.Fatal("empty tool name was accepted")
	}
	output, warnings := syntax.Substitute("${}", coverageSyntaxPath, syntax.Substitutions{})
	if output != "${}" || len(warnings) != 0 {
		t.Fatalf("substitution = %q, warnings = %#v", output, warnings)
	}
}

func TestParseDocumentRejectsInvalidArgumentShapes(t *testing.T) {
	t.Parallel()

	for _, arguments := range []string{"arguments:", "arguments:\n  - [nested]"} {
		document, warnings := syntax.ParseDocument("---\nname: demo\ndescription: useful\n"+arguments+"\n---\nbody", coverageSyntaxPath, syntax.HostClaude)
		if len(document.Arguments) != 0 || len(warnings) != 1 {
			t.Fatalf("arguments %q: document = %#v, warnings = %#v", arguments, document, warnings)
		}
	}
}

func TestParseInstructionPublicMalformedEntries(t *testing.T) {
	t.Parallel()

	invalid, warnings := syntax.ParseInstruction("---\n  orphan\n---\nbody", coverageSyntaxPath, syntax.HostCodex)
	if invalid.Body != "body" || len(warnings) != 1 {
		t.Fatalf("instruction = %#v, warnings = %#v", invalid, warnings)
	}
	partial, warnings := syntax.ParseInstruction("---\nallowed-tools: Read\nbody", coverageSyntaxPath, syntax.HostCodex)
	if len(warnings) != 1 || !partial.Policy.Allows("Read", "") || partial.Policy.Allows("Write", "") {
		t.Fatalf("instruction = %#v, warnings = %#v", partial, warnings)
	}
}

func TestScanCodeRegionsPublicDelimiterEdges(t *testing.T) {
	t.Parallel()

	for _, source := range []string{"``not a fence", "```bad`tick"} {
		if regions := syntax.ScanCodeRegions(source); len(regions) != 0 {
			t.Fatalf("ScanCodeRegions(%q) = %#v", source, regions)
		}
	}
	regions := syntax.ScanCodeRegions("`a``b`")
	if len(regions) != 1 || regions[0].Kind != syntax.CodeRegionInline {
		t.Fatalf("inline regions = %#v", regions)
	}
}

func TestScopeStoreSetOrIntersectInitializesStorage(t *testing.T) {
	t.Parallel()

	var store syntax.ScopeStore
	key := syntax.ScopeKey{SessionID: "session", InvocationID: "invocation"}
	policy := syntax.NewRestrictedToolPolicy([]syntax.ToolPattern{{Tool: "Read"}}, nil)
	if err := store.SetOrIntersectPolicy(key, policy); err != nil {
		t.Fatal(err)
	}
	scope, ok := store.Get(key)
	if !ok || !scope.Policy.Allows("Read", "") || scope.Policy.Allows("Write", "") {
		t.Fatalf("scope = %#v, found = %v", scope, ok)
	}
}
