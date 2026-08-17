package compaction

import (
	"context"
	"os"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/plasmid-dev/plasmid/config"
	"github.com/plasmid-dev/plasmid/internal/fixture"
	"github.com/plasmid-dev/plasmid/sessionstore"
	"github.com/plasmid-dev/plasmid/warning"
)

const (
	tokenFixtureRunner      = "compaction/estimate"
	compactionFixtureRunner = "compaction/policy"
)

func init() {
	fixture.RegisterRunner("tokenestimate", "compaction/estimate", "estimate")
	fixture.RegisterRunner("compaction", "compaction/policy", "calibration", "policy", "resume", "sidecar-failure")
}

func TestMain(m *testing.M) { os.Exit(fixture.Run(m)) }

type fixtureInput struct {
	Policy       config.Compaction `json:"policy"`
	Request      model.LLMRequest  `json:"request"`
	State        durableState      `json:"state"`
	PromptTokens int32             `json:"promptTokens"`
}

type fixtureOutput struct {
	Canonical string                  `json:"canonical,omitempty"`
	Estimate  *Estimate               `json:"estimate,omitempty"`
	Triggered bool                    `json:"triggered,omitempty"`
	Exhausted bool                    `json:"exhausted,omitempty"`
	Contents  []fixtureContent        `json:"contents,omitempty"`
	State     *durableState           `json:"state,omitempty"`
	Warnings  []fixture.WarningFields `json:"warnings,omitempty"`
}

type fixtureContent struct {
	Role      string   `json:"role"`
	Texts     []string `json:"texts,omitempty"`
	Calls     []string `json:"calls,omitempty"`
	Responses []string `json:"responses,omitempty"`
}

func TestTokenEstimateFixtures(t *testing.T) {
	fixture.WalkKinds(t, "tokenestimate", tokenFixtureRunner, []string{"estimate"}, func(t *testing.T, testCase fixture.Case) {
		var input fixtureInput
		testCase.Decode(t, "input.json", &input)
		estimate, err := EstimateRequest(&input.Request)
		if err != nil {
			t.Fatal(err)
		}
		canonical, err := canonicalJSON(requestWire{Model: input.Request.Model, Contents: input.Request.Contents, Config: input.Request.Config})
		if err != nil {
			t.Fatal(err)
		}
		testCase.CompareJSON(t, "expected.json", fixtureOutput{Canonical: string(canonical), Estimate: &estimate}, fixture.Paths{}, fixture.GoldenReadOnly)
	})
	fixture.AssertCoverage(t, "tokenestimate")
}

func TestCompactionFixtures(t *testing.T) {
	fixture.Walk(t, "compaction", compactionFixtureRunner, func(t *testing.T, testCase fixture.Case) {
		var input fixtureInput
		testCase.Decode(t, "input.json", &input)
		var output fixtureOutput
		switch testCase.Metadata(t).Kind {
		case "policy", "resume":
			got, err := applyPolicy(input.Policy, &input.State, &input.Request)
			if err != nil {
				t.Fatal(err)
			}
			output.Triggered, output.Exhausted = got.Triggered, got.Exhausted
			output.Contents, output.State = projectContents(t, input.Request.Contents), &input.State
		case "calibration":
			manager := New(Config{Policy: input.Policy})
			current := identity{session: "fixture", invocation: "fixture"}
			manager.before(context.Background(), current, &input.Request)
			manager.after(context.Background(), current, &model.LLMResponse{UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: input.PromptTokens}}, nil)
			state := manager.session(current)
			state.mu.Lock()
			durable := state.durable
			output.State = &durable
			state.mu.Unlock()
			output.Contents = projectContents(t, input.Request.Contents)
		case "sidecar-failure":
			store, err := sessionstore.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Create(context.Background(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "fixture"}); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			warnings := &warning.SliceSink{}
			manager := New(Config{Policy: input.Policy, Store: store, WarningSink: warnings})
			current := identity{app: "app", user: "user", session: "fixture", invocation: "fixture"}
			manager.before(context.Background(), current, &input.Request)
			state := manager.session(current)
			state.mu.Lock()
			durable := state.durable
			output.State = &durable
			state.mu.Unlock()
			output.Contents = projectContents(t, input.Request.Contents)
			output.Warnings = fixture.StableWarnings(warnings.Warnings())
		default:
			t.Fatalf("unknown compaction fixture kind %q", testCase.Metadata(t).Kind)
		}
		testCase.CompareJSON(t, "expected.json", output, fixture.Paths{}, fixture.GoldenReadOnly)
	})
	fixture.AssertCoverage(t, "compaction")
}

func projectContents(t *testing.T, contents []*genai.Content) []fixtureContent {
	t.Helper()
	result := make([]fixtureContent, 0, len(contents))
	for _, content := range contents {
		if content == nil {
			continue
		}
		item := fixtureContent{Role: content.Role}
		for _, part := range content.Parts {
			if part == nil {
				continue
			}
			if part.Text != "" {
				item.Texts = append(item.Texts, part.Text)
			}
			if part.FunctionCall != nil {
				item.Calls = append(item.Calls, pairKey(part.FunctionCall.ID, part.FunctionCall.Name))
			}
			if part.FunctionResponse != nil {
				body, err := canonicalJSON(part.FunctionResponse.Response)
				if err != nil {
					t.Fatal(err)
				}
				item.Responses = append(item.Responses, pairKey(part.FunctionResponse.ID, part.FunctionResponse.Name)+":"+string(body))
			}
			if part.ToolCall != nil {
				item.Calls = append(item.Calls, pairKey(part.ToolCall.ID, string(part.ToolCall.ToolType)))
			}
			if part.ToolResponse != nil {
				body, err := canonicalJSON(part.ToolResponse.Response)
				if err != nil {
					t.Fatal(err)
				}
				item.Responses = append(item.Responses, pairKey(part.ToolResponse.ID, string(part.ToolResponse.ToolType))+":"+string(body))
			}
		}
		result = append(result, item)
	}
	return result
}
