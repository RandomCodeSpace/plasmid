package adkloop

import (
	"encoding/json"
	"fmt"
	"maps"

	"google.golang.org/genai"

	"google.golang.org/adk/v2/session"

	"github.com/plasmid-dev/plasmid/loop"
	warningpkg "github.com/plasmid-dev/plasmid/warning"
)

func toLoopEvents(sessionID string, event *session.Event) ([]loop.Event, error) {
	if event == nil {
		return nil, fmt.Errorf("%w: nil ADK event", ErrFidelity)
	}
	raw, err := json.Marshal(event)
	if err != nil {
		return nil, fmt.Errorf("marshal ADK event: %w", err)
	}
	usage := toLoopUsage(event.UsageMetadata)
	base := loop.Event{
		ID:             event.ID,
		SessionID:      sessionID,
		InvocationID:   event.InvocationID,
		Branch:         event.Branch,
		IsolationScope: event.IsolationScope,
		Author:         event.Author,
		StateDelta:     maps.Clone(event.Actions.StateDelta),
		Usage:          usage,
		Final:          isFinalResponse(event),
		Timestamp:      event.Timestamp,
	}
	withRaw := func(item loop.Event) loop.Event {
		item.Raw = append(json.RawMessage(nil), raw...)
		return item
	}
	warning := func(code, message string) loop.Event {
		value := warningpkg.Warning{Code: code, Source: "adkloop", Message: message}
		item := base
		item.Kind = loop.EventWarning
		item.Warning = &value
		return withRaw(item)
	}

	events := make([]loop.Event, 0, 3)
	if event.ErrorCode != "" || event.ErrorMessage != "" {
		message := event.ErrorMessage
		if event.ErrorCode != "" {
			if message == "" {
				message = event.ErrorCode
			} else {
				message = event.ErrorCode + ": " + message
			}
		}
		item := base
		item.Kind = loop.EventError
		item.Text = message
		events = append(events, withRaw(item))
	}

	if event.Content != nil {
		for index, part := range event.Content.Parts {
			if part == nil {
				events = append(events, warning(warningpkg.WarnADKEventMalformed, fmt.Sprintf("part %d is nil", index)))
				continue
			}
			partJSON, err := json.Marshal(part)
			if err != nil {
				events = append(events, warning(warningpkg.WarnADKEventMalformed, fmt.Sprintf("part %d cannot be encoded: %v", index, err)))
				continue
			}
			var partFields map[string]json.RawMessage
			if err := json.Unmarshal(partJSON, &partFields); err != nil {
				events = append(events, warning(warningpkg.WarnADKEventMalformed, fmt.Sprintf("part %d cannot be inspected: %v", index, err)))
				continue
			}
			if part.Thought && part.FunctionCall == nil && part.FunctionResponse == nil {
				events = append(events, warning(warningpkg.WarnADKEventUnknownPart, fmt.Sprintf("part %d has no portable text or function projection", index)))
				continue
			}
			if len(partFields) > 1 {
				events = append(events, warning(warningpkg.WarnADKEventMalformed, fmt.Sprintf("part %d contains multiple provider fields", index)))
				continue
			}
			if part.Text != "" && !part.Thought && part.FunctionCall == nil && part.FunctionResponse == nil {
				item := base
				if event.Partial {
					item.Kind = loop.EventTextDelta
				} else {
					item.Kind = loop.EventText
				}
				item.Text = part.Text
				events = append(events, withRaw(item))
				continue
			}
			if event.Partial {
				events = append(events, warning(warningpkg.WarnADKEventMalformed, fmt.Sprintf("partial part %d is not plain text", index)))
				continue
			}
			if call := part.FunctionCall; call != nil {
				if call.ID == "" || call.Name == "" {
					events = append(events, warning(warningpkg.WarnADKEventMalformed, fmt.Sprintf("function call part %d is missing id or name", index)))
					continue
				}
				item := base
				item.Kind = loop.EventToolCall
				item.Tool = &loop.ToolInvocation{
					ID:           call.ID,
					Name:         call.Name,
					Args:         cloneMap(call.Args),
					SessionID:    sessionID,
					InvocationID: event.InvocationID,
				}
				events = append(events, withRaw(item))
				continue
			}
			if response := part.FunctionResponse; response != nil {
				if response.ID == "" || response.Name == "" {
					events = append(events, warning(warningpkg.WarnADKEventMalformed, fmt.Sprintf("function response part %d is missing id or name", index)))
					continue
				}
				content := cloneMap(response.Response)
				_, isError := content["error"]
				item := base
				item.Kind = loop.EventToolResult
				item.Tool = &loop.ToolInvocation{
					Name:         response.Name,
					Content:      content,
					IsError:      isError,
					CallID:       response.ID,
					SessionID:    sessionID,
					InvocationID: event.InvocationID,
				}
				events = append(events, withRaw(item))
				continue
			}
			events = append(events, warning(warningpkg.WarnADKEventUnknownPart, fmt.Sprintf("part %d has no portable text or function projection", index)))
		}
	}

	if event.TurnComplete {
		item := base
		item.Kind = loop.EventTurnComplete
		events = append(events, withRaw(item))
	}
	if len(events) == 0 {
		if usage != nil {
			item := base
			item.Kind = loop.EventNotice
			events = append(events, withRaw(item))
		} else if event.Content == nil {
			events = append(events, warning(warningpkg.WarnADKEventMalformed, "event has nil content and no metadata"))
		} else if len(event.Content.Parts) == 0 {
			events = append(events, warning(warningpkg.WarnADKEventMalformed, "event content has no parts"))
		}
	}
	return events, nil
}

func isFinalResponse(event *session.Event) bool {
	if event.Content != nil {
		for _, part := range event.Content.Parts {
			if part == nil {
				return false
			}
		}
	}
	return event.IsFinalResponse()
}

func toLoopUsage(metadata *genai.GenerateContentResponseUsageMetadata) *loop.Usage {
	if metadata == nil {
		return nil
	}
	return &loop.Usage{
		PromptTokens:     int(metadata.PromptTokenCount),
		CompletionTokens: int(metadata.CandidatesTokenCount),
		TotalTokens:      int(metadata.TotalTokenCount),
	}
}

func toADKUsage(usage *loop.Usage) *genai.GenerateContentResponseUsageMetadata {
	if usage == nil {
		return nil
	}
	return &genai.GenerateContentResponseUsageMetadata{
		PromptTokenCount:     int32(usage.PromptTokens),
		CandidatesTokenCount: int32(usage.CompletionTokens),
		TotalTokenCount:      int32(usage.TotalTokens),
	}
}

func cloneMap(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	output := make(map[string]any, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}
