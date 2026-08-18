package outputlimit

import (
	"encoding/json"
	"reflect"
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

func TestBoundJSONRejectsInvalidInputsAndFallsBackMinimally(t *testing.T) {
	tests := []struct {
		name     string
		project  map[string]any
		grant    int
		fallback JSONFallback
		want     map[string]any
		wantErr  string
	}{
		{
			name:    "missing fallback",
			project: map[string]any{},
			grant:   32,
			wantErr: "fallback is required",
		},
		{
			name:     "invalid projected value",
			project:  map[string]any{"bad": make(chan int)},
			grant:    32,
			fallback: func(string, Report) map[string]any { return nil },
			wantErr:  "unsupported type",
		},
		{
			name:     "exhausted grant",
			project:  map[string]any{},
			grant:    1,
			fallback: func(string, Report) map[string]any { return nil },
			wantErr:  "output budget is exhausted",
		},
		{
			name:    "invalid fallback value",
			project: map[string]any{"content": strings.Repeat("x", 64)},
			grant:   32,
			fallback: func(string, Report) map[string]any {
				return map[string]any{"bad": make(chan int)}
			},
			wantErr: "bound truncated JSON result",
		},
		{
			name:    "minimal marker",
			project: map[string]any{"content": strings.Repeat("x", 64)},
			grant:   20,
			fallback: func(string, Report) map[string]any {
				return map[string]any{"content": strings.Repeat("x", 64)}
			},
			want: map[string]any{"truncated": true},
		},
		{
			name:    "marker cannot fit",
			project: map[string]any{"content": strings.Repeat("x", 64)},
			grant:   10,
			fallback: func(string, Report) map[string]any {
				return map[string]any{"content": strings.Repeat("x", 64)}
			},
			wantErr: "output limit is too small",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, emitted, err := BoundJSON(test.project, test.grant, Policy{}, test.fallback)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("BoundJSON() error = %v, want containing %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("BoundJSON() = %#v, want %#v", got, test.want)
			}
			encoded, err := json.Marshal(got)
			if err != nil || emitted != len(encoded) {
				t.Fatalf("emitted = %d, encoding = %q, error = %v", emitted, encoded, err)
			}
		})
	}
}
