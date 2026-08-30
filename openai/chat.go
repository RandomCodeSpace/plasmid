package openai

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"iter"
	"math"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	openaisdk "github.com/openai/openai-go/v3"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// ChatErrorKind identifies a stable Chat Completions compatibility failure.
type ChatErrorKind string

const (
	ChatErrorDuplicateToolCallID  ChatErrorKind = "duplicate_tool_call_id"
	ChatErrorEmptyChoices         ChatErrorKind = "empty_choices"
	ChatErrorMalformedArguments   ChatErrorKind = "malformed_arguments"
	ChatErrorMissingFunctionName  ChatErrorKind = "missing_function_name"
	ChatErrorMultipleChoices      ChatErrorKind = "multiple_choices"
	ChatErrorNilRequest           ChatErrorKind = "nil_request"
	ChatErrorUnsupportedContent   ChatErrorKind = "unsupported_content"
	ChatErrorUnsupportedStreaming ChatErrorKind = "unsupported_streaming"
	ChatErrorUnsupportedToolCall  ChatErrorKind = "unsupported_tool_call"
)

// ChatError reports a Chat Completions compatibility failure without wire data.
type ChatError struct {
	Kind ChatErrorKind
}

func (err *ChatError) Error() string {
	return "openai: chat completions " + string(err.Kind)
}

// Is matches Chat errors by kind.
func (err *ChatError) Is(target error) bool {
	want, ok := target.(*ChatError)
	return ok && err != nil && err.Kind == want.Kind
}

type chatModel struct {
	client     *openaisdk.Client
	name       string
	tokenLimit ChatTokenLimitDialect
}

func (modelValue *chatModel) Name() string { return modelValue.name }

func (modelValue *chatModel) GenerateContent(ctx context.Context, request *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if stream {
			yield(nil, chatError(ChatErrorUnsupportedStreaming))
			return
		}
		if request == nil {
			yield(nil, chatError(ChatErrorNilRequest))
			return
		}
		params, err := modelValue.buildRequest(request)
		if err != nil {
			yield(nil, err)
			return
		}
		var wire chatResponse
		if err := modelValue.client.Post(ctx, "chat/completions", params, &wire); err != nil {
			yield(nil, err)
			return
		}
		response, err := convertChatResponse(&wire)
		if err != nil {
			yield(nil, err)
			return
		}
		yield(response, nil)
	}
}

type chatRequest struct {
	Model               string        `json:"model"`
	Messages            []chatMessage `json:"messages"`
	Tools               []chatTool    `json:"tools,omitempty"`
	ToolChoice          any           `json:"tool_choice,omitempty"`
	N                   int           `json:"n"`
	MaxTokens           *int32        `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int32        `json:"max_completion_tokens,omitempty"`
	Temperature         *float32      `json:"temperature,omitempty"`
	TopP                *float32      `json:"top_p,omitempty"`
	Stop                []string      `json:"stop,omitempty"`
	FrequencyPenalty    *float32      `json:"frequency_penalty,omitempty"`
	PresencePenalty     *float32      `json:"presence_penalty,omitempty"`
	Seed                *int32        `json:"seed,omitempty"`
}

type chatMessage struct {
	Role       string                `json:"role"`
	Content    *string               `json:"content,omitempty"`
	ToolCalls  []chatRequestToolCall `json:"tool_calls,omitempty"`
	ToolCallID string                `json:"tool_call_id,omitempty"`
}

type chatRequestToolCall struct {
	ID       string              `json:"id"`
	Type     string              `json:"type"`
	Function chatRequestFunction `json:"function"`
}

type chatRequestFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatTool struct {
	Type     string           `json:"type"`
	Function chatToolFunction `json:"function"`
}

type chatToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters"`
}

func (modelValue *chatModel) buildRequest(request *model.LLMRequest) (chatRequest, error) {
	modelName := modelValue.name
	if request.Model != "" {
		modelName = request.Model
	}
	result := chatRequest{Model: modelName, N: 1}
	config := request.Config
	if config != nil && config.SystemInstruction != nil {
		text, err := chatText(config.SystemInstruction.Parts)
		if err != nil {
			return chatRequest{}, err
		}
		result.Messages = append(result.Messages, chatMessage{Role: "system", Content: stringPointer(text)})
	}

	tracker := chatCallTracker{used: make(map[string]struct{})}
	for _, content := range request.Contents {
		messages, err := tracker.convertContent(content)
		if err != nil {
			return chatRequest{}, err
		}
		result.Messages = append(result.Messages, messages...)
	}
	if len(result.Messages) == 0 {
		return chatRequest{}, chatError(ChatErrorUnsupportedContent)
	}

	if config == nil {
		return result, nil
	}
	if config.TopK != nil || config.CandidateCount > 1 || config.ResponseLogprobs || config.Logprobs != nil ||
		config.ResponseSchema != nil || config.ResponseJsonSchema != nil || config.ResponseMIMEType != "" ||
		config.SafetySettings != nil || config.Labels != nil {
		return chatRequest{}, chatError(ChatErrorUnsupportedContent)
	}
	result.Temperature = config.Temperature
	result.TopP = config.TopP
	result.Stop = append([]string(nil), config.StopSequences...)
	result.FrequencyPenalty = config.FrequencyPenalty
	result.PresencePenalty = config.PresencePenalty
	result.Seed = config.Seed
	if config.MaxOutputTokens > 0 {
		limit := config.MaxOutputTokens
		if modelValue.tokenLimit == ChatTokenLimitMaxTokens {
			result.MaxTokens = &limit
		} else {
			result.MaxCompletionTokens = &limit
		}
	}

	tools, err := convertChatTools(config.Tools)
	if err != nil {
		return chatRequest{}, err
	}
	result.Tools = tools
	choice, err := convertChatToolChoice(config.ToolConfig)
	if err != nil {
		return chatRequest{}, err
	}
	result.ToolChoice = choice
	return result, nil
}

type chatCallTracker struct {
	next    int
	pending []pendingChatCall
	used    map[string]struct{}
}

type pendingChatCall struct {
	id   string
	name string
}

func (tracker *chatCallTracker) convertContent(content *genai.Content) ([]chatMessage, error) {
	if content == nil || len(content.Parts) == 0 {
		return nil, chatError(ChatErrorUnsupportedContent)
	}
	role := content.Role
	if role == "" {
		role = genai.RoleUser
	}
	if role != genai.RoleUser && role != genai.RoleModel && role != "system" && role != "developer" {
		return nil, chatError(ChatErrorUnsupportedContent)
	}

	if role == genai.RoleModel {
		message := chatMessage{Role: "assistant"}
		var text strings.Builder
		for _, part := range content.Parts {
			kind, err := chatPartForm(part)
			if err != nil {
				return nil, err
			}
			switch kind {
			case chatPartFunctionCall:
				call, err := tracker.convertCall(part.FunctionCall)
				if err != nil {
					return nil, err
				}
				message.ToolCalls = append(message.ToolCalls, call)
			case chatPartText:
				if !part.Thought {
					text.WriteString(part.Text)
				}
			default:
				return nil, chatError(ChatErrorUnsupportedContent)
			}
		}
		if text.Len() > 0 {
			message.Content = stringPointer(text.String())
		}
		if message.Content == nil && len(message.ToolCalls) == 0 {
			return nil, chatError(ChatErrorUnsupportedContent)
		}
		return []chatMessage{message}, nil
	}

	var text strings.Builder
	var messages []chatMessage
	flushText := func() {
		if text.Len() == 0 {
			return
		}
		messages = append(messages, chatMessage{Role: role, Content: stringPointer(text.String())})
		text.Reset()
	}
	for _, part := range content.Parts {
		kind, err := chatPartForm(part)
		if err != nil {
			return nil, err
		}
		switch kind {
		case chatPartFunctionResponse:
			if role != genai.RoleUser {
				return nil, chatError(ChatErrorUnsupportedContent)
			}
			flushText()
			message, err := tracker.convertResponse(part.FunctionResponse)
			if err != nil {
				return nil, err
			}
			messages = append(messages, message)
		case chatPartText:
			if !part.Thought {
				text.WriteString(part.Text)
			}
		default:
			return nil, chatError(ChatErrorUnsupportedContent)
		}
	}
	flushText()
	if len(messages) == 0 {
		return nil, chatError(ChatErrorUnsupportedContent)
	}
	return messages, nil
}

func (tracker *chatCallTracker) convertCall(call *genai.FunctionCall) (chatRequestToolCall, error) {
	if call == nil || strings.TrimSpace(call.Name) == "" {
		return chatRequestToolCall{}, chatError(ChatErrorMissingFunctionName)
	}
	if len(call.PartialArgs) != 0 || call.WillContinue != nil {
		return chatRequestToolCall{}, chatError(ChatErrorUnsupportedContent)
	}
	arguments := call.Args
	if arguments == nil {
		arguments = map[string]any{}
	}
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return chatRequestToolCall{}, chatError(ChatErrorUnsupportedContent)
	}
	id := call.ID
	if id == "" {
		id = synthesizedChatCallID("history", tracker.next)
	}
	tracker.next++
	if _, duplicate := tracker.used[id]; duplicate {
		return chatRequestToolCall{}, chatError(ChatErrorDuplicateToolCallID)
	}
	tracker.used[id] = struct{}{}
	tracker.pending = append(tracker.pending, pendingChatCall{id: id, name: call.Name})
	return chatRequestToolCall{
		ID: id, Type: "function",
		Function: chatRequestFunction{Name: call.Name, Arguments: string(encoded)},
	}, nil
}

func (tracker *chatCallTracker) convertResponse(response *genai.FunctionResponse) (chatMessage, error) {
	if response == nil || strings.TrimSpace(response.Name) == "" {
		return chatMessage{}, chatError(ChatErrorMissingFunctionName)
	}
	if len(response.Parts) != 0 || response.WillContinue != nil || response.Scheduling != "" {
		return chatMessage{}, chatError(ChatErrorUnsupportedContent)
	}
	id := response.ID
	match := -1
	if id == "" {
		for index, pending := range tracker.pending {
			if pending.name != response.Name {
				continue
			}
			if match >= 0 {
				return chatMessage{}, chatError(ChatErrorUnsupportedContent)
			}
			match = index
			id = pending.id
		}
	} else {
		for index, pending := range tracker.pending {
			if pending.id == id {
				if pending.name != response.Name {
					return chatMessage{}, chatError(ChatErrorUnsupportedContent)
				}
				match = index
				break
			}
		}
	}
	if match < 0 {
		return chatMessage{}, chatError(ChatErrorUnsupportedContent)
	}
	tracker.pending = append(tracker.pending[:match], tracker.pending[match+1:]...)
	payload := response.Response
	if payload == nil {
		payload = map[string]any{}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return chatMessage{}, chatError(ChatErrorUnsupportedContent)
	}
	return chatMessage{Role: "tool", Content: stringPointer(string(encoded)), ToolCallID: id}, nil
}

func chatText(parts []*genai.Part) (string, error) {
	var text strings.Builder
	for _, part := range parts {
		kind, err := chatPartForm(part)
		if err != nil || kind != chatPartText {
			return "", chatError(ChatErrorUnsupportedContent)
		}
		if part.Thought {
			continue
		}
		text.WriteString(part.Text)
	}
	return text.String(), nil
}

type chatPartKind uint8

const (
	chatPartText chatPartKind = iota + 1
	chatPartFunctionCall
	chatPartFunctionResponse
)

func chatPartForm(part *genai.Part) (chatPartKind, error) {
	if part == nil || part.MediaResolution != nil || part.CodeExecutionResult != nil ||
		part.ExecutableCode != nil || part.FileData != nil || part.InlineData != nil ||
		part.VideoMetadata != nil || part.ToolCall != nil || part.ToolResponse != nil ||
		len(part.PartMetadata) != 0 || part.AudioTranscription != nil {
		return 0, chatError(ChatErrorUnsupportedContent)
	}

	kind := chatPartKind(0)
	forms := 0
	if part.Text != "" {
		kind = chatPartText
		forms++
	}
	if part.FunctionCall != nil {
		kind = chatPartFunctionCall
		forms++
	}
	if part.FunctionResponse != nil {
		kind = chatPartFunctionResponse
		forms++
	}
	if forms != 1 || (part.Thought && kind != chatPartText) {
		return 0, chatError(ChatErrorUnsupportedContent)
	}
	return kind, nil
}

func convertChatTools(tools []*genai.Tool) ([]chatTool, error) {
	var result []chatTool
	for _, tool := range tools {
		if tool == nil || len(tool.FunctionDeclarations) == 0 || hasNonFunctionTool(tool) {
			return nil, chatError(ChatErrorUnsupportedContent)
		}
		for _, declaration := range tool.FunctionDeclarations {
			if declaration == nil || strings.TrimSpace(declaration.Name) == "" {
				return nil, chatError(ChatErrorMissingFunctionName)
			}
			schema, err := chatSchema(declaration)
			if err != nil {
				return nil, err
			}
			result = append(result, chatTool{Type: "function", Function: chatToolFunction{
				Name: declaration.Name, Description: declaration.Description, Parameters: schema,
			}})
		}
	}
	return result, nil
}

func hasNonFunctionTool(tool *genai.Tool) bool {
	return tool.Retrieval != nil || tool.ComputerUse != nil || tool.FileSearch != nil ||
		tool.GoogleSearch != nil || tool.GoogleMaps != nil || tool.CodeExecution != nil ||
		tool.EnterpriseWebSearch != nil || tool.GoogleSearchRetrieval != nil ||
		tool.ParallelAISearch != nil || tool.URLContext != nil || len(tool.MCPServers) != 0 || tool.ExaAISearch != nil
}

func chatSchema(declaration *genai.FunctionDeclaration) (map[string]any, error) {
	if declaration.ParametersJsonSchema != nil {
		return normalizeChatSchema(declaration.ParametersJsonSchema)
	}
	if declaration.Parameters != nil {
		return normalizeChatSchema(declaration.Parameters)
	}
	return map[string]any{"properties": map[string]any{}, "type": "object"}, nil
}

func normalizeChatSchema(schema any) (map[string]any, error) {
	encoded, err := json.Marshal(schema)
	if err != nil {
		return nil, chatError(ChatErrorUnsupportedContent)
	}
	var result map[string]any
	if err := json.Unmarshal(encoded, &result); err != nil || result == nil {
		return nil, chatError(ChatErrorUnsupportedContent)
	}
	lowercaseChatSchemaTypes(result)
	return result, nil
}

func lowercaseChatSchemaTypes(value any) {
	schema, ok := value.(map[string]any)
	if !ok {
		return
	}
	switch schemaType := schema["type"].(type) {
	case string:
		schema["type"] = strings.ToLower(schemaType)
	case []any:
		for index, item := range schemaType {
			if name, ok := item.(string); ok {
				schemaType[index] = strings.ToLower(name)
			}
		}
	}

	for _, keyword := range []string{"properties", "patternProperties", "dependentSchemas", "$defs", "definitions"} {
		children, _ := schema[keyword].(map[string]any)
		for _, child := range children {
			lowercaseChatSchemaTypes(child)
		}
	}
	for _, keyword := range []string{
		"additionalProperties", "unevaluatedProperties", "propertyNames", "contains",
		"if", "then", "else", "not", "additionalItems", "unevaluatedItems",
	} {
		lowercaseChatSchemaTypes(schema[keyword])
	}
	for _, keyword := range []string{"items", "prefixItems", "allOf", "anyOf", "oneOf"} {
		switch children := schema[keyword].(type) {
		case map[string]any:
			lowercaseChatSchemaTypes(children)
		case []any:
			for _, child := range children {
				lowercaseChatSchemaTypes(child)
			}
		}
	}
	if dependencies, ok := schema["dependencies"].(map[string]any); ok {
		for _, child := range dependencies {
			lowercaseChatSchemaTypes(child)
		}
	}
}

func convertChatToolChoice(config *genai.ToolConfig) (any, error) {
	if config == nil || config.FunctionCallingConfig == nil {
		return nil, nil
	}
	calling := config.FunctionCallingConfig
	switch calling.Mode {
	case "", genai.FunctionCallingConfigModeUnspecified, genai.FunctionCallingConfigModeAuto:
		if len(calling.AllowedFunctionNames) == 0 {
			return "auto", nil
		}
		return chatAllowedToolsChoice(calling.AllowedFunctionNames, "auto")
	case genai.FunctionCallingConfigModeNone:
		if len(calling.AllowedFunctionNames) == 0 {
			return "none", nil
		}
	case genai.FunctionCallingConfigModeAny:
		if len(calling.AllowedFunctionNames) == 0 {
			return "required", nil
		}
		return chatAllowedToolsChoice(calling.AllowedFunctionNames, "required")
	}
	return nil, chatError(ChatErrorUnsupportedContent)
}

func chatAllowedToolsChoice(names []string, mode string) (any, error) {
	tools := make([]map[string]any, 0, len(names))
	for _, name := range names {
		if strings.TrimSpace(name) == "" {
			return nil, chatError(ChatErrorUnsupportedContent)
		}
		tools = append(tools, map[string]any{
			"type": "function", "function": map[string]any{"name": name},
		})
	}
	return map[string]any{
		"type":          "allowed_tools",
		"allowed_tools": map[string]any{"mode": mode, "tools": tools},
	}, nil
}

type chatResponse struct {
	ID      string               `json:"id"`
	Model   string               `json:"model"`
	Choices []chatResponseChoice `json:"choices"`
	Usage   chatUsage            `json:"usage"`
}

type chatResponseChoice struct {
	FinishReason string              `json:"finish_reason"`
	Message      chatResponseMessage `json:"message"`
}

type chatResponseMessage struct {
	Content   string                 `json:"content"`
	ToolCalls []chatResponseToolCall `json:"tool_calls"`
}

type chatResponseToolCall struct {
	ID       string               `json:"id"`
	Type     string               `json:"type"`
	Function chatResponseFunction `json:"function"`
}

type chatResponseFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

func convertChatResponse(response *chatResponse) (*model.LLMResponse, error) {
	if response == nil || len(response.Choices) == 0 {
		return nil, chatError(ChatErrorEmptyChoices)
	}
	if len(response.Choices) != 1 {
		return nil, chatError(ChatErrorMultipleChoices)
	}
	choice := response.Choices[0]
	parts := make([]*genai.Part, 0, 1+len(choice.Message.ToolCalls))
	if text := stripLeadingThinkBlock(choice.Message.Content); text != "" {
		parts = append(parts, &genai.Part{Text: text})
	}
	normalizedIDs := normalizeChatResponseCallIDs(response.ID, choice.Message.ToolCalls)
	for index, call := range choice.Message.ToolCalls {
		typeName := call.Type
		if typeName == "" {
			typeName = "function"
		}
		if typeName != "function" {
			return nil, chatError(ChatErrorUnsupportedToolCall)
		}
		if strings.TrimSpace(call.Function.Name) == "" {
			return nil, chatError(ChatErrorMissingFunctionName)
		}
		arguments, err := decodeChatArguments(call.Function.Arguments)
		if err != nil {
			return nil, err
		}
		parts = append(parts, &genai.Part{FunctionCall: &genai.FunctionCall{
			ID: normalizedIDs[index], Name: call.Function.Name, Args: arguments,
		}})
	}
	return &model.LLMResponse{
		Content:      &genai.Content{Role: genai.RoleModel, Parts: parts},
		FinishReason: chatFinishReason(choice.FinishReason),
		ModelVersion: response.Model,
		UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount:     safeChatInt32(response.Usage.PromptTokens),
			CandidatesTokenCount: safeChatInt32(response.Usage.CompletionTokens),
			TotalTokenCount:      safeChatInt32(response.Usage.TotalTokens),
		},
		CustomMetadata: map[string]any{
			"openai_response_id":       response.ID,
			"openai_model":             response.Model,
			"openai_finish_reason":     choice.FinishReason,
			"openai_prompt_tokens":     response.Usage.PromptTokens,
			"openai_completion_tokens": response.Usage.CompletionTokens,
			"openai_total_tokens":      response.Usage.TotalTokens,
		},
	}, nil
}

func decodeChatArguments(raw string) (map[string]any, error) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, chatError(ChatErrorMalformedArguments)
	}
	object, ok := value.(map[string]any)
	if !ok || object == nil {
		return nil, chatError(ChatErrorMalformedArguments)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, chatError(ChatErrorMalformedArguments)
	}
	return object, nil
}

func stripLeadingThinkBlock(text string) string {
	start := 0
	for start < len(text) {
		runeValue, width := utf8.DecodeRuneInString(text[start:])
		if !unicode.IsSpace(runeValue) {
			break
		}
		start += width
	}
	if !strings.HasPrefix(text[start:], "<think>") {
		return text
	}
	end := strings.Index(text[start+len("<think>"):], "</think>")
	if end < 0 {
		return text
	}
	end += start + len("<think>") + len("</think>")
	return text[end:]
}

func normalizeChatResponseCallIDs(responseID string, calls []chatResponseToolCall) []string {
	reserved := make(map[string]struct{}, len(calls))
	for _, call := range calls {
		if call.ID != "" {
			reserved[call.ID] = struct{}{}
		}
	}

	result := make([]string, len(calls))
	used := make(map[string]struct{}, len(calls))
	for index, call := range calls {
		id := call.ID
		if _, duplicate := used[id]; id == "" || duplicate {
			for ordinal := index; ; ordinal++ {
				candidate := synthesizedChatCallID(responseID, ordinal)
				if _, exists := reserved[candidate]; exists {
					continue
				}
				if _, exists := used[candidate]; exists {
					continue
				}
				id = candidate
				break
			}
		}
		result[index] = id
		used[id] = struct{}{}
	}
	return result
}

func synthesizedChatCallID(responseID string, ordinal int) string {
	digest := sha256.Sum256([]byte(responseID + "\x00" + strconv.Itoa(ordinal)))
	return "plasmid-chat-" + hex.EncodeToString(digest[:12])
}

func chatFinishReason(reason string) genai.FinishReason {
	switch reason {
	case "stop", "tool_calls", "function_call":
		return genai.FinishReasonStop
	case "length":
		return genai.FinishReasonMaxTokens
	case "content_filter":
		return genai.FinishReasonSafety
	case "":
		return genai.FinishReasonUnspecified
	default:
		return genai.FinishReasonOther
	}
}

func safeChatInt32(value int64) int32 {
	if value > math.MaxInt32 {
		return math.MaxInt32
	}
	if value < math.MinInt32 {
		return math.MinInt32
	}
	return int32(value)
}

func stringPointer(value string) *string { return &value }

func chatError(kind ChatErrorKind) *ChatError { return &ChatError{Kind: kind} }

var _ model.LLM = (*chatModel)(nil)
