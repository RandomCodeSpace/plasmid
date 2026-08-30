// Package toolcallrecovery defines private metadata carried from provider
// decoding to a guarded tool execution boundary.
package toolcallrecovery

const (
	MetadataKey             = "plasmid_tool_call_argument_failures"
	InvalidArgumentsMessage = "invalid tool arguments"
)

// Failures maps normalized function-call IDs to model-visible safe errors.
type Failures map[string]string
