package loop

import (
	"context"
	"encoding/json"
)

// Tool is a provider-neutral callable tool.
type Tool interface {
	Name() string
	Description() string
	InputSchema() json.RawMessage
	Call(context.Context, ToolCall) (ToolResult, error)
}

// Toolset supplies a dynamic tool list for a turn.
type Toolset interface {
	Name() string
	Tools(context.Context, View) ([]Tool, error)
	Close() error
}

// ToolSchemas projects tools to schemas in input order. Schema bytes and the
// returned slice do not alias tool-owned memory.
func ToolSchemas(tools []Tool) []ToolSchema {
	schemas := make([]ToolSchema, len(tools))
	for index, tool := range tools {
		if tool == nil {
			continue
		}
		schema := tool.InputSchema()
		schemas[index] = ToolSchema{
			Name:        tool.Name(),
			Description: tool.Description(),
			InputSchema: append(json.RawMessage(nil), schema...),
		}
	}
	return schemas
}

// FilterTools applies the allow list before the deny list. An explicit allow
// wins when the same name appears in both lists. Original order is retained.
func FilterTools(tools []Tool, view View) []Tool {
	allowed := stringSet(view.AllowedTools)
	disallowed := stringSet(view.DisallowedTools)
	filtered := make([]Tool, 0, len(tools))
	for _, tool := range tools {
		if tool == nil {
			continue
		}
		name := tool.Name()
		_, explicitlyAllowed := allowed[name]
		if len(allowed) != 0 && !explicitlyAllowed {
			continue
		}
		if _, denied := disallowed[name]; denied && !explicitlyAllowed {
			continue
		}
		filtered = append(filtered, tool)
	}
	return filtered
}

func stringSet(values []string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	return set
}
