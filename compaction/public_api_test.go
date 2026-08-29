package compaction_test

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/RandomCodeSpace/plasmid/compaction"
	"github.com/RandomCodeSpace/plasmid/config"
	"github.com/RandomCodeSpace/plasmid/outputlimit"
	"github.com/RandomCodeSpace/plasmid/sessionstore"
	"github.com/RandomCodeSpace/plasmid/warning"
)

func TestEstimateRequestCountsAllPublicNativeVariants(t *testing.T) {
	request := &model.LLMRequest{
		Contents: []*genai.Content{
			nil,
			{Role: genai.RoleUser, Parts: []*genai.Part{
				nil,
				{FunctionResponse: &genai.FunctionResponse{
					Name:     "read",
					Response: map[string]any{"value": "ok"},
					Parts: []*genai.FunctionResponsePart{
						nil,
						{InlineData: &genai.FunctionResponseBlob{MIMEType: "application/octet-stream", Data: []byte("body")}},
					},
				}},
				{ToolCall: &genai.ToolCall{ID: "tool-call", ToolType: genai.ToolType("computer_use")}},
				{ToolResponse: &genai.ToolResponse{ID: "tool-response", ToolType: genai.ToolType("computer_use"), Response: map[string]any{"ok": true}}},
				{ThoughtSignature: []byte("signature")},
			}},
		},
		Config: &genai.GenerateContentConfig{Tools: []*genai.Tool{
			nil,
			{FunctionDeclarations: []*genai.FunctionDeclaration{{Name: "read"}, {Name: "write"}}},
		}},
	}

	got, err := compaction.EstimateRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if got.Contents != 1 || got.Parts != 4 || got.Functions != 3 || got.Binaries != 2 || got.ToolDeclarations != 2 {
		t.Fatalf("EstimateRequest() counts = %#v", got)
	}
}

func TestEstimateRequestRejectsNonJSONNativeArguments(t *testing.T) {
	request := &model.LLMRequest{Contents: []*genai.Content{{Parts: []*genai.Part{{
		FunctionCall: &genai.FunctionCall{Name: "invalid", Args: map[string]any{"value": make(chan int)}},
	}}}}}
	if _, err := compaction.EstimateRequest(request); err == nil {
		t.Fatal("EstimateRequest() accepted a non-JSON function argument")
	}
}

func TestManagerPublicCallbacksWarnAndResetBudget(t *testing.T) {
	warnings := &warning.SliceSink{}
	budget := outputlimit.NewBudget(1_000)
	reservation := budget.Reserve("session", 500)
	budget.Consume("session", reservation.ID, 500)
	manager := compaction.New(compaction.Config{
		Policy: config.Compaction{
			ContextTokens: 1, TriggerFraction: 0.5, TargetFraction: 0.1,
			MinimumElisionTokens: 1,
		},
		Budget: budget, WarningSink: warnings,
	})
	ctx := newCompactionContext(t, "session", "trigger")
	request := functionResponseRequest("call", "read", strings.Repeat("large body ", 100))
	if replacement, err := manager.BeforeModel(ctx, request); replacement != nil || err != nil {
		t.Fatalf("BeforeModel() = %#v, %v", replacement, err)
	}
	if used, _ := budget.Report("session"); used != 0 {
		t.Fatalf("budget usage after compaction = %d, want 0", used)
	}
	assertCompactionWarning(t, warnings, warning.WarnCompactionBudgetExhausted)

	invalid := &model.LLMRequest{Contents: []*genai.Content{{Parts: []*genai.Part{{
		FunctionCall: &genai.FunctionCall{Name: "invalid", Args: map[string]any{"value": make(chan int)}},
	}}}}}
	if replacement, err := manager.BeforeModel(newCompactionContext(t, "invalid", "invalid"), invalid); replacement != nil || err != nil {
		t.Fatalf("BeforeModel(invalid) = %#v, %v", replacement, err)
	}
	assertCompactionWarning(t, warnings, warning.WarnCompactionEstimateFailed)
}

func TestManagerPublicCallbacksFailSoftWhenResponseChangesDuringProjection(t *testing.T) {
	warnings := &warning.SliceSink{}
	manager := compaction.New(compaction.Config{
		Policy: config.Compaction{
			ContextTokens: 1, TriggerFraction: 0.5, TargetFraction: 0.1,
			MinimumElisionTokens: 1,
		},
		WarningSink: warnings,
	})
	value := &failAfterFirstMarshal{}
	request := &model.LLMRequest{Contents: []*genai.Content{
		genai.NewContentFromText("initial", genai.RoleUser),
		{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "call", Name: "read"}}}},
		{Role: genai.RoleUser, Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{
			ID: "call", Name: "read", Response: map[string]any{"output": value},
		}}}},
		genai.NewContentFromText("active", genai.RoleUser),
	}}
	if replacement, err := manager.BeforeModel(newCompactionContext(t, "projection-error", "one"), request); replacement != nil || err != nil {
		t.Fatalf("BeforeModel() = %#v, %v", replacement, err)
	}
	if value.calls < 2 {
		t.Fatalf("MarshalJSON calls = %d, want at least 2", value.calls)
	}
	assertCompactionWarning(t, warnings, warning.WarnCompactionEstimateFailed)
}

type failAfterFirstMarshal struct {
	calls int
}

func (value *failAfterFirstMarshal) MarshalJSON() ([]byte, error) {
	value.calls++
	if value.calls > 1 {
		return nil, errors.New("projection changed")
	}
	return []byte(`"valid"`), nil
}

func TestManagerPublicCalibrationIgnoresIncompleteUsage(t *testing.T) {
	manager := compaction.New(compaction.Config{Policy: config.Compaction{
		ContextTokens: 10_000, TriggerFraction: 0.9, TargetFraction: 0.5, Calibration: true,
	}})
	tests := []struct {
		name     string
		response *model.LLMResponse
		cause    error
	}{
		{name: "response error", response: &model.LLMResponse{UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 10}}, cause: errors.New("model failed")},
		{name: "nil response"},
		{name: "nil metadata", response: &model.LLMResponse{}},
		{name: "zero prompt", response: &model.LLMResponse{UsageMetadata: &genai.GenerateContentResponseUsageMetadata{}}},
		{name: "valid", response: &model.LLMResponse{UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: math.MaxInt32}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := newCompactionContext(t, test.name, "invocation")
			if _, err := manager.BeforeModel(ctx, &model.LLMRequest{}); err != nil {
				t.Fatal(err)
			}
			if replacement, err := manager.AfterModel(ctx, test.response, test.cause); replacement != nil || err != nil {
				t.Fatalf("AfterModel() = %#v, %v", replacement, err)
			}
		})
	}

	ctx := newCompactionContext(t, "missing", "missing")
	if replacement, err := manager.AfterModel(ctx, &model.LLMResponse{UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 1}}, nil); replacement != nil || err != nil {
		t.Fatalf("AfterModel(without pending request) = %#v, %v", replacement, err)
	}
}

func TestManagerPublicSidecarValidationWarnsOnce(t *testing.T) {
	store := openCompactionStore(t)
	createCompactionSession(t, store, "invalid-sidecar")
	if err := store.AppendSidecar(t.Context(), "app", "user", "invalid-sidecar", "compaction.v1", map[string]any{
		"version": 99, "calibration": 1,
	}); err != nil {
		t.Fatal(err)
	}
	warnings := &warning.SliceSink{}
	manager := compaction.New(compaction.Config{
		Policy: config.Compaction{ContextTokens: 1_000, TriggerFraction: 0.9, TargetFraction: 0.5},
		Store:  store, WarningSink: warnings,
	})
	ctx := newCompactionContext(t, "invalid-sidecar", "one")
	for range 2 {
		if _, err := manager.BeforeModel(ctx, &model.LLMRequest{}); err != nil {
			t.Fatal(err)
		}
	}
	got := warnings.Warnings()
	if len(got) != 1 || got[0].Code != warning.WarnCompactionSidecarLoad {
		t.Fatalf("warnings = %#v, want one sidecar-load warning", got)
	}
}

func TestManagerPublicCompactionHandlesSparseToolTurns(t *testing.T) {
	computer := genai.ToolType("computer_use")
	tests := []struct {
		name     string
		preserve string
		parts    []*genai.Part
	}{
		{
			name: "preserved tool call", preserve: string(computer),
			parts: []*genai.Part{nil, {ToolCall: &genai.ToolCall{ID: "tool", ToolType: computer}}},
		},
		{
			name: "preserved tool response", preserve: string(computer),
			parts: []*genai.Part{nil, {ToolResponse: &genai.ToolResponse{ID: "tool", ToolType: computer, Response: map[string]any{"output": strings.Repeat("body", 20)}}}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager := compaction.New(compaction.Config{Policy: config.Compaction{
				ContextTokens: 1, TriggerFraction: 0.5, TargetFraction: 0.1,
				MinimumElisionTokens: 1, PreserveToolNames: []string{test.preserve},
			}})
			request := &model.LLMRequest{Contents: []*genai.Content{
				genai.NewContentFromText("initial", genai.RoleUser),
				{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "answer"}}},
				genai.NewContentFromText("candidate", genai.RoleUser),
				nil,
				{Role: genai.RoleModel, Parts: test.parts},
				genai.NewContentFromText("active", genai.RoleUser),
			}}
			if _, err := manager.BeforeModel(newCompactionContext(t, test.name, "one"), request); err != nil {
				t.Fatal(err)
			}
			if len(request.Contents) != 6 {
				t.Fatalf("preserved turn was dropped: %#v", request.Contents)
			}
		})
	}
}

func TestManagerPublicCompactionRestoresNonReducingToolResponse(t *testing.T) {
	computer := genai.ToolType("computer_use")
	manager := compaction.New(compaction.Config{Policy: config.Compaction{
		ContextTokens: 1, TriggerFraction: 0.5, TargetFraction: 0.1,
	}})
	response := &genai.ToolResponse{ID: "tool", ToolType: computer, Response: map[string]any{}}
	request := &model.LLMRequest{Contents: []*genai.Content{
		genai.NewContentFromText("initial", genai.RoleUser),
		{Role: genai.RoleModel, Parts: []*genai.Part{{ToolCall: &genai.ToolCall{ID: "tool", ToolType: computer}}}},
		{Role: genai.RoleUser, Parts: []*genai.Part{{ToolResponse: response}}},
		genai.NewContentFromText("active", genai.RoleUser),
	}}
	if _, err := manager.BeforeModel(newCompactionContext(t, "tool-restore", "one"), request); err != nil {
		t.Fatal(err)
	}
	if len(response.Response) != 0 {
		t.Fatalf("non-reducing response changed to %#v", response.Response)
	}
}

func TestManagerPublicCompactionTraversesSparseCompleteToolPair(t *testing.T) {
	computer := genai.ToolType("computer_use")
	manager := compaction.New(compaction.Config{Policy: config.Compaction{
		ContextTokens: 1, TriggerFraction: 0.5, TargetFraction: 0.1,
		MinimumElisionTokens: 1,
	}})
	request := &model.LLMRequest{Contents: []*genai.Content{
		genai.NewContentFromText("initial", genai.RoleUser),
		genai.NewContentFromText("answer", genai.RoleModel),
		genai.NewContentFromText("candidate", genai.RoleUser),
		nil,
		{Role: genai.RoleModel, Parts: []*genai.Part{nil, {ToolCall: &genai.ToolCall{ID: "tool", ToolType: computer}}}},
		{Role: genai.RoleUser, Parts: []*genai.Part{{ToolResponse: &genai.ToolResponse{
			ID: "tool", ToolType: computer, Response: map[string]any{"output": strings.Repeat("body", 40)},
		}}}},
		genai.NewContentFromText("active", genai.RoleUser),
	}}
	if _, err := manager.BeforeModel(newCompactionContext(t, "sparse-pair", "one"), request); err != nil {
		t.Fatal(err)
	}
	if len(request.Contents) >= 7 {
		t.Fatal("eligible complete tool turn was not compacted")
	}
}

func TestManagerPublicCompactionAcceptsMultipartFunctionResponse(t *testing.T) {
	manager := compaction.New(compaction.Config{Policy: config.Compaction{
		ContextTokens: 1, TriggerFraction: 0.5, TargetFraction: 0.1,
		MinimumElisionTokens: 1,
	}})
	request := &model.LLMRequest{Contents: []*genai.Content{
		genai.NewContentFromText("initial", genai.RoleUser),
		{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "read", Name: "read"}}}},
		{Role: genai.RoleUser, Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{
			ID: "read", Name: "read",
			Response: map[string]any{"output": strings.Repeat("body", 40), "status": "ok"},
			Parts:    []*genai.FunctionResponsePart{genai.NewFunctionResponsePartFromBytes([]byte("detail"), "text/plain")},
		}}}},
		genai.NewContentFromText("active", genai.RoleUser),
	}}
	if _, err := manager.BeforeModel(newCompactionContext(t, "multipart", "one"), request); err != nil {
		t.Fatal(err)
	}
}

func TestManagerPublicCompactionStopsAtHigherTarget(t *testing.T) {
	manager := compaction.New(compaction.Config{Policy: config.Compaction{
		ContextTokens: 1_000, TriggerFraction: 0.01, TargetFraction: 0.99,
		MinimumElisionTokens: 1,
	}})
	request := functionResponseRequest("target", "read", strings.Repeat("body", 20))
	before := request.Contents[2].Parts[0].FunctionResponse.Response["output"]
	if _, err := manager.BeforeModel(newCompactionContext(t, "higher-target", "one"), request); err != nil {
		t.Fatal(err)
	}
	if after := request.Contents[2].Parts[0].FunctionResponse.Response["output"]; after != before {
		t.Fatalf("response changed below target: got %#v, want %#v", after, before)
	}
}

func TestManagerPublicStickyDropIgnoresMissingHistoricalTurn(t *testing.T) {
	store := openCompactionStore(t)
	createCompactionSession(t, store, "sticky-drop")
	policy := config.Compaction{
		ContextTokens: 1, TriggerFraction: 0.5, TargetFraction: 0.1,
		MinimumElisionTokens: 1,
	}
	first := compaction.New(compaction.Config{Policy: policy, Store: store})
	original := plainTurnRequest("historical")
	if _, err := first.BeforeModel(newCompactionContext(t, "sticky-drop", "first"), original); err != nil {
		t.Fatal(err)
	}
	if len(original.Contents) >= 5 {
		t.Fatal("first compaction did not persist a dropped turn")
	}

	policy.ContextTokens = 1_000_000
	second := compaction.New(compaction.Config{Policy: policy, Store: store})
	fresh := plainTurnRequest("different")
	if _, err := second.BeforeModel(newCompactionContext(t, "sticky-drop", "second"), fresh); err != nil {
		t.Fatal(err)
	}
	if len(fresh.Contents) != 5 {
		t.Fatalf("unrelated turn was changed during sticky replay: %#v", fresh.Contents)
	}
}

type compactionContext struct {
	agent.StrictContextMock
	sessionID    string
	invocationID string
}

func newCompactionContext(t *testing.T, sessionID, invocationID string) *compactionContext {
	t.Helper()
	return &compactionContext{
		StrictContextMock: agent.NewStrictContextMock(t.Context()),
		sessionID:         sessionID, invocationID: invocationID,
	}
}

func (*compactionContext) AppName() string { return "app" }
func (*compactionContext) UserID() string  { return "user" }
func (c *compactionContext) SessionID() string {
	return c.sessionID
}
func (c *compactionContext) InvocationID() string {
	return c.invocationID
}

func functionResponseRequest(id, name, body string) *model.LLMRequest {
	return &model.LLMRequest{Contents: []*genai.Content{
		genai.NewContentFromText("prompt", genai.RoleUser),
		{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: id, Name: name}}}},
		{Role: genai.RoleUser, Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{ID: id, Name: name, Response: map[string]any{"output": body}}}}},
		genai.NewContentFromText("active", genai.RoleUser),
	}}
}

func plainTurnRequest(body string) *model.LLMRequest {
	return &model.LLMRequest{Contents: []*genai.Content{
		genai.NewContentFromText("initial", genai.RoleUser),
		genai.NewContentFromText("answer", genai.RoleModel),
		genai.NewContentFromText(body, genai.RoleUser),
		genai.NewContentFromText(strings.Repeat("large ", 100), genai.RoleModel),
		genai.NewContentFromText("active", genai.RoleUser),
	}}
}

func openCompactionStore(t *testing.T) *sessionstore.Store {
	t.Helper()
	fsync := false
	store, err := sessionstore.OpenWith(sessionstore.Options{Dir: t.TempDir(), Fsync: &fsync, WarningSink: warning.DiscardSink{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func createCompactionSession(t *testing.T, store *sessionstore.Store, id string) {
	t.Helper()
	if _, err := store.Create(context.Background(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: id}); err != nil {
		t.Fatal(err)
	}
}

func assertCompactionWarning(t *testing.T, sink *warning.SliceSink, code string) {
	t.Helper()
	for _, item := range sink.Warnings() {
		if item.Code == code {
			return
		}
	}
	t.Fatalf("warning %q not found in %#v", code, sink.Warnings())
}
