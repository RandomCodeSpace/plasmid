package codingtools

import (
	"context"
	"encoding/json"
	"iter"
	"os"
	"reflect"
	"testing"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/RandomCodeSpace/plasmid/outputlimit"
	"github.com/RandomCodeSpace/plasmid/shellexec"
	"github.com/RandomCodeSpace/plasmid/warning"
	"github.com/RandomCodeSpace/plasmid/workspace"
)

type declarer interface {
	Declaration() *genai.FunctionDeclaration
}

func TestNativeToolsExposeExplicitSchemas(t *testing.T) {
	root, err := workspace.NewRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	shell, err := shellexec.New(shellexec.Config{Root: root, Shell: "sh"})
	if err != nil {
		t.Fatal(err)
	}
	set, err := New(Config{Root: root, Queue: workspace.NewMutationQueue(), Ledger: workspace.NewLedger(), Touch: workspace.NewTouchBus(), Shell: shell})
	if err != nil {
		t.Fatal(err)
	}
	wantSchemas := map[string]json.RawMessage{
		"read": ReadInputSchema(), "write": WriteInputSchema(), "edit": EditInputSchema(),
		"bash": BashInputSchema(), "grep": GrepInputSchema(), "find": FindInputSchema(), "ls": ListInputSchema(),
	}
	for _, native := range set.Tools() {
		assertNativeToolSchema(t, native, wantSchemas[native.Name()])
	}
}

func assertNativeToolSchema(t *testing.T, native any, schema json.RawMessage) {
	t.Helper()
	tool, ok := native.(declarer)
	if !ok {
		t.Fatalf("value %T is not a native function tool", native)
	}
	declaration := tool.Declaration()
	want := normalizedSchemaObject(t, schema)
	parameters, err := json.Marshal(declaration.ParametersJsonSchema)
	if err != nil {
		t.Fatal(err)
	}
	var got any
	if err := json.Unmarshal(parameters, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tool parameters = %#v, want %#v", got, want)
	}
	assertResponseSchemaObject(t, declaration.ResponseJsonSchema)
}

func normalizedSchemaObject(t *testing.T, schema json.RawMessage) any {
	t.Helper()
	var value any
	if err := json.Unmarshal(schema, &value); err != nil {
		t.Fatal(err)
	}
	if object, ok := value.(map[string]any); ok {
		if required, ok := object["required"].([]any); ok && len(required) == 0 {
			delete(object, "required")
		}
	}
	return value
}

func assertResponseSchemaObject(t *testing.T, schema any) {
	t.Helper()
	encoded, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.Unmarshal(encoded, &response); err != nil || response["type"] != "object" {
		t.Fatalf("response schema = %#v", schema)
	}
}

func TestNativeADKRunnerInvokesReadTool(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(directory+"/file.txt", []byte("native\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := workspace.NewRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	bus := workspace.NewTouchBus()
	observer := &listObserver{}
	bus.Subscribe(observer)
	set, err := New(Config{
		Root:        root,
		Queue:       workspace.NewMutationQueue(),
		Ledger:      workspace.NewLedger(),
		Touch:       bus,
		Budget:      outputlimit.NewBudget(100000),
		WarningSink: warning.DiscardSink{},
	})
	if err != nil {
		t.Fatal(err)
	}
	llm := &nativeToolModel{responses: []*genai.Content{
		{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "read-call", Name: "read", Args: map[string]any{"path": fixtureTargetFile}}}}},
		genai.NewContentFromText("done", genai.RoleModel),
	}}
	agentValue, err := llmagent.New(llmagent.Config{Name: "coding_agent", Model: llm, Tools: set.Tools()})
	if err != nil {
		t.Fatal(err)
	}
	runnerValue, err := runner.NewInMemory("plasmid-test", agentValue)
	if err != nil {
		t.Fatal(err)
	}

	var response *genai.FunctionResponse
	for event, runErr := range runnerValue.Run(t.Context(), "user", "native-session", genai.NewContentFromText("read the file", genai.RoleUser), agent.RunConfig{}) {
		if runErr != nil {
			t.Fatal(runErr)
		}
		if found := nativeReadResponse(event); found != nil {
			response = found
		}
	}
	assertNativeRunnerResult(t, response, observer.snapshot(), llm.calls)
}

func assertNativeRunnerResult(t *testing.T, response *genai.FunctionResponse, touches []workspace.Touch, modelCalls int) {
	t.Helper()
	if response == nil {
		t.Fatal("native runner emitted no read FunctionResponse")
	}
	if response.ID != "read-call" || response.Response["path"] != fixtureTargetFile {
		t.Fatalf("function response = %#v", response)
	}
	if len(touches) != 1 || touches[0].SessionID != "native-session" || touches[0].InvocationID != "read-call" || touches[0].Path != fixtureTargetFile || touches[0].Kind != workspace.TouchRead {
		t.Fatalf("touches = %#v", touches)
	}
	if modelCalls != 2 {
		t.Fatalf("model calls = %d, want 2", modelCalls)
	}
}

func nativeReadResponse(event *session.Event) *genai.FunctionResponse {
	if event == nil || event.Content == nil {
		return nil
	}
	for _, part := range event.Content.Parts {
		if part != nil && part.FunctionResponse != nil && part.FunctionResponse.Name == "read" {
			return part.FunctionResponse
		}
	}
	return nil
}

type nativeToolModel struct {
	responses []*genai.Content
	calls     int
}

func (*nativeToolModel) Name() string { return "native-tool-test" }

func (m *nativeToolModel) GenerateContent(_ context.Context, request *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if m.calls >= len(m.responses) {
			yield(nil, context.Canceled)
			return
		}
		if m.calls == 0 {
			if request.Tools["read"] == nil {
				yield(nil, &missingNativeReadToolError{})
				return
			}
		}
		content := m.responses[m.calls]
		m.calls++
		yield(&model.LLMResponse{Content: content}, nil)
	}
}

type missingNativeReadToolError struct{}

func (*missingNativeReadToolError) Error() string { return "native request omitted read tool" }

var _ model.LLM = (*nativeToolModel)(nil)
