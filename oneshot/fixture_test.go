package oneshot

import (
	"os"
	"slices"
	"sync"
	"testing"

	"github.com/RandomCodeSpace/plasmid/internal/fixture"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"
)

const (
	oneshotFixtureRunner         = "oneshot/core"
	oneshotControlsFixtureRunner = "oneshot/controls"
)

type oneshotFixtureInput struct {
	Instruction string   `json:"instruction"`
	Prompt      string   `json:"prompt"`
	Response    string   `json:"response"`
	Tools       []string `json:"tools"`
}

type oneshotControlsFixtureInput struct {
	Text                    string              `json:"text"`
	FinalText               string              `json:"final_text"`
	FinishReason            genai.FinishReason  `json:"finish_reason"`
	ToolCalls               []string            `json:"tool_calls"`
	MaxOutputTokens         int32               `json:"max_output_tokens"`
	MaxReturnedTextBytes    int                 `json:"max_returned_text_bytes"`
	MaxModelCalls           int                 `json:"max_model_calls"`
	MaxToolCallsPerResponse int                 `json:"max_tool_calls_per_response"`
	ToolExecution           ToolExecutionPolicy `json:"tool_execution"`
}

func init() {
	fixture.RegisterRunner("oneshot", oneshotFixtureRunner, "core")
	fixture.RegisterRunner("oneshot", oneshotControlsFixtureRunner, "controls")
}

func TestOneshotControlFixtures(t *testing.T) {
	fixture.WalkKinds(t, "oneshot", oneshotControlsFixtureRunner, []string{"controls"}, func(t *testing.T, testCase fixture.Case) {
		var input oneshotControlsFixtureInput
		testCase.Decode(t, "input.json", &input)
		projection := runOneshotControlsFixture(t, input)
		testCase.CompareJSON(t, "expected.json", projection, fixture.Paths{}, fixture.GoldenReadOnly)
	})
}

func TestMain(m *testing.M) {
	os.Exit(fixture.Run(m))
}

func TestOneshotFixtureCoverage(t *testing.T) {
	fixture.AssertCoverage(t, "oneshot")
}

func TestOneshotCoreFixtures(t *testing.T) {
	fixture.WalkKinds(t, "oneshot", oneshotFixtureRunner, []string{"core"}, func(t *testing.T, testCase fixture.Case) {
		var input oneshotFixtureInput
		testCase.Decode(t, "input.json", &input)
		projection := runOneshotFixture(t, input)
		testCase.CompareJSON(t, "expected.json", projection, fixture.Paths{}, fixture.GoldenReadOnly)
	})
}

func runOneshotFixture(t *testing.T, input oneshotFixtureInput) any {
	t.Helper()
	projection := struct {
		Instruction string   `json:"instruction"`
		Metadata    Metadata `json:"metadata"`
		Prompt      string   `json:"prompt"`
		Stream      bool     `json:"stream"`
		Text        string   `json:"text"`
		Tools       []string `json:"tools"`
	}{}
	modelValue := &scriptedModel{step: func(_ int, request *model.LLMRequest, stream bool) (*model.LLMResponse, error) {
		projection.Instruction = systemInstruction(request)
		projection.Prompt = latestUserText(request)
		projection.Stream = stream
		projection.Tools = requestToolNames(request)
		return textResponse(input.Response), nil
	}}
	tools := make([]tool.Tool, len(input.Tools))
	for index, name := range input.Tools {
		tools[index] = &testFunctionTool{name: name}
	}
	result, err := Run(t.Context(), boundedRequest(Request{Model: modelValue, Instruction: input.Instruction, Prompt: input.Prompt, Tools: tools}))
	if err != nil {
		t.Fatal(err)
	}
	projection.Text = result.Text
	projection.Metadata = result.Metadata
	slices.Sort(projection.Tools)
	return projection
}

func runOneshotControlsFixture(t *testing.T, input oneshotControlsFixtureInput) any {
	t.Helper()
	var observed []int32
	modelValue := &scriptedModel{step: func(call int, request *model.LLMRequest, _ bool) (*model.LLMResponse, error) {
		observed = append(observed, request.Config.MaxOutputTokens)
		if call == 0 {
			response := functionCallBatchResponse(input.Text, input.ToolCalls...)
			response.FinishReason = input.FinishReason
			return response, nil
		}
		return textResponse(input.FinalText), nil
	}}
	var mu sync.Mutex
	executed := make([]string, 0, len(input.ToolCalls))
	tools := make([]tool.Tool, len(input.ToolCalls))
	for index, name := range input.ToolCalls {
		name := name
		tools[index] = &testFunctionTool{name: name, run: func(agent.Context, any) (map[string]any, error) {
			mu.Lock()
			executed = append(executed, name)
			mu.Unlock()
			return map[string]any{"name": name}, nil
		}}
	}
	result, err := Run(t.Context(), Request{
		Model: modelValue, Prompt: "fixture", Tools: tools,
		MaxOutputTokens: input.MaxOutputTokens, MaxReturnedTextBytes: input.MaxReturnedTextBytes,
		MaxModelCalls: input.MaxModelCalls, MaxToolCallsPerResponse: input.MaxToolCallsPerResponse,
		ToolExecution: input.ToolExecution,
	})
	if executed == nil {
		executed = []string{}
	}
	toolResults := toolResultNames(result.ToolResults)
	if toolResults == nil {
		toolResults = []string{}
	}
	return struct {
		Code            ErrorCode `json:"code"`
		Executed        []string  `json:"executed"`
		MaxOutputTokens []int32   `json:"max_output_tokens"`
		ModelCalls      int       `json:"model_calls"`
		Text            string    `json:"text"`
		ToolCalls       int       `json:"tool_calls"`
		ToolResults     []string  `json:"tool_results"`
	}{
		Code: CodeOf(err), Executed: executed, MaxOutputTokens: observed,
		ModelCalls: result.Metadata.ModelCalls, Text: result.Text, ToolCalls: result.Metadata.ToolCalls,
		ToolResults: toolResults,
	}
}
