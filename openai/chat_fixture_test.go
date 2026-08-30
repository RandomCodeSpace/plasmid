package openai_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/RandomCodeSpace/plasmid/internal/fixture"
	"github.com/RandomCodeSpace/plasmid/oneshot"
	"github.com/RandomCodeSpace/plasmid/openai"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"
)

type chatWireFixtureInput struct {
	Environment map[string]string `json:"environment"`
	Scenario    string            `json:"scenario"`
}

func TestOpenAIChatWireFixtures(t *testing.T) {
	fixture.WalkKinds(t, "openai", openAIChatFixtureRunner, []string{"chat-wire"}, func(t *testing.T, testCase fixture.Case) {
		var specification struct {
			Area string `json:"area"`
			ID   string `json:"id"`
			Kind string `json:"kind"`
		}
		var input chatWireFixtureInput
		testCase.Decode(t, "case.json", &specification)
		testCase.Decode(t, "input.json", &input)
		if specification.Area != "openai" || specification.ID != testCase.ID || specification.Kind != "chat-wire" {
			t.Fatalf("invalid fixture identity: %#v", specification)
		}
		for name, value := range input.Environment {
			t.Setenv(name, value)
		}
		result := runChatWireFixture(t, input)
		testCase.CompareJSON(t, "expected.json", result, fixture.Paths{}, fixture.GoldenReadOnly)
	})
}

func runChatWireFixture(t *testing.T, input chatWireFixtureInput) any {
	t.Helper()
	switch input.Scenario {
	case "history":
		return runChatHistoryFixture(t)
	case "validation":
		return runChatValidationFixture(t)
	case "finish-and-reasoning":
		return runChatFinishAndReasoningFixture(t)
	case "hostile-environment":
		return runChatHostileEnvironmentFixture(t)
	case "transport-controls":
		return runChatTransportControlsFixture(t)
	default:
		t.Fatalf("unknown Chat wire fixture scenario %q", input.Scenario)
		return nil
	}
}

func runChatTransportControlsFixture(t *testing.T) any {
	t.Helper()
	request := &model.LLMRequest{
		Contents: []*genai.Content{genai.NewContentFromText("hello", genai.RoleUser)},
		Config:   &genai.GenerateContentConfig{MaxOutputTokens: 11},
	}

	var customAttempts atomic.Int64
	var customRequest map[string]any
	var authorization []string
	var customPath string
	customClient := &http.Client{Transport: roundTripFunc(func(httpRequest *http.Request) (*http.Response, error) {
		customAttempts.Add(1)
		if err := json.NewDecoder(httpRequest.Body).Decode(&customRequest); err != nil {
			return nil, err
		}
		authorization = append([]string{}, httpRequest.Header.Values("Authorization")...)
		customPath = httpRequest.URL.Path
		return chatFixtureHTTPResponse(httpRequest, http.StatusOK, chatTextResponse("stop", "ok"), nil), nil
	})}
	customModel := newChatTransportFixtureModel(t, customClient, 4096, 0)
	customResponse, customErr := oneChatResponse(customModel, t.Context(), request, false)
	if customErr != nil {
		t.Fatal(customErr)
	}

	var retryAttempts atomic.Int64
	retryClient := &http.Client{Transport: roundTripFunc(func(httpRequest *http.Request) (*http.Response, error) {
		retryAttempts.Add(1)
		return chatFixtureHTTPResponse(httpRequest, http.StatusInternalServerError, `{"error":{"message":"unavailable"}}`, http.Header{"Retry-After": {"0"}}), nil
	})}
	retryModel := newChatTransportFixtureModel(t, retryClient, 4096, 0)
	_, retryErr := oneChatResponse(retryModel, t.Context(), request, false)

	chatBody := []byte(chatTextResponse("stop", "oversized"))
	compressedBody := gzipBytes(t, chatBody)
	var capAttempts atomic.Int64
	capClient := &http.Client{Transport: roundTripFunc(func(httpRequest *http.Request) (*http.Response, error) {
		capAttempts.Add(1)
		header := http.Header{"Content-Encoding": {"gzip"}, "Content-Type": {"application/json"}}
		return chatFixtureHTTPResponseBytes(httpRequest, http.StatusOK, compressedBody, header), nil
	})}
	capModel := newChatTransportFixtureModel(t, capClient, int64(len(chatBody)-1), 0)
	_, capErr := oneChatResponse(capModel, t.Context(), request, false)

	var streamAttempts atomic.Int64
	streamClient := &http.Client{Transport: roundTripFunc(func(httpRequest *http.Request) (*http.Response, error) {
		streamAttempts.Add(1)
		return chatFixtureHTTPResponse(httpRequest, http.StatusOK, chatTextResponse("stop", "unexpected"), nil), nil
	})}
	streamModel := newChatTransportFixtureModel(t, streamClient, 4096, 0)
	_, streamErr := oneChatResponse(streamModel, t.Context(), request, true)

	return map[string]any{
		"custom_client": map[string]any{
			"attempts": customAttempts.Load(), "authorization": authorization,
			"max_completion_tokens_present": mapHasKey(customRequest, "max_completion_tokens"),
			"max_tokens":                    customRequest["max_tokens"], "path": customPath, "text": responseText(customResponse),
		},
		"decompressed_cap": map[string]any{"attempts": capAttempts.Load(), "failure": fixtureErrorKind(capErr)},
		"streaming":        map[string]any{"attempts": streamAttempts.Load(), "failure": fixtureChatErrorKind(streamErr)},
		"zero_retries":     map[string]any{"attempts": retryAttempts.Load(), "failure": fixtureErrorKind(retryErr)},
	}
}

func newChatTransportFixtureModel(t *testing.T, client *http.Client, maxResponseBytes int64, maxRetries int) model.LLM {
	t.Helper()
	llm, err := openai.New(t.Context(), openai.Config{
		Protocol: openai.ProtocolChatCompletions, Model: "fixture-model", BaseURL: "https://fixture.invalid/v1",
		APIKey: "", HTTPClient: client, MaxResponseBytes: maxResponseBytes, MaxRetries: maxRetries,
		ChatTokenLimit: openai.ChatTokenLimitMaxTokens,
	})
	if err != nil {
		t.Fatal(err)
	}
	return llm
}

func chatFixtureHTTPResponse(request *http.Request, status int, body string, header http.Header) *http.Response {
	return chatFixtureHTTPResponseBytes(request, status, []byte(body), header)
}

func chatFixtureHTTPResponseBytes(request *http.Request, status int, body []byte, header http.Header) *http.Response {
	if header == nil {
		header = http.Header{"Content-Type": {"application/json"}}
	}
	return &http.Response{
		StatusCode: status, Header: header, Body: io.NopCloser(bytes.NewReader(body)), Request: request,
	}
}

func runChatHistoryFixture(t *testing.T) any {
	t.Helper()
	var calls atomic.Int64
	requests := make(chan map[string]any, 2)
	server := chatServer(t, func(request map[string]any) string {
		requests <- request
		if calls.Add(1) == 1 {
			return `{"id":"fixture-history","model":"provider-model","choices":[{"finish_reason":"tool_calls","message":{"tool_calls":[{"function":{"name":"lookup","arguments":"{\"city\":\"Paris\"}"}}]}}],"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}}`
		}
		return chatTextResponse("stop", "<think>hidden</think>21 C")
	})
	defer server.Close()
	llm := newChatModel(t, server.URL+"/v1", server.Client(), openai.ChatTokenLimitMaxCompletionTokens)
	config := &genai.GenerateContentConfig{
		SystemInstruction: genai.NewContentFromText("Use tools.", "system"), MaxOutputTokens: 64,
		Tools: []*genai.Tool{
			{FunctionDeclarations: []*genai.FunctionDeclaration{
				{
					Name: "lookup", Description: "Look up a city", ParametersJsonSchema: map[string]any{
						"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}},
					},
				},
			}},
		},
		ToolConfig: &genai.ToolConfig{FunctionCallingConfig: &genai.FunctionCallingConfig{Mode: genai.FunctionCallingConfigModeAny}},
	}
	user := genai.NewContentFromText("Weather?", genai.RoleUser)
	first, err := oneChatResponse(llm, t.Context(), &model.LLMRequest{Contents: []*genai.Content{user}, Config: config}, false)
	if err != nil {
		t.Fatal(err)
	}
	id := first.Content.Parts[0].FunctionCall.ID
	result := &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{
		ID: id, Name: "lookup", Response: map[string]any{"temperature_c": 21},
	}}}}
	final, err := oneChatResponse(llm, t.Context(), &model.LLMRequest{Contents: []*genai.Content{user, first.Content, result}, Config: config}, false)
	if err != nil {
		t.Fatal(err)
	}
	firstWire := <-requests
	secondWire := <-requests
	secondMessages := secondWire["messages"].([]any)
	assistant := secondMessages[2].(map[string]any)
	tool := secondMessages[3].(map[string]any)
	declaration := firstWire["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	return map[string]any{
		"first_roles":             fixtureMessageRoles(firstWire),
		"second_roles":            fixtureMessageRoles(secondWire),
		"normalized_id":           id,
		"assistant_tool_call_id":  assistant["tool_calls"].([]any)[0].(map[string]any)["id"],
		"tool_result_id":          tool["tool_call_id"],
		"tool_result":             tool["content"],
		"tool_name":               declaration["name"],
		"tool_description":        declaration["description"],
		"tool_schema":             declaration["parameters"],
		"tool_choice":             firstWire["tool_choice"],
		"max_completion_tokens":   firstWire["max_completion_tokens"],
		"max_tokens_present":      mapHasKey(firstWire, "max_tokens"),
		"final_text":              responseText(final),
		"first_finish_reason":     first.FinishReason,
		"first_response_id":       first.CustomMetadata["openai_response_id"],
		"first_total_token_count": first.UsageMetadata.TotalTokenCount,
	}
}

func runChatValidationFixture(t *testing.T) any {
	t.Helper()
	responses := make(chan string, 16)
	server := chatServer(t, func(map[string]any) string { return <-responses })
	defer server.Close()
	llm := newChatModel(t, server.URL+"/v1", server.Client(), openai.ChatTokenLimitMaxTokens)
	request := &model.LLMRequest{Contents: []*genai.Content{genai.NewContentFromText("hello", genai.RoleUser)}}
	arguments := map[string]string{
		"blank": "", "empty_object": "{}", "malformed": "{", "trailing": "{} {}", "null": "null", "array": "[]", "scalar": "1",
	}
	argumentResults := make(map[string]string, len(arguments))
	for name, raw := range arguments {
		responses <- fixtureToolCallResponse("validation-"+name, "", "function", "lookup", raw)
		response, err := oneChatResponse(llm, t.Context(), request, false)
		if err == nil && response != nil && response.Content.Parts[0].FunctionCall != nil {
			argumentResults[name] = "valid_object"
		} else {
			argumentResults[name] = fixtureChatErrorKind(err)
		}
	}
	duplicateBody := `{"id":"duplicate","choices":[{"finish_reason":"tool_calls","message":{"tool_calls":[{"id":"same","function":{"name":"a","arguments":"{}"}},{"id":"same","function":{"name":"b","arguments":"{}"}}]}}]}`
	responses <- duplicateBody
	firstDuplicate, err := oneChatResponse(llm, t.Context(), request, false)
	if err != nil {
		t.Fatal(err)
	}
	responses <- duplicateBody
	secondDuplicate, err := oneChatResponse(llm, t.Context(), request, false)
	if err != nil {
		t.Fatal(err)
	}
	firstDuplicateIDs := functionCallIDs(firstDuplicate)
	secondDuplicateIDs := functionCallIDs(secondDuplicate)
	if !reflect.DeepEqual(firstDuplicateIDs, secondDuplicateIDs) || len(firstDuplicateIDs) != 2 ||
		firstDuplicateIDs[0] == "" || firstDuplicateIDs[1] == "" || firstDuplicateIDs[0] == firstDuplicateIDs[1] {
		t.Fatalf("normalized duplicate IDs first=%v second=%v", firstDuplicateIDs, secondDuplicateIDs)
	}
	duplicateNames := []string{
		firstDuplicate.Content.Parts[0].FunctionCall.Name,
		firstDuplicate.Content.Parts[1].FunctionCall.Name,
	}
	responses <- fixtureToolCallResponse("unsupported", "call", "custom", "lookup", "{}")
	_, unsupportedErr := oneChatResponse(llm, t.Context(), request, false)
	responses <- fixtureToolCallResponse("missing-name", "call", "function", "", "{}")
	_, missingNameErr := oneChatResponse(llm, t.Context(), request, false)
	responses <- `{"id":"empty","choices":[]}`
	_, emptyErr := oneChatResponse(llm, t.Context(), request, false)
	responses <- `{"id":"many","choices":[{"message":{"content":"a"}},{"message":{"content":"b"}}]}`
	_, multipleErr := oneChatResponse(llm, t.Context(), request, false)
	return map[string]any{
		"arguments":        argumentResults,
		"duplicate_ids":    firstDuplicateIDs,
		"duplicate_names":  duplicateNames,
		"unsupported_type": fixtureChatErrorKind(unsupportedErr),
		"missing_name":     fixtureChatErrorKind(missingNameErr),
		"empty_choices":    fixtureChatErrorKind(emptyErr),
		"mixed_recovery":   runChatMixedRecoveryFixture(t),
		"multiple_choices": fixtureChatErrorKind(multipleErr),
	}
}

func runChatMixedRecoveryFixture(t *testing.T) any {
	t.Helper()
	requests := make(chan map[string]any, 2)
	var calls atomic.Int64
	server := chatServer(t, func(request map[string]any) string {
		requests <- request
		if calls.Add(1) == 1 {
			return `{"id":"mixed","model":"fixture-model","choices":[{"finish_reason":"tool_calls","message":{"tool_calls":[{"id":"same","type":"function","function":{"name":"reject","arguments":"{"}},{"id":"same","type":"function","function":{"name":"accept","arguments":"{\"value\":\"kept\"}"}},{"type":"function","function":{"name":"missing","arguments":"{}"}}]}}]}`
		}
		return chatTextResponse("stop", "done")
	})
	defer server.Close()

	var executed []string
	reject, err := functiontool.NewStreaming[map[string]any](functiontool.Config{
		Name: "reject", Description: "fixture stream",
	}, func(_ agent.Context, _ map[string]any) iter.Seq2[string, error] {
		executed = append(executed, "reject")
		return func(yield func(string, error) bool) { yield("unexpected", nil) }
	})
	if err != nil {
		t.Fatal(err)
	}
	accept, err := functiontool.New[map[string]any, map[string]any](functiontool.Config{
		Name: "accept", Description: "fixture function",
	}, func(_ agent.Context, arguments map[string]any) (map[string]any, error) {
		executed = append(executed, "accept")
		return map[string]any{"value": arguments["value"]}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := oneshot.Run(t.Context(), oneshot.Request{
		Model:       newChatModel(t, server.URL+"/v1", server.Client(), openai.ChatTokenLimitMaxTokens),
		Instruction: "recover safely", Prompt: "recover", Tools: []tool.Tool{reject, accept},
		MaxOutputTokens: 64, MaxReturnedTextBytes: 1024, MaxModelCalls: 2, MaxToolCallsPerResponse: 3,
		ToolExecution: oneshot.ToolExecutionSequential,
	})
	if err != nil {
		t.Fatal(err)
	}
	<-requests
	second := <-requests
	messages := second["messages"].([]any)
	var assistantIDs, assistantNames, toolIDs, toolContents []any
	for _, raw := range messages {
		message := raw.(map[string]any)
		switch message["role"] {
		case "assistant":
			for _, rawCall := range message["tool_calls"].([]any) {
				call := rawCall.(map[string]any)
				assistantIDs = append(assistantIDs, call["id"])
				assistantNames = append(assistantNames, call["function"].(map[string]any)["name"])
			}
		case "tool":
			toolIDs = append(toolIDs, message["tool_call_id"])
			var content map[string]any
			if decodeErr := json.Unmarshal([]byte(message["content"].(string)), &content); decodeErr != nil {
				t.Fatal(decodeErr)
			}
			if value, _ := content["error"].(string); strings.Contains(value, "tool 'missing' not found") {
				content["error"] = "unknown tool"
			}
			toolContents = append(toolContents, content)
		}
	}
	resultNames := make([]string, len(result.ToolResults))
	for index, toolResult := range result.ToolResults {
		resultNames[index] = toolResult.Name
	}
	return map[string]any{
		"assistant_ids":   assistantIDs,
		"assistant_names": assistantNames,
		"executed":        executed,
		"result_names":    resultNames,
		"text":            result.Text,
		"tool_calls":      result.Metadata.ToolCalls,
		"tool_contents":   toolContents,
		"tool_ids":        toolIDs,
	}
}

func runChatFinishAndReasoningFixture(t *testing.T) any {
	t.Helper()
	responses := make(chan string, 32)
	server := chatServer(t, func(map[string]any) string { return <-responses })
	defer server.Close()
	llm := newChatModel(t, server.URL+"/v1", server.Client(), openai.ChatTokenLimitMaxTokens)
	request := &model.LLMRequest{Contents: []*genai.Content{genai.NewContentFromText("hello", genai.RoleUser)}}
	finishes := []string{"stop", "tool_calls", "content_filter", "length", "unknown", ""}
	finishResults := make(map[string]string, len(finishes))
	for _, reason := range finishes {
		responses <- chatTextResponse(reason, "ok")
		response, err := oneChatResponse(llm, t.Context(), request, false)
		if err != nil {
			t.Fatal(err)
		}
		key := reason
		if key == "" {
			key = "missing"
		}
		finishResults[key] = string(response.FinishReason)
	}
	texts := map[string]string{
		"complete":           "<think>secret</think>answer",
		"leading_whitespace": " \n<think>secret</think>answer",
		"incomplete":         "<think>secret",
		"embedded":           "prefix <think>secret</think>answer",
		"later":              "answer<think>secret</think>",
		"two_blocks":         "<think>one</think><think>two</think>answer",
	}
	textResults := make(map[string]string, len(texts))
	for name, text := range texts {
		responses <- chatTextResponse("stop", text)
		response, err := oneChatResponse(llm, t.Context(), request, false)
		if err != nil {
			t.Fatal(err)
		}
		textResults[name] = responseText(response)
	}
	return map[string]any{"finish_reasons": finishResults, "reasoning_removal": textResults}
}

func runChatHostileEnvironmentFixture(t *testing.T) any {
	t.Helper()
	received := make(chan *http.Request, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		received <- request.Clone(request.Context())
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, chatTextResponse("stop", "ok"))
	}))
	defer server.Close()
	llm := newChatModel(t, server.URL+"/v1?typed=secret#fragment", server.Client(), openai.ChatTokenLimitMaxTokens)
	response, err := oneChatResponse(llm, t.Context(), &model.LLMRequest{Contents: []*genai.Content{genai.NewContentFromText("hello", genai.RoleUser)}}, false)
	if err != nil {
		t.Fatal(err)
	}
	request := <-received
	authorization := append([]string{}, request.Header.Values("Authorization")...)
	return map[string]any{
		"method": request.Method, "path": request.URL.Path, "raw_query": request.URL.RawQuery,
		"authorization":           authorization,
		"ambient_headers_present": ambientHeadersPresent(request.Header),
		"text":                    responseText(response),
	}
}

func fixtureMessageRoles(request map[string]any) []string {
	messages := request["messages"].([]any)
	result := make([]string, 0, len(messages))
	for _, message := range messages {
		result = append(result, message.(map[string]any)["role"].(string))
	}
	return result
}

func fixtureToolCallResponse(id, callID, typeName, functionName, arguments string) string {
	payload := map[string]any{
		"id": id, "model": "fixture-model",
		"choices": []any{map[string]any{
			"finish_reason": "tool_calls",
			"message": map[string]any{"tool_calls": []any{map[string]any{
				"id": callID, "type": typeName,
				"function": map[string]any{"name": functionName, "arguments": arguments},
			}}},
		}},
	}
	encoded, _ := json.Marshal(payload)
	return string(encoded)
}

func fixtureChatErrorKind(err error) string {
	var compatibility *openai.ChatError
	if errors.As(err, &compatibility) {
		return string(compatibility.Kind)
	}
	if err == nil {
		return ""
	}
	return "other"
}

func mapHasKey(values map[string]any, key string) bool {
	_, ok := values[key]
	return ok
}
