package openai

import (
	"encoding/json"
	"errors"
)

const rawChatToolResultKey = "plasmid.openai.raw_chat_tool_result"

var errRawChatToolResultMarker = errors.New("openai: raw Chat tool result marker cannot be serialized")

type rawChatToolResultMarker struct {
	encoded []byte
}

func (rawChatToolResultMarker) MarshalJSON() ([]byte, error) {
	return nil, errRawChatToolResultMarker
}

// RawChatToolResult marks a JSON-compatible value for direct Chat Completions
// conversion or a bounded one-shot run. Responses and durable session
// persistence are unsupported and fail serialization closed.
func RawChatToolResult(value any) (map[string]any, error) {
	encoded, err := marshalRawChatToolResult(value)
	if err != nil {
		return nil, chatError(ChatErrorInvalidToolResult)
	}
	return map[string]any{rawChatToolResultKey: rawChatToolResultMarker{encoded: encoded}}, nil
}

func marshalRawChatToolResult(value any) (encoded []byte, err error) {
	defer func() {
		if recover() != nil {
			encoded = nil
			err = errRawChatToolResultMarker
		}
	}()
	return json.Marshal(value)
}

func markedRawChatToolResult(result map[string]any) ([]byte, bool) {
	if len(result) != 1 {
		return nil, false
	}
	marker, marked := result[rawChatToolResultKey].(rawChatToolResultMarker)
	if !marked {
		return nil, false
	}
	return marker.encoded, true
}
