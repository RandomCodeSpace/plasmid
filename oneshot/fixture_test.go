package oneshot

import (
	"os"
	"slices"
	"testing"

	"github.com/RandomCodeSpace/plasmid/internal/fixture"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
)

const oneshotFixtureRunner = "oneshot/core"

type oneshotFixtureInput struct {
	Instruction string   `json:"instruction"`
	Prompt      string   `json:"prompt"`
	Response    string   `json:"response"`
	Tools       []string `json:"tools"`
}

func init() {
	fixture.RegisterRunner("oneshot", oneshotFixtureRunner, "core")
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
	result, err := Run(t.Context(), Request{Model: modelValue, Instruction: input.Instruction, Prompt: input.Prompt, Tools: tools})
	if err != nil {
		t.Fatal(err)
	}
	projection.Text = result.Text
	projection.Metadata = result.Metadata
	slices.Sort(projection.Tools)
	return projection
}
