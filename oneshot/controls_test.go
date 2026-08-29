package oneshot

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/toolutils"
	"google.golang.org/genai"
)

func TestRunValidatesEveryControlBeforeCallingModel(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Request)
	}{
		{name: "zero output tokens", mutate: func(request *Request) { request.MaxOutputTokens = 0 }},
		{name: "negative output tokens", mutate: func(request *Request) { request.MaxOutputTokens = -1 }},
		{name: "zero text bytes", mutate: func(request *Request) { request.MaxReturnedTextBytes = 0 }},
		{name: "negative text bytes", mutate: func(request *Request) { request.MaxReturnedTextBytes = -1 }},
		{name: "zero model calls", mutate: func(request *Request) { request.MaxModelCalls = 0 }},
		{name: "negative model calls", mutate: func(request *Request) { request.MaxModelCalls = -1 }},
		{name: "zero tool calls", mutate: func(request *Request) { request.MaxToolCallsPerResponse = 0 }},
		{name: "negative tool calls", mutate: func(request *Request) { request.MaxToolCallsPerResponse = -1 }},
		{name: "unknown tool policy", mutate: func(request *Request) { request.ToolExecution = ToolExecutionPolicy(2) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			modelValue := &scriptedModel{step: func(int, *model.LLMRequest, bool) (*model.LLMResponse, error) {
				t.Fatal("model was called")
				return nil, nil
			}}
			request := boundedRequest(Request{Model: modelValue, Prompt: "validate"})
			test.mutate(&request)

			result, err := Run(t.Context(), request)
			if CodeOf(err) != CodeInvalidArgument || !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("error = %v, code = %q", err, CodeOf(err))
			}
			if modelValue.calls != 0 || result.Text != "" || len(result.ToolResults) != 0 || result.Metadata != (Metadata{}) {
				t.Fatalf("calls = %d, result = %#v", modelValue.calls, result)
			}
		})
	}
}

func TestRunAcceptsMinimumControlValues(t *testing.T) {
	modelValue := &scriptedModel{step: func(_ int, request *model.LLMRequest, _ bool) (*model.LLMResponse, error) {
		if request.Config == nil || request.Config.MaxOutputTokens != 1 {
			return nil, fmt.Errorf("max output tokens = %#v", request.Config)
		}
		return textResponse("x"), nil
	}}
	request := Request{
		Model: modelValue, Prompt: "minimum", MaxOutputTokens: 1, MaxReturnedTextBytes: 1,
		MaxModelCalls: 1, MaxToolCallsPerResponse: 1,
	}
	result, err := Run(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "x" || result.Metadata.ModelCalls != 1 {
		t.Fatalf("result = %#v", result)
	}
}

func TestMaxOutputTokensAppliesToEveryModelRequest(t *testing.T) {
	const limit = 17
	var observed []int32
	modelValue := &scriptedModel{step: func(call int, request *model.LLMRequest, _ bool) (*model.LLMResponse, error) {
		observed = append(observed, request.Config.MaxOutputTokens)
		if call == 0 {
			return functionCallResponse("echo"), nil
		}
		return textResponse("done"), nil
	}}
	request := boundedRequest(Request{
		Model: modelValue, Prompt: "echo", Tools: []tool.Tool{&testFunctionTool{name: "echo"}},
	})
	request.MaxOutputTokens = limit
	result, err := Run(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(observed, []int32{limit, limit}) || result.Metadata.ModelCalls != 2 {
		t.Fatalf("observed = %v, metadata = %#v", observed, result.Metadata)
	}
}

func TestRunReturnsTypedOutputTruncationWithPartialText(t *testing.T) {
	response := textResponse("partial")
	response.FinishReason = genai.FinishReasonMaxTokens
	service := newTracingSessionService()
	result, err := runWithSessionService(t.Context(), boundedRequest(Request{
		Model:  &scriptedModel{step: func(int, *model.LLMRequest, bool) (*model.LLMResponse, error) { return response, nil }},
		Prompt: "truncate",
	}), service)
	assertSafeReturnedError(t, err, CodeOutputTruncated, ErrOutputTruncated)
	if result.Text != "partial" || result.Metadata.ModelCalls != 1 {
		t.Fatalf("result = %#v", result)
	}
	service.assertLifecycle(t, 1)
}

func TestCallerFailurePrecedesResponseLimitOutcomes(t *testing.T) {
	tests := []struct {
		name      string
		failure   error
		cancel    bool
		wantCode  ErrorCode
		wantError error
	}{
		{name: "transport", failure: errors.New("transport secret"), wantCode: CodeExecutionFailed, wantError: ErrExecutionFailed},
		{name: "cancellation", failure: context.Canceled, cancel: true, wantCode: CodeCanceled, wantError: ErrCanceled},
		{name: "hostile error", failure: panicCallerError{method: "Error"}, wantCode: CodeModelPanic, wantError: ErrModelPanic},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var toolCalls atomic.Int32
			response := functionCallBatchResponse("overflow", "first", "second")
			response.FinishReason = genai.FinishReasonMaxTokens
			modelValue := &scriptedModel{step: func(int, *model.LLMRequest, bool) (*model.LLMResponse, error) {
				if test.cancel {
					cancel()
				}
				return response, test.failure
			}}
			request := boundedRequest(Request{
				Model: modelValue, Prompt: "precedence",
				Tools: []tool.Tool{
					&testFunctionTool{name: "first", run: func(agent.Context, any) (map[string]any, error) { toolCalls.Add(1); return nil, nil }},
					&testFunctionTool{name: "second", run: func(agent.Context, any) (map[string]any, error) { toolCalls.Add(1); return nil, nil }},
				},
			})
			request.MaxReturnedTextBytes = 3
			request.MaxToolCallsPerResponse = 1

			result, err := Run(ctx, request)
			assertSafeReturnedError(t, err, test.wantCode, test.wantError, "transport secret", "CALLER_METHOD_SECRET")
			if result.Text != "ove" || toolCalls.Load() != 0 || result.Metadata.ModelCalls != 1 {
				t.Fatalf("tool calls = %d, result = %#v", toolCalls.Load(), result)
			}
		})
	}
}

func TestModelGuardDoesNotRecoverPlasmidRequestPanics(t *testing.T) {
	statistics := &runStatistics{}
	failures := &failureRecorder{}
	request := boundedRequest(Request{Model: finalModel("unused")})
	guarded, err := protectModel(
		request.Model,
		statistics,
		failures,
		&responseRecorder{},
		controlsFromRequest(request),
	)
	if err != nil {
		t.Fatal(err)
	}

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		guarded.GenerateContent(t.Context(), nil, false)(func(*model.LLMResponse, error) bool { return true })
	}()
	if recovered == nil {
		t.Fatal("internal request panic was recovered as a caller failure")
	}
	if failure := failures.failure(); failure != nil {
		t.Fatalf("recorded caller failure = %v", failure)
	}
	if statistics.modelCalls.Load() != 1 {
		t.Fatalf("model calls = %d", statistics.modelCalls.Load())
	}
}

func TestReturnedTextByteCapBoundaryAndOverflow(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		limit    int
		wantText string
		wantCode ErrorCode
	}{
		{name: "one byte", text: "x", limit: 1, wantText: "x"},
		{name: "exact boundary", text: "abc", limit: 3, wantText: "abc"},
		{name: "one byte overflow", text: "abcd", limit: 3, wantText: "abc", wantCode: CodeTextTruncated},
		{name: "raw byte boundary", text: "éx", limit: 2, wantText: "é", wantCode: CodeTextTruncated},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := boundedRequest(Request{Model: finalModel(test.text), Prompt: "cap"})
			request.MaxReturnedTextBytes = test.limit
			result, err := Run(t.Context(), request)
			if test.wantCode == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				assertSafeReturnedError(t, err, test.wantCode, ErrTextTruncated)
			}
			if result.Text != test.wantText || len(result.Text) > test.limit {
				t.Fatalf("result = %#v, bytes = %d", result, len(result.Text))
			}
		})
	}
}

func TestMaxModelCallsBoundsTheNativeOuterLoop(t *testing.T) {
	var executed atomic.Int32
	toolValue := &testFunctionTool{name: "once", run: func(agent.Context, any) (map[string]any, error) {
		executed.Add(1)
		return map[string]any{"value": "kept"}, nil
	}}
	modelValue := &scriptedModel{step: func(call int, _ *model.LLMRequest, _ bool) (*model.LLMResponse, error) {
		if call != 0 {
			t.Fatalf("source model call = %d", call)
		}
		return functionCallResponse("once"), nil
	}}
	request := boundedRequest(Request{Model: modelValue, Prompt: "loop", Tools: []tool.Tool{toolValue}})
	request.MaxModelCalls = 1

	result, err := Run(t.Context(), request)
	assertSafeReturnedError(t, err, CodeModelCallLimit, ErrModelCallLimit)
	if modelValue.calls != 1 || executed.Load() != 1 {
		t.Fatalf("model calls = %d, tool calls = %d", modelValue.calls, executed.Load())
	}
	if result.Metadata.ModelCalls != 1 || result.Metadata.ToolCalls != 1 || len(result.ToolResults) != 1 {
		t.Fatalf("result = %#v", result)
	}
	if result.ToolResults[0].Name != "once" || result.ToolResults[0].Response["value"] != "kept" {
		t.Fatalf("tool result = %#v", result.ToolResults[0])
	}
}

func TestToolCallLimitRejectsWholeResponseBeforeExecution(t *testing.T) {
	var calls atomic.Int32
	tools := []tool.Tool{
		&testFunctionTool{name: "first", run: func(agent.Context, any) (map[string]any, error) { calls.Add(1); return nil, nil }},
		&testFunctionTool{name: "second", run: func(agent.Context, any) (map[string]any, error) { calls.Add(1); return nil, nil }},
	}
	response := functionCallBatchResponse("prefix", "first", "second")
	request := boundedRequest(Request{
		Model:  &scriptedModel{step: func(int, *model.LLMRequest, bool) (*model.LLMResponse, error) { return response, nil }},
		Prompt: "overflow", Tools: tools,
	})
	request.MaxToolCallsPerResponse = 1

	result, err := Run(t.Context(), request)
	assertSafeReturnedError(t, err, CodeToolCallLimit, ErrToolCallLimit)
	if calls.Load() != 0 || result.Metadata.ToolCalls != 0 || len(result.ToolResults) != 0 {
		t.Fatalf("calls = %d, result = %#v", calls.Load(), result)
	}
	if result.Text != "prefix" || result.Metadata.ModelCalls != 1 {
		t.Fatalf("partial result = %#v", result)
	}
}

func TestContentLimitOutcomePrecedenceRejectsToolBatch(t *testing.T) {
	tests := []struct {
		name         string
		finishReason genai.FinishReason
		wantCode     ErrorCode
		wantError    error
	}{
		{name: "output before text and tools", finishReason: genai.FinishReasonMaxTokens, wantCode: CodeOutputTruncated, wantError: ErrOutputTruncated},
		{name: "text before tools", wantCode: CodeTextTruncated, wantError: ErrTextTruncated},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			response := functionCallBatchResponse("overflow", "first", "second")
			response.FinishReason = test.finishReason
			request := boundedRequest(Request{
				Model: &scriptedModel{step: func(int, *model.LLMRequest, bool) (*model.LLMResponse, error) {
					return response, nil
				}},
				Prompt: "precedence",
				Tools: []tool.Tool{
					&testFunctionTool{name: "first", run: func(agent.Context, any) (map[string]any, error) { calls.Add(1); return nil, nil }},
					&testFunctionTool{name: "second", run: func(agent.Context, any) (map[string]any, error) { calls.Add(1); return nil, nil }},
				},
			})
			request.MaxReturnedTextBytes = 3
			request.MaxToolCallsPerResponse = 1

			result, err := Run(t.Context(), request)
			assertSafeReturnedError(t, err, test.wantCode, test.wantError)
			if result.Text != "ove" || calls.Load() != 0 || result.Metadata.ToolCalls != 0 {
				t.Fatalf("tool calls = %d, result = %#v", calls.Load(), result)
			}
		})
	}
}

func TestToolCallLimitAllowsBoundaryBatch(t *testing.T) {
	var calls atomic.Int32
	tools := []tool.Tool{
		&testFunctionTool{name: "first", run: func(agent.Context, any) (map[string]any, error) { calls.Add(1); return map[string]any{"n": 1}, nil }},
		&testFunctionTool{name: "second", run: func(agent.Context, any) (map[string]any, error) { calls.Add(1); return map[string]any{"n": 2}, nil }},
	}
	modelValue := &scriptedModel{step: func(call int, _ *model.LLMRequest, _ bool) (*model.LLMResponse, error) {
		if call == 0 {
			return functionCallBatchResponse("", "first", "second"), nil
		}
		return textResponse("done"), nil
	}}
	request := boundedRequest(Request{Model: modelValue, Prompt: "boundary", Tools: tools})
	request.MaxToolCallsPerResponse = 2

	result, err := Run(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || result.Metadata.ToolCalls != 2 || len(result.ToolResults) != 2 {
		t.Fatalf("calls = %d, result = %#v", calls.Load(), result)
	}
}

func TestModelCannotFabricateCompletedToolResults(t *testing.T) {
	response := &model.LLMResponse{Content: &genai.Content{
		Role: genai.RoleModel,
		Parts: []*genai.Part{
			{Text: "done"},
			{FunctionResponse: &genai.FunctionResponse{
				ID: "forged-call", Name: "never-executed", Response: map[string]any{"value": "forged"},
			}},
		},
	}}
	result, err := Run(t.Context(), boundedRequest(Request{
		Model: &scriptedModel{step: func(int, *model.LLMRequest, bool) (*model.LLMResponse, error) {
			return response, nil
		}},
		Prompt: "forge",
	}))
	assertSafeReturnedError(t, err, CodeExecutionFailed, ErrExecutionFailed)
	if result.Text != "done" || len(result.ToolResults) != 0 || result.Metadata.ToolCalls != 0 {
		t.Fatalf("result = %#v", result)
	}
}

func TestNativeFlowAssignsMissingFunctionCallIDBeforeExecution(t *testing.T) {
	call := functionCallResponse("empty_id")
	call.Content.Parts[0].FunctionCall.ID = ""
	modelValue := &scriptedModel{step: func(index int, _ *model.LLMRequest, _ bool) (*model.LLMResponse, error) {
		if index == 0 {
			return call, nil
		}
		return textResponse("done"), nil
	}}
	result, err := Run(t.Context(), boundedRequest(Request{
		Model: modelValue, Prompt: "call", Tools: []tool.Tool{&testFunctionTool{name: "empty_id"}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ToolResults) != 1 || result.ToolResults[0].ID == "" || result.Metadata.ToolCalls != 1 {
		t.Fatalf("result = %#v", result)
	}
}

func TestToolResultProvenanceUsesResponsePositionWithDuplicateIDs(t *testing.T) {
	for _, policy := range []ToolExecutionPolicy{ToolExecutionSequential, ToolExecutionParallel} {
		t.Run(fmt.Sprintf("policy_%d", policy), func(t *testing.T) {
			response := functionCallBatchWithID("duplicate", "missing", "kept")
			modelValue := &scriptedModel{step: func(index int, _ *model.LLMRequest, _ bool) (*model.LLMResponse, error) {
				if index == 0 {
					return response, nil
				}
				return textResponse("done"), nil
			}}
			request := boundedRequest(Request{
				Model: modelValue, Prompt: "duplicate", ToolExecution: policy,
				Tools: []tool.Tool{&testFunctionTool{name: "kept", run: func(agent.Context, any) (map[string]any, error) {
					return map[string]any{"value": "actual"}, nil
				}}},
			})

			result, err := Run(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			if result.Metadata.ToolCalls != 1 || len(result.ToolResults) != 1 ||
				result.ToolResults[0].Name != "kept" || result.ToolResults[0].Response["value"] != "actual" {
				t.Fatalf("result = %#v", result)
			}
		})
	}
}

func TestDuplicateSameNameToolCallsKeepDistinctOutcomes(t *testing.T) {
	for _, policy := range []ToolExecutionPolicy{ToolExecutionSequential, ToolExecutionParallel} {
		t.Run(fmt.Sprintf("policy_%d", policy), func(t *testing.T) {
			response := functionCallBatchWithID("duplicate", "same", "same")
			response.Content.Parts[0].FunctionCall.Args = map[string]any{"valid": false}
			response.Content.Parts[1].FunctionCall.Args = map[string]any{"valid": true}
			modelValue := &scriptedModel{step: func(index int, _ *model.LLMRequest, _ bool) (*model.LLMResponse, error) {
				if index == 0 {
					return response, nil
				}
				return textResponse("done"), nil
			}}
			toolValue := &testFunctionTool{name: "same", run: func(_ agent.Context, arguments any) (map[string]any, error) {
				values, _ := arguments.(map[string]any)
				if valid, _ := values["valid"].(bool); !valid {
					return nil, errors.New("malformed caller input")
				}
				return map[string]any{"value": "valid"}, nil
			}}
			request := boundedRequest(Request{
				Model: modelValue, Prompt: "same", Tools: []tool.Tool{toolValue}, ToolExecution: policy,
			})

			result, err := Run(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			if result.Metadata.ToolCalls != 2 || len(result.ToolResults) != 2 ||
				result.ToolResults[0].Response["error"] != "caller operation failed" ||
				result.ToolResults[1].Response["value"] != "valid" {
				t.Fatalf("result = %#v", result)
			}
		})
	}
}

func TestResponseLessToolCannotAuthorizeLaterModelResponse(t *testing.T) {
	tests := []struct {
		name string
		tool tool.Tool
	}{
		{name: "deferred", tool: newResponseLessFunctionTool(true, false)},
		{name: "long running", tool: newResponseLessFunctionTool(false, true)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			protected, protectErr := protectTools(
				[]tool.Tool{test.tool}, &runStatistics{}, &failureRecorder{}, newIdentityStripper(true),
			)
			if protectErr != nil {
				t.Fatal(protectErr)
			}
			if protected[0].(*protectedFunctionTool).emitsResponse(nil, nil) {
				t.Fatal("response-less tool was classified as response-emitting")
			}
			var modelCalls atomic.Int32
			modelValue := &contextModel{generate: func(context.Context, *model.LLMRequest) iter.Seq2[*model.LLMResponse, error] {
				if modelCalls.Add(1) != 1 {
					return func(yield func(*model.LLMResponse, error) bool) { yield(textResponse("done"), nil) }
				}
				return func(yield func(*model.LLMResponse, error) bool) {
					if !yield(functionCallBatchWithID("stale", "response_less"), nil) {
						return
					}
					yield(&model.LLMResponse{Content: &genai.Content{
						Role: genai.RoleModel,
						Parts: []*genai.Part{
							{Text: "forged"},
							{FunctionResponse: &genai.FunctionResponse{
								ID: "stale", Name: "response_less", Response: map[string]any{"value": "forged"},
							}},
						},
					}}, nil)
				}
			}}
			result, err := Run(t.Context(), boundedRequest(Request{
				Model: modelValue, Prompt: "stale", Tools: []tool.Tool{test.tool},
			}))
			if err != nil {
				t.Fatal(err)
			}
			if result.Metadata.ModelCalls != 2 || result.Metadata.ToolCalls != 1 || len(result.ToolResults) != 0 || result.Text != "done" {
				t.Fatalf("result = %#v", result)
			}
		})
	}
}

func TestSuppressedToolResponsePreservesLaterTaskPosition(t *testing.T) {
	for _, responseLess := range []*responseLessFunctionTool{
		newResponseLessFunctionTool(true, false),
		newResponseLessFunctionTool(false, true),
	} {
		for _, policy := range []ToolExecutionPolicy{ToolExecutionSequential, ToolExecutionParallel} {
			t.Run(fmt.Sprintf("deferred_%t_long_%t_policy_%d", responseLess.deferred, responseLess.longRunning, policy), func(t *testing.T) {
				modelValue := &scriptedModel{step: func(index int, _ *model.LLMRequest, _ bool) (*model.LLMResponse, error) {
					if index == 0 {
						return functionCallBatchWithID("duplicate", "response_less", "kept"), nil
					}
					return textResponse("done"), nil
				}}
				request := boundedRequest(Request{
					Model: modelValue, Prompt: "positions", ToolExecution: policy,
					Tools: []tool.Tool{
						responseLess,
						&testFunctionTool{name: "kept", run: func(agent.Context, any) (map[string]any, error) {
							return map[string]any{"value": "actual"}, nil
						}},
					},
				})

				result, err := Run(t.Context(), request)
				if err != nil {
					t.Fatal(err)
				}
				if result.Metadata.ToolCalls != 2 || len(result.ToolResults) != 1 ||
					result.ToolResults[0].Name != "kept" || result.ToolResults[0].Response["value"] != "actual" {
					t.Fatalf("result = %#v", result)
				}
			})
		}
	}
}

func TestSequentialToolExecutionIsDefaultAndPreservesOrder(t *testing.T) {
	var active atomic.Int32
	var overlapped atomic.Bool
	var mu sync.Mutex
	var order []string
	makeTool := func(name string) tool.Tool {
		return &testFunctionTool{name: name, run: func(agent.Context, any) (map[string]any, error) {
			if active.Add(1) != 1 {
				overlapped.Store(true)
			}
			mu.Lock()
			order = append(order, name)
			mu.Unlock()
			time.Sleep(5 * time.Millisecond)
			active.Add(-1)
			return map[string]any{"name": name}, nil
		}}
	}
	modelValue := batchThenFinalModel("done", "first", "second")
	request := boundedRequest(Request{
		Model: modelValue, Prompt: "sequential", Tools: []tool.Tool{makeTool("first"), makeTool("second")},
	})

	result, err := Run(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if overlapped.Load() || !slices.Equal(order, []string{"first", "second"}) {
		t.Fatalf("overlap = %t, order = %v", overlapped.Load(), order)
	}
	if got := toolResultNames(result.ToolResults); !slices.Equal(got, []string{"first", "second"}) {
		t.Fatalf("tool results = %v", got)
	}
}

func TestParallelToolExecutionRequiresExplicitPolicy(t *testing.T) {
	started := make(chan string, 2)
	completedTool := make(chan string, 2)
	releaseFirst := make(chan struct{})
	releaseSecond := make(chan struct{})
	makeTool := func(name string) tool.Tool {
		return &testFunctionTool{name: name, run: func(agent.Context, any) (map[string]any, error) {
			started <- name
			if name == "first" {
				<-releaseFirst
			} else {
				<-releaseSecond
			}
			completedTool <- name
			return map[string]any{"name": name}, nil
		}}
	}
	request := boundedRequest(Request{
		Model: batchThenFinalModel("done", "first", "second"), Prompt: "parallel",
		Tools: []tool.Tool{makeTool("first"), makeTool("second")}, ToolExecution: ToolExecutionParallel,
	})
	type runResult struct {
		result Result
		err    error
	}
	finished := make(chan runResult, 1)
	go func() {
		result, err := Run(t.Context(), request)
		finished <- runResult{result: result, err: err}
	}()

	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			close(releaseFirst)
			close(releaseSecond)
			t.Fatal("tool calls did not overlap")
		}
	}
	close(releaseSecond)
	if name := <-completedTool; name != "second" {
		t.Fatalf("first completed tool = %q", name)
	}
	close(releaseFirst)
	completed := <-finished
	if completed.err != nil {
		t.Fatal(completed.err)
	}
	if got := toolResultNames(completed.result.ToolResults); !slices.Equal(got, []string{"first", "second"}) {
		t.Fatalf("tool results = %v", got)
	}
}

func TestAllowedToolErrorsReturnToModelWithinLimits(t *testing.T) {
	const toolName = "failing"
	toolValue := &testFunctionTool{name: toolName, run: func(agent.Context, any) (map[string]any, error) {
		return nil, errors.New("caller secret")
	}}
	var received string
	modelValue := &scriptedModel{step: func(call int, request *model.LLMRequest, _ bool) (*model.LLMResponse, error) {
		if request.Config.MaxOutputTokens != 9 {
			return nil, fmt.Errorf("max output tokens = %d", request.Config.MaxOutputTokens)
		}
		if call == 0 {
			return functionCallResponse(toolName), nil
		}
		received = functionResponseError(request, toolName)
		return textResponse("handled"), nil
	}}
	request := boundedRequest(Request{Model: modelValue, Prompt: "error", Tools: []tool.Tool{toolValue}})
	request.MaxOutputTokens = 9
	request.MaxModelCalls = 2

	result, err := Run(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if received != "caller operation failed" || result.Text != "handled" || result.Metadata.ModelCalls != 2 {
		t.Fatalf("received = %q, result = %#v", received, result)
	}
	if len(result.ToolResults) != 1 || result.ToolResults[0].Response["error"] != "caller operation failed" {
		t.Fatalf("tool results = %#v", result.ToolResults)
	}
}

func TestErrorValuedToolResultsAreSanitizedAtCallerBoundary(t *testing.T) {
	tests := []struct {
		name      string
		policy    ToolExecutionPolicy
		failure   error
		wantCode  ErrorCode
		wantError error
	}{
		{name: "ordinary sequential", failure: errors.New("ordinary secret")},
		{name: "ordinary parallel", policy: ToolExecutionParallel, failure: errors.New("ordinary secret")},
		{name: "hostile sequential", failure: panicCallerError{method: "Error"}, wantCode: CodeToolPanic, wantError: ErrToolPanic},
		{name: "hostile parallel", policy: ToolExecutionParallel, failure: panicCallerError{method: "Error"}, wantCode: CodeToolPanic, wantError: ErrToolPanic},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var received string
			modelValue := &scriptedModel{step: func(call int, request *model.LLMRequest, _ bool) (*model.LLMResponse, error) {
				if call == 0 {
					return functionCallResponse("result_error"), nil
				}
				received = functionResponseError(request, "result_error")
				return textResponse("handled"), nil
			}}
			toolValue := &testFunctionTool{name: "result_error", run: func(agent.Context, any) (map[string]any, error) {
				return map[string]any{"error": test.failure}, nil
			}}
			request := boundedRequest(Request{
				Model: modelValue, Prompt: "error result", Tools: []tool.Tool{toolValue}, ToolExecution: test.policy,
			})
			result, err := Run(t.Context(), request)
			if test.wantCode == "" {
				if err != nil {
					t.Fatal(err)
				}
				if received != "caller operation failed" || result.Text != "handled" {
					t.Fatalf("received = %q, result = %#v", received, result)
				}
			} else {
				assertSafeReturnedError(t, err, test.wantCode, test.wantError, "CALLER_METHOD_SECRET")
			}
			if len(result.ToolResults) != 1 {
				t.Fatalf("tool results = %#v", result.ToolResults)
			}
			visible, _ := result.ToolResults[0].Response["error"].(string)
			if visible == "" || strings.Contains(visible, "secret") {
				t.Fatalf("visible tool error = %q", visible)
			}
		})
	}
}

func TestPartialToolResultsSurviveTransportFailure(t *testing.T) {
	modelValue := &scriptedModel{step: func(call int, _ *model.LLMRequest, _ bool) (*model.LLMResponse, error) {
		if call == 0 {
			return functionCallResponse("kept"), nil
		}
		return nil, errors.New("transport secret")
	}}
	request := boundedRequest(Request{
		Model: modelValue, Prompt: "partial", Tools: []tool.Tool{&testFunctionTool{name: "kept"}},
	})
	result, err := Run(t.Context(), request)
	assertSafeReturnedError(t, err, CodeExecutionFailed, ErrExecutionFailed, "transport secret")
	if len(result.ToolResults) != 1 || result.ToolResults[0].Name != "kept" || result.Metadata.ModelCalls != 2 {
		t.Fatalf("result = %#v", result)
	}
}

func TestPartialTextSurvivesEmptyResponseWithTransportFailure(t *testing.T) {
	modelValue := &scriptedModel{step: func(call int, _ *model.LLMRequest, _ bool) (*model.LLMResponse, error) {
		if call == 0 {
			return functionCallBatchResponse("prefix", "kept"), nil
		}
		return textResponse(""), errors.New("transport secret")
	}}
	request := boundedRequest(Request{
		Model: modelValue, Prompt: "partial", Tools: []tool.Tool{&testFunctionTool{name: "kept"}},
	})
	result, err := Run(t.Context(), request)
	assertSafeReturnedError(t, err, CodeExecutionFailed, ErrExecutionFailed, "transport secret")
	if result.Text != "prefix" || len(result.ToolResults) != 1 || result.ToolResults[0].Name != "kept" {
		t.Fatalf("result = %#v", result)
	}
}

func TestResponseRecorderUsesPartialStreamSemantics(t *testing.T) {
	recorder := &responseRecorder{}
	recorder.startModelCall()
	if recorder.record(textResponse("ab").Content, true, 3) {
		t.Fatal("first partial chunk overflowed")
	}
	if !recorder.record(textResponse("cd").Content, true, 3) || recorder.textValue() != "abc" {
		t.Fatalf("partial text = %q", recorder.textValue())
	}

	recorder.startModelCall()
	if recorder.record(textResponse("").Content, false, 3) || recorder.textValue() != "abc" {
		t.Fatalf("empty response replaced partial text: %q", recorder.textValue())
	}
	recorder.startModelCall()
	if recorder.record(textResponse("new").Content, false, 3) || recorder.textValue() != "new" {
		t.Fatalf("completed response did not replace partial text: %q", recorder.textValue())
	}
}

func TestPartialTextSurvivesIncompleteModelStream(t *testing.T) {
	response := textResponse("partial")
	response.Partial = true
	request := boundedRequest(Request{
		Model: &scriptedModel{step: func(int, *model.LLMRequest, bool) (*model.LLMResponse, error) {
			return response, nil
		}},
		Prompt: "partial",
	})

	result, err := Run(t.Context(), request)
	assertSafeReturnedError(t, err, CodeExecutionFailed, ErrExecutionFailed)
	if result.Text != "partial" || result.Metadata.ModelCalls != 1 {
		t.Fatalf("result = %#v", result)
	}
}

func TestPartialToolResultsSurviveCancellationAndCallerPanics(t *testing.T) {
	makeTool := func(panicRun bool) tool.Tool {
		return &testFunctionTool{name: "kept", panicRun: panicRun, run: func(agent.Context, any) (map[string]any, error) {
			return map[string]any{"value": "kept"}, nil
		}}
	}
	tests := []struct {
		name      string
		request   func(context.CancelFunc) Request
		wantCode  ErrorCode
		wantError error
	}{
		{
			name: "cancellation",
			request: func(cancel context.CancelFunc) Request {
				modelValue := &scriptedModel{step: func(call int, _ *model.LLMRequest, _ bool) (*model.LLMResponse, error) {
					if call == 0 {
						return functionCallResponse("kept"), nil
					}
					cancel()
					return nil, context.Canceled
				}}
				return boundedRequest(Request{Model: modelValue, Prompt: "partial", Tools: []tool.Tool{makeTool(false)}})
			},
			wantCode: CodeCanceled, wantError: ErrCanceled,
		},
		{
			name: "model panic",
			request: func(context.CancelFunc) Request {
				modelValue := &scriptedModel{step: func(call int, _ *model.LLMRequest, _ bool) (*model.LLMResponse, error) {
					if call == 0 {
						return functionCallResponse("kept"), nil
					}
					panic("MODEL_SECRET")
				}}
				return boundedRequest(Request{Model: modelValue, Prompt: "partial", Tools: []tool.Tool{makeTool(false)}})
			},
			wantCode: CodeModelPanic, wantError: ErrModelPanic,
		},
		{
			name: "tool panic",
			request: func(context.CancelFunc) Request {
				return boundedRequest(Request{
					Model: toolThenFinalModel("kept", "unreachable"), Prompt: "partial", Tools: []tool.Tool{makeTool(true)},
				})
			},
			wantCode: CodeToolPanic, wantError: ErrToolPanic,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			result, err := Run(ctx, test.request(cancel))
			assertSafeReturnedError(t, err, test.wantCode, test.wantError, "MODEL_SECRET", "TOPSECRET")
			if len(result.ToolResults) != 1 || result.ToolResults[0].Name != "kept" || result.Metadata.ToolCalls != 1 {
				t.Fatalf("result = %#v", result)
			}
		})
	}
}

func TestCyclicCallerErrorsAreBoundedAndCleanupRuns(t *testing.T) {
	tests := []struct {
		name     string
		request  func(error) Request
		wantCode ErrorCode
	}{
		{
			name: "model error",
			request: func(failure error) Request {
				return boundedRequest(Request{Model: errorModel{err: failure}, Prompt: "fail"})
			},
			wantCode: CodeExecutionFailed,
		},
		{
			name: "tool error",
			request: func(failure error) Request {
				return boundedRequest(Request{
					Model: toolThenFinalModel("cyclic", "handled"), Prompt: "call",
					Tools: []tool.Tool{&testFunctionTool{name: "cyclic", run: func(agent.Context, any) (map[string]any, error) {
						return nil, failure
					}}},
				})
			},
		},
		{
			name: "error-valued tool result",
			request: func(failure error) Request {
				return boundedRequest(Request{
					Model: toolThenFinalModel("cyclic", "handled"), Prompt: "call",
					Tools: []tool.Tool{&testFunctionTool{name: "cyclic", run: func(agent.Context, any) (map[string]any, error) {
						return map[string]any{"error": failure}, nil
					}}},
				})
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			failure := &cyclicCallerError{}
			service := newTracingSessionService()
			result, err := runWithSessionService(t.Context(), test.request(failure), service)
			if test.wantCode == "" {
				if err != nil || result.Text != "handled" {
					t.Fatalf("result = %#v, error = %v", result, err)
				}
			} else {
				assertSafeReturnedError(t, err, test.wantCode, ErrExecutionFailed)
			}
			service.assertLifecycle(t, 1)
		})
	}
}

func TestParallelTaskRunnerPropagatesEarliestPanicAfterWaiting(t *testing.T) {
	secondStarted := make(chan struct{})
	cleaned := false
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		defer func() { cleaned = true }()
		runnerContext := context.WithValue(t.Context(), toolExecutionContextKey{}, &runStatistics{})
		parallelTaskRunner(runnerContext, []func(context.Context){
			func(context.Context) {
				<-secondStarted
				panic("first task panic")
			},
			func(context.Context) {
				close(secondStarted)
				panic("second task panic")
			},
		})
	}()
	if recovered != "first task panic" || !cleaned {
		t.Fatalf("recovered = %#v, cleaned = %t", recovered, cleaned)
	}
}

func TestParallelCallerToolPanicRemainsTypedAndCleansUp(t *testing.T) {
	service := newTracingSessionService()
	request := boundedRequest(Request{
		Model: toolThenFinalModel("explode", "ignored"), Prompt: "panic",
		Tools:         []tool.Tool{&testFunctionTool{name: "explode", panicRun: true}},
		ToolExecution: ToolExecutionParallel,
	})
	result, err := runWithSessionService(t.Context(), request, service)
	assertSafeReturnedError(t, err, CodeToolPanic, ErrToolPanic, "TOPSECRET")
	if result.Metadata.ToolCalls != 1 || len(result.ToolResults) != 1 {
		t.Fatalf("result = %#v", result)
	}
	service.assertLifecycle(t, 1)
}

func TestLimitFailuresPreserveCleanupAndRecovery(t *testing.T) {
	tests := []struct {
		name    string
		request Request
		code    ErrorCode
	}{
		{
			name: "output truncation",
			request: boundedRequest(Request{Model: &scriptedModel{step: func(int, *model.LLMRequest, bool) (*model.LLMResponse, error) {
				response := textResponse("partial")
				response.FinishReason = genai.FinishReasonMaxTokens
				return response, nil
			}}, Prompt: "truncate"}),
			code: CodeOutputTruncated,
		},
		{
			name: "text truncation",
			request: func() Request {
				request := boundedRequest(Request{Model: finalModel("overflow"), Prompt: "truncate"})
				request.MaxReturnedTextBytes = 1
				return request
			}(),
			code: CodeTextTruncated,
		},
		{
			name: "model call limit",
			request: func() Request {
				request := boundedRequest(Request{
					Model: toolThenFinalModel("again", "unused"), Prompt: "limit",
					Tools: []tool.Tool{&testFunctionTool{name: "again"}},
				})
				request.MaxModelCalls = 1
				return request
			}(),
			code: CodeModelCallLimit,
		},
		{
			name: "tool call limit",
			request: func() Request {
				request := boundedRequest(Request{
					Model: &scriptedModel{step: func(int, *model.LLMRequest, bool) (*model.LLMResponse, error) {
						return functionCallBatchResponse("", "a", "b"), nil
					}}, Prompt: "limit",
					Tools: []tool.Tool{&testFunctionTool{name: "a"}, &testFunctionTool{name: "b"}},
				})
				request.MaxToolCallsPerResponse = 1
				return request
			}(),
			code: CodeToolCallLimit,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := newTracingSessionService()
			_, err := runWithSessionService(t.Context(), test.request, service)
			if CodeOf(err) != test.code {
				t.Fatalf("error = %v, code = %q", err, CodeOf(err))
			}
			service.assertLifecycle(t, 1)

			result, err := runWithSessionService(t.Context(), boundedRequest(Request{Model: finalModel("recovered"), Prompt: "again"}), service)
			if err != nil || result.Text != "recovered" {
				t.Fatalf("recovery result = %#v, error = %v", result, err)
			}
			service.assertLifecycle(t, 2)
		})
	}
}

func TestLimitFailuresJoinCleanupFailure(t *testing.T) {
	tests := []struct {
		name    string
		request func() Request
		code    ErrorCode
		primary error
	}{
		{
			name: "output truncation",
			request: func() Request {
				response := textResponse("partial")
				response.FinishReason = genai.FinishReasonMaxTokens
				return boundedRequest(Request{Model: &scriptedModel{step: func(int, *model.LLMRequest, bool) (*model.LLMResponse, error) {
					return response, nil
				}}, Prompt: "truncate"})
			},
			code: CodeOutputTruncated, primary: ErrOutputTruncated,
		},
		{
			name: "text truncation",
			request: func() Request {
				request := boundedRequest(Request{Model: finalModel("overflow"), Prompt: "truncate"})
				request.MaxReturnedTextBytes = 1
				return request
			},
			code: CodeTextTruncated, primary: ErrTextTruncated,
		},
		{
			name: "model call limit",
			request: func() Request {
				request := boundedRequest(Request{
					Model: toolThenFinalModel("again", "unused"), Prompt: "limit",
					Tools: []tool.Tool{&testFunctionTool{name: "again"}},
				})
				request.MaxModelCalls = 1
				return request
			},
			code: CodeModelCallLimit, primary: ErrModelCallLimit,
		},
		{
			name: "tool call limit",
			request: func() Request {
				request := boundedRequest(Request{
					Model: &scriptedModel{step: func(int, *model.LLMRequest, bool) (*model.LLMResponse, error) {
						return functionCallBatchResponse("", "a", "b"), nil
					}}, Prompt: "limit", Tools: []tool.Tool{&testFunctionTool{name: "a"}, &testFunctionTool{name: "b"}},
				})
				request.MaxToolCallsPerResponse = 1
				return request
			},
			code: CodeToolCallLimit, primary: ErrToolCallLimit,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := newTracingSessionService()
			service.deleteErr = errors.New("cleanup secret")
			_, err := runWithSessionService(t.Context(), test.request(), service)
			if CodeOf(err) != test.code || !errors.Is(err, test.primary) || !errors.Is(err, ErrCleanupFailed) {
				t.Fatalf("error = %v, code = %q", err, CodeOf(err))
			}
			assertSafeReturnedError(t, err, test.code, test.primary, "cleanup secret")
		})
	}
}

type contextModel struct {
	generate func(context.Context, *model.LLMRequest) iter.Seq2[*model.LLMResponse, error]
}

type cyclicCallerError struct{}

func (*cyclicCallerError) Error() string   { return "cyclic caller error" }
func (e *cyclicCallerError) Unwrap() error { return e }

type responseLessFunctionTool struct {
	testFunctionTool
	deferred    bool
	longRunning bool
}

func newResponseLessFunctionTool(deferred, longRunning bool) *responseLessFunctionTool {
	return &responseLessFunctionTool{
		testFunctionTool: testFunctionTool{
			name: "response_less",
			run:  func(agent.Context, any) (map[string]any, error) { return nil, nil },
		},
		deferred: deferred, longRunning: longRunning,
	}
}

func (t *responseLessFunctionTool) IsLongRunning() bool  { return t.longRunning }
func (t *responseLessFunctionTool) DefersResponse() bool { return t.deferred }
func (t *responseLessFunctionTool) ProcessRequest(_ agent.Context, request *model.LLMRequest) error {
	return toolutils.PackTool(request, t)
}

func (*contextModel) Name() string { return "context" }
func (m *contextModel) GenerateContent(ctx context.Context, request *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return m.generate(ctx, request)
}

func functionCallBatchResponse(text string, names ...string) *model.LLMResponse {
	parts := make([]*genai.Part, 0, len(names)+1)
	if text != "" {
		parts = append(parts, &genai.Part{Text: text})
	}
	for index, name := range names {
		parts = append(parts, &genai.Part{FunctionCall: &genai.FunctionCall{
			ID: fmt.Sprintf("call-%d", index+1), Name: name, Args: map[string]any{},
		}})
	}
	return &model.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: parts}}
}

func functionCallBatchWithID(id string, names ...string) *model.LLMResponse {
	response := functionCallBatchResponse("", names...)
	for _, part := range response.Content.Parts {
		part.FunctionCall.ID = id
	}
	return response
}

func batchThenFinalModel(final string, names ...string) model.LLM {
	return &scriptedModel{step: func(call int, _ *model.LLMRequest, _ bool) (*model.LLMResponse, error) {
		if call == 0 {
			return functionCallBatchResponse("", names...), nil
		}
		return textResponse(final), nil
	}}
}

func toolResultNames(results []ToolResult) []string {
	names := make([]string, len(results))
	for index, result := range results {
		names[index] = result.Name
	}
	return names
}

var _ model.LLM = (*contextModel)(nil)
