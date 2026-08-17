package loop

import "context"

// BeforeModelHook may mutate req. A non-nil response short-circuits the chain.
type BeforeModelHook func(context.Context, *ModelRequest) (*ModelResponse, error)

// AfterModelHook may replace resp. Replacements flow through later hooks.
type AfterModelHook func(context.Context, *ModelResponse, error) (*ModelResponse, error)

// BeforeToolHook may mutate call. A non-nil result short-circuits the chain.
type BeforeToolHook func(context.Context, *ToolCall) (*ToolResult, error)

// AfterToolHook may replace res. Replacements flow through later hooks.
type AfterToolHook func(context.Context, *ToolCall, *ToolResult, error) (*ToolResult, error)

// Hooks groups the four normative interception chains.
type Hooks struct {
	BeforeModel []BeforeModelHook
	AfterModel  []AfterModelHook
	BeforeTool  []BeforeToolHook
	AfterTool   []AfterToolHook
}

// Merge returns receiver hooks followed by other hooks in every slot. Every
// returned slice has independent backing storage.
func (h Hooks) Merge(other Hooks) Hooks {
	return Hooks{
		BeforeModel: mergeHooks(h.BeforeModel, other.BeforeModel),
		AfterModel:  mergeHooks(h.AfterModel, other.AfterModel),
		BeforeTool:  mergeHooks(h.BeforeTool, other.BeforeTool),
		AfterTool:   mergeHooks(h.AfterTool, other.AfterTool),
	}
}

func mergeHooks[H any](left, right []H) []H {
	merged := make([]H, 0, len(left)+len(right))
	merged = append(merged, left...)
	merged = append(merged, right...)
	return merged
}

// RunBeforeModel executes the before-model chain.
func (h Hooks) RunBeforeModel(ctx context.Context, request *ModelRequest) (*ModelResponse, error) {
	for _, hook := range h.BeforeModel {
		if hook == nil {
			continue
		}
		response, err := hook(ctx, request)
		if err != nil || response != nil {
			return response, err
		}
	}
	return nil, nil
}

// RunAfterModel executes the after-model chain.
func (h Hooks) RunAfterModel(ctx context.Context, response *ModelResponse, runErr error) (*ModelResponse, error) {
	current := response
	currentErr := runErr
	for _, hook := range h.AfterModel {
		if hook == nil {
			continue
		}
		replacement, err := hook(ctx, current, currentErr)
		if replacement != nil {
			current = replacement
		}
		if err != nil {
			return current, err
		}
	}
	return current, currentErr
}

// RunBeforeTool executes the before-tool chain.
func (h Hooks) RunBeforeTool(ctx context.Context, call *ToolCall) (*ToolResult, error) {
	for _, hook := range h.BeforeTool {
		if hook == nil {
			continue
		}
		result, err := hook(ctx, call)
		if err != nil || result != nil {
			return result, err
		}
	}
	return nil, nil
}

// RunAfterTool executes the after-tool chain.
func (h Hooks) RunAfterTool(ctx context.Context, call *ToolCall, result *ToolResult, runErr error) (*ToolResult, error) {
	current := result
	currentErr := runErr
	for _, hook := range h.AfterTool {
		if hook == nil {
			continue
		}
		replacement, err := hook(ctx, call, current, currentErr)
		if replacement != nil {
			current = replacement
		}
		if err != nil {
			return current, err
		}
	}
	return current, currentErr
}
