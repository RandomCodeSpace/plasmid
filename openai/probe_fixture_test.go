package openai_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RandomCodeSpace/plasmid/internal/fixture"
	"github.com/RandomCodeSpace/plasmid/oneshot"
	"github.com/RandomCodeSpace/plasmid/openai"
)

const openAIProbeFixtureRunner = "openai/probe-wire"

type probeWireFixtureInput struct {
	Scenario string `json:"scenario"`
}

type probeWireProjection struct {
	Attempts         int64  `json:"attempts"`
	Code             string `json:"code"`
	ContextCanceled  bool   `json:"context_canceled"`
	DeadlineExceeded bool   `json:"deadline_exceeded"`
	ForcedPing       bool   `json:"forced_ping"`
	InputTokens      int64  `json:"input_tokens"`
	Model            string `json:"model"`
	ModelCalls       int    `json:"model_calls"`
	OutputTokens     int64  `json:"output_tokens"`
	Path             string `json:"path"`
	RawQuery         string `json:"raw_query"`
	Redacted         bool   `json:"redacted"`
	SharedError      bool   `json:"shared_error"`
	Text             string `json:"text"`
	ToolCalls        int    `json:"tool_calls"`
	ToolDeclarations int    `json:"tool_declarations"`
	ToolName         string `json:"tool_name"`
	ToolResults      int    `json:"tool_results"`
	TotalTokens      int64  `json:"total_tokens"`
}

type capturedProbeRequest struct {
	payload  map[string]any
	path     string
	rawQuery string
}

func init() {
	fixture.RegisterRunner("openai", openAIProbeFixtureRunner, "probe-wire")
}

func TestOpenAIProbeWireFixtures(t *testing.T) {
	fixture.WalkKinds(t, "openai", openAIProbeFixtureRunner, []string{"probe-wire"}, func(t *testing.T, testCase fixture.Case) {
		var input probeWireFixtureInput
		testCase.Decode(t, "input.json", &input)
		responses := runProbeWireProtocol(t, openai.ProtocolResponses, input.Scenario)
		chat := runProbeWireProtocol(t, openai.ProtocolChatCompletions, input.Scenario)
		result := map[string]any{
			"chat_completions": chat,
			"responses":        responses,
			"semantic_parity":  sameProbeSemantics(responses, chat),
		}
		testCase.CompareJSON(t, "expected.json", result, fixture.Paths{}, fixture.GoldenReadOnly)
	})
}

func runProbeWireProtocol(t *testing.T, protocol openai.Protocol, scenario string) probeWireProjection {
	t.Helper()
	var attempts atomic.Int64
	captured := make(chan capturedProbeRequest, 1)
	ctx := t.Context()
	var cancel context.CancelFunc
	if scenario == "canceled" {
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()
	}
	var deadlineContext *probeDeadlineContext
	if scenario == "timeout" {
		deadlineContext = newProbeDeadlineContext(ctx)
		ctx = deadlineContext
		defer deadlineContext.expire()
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		attempts.Add(1)
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("decode probe request: %v", err)
		}
		captured <- capturedProbeRequest{payload: payload, path: request.URL.Path, rawQuery: request.URL.RawQuery}
		if scenario == "canceled" {
			cancel()
			<-request.Context().Done()
			return
		}
		if scenario == "timeout" {
			deadlineContext.expire()
			<-request.Context().Done()
			return
		}
		status, body := probeWireResponse(protocol, scenario)
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("Retry-After", "0")
		writer.WriteHeader(status)
		_, _ = io.WriteString(writer, body)
	}))
	defer server.Close()

	llm, err := openai.New(ctx, openai.Config{
		Protocol: protocol, Model: "fixture-model",
		BaseURL: server.URL + "/v1?endpoint_secret=typed-secret#typed-fragment",
		APIKey:  "fixture-secret-key", HTTPClient: server.Client(), MaxResponseBytes: 8192,
		MaxRetries: 0, ChatTokenLimit: probeChatTokenDialect(protocol),
	})
	if err != nil {
		t.Fatal(err)
	}
	result, probeErr := oneshot.ProbeToolCalling(ctx, llm)
	var request capturedProbeRequest
	select {
	case request = <-captured:
	case <-time.After(5 * time.Second):
		t.Fatal("provider did not receive probe request")
	}
	toolName, toolCount, forced := probeWireToolShape(t, protocol, request.payload)
	var shared *oneshot.Error
	projection := probeWireProjection{
		Attempts: attempts.Load(), Code: string(oneshot.CodeOf(probeErr)),
		ContextCanceled: errors.Is(probeErr, context.Canceled), DeadlineExceeded: errors.Is(probeErr, context.DeadlineExceeded),
		ForcedPing: forced, InputTokens: result.Metadata.Usage.InputTokens,
		Model: request.payload["model"].(string), ModelCalls: result.Metadata.ModelCalls,
		OutputTokens: result.Metadata.Usage.OutputTokens,
		Path:         request.path, RawQuery: request.rawQuery,
		Redacted: probeErrorIsRedacted(probeErr, server.URL), SharedError: probeErr == nil || errors.As(probeErr, &shared),
		Text: result.Text, ToolCalls: result.Metadata.ToolCalls, ToolDeclarations: toolCount, ToolName: toolName,
		ToolResults: len(result.ToolResults), TotalTokens: result.Metadata.Usage.TotalTokens,
	}
	if probeErr == nil {
		projection.Code = "success"
	}
	return projection
}

func probeWireResponse(protocol openai.Protocol, scenario string) (int, string) {
	if scenario == "upstream-failure" {
		return http.StatusInternalServerError, `{"error":{"message":"provider-body-secret"}}`
	}
	if protocol == openai.ProtocolChatCompletions {
		return http.StatusOK, chatProbeResponse(scenario)
	}
	return http.StatusOK, responsesProbeResponse(scenario)
}

func responsesProbeResponse(scenario string) string {
	response := map[string]any{
		"id": "probe-response", "model": "fixture-model",
		"output": []any{responsesProbeCall("plasmid_ping", map[string]any{"marker": "plasmid-probe-v1"})},
		"usage":  responsesProbeUsage(),
	}
	switch scenario {
	case "success", "canceled", "timeout":
	case "text-answer":
		response["output"] = []any{responsesProbeText("pong")}
	case "empty-reply":
		response["output"] = []any{map[string]any{
			"type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": "hidden"}},
		}}
	case "unrelated-tool":
		response["output"] = []any{responsesProbeCall("other", map[string]any{"marker": "plasmid-probe-v1"})}
	case "malformed-ping":
		response["output"] = []any{responsesProbeCall("plasmid_ping", map[string]any{"marker": "wrong"})}
	case "malformed-arguments":
		response["output"] = []any{map[string]any{
			"type": "function_call", "id": "fc-call-1", "call_id": "call-1", "name": "plasmid_ping", "arguments": "{",
		}}
	case "multiple-calls":
		response["output"] = []any{
			responsesProbeCall("plasmid_ping", map[string]any{"marker": "plasmid-probe-v1"}),
			responsesProbeCallWithID("call-2", "plasmid_ping", map[string]any{"marker": "plasmid-probe-v1"}),
		}
	case "mixed-text":
		response["output"] = []any{
			responsesProbeText("pong"),
			responsesProbeCall("plasmid_ping", map[string]any{"marker": "plasmid-probe-v1"}),
		}
	case "custom-tool":
		response["output"] = []any{map[string]any{
			"type": "custom_tool_call", "id": "custom-1", "call_id": "call-1", "name": "plasmid_ping", "input": "secret",
		}}
	case "output-truncated":
		response["status"] = "incomplete"
		response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	default:
		panic("unknown probe scenario " + scenario)
	}
	encoded, _ := json.Marshal(response)
	return string(encoded)
}

func chatProbeResponse(scenario string) string {
	message := map[string]any{"tool_calls": []any{chatProbeCall("call-1", "function", "plasmid_ping", map[string]any{"marker": "plasmid-probe-v1"})}}
	finishReason := "tool_calls"
	switch scenario {
	case "success", "canceled", "timeout":
	case "text-answer":
		message = map[string]any{"content": "pong"}
		finishReason = "stop"
	case "empty-reply":
		message = map[string]any{"content": "<think>hidden</think>"}
		finishReason = "stop"
	case "unrelated-tool":
		message = map[string]any{"tool_calls": []any{chatProbeCall("call-1", "function", "other", map[string]any{"marker": "plasmid-probe-v1"})}}
	case "malformed-ping":
		message = map[string]any{"tool_calls": []any{chatProbeCall("call-1", "function", "plasmid_ping", map[string]any{"marker": "wrong"})}}
	case "malformed-arguments":
		message = map[string]any{"tool_calls": []any{map[string]any{
			"id": "call-1", "type": "function", "function": map[string]any{"name": "plasmid_ping", "arguments": "{"},
		}}}
	case "multiple-calls":
		message = map[string]any{"tool_calls": []any{
			chatProbeCall("call-1", "function", "plasmid_ping", map[string]any{"marker": "plasmid-probe-v1"}),
			chatProbeCall("call-2", "function", "plasmid_ping", map[string]any{"marker": "plasmid-probe-v1"}),
		}}
	case "mixed-text":
		message["content"] = "pong"
	case "custom-tool":
		message = map[string]any{"tool_calls": []any{chatProbeCall("call-1", "custom", "plasmid_ping", map[string]any{"marker": "plasmid-probe-v1"})}}
	case "output-truncated":
		finishReason = "length"
	default:
		panic("unknown probe scenario " + scenario)
	}
	payload := map[string]any{
		"id": "probe-chat", "model": "fixture-model",
		"choices": []any{map[string]any{"finish_reason": finishReason, "message": message}},
		"usage":   map[string]any{"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5},
	}
	encoded, _ := json.Marshal(payload)
	return string(encoded)
}

func responsesProbeCall(name string, arguments map[string]any) map[string]any {
	return responsesProbeCallWithID("call-1", name, arguments)
}

func responsesProbeCallWithID(id, name string, arguments map[string]any) map[string]any {
	encoded, _ := json.Marshal(arguments)
	return map[string]any{"type": "function_call", "id": "fc-" + id, "call_id": id, "name": name, "arguments": string(encoded)}
}

func responsesProbeText(text string) map[string]any {
	return map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": text}}}
}

func responsesProbeUsage() map[string]any {
	return map[string]any{
		"input_tokens": 3, "input_tokens_details": map[string]any{"cached_tokens": 0},
		"output_tokens": 2, "output_tokens_details": map[string]any{"reasoning_tokens": 0}, "total_tokens": 5,
	}
}

func chatProbeCall(id, typeName, name string, arguments map[string]any) map[string]any {
	encoded, _ := json.Marshal(arguments)
	return map[string]any{
		"id": id, "type": typeName,
		"function": map[string]any{"name": name, "arguments": string(encoded)},
	}
}

func probeWireToolShape(t *testing.T, protocol openai.Protocol, payload map[string]any) (string, int, bool) {
	t.Helper()
	tools, ok := payload["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("%s probe tools = %#v", protocol, payload["tools"])
	}
	tool := tools[0].(map[string]any)
	name, _ := tool["name"].(string)
	if protocol == openai.ProtocolChatCompletions {
		function := tool["function"].(map[string]any)
		name, _ = function["name"].(string)
	}
	choice, _ := json.Marshal(payload["tool_choice"])
	return name, len(tools), name == "plasmid_ping" && strings.Contains(string(choice), `"plasmid_ping"`)
}

func sameProbeSemantics(first, second probeWireProjection) bool {
	return first.Code == second.Code && first.Attempts == second.Attempts &&
		first.ContextCanceled == second.ContextCanceled && first.DeadlineExceeded == second.DeadlineExceeded &&
		first.InputTokens == second.InputTokens && first.ModelCalls == second.ModelCalls &&
		first.OutputTokens == second.OutputTokens && first.Text == second.Text && first.ToolCalls == second.ToolCalls &&
		first.Redacted == second.Redacted && first.SharedError == second.SharedError &&
		first.ToolDeclarations == second.ToolDeclarations && first.ToolName == second.ToolName &&
		first.ToolResults == second.ToolResults && first.TotalTokens == second.TotalTokens &&
		first.ForcedPing == second.ForcedPing
}

func probeErrorIsRedacted(err error, serverURL string) bool {
	if err == nil {
		return true
	}
	text := err.Error()
	for _, secret := range []string{"fixture-secret-key", "provider-body-secret", "typed-secret", "typed-fragment", serverURL} {
		if strings.Contains(text, secret) {
			return false
		}
	}
	return true
}

func probeChatTokenDialect(protocol openai.Protocol) openai.ChatTokenLimitDialect {
	if protocol == openai.ProtocolChatCompletions {
		return openai.ChatTokenLimitMaxTokens
	}
	return ""
}

type probeDeadlineContext struct {
	context.Context
	done chan struct{}
	once sync.Once
}

func newProbeDeadlineContext(parent context.Context) *probeDeadlineContext {
	return &probeDeadlineContext{Context: parent, done: make(chan struct{})}
}

func (ctx *probeDeadlineContext) Done() <-chan struct{} { return ctx.done }

func (ctx *probeDeadlineContext) Err() error {
	select {
	case <-ctx.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

func (ctx *probeDeadlineContext) expire() { ctx.once.Do(func() { close(ctx.done) }) }
