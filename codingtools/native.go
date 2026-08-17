package codingtools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
	"google.golang.org/adk/v2/agent"
	adktool "google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
)

type nativeHandler func(context.Context, string, map[string]any) (map[string]any, error)

func newNativeTool(name, description string, rawSchema json.RawMessage, handler nativeHandler) (adktool.Tool, error) {
	if name == "" {
		return nil, errors.New("construct native coding tool: name must not be empty")
	}
	if handler == nil {
		return nil, fmt.Errorf("construct native coding tool %q: handler is required", name)
	}
	var inputSchema jsonschema.Schema
	if err := json.Unmarshal(rawSchema, &inputSchema); err != nil {
		return nil, fmt.Errorf("construct native coding tool %q: decode input schema: %w", name, err)
	}
	outputSchema := &jsonschema.Schema{Type: "object"}
	return functiontool.New[map[string]any, map[string]any](functiontool.Config{
		Name:         name,
		Description:  description,
		InputSchema:  &inputSchema,
		OutputSchema: outputSchema,
	}, func(ctx agent.Context, args map[string]any) (map[string]any, error) {
		result, err := handler(ctx, ctx.SessionID(), args)
		if err != nil {
			return nil, err
		}
		if result == nil {
			return nil, fmt.Errorf("coding tool %q returned a nil result object", name)
		}
		return result, nil
	})
}
