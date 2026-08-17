package codingtools

// ToolSpecifierFunc adapts a function to a tool-call specifier.
type ToolSpecifierFunc = func(string, map[string]any) string

// ToolSpecifier returns the primary subject of a known coding-tool call.
func ToolSpecifier(toolName string, input map[string]any) string {
	var key string
	switch toolName {
	case "bash":
		key = "command"
	case "read", "write", "edit", "ls", "find":
		key = "path"
	case "grep":
		key = "pattern"
	default:
		return ""
	}
	value, ok := input[key].(string)
	if !ok {
		return ""
	}
	return value
}
