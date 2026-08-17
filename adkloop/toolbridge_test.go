package adkloop

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"google.golang.org/genai"

	"google.golang.org/adk/v2/agent"
	adktool "google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/toolconfirmation"

	"github.com/plasmid-dev/plasmid/loop"
)

type bridgeContext struct {
	agent.Context
	sessionID    string
	invocationID string
	callID       string
}

func (c *bridgeContext) SessionID() string    { return c.sessionID }
func (c *bridgeContext) InvocationID() string { return c.invocationID }
func (c *bridgeContext) FunctionCallID() string {
	return c.callID
}
func (*bridgeContext) ToolConfirmation() *toolconfirmation.ToolConfirmation { return nil }

type runnableADKTool interface {
	adktool.Tool
	Run(agent.Context, any) (map[string]any, error)
}

type declaredADKTool interface {
	adktool.Tool
	Declaration() *genai.FunctionDeclaration
}

func TestToolBridgePreservesDeclarationAndCallIdentity(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"key":{"type":"string","minLength":1}},"required":["key"],"additionalProperties":false}`)
	var gotCall loop.ToolCall
	core := &testTool{
		name:        "lookup",
		description: "look up one key",
		schema:      schema,
		call: func(_ context.Context, call loop.ToolCall) (loop.ToolResult, error) {
			gotCall = call
			return loop.ToolResult{CallID: call.ID, Content: map[string]any{"value": "beta"}}, nil
		},
	}
	bridged, err := newToolBridge(core)
	if err != nil {
		t.Fatal(err)
	}
	declared, ok := bridged.(declaredADKTool)
	if !ok {
		t.Fatalf("bridged tool %T has no declaration", bridged)
	}
	declaration := declared.Declaration()
	if declaration.Name != "lookup" || declaration.Description != "look up one key" {
		t.Fatalf("declaration = %#v", declaration)
	}
	assertJSONEqual(t, declaration.ParametersJsonSchema, schema)
	core.schema[0] = '['
	assertJSONEqual(t, declaration.ParametersJsonSchema, json.RawMessage(`{"type":"object","properties":{"key":{"type":"string","minLength":1}},"required":["key"],"additionalProperties":false}`))

	runnable, ok := bridged.(runnableADKTool)
	if !ok {
		t.Fatalf("bridged tool %T is not runnable", bridged)
	}
	args := map[string]any{"key": "alpha"}
	result, err := runnable.Run(&bridgeContext{sessionID: "session-1", invocationID: "invocation-1", callID: "call-1"}, args)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result, map[string]any{"value": "beta"}) {
		t.Fatalf("result = %#v", result)
	}
	wantCall := loop.ToolCall{
		ID: "call-1", Name: "lookup", Args: map[string]any{"key": "alpha"}, SessionID: "session-1", InvocationID: "invocation-1",
	}
	if !reflect.DeepEqual(gotCall, wantCall) {
		t.Fatalf("call = %#v, want %#v", gotCall, wantCall)
	}
}

func TestToolBridgeRejectsInvalidSchemas(t *testing.T) {
	tests := []struct {
		name   string
		schema json.RawMessage
	}{
		{name: "empty"},
		{name: "whitespace", schema: json.RawMessage(" \n\t")},
		{name: "malformed", schema: json.RawMessage(`{"type":`)},
		{name: "array", schema: json.RawMessage(`[]`)},
		{name: "null", schema: json.RawMessage(`null`)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := newToolBridge(&testTool{name: "tool", schema: test.schema})
			if err == nil {
				t.Fatal("newToolBridge accepted an invalid schema")
			}
		})
	}
}

func TestToolBridgeRejectsLossyResults(t *testing.T) {
	sentinel := errors.New("tool failed")
	tests := []struct {
		name       string
		result     loop.ToolResult
		callErr    error
		wantErr    error
		wantResult map[string]any
	}{
		{
			name: "nil content", result: loop.ToolResult{CallID: "call-1"}, wantErr: ErrFidelity,
		},
		{
			name: "mismatched call ID", result: loop.ToolResult{CallID: "other", Content: map[string]any{}}, wantErr: ErrFidelity,
		},
		{
			name: "IsError with structured content", result: loop.ToolResult{CallID: "call-1", Content: map[string]any{"error": "failed", "retryable": true}, IsError: true}, wantResult: map[string]any{"error": "failed", "retryable": true},
		},
		{
			name: "IsError adds marker to structured content", result: loop.ToolResult{CallID: "call-1", Content: map[string]any{"retryable": true}, IsError: true}, wantResult: map[string]any{"error": true, "retryable": true},
		},
		{
			name: "Go error preserves structured content", result: loop.ToolResult{CallID: "call-1", Content: map[string]any{"error": "failed", "retryable": true}, IsError: true}, callErr: sentinel, wantResult: map[string]any{"error": "failed", "retryable": true},
		},
		{
			name: "Go error supplies marker to structured content", result: loop.ToolResult{CallID: "call-1", Content: map[string]any{"retryable": true}, IsError: true}, callErr: sentinel, wantResult: map[string]any{"error": sentinel.Error(), "retryable": true},
		},
		{
			name: "Go error without IsError propagates", result: loop.ToolResult{CallID: "call-1", Content: map[string]any{"partial": true}}, callErr: sentinel, wantErr: sentinel,
		},
		{
			name: "Go error propagates without content", result: loop.ToolResult{CallID: "call-1", Content: map[string]any{}, IsError: true}, callErr: sentinel, wantErr: sentinel,
		},
		{
			name: "IsError without content or Go error", result: loop.ToolResult{CallID: "call-1", Content: map[string]any{}, IsError: true}, wantErr: ErrFidelity,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			core := &testTool{
				name:   "tool",
				schema: json.RawMessage(`{"type":"object"}`),
				call: func(context.Context, loop.ToolCall) (loop.ToolResult, error) {
					return test.result, test.callErr
				},
			}
			bridged, err := newToolBridge(core)
			if err != nil {
				t.Fatal(err)
			}
			got, err := bridged.(runnableADKTool).Run(&bridgeContext{callID: "call-1"}, map[string]any{})
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
			if !reflect.DeepEqual(got, test.wantResult) {
				t.Fatalf("result = %#v, want %#v", got, test.wantResult)
			}
		})
	}
}

func assertJSONEqual(t *testing.T, got, want any) {
	t.Helper()
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var gotValue, wantValue any
	if err := json.Unmarshal(gotJSON, &gotValue); err != nil {
		t.Fatalf("decode got JSON %s: %v", gotJSON, err)
	}
	if err := json.Unmarshal(wantJSON, &wantValue); err != nil {
		t.Fatalf("decode want JSON %s: %v", wantJSON, err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("JSON = %s, want %s", gotJSON, wantJSON)
	}
}
