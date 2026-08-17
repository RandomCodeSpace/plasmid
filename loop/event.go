package loop

import (
	"bytes"
	"encoding/json"
	"fmt"
	"iter"
	"time"

	"github.com/plasmid-dev/plasmid/warning"
)

// EventKind is a stable streamed-event wire value. Unknown values are retained.
type EventKind string

const (
	EventTextDelta    EventKind = "text_delta"
	EventText         EventKind = "text"
	EventToolCall     EventKind = "tool_call"
	EventToolResult   EventKind = "tool_result"
	EventTurnComplete EventKind = "turn_complete"
	EventNotice       EventKind = "notice"
	EventWarning      EventKind = "warning"
	EventError        EventKind = "error"
)

// ToolInvocation is the minimal event-side tool projection needed by the loop
// bridge. ID identifies an invocation and CallID links a result to its call.
// Calls populate Args; results populate Content and IsError. Both payloads are
// portable JSON objects so EventToolResult never depends on provider-native Raw
// data. Provider-specific function types do not belong in this contract.
type ToolInvocation struct {
	ID           string         `json:"id"`
	Name         string         `json:"name"`
	Args         map[string]any `json:"args"`
	Content      map[string]any `json:"content"`
	IsError      bool           `json:"isError"`
	CallID       string         `json:"callId"`
	SessionID    string         `json:"sessionId"`
	InvocationID string         `json:"invocationId"`
}

// Event is the single provider-neutral stream item. Err is yielded separately
// by iter.Seq2 and excluded from portable JSON. Raw holds an opaque, valid
// JSON encoding of the provider event for durable adapter reconstruction.
type Event struct {
	ID             string           `json:"id"`
	Kind           EventKind        `json:"kind"`
	SessionID      string           `json:"sessionId"`
	InvocationID   string           `json:"invocationId"`
	Branch         string           `json:"branch,omitempty"`
	IsolationScope string           `json:"isolationScope,omitempty"`
	Author         string           `json:"author"`
	Text           string           `json:"text"`
	Tool           *ToolInvocation  `json:"tool"`
	Usage          *Usage           `json:"usage"`
	Warning        *warning.Warning `json:"warning"`
	Err            error            `json:"-"`
	Final          bool             `json:"final"`
	Timestamp      time.Time        `json:"timestamp"`
	StateDelta     map[string]any   `json:"stateDelta,omitempty"`
	Raw            json.RawMessage  `json:"raw,omitempty"`
}

// IsText reports whether an event contains complete or incremental text.
func (e Event) IsText() bool {
	return e.Kind == EventText || e.Kind == EventTextDelta
}

// String returns a stable log-oriented representation.
func (e Event) String() string {
	switch {
	case e.Warning != nil:
		return e.Warning.String()
	case e.Err != nil:
		return fmt.Sprintf("%s: %s", e.Kind, e.Err)
	case e.Text != "":
		return fmt.Sprintf("%s: %s", e.Kind, e.Text)
	case e.Tool != nil && e.Tool.Name != "":
		return fmt.Sprintf("%s: %s", e.Kind, e.Tool.Name)
	default:
		return string(e.Kind)
	}
}

// NormalizeStream optionally coalesces consecutive same-author text deltas.
// Iterator errors are yielded as separate boundaries and never folded into an
// event. Returning false from yield stops upstream consumption immediately.
func NormalizeStream(in iter.Seq2[Event, error], coalesce bool) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		if in == nil {
			return
		}
		if !coalesce {
			for event, err := range in {
				if !yield(event, err) {
					return
				}
			}
			return
		}

		var pending *Event
		flush := func() bool {
			if pending == nil {
				return true
			}
			event := *pending
			pending = nil
			return yield(event, nil)
		}

		for event, err := range in {
			if err != nil {
				if !flush() || !yield(Event{}, err) {
					return
				}
				continue
			}
			if event.Kind != EventTextDelta {
				if !flush() || !yield(event, nil) {
					return
				}
				continue
			}
			if pending == nil {
				copy := event
				copy.Kind = EventText
				pending = &copy
				continue
			}
			if pending.Author != event.Author {
				if !flush() {
					return
				}
				copy := event
				copy.Kind = EventText
				pending = &copy
				continue
			}
			pending.Text += event.Text
			pending.Final = event.Final
			if !rawEqual(pending.Raw, event.Raw) {
				pending.Raw = nil
			}
		}
		flush()
	}
}

func rawEqual(left, right json.RawMessage) bool {
	return bytes.Equal(left, right)
}
