// Package sessionstore persists provider-neutral loop sessions.
package sessionstore

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"strconv"
	"time"

	"github.com/plasmid-dev/plasmid/loop"
	"github.com/plasmid-dev/plasmid/warning"
)

const (
	recordVersion = 1
	recordSession = "session"
	recordEvent   = "event"
	recordSidecar = "sidecar"
)

type record struct {
	V       int          `json:"v"`
	Type    string       `json:"type"`
	Order   uint64       `json:"order,omitempty"`
	Session *header      `json:"session,omitempty"`
	Event   *eventRecord `json:"event,omitempty"`
	Sidecar *sidecar     `json:"sidecar,omitempty"`
}

type preparedRecord struct {
	typ     string
	payload []byte
}

func prepareHeaderRecord(value header) (preparedRecord, error) {
	data, err := json.Marshal(record{V: recordVersion, Type: recordSession, Session: &value})
	if err != nil {
		return preparedRecord{}, fmt.Errorf("encode session record: %w", err)
	}
	return preparedRecord{typ: recordSession, payload: data}, nil
}

func prepareEventRecord(value loop.Event) (preparedRecord, eventRecord, error) {
	event, err := newEventRecord(value)
	if err != nil {
		return preparedRecord{}, eventRecord{}, err
	}
	data, err := json.Marshal(record{V: recordVersion, Type: recordEvent, Event: &event})
	if err != nil {
		return preparedRecord{}, eventRecord{}, fmt.Errorf("encode session record: %w", err)
	}
	return preparedRecord{typ: recordEvent, payload: data}, event, nil
}

func prepareSidecarRecord(value sidecar) (preparedRecord, error) {
	data, err := json.Marshal(record{V: recordVersion, Type: recordSidecar, Sidecar: &value})
	if err != nil {
		return preparedRecord{}, fmt.Errorf("encode session record: %w", err)
	}
	return preparedRecord{typ: recordSidecar, payload: data}, nil
}

func (p preparedRecord) bytes(order uint64) []byte {
	if order == 0 {
		return append(append([]byte(nil), p.payload...), '\n')
	}
	data := make([]byte, 0, len(p.payload)+32)
	data = append(data, p.payload[:len(p.payload)-1]...)
	data = append(data, ',', '"', 'o', 'r', 'd', 'e', 'r', '"', ':')
	data = strconv.AppendUint(data, order, 10)
	data = append(data, '}', '\n')
	return data
}

type header struct {
	ID      string         `json:"id"`
	AppName string         `json:"appName"`
	UserID  string         `json:"userId"`
	State   map[string]any `json:"state"`
}

type sidecar struct {
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}

// eventRecord contains only portable event fields. raw is base64 rather than
// JSON so a nil Raw and every valid byte representation remain distinguishable.
type eventRecord struct {
	ID             string               `json:"id"`
	Kind           loop.EventKind       `json:"kind"`
	SessionID      string               `json:"sessionId"`
	InvocationID   string               `json:"invocationId"`
	Branch         string               `json:"branch,omitempty"`
	IsolationScope string               `json:"isolationScope,omitempty"`
	Author         string               `json:"author"`
	Text           string               `json:"text"`
	Tool           *loop.ToolInvocation `json:"tool"`
	Usage          *loop.Usage          `json:"usage"`
	Warning        *warning.Warning     `json:"warning"`
	Final          bool                 `json:"final"`
	Timestamp      time.Time            `json:"timestamp"`
	StateDelta     map[string]any       `json:"stateDelta,omitempty"`
	Raw            *string              `json:"raw,omitempty"`
}

func newEventRecord(event loop.Event) (eventRecord, error) {
	if event.Raw != nil && !json.Valid(event.Raw) {
		return eventRecord{}, fmt.Errorf("event Raw is not one complete JSON value")
	}

	record := eventRecord{
		ID:             event.ID,
		Kind:           event.Kind,
		SessionID:      event.SessionID,
		InvocationID:   event.InvocationID,
		Branch:         event.Branch,
		IsolationScope: event.IsolationScope,
		Author:         event.Author,
		Text:           event.Text,
		Tool:           event.Tool,
		Usage:          event.Usage,
		Warning:        event.Warning,
		Final:          event.Final,
		StateDelta:     withoutTemporaryState(event.StateDelta),
	}
	record.Timestamp = event.Timestamp
	if event.Raw != nil {
		encoded := base64.StdEncoding.EncodeToString(event.Raw)
		record.Raw = &encoded
	}
	return record, nil
}

func (record eventRecord) event() (loop.Event, error) {
	event := loop.Event{
		ID:             record.ID,
		Kind:           record.Kind,
		SessionID:      record.SessionID,
		InvocationID:   record.InvocationID,
		Branch:         record.Branch,
		IsolationScope: record.IsolationScope,
		Author:         record.Author,
		Text:           record.Text,
		Tool:           record.Tool,
		Usage:          record.Usage,
		Warning:        record.Warning,
		Final:          record.Final,
		StateDelta:     maps.Clone(record.StateDelta),
	}
	event.Timestamp = record.Timestamp
	if record.Raw != nil {
		raw, err := base64.StdEncoding.DecodeString(*record.Raw)
		if err != nil {
			return loop.Event{}, fmt.Errorf("decode event Raw: %w", err)
		}
		if !json.Valid(raw) {
			return loop.Event{}, fmt.Errorf("event Raw is not one complete JSON value")
		}
		event.Raw = json.RawMessage(raw)
	}
	return event, nil
}

// decodeRecord identifies records that replay must skip without treating an
// unknown forward-compatible envelope as log corruption.
func decodeRecord(data []byte) (record, string, error) {
	var envelope struct {
		V    int    `json:"v"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return record{}, "", fmt.Errorf("decode session record: %w", err)
	}
	if envelope.V != recordVersion {
		return record{}, warning.WarnSessionRecordUnsupportedVersion, nil
	}
	switch envelope.Type {
	case recordSession, recordEvent, recordSidecar:
	default:
		return record{}, warning.WarnSessionRecordUnknown, nil
	}

	var decoded record
	if err := json.Unmarshal(data, &decoded); err != nil {
		return record{}, "", fmt.Errorf("decode session record: %w", err)
	}
	switch envelope.Type {
	case recordSession:
		if decoded.Session == nil {
			return record{}, "", fmt.Errorf("session record has no session")
		}
	case recordEvent:
		if decoded.Event == nil {
			return record{}, "", fmt.Errorf("event record has no event")
		}
	case recordSidecar:
		if decoded.Sidecar == nil {
			return record{}, "", fmt.Errorf("sidecar record has no sidecar")
		}
	}
	return decoded, "", nil
}
