package compaction

import (
	"fmt"
	"strings"
	"testing"
	"testing/quick"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/plasmid-dev/plasmid/config"
)

func TestPolicyElidesOldestEligibleResponseAndReappliesStickyDecision(t *testing.T) {
	request := responseRequest("call-1", "read", strings.Repeat("old output ", 500))
	state := durableState{Version: sidecarVersion, Calibration: 1}
	policy := config.Compaction{ContextTokens: 1000, TriggerFraction: 0.5, TargetFraction: 0.4, MinimumElisionTokens: 1}
	got, err := applyPolicy(policy, &state, request)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Triggered || len(state.ElidedResponses) != 1 || state.ElidedResponses[0] != "id:call-1" {
		t.Fatalf("result = %#v, state = %#v", got, state)
	}
	assertResponseBody(t, request, ElisionMarker)

	fresh := responseRequest("call-1", "read", "body restored from transcript")
	policy.ContextTokens = 1_000_000
	got, err = applyPolicy(policy, &state, fresh)
	if err != nil {
		t.Fatal(err)
	}
	if got.Triggered {
		t.Fatal("sticky-only replay unexpectedly triggered a new compaction")
	}
	assertResponseBody(t, fresh, ElisionMarker)
}

func TestPolicyPreservesConfiguredToolsAndRecentContents(t *testing.T) {
	request := responseRequest("call-1", "read", strings.Repeat("preserved ", 500))
	state := durableState{Version: sidecarVersion, Calibration: 1}
	policy := config.Compaction{
		ContextTokens: 100, TriggerFraction: 0.5, TargetFraction: 0.4,
		MinimumElisionTokens: 1, PreserveToolNames: []string{"read"}, KeepRecentContents: 4,
	}
	got, err := applyPolicy(policy, &state, request)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Exhausted || len(state.ElidedResponses) != 0 || len(request.Contents) != 4 {
		t.Fatalf("result = %#v, state = %#v, contents = %d", got, state, len(request.Contents))
	}
	assertResponseBody(t, request, strings.Repeat("preserved ", 500))
}

func TestPolicyDoesNotElideRecentResponse(t *testing.T) {
	request := responseRequest("call-1", "read", strings.Repeat("recent ", 500))
	state := durableState{Version: sidecarVersion, Calibration: 1}
	policy := config.Compaction{
		ContextTokens: 100, TriggerFraction: 0.5, TargetFraction: 0.4,
		MinimumElisionTokens: 1, KeepRecentContents: 2,
	}
	got, err := applyPolicy(policy, &state, request)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Exhausted || len(state.ElidedResponses) != 0 {
		t.Fatalf("result = %#v, state = %#v", got, state)
	}
	assertResponseBody(t, request, strings.Repeat("recent ", 500))
}

func TestPolicyDropsOldestCompleteTurnButNeverIndexZeroOrActiveTurn(t *testing.T) {
	request := &model.LLMRequest{Contents: []*genai.Content{
		genai.NewContentFromText("first", genai.RoleUser),
		genai.NewContentFromText(strings.Repeat("protected ", 100), genai.RoleModel),
		genai.NewContentFromText("second", genai.RoleUser),
		genai.NewContentFromText(strings.Repeat("drop ", 800), genai.RoleModel),
		genai.NewContentFromText("current", genai.RoleUser),
	}}
	state := durableState{Version: sidecarVersion, Calibration: 1}
	policy := config.Compaction{ContextTokens: 600, TriggerFraction: 0.5, TargetFraction: 0.4, KeepRecentContents: 1, MinimumElisionTokens: 1}
	got, err := applyPolicy(policy, &state, request)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Triggered || len(state.DroppedTurns) != 1 {
		t.Fatalf("result = %#v, state = %#v", got, state)
	}
	if len(request.Contents) != 3 || request.Contents[0].Parts[0].Text != "first" || request.Contents[2].Parts[0].Text != "current" {
		t.Fatalf("remaining contents = %#v", request.Contents)
	}
}

func TestPolicyDoesNotSplitFunctionCallResponsePair(t *testing.T) {
	request := &model.LLMRequest{Contents: []*genai.Content{
		genai.NewContentFromText("first", genai.RoleUser),
		genai.NewContentFromText("first answer", genai.RoleModel),
		genai.NewContentFromText("second", genai.RoleUser),
		{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "pair", Name: "read"}}}},
		genai.NewContentFromText("current", genai.RoleUser),
		{Role: genai.RoleUser, Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{ID: "pair", Name: "read", Response: map[string]any{"output": "late"}}}}},
	}}
	state := durableState{Version: sidecarVersion, Calibration: 1}
	policy := config.Compaction{ContextTokens: 1, TriggerFraction: 0.5, TargetFraction: 0.1, MinimumElisionTokens: 1000}
	got, err := applyPolicy(policy, &state, request)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Exhausted || len(state.DroppedTurns) != 0 || len(request.Contents) != 6 {
		t.Fatalf("pair was split: result=%#v state=%#v contents=%d", got, state, len(request.Contents))
	}
}

func TestPolicyNativeResponseAndStickyDropBranches(t *testing.T) {
	tests := []struct {
		name    string
		request func(t *testing.T) (*model.LLMRequest, durableState)
		check   func(t *testing.T, request *model.LLMRequest, state durableState, got policyResult)
	}{
		{
			name: "server tool response elision",
			request: func(t *testing.T) (*model.LLMRequest, durableState) {
				return &model.LLMRequest{Contents: []*genai.Content{
					genai.NewContentFromText("first", genai.RoleUser),
					{Role: genai.RoleModel, Parts: []*genai.Part{{ToolCall: &genai.ToolCall{ID: "server", ToolType: genai.ToolTypeGoogleSearchWeb}}}},
					{Role: genai.RoleUser, Parts: []*genai.Part{{ToolResponse: &genai.ToolResponse{ID: "server", ToolType: genai.ToolTypeGoogleSearchWeb, Response: map[string]any{"output": strings.Repeat("result ", 500)}}}}},
					genai.NewContentFromText("current", genai.RoleUser),
				}}, durableState{Version: sidecarVersion, Calibration: 1}
			},
			check: func(t *testing.T, request *model.LLMRequest, state durableState, got policyResult) {
				if !got.Triggered || len(state.ElidedResponses) != 1 || state.ElidedResponses[0] != "id:server" {
					t.Fatalf("result = %#v, state = %#v", got, state)
				}
				response := request.Contents[2].Parts[0].ToolResponse
				if response.ID != "server" || response.ToolType != genai.ToolTypeGoogleSearchWeb || response.Response["output"] != ElisionMarker {
					t.Fatalf("tool response = %#v", response)
				}
			},
		},
		{
			name: "sticky dropped turn replay",
			request: func(t *testing.T) (*model.LLMRequest, durableState) {
				contents := []*genai.Content{
					genai.NewContentFromText("first", genai.RoleUser),
					genai.NewContentFromText("protected", genai.RoleModel),
					genai.NewContentFromText("second", genai.RoleUser),
					genai.NewContentFromText(strings.Repeat("restored ", 500), genai.RoleModel),
					genai.NewContentFromText("current", genai.RoleUser),
				}
				key, err := turnKey(contents[2:4])
				if err != nil {
					t.Fatal(err)
				}
				return &model.LLMRequest{Contents: contents}, durableState{Version: sidecarVersion, Calibration: 1, DroppedTurns: []string{key}}
			},
			check: func(t *testing.T, request *model.LLMRequest, state durableState, got policyResult) {
				if got.StateChanged || len(request.Contents) != 3 || request.Contents[2].Parts[0].Text != "current" {
					t.Fatalf("result = %#v, state = %#v, contents = %#v", got, state, request.Contents)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request, state := test.request(t)
			got, err := applyPolicy(config.Compaction{
				ContextTokens: 100, TriggerFraction: 0.5, TargetFraction: 0.4,
				MinimumElisionTokens: 1, KeepRecentContents: 1,
			}, &state, request)
			if err != nil {
				t.Fatal(err)
			}
			test.check(t, request, state, got)
		})
	}
}

func TestPolicyPreservesNativeValuesOutsideChangedResponse(t *testing.T) {
	const large = int64(9_007_199_254_740_993)
	request := responseRequest("call-1", "read", strings.Repeat("old output ", 500))
	request.Contents[0].Parts[0].FunctionCall = &genai.FunctionCall{ID: "protected", Name: "tool", Args: map[string]any{"large": large}}
	state := durableState{Version: sidecarVersion, Calibration: 1}
	got, err := applyPolicy(config.Compaction{
		ContextTokens: 1000, TriggerFraction: 0.5, TargetFraction: 0.4,
		MinimumElisionTokens: 1,
	}, &state, request)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Triggered {
		t.Fatal("request did not trigger compaction")
	}
	value := request.Contents[0].Parts[0].FunctionCall.Args["large"]
	if value != large {
		t.Fatalf("large native value = %#v (%T), want %d (int64)", value, value, large)
	}
}

func TestPolicyLeavesRequestIdentityUntouchedWhenNoDecisionApplies(t *testing.T) {
	request := &model.LLMRequest{Contents: []*genai.Content{{
		Role: genai.RoleUser,
		Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
			ID: "protected", Name: "tool", Args: map[string]any{"large": int64(9_007_199_254_740_993)},
		}}},
	}}}
	content, part := request.Contents[0], request.Contents[0].Parts[0]
	state := durableState{Version: sidecarVersion, Calibration: 1, ElidedResponses: []string{"id:missing"}}
	got, err := applyPolicy(config.Compaction{
		ContextTokens: 100_000, TriggerFraction: 0.85, TargetFraction: 0.6,
		MinimumElisionTokens: 1,
	}, &state, request)
	if err != nil {
		t.Fatal(err)
	}
	if got.Triggered || request.Contents[0] != content || request.Contents[0].Parts[0] != part {
		t.Fatalf("no-op compaction changed request identity: result=%#v", got)
	}
}

func TestPolicyStickyDecisionsRespectCurrentPreservedTools(t *testing.T) {
	request := responseRequest("call-1", "read", "restored body")
	state := durableState{Version: sidecarVersion, Calibration: 1, ElidedResponses: []string{"id:call-1"}}
	got, err := applyPolicy(config.Compaction{
		ContextTokens: 100_000, TriggerFraction: 0.85, TargetFraction: 0.6,
		MinimumElisionTokens: 1, PreserveToolNames: []string{"read"},
	}, &state, request)
	if err != nil {
		t.Fatal(err)
	}
	if got.Triggered {
		t.Fatal("preserved sticky response unexpectedly triggered compaction")
	}
	assertResponseBody(t, request, "restored body")
}

func TestPolicyDoesNotDropTurnContainingPreservedTool(t *testing.T) {
	request := &model.LLMRequest{Contents: []*genai.Content{
		genai.NewContentFromText("first", genai.RoleUser),
		genai.NewContentFromText("protected", genai.RoleModel),
		genai.NewContentFromText("second", genai.RoleUser),
		{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "read-1", Name: "read"}}}},
		{Role: genai.RoleUser, Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{ID: "read-1", Name: "read", Response: map[string]any{"output": strings.Repeat("body ", 500)}}}}},
		genai.NewContentFromText("current", genai.RoleUser),
	}}
	state := durableState{Version: sidecarVersion, Calibration: 1}
	got, err := applyPolicy(config.Compaction{
		ContextTokens: 1, TriggerFraction: 0.5, TargetFraction: 0.1,
		MinimumElisionTokens: 1, KeepRecentContents: 1, PreserveToolNames: []string{"read"},
	}, &state, request)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Exhausted || len(request.Contents) != 6 || len(state.DroppedTurns) != 0 {
		t.Fatalf("result=%#v state=%#v contents=%d", got, state, len(request.Contents))
	}
}

func TestPolicyElidesEligibleResponseAfterPreservedTurn(t *testing.T) {
	request := &model.LLMRequest{Contents: []*genai.Content{
		genai.NewContentFromText("first", genai.RoleUser),
		{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "read-1", Name: "read"}}}},
		{Role: genai.RoleUser, Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{ID: "read-1", Name: "read", Response: map[string]any{"output": strings.Repeat("preserved ", 100)}}}}},
		genai.NewContentFromText("protected answer", genai.RoleModel),
		genai.NewContentFromText("second", genai.RoleUser),
		{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "grep-1", Name: "grep"}}}},
		{Role: genai.RoleUser, Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{ID: "grep-1", Name: "grep", Response: map[string]any{"matches": strings.Repeat("eligible ", 500)}}}}},
	}}
	state := durableState{Version: sidecarVersion, Calibration: 1}
	got, err := applyPolicy(config.Compaction{
		ContextTokens: 1, TriggerFraction: 0.5, TargetFraction: 0.1,
		MinimumElisionTokens: 1, PreserveToolNames: []string{"read"},
	}, &state, request)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Triggered || !got.Exhausted || len(state.ElidedResponses) != 1 {
		t.Fatalf("result=%#v state=%#v", got, state)
	}
	if got := request.Contents[6].Parts[0].FunctionResponse.Response["output"]; got != ElisionMarker {
		t.Fatalf("eligible response = %v, want %q", got, ElisionMarker)
	}
}

func TestPolicySkipsElisionThatWouldNotReduceEstimate(t *testing.T) {
	request := responseRequest("call-1", "read", "")
	before, err := EstimateRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	state := durableState{Version: sidecarVersion, Calibration: 1}
	got, err := applyPolicy(config.Compaction{
		ContextTokens: 1, TriggerFraction: 0.5, TargetFraction: 0.1,
		MinimumElisionTokens: 0, KeepRecentContents: 1,
	}, &state, request)
	if err != nil {
		t.Fatal(err)
	}
	after, err := EstimateRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Exhausted || len(state.ElidedResponses) != 0 || after.Tokens != before.Tokens {
		t.Fatalf("result=%#v state=%#v estimates=%d/%d", got, state, before.Tokens, after.Tokens)
	}
	assertResponseBody(t, request, "")
}

func TestPolicyStickyTurnMultiplicityDropsOnlySelectedOccurrences(t *testing.T) {
	contents := []*genai.Content{
		genai.NewContentFromText("first", genai.RoleUser),
		genai.NewContentFromText("protected", genai.RoleModel),
		genai.NewContentFromText("duplicate", genai.RoleUser),
		genai.NewContentFromText("same", genai.RoleModel),
		genai.NewContentFromText("duplicate", genai.RoleUser),
		genai.NewContentFromText("same", genai.RoleModel),
		genai.NewContentFromText("current", genai.RoleUser),
	}
	key, err := turnKey(contents[2:4])
	if err != nil {
		t.Fatal(err)
	}
	request := &model.LLMRequest{Contents: contents}
	state := durableState{Version: sidecarVersion, Calibration: 1, DroppedTurns: []string{key}}
	got, err := applyPolicy(config.Compaction{
		ContextTokens: 100_000, TriggerFraction: 0.85, TargetFraction: 0.6,
		MinimumElisionTokens: 1, KeepRecentContents: 1,
	}, &state, request)
	if err != nil {
		t.Fatal(err)
	}
	if got.Triggered || len(request.Contents) != 5 || request.Contents[2].Parts[0].Text != "duplicate" {
		t.Fatalf("result=%#v contents=%#v", got, request.Contents)
	}
}

func TestPolicyRecordsMultiplicityWhenDroppingIdenticalTurns(t *testing.T) {
	request := &model.LLMRequest{Contents: []*genai.Content{
		genai.NewContentFromText("first", genai.RoleUser),
		genai.NewContentFromText("protected", genai.RoleModel),
		genai.NewContentFromText("duplicate", genai.RoleUser),
		genai.NewContentFromText(strings.Repeat("same ", 500), genai.RoleModel),
		genai.NewContentFromText("duplicate", genai.RoleUser),
		genai.NewContentFromText(strings.Repeat("same ", 500), genai.RoleModel),
		genai.NewContentFromText("current", genai.RoleUser),
	}}
	state := durableState{Version: sidecarVersion, Calibration: 1}
	got, err := applyPolicy(config.Compaction{
		ContextTokens: 100, TriggerFraction: 0.5, TargetFraction: 0.4,
		MinimumElisionTokens: 1, KeepRecentContents: 1,
	}, &state, request)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Triggered || len(state.DroppedTurns) != 2 || state.DroppedTurns[0] != state.DroppedTurns[1] || len(request.Contents) != 3 {
		t.Fatalf("result=%#v state=%#v contents=%d", got, state, len(request.Contents))
	}
}

func TestPolicyMatchesRepeatedIDLessCallsByOccurrence(t *testing.T) {
	contents := []*genai.Content{
		genai.NewContentFromText("first", genai.RoleUser),
		genai.NewContentFromText("protected", genai.RoleModel),
		genai.NewContentFromText("older", genai.RoleUser),
		{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{Name: "read"}}}},
		{Role: genai.RoleUser, Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{Name: "read", Response: map[string]any{"output": "older result"}}}}},
		genai.NewContentFromText("older answer", genai.RoleModel),
		genai.NewContentFromText("newer", genai.RoleUser),
		{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{Name: "read"}}}},
		{Role: genai.RoleUser, Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{Name: "read", Response: map[string]any{"output": "newer result"}}}}},
		genai.NewContentFromText("newer answer", genai.RoleModel),
		genai.NewContentFromText("current", genai.RoleUser),
	}
	groups, err := completeTurns(contents)
	if err != nil {
		t.Fatal(err)
	}
	candidate := firstDroppableTurn(contents, groups, 1, nil)
	if candidate == nil || candidate.start != 2 || candidate.end != 6 {
		t.Fatalf("first droppable turn = %#v, want [2,6)", candidate)
	}
}

func TestPolicyPersistsRepeatedIDLessResponseMultiplicity(t *testing.T) {
	body := strings.Repeat("same body ", 200)
	request := repeatedIDLessResponseRequest(body)
	state := durableState{Version: sidecarVersion, Calibration: 1}
	policy := config.Compaction{
		ContextTokens: 1, TriggerFraction: 0.5, TargetFraction: 0.1,
		MinimumElisionTokens: 1, KeepRecentContents: 1,
	}
	got, err := applyPolicy(policy, &state, request)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Triggered || len(state.ElidedResponses) != 2 || state.ElidedResponses[0] != state.ElidedResponses[1] {
		t.Fatalf("result=%#v state=%#v", got, state)
	}

	fresh := repeatedIDLessResponseRequest(body)
	state.ElidedResponses = state.ElidedResponses[:1]
	policy.ContextTokens = 1_000_000
	got, err = applyPolicy(policy, &state, fresh)
	if err != nil {
		t.Fatal(err)
	}
	if got.Triggered {
		t.Fatal("sticky-only replay unexpectedly triggered compaction")
	}
	if first, second := responseBody(fresh.Contents[2]), responseBody(fresh.Contents[4]); first != ElisionMarker || second != body {
		t.Fatalf("replayed bodies = %q, %q", first, second)
	}
}

func TestPolicyPropertiesPreserveIndexZeroAndNeverIncreaseEstimate(t *testing.T) {
	property := func(size uint16) bool {
		body := strings.Repeat("x", int(size)+1)
		request := responseRequest("property", "tool", body)
		before, err := EstimateRequest(request)
		if err != nil {
			return false
		}
		first := request.Contents[0].Parts[0].Text
		state := durableState{Version: sidecarVersion, Calibration: 1}
		_, err = applyPolicy(config.Compaction{ContextTokens: 1, TriggerFraction: 0.5, TargetFraction: 0.1, MinimumElisionTokens: 1}, &state, request)
		if err != nil || request.Contents[0].Parts[0].Text != first {
			return false
		}
		after, err := EstimateRequest(request)
		return err == nil && after.Tokens <= before.Tokens
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 200}); err != nil {
		t.Fatal(err)
	}
}

func responseRequest(id, name, body string) *model.LLMRequest {
	return &model.LLMRequest{Contents: []*genai.Content{
		genai.NewContentFromText("first", genai.RoleUser),
		{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: id, Name: name}}}},
		{Role: genai.RoleUser, Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{ID: id, Name: name, Response: map[string]any{"output": body}}}}},
		genai.NewContentFromText("current", genai.RoleUser),
	}}
}

func repeatedIDLessResponseRequest(body string) *model.LLMRequest {
	return &model.LLMRequest{Contents: []*genai.Content{
		genai.NewContentFromText("first", genai.RoleUser),
		{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{Name: "read"}}}},
		{Role: genai.RoleUser, Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{Name: "read", Response: map[string]any{"output": body}}}}},
		{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{Name: "read"}}}},
		{Role: genai.RoleUser, Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{Name: "read", Response: map[string]any{"output": body}}}}},
		genai.NewContentFromText("current", genai.RoleUser),
	}}
}

func responseBody(content *genai.Content) string {
	return fmt.Sprint(content.Parts[0].FunctionResponse.Response["output"])
}

func assertResponseBody(t *testing.T, request *model.LLMRequest, want string) {
	t.Helper()
	for _, content := range request.Contents {
		for _, part := range content.Parts {
			if part != nil && part.FunctionResponse != nil {
				if got := fmt.Sprint(part.FunctionResponse.Response["output"]); got != want {
					t.Fatalf("response body = %q, want %q", got, want)
				}
				return
			}
		}
	}
	t.Fatal("request contains no function response")
}
