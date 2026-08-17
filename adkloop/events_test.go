package adkloop

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"google.golang.org/genai"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"

	"github.com/plasmid-dev/plasmid/loop"
	warningpkg "github.com/plasmid-dev/plasmid/warning"
)

func TestToLoopEventsPreservesMixedPartOrderAndIdentity(t *testing.T) {
	timestamp := time.Date(2026, 8, 17, 12, 0, 0, 123000000, time.UTC)
	native := &session.Event{
		ID: "event-1", InvocationID: "invocation-1", Author: "agent", Timestamp: timestamp,
		LLMResponse: model.LLMResponse{
			ErrorCode: "provider_error", ErrorMessage: "details", TurnComplete: true,
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount: 3, CandidatesTokenCount: 5, TotalTokenCount: 8,
			},
			Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{
				genai.NewPartFromText("first"),
				{FunctionCall: &genai.FunctionCall{ID: "call-1", Name: "lookup", Args: map[string]any{"key": "alpha"}}},
				{FunctionResponse: &genai.FunctionResponse{ID: "call-1", Name: "lookup", Response: map[string]any{"value": "beta"}}},
				nil,
				{Thought: true, Text: "provider-only thought"},
				genai.NewPartFromText("last"),
			}},
		},
	}
	events, err := toLoopEvents("session-1", native)
	if err != nil {
		t.Fatal(err)
	}
	wantKinds := []loop.EventKind{
		loop.EventError,
		loop.EventText,
		loop.EventToolCall,
		loop.EventToolResult,
		loop.EventWarning,
		loop.EventWarning,
		loop.EventText,
		loop.EventTurnComplete,
	}
	gotKinds := make([]loop.EventKind, len(events))
	raw, err := json.Marshal(native)
	if err != nil {
		t.Fatal(err)
	}
	for index, event := range events {
		gotKinds[index] = event.Kind
		if event.SessionID != "session-1" || event.InvocationID != "invocation-1" || event.Author != "agent" || !event.Timestamp.Equal(timestamp) {
			t.Errorf("event %d identity = %#v", index, event)
		}
		eventRaw := event.Raw
		if !bytes.Equal(eventRaw, raw) {
			t.Errorf("event %d Raw = %#v, want %s", index, event.Raw, raw)
		}
		if event.Usage == nil || *event.Usage != (loop.Usage{PromptTokens: 3, CompletionTokens: 5, TotalTokens: 8}) {
			t.Errorf("event %d usage = %#v", index, event.Usage)
		}
	}
	if !reflect.DeepEqual(gotKinds, wantKinds) {
		t.Fatalf("event kinds = %#v, want %#v", gotKinds, wantKinds)
	}
	if events[0].Text != "provider_error: details" || events[1].Text != "first" || events[6].Text != "last" {
		t.Fatalf("text events = %#v", events)
	}
	if events[2].Tool == nil || events[2].Tool.ID != "call-1" || events[2].Tool.Name != "lookup" || !reflect.DeepEqual(events[2].Tool.Args, map[string]any{"key": "alpha"}) {
		t.Fatalf("tool call = %#v", events[2])
	}
	if events[3].Tool == nil || events[3].Tool.CallID != "call-1" || events[3].Tool.Name != "lookup" || events[3].Tool.Content["value"] != "beta" || events[3].Tool.IsError {
		t.Fatalf("tool result = %#v", events[3])
	}
	if events[4].Warning == nil || events[4].Warning.Code != warningpkg.WarnADKEventMalformed || events[5].Warning == nil || events[5].Warning.Code != warningpkg.WarnADKEventUnknownPart {
		t.Fatalf("warnings = %#v, %#v", events[4], events[5])
	}
}

func TestToLoopEventsClassifiesEdgeCases(t *testing.T) {
	usage := &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 1, TotalTokenCount: 1}
	tests := []struct {
		name      string
		event     *session.Event
		wantKind  loop.EventKind
		wantCode  string
		wantError bool
	}{
		{name: "nil event", wantError: true},
		{
			name: "partial text", event: &session.Event{LLMResponse: model.LLMResponse{Partial: true, Content: genai.NewContentFromText("delta", genai.RoleModel)}}, wantKind: loop.EventTextDelta,
		},
		{
			name: "partial function call", event: &session.Event{LLMResponse: model.LLMResponse{Partial: true, Content: &genai.Content{Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "id", Name: "tool"}}}}}}, wantKind: loop.EventWarning, wantCode: warningpkg.WarnADKEventMalformed,
		},
		{
			name: "nil part", event: &session.Event{LLMResponse: model.LLMResponse{Content: &genai.Content{Parts: []*genai.Part{nil}}}}, wantKind: loop.EventWarning, wantCode: warningpkg.WarnADKEventMalformed,
		},
		{
			name: "call missing ID", event: &session.Event{LLMResponse: model.LLMResponse{Content: &genai.Content{Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{Name: "tool"}}}}}}, wantKind: loop.EventWarning, wantCode: warningpkg.WarnADKEventMalformed,
		},
		{
			name: "result missing name", event: &session.Event{LLMResponse: model.LLMResponse{Content: &genai.Content{Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{ID: "id", Response: map[string]any{}}}}}}}, wantKind: loop.EventWarning, wantCode: warningpkg.WarnADKEventMalformed,
		},
		{
			name: "unknown provider part", event: &session.Event{LLMResponse: model.LLMResponse{Content: &genai.Content{Parts: []*genai.Part{{Thought: true, Text: "hidden"}}}}}, wantKind: loop.EventWarning, wantCode: warningpkg.WarnADKEventUnknownPart,
		},
		{
			name: "invalid mixed fields", event: &session.Event{LLMResponse: model.LLMResponse{Content: &genai.Content{Parts: []*genai.Part{{Text: "lost", FunctionCall: &genai.FunctionCall{ID: "id", Name: "tool"}}}}}}, wantKind: loop.EventWarning, wantCode: warningpkg.WarnADKEventMalformed,
		},
		{
			name: "usage only", event: &session.Event{LLMResponse: model.LLMResponse{UsageMetadata: usage}}, wantKind: loop.EventNotice,
		},
		{
			name: "nil content", event: &session.Event{}, wantKind: loop.EventWarning, wantCode: warningpkg.WarnADKEventMalformed,
		},
		{
			name: "empty content", event: &session.Event{LLMResponse: model.LLMResponse{Content: &genai.Content{}}}, wantKind: loop.EventWarning, wantCode: warningpkg.WarnADKEventMalformed,
		},
		{
			name: "error result", event: &session.Event{LLMResponse: model.LLMResponse{Content: &genai.Content{Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{ID: "id", Name: "tool", Response: map[string]any{"error": "failed"}}}}}}}, wantKind: loop.EventToolResult,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			events, err := toLoopEvents("session", test.event)
			if test.wantError {
				if err == nil {
					t.Fatal("expected conversion error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(events) != 1 || events[0].Kind != test.wantKind {
				t.Fatalf("events = %#v", events)
			}
			if test.wantCode != "" && (events[0].Warning == nil || events[0].Warning.Code != test.wantCode) {
				t.Fatalf("warning = %#v", events[0].Warning)
			}
			if test.name == "error result" && (events[0].Tool == nil || !events[0].Tool.IsError) {
				t.Fatalf("error result = %#v", events[0])
			}
		})
	}
}

func TestToLoopEventsClonesPortablePayloads(t *testing.T) {
	args := map[string]any{"key": "alpha"}
	response := map[string]any{"value": "beta"}
	native := &session.Event{LLMResponse: model.LLMResponse{Content: &genai.Content{Parts: []*genai.Part{
		{FunctionCall: &genai.FunctionCall{ID: "call", Name: "tool", Args: args}},
		{FunctionResponse: &genai.FunctionResponse{ID: "call", Name: "tool", Response: response}},
	}}}}
	events, err := toLoopEvents("session", native)
	if err != nil {
		t.Fatal(err)
	}
	args["key"] = "mutated"
	response["value"] = "mutated"
	if events[0].Tool.Args["key"] != "alpha" || events[1].Tool.Content["value"] != "beta" {
		t.Fatalf("portable payload aliases native event: %#v", events)
	}
}
