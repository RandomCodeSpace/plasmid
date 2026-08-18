package syntax_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/plasmid-dev/plasmid/internal/syntax"
	"github.com/plasmid-dev/plasmid/warning"
)

func TestParseTemplateWithoutFrontmatterRetainsHost(t *testing.T) {
	t.Parallel()

	document, warnings := syntax.ParseTemplate("plain body", "prompt.md", syntax.HostCodex, "prompt")
	if len(warnings) != 0 {
		t.Fatalf("warnings = %#v", warnings)
	}
	if document.Host != syntax.HostCodex {
		t.Fatalf("Host = %q, want %q", document.Host, syntax.HostCodex)
	}
}

func TestSupportMatrixIsHostSpecificAndDefensive(t *testing.T) {
	t.Parallel()

	portable := syntax.SupportMatrix(syntax.HostPortable)
	claude := syntax.SupportMatrix(syntax.HostClaude)
	plasmid := syntax.SupportMatrix(syntax.HostPlasmid)
	if len(portable) != 6 || len(claude) != 18 || !reflect.DeepEqual(claude, plasmid) {
		t.Fatalf("matrix lengths = portable %d, claude %d, plasmid %d", len(portable), len(claude), len(plasmid))
	}
	claude[0].Name = "mutated"
	if syntax.SupportMatrix(syntax.HostClaude)[0].Name != "name" {
		t.Fatal("SupportMatrix returned shared storage")
	}
}

func TestParseYAMLPublicShapes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		source string
		kind   syntax.YAMLKind
		scalar string
	}{
		{name: "empty", source: "\n# comment\n", kind: syntax.YAMLMapping},
		{name: "folded", source: "value: >\n  one\n  two\n\n  three\n", kind: syntax.YAMLScalar, scalar: "one two\n\nthree\n"},
		{name: "flow", source: "value: ['one', \"two\", three]", kind: syntax.YAMLSequence},
		{name: "nested sequence mapping", source: "value:\n  - name: one\n  - name: two", kind: syntax.YAMLSequence},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) { assertYAMLShape(t, test.name, test.source, test.kind, test.scalar) })
	}
}

func assertYAMLShape(t *testing.T, name, source string, kind syntax.YAMLKind, scalar string) {
	value, err := syntax.ParseYAML(source)
	if err != nil {
		t.Fatal(err)
	}
	if name == "empty" {
		if value.Kind != syntax.YAMLMapping || len(value.Mapping) != 0 {
			t.Fatalf("value = %#v", value)
		}
		return
	}
	got := value.Mapping[0].Value
	if got.Kind != kind || got.Scalar != scalar {
		t.Fatalf("value = %#v", got)
	}
}

func TestParseYAMLPublicErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		source string
		line   int
	}{
		{name: "indented root", source: "  name: value", line: 1},
		{name: "unexpected indentation", source: "name: value\n  stray: value", line: 2},
		{name: "empty key", source: ": value", line: 1},
		{name: "complex key", source: "? key: value", line: 1},
		{name: "tag", source: "name: !tag value", line: 1},
		{name: "flow mapping", source: "name: {key: value}", line: 1},
		{name: "unterminated flow", source: "name: [one, two", line: 1},
		{name: "double quote", source: `name: "unterminated`, line: 1},
		{name: "invalid escape", source: `name: "bad\q"`, line: 1},
		{name: "sequence indentation", source: "name:\n  - one\n   - two", line: 3},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := syntax.ParseYAML(test.source)
			var yamlErr *syntax.YAMLError
			if !errors.As(err, &yamlErr) || yamlErr.Line != test.line || yamlErr.Error() == "" {
				t.Fatalf("error = %v, want YAML line %d", err, test.line)
			}
		})
	}
}

func TestParseDocumentPublicProjection(t *testing.T) {
	t.Parallel()

	source := `---
name: demo
description: useful
license: MIT
compatibility: go
metadata:
  owner: team
  owner: duplicate
  nested: [invalid]
arguments: [target, target, bad-name]
globs: ["**/*.go", "{bad}"]
argument-hint: "[target]"
allowed-tools: [Read, "Bash(go test *)"]
disallowed-tools: Write
disable-model-invocation: true
user-invocable: false
---
body`
	document, warnings := syntax.ParseDocument(source, "SKILL.md", syntax.HostClaude)
	if document.Name != "demo" || document.License != "MIT" || document.Compatibility != "go" || document.ArgumentHint != "[target]" || document.Body != "body" {
		t.Fatalf("document = %#v", document)
	}
	if !document.RestrictsTools() || document.Exposure != (syntax.Exposure{}) || len(document.Arguments) != 1 || len(document.Globs) != 1 || len(document.Metadata) != 1 {
		t.Fatalf("projected document = %#v", document)
	}
	if len(warnings) != 5 || warnings[0].Code != warning.WarnSyntaxDuplicateField || warnings[4].Code != warning.WarnSyntaxInvalidGlob {
		t.Fatalf("warnings = %#v", warnings)
	}
	policy := document.ToolPolicy()
	if !policy.Allows("read", "x") || policy.Allows("write", "x") || policy.Allows("bash", "go vet ./...") {
		t.Fatalf("policy = %#v / %#v", policy.Allowed(), policy.Denied())
	}
}

func TestParseDocumentPublicFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		source string
		host   syntax.Host
		code   string
	}{
		{name: "invalid host", source: "---\nname: demo\ndescription: useful\n---\n", host: syntax.Host("invalid"), code: warning.WarnSyntaxInvalidFrontmatter},
		{name: "missing delimiter", source: "name: demo", host: syntax.HostPortable, code: warning.WarnSyntaxInvalidFrontmatter},
		{name: "missing required", source: "---\nlicense: MIT\n---\n", host: syntax.HostPortable, code: warning.WarnSyntaxInvalidField},
		{name: "invalid boolean type", source: "---\nname: demo\ndescription: useful\ndisable-model-invocation: [true]\n---\n", host: syntax.HostClaude, code: warning.WarnSyntaxInvalidField},
		{name: "invalid metadata type", source: "---\nname: demo\ndescription: useful\nmetadata: value\n---\n", host: syntax.HostPortable, code: warning.WarnSyntaxInvalidField},
		{name: "invalid arguments type", source: "---\nname: demo\ndescription: useful\narguments:\n  key: value\n---\n", host: syntax.HostClaude, code: warning.WarnSyntaxInvalidField},
		{name: "invalid tools type", source: "---\nname: demo\ndescription: useful\nallowed-tools:\n  key: value\n---\n", host: syntax.HostClaude, code: warning.WarnSyntaxInvalidField},
		{name: "malformed tools", source: "---\nname: demo\ndescription: useful\nallowed-tools: [Read\n---\n", host: syntax.HostClaude, code: warning.WarnSyntaxInvalidField},
		{name: "indented orphan", source: "---\n  invalid\nname: demo\ndescription: useful\n---\n", host: syntax.HostClaude, code: warning.WarnSyntaxInvalidFrontmatter},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			document, warnings := syntax.ParseDocument(test.source, "SKILL.md", test.host)
			if len(warnings) == 0 || warnings[0].Code != test.code {
				t.Fatalf("document = %#v, warnings = %#v", document, warnings)
			}
		})
	}
}

func TestParseTemplatePublicVariants(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		source   string
		identity string
		invoked  bool
	}{
		{name: "empty identity", source: "body"},
		{name: "bare", source: "body", identity: "prompt", invoked: true},
		{name: "empty frontmatter", source: "---\n---\nbody", identity: "prompt", invoked: true},
		{name: "ordinary delimiter text", source: "body\n---\ntext", identity: "prompt", invoked: true},
		{name: "frontmatter injects identity", source: "---\nallowed-tools: Read\n---\nbody", identity: "prompt", invoked: true},
		{name: "frontmatter keeps fields", source: "---\nname: ignored\ndescription: useful\n---\nbody", identity: "prompt", invoked: true},
		{name: "unterminated bare delimiter", source: "---", identity: "prompt"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			document, warnings := syntax.ParseTemplate(test.source, "prompt.md", syntax.HostClaude, test.identity)
			if document.Exposure.Allows(syntax.InvocationUser, false, true) != test.invoked {
				t.Fatalf("document = %#v, warnings = %#v", document, warnings)
			}
			if test.identity != "" && document.Name != test.identity {
				t.Fatalf("Name = %q, want %q", document.Name, test.identity)
			}
		})
	}
}

func TestParseArgumentsPublicVariants(t *testing.T) {
	t.Parallel()

	arguments, err := syntax.ParseArguments(`plain '' "two words" escaped\ value target=prod other=value`, []string{"target"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"plain", "", "two words", "escaped value", "other=value"}
	if !reflect.DeepEqual(arguments.Positionals, want) || !reflect.DeepEqual(arguments.Named, []syntax.NamedArgument{{Name: "target", Value: "prod"}}) {
		t.Fatalf("arguments = %#v", arguments)
	}
	for _, test := range []struct {
		name     string
		source   string
		declared []string
	}{
		{name: "invalid declared", declared: []string{"bad-name"}},
		{name: "duplicate declared", declared: []string{"name", "name"}},
		{name: "duplicate named", source: "name=a name=b", declared: []string{"name"}},
		{name: "trailing escape", source: `value\`},
		{name: "unterminated single", source: `'value`},
		{name: "unterminated double", source: `"value`},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := syntax.ParseArguments(test.source, test.declared); err == nil {
				t.Fatal("ParseArguments returned nil error")
			}
		})
	}
}

func TestSubstitutePublicVariants(t *testing.T) {
	t.Parallel()

	arguments, err := syntax.ParseArguments("one declared=", []string{"declared"})
	if err != nil {
		t.Fatal(err)
	}
	values := syntax.Substitutions{
		Arguments: arguments,
		Variables: syntax.Variables{SessionID: "session", SkillDir: "skill", ProjectDir: "project", PluginRoot: "plugin", PluginData: "data", Effort: "high"},
	}
	source := "$0|$1|$99|${declared}|${SESSION_ID}|${CLAUDE_SESSION_ID}|${SKILL_DIR}|${PROJECT_DIR}|${CLAUDE_PROJECT_DIR}|${PLUGIN_ROOT}|${CLAUDE_PLUGIN_ROOT}|${PLUGIN_DATA}|${CLAUDE_PLUGIN_DATA}|${EFFORT}|${CLAUDE_CODE_EFFORT_LEVEL}|$ARGUMENTSX|${bad-name}|$"
	output, warnings := syntax.Substitute(source, "prompt.md", values)
	if !strings.Contains(output, "$0|one|$99||session|session|skill|project|project|plugin|plugin|data|data|high|high|$ARGUMENTSX|${bad-name}|$") {
		t.Fatalf("output = %q", output)
	}
	if len(warnings) != 2 || warnings[0].Code != warning.WarnSyntaxMissingArgument || warnings[1].Code != warning.WarnSyntaxMissingArgument {
		t.Fatalf("warnings = %#v", warnings)
	}
	for _, test := range []struct {
		name    string
		source  string
		maximum int
	}{
		{name: "literal", source: "long", maximum: 3},
		{name: "dollar", source: "$", maximum: 0},
		{name: "replacement", source: "$ARGUMENTS", maximum: 2},
		{name: "unresolved token", source: "${UNKNOWN}", maximum: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			maximum := test.maximum
			if maximum == 0 {
				maximum = -1
			}
			_, _, err := syntax.SubstituteBounded(test.source, "prompt.md", values, maximum)
			if test.name != "dollar" && !errors.Is(err, syntax.ErrSubstitutionLimit) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestToolPolicyPublicVariants(t *testing.T) {
	t.Parallel()

	for _, source := range []string{"", "Read()", "Read)", "Bad Name", "Read(a(b))", "Read(line\nbreak)"} {
		if _, err := syntax.ParseToolPattern(source); err == nil {
			t.Errorf("ParseToolPattern(%q) returned nil error", source)
		}
	}
	patterns, parseErrors := syntax.ParseToolPatterns(" \t\n")
	if len(patterns) != 0 || len(parseErrors) != 1 {
		t.Fatalf("ParseToolPatterns = %#v, %#v", patterns, parseErrors)
	}
	policy := syntax.NewRestrictedToolPolicy(nil, []syntax.ToolPattern{{Tool: "Write"}})
	if policy.Allows("Read", "x") || policy.Visible("Read") || len(policy.Allowed()) != 0 || len(policy.Denied()) != 1 {
		t.Fatalf("restricted policy = %#v / %#v", policy.Allowed(), policy.Denied())
	}
	var zero syntax.ToolPolicy
	if !zero.Allows("Read", "x") || len(zero.Allowed()) != 0 || len(zero.Denied()) != 0 {
		t.Fatalf("zero policy = %#v", zero)
	}
	wildcard := syntax.NewToolPolicy([]syntax.ToolPattern{{Tool: "*"}}, []syntax.ToolPattern{{Tool: "Read", Argument: "secret/*"}})
	if !wildcard.Visible("Read") || wildcard.Allows("Read", "secret/key") || !wildcard.Allows("Read", "public/key") {
		t.Fatal("wildcard or argument-specific deny behavior is wrong")
	}
}

func TestScopeStorePublicLifecycle(t *testing.T) {
	t.Parallel()

	var store syntax.ScopeStore
	key := syntax.ScopeKey{SessionID: "session", InvocationID: "one"}
	other := syntax.ScopeKey{SessionID: "session", InvocationID: "two"}
	readOnly := syntax.NewRestrictedToolPolicy([]syntax.ToolPattern{{Tool: "Read"}}, nil)
	assertInvalidScopeKeys(t, &store, readOnly)
	if store.Set(key, syntax.TurnScope{Policy: syntax.NewToolPolicy(nil, nil), Arguments: syntax.Arguments{Declared: []string{"name"}, Positionals: []string{"one"}, Named: []syntax.NamedArgument{{Name: "name", Value: "value"}}}}) != nil {
		t.Fatal("Set failed")
	}
	if store.Begin(key, syntax.TurnScope{}) == nil || store.IntersectPolicy(other, readOnly) == nil {
		t.Fatal("duplicate or missing scope was accepted")
	}
	if store.IntersectPolicy(key, readOnly) != nil || store.SetOrIntersectPolicy(key, syntax.NewToolPolicy(nil, []syntax.ToolPattern{{Tool: "Write"}})) != nil {
		t.Fatal("policy narrowing failed")
	}
	if store.SetOrIntersectPolicy(other, readOnly) != nil {
		t.Fatal("scope creation failed")
	}
	assertStoredScope(t, &store, key)
	assertScopeRelease(t, &store, key)
}

func assertInvalidScopeKeys(t *testing.T, store *syntax.ScopeStore, policy syntax.ToolPolicy) {
	invalid := syntax.ScopeKey{}
	if store.Set(invalid, syntax.TurnScope{}) == nil || store.Begin(invalid, syntax.TurnScope{}) == nil || store.SetOrIntersectPolicy(invalid, policy) == nil {
		t.Fatal("invalid keys were accepted")
	}
}

func assertStoredScope(t *testing.T, store *syntax.ScopeStore, key syntax.ScopeKey) {
	scope, ok := store.Get(key)
	if !ok || !scope.Policy.Allows("Read", "x") || scope.Policy.Allows("Write", "x") {
		t.Fatalf("scope = %#v, found = %v", scope, ok)
	}
	scope.Arguments.Declared[0] = "mutated"
	stored, _ := store.Get(key)
	if stored.Arguments.Declared[0] != "name" || store.Len() != 2 {
		t.Fatal("stored scope was not defensive")
	}
}

func assertScopeRelease(t *testing.T, store *syntax.ScopeStore, key syntax.ScopeKey) {
	if removed := store.ReleaseSession("missing"); removed != 0 {
		t.Fatalf("removed = %d", removed)
	}
	if removed := store.ReleaseSession("session"); removed != 2 || store.Len() != 0 || store.Release(key) {
		t.Fatalf("removed = %d, len = %d", removed, store.Len())
	}
	if _, ok := store.Get(key); ok {
		t.Fatal("released scope remained visible")
	}
}

func TestExposurePublicVariants(t *testing.T) {
	t.Parallel()

	exposure := syntax.Exposure{UserInvocable: false, ModelInvocable: true}
	if exposure.Allows(syntax.InvocationUser, false, true) || !exposure.Allows(syntax.InvocationModel, false, false) || exposure.Allows(syntax.InvocationKind(99), false, true) {
		t.Fatal("exposure classification is wrong")
	}
}

func TestInstructionPublicVariants(t *testing.T) {
	t.Parallel()

	plain, warnings := syntax.ParseInstruction("plain", "AGENTS.md", syntax.HostCodex)
	if plain.Body != "plain" || len(warnings) != 0 || !plain.Policy.Allows("Write", "x") {
		t.Fatalf("plain = %#v, warnings = %#v", plain, warnings)
	}
	malformed, warnings := syntax.ParseInstruction("---\r\npaths: [\"**/*.go\"]\r\nallowed-tools: [Read\r\nbody", "AGENTS.md", syntax.HostCodex)
	if !malformed.PathScopeDeclared || malformed.Policy.Allows("Write", "x") || len(warnings) != 1 {
		t.Fatalf("malformed = %#v, warnings = %#v", malformed, warnings)
	}
	source := "---\npaths:\n  key: value\nallowed-tools: [Read]\nallowed-tools: Write\nunknown: value\n---\nbody"
	parsed, warnings := syntax.ParseInstruction(source, "AGENTS.md", syntax.HostCodex)
	if !parsed.PathScopeDeclared || len(warnings) != 3 {
		t.Fatalf("parsed = %#v, warnings = %#v", parsed, warnings)
	}
}

func TestCommandAndMarkdownPublicVariants(t *testing.T) {
	t.Parallel()

	source := "\\!`escaped` !``two`` `open\n  ~~~!\nrun\n  ~~~\n    ```!\nno\n    ```\n"
	directives := syntax.ScanCommandDirectives(source)
	if len(directives) != 2 || directives[0].Command != "two" || directives[1].Command != "run\n" {
		t.Fatalf("directives = %#v", directives)
	}
	regions := syntax.ScanCodeRegions("\\`escaped\\` and `open")
	if len(regions) != 0 || syntax.IsCodeOffset(regions, -1) {
		t.Fatalf("regions = %#v", regions)
	}
}

func TestNativeToolInvocationMarshalFailure(t *testing.T) {
	t.Parallel()

	name, argument := syntax.NativeToolInvocation("Custom", map[string]any{"invalid": func() {}})
	if name != "Custom" || argument != "{}" {
		t.Fatalf("NativeToolInvocation = %q, %q", name, argument)
	}
}

func TestParseYAMLSequenceFailures(t *testing.T) {
	t.Parallel()

	for _, source := range []string{
		"name:\n  - one\n  key: value",
		"name:\n  -",
		"name: [one,,two]",
		"name: [one,]",
		`name: ["unterminated]`,
		"name: ['single]",
		"name: [bad{]",
	} {
		if _, err := syntax.ParseYAML(source); err == nil {
			t.Errorf("ParseYAML(%q) returned nil error", source)
		}
	}
}
