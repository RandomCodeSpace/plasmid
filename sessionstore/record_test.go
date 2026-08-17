package sessionstore

import (
	"testing"

	"google.golang.org/adk/v2/session"

	"github.com/plasmid-dev/plasmid/warning"
)

func TestNativeEventRecordDropsTemporaryStateWithoutMutatingCaller(t *testing.T) {
	event := &session.Event{ID: "event", Actions: session.EventActions{StateDelta: map[string]any{"state": 1, "temp:state": 2}}}
	stored := cloneEvent(event)
	stored.Actions.StateDelta = withoutTemporaryState(stored.Actions.StateDelta)
	if _, exists := stored.Actions.StateDelta["temp:state"]; exists {
		t.Fatal("temporary state was persisted")
	}
	if _, exists := event.Actions.StateDelta["temp:state"]; !exists {
		t.Fatal("caller event was mutated")
	}
}

func TestDecodeRecordWarnings(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{name: "unknown version", data: `{"v":99,"type":"event"}`, want: warning.WarnSessionRecordUnsupportedVersion},
		{name: "unknown type", data: `{"v":2,"type":"future"}`, want: warning.WarnSessionRecordUnknown},
		{name: "unknown version ignores malformed payload", data: `{"v":99,"type":"event","event":"bad"}`, want: warning.WarnSessionRecordUnsupportedVersion},
		{name: "unknown type ignores malformed payload", data: `{"v":2,"type":"future","event":"bad"}`, want: warning.WarnSessionRecordUnknown},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, warningCode, err := decodeRecord([]byte(test.data))
			if err != nil {
				t.Fatal(err)
			}
			if warningCode != test.want {
				t.Fatalf("warning = %q, want %q", warningCode, test.want)
			}
		})
	}
}
