package syntax

import (
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/plasmid-dev/plasmid/internal/fixture"
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
		var actual any
		switch metadata.Kind {
		case "yaml":
			value, err := ParseYAML(input.Source)
			line := 0
			var yamlErr *YAMLError
			if errors.As(err, &yamlErr) {
				line = yamlErr.Line
			}
			actual = map[string]any{"error": err != nil, "error_line": line, "value": value}
		case "frontmatter":
			document, warnings := ParseDocument(input.Source, "fixture.md", input.Host)
			policy := document.ToolPolicy()
			results := make([]bool, 0, len(input.Calls))
			for _, call := range input.Calls {
				results = append(results, policy.Allows(call.Tool, call.Argument))
			}
			actual = map[string]any{
				"document": document,
				"policy": map[string]any{
					"allowed": append([]ToolPattern{}, policy.Allowed()...),
					"denied":  append([]ToolPattern{}, policy.Denied()...),
					"results": results,
				},
				"warnings": fixture.StableWarnings(warnings),
			}
		case "markdown":
			actual = ScanCodeRegions(input.Source)
		case "command-directives":
			actual = ScanCommandDirectives(input.Source)
		case "instruction":
			instruction, warnings := ParseInstruction(input.Source, "fixture.md", input.Host)
			results := make([]bool, 0, len(input.Calls))
			for _, call := range input.Calls {
				results = append(results, instruction.Policy.Allows(call.Tool, call.Argument))
			}
			actual = map[string]any{"body": instruction.Body, "globs": instruction.Globs, "results": results, "warnings": fixture.StableWarnings(warnings)}
		case "native-tool":
			name, argument := NativeToolInvocation(input.ToolName, input.ToolArgs)
			actual = map[string]any{"name": name, "argument": argument}
		case "substitution":
			arguments, err := ParseArguments(input.Arguments, input.Declared)
			if err != nil {
				t.Fatal(err)
			}
			output, warnings := Substitute(input.Source, "fixture.md", Substitutions{Arguments: arguments, Variables: input.Variables})
			actual = map[string]any{"output": output, "warnings": fixture.StableWarnings(warnings)}
		case "matrix":
			if len(input.Hosts) == 0 {
				actual = SupportMatrix(input.Host)
				break
			}
			matrices := make([]map[string]any, 0, len(input.Hosts))
			for _, host := range input.Hosts {
				matrices = append(matrices, map[string]any{"host": host, "rules": SupportMatrix(host)})
			}
			actual = matrices
		case "exposure":
			results := make([]bool, 0, len(input.Requests))
			for _, request := range input.Requests {
				results = append(results, input.Exposure.Allows(request.Kind, input.RepositoryScoped, request.Trusted))
			}
			actual = results
		case "tool-policy":
			allowed := mustToolPatterns(t, input.Allowed)
			denied := mustToolPatterns(t, input.Denied)
			policy := NewToolPolicy(allowed, denied)
			if input.NestedAllowed != "" {
				policy = policy.Intersect(NewToolPolicy(mustToolPatterns(t, input.NestedAllowed), nil))
			}
			results := make([]bool, 0, len(input.Calls))
			for _, call := range input.Calls {
				results = append(results, policy.Allows(call.Tool, call.Argument))
			}
			actual = results
		case "arguments":
			arguments, err := ParseArguments(input.Source, input.Declared)
			actual = map[string]any{"arguments": arguments, "error": err != nil}
		case "scope":
			actual = runScopeFixture(t, input.Concurrent)
		default:
			t.Fatalf("unknown syntax fixture kind %q", metadata.Kind)
		}
		testCase.CompareJSON(t, "expected.json", actual, fixture.Paths{}, fixture.GoldenReadOnly)
	})
	fixture.AssertCoverage(t, "syntax")
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
