package openai_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/RandomCodeSpace/plasmid/openai"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

func TestChatCompletionsPreservesMultiTurnHistoryToolsAndMetadata(t *testing.T) {
	var (
		mu       sync.Mutex
		requests []map[string]any
		paths    []string
		calls    atomic.Int64
	)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Errorf("decode body: %v", err)
		}
		mu.Lock()
		requests = append(requests, payload)
		paths = append(paths, request.Method+" "+request.URL.Path)
		mu.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			_, _ = io.WriteString(writer, `{
				"id":"chat-response-1","model":"provider-model",
				"choices":[{"finish_reason":"tool_calls","message":{"content":"","tool_calls":[{
					"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{\"city\":\"Paris\"}"}
				}]}}],
				"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}
			}`)
			return
		}
		_, _ = io.WriteString(writer, `{
			"id":"chat-response-2","model":"provider-model",
			"choices":[{"finish_reason":"stop","message":{"content":"<think>provider reasoning</think>Paris is 21 C."}}],
			"usage":{"prompt_tokens":19,"completion_tokens":5,"total_tokens":24}
		}`)
	}))
	defer server.Close()

	llm := newChatModel(t, server.URL+"/v1", server.Client(), openai.ChatTokenLimitMaxCompletionTokens)
	config := &genai.GenerateContentConfig{
		SystemInstruction: genai.NewContentFromText("Use tools when needed.", "system"),
		MaxOutputTokens:   128,
		Tools: []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{
			{Name: "lookup", Description: "Look up a city", ParametersJsonSchema: map[string]any{
				"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}, "required": []string{"city"},
			}},
			{Name: "clock", Description: "Read a clock", ParametersJsonSchema: map[string]any{"type": "object", "properties": map[string]any{}}},
		}}},
		ToolConfig: &genai.ToolConfig{FunctionCallingConfig: &genai.FunctionCallingConfig{
			Mode: genai.FunctionCallingConfigModeAny, AllowedFunctionNames: []string{"lookup"},
		}},
	}
	firstRequest := &model.LLMRequest{
		Model: "request-model", Config: config,
		Contents: []*genai.Content{genai.NewContentFromText("Weather in Paris?", genai.RoleUser)},
	}
	first, err := oneChatResponse(llm, t.Context(), firstRequest, false)
	if err != nil {
		t.Fatal(err)
	}
	if first.FinishReason != genai.FinishReasonStop || first.ModelVersion != "provider-model" {
		t.Fatalf("first response finish/model = %q/%q", first.FinishReason, first.ModelVersion)
	}
	if first.UsageMetadata.PromptTokenCount != 11 || first.UsageMetadata.CandidatesTokenCount != 7 || first.UsageMetadata.TotalTokenCount != 18 {
		t.Fatalf("first usage = %#v", first.UsageMetadata)
	}
	if first.CustomMetadata["openai_response_id"] != "chat-response-1" ||
		first.CustomMetadata["openai_model"] != "provider-model" ||
		first.CustomMetadata["openai_finish_reason"] != "tool_calls" {
		t.Fatalf("first metadata = %#v", first.CustomMetadata)
	}
	call := first.Content.Parts[0].FunctionCall
	if call == nil || call.ID != "call-1" || call.Name != "lookup" || call.Args["city"] != "Paris" {
		t.Fatalf("first function call = %#v", call)
	}

	toolResult := &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{
		ID: "call-1", Name: "lookup", Response: map[string]any{"temperature_c": 21},
	}}}}
	secondRequest := &model.LLMRequest{Model: "request-model", Config: config, Contents: []*genai.Content{
		firstRequest.Contents[0], first.Content, toolResult,
	}}
	second, err := oneChatResponse(llm, t.Context(), secondRequest, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := responseText(second); got != "Paris is 21 C." {
		t.Fatalf("final text = %q", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(paths, []string{"POST /v1/chat/completions", "POST /v1/chat/completions"}) {
		t.Fatalf("request paths = %#v", paths)
	}
	if len(requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(requests))
	}
	assertFirstChatRequest(t, requests[0])
	assertSecondChatRequest(t, requests[1])
}

func assertFirstChatRequest(t *testing.T, request map[string]any) {
	t.Helper()
	if request["model"] != "request-model" || request["n"] != float64(1) {
		t.Fatalf("model/n = %#v/%#v", request["model"], request["n"])
	}
	if request["max_completion_tokens"] != float64(128) {
		t.Fatalf("max_completion_tokens = %#v", request["max_completion_tokens"])
	}
	if _, exists := request["max_tokens"]; exists {
		t.Fatal("max_tokens was sent with max_completion_tokens dialect")
	}
	messages := request["messages"].([]any)
	if len(messages) != 2 || messages[0].(map[string]any)["role"] != "system" || messages[1].(map[string]any)["role"] != "user" {
		t.Fatalf("first messages = %#v", messages)
	}
	tools := request["tools"].([]any)
	if len(tools) != 2 || tools[0].(map[string]any)["function"].(map[string]any)["name"] != "lookup" ||
		tools[1].(map[string]any)["function"].(map[string]any)["name"] != "clock" {
		t.Fatalf("tools = %#v", tools)
	}
	lookup := tools[0].(map[string]any)["function"].(map[string]any)
	if lookup["description"] != "Look up a city" || lookup["parameters"].(map[string]any)["type"] != "object" {
		t.Fatalf("lookup declaration = %#v", lookup)
	}
	choice := request["tool_choice"].(map[string]any)
	allowed := choice["allowed_tools"].(map[string]any)
	allowedTools := allowed["tools"].([]any)
	if choice["type"] != "allowed_tools" || allowed["mode"] != "required" ||
		allowedTools[0].(map[string]any)["function"].(map[string]any)["name"] != "lookup" {
		t.Fatalf("tool_choice = %#v", choice)
	}
}

func assertSecondChatRequest(t *testing.T, request map[string]any) {
	t.Helper()
	messages := request["messages"].([]any)
	if len(messages) != 4 {
		t.Fatalf("second messages = %#v", messages)
	}
	wantRoles := []string{"system", "user", "assistant", "tool"}
	for index, want := range wantRoles {
		if got := messages[index].(map[string]any)["role"]; got != want {
			t.Fatalf("message %d role = %#v, want %q", index, got, want)
		}
	}
	assistant := messages[2].(map[string]any)
	toolCalls := assistant["tool_calls"].([]any)
	if toolCalls[0].(map[string]any)["id"] != "call-1" {
		t.Fatalf("assistant tool calls = %#v", toolCalls)
	}
	tool := messages[3].(map[string]any)
	if tool["tool_call_id"] != "call-1" || tool["content"] != `{"temperature_c":21}` {
		t.Fatalf("tool result = %#v", tool)
	}
}

func TestChatCompletionsNormalizesOnlySchemaNodes(t *testing.T) {
	received := make(chan map[string]any, 1)
	server := chatServer(t, func(request map[string]any) string {
		received <- request
		return chatTextResponse("stop", "ok")
	})
	defer server.Close()
	llm := newChatModel(t, server.URL+"/v1", server.Client(), openai.ChatTokenLimitMaxTokens)
	schema := map[string]any{
		"type": "OBJECT",
		"properties": map[string]any{
			"value": map[string]any{
				"type":    "STRING",
				"default": map[string]any{"type": "PRESERVE_DEFAULT"},
			},
		},
		"$defs": map[string]any{
			"nested": map[string]any{"type": "ARRAY", "items": map[string]any{"type": "INTEGER"}},
		},
		"examples": []any{map[string]any{"type": "PRESERVE_EXAMPLE"}},
		"const":    map[string]any{"type": "PRESERVE_CONST"},
		"enum":     []any{map[string]any{"type": "PRESERVE_ENUM"}},
	}
	request := &model.LLMRequest{
		Contents: []*genai.Content{genai.NewContentFromText("hello", genai.RoleUser)},
		Config: &genai.GenerateContentConfig{Tools: []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{{
			Name: "lookup", ParametersJsonSchema: schema,
		}}}}},
	}
	if _, err := oneChatResponse(llm, t.Context(), request, false); err != nil {
		t.Fatal(err)
	}
	tool := (<-received)["tools"].([]any)[0].(map[string]any)
	parameters := tool["function"].(map[string]any)["parameters"].(map[string]any)
	property := parameters["properties"].(map[string]any)["value"].(map[string]any)
	nested := parameters["$defs"].(map[string]any)["nested"].(map[string]any)
	if parameters["type"] != "object" || property["type"] != "string" ||
		nested["type"] != "array" || nested["items"].(map[string]any)["type"] != "integer" {
		t.Fatalf("schema node types were not normalized: %#v", parameters)
	}
	if property["default"].(map[string]any)["type"] != "PRESERVE_DEFAULT" ||
		parameters["examples"].([]any)[0].(map[string]any)["type"] != "PRESERVE_EXAMPLE" ||
		parameters["const"].(map[string]any)["type"] != "PRESERVE_CONST" ||
		parameters["enum"].([]any)[0].(map[string]any)["type"] != "PRESERVE_ENUM" {
		t.Fatalf("literal schema data was mutated: %#v", parameters)
	}
}

func TestChatCompletionsTokenLimitDialects(t *testing.T) {
	tests := []struct {
		name       string
		dialect    openai.ChatTokenLimitDialect
		limit      int32
		wantField  string
		otherField string
	}{
		{name: "max tokens", dialect: openai.ChatTokenLimitMaxTokens, limit: 9, wantField: "max_tokens", otherField: "max_completion_tokens"},
		{name: "max completion tokens", dialect: openai.ChatTokenLimitMaxCompletionTokens, limit: 9, wantField: "max_completion_tokens", otherField: "max_tokens"},
		{name: "zero max tokens", dialect: openai.ChatTokenLimitMaxTokens, limit: 0, otherField: "max_completion_tokens"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload := make(chan map[string]any, 1)
			server := chatServer(t, func(request map[string]any) string {
				payload <- request
				return chatTextResponse("stop", "ok")
			})
			defer server.Close()
			llm := newChatModel(t, server.URL+"/v1", server.Client(), test.dialect)
			_, err := oneChatResponse(llm, t.Context(), &model.LLMRequest{
				Contents: []*genai.Content{genai.NewContentFromText("hello", genai.RoleUser)},
				Config:   &genai.GenerateContentConfig{MaxOutputTokens: test.limit},
			}, false)
			if err != nil {
				t.Fatal(err)
			}
			request := <-payload
			if test.wantField != "" && request[test.wantField] != float64(test.limit) {
				t.Fatalf("%s = %#v", test.wantField, request[test.wantField])
			}
			if test.wantField == "" {
				if _, exists := request["max_tokens"]; exists {
					t.Fatal("zero MaxOutputTokens sent max_tokens")
				}
			}
			if _, exists := request[test.otherField]; exists {
				t.Fatalf("unexpected field %s", test.otherField)
			}
		})
	}
}

func TestChatCompletionsToolSelectionModes(t *testing.T) {
	tests := []struct {
		name    string
		config  *genai.FunctionCallingConfig
		want    any
		wantErr bool
	}{
		{name: "auto", config: &genai.FunctionCallingConfig{Mode: genai.FunctionCallingConfigModeAuto}, want: "auto"},
		{name: "none", config: &genai.FunctionCallingConfig{Mode: genai.FunctionCallingConfigModeNone}, want: "none"},
		{name: "required", config: &genai.FunctionCallingConfig{Mode: genai.FunctionCallingConfigModeAny}, want: "required"},
		{name: "one required allowed function", config: &genai.FunctionCallingConfig{Mode: genai.FunctionCallingConfigModeAny, AllowedFunctionNames: []string{"lookup"}}, want: map[string]any{
			"type": "allowed_tools",
			"allowed_tools": map[string]any{
				"mode": "required",
				"tools": []any{
					map[string]any{"type": "function", "function": map[string]any{"name": "lookup"}},
				},
			},
		}},
		{name: "auto allowed subset", config: &genai.FunctionCallingConfig{Mode: genai.FunctionCallingConfigModeAuto, AllowedFunctionNames: []string{"lookup", "clock"}}, want: map[string]any{
			"type": "allowed_tools",
			"allowed_tools": map[string]any{
				"mode": "auto",
				"tools": []any{
					map[string]any{"type": "function", "function": map[string]any{"name": "lookup"}},
					map[string]any{"type": "function", "function": map[string]any{"name": "clock"}},
				},
			},
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload := make(chan map[string]any, 1)
			server := chatServer(t, func(request map[string]any) string {
				payload <- request
				return chatTextResponse("stop", "ok")
			})
			defer server.Close()
			llm := newChatModel(t, server.URL+"/v1", server.Client(), openai.ChatTokenLimitMaxTokens)
			request := &model.LLMRequest{
				Contents: []*genai.Content{genai.NewContentFromText("hello", genai.RoleUser)},
				Config: &genai.GenerateContentConfig{
					Tools:      []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{{Name: "lookup"}, {Name: "clock"}}}},
					ToolConfig: &genai.ToolConfig{FunctionCallingConfig: test.config},
				},
			}
			_, err := oneChatResponse(llm, t.Context(), request, false)
			if test.wantErr {
				assertChatError(t, err, openai.ChatErrorUnsupportedContent)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := (<-payload)["tool_choice"]; !reflect.DeepEqual(got, test.want) {
				t.Fatalf("tool_choice = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestChatCompletionsNormalizesAndValidatesToolCallsBeforeYield(t *testing.T) {
	responses := make(chan string, 8)
	server := chatServer(t, func(map[string]any) string { return <-responses })
	defer server.Close()
	llm := newChatModel(t, server.URL+"/v1", server.Client(), openai.ChatTokenLimitMaxTokens)
	req := &model.LLMRequest{Contents: []*genai.Content{genai.NewContentFromText("hello", genai.RoleUser)}}

	const (
		generatedCollisionID = "plasmid-chat-8164847dad47f41982a5b6e1"
		generatedSecondID    = "plasmid-chat-4fa72f133cb9f7a9830e0d6f"
		generatedThirdID     = "plasmid-chat-97349321c935c2779f7c8a40"
	)
	normalizationTests := []struct {
		name      string
		toolCalls string
		wantIDs   []string
	}{
		{
			name:      "duplicate explicit and blank",
			toolCalls: `[{"id":"same","type":"function","function":{"name":"a","arguments":"{}"}},{"id":"same","type":"function","function":{"name":"b","arguments":"{}"}},{"type":"function","function":{"name":"c","arguments":"{}"}}]`,
			wantIDs:   []string{"same", generatedSecondID, generatedThirdID},
		},
		{
			name:      "generated collision with later explicit",
			toolCalls: `[{"type":"function","function":{"name":"a","arguments":"{}"}},{"id":"` + generatedCollisionID + `","type":"function","function":{"name":"b","arguments":"{}"}},{"id":"` + generatedCollisionID + `","type":"function","function":{"name":"c","arguments":"{}"}}]`,
			wantIDs:   []string{generatedSecondID, generatedCollisionID, generatedThirdID},
		},
	}
	for _, test := range normalizationTests {
		t.Run(test.name, func(t *testing.T) {
			responseBody := `{"id":"same-response","model":"m","choices":[{"finish_reason":"tool_calls","message":{"tool_calls":` + test.toolCalls + `}}]}`
			responses <- responseBody
			first, err := oneChatResponse(llm, t.Context(), req, false)
			if err != nil {
				t.Fatal(err)
			}
			responses <- responseBody
			second, err := oneChatResponse(llm, t.Context(), req, false)
			if err != nil {
				t.Fatal(err)
			}
			firstIDs := functionCallIDs(first)
			secondIDs := functionCallIDs(second)
			if !reflect.DeepEqual(firstIDs, test.wantIDs) || !reflect.DeepEqual(secondIDs, test.wantIDs) {
				t.Fatalf("normalized IDs first=%#v second=%#v, want %#v", firstIDs, secondIDs, test.wantIDs)
			}
			seen := make(map[string]struct{}, len(firstIDs))
			for _, id := range firstIDs {
				if id == "" {
					t.Fatalf("normalized IDs contain a blank: %#v", firstIDs)
				}
				if _, duplicate := seen[id]; duplicate {
					t.Fatalf("normalized IDs contain a duplicate: %#v", firstIDs)
				}
				seen[id] = struct{}{}
			}
			for index, wantName := range []string{"a", "b", "c"} {
				if got := first.Content.Parts[index].FunctionCall.Name; got != wantName {
					t.Fatalf("call %d name = %q, want %q", index, got, wantName)
				}
			}
		})
	}

	argumentTests := []struct {
		name      string
		arguments string
		want      map[string]any
	}{
		{name: "blank", arguments: "", want: map[string]any{}},
		{name: "whitespace", arguments: " \n\t", want: map[string]any{}},
		{name: "object", arguments: `{"value":"kept"}`, want: map[string]any{"value": "kept"}},
	}
	for _, test := range argumentTests {
		t.Run(test.name+" arguments", func(t *testing.T) {
			responses <- fixtureToolCallResponse("arguments", "call", "function", "lookup", test.arguments)
			response, err := oneChatResponse(llm, t.Context(), req, false)
			if err != nil {
				t.Fatal(err)
			}
			call := response.Content.Parts[0].FunctionCall
			if !reflect.DeepEqual(call.Args, test.want) {
				t.Fatalf("arguments = %#v, want %#v", call.Args, test.want)
			}
		})
	}

	validationTests := []struct {
		name      string
		toolCalls string
		kind      openai.ChatErrorKind
	}{
		{name: "unsupported type", toolCalls: `[{"id":"a","type":"custom","function":{"name":"a","arguments":"{}"}}]`, kind: openai.ChatErrorUnsupportedToolCall},
		{name: "missing name", toolCalls: `[{"id":"a","type":"function","function":{"arguments":"{}"}}]`, kind: openai.ChatErrorMissingFunctionName},
		{name: "malformed", toolCalls: `[{"id":"a","type":"function","function":{"name":"a","arguments":"{"}}]`, kind: openai.ChatErrorMalformedArguments},
		{name: "trailing", toolCalls: `[{"id":"a","type":"function","function":{"name":"a","arguments":"{} {}"}}]`, kind: openai.ChatErrorMalformedArguments},
		{name: "null", toolCalls: `[{"id":"a","type":"function","function":{"name":"a","arguments":"null"}}]`, kind: openai.ChatErrorMalformedArguments},
		{name: "array", toolCalls: `[{"id":"a","type":"function","function":{"name":"a","arguments":"[]"}}]`, kind: openai.ChatErrorMalformedArguments},
		{name: "scalar", toolCalls: `[{"id":"a","type":"function","function":{"name":"a","arguments":"1"}}]`, kind: openai.ChatErrorMalformedArguments},
	}
	for _, test := range validationTests {
		t.Run(test.name, func(t *testing.T) {
			responses <- `{"id":"same-response","model":"m","choices":[{"finish_reason":"tool_calls","message":{"content":"must not yield","tool_calls":` + test.toolCalls + `}}]}`
			response, err := oneChatResponse(llm, t.Context(), req, false)
			if response != nil {
				t.Fatalf("response was yielded before validation: %#v", response)
			}
			assertChatError(t, err, test.kind)
		})
	}
}

func TestChatCompletionsNormalizedIDRoundTripsIntoHistory(t *testing.T) {
	var (
		calls  atomic.Int64
		second = make(chan map[string]any, 1)
	)
	server := chatServer(t, func(request map[string]any) string {
		if calls.Add(1) == 1 {
			return `{"id":"normalization-source","model":"m","choices":[{"finish_reason":"tool_calls","message":{"tool_calls":[{"function":{"name":"lookup","arguments":"{}"}}]}}]}`
		}
		second <- request
		return chatTextResponse("stop", "done")
	})
	defer server.Close()
	llm := newChatModel(t, server.URL+"/v1", server.Client(), openai.ChatTokenLimitMaxTokens)
	user := genai.NewContentFromText("hello", genai.RoleUser)
	first, err := oneChatResponse(llm, t.Context(), &model.LLMRequest{Contents: []*genai.Content{user}}, false)
	if err != nil {
		t.Fatal(err)
	}
	id := first.Content.Parts[0].FunctionCall.ID
	toolResult := &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{
		ID: id, Name: "lookup", Response: map[string]any{"ok": true},
	}}}}
	_, err = oneChatResponse(llm, t.Context(), &model.LLMRequest{Contents: []*genai.Content{user, first.Content, toolResult}}, false)
	if err != nil {
		t.Fatal(err)
	}
	messages := (<-second)["messages"].([]any)
	assistantCall := messages[1].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	toolMessage := messages[2].(map[string]any)
	if assistantCall["id"] != id || toolMessage["tool_call_id"] != id {
		t.Fatalf("normalized ID %q did not round-trip: call=%#v tool=%#v", id, assistantCall, toolMessage)
	}
}

func TestChatCompletionsMatchesToolResponsesByIDAndName(t *testing.T) {
	received := make(chan map[string]any, 1)
	server := chatServer(t, func(request map[string]any) string {
		received <- request
		return chatTextResponse("stop", "done")
	})
	defer server.Close()
	llm := newChatModel(t, server.URL+"/v1", server.Client(), openai.ChatTokenLimitMaxTokens)
	request := toolResponseHistory("", "clock", []toolHistoryCall{
		{id: "call-a", name: "lookup"},
		{id: "call-b", name: "clock"},
	})
	if _, err := oneChatResponse(llm, t.Context(), request, false); err != nil {
		t.Fatal(err)
	}
	messages := (<-received)["messages"].([]any)
	toolMessage := messages[len(messages)-1].(map[string]any)
	if toolMessage["tool_call_id"] != "call-b" {
		t.Fatalf("tool_call_id = %#v, want call-b", toolMessage["tool_call_id"])
	}

	var calls atomic.Int64
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("unexpected transport")
	})}
	invalid := []struct {
		name     string
		response *model.LLMRequest
	}{
		{
			name: "explicit ID name mismatch",
			response: toolResponseHistory("call-a", "clock", []toolHistoryCall{
				{id: "call-a", name: "lookup"},
			}),
		},
		{
			name: "ambiguous ID-less response",
			response: toolResponseHistory("", "lookup", []toolHistoryCall{
				{id: "call-a", name: "lookup"},
				{id: "call-b", name: "lookup"},
			}),
		},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			llm := newChatModel(t, "https://example.test/v1", client, openai.ChatTokenLimitMaxTokens)
			_, err := oneChatResponse(llm, t.Context(), test.response, false)
			assertChatError(t, err, openai.ChatErrorUnsupportedContent)
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("transport calls = %d, want 0", calls.Load())
	}
}

func TestChatCompletionsRejectsUnrepresentableToolResponseStateWithoutTransport(t *testing.T) {
	continued := false
	tests := []struct {
		name   string
		mutate func(*genai.FunctionResponse)
	}{
		{
			name: "inline response part",
			mutate: func(response *genai.FunctionResponse) {
				response.Parts = []*genai.FunctionResponsePart{genai.NewFunctionResponsePartFromBytes([]byte("x"), "text/plain")}
			},
		},
		{
			name: "file response part",
			mutate: func(response *genai.FunctionResponse) {
				response.Parts = []*genai.FunctionResponsePart{genai.NewFunctionResponsePartFromURI("file:///result.txt", "text/plain")}
			},
		},
		{
			name: "continuation flag",
			mutate: func(response *genai.FunctionResponse) {
				response.WillContinue = &continued
			},
		},
		{
			name: "scheduling mode",
			mutate: func(response *genai.FunctionResponse) {
				response.Scheduling = genai.FunctionResponseSchedulingSilent
			},
		},
	}
	var calls atomic.Int64
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("unexpected transport")
	})}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := toolResponseHistory("call-a", "lookup", []toolHistoryCall{{id: "call-a", name: "lookup"}})
			test.mutate(request.Contents[2].Parts[0].FunctionResponse)
			llm := newChatModel(t, "https://example.test/v1", client, openai.ChatTokenLimitMaxTokens)
			_, err := oneChatResponse(llm, t.Context(), request, false)
			assertChatError(t, err, openai.ChatErrorUnsupportedContent)
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("transport calls = %d, want 0", calls.Load())
	}
}

func TestChatCompletionsRejectsUnrepresentableToolCallStateWithoutTransport(t *testing.T) {
	continued := false
	tests := []struct {
		name   string
		mutate func(*genai.FunctionCall)
	}{
		{
			name: "partial arguments",
			mutate: func(call *genai.FunctionCall) {
				call.PartialArgs = []*genai.PartialArg{{}}
			},
		},
		{
			name: "continuation flag",
			mutate: func(call *genai.FunctionCall) {
				call.WillContinue = &continued
			},
		},
	}
	var calls atomic.Int64
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("unexpected transport")
	})}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := toolResponseHistory("call-a", "lookup", []toolHistoryCall{{id: "call-a", name: "lookup"}})
			test.mutate(request.Contents[1].Parts[0].FunctionCall)
			llm := newChatModel(t, "https://example.test/v1", client, openai.ChatTokenLimitMaxTokens)
			_, err := oneChatResponse(llm, t.Context(), request, false)
			assertChatError(t, err, openai.ChatErrorUnsupportedContent)
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("transport calls = %d, want 0", calls.Load())
	}
}

type toolHistoryCall struct {
	id   string
	name string
}

func toolResponseHistory(responseID, responseName string, calls []toolHistoryCall) *model.LLMRequest {
	parts := make([]*genai.Part, 0, len(calls))
	for _, call := range calls {
		parts = append(parts, &genai.Part{FunctionCall: &genai.FunctionCall{
			ID: call.id, Name: call.name, Args: map[string]any{},
		}})
	}
	return &model.LLMRequest{Contents: []*genai.Content{
		genai.NewContentFromText("use tools", genai.RoleUser),
		{Role: genai.RoleModel, Parts: parts},
		{Role: genai.RoleUser, Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{
			ID: responseID, Name: responseName, Response: map[string]any{"ok": true},
		}}}},
	}}
}

func TestChatCompletionsChoiceAndFinishReasonCompatibility(t *testing.T) {
	responses := make(chan string, 16)
	server := chatServer(t, func(map[string]any) string { return <-responses })
	defer server.Close()
	llm := newChatModel(t, server.URL+"/v1", server.Client(), openai.ChatTokenLimitMaxTokens)
	req := &model.LLMRequest{Contents: []*genai.Content{genai.NewContentFromText("hello", genai.RoleUser)}}

	responses <- `{"id":"empty","choices":[]}`
	_, err := oneChatResponse(llm, t.Context(), req, false)
	assertChatError(t, err, openai.ChatErrorEmptyChoices)
	responses <- `{"id":"many","choices":[{"finish_reason":"stop","message":{"content":"a"}},{"finish_reason":"stop","message":{"content":"b"}}]}`
	_, err = oneChatResponse(llm, t.Context(), req, false)
	assertChatError(t, err, openai.ChatErrorMultipleChoices)

	finishTests := []struct {
		reason string
		want   genai.FinishReason
	}{
		{reason: "stop", want: genai.FinishReasonStop},
		{reason: "tool_calls", want: genai.FinishReasonStop},
		{reason: "content_filter", want: genai.FinishReasonSafety},
		{reason: "length", want: genai.FinishReasonMaxTokens},
		{reason: "unknown", want: genai.FinishReasonOther},
		{reason: "", want: genai.FinishReasonUnspecified},
	}
	for _, test := range finishTests {
		responses <- chatTextResponse(test.reason, "ok")
		response, err := oneChatResponse(llm, t.Context(), req, false)
		if err != nil {
			t.Fatal(err)
		}
		if response.FinishReason != test.want || response.CustomMetadata["openai_finish_reason"] != test.reason {
			t.Fatalf("finish %q => %q metadata %#v", test.reason, response.FinishReason, response.CustomMetadata)
		}
	}
}

func TestChatCompletionsReasoningRemovalIsNarrow(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{name: "complete leading", text: "<think>secret</think>answer", want: "answer"},
		{name: "leading whitespace", text: " \n\t<think>secret</think>answer", want: "answer"},
		{name: "incomplete", text: "<think>secret", want: "<think>secret"},
		{name: "embedded", text: "prefix <think>secret</think>answer", want: "prefix <think>secret</think>answer"},
		{name: "later", text: "answer<think>secret</think>", want: "answer<think>secret</think>"},
		{name: "two blocks", text: "<think>one</think><think>two</think>answer", want: "<think>two</think>answer"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := chatServer(t, func(map[string]any) string {
				return `{"id":"reasoning","model":"m","choices":[{"finish_reason":"stop","message":{"content":` + mustJSON(t, test.text) + `,"reasoning":"ignored","reasoning_content":"ignored"}}]}`
			})
			defer server.Close()
			llm := newChatModel(t, server.URL+"/v1", server.Client(), openai.ChatTokenLimitMaxTokens)
			response, err := oneChatResponse(llm, t.Context(), &model.LLMRequest{
				Contents: []*genai.Content{{Role: genai.RoleModel, Parts: []*genai.Part{
					{Text: "request thought", Thought: true, ThoughtSignature: []byte("ignored")}, {Text: "visible history"},
				}}},
			}, false)
			if err != nil {
				t.Fatal(err)
			}
			if got := responseText(response); got != test.want {
				t.Fatalf("text = %q, want %q", got, test.want)
			}
		})
	}
}

func TestChatCompletionsRejectsStreamingAndInvalidInputsWithoutTransport(t *testing.T) {
	var calls atomic.Int64
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("unexpected transport")
	})}
	llm := newChatModel(t, "https://example.test/v1", client, openai.ChatTokenLimitMaxTokens)
	valid := &model.LLMRequest{Contents: []*genai.Content{genai.NewContentFromText("hello", genai.RoleUser)}}
	_, err := oneChatResponse(llm, t.Context(), valid, true)
	assertChatError(t, err, openai.ChatErrorUnsupportedStreaming)
	_, err = oneChatResponse(llm, t.Context(), nil, false)
	assertChatError(t, err, openai.ChatErrorNilRequest)
	_, err = oneChatResponse(llm, t.Context(), &model.LLMRequest{Contents: []*genai.Content{{
		Role: genai.RoleUser, Parts: []*genai.Part{{InlineData: &genai.Blob{Data: []byte("x")}}},
	}}}, false)
	assertChatError(t, err, openai.ChatErrorUnsupportedContent)
	for _, content := range []*genai.Content{
		{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "hello", InlineData: &genai.Blob{Data: []byte("x")}}}},
		{Role: genai.RoleModel, Parts: []*genai.Part{{
			FunctionCall: &genai.FunctionCall{ID: "call", Name: "lookup", Args: map[string]any{}},
			InlineData:   &genai.Blob{Data: []byte("x")},
		}}},
	} {
		_, err = oneChatResponse(llm, t.Context(), &model.LLMRequest{Contents: []*genai.Content{content}}, false)
		assertChatError(t, err, openai.ChatErrorUnsupportedContent)
	}
	_, err = oneChatResponse(llm, t.Context(), &model.LLMRequest{Contents: []*genai.Content{{
		Role: genai.RoleUser, Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{ID: "unknown", Name: "lookup", Response: map[string]any{}}}},
	}}}, false)
	assertChatError(t, err, openai.ChatErrorUnsupportedContent)
	if calls.Load() != 0 {
		t.Fatalf("transport calls = %d, want 0", calls.Load())
	}
}

func TestChatCompletionsIgnoresHostileEnvironmentWithEmptyKey(t *testing.T) {
	for name, value := range map[string]string{
		"OPENAI_API_KEY": "ambient-api-key", "OPENAI_ADMIN_KEY": "ambient-admin-key",
		"OPENAI_ORG_ID": "ambient-org", "OPENAI_PROJECT_ID": "ambient-project",
		"OPENAI_BASE_URL": "https://ambient.invalid/v1", "OPENAI_WEBHOOK_SECRET": "ambient-webhook",
		"OPENAI_CUSTOM_HEADERS": "Authorization: Bearer ambient-authorization\nX-Ambient: secret",
	} {
		t.Setenv(name, value)
	}
	received := make(chan *http.Request, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		received <- request.Clone(request.Context())
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, chatTextResponse("stop", "ok"))
	}))
	defer server.Close()
	llm := newChatModel(t, server.URL+"/v1?typed=secret#fragment", server.Client(), openai.ChatTokenLimitMaxTokens)
	_, err := oneChatResponse(llm, t.Context(), &model.LLMRequest{Contents: []*genai.Content{genai.NewContentFromText("hello", genai.RoleUser)}}, false)
	if err != nil {
		t.Fatal(err)
	}
	request := <-received
	if request.URL.Path != "/v1/chat/completions" || request.URL.RawQuery != "" {
		t.Fatalf("endpoint = %s?%s", request.URL.Path, request.URL.RawQuery)
	}
	for _, header := range []string{"Authorization", "OpenAI-Organization", "OpenAI-Project", "X-Ambient"} {
		if values := request.Header.Values(header); len(values) != 0 {
			t.Fatalf("%s = %#v, want absent", header, values)
		}
	}
}

func TestChatCompletionsUpstreamFailuresAreRedacted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writeResponse(writer, http.StatusBadGateway, `{"error":{"message":"provider-body-secret"}}`)
	}))
	defer server.Close()
	llm := newChatModel(t, server.URL+"/v1?query-secret=value#fragment-secret", server.Client(), openai.ChatTokenLimitMaxTokens)
	_, err := oneChatResponse(llm, t.Context(), &model.LLMRequest{
		Contents: []*genai.Content{genai.NewContentFromText("hello", genai.RoleUser)},
	}, false)
	var requestError *openai.RequestError
	if !errors.As(err, &requestError) || requestError.Failure != openai.RequestFailureProvider || requestError.StatusCode != http.StatusBadGateway {
		t.Fatalf("error = %T %#v", err, err)
	}
	for _, secret := range []string{"provider-body-secret", "query-secret", "fragment-secret", server.URL} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error exposed %q: %v", secret, err)
		}
	}
}

func TestChatCompletionsUsageClampsToNativeRange(t *testing.T) {
	server := chatServer(t, func(map[string]any) string {
		return `{"id":"usage","model":"m","choices":[{"finish_reason":"stop","message":{"content":"ok"}}],"usage":{"prompt_tokens":9223372036854775807,"completion_tokens":-9223372036854775808,"total_tokens":7}}`
	})
	defer server.Close()
	llm := newChatModel(t, server.URL+"/v1", server.Client(), openai.ChatTokenLimitMaxTokens)
	response, err := oneChatResponse(llm, t.Context(), &model.LLMRequest{Contents: []*genai.Content{genai.NewContentFromText("hello", genai.RoleUser)}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if response.UsageMetadata.PromptTokenCount != math.MaxInt32 || response.UsageMetadata.CandidatesTokenCount != math.MinInt32 || response.UsageMetadata.TotalTokenCount != 7 {
		t.Fatalf("usage = %#v", response.UsageMetadata)
	}
	if response.CustomMetadata["openai_prompt_tokens"] != int64(math.MaxInt64) ||
		response.CustomMetadata["openai_completion_tokens"] != int64(math.MinInt64) ||
		response.CustomMetadata["openai_total_tokens"] != int64(7) {
		t.Fatalf("usage metadata = %#v", response.CustomMetadata)
	}
}

func newChatModel(t *testing.T, baseURL string, client *http.Client, dialect openai.ChatTokenLimitDialect) model.LLM {
	t.Helper()
	llm, err := openai.New(t.Context(), openai.Config{
		Protocol: openai.ProtocolChatCompletions, Model: "configured-model", BaseURL: baseURL,
		HTTPClient: client, MaxResponseBytes: 1 << 20, MaxRetries: 0, ChatTokenLimit: dialect,
	})
	if err != nil {
		t.Fatal(err)
	}
	return llm
}

func oneChatResponse(llm model.LLM, ctx context.Context, request *model.LLMRequest, stream bool) (*model.LLMResponse, error) {
	var response *model.LLMResponse
	for current, err := range llm.GenerateContent(ctx, request, stream) {
		if err != nil {
			return nil, err
		}
		if response != nil {
			return nil, errors.New("model yielded multiple responses")
		}
		response = current
	}
	return response, nil
}

func chatServer(t *testing.T, response func(map[string]any) string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Errorf("decode request: %v", err)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, response(payload))
	}))
}

func chatTextResponse(reason, text string) string {
	encoded, _ := json.Marshal(map[string]any{
		"id": "chat-response", "model": "provider-model",
		"choices": []any{map[string]any{"finish_reason": reason, "message": map[string]any{"content": text}}},
	})
	return string(encoded)
}

func functionCallIDs(response *model.LLMResponse) []string {
	var result []string
	for _, part := range response.Content.Parts {
		if part.FunctionCall != nil {
			result = append(result, part.FunctionCall.ID)
		}
	}
	return result
}

func responseText(response *model.LLMResponse) string {
	var result strings.Builder
	if response == nil || response.Content == nil {
		return ""
	}
	for _, part := range response.Content.Parts {
		if part != nil && !part.Thought {
			result.WriteString(part.Text)
		}
	}
	return result.String()
}

func assertChatError(t *testing.T, err error, kind openai.ChatErrorKind) {
	t.Helper()
	var compatibility *openai.ChatError
	if !errors.As(err, &compatibility) || compatibility.Kind != kind {
		t.Fatalf("error = %T %v, want ChatError %q", err, err, kind)
	}
}

func mustJSON(t *testing.T, value string) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
