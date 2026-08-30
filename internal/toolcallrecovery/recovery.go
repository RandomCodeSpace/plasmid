// Package toolcallrecovery defines private metadata carried from provider
// decoding to a guarded tool execution boundary.
package toolcallrecovery

const (
	MetadataKey             = "plasmid_tool_call_argument_failures"
	InvalidArgumentsMessage = "invalid tool arguments"
	RequestToolKey          = "plasmid.internal/tool-call-recovery"
)

// Failures maps normalized function-call IDs to model-visible safe errors.
type Failures map[string]string

// RequestMarker is implemented by providers that can opt a request into
// call-level argument recovery without changing their public model API.
type RequestMarker interface {
	MarkToolCallRecovery(map[string]any)
}

// RequestTool is the unadvertised inert marker carried in a capable request.
type RequestTool struct{}

func (RequestTool) Name() string        { return RequestToolKey }
func (RequestTool) Description() string { return "" }
func (RequestTool) IsLongRunning() bool { return false }
