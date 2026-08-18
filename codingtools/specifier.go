package codingtools

// ToolSpecifierFunc adapts a function to a tool-call specifier.
type ToolSpecifierFunc = func(string, map[string]any) string

const (
	bashToolName  = "bash"
	readToolName  = "read"
	writeToolName = "write"
	editToolName  = "edit"
	listToolName  = "ls"
	findToolName  = "find"
	grepToolName  = "grep"
)

// ToolSpecifier returns the primary subject of a known coding-tool call.
func ToolSpecifier(toolName string, input map[string]any) string {
	var key string
	switch toolName {
	case bashToolName:
		key = "command"
	case readToolName, writeToolName, editToolName, listToolName, findToolName:
		key = "path"
	case grepToolName:
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
