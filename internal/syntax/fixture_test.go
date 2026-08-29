package syntax

import (
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/RandomCodeSpace/plasmid/internal/fixture"
)

func init() {
	fixture.RegisterRunner("syntax", "syntax/all", "arguments", "command-directives", "exposure", "frontmatter", "instruction", "markdown", "matrix", "native-tool", "scope", "substitution", "tool-policy", "yaml")
}

func TestMain(m *testing.M) {
	os.Exit(fixture.Run(m))
}

type syntaxFixtureInput struct {
	Allowed          string         `json:"allowed"`
	Arguments        string         `json:"arguments"`
	Calls            []toolCall     `json:"calls"`
	Concurrent       int            `json:"concurrent"`
	Declared         []string       `json:"declared"`
	Denied           string         `json:"denied"`
	Exposure         Exposure       `json:"exposure"`
	Host             Host           `json:"host"`
	Hosts            []Host         `json:"hosts"`
	NestedAllowed    string         `json:"nested_allowed"`
	RepositoryScoped bool           `json:"repository_scoped"`
	Requests         []exposureCall `json:"requests"`
	Source           string         `json:"source"`
	ToolArgs         map[string]any `json:"tool_args"`
	ToolName         string         `json:"tool_name"`
	Variables        Variables      `json:"variables"`
}

type toolCall struct {
	Argument string `json:"argument"`
	Tool     string `json:"tool"`
}

type exposureCall struct {
	Kind    InvocationKind `json:"kind"`
	Trusted bool           `json:"trusted"`
}

func TestSyntaxFixtures(t *testing.T) {
	fixture.Walk(t, "syntax", "syntax/all", func(t *testing.T, testCase fixture.Case) {
		metadata := testCase.Metadata(t)
		var input syntaxFixtureInput
		testCase.Decode(t, "input.json", &input)
		actual := runSyntaxFixture(t, metadata.Kind, input)
		testCase.CompareJSON(t, "expected.json", actual, fixture.Paths{}, fixture.GoldenReadOnly)
	})
	fixture.AssertCoverage(t, "syntax")
}

func runSyntaxFixture(t *testing.T, kind string, input syntaxFixtureInput) any {
	t.Helper()
	switch kind {
	case "yaml":
		return runYAMLFixture(input)
	case "frontmatter":
		return runFrontmatterFixture(input)
	case "markdown":
		return ScanCodeRegions(input.Source)
	case "command-directives":
		return ScanCommandDirectives(input.Source)
	case "instruction":
		return runInstructionFixture(input)
	case "native-tool":
		name, argument := NativeToolInvocation(input.ToolName, input.ToolArgs)
		return map[string]any{"name": name, "argument": argument}
	case "substitution":
		return runSubstitutionFixture(t, input)
	case "matrix":
		return runMatrixFixture(input)
	case "exposure":
		return runExposureFixture(input)
	case "tool-policy":
		return runToolPolicyFixture(t, input)
	case fieldArguments:
		arguments, err := ParseArguments(input.Source, input.Declared)
		return map[string]any{"arguments": arguments, "error": err != nil}
	case "scope":
		return runScopeFixture(t, input.Concurrent)
	default:
		t.Fatalf("unknown syntax fixture kind %q", kind)
		return nil
	}
}

func runYAMLFixture(input syntaxFixtureInput) map[string]any {
	value, err := ParseYAML(input.Source)
	line := 0
	var yamlErr *YAMLError
	if errors.As(err, &yamlErr) {
		line = yamlErr.Line
	}
	return map[string]any{"error": err != nil, "error_line": line, "value": value}
}

func runFrontmatterFixture(input syntaxFixtureInput) map[string]any {
	document, warnings := ParseDocument(input.Source, "fixture.md", input.Host)
	policy := document.ToolPolicy()
	return map[string]any{
		"document": document,
		"policy": map[string]any{
			"allowed": append([]ToolPattern{}, policy.Allowed()...),
			"denied":  append([]ToolPattern{}, policy.Denied()...),
			"results": policyResults(policy, input.Calls),
		},
		"warnings": fixture.StableWarnings(warnings),
	}
}

func runInstructionFixture(input syntaxFixtureInput) map[string]any {
	instruction, warnings := ParseInstruction(input.Source, "fixture.md", input.Host)
	return map[string]any{
		"body": instruction.Body, "globs": instruction.Globs,
		"results": policyResults(instruction.Policy, input.Calls), "warnings": fixture.StableWarnings(warnings),
	}
}

func runSubstitutionFixture(t *testing.T, input syntaxFixtureInput) map[string]any {
	t.Helper()
	arguments, err := ParseArguments(input.Arguments, input.Declared)
	if err != nil {
		t.Fatal(err)
	}
	output, warnings := Substitute(input.Source, "fixture.md", Substitutions{Arguments: arguments, Variables: input.Variables})
	return map[string]any{"output": output, "warnings": fixture.StableWarnings(warnings)}
}

func runMatrixFixture(input syntaxFixtureInput) any {
	if len(input.Hosts) == 0 {
		return SupportMatrix(input.Host)
	}
	matrices := make([]map[string]any, 0, len(input.Hosts))
	for _, host := range input.Hosts {
		matrices = append(matrices, map[string]any{"host": host, "rules": SupportMatrix(host)})
	}
	return matrices
}

func runExposureFixture(input syntaxFixtureInput) []bool {
	results := make([]bool, 0, len(input.Requests))
	for _, request := range input.Requests {
		results = append(results, input.Exposure.Allows(request.Kind, input.RepositoryScoped, request.Trusted))
	}
	return results
}

func runToolPolicyFixture(t *testing.T, input syntaxFixtureInput) []bool {
	t.Helper()
	policy := NewToolPolicy(mustToolPatterns(t, input.Allowed), mustToolPatterns(t, input.Denied))
	if input.NestedAllowed != "" {
		policy = policy.Intersect(NewToolPolicy(mustToolPatterns(t, input.NestedAllowed), nil))
	}
	return policyResults(policy, input.Calls)
}

func policyResults(policy ToolPolicy, calls []toolCall) []bool {
	results := make([]bool, 0, len(calls))
	for _, call := range calls {
		results = append(results, policy.Allows(call.Tool, call.Argument))
	}
	return results
}

func mustToolPatterns(t *testing.T, source string) []ToolPattern {
	t.Helper()
	if source == "" {
		return nil
	}
	patterns, parseErrors := ParseToolPatterns(source)
	if len(parseErrors) != 0 {
		t.Fatal(parseErrors)
	}
	return patterns
}

func runScopeFixture(t *testing.T, count int) map[string]any {
	t.Helper()
	var store ScopeStore
	var group sync.WaitGroup
	for index := range count {
		group.Add(1)
		go func() {
			defer group.Done()
			key := ScopeKey{SessionID: "fixture", InvocationID: string(rune(index + 1))}
			if err := store.Begin(key, TurnScope{}); err != nil {
				t.Errorf("Begin() error = %v", err)
			}
		}()
	}
	group.Wait()
	active := store.Len()
	for index := range count {
		store.Release(ScopeKey{SessionID: "fixture", InvocationID: string(rune(index + 1))})
	}
	return map[string]any{"active": active, "released": store.Len()}
}
