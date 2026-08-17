package sessionstore

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/plasmid-dev/plasmid/loop"
	"github.com/plasmid-dev/plasmid/warning"
)

func TestEventRecordRawRoundTrip(t *testing.T) {
	for _, raw := range []json.RawMessage{nil, json.RawMessage("null"), json.RawMessage(" \n {\"provider\": true}\t")} {
		t.Run(string(raw), func(t *testing.T) {
			event := loop.Event{ID: "event", Kind: loop.EventText, SessionID: "session", Timestamp: time.Unix(1, 0).UTC(), Raw: raw}
			record, err := newEventRecord(event)
			if err != nil {
				t.Fatal(err)
			}
			restored, err := record.event()
			if err != nil {
				t.Fatal(err)
			}
			if (restored.Raw == nil) != (raw == nil) || !bytes.Equal(restored.Raw, raw) {
				t.Fatalf("Raw = %q, want %q", restored.Raw, raw)
			}
		})
	}
}

func TestEventRecordRejectsInvalidRaw(t *testing.T) {
	if _, err := newEventRecord(loop.Event{Raw: json.RawMessage(`{"unterminated":`)}); err == nil {
		t.Fatal("newEventRecord accepted invalid Raw")
	}
}

func TestEventRecordDropsTemporaryState(t *testing.T) {
	record, err := newEventRecord(loop.Event{StateDelta: map[string]any{"state": 1, "app:state": 2, "user:state": 3, "temp:state": 4}})
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := record.StateDelta["temp:state"]; exists {
		t.Fatal("temporary state was persisted")
	}
}

func TestDecodeRecordWarnings(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{name: "unknown version", data: `{"v":2,"type":"event"}`, want: warning.WarnSessionRecordUnsupportedVersion},
		{name: "unknown type", data: `{"v":1,"type":"future"}`, want: warning.WarnSessionRecordUnknown},
		{name: "unknown version ignores malformed payload", data: `{"v":2,"type":"event","event":"not-an-object"}`, want: warning.WarnSessionRecordUnsupportedVersion},
		{name: "unknown type ignores malformed payload", data: `{"v":1,"type":"future","event":"not-an-object"}`, want: warning.WarnSessionRecordUnknown},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, warning, err := decodeRecord([]byte(test.data))
			if err != nil {
				t.Fatal(err)
			}
			if warning != test.want {
				t.Fatalf("warning = %q, want %q", warning, test.want)
			}
		})
	}
}
