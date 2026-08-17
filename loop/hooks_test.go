package loop

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestHooksMergeOrderAndCopies(t *testing.T) {
	t.Parallel()
	leftCalls := 0
	rightCalls := 0
	left := Hooks{BeforeModel: []BeforeModelHook{func(context.Context, *ModelRequest) (*ModelResponse, error) {
		leftCalls++
		return nil, nil
	}}}
	right := Hooks{BeforeModel: []BeforeModelHook{func(context.Context, *ModelRequest) (*ModelResponse, error) {
		rightCalls++
		return nil, nil
	}}}
	merged := left.Merge(right)
	merged.BeforeModel[0] = nil
	if left.BeforeModel[0] == nil || right.BeforeModel[0] == nil {
		t.Fatal("Merge output aliases an input slice")
	}
	merged = left.Merge(right)
	if _, err := merged.RunBeforeModel(context.Background(), &ModelRequest{}); err != nil {
		t.Fatal(err)
	}
	if leftCalls != 1 || rightCalls != 1 {
		t.Fatalf("calls = %d, %d", leftCalls, rightCalls)
	}
}

func TestBeforeHookShortCircuitAndError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("stop")
	tests := []struct {
		name      string
		withError bool
	}{
		{name: "result"},
		{name: "error", withError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var calls []string
			hooks := Hooks{BeforeTool: []BeforeToolHook{
				func(_ context.Context, call *ToolCall) (*ToolResult, error) {
					calls = append(calls, "first")
					call.Name = "mutated"
					return nil, nil
				},
				func(context.Context, *ToolCall) (*ToolResult, error) {
					calls = append(calls, "second")
					result := &ToolResult{CallID: "short"}
					if test.withError {
						return result, sentinel
					}
					return result, nil
				},
				func(context.Context, *ToolCall) (*ToolResult, error) {
					calls = append(calls, "third")
					return nil, nil
				},
			}}
			call := &ToolCall{Name: "original"}
			result, err := hooks.RunBeforeTool(context.Background(), call)
			if !reflect.DeepEqual(calls, []string{"first", "second"}) || call.Name != "mutated" || result.CallID != "short" {
				t.Fatalf("calls = %#v, call = %#v, result = %#v", calls, call, result)
			}
			if test.withError && !errors.Is(err, sentinel) || !test.withError && err != nil {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestAfterHooksFlowReplacementsAndErrors(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("hook failed")
	var seen []string
	hooks := Hooks{AfterModel: []AfterModelHook{
		func(_ context.Context, response *ModelResponse, err error) (*ModelResponse, error) {
			if !errors.Is(err, sentinel) {
				t.Fatalf("first hook error = %v", err)
			}
			seen = append(seen, response.Message.Text)
			return &ModelResponse{Message: Message{Text: "replacement"}}, nil
		},
		func(_ context.Context, response *ModelResponse, err error) (*ModelResponse, error) {
			if !errors.Is(err, sentinel) {
				t.Fatalf("second hook error = %v", err)
			}
			seen = append(seen, response.Message.Text)
			return &ModelResponse{Message: Message{Text: "final"}}, nil
		},
	}}
	response, err := hooks.RunAfterModel(context.Background(), &ModelResponse{Message: Message{Text: "initial"}}, sentinel)
	if !errors.Is(err, sentinel) || response.Message.Text != "final" || !reflect.DeepEqual(seen, []string{"initial", "replacement"}) {
		t.Fatalf("response = %#v, err = %v, seen = %#v", response, err, seen)
	}

	calls := 0
	hooks.AfterTool = []AfterToolHook{
		func(context.Context, *ToolCall, *ToolResult, error) (*ToolResult, error) {
			calls++
			return &ToolResult{CallID: "kept"}, sentinel
		},
		func(context.Context, *ToolCall, *ToolResult, error) (*ToolResult, error) {
			calls++
			return nil, nil
		},
	}
	result, err := hooks.RunAfterTool(context.Background(), &ToolCall{}, &ToolResult{CallID: "initial"}, nil)
	if calls != 1 || result.CallID != "kept" || !errors.Is(err, sentinel) {
		t.Fatalf("calls = %d, result = %#v, err = %v", calls, result, err)
	}
}
