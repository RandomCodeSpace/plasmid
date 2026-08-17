package adkloop

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"

	"github.com/google/jsonschema-go/jsonschema"

	"google.golang.org/adk/v2/agent"
	adktool "google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/plasmid-dev/plasmid/loop"
)

func newToolBridge(core loop.Tool) (adktool.Tool, error) {
	schemaBytes := append(json.RawMessage(nil), core.InputSchema()...)
	trimmedSchema := bytes.TrimSpace(schemaBytes)
	if len(trimmedSchema) == 0 {
		return nil, fmt.Errorf("input schema is empty")
	}
	if bytes.Equal(trimmedSchema, []byte("null")) {
		return nil, fmt.Errorf("input schema is null")
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(schemaBytes, &schema); err != nil {
		return nil, fmt.Errorf("decode input schema: %w", err)
	}

	return functiontool.New[map[string]any, map[string]any](functiontool.Config{
		Name:        core.Name(),
		Description: core.Description(),
		InputSchema: &schema,
	}, func(ctx agent.Context, args map[string]any) (map[string]any, error) {
		callID := ctx.FunctionCallID()
		result, err := core.Call(ctx, loop.ToolCall{
			ID:           callID,
			Name:         core.Name(),
			Args:         args,
			SessionID:    ctx.SessionID(),
			InvocationID: ctx.InvocationID(),
		})
		if result.CallID != "" && result.CallID != callID {
			return nil, fmt.Errorf("%w: tool result call ID %q does not match %q", ErrFidelity, result.CallID, callID)
		}
		if result.IsError {
			if len(result.Content) == 0 && err != nil {
				return nil, err
			}
			content, contentErr := adkToolResultContent(result, err)
			if contentErr != nil {
				return nil, fmt.Errorf("tool %q: %w", core.Name(), contentErr)
			}
			return content, nil
		}
		if err != nil {
			return nil, err
		}
		content, contentErr := adkToolResultContent(result, nil)
		if contentErr != nil {
			return nil, fmt.Errorf("tool %q: %w", core.Name(), contentErr)
		}
		return content, nil
	})
}

func adkToolResultContent(result loop.ToolResult, cause error) (map[string]any, error) {
	if result.Content == nil {
		return nil, fmt.Errorf("%w: tool returned nil object content", ErrFidelity)
	}
	if !result.IsError {
		return result.Content, nil
	}
	if len(result.Content) == 0 {
		return nil, fmt.Errorf("%w: tool returned an empty error result without a Go error", ErrFidelity)
	}
	content := cloneMap(result.Content)
	if _, exists := content["error"]; !exists {
		if cause != nil {
			content["error"] = cause.Error()
		} else {
			content["error"] = true
		}
	}
	return content, nil
}

type toolsetBridge struct {
	core     loop.Toolset
	reserved map[string]struct{}
}

func (b *toolsetBridge) Name() string { return b.core.Name() }

func (b *toolsetBridge) Tools(ctx agent.ReadonlyContext) ([]adktool.Tool, error) {
	coreTools, err := b.core.Tools(ctx, loop.View{
		SessionID:    ctx.SessionID(),
		InvocationID: ctx.InvocationID(),
	})
	if err != nil {
		return nil, err
	}
	seen := maps.Clone(b.reserved)
	bridged := make([]adktool.Tool, 0, len(coreTools))
	for index, coreTool := range coreTools {
		if nilInterface(coreTool) {
			return nil, fmt.Errorf("%w: toolset %q returned nil tool %d", ErrInvalidConfig, b.Name(), index)
		}
		name := coreTool.Name()
		if name == "" {
			return nil, fmt.Errorf("%w: toolset %q returned an empty tool name", ErrInvalidConfig, b.Name())
		}
		if _, duplicate := seen[name]; duplicate {
			return nil, fmt.Errorf("%w: duplicate dynamic tool name %q", ErrInvalidConfig, name)
		}
		seen[name] = struct{}{}
		tool, err := newToolBridge(coreTool)
		if err != nil {
			return nil, fmt.Errorf("toolset %q tool %q: %w", b.Name(), name, err)
		}
		bridged = append(bridged, tool)
	}
	return bridged, nil
}
