package codingtools

import (
	"context"
	"encoding/json"

	"github.com/google/jsonschema-go/jsonschema"
	"google.golang.org/adk/v2/agent"
	adktool "google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
)

type nativeHandler[T any] func(context.Context, string, T) (map[string]any, error)

type invocationIDKey struct{}

func invocationID(ctx context.Context) string {
	value, _ := ctx.Value(invocationIDKey{}).(string)
	return value
}

func newNativeTool[T any](name, description string, rawSchema json.RawMessage, handler nativeHandler[T]) (adktool.Tool, error) {
	var inputSchema jsonschema.Schema
	_ = json.Unmarshal(rawSchema, &inputSchema)
	outputSchema := &jsonschema.Schema{Type: "object"}
	return functiontool.New[T, map[string]any](functiontool.Config{
		Name:         name,
		Description:  description,
		InputSchema:  &inputSchema,
		OutputSchema: outputSchema,
	}, func(ctx agent.Context, args T) (map[string]any, error) {
		handlerContext := context.WithValue(ctx, invocationIDKey{}, ctx.FunctionCallID())
		result, err := handler(handlerContext, ctx.SessionID(), args)
		if err != nil {
			return nil, err
		}
		return result, nil
	})
}
