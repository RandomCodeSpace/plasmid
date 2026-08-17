package loop

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

type stubTool struct {
	name        string
	description string
	schema      json.RawMessage
}

func (t *stubTool) Name() string                 { return t.name }
func (t *stubTool) Description() string          { return t.description }
func (t *stubTool) InputSchema() json.RawMessage { return t.schema }
func (t *stubTool) Call(context.Context, ToolCall) (ToolResult, error) {
	return ToolResult{}, nil
}

func TestToolSchemasDefensiveCopy(t *testing.T) {
	t.Parallel()
	first := &stubTool{name: "first", description: "one", schema: json.RawMessage(`{"type":"object"}`)}
	second := &stubTool{name: "second", description: "two", schema: json.RawMessage(`{"type":"string"}`)}
	got := ToolSchemas([]Tool{first, second})
	if got[0].Name != "first" || got[1].Name != "second" {
		t.Fatalf("schemas = %#v", got)
	}
	got[0].InputSchema[0] = 'x'
	got[1] = ToolSchema{}
	if string(first.schema) != `{"type":"object"}` || second.Name() != "second" {
		t.Fatal("ToolSchemas output aliases input")
	}
}

func TestFilterTools(t *testing.T) {
	t.Parallel()
	tools := []Tool{&stubTool{name: "a"}, &stubTool{name: "b"}, &stubTool{name: "c"}}
	tests := []struct {
		name string
		view View
		want []string
	}{
		{name: "none", want: []string{"a", "b", "c"}},
		{name: "allowed first", view: View{AllowedTools: []string{"c", "a"}}, want: []string{"a", "c"}},
		{name: "disallowed", view: View{DisallowedTools: []string{"b"}}, want: []string{"a", "c"}},
		{name: "explicit allow wins conflict", view: View{AllowedTools: []string{"b"}, DisallowedTools: []string{"b"}}, want: []string{"b"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			filtered := FilterTools(tools, test.view)
			got := make([]string, len(filtered))
			for index, tool := range filtered {
				got[index] = tool.Name()
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("names = %#v, want %#v", got, test.want)
			}
			if len(filtered) != 0 {
				filtered[0] = &stubTool{name: "mutated"}
			}
			if tools[0].Name() != "a" {
				t.Fatal("FilterTools output aliases input slice")
			}
		})
	}
}
