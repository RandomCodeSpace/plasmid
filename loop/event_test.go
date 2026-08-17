package loop

import (
	"encoding/json"
	"errors"
	"iter"
	"reflect"
	"strings"
	"testing"
	"time"
)

type streamItem struct {
	event Event
	err   error
}

func sequence(items []streamItem, pulled *int) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		for _, item := range items {
			if pulled != nil {
				*pulled++
			}
			if !yield(item.event, item.err) {
				return
			}
		}
	}
}

func collectStream(sequence iter.Seq2[Event, error]) []streamItem {
	var items []streamItem
	for event, err := range sequence {
		items = append(items, streamItem{event: event, err: err})
	}
	return items
}

func TestNormalizeStream(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("provider stopped")
	timestamp := time.Unix(100, 0).UTC()
	tests := []struct {
		name     string
		coalesce bool
		input    []streamItem
		want     []streamItem
	}{
		{
			name:     "disabled",
			coalesce: false,
			input: []streamItem{
				{event: Event{Kind: EventTextDelta, Author: "a", Text: "one"}},
				{event: Event{Kind: EventTextDelta, Author: "a", Text: "two"}},
			},
			want: []streamItem{
				{event: Event{Kind: EventTextDelta, Author: "a", Text: "one"}},
				{event: Event{Kind: EventTextDelta, Author: "a", Text: "two"}},
			},
		},
		{
			name:     "same author",
			coalesce: true,
			input: []streamItem{
				{event: Event{Kind: EventTextDelta, SessionID: "s", InvocationID: "i", Author: "a", Text: "one", Timestamp: timestamp, Raw: json.RawMessage(`{"n": 1}`)}},
				{event: Event{Kind: EventTextDelta, SessionID: "s", InvocationID: "i", Author: "a", Text: "two", Final: true, Timestamp: timestamp.Add(time.Second), Raw: json.RawMessage(`{"n": 1}`)}},
			},
			want: []streamItem{{event: Event{Kind: EventText, SessionID: "s", InvocationID: "i", Author: "a", Text: "onetwo", Final: true, Timestamp: timestamp, Raw: json.RawMessage(`{"n": 1}`)}}},
		},
		{
			name:     "author and kind boundaries",
			coalesce: true,
			input: []streamItem{
				{event: Event{Kind: EventTextDelta, Author: "a", Text: "one"}},
				{event: Event{Kind: EventTextDelta, Author: "b", Text: "two"}},
				{event: Event{Kind: EventNotice, Text: "notice"}},
			},
			want: []streamItem{
				{event: Event{Kind: EventText, Author: "a", Text: "one"}},
				{event: Event{Kind: EventText, Author: "b", Text: "two"}},
				{event: Event{Kind: EventNotice, Text: "notice"}},
			},
		},
		{
			name:     "error boundary",
			coalesce: true,
			input: []streamItem{
				{event: Event{Kind: EventTextDelta, Author: "a", Text: "one"}},
				{err: sentinel},
				{event: Event{Kind: EventTextDelta, Author: "a", Text: "two"}},
			},
			want: []streamItem{
				{event: Event{Kind: EventText, Author: "a", Text: "one"}},
				{err: sentinel},
				{event: Event{Kind: EventText, Author: "a", Text: "two"}},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := collectStream(NormalizeStream(sequence(test.input, nil), test.coalesce))
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("stream = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestNormalizeStreamStopsUpstream(t *testing.T) {
	t.Parallel()
	pulled := 0
	stream := NormalizeStream(sequence([]streamItem{
		{event: Event{Kind: EventNotice, Text: "one"}},
		{event: Event{Kind: EventNotice, Text: "two"}},
	}, &pulled), false)
	seen := 0
	stream(func(Event, error) bool {
		seen++
		return false
	})
	if pulled != 1 || seen != 1 {
		t.Fatalf("pulled = %d, seen = %d", pulled, seen)
	}
}

func TestNormalizeStreamCoalescedStopClosesUpstream(t *testing.T) {
	t.Parallel()
	pulled := 0
	closed := false
	in := func(yield func(Event, error) bool) {
		defer func() { closed = true }()
		items := []Event{
			{Kind: EventTextDelta, Author: "a", Text: "one"},
			{Kind: EventNotice, Text: "boundary"},
			{Kind: EventNotice, Text: "must not be pulled"},
		}
		for _, event := range items {
			pulled++
			if !yield(event, nil) {
				return
			}
		}
	}
	seen := 0
	NormalizeStream(in, true)(func(event Event, err error) bool {
		seen++
		if err != nil || event.Kind != EventText || event.Text != "one" {
			t.Fatalf("first item = (%#v, %v)", event, err)
		}
		return false
	})
	if pulled != 2 || seen != 1 || !closed {
		t.Fatalf("pulled = %d, seen = %d, closed = %v", pulled, seen, closed)
	}
}

func TestNormalizeStreamDropsDivergentRaw(t *testing.T) {
	t.Parallel()
	got := collectStream(NormalizeStream(sequence([]streamItem{
		{event: Event{Kind: EventTextDelta, Author: "a", Text: "one", Raw: json.RawMessage(`"a"`)}},
		{event: Event{Kind: EventTextDelta, Author: "a", Text: "two", Raw: json.RawMessage(`"b"`)}},
	}, nil), true))
	if len(got) != 1 || got[0].event.Raw != nil {
		t.Fatalf("stream = %#v", got)
	}
}

func TestNormalizeStreamPreservesEquivalentPortableRaw(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		first json.RawMessage
		last  json.RawMessage
	}{
		{
			name:  "objects",
			first: json.RawMessage(`{"nested":{"ok":true},"values":[1,"two"]}`),
			last:  json.RawMessage(`{"nested":{"ok":true},"values":[1,"two"]}`),
		},
		{name: "array", first: json.RawMessage(`["same",2,false]`), last: json.RawMessage(`["same",2,false]`)},
		{name: "string", first: json.RawMessage(`"same"`), last: json.RawMessage(`"same"`)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := collectStream(NormalizeStream(sequence([]streamItem{
				{event: Event{Kind: EventTextDelta, Author: "a", Text: "one", Raw: test.first}},
				{event: Event{Kind: EventTextDelta, Author: "a", Text: "two", Raw: test.last}},
			}, nil), true))
			if len(got) != 1 || !reflect.DeepEqual(got[0].event.Raw, test.first) {
				t.Fatalf("raw = %#v, want %#v", got[0].event.Raw, test.first)
			}
		})
	}
}

func TestPortableJSONRoundTrip(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("not portable")
	original := Event{
		Kind: EventToolResult,
		Text: "portable error text",
		Tool: &ToolInvocation{
			ID:      "invocation",
			Name:    "read",
			CallID:  "call",
			Content: map[string]any{"output": "failed", "status": float64(1)},
			IsError: true,
		},
		Err: sentinel,
		Raw: json.RawMessage(`{"provider":"value"}`),
	}
	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "not portable") || strings.Contains(string(encoded), `"err"`) {
		t.Fatalf("Go error leaked into JSON: %s", encoded)
	}
	var decoded Event
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded.Raw, original.Raw) || !reflect.DeepEqual(decoded.Tool, original.Tool) || decoded.Err != nil {
		t.Fatalf("decoded = %#v", decoded)
	}

	message := Message{Role: RoleAssistant, Raw: []any{"portable", float64(2)}}
	encoded, err = json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	var decodedMessage Message
	if err := json.Unmarshal(encoded, &decodedMessage); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decodedMessage.Raw, message.Raw) {
		t.Fatalf("raw = %#v, want %#v", decodedMessage.Raw, message.Raw)
	}
}

func TestEventRawRequiresValidJSON(t *testing.T) {
	t.Parallel()
	if _, err := json.Marshal(Event{Raw: json.RawMessage(`{"provider":`)}); err == nil {
		t.Fatal("marshal accepted invalid event Raw JSON")
	}
}

func TestEventHelpers(t *testing.T) {
	t.Parallel()
	if !(Event{Kind: EventTextDelta}).IsText() || !(Event{Kind: EventText}).IsText() || (Event{Kind: EventNotice}).IsText() {
		t.Fatal("IsText classification is wrong")
	}
	warning := Warning{Path: "a.md", Line: 2, Code: WarnSyntaxUnknownField, Message: "bad field"}
	if got, want := (Event{Kind: EventWarning, Warning: &warning}).String(), "a.md:2: syntax.unknown-field: bad field"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}
