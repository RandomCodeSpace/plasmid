package outputlimit

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBoundJSONOwnsCompleteSerializedLimit(t *testing.T) {
	tests := []struct {
		name  string
		grant int
		want  bool
	}{
		{name: "within limit", grant: 512},
		{name: "truncated", grant: 128, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, emitted, err := BoundJSON(
				map[string]any{"content": strings.Repeat("x", 300)},
				test.grant,
				Policy{MaxBytes: 512, MaxLines: 20, MaxLineBytes: 512, HeadFraction: 0.5},
				func(text string, report Report) map[string]any {
					return map[string]any{"output": text, "truncated": true, "truncation": report}
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			if len(encoded) > test.grant || emitted != len(encoded) {
				t.Fatalf("serialized result = %d bytes, emitted = %d, grant = %d", len(encoded), emitted, test.grant)
			}
			if got, _ := result["truncated"].(bool); got != test.want {
				t.Fatalf("truncated = %v, want %v", got, test.want)
			}
		})
	}
}
