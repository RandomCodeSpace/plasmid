package adkloop

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"google.golang.org/genai"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	adktool "google.golang.org/adk/v2/tool"

	"github.com/plasmid-dev/plasmid/loop"
)

func applyHookConfig(config *llmagent.Config, hooks loop.Hooks) {
	if len(hooks.BeforeModel) > 0 {
		config.BeforeModelCallbacks = []llmagent.BeforeModelCallback{func(ctx agent.Context, request *model.LLMRequest) (*model.LLMResponse, error) {
			projected, snapshot, err := modelRequestToLoop(request)
			if err != nil {
				return nil, err
			}
			replacement, hookErr := hooks.RunBeforeModel(ctx, projected)
			applyErr := applyModelRequest(request, projected, snapshot)
			var native *model.LLMResponse
			var conversionErr error
			if replacement != nil {
				native, conversionErr = loopResponseToADK(replacement, nil)
			}
			return native, errors.Join(hookErr, applyErr, conversionErr)
		}}
	}
	if len(hooks.AfterModel) > 0 {
		config.AfterModelCallbacks = []llmagent.AfterModelCallback{func(ctx agent.Context, response *model.LLMResponse, responseErr error) (*model.LLMResponse, error) {
			projected, err := modelResponseToLoop(response, responseErr)
			if err != nil {
				return nil, err
			}
			snapshot := cloneLoopModelResponse(projected)
			replacement, hookErr := hooks.RunAfterModel(ctx, projected, responseErr)
			native, conversionErr := loopResponseToADK(replacement, snapshot)
			return native, errors.Join(hookErr, conversionErr)
		}}
		config.OnModelErrorCallbacks = []llmagent.OnModelErrorCallback{func(ctx agent.Context, _ *model.LLMRequest, responseErr error) (*model.LLMResponse, error) {
			replacement, hookErr := hooks.RunAfterModel(ctx, nil, responseErr)
			native, conversionErr := loopResponseToADK(replacement, nil)
			return native, errors.Join(hookErr, conversionErr)
		}}
	}
	if len(hooks.BeforeTool) > 0 {
		config.BeforeToolCallbacks = []llmagent.BeforeToolCallback{func(ctx agent.Context, tool adktool.Tool, args map[string]any) (map[string]any, error) {
			call := &loop.ToolCall{
				ID:           ctx.FunctionCallID(),
				Name:         tool.Name(),
				Args:         args,
				SessionID:    ctx.SessionID(),
				InvocationID: ctx.InvocationID(),
			}
			identity := *call
			result, hookErr := hooks.RunBeforeTool(ctx, call)
			if call.ID != identity.ID || call.Name != identity.Name || call.SessionID != identity.SessionID || call.InvocationID != identity.InvocationID {
				return nil, errors.Join(hookErr, ErrImmutableHookField)
			}
			updatedArgs := cloneMap(call.Args)
			clear(args)
			for key, value := range updatedArgs {
				args[key] = value
			}
			if result == nil {
				return nil, hookErr
			}
			if result.CallID != "" && result.CallID != identity.ID {
				return nil, errors.Join(hookErr, fmt.Errorf("%w: before-tool result call ID changed", ErrImmutableHookField))
			}
			content, contentErr := adkToolResultContent(*result, nil)
			return content, errors.Join(hookErr, contentErr)
		}}
	}
	if len(hooks.AfterTool) > 0 {
		config.AfterToolCallbacks = []llmagent.AfterToolCallback{func(ctx agent.Context, tool adktool.Tool, args, result map[string]any, toolErr error) (map[string]any, error) {
			call := &loop.ToolCall{
				ID:           ctx.FunctionCallID(),
				Name:         tool.Name(),
				Args:         args,
				SessionID:    ctx.SessionID(),
				InvocationID: ctx.InvocationID(),
			}
			identity := *call
			_, structuredError := result["error"]
			projected := &loop.ToolResult{CallID: call.ID, Content: result, IsError: toolErr != nil || structuredError}
			replacement, hookErr := hooks.RunAfterTool(ctx, call, projected, toolErr)
			if call.ID != identity.ID || call.Name != identity.Name || call.SessionID != identity.SessionID || call.InvocationID != identity.InvocationID {
				return nil, errors.Join(hookErr, ErrImmutableHookField)
			}
			if replacement == nil {
				return nil, hookErr
			}
			if replacement.CallID != "" && replacement.CallID != identity.ID {
				return nil, errors.Join(hookErr, fmt.Errorf("%w: after-tool result call ID changed", ErrImmutableHookField))
			}
			content, contentErr := adkToolResultContent(*replacement, nil)
			return content, errors.Join(hookErr, contentErr)
		}}
	}
}

type requestSnapshot struct {
	model    string
	system   string
	messages []loop.Message
	tools    []loop.ToolSchema
}

func modelRequestToLoop(request *model.LLMRequest) (*loop.ModelRequest, requestSnapshot, error) {
	if request == nil {
		return nil, requestSnapshot{}, fmt.Errorf("%w: nil model request", ErrFidelity)
	}
	projected := &loop.ModelRequest{Model: request.Model, Raw: request}
	if request.Config != nil && request.Config.SystemInstruction != nil {
		message, _ := contentToMessage(request.Config.SystemInstruction)
		projected.System = message.Text
	}
	projected.Messages = make([]loop.Message, len(request.Contents))
	for index, content := range request.Contents {
		message, _ := contentToMessage(content)
		projected.Messages[index] = message
	}
	if request.Config != nil {
		for _, group := range request.Config.Tools {
			if group == nil {
				continue
			}
			for _, declaration := range group.FunctionDeclarations {
				if declaration == nil {
					continue
				}
				input := declaration.ParametersJsonSchema
				if input == nil {
					input = declaration.Parameters
				}
				encoded, err := json.Marshal(input)
				if err != nil {
					return nil, requestSnapshot{}, fmt.Errorf("marshal tool schema %q: %w", declaration.Name, err)
				}
				projected.Tools = append(projected.Tools, loop.ToolSchema{Name: declaration.Name, Description: declaration.Description, InputSchema: encoded})
			}
		}
	}
	snapshot := requestSnapshot{
		model:    projected.Model,
		system:   projected.System,
		messages: cloneMessages(projected.Messages),
		tools:    cloneToolSchemas(projected.Tools),
	}
	return projected, snapshot, nil
}

func applyModelRequest(native *model.LLMRequest, projected *loop.ModelRequest, snapshot requestSnapshot) error {
	if projected.Raw != native {
		return fmt.Errorf("%w: model request Raw was replaced", ErrFidelity)
	}
	native.Model = projected.Model
	if !reflect.DeepEqual(projected.Tools, snapshot.tools) {
		return fmt.Errorf("%w: model hook changed tool declarations", ErrFidelity)
	}
	if projected.System != snapshot.system {
		if native.Config == nil {
			native.Config = &genai.GenerateContentConfig{}
		}
		if native.Config.SystemInstruction != nil {
			_, unsupported := contentToMessage(native.Config.SystemInstruction)
			if unsupported {
				return fmt.Errorf("%w: cannot replace multimodal system instruction", ErrFidelity)
			}
		}
		native.Config.SystemInstruction = genai.NewContentFromText(projected.System, genai.Role("system"))
	}
	if len(projected.Messages) != len(snapshot.messages) {
		contents := make([]*genai.Content, len(projected.Messages))
		for index, message := range projected.Messages {
			content, err := messageToContent(message)
			if err != nil {
				return fmt.Errorf("message %d: %w", index, err)
			}
			contents[index] = content
		}
		native.Contents = contents
		return nil
	}
	for index := range projected.Messages {
		if messagePortableEqual(projected.Messages[index], snapshot.messages[index]) {
			continue
		}
		content, err := messageToContent(projected.Messages[index])
		if err != nil {
			return fmt.Errorf("message %d: %w", index, err)
		}
		if index >= len(native.Contents) {
			return fmt.Errorf("%w: Raw model request changed message count during portable mutation", ErrFidelity)
		}
		native.Contents[index] = content
	}
	return nil
}

func modelResponseToLoop(response *model.LLMResponse, responseErr error) (*loop.ModelResponse, error) {
	if response == nil {
		return nil, nil
	}
	message, _ := contentToMessage(response.Content)
	return &loop.ModelResponse{
		Message: message,
		Usage:   toLoopUsage(response.UsageMetadata),
		Err:     responseErr,
		Raw:     response,
	}, nil
}

func loopResponseToADK(response, original *loop.ModelResponse) (*model.LLMResponse, error) {
	if response == nil {
		return nil, nil
	}
	if original != nil && modelResponsePortableEqual(response, original) {
		if native, ok := response.Raw.(*model.LLMResponse); ok {
			return native, nil
		}
	}

	var native *model.LLMResponse
	switch raw := response.Raw.(type) {
	case nil:
		native = &model.LLMResponse{}
	case *model.LLMResponse:
		clone, err := cloneModelResponse(raw)
		if err != nil {
			return nil, err
		}
		native = clone
	case json.RawMessage:
		native = &model.LLMResponse{}
		if err := json.Unmarshal(raw, native); err != nil {
			return nil, fmt.Errorf("decode model response Raw: %w", err)
		}
	default:
		return nil, fmt.Errorf("%w: unsupported model response Raw type %T", ErrFidelity, response.Raw)
	}
	content, err := messageToContent(response.Message)
	if err != nil {
		return nil, err
	}
	native.Content = content
	native.UsageMetadata = toADKUsage(response.Usage)
	return native, nil
}

func contentToMessage(content *genai.Content) (loop.Message, bool) {
	if content == nil {
		return loop.Message{}, false
	}
	message := loop.Message{Role: fromADKRole(content.Role), Raw: content}
	unsupported := false
	textParts := 0
	for _, part := range content.Parts {
		if part == nil {
			unsupported = true
			continue
		}
		encoded, err := json.Marshal(part)
		if err != nil {
			unsupported = true
			continue
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(encoded, &fields) != nil {
			unsupported = true
			continue
		}
		switch {
		case part.FunctionCall != nil && len(fields) == 1:
			message.ToolCalls = append(message.ToolCalls, loop.ToolCall{ID: part.FunctionCall.ID, Name: part.FunctionCall.Name, Args: cloneMap(part.FunctionCall.Args)})
		case part.FunctionResponse != nil && len(fields) == 1:
			_, isError := part.FunctionResponse.Response["error"]
			message.ToolResults = append(message.ToolResults, loop.ToolResult{CallID: part.FunctionResponse.ID, Content: cloneMap(part.FunctionResponse.Response), IsError: isError})
		case part.Text != "" && len(fields) == 1:
			message.Text += part.Text
			textParts++
		default:
			unsupported = true
		}
	}
	if textParts > 1 {
		unsupported = true
	}
	return message, unsupported
}

func messageToContent(message loop.Message) (*genai.Content, error) {
	if message.Raw != nil {
		var rawContent *genai.Content
		switch raw := message.Raw.(type) {
		case *genai.Content:
			rawContent = raw
		case json.RawMessage:
			rawContent = &genai.Content{}
			if err := json.Unmarshal(raw, rawContent); err != nil {
				return nil, fmt.Errorf("decode message Raw: %w", err)
			}
		default:
			return nil, fmt.Errorf("%w: unsupported message Raw type %T", ErrFidelity, message.Raw)
		}
		projection, unsupported := contentToMessage(rawContent)
		if messagePortableEqual(message, projection) {
			return cloneContent(rawContent)
		}
		if unsupported {
			return nil, fmt.Errorf("%w: cannot mutate content with unprojected parts", ErrFidelity)
		}
	}
	if message.ApproxTokens != 0 {
		return nil, fmt.Errorf("%w: approximate token count has no ADK content field", ErrFidelity)
	}
	if len(message.ToolResults) != 0 {
		return nil, fmt.Errorf("%w: portable tool results do not carry function names", ErrFidelity)
	}
	role, err := toADKRole(message.Role)
	if err != nil {
		return nil, err
	}
	parts := make([]*genai.Part, 0, 1+len(message.ToolCalls))
	if message.Text != "" {
		parts = append(parts, genai.NewPartFromText(message.Text))
	}
	for _, call := range message.ToolCalls {
		if call.Name == "" {
			return nil, fmt.Errorf("%w: portable tool call has no name", ErrFidelity)
		}
		parts = append(parts, &genai.Part{FunctionCall: &genai.FunctionCall{ID: call.ID, Name: call.Name, Args: cloneMap(call.Args)}})
	}
	return &genai.Content{Role: string(role), Parts: parts}, nil
}

func fromADKRole(role string) loop.Role {
	switch role {
	case genai.RoleUser:
		return loop.RoleUser
	case genai.RoleModel:
		return loop.RoleAssistant
	default:
		return loop.Role(role)
	}
}

func toADKRole(role loop.Role) (genai.Role, error) {
	switch role {
	case "", loop.RoleUser:
		return genai.RoleUser, nil
	case loop.RoleAssistant:
		return genai.RoleModel, nil
	default:
		return "", fmt.Errorf("%w: role %q has no ADK content role", ErrFidelity, role)
	}
}

func cloneContent(content *genai.Content) (*genai.Content, error) {
	if content == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(content)
	if err != nil {
		return nil, fmt.Errorf("clone content: %w", err)
	}
	clone := &genai.Content{}
	if err := json.Unmarshal(encoded, clone); err != nil {
		return nil, fmt.Errorf("clone content: %w", err)
	}
	return clone, nil
}

func cloneModelResponse(response *model.LLMResponse) (*model.LLMResponse, error) {
	encoded, err := json.Marshal(response)
	if err != nil {
		return nil, fmt.Errorf("clone model response: %w", err)
	}
	clone := &model.LLMResponse{}
	if err := json.Unmarshal(encoded, clone); err != nil {
		return nil, fmt.Errorf("clone model response: %w", err)
	}
	return clone, nil
}

func cloneMessages(messages []loop.Message) []loop.Message {
	clones := make([]loop.Message, len(messages))
	for index, message := range messages {
		clones[index] = message
		clones[index].ToolCalls = append([]loop.ToolCall(nil), message.ToolCalls...)
		for callIndex := range clones[index].ToolCalls {
			clones[index].ToolCalls[callIndex].Args = cloneMap(clones[index].ToolCalls[callIndex].Args)
		}
		clones[index].ToolResults = append([]loop.ToolResult(nil), message.ToolResults...)
		for resultIndex := range clones[index].ToolResults {
			clones[index].ToolResults[resultIndex].Content = cloneMap(clones[index].ToolResults[resultIndex].Content)
		}
	}
	return clones
}

func cloneToolSchemas(schemas []loop.ToolSchema) []loop.ToolSchema {
	clones := append([]loop.ToolSchema(nil), schemas...)
	for index := range clones {
		clones[index].InputSchema = append(json.RawMessage(nil), clones[index].InputSchema...)
	}
	return clones
}

func messagePortableEqual(left, right loop.Message) bool {
	left.Raw = nil
	right.Raw = nil
	return reflect.DeepEqual(left, right)
}

func modelResponsePortableEqual(left, right *loop.ModelResponse) bool {
	if left == nil || right == nil {
		return left == right
	}
	return messagePortableEqual(left.Message, right.Message) && reflect.DeepEqual(left.Usage, right.Usage) && errors.Is(left.Err, right.Err)
}

func cloneLoopModelResponse(response *loop.ModelResponse) *loop.ModelResponse {
	if response == nil {
		return nil
	}
	clone := *response
	clone.Message = cloneMessages([]loop.Message{response.Message})[0]
	if response.Usage != nil {
		usage := *response.Usage
		clone.Usage = &usage
	}
	return &clone
}
