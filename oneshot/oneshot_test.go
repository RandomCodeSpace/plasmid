package oneshot

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/toolutils"
	"google.golang.org/genai"
)

func TestRunForwardsExactInputsAndCollectsMetadata(t *testing.T) {
	var captured *model.LLMRequest
	var streamed bool
	modelValue := &scriptedModel{step: func(call int, request *model.LLMRequest, stream bool) (*model.LLMResponse, error) {
		if call != 0 {
			return nil, fmt.Errorf("unexpected model call %d", call)
		}
		captured = request
		streamed = stream
		return &model.LLMResponse{
			Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{
				{Text: "private", Thought: true}, {Text: "exact "}, {Text: "answer"},
			}},
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount: 7, CandidatesTokenCount: 3, TotalTokenCount: 10,
			},
		}, nil
	}}
	toolValue := &testFunctionTool{name: "lookup", description: "look up a value"}
	request := Request{
		Model: modelValue, Instruction: "keep {braces} literal", Prompt: "do the exact task", Tools: []tool.Tool{toolValue},
	}

	result, err := Run(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if streamed {
		t.Fatal("model received a streaming request")
	}
	if got := systemInstruction(captured); got != request.Instruction {
		t.Fatalf("instruction = %q, want %q", got, request.Instruction)
	}
	if got := latestUserText(captured); got != request.Prompt {
		t.Fatalf("prompt = %q, want %q", got, request.Prompt)
	}
	if got := requestToolNames(captured); !slices.Equal(got, []string{"lookup"}) {
		t.Fatalf("tools = %v, want [lookup]", got)
	}
	if result.Text != "exact answer" {
		t.Fatalf("text = %q", result.Text)
	}
	wantMetadata := Metadata{ModelCalls: 1, Usage: Usage{InputTokens: 7, OutputTokens: 3, TotalTokens: 10}}
	if result.Metadata != wantMetadata {
		t.Fatalf("metadata = %#v, want %#v", result.Metadata, wantMetadata)
	}
}

func TestRunWithNoToolsExposesNoTools(t *testing.T) {
	modelValue := &scriptedModel{step: func(_ int, request *model.LLMRequest, _ bool) (*model.LLMResponse, error) {
		if len(request.Tools) != 0 {
			return nil, fmt.Errorf("request tools = %v", requestToolNames(request))
		}
		if request.Config != nil && len(request.Config.Tools) != 0 {
			return nil, fmt.Errorf("generate config exposed %d tools", len(request.Config.Tools))
		}
		return textResponse("done"), nil
	}}
	result, err := Run(t.Context(), Request{Model: modelValue, Prompt: "no tools"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "done" || result.Metadata.ToolCalls != 0 {
		t.Fatalf("result = %#v", result)
	}
}

func TestToolRequestInstructionAugmentationIsPreserved(t *testing.T) {
	const (
		callerInstruction = "Keep {literal_braces} unchanged."
		toolInstruction   = "Native tool policy applies."
	)
	var receivedInstruction string
	modelValue := &scriptedModel{step: func(_ int, request *model.LLMRequest, _ bool) (*model.LLMResponse, error) {
		receivedInstruction = systemInstruction(request)
		return textResponse("done"), nil
	}}
	toolValue := &instructionAugmentingTool{
		testFunctionTool: testFunctionTool{name: "augment_instruction"},
		instruction:      toolInstruction,
	}
	result, err := Run(t.Context(), Request{
		Model: modelValue, Instruction: callerInstruction, Prompt: "answer", Tools: []tool.Tool{toolValue},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := callerInstruction + "\n\n" + toolInstruction
	if receivedInstruction != want {
		t.Fatalf("instruction = %q, want %q", receivedInstruction, want)
	}
	if result.Text != "done" {
		t.Fatalf("result = %#v", result)
	}
}

func TestAgentIdentityIsRemovedBeforeNativeToolProcessors(t *testing.T) {
	identity := fmt.Sprintf("You are an agent. Your internal name is %q.", agentName)
	tests := []struct {
		name        string
		instruction string
		mutate      func(string) string
		want        string
	}{
		{
			name:        "tool prepends identical identity",
			instruction: "caller",
			mutate:      func(value string) string { return identity + "\n\n" + value },
			want:        identity + "\n\ncaller",
		},
		{
			name:        "tool inserts before identity suffix",
			instruction: "caller",
			mutate: func(value string) string {
				suffix := "\n\n" + identity
				if strings.HasSuffix(value, suffix) {
					return strings.TrimSuffix(value, suffix) + "\n\ninserted" + suffix
				}
				return value + "\n\ninserted"
			},
			want: "caller\n\ninserted",
		},
		{
			name:        "empty caller instruction",
			instruction: "",
			mutate:      func(string) string { return "tool only" },
			want:        "tool only",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var processorInput string
			var modelInput string
			toolValue := &instructionMutatingTool{
				testFunctionTool: testFunctionTool{name: "mutate_instruction"},
				seen:             &processorInput,
				mutate:           test.mutate,
			}
			modelValue := &scriptedModel{step: func(_ int, request *model.LLMRequest, _ bool) (*model.LLMResponse, error) {
				modelInput = systemInstruction(request)
				return textResponse("done"), nil
			}}

			_, err := Run(t.Context(), Request{
				Model: modelValue, Instruction: test.instruction, Prompt: "answer", Tools: []tool.Tool{toolValue},
			})
			if err != nil {
				t.Fatal(err)
			}
			if processorInput != test.instruction {
				t.Fatalf("processor instruction = %q, want caller instruction %q", processorInput, test.instruction)
			}
			if modelInput != test.want {
				t.Fatalf("model instruction = %q, want %q", modelInput, test.want)
			}
		})
	}
}

func TestAgentIdentityIsRemovedOnlyBeforeFirstNativeToolProcessor(t *testing.T) {
	identity := fmt.Sprintf("You are an agent. Your internal name is %q.", agentName)
	var firstInput string
	first := &instructionMutatingTool{
		testFunctionTool: testFunctionTool{name: "first_processor"},
		seen:             &firstInput,
		mutate:           func(value string) string { return value + "\n\n" + identity },
	}
	var secondInput string
	second := &instructionMutatingTool{
		testFunctionTool: testFunctionTool{name: "second_processor"},
		seen:             &secondInput,
		mutate:           func(value string) string { return value + "\n\nsecond" },
	}
	var modelInput string
	modelValue := &scriptedModel{step: func(_ int, request *model.LLMRequest, _ bool) (*model.LLMResponse, error) {
		modelInput = systemInstruction(request)
		return textResponse("done"), nil
	}}

	_, err := Run(t.Context(), Request{
		Model: modelValue, Instruction: "caller", Prompt: "answer", Tools: []tool.Tool{first, second},
	})
	if err != nil {
		t.Fatal(err)
	}
	if firstInput != "caller" {
		t.Fatalf("first processor instruction = %q", firstInput)
	}
	wantSecond := "caller\n\n" + identity
	if secondInput != wantSecond {
		t.Fatalf("second processor instruction = %q, want %q", secondInput, wantSecond)
	}
	if want := wantSecond + "\n\nsecond"; modelInput != want {
		t.Fatalf("model instruction = %q, want %q", modelInput, want)
	}
}

func TestAgentIdentityIsRemovedWithoutTools(t *testing.T) {
	for _, instruction := range []string{"caller", ""} {
		t.Run(fmt.Sprintf("instruction_%q", instruction), func(t *testing.T) {
			var modelInput string
			modelValue := &scriptedModel{step: func(_ int, request *model.LLMRequest, _ bool) (*model.LLMResponse, error) {
				modelInput = systemInstruction(request)
				return textResponse("done"), nil
			}}
			_, err := Run(t.Context(), Request{Model: modelValue, Instruction: instruction, Prompt: "answer"})
			if err != nil {
				t.Fatal(err)
			}
			if modelInput != instruction {
				t.Fatalf("model instruction = %q, want %q", modelInput, instruction)
			}
		})
	}
}

func TestCallerToolOwnsItsFilesystemSideEffect(t *testing.T) {
	target := filepath.Join(t.TempDir(), "caller-owned.txt")
	toolValue := &testFunctionTool{name: "write_owned", run: func(agent.Context, any) (map[string]any, error) {
		if err := os.WriteFile(target, []byte("caller data"), 0o600); err != nil {
			return nil, err
		}
		return map[string]any{"path": target}, nil
	}}
	modelValue := toolThenFinalModel("write_owned", "finished")

	result, err := Run(t.Context(), Request{Model: modelValue, Instruction: "use only the supplied tool", Prompt: "write", Tools: []tool.Tool{toolValue}})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "caller data" {
		t.Fatalf("side effect = %q", data)
	}
	if result.Text != "finished" || result.Metadata.ModelCalls != 2 || result.Metadata.ToolCalls != 1 {
		t.Fatalf("result = %#v", result)
	}
}

func TestRunAggregatesUsageAcrossToolTurn(t *testing.T) {
	modelValue := &scriptedModel{step: func(call int, _ *model.LLMRequest, _ bool) (*model.LLMResponse, error) {
		if call == 0 {
			response := functionCallResponse("echo")
			response.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 3, CandidatesTokenCount: 4, TotalTokenCount: 7}
			return response, nil
		}
		response := textResponse("done")
		response.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 5, CandidatesTokenCount: 6, TotalTokenCount: 11}
		return response, nil
	}}
	result, err := Run(t.Context(), Request{Model: modelValue, Prompt: "echo", Tools: []tool.Tool{&testFunctionTool{name: "echo"}}})
	if err != nil {
		t.Fatal(err)
	}
	want := Metadata{ModelCalls: 2, ToolCalls: 1, Usage: Usage{InputTokens: 8, OutputTokens: 10, TotalTokens: 18}}
	if result.Metadata != want {
		t.Fatalf("metadata = %#v, want %#v", result.Metadata, want)
	}
}

func TestRunAcceptsAnEmptyFinalResponse(t *testing.T) {
	modelValue := &scriptedModel{step: func(int, *model.LLMRequest, bool) (*model.LLMResponse, error) {
		return &model.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{InlineData: &genai.Blob{MIMEType: "text/plain", Data: []byte("non-text")}}}}}, nil
	}}
	result, err := Run(t.Context(), Request{Model: modelValue, Prompt: "answer"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "" || result.Metadata.ModelCalls != 1 {
		t.Fatalf("result = %#v", result)
	}
}

func TestRunReturnsTypedNoFinalResponse(t *testing.T) {
	result, err := Run(t.Context(), Request{Model: emptyModel{}, Prompt: "answer"})
	if CodeOf(err) != CodeNoFinalResponse || !errors.Is(err, ErrNoFinalResponse) {
		t.Fatalf("error = %v, code = %q", err, CodeOf(err))
	}
	if result.Metadata.ModelCalls != 1 {
		t.Fatalf("metadata = %#v", result.Metadata)
	}
}

func TestRunValidatesCallerInputs(t *testing.T) {
	var typedNil *scriptedModel
	tests := []struct {
		name    string
		ctx     context.Context
		request Request
	}{
		{name: "nil context", request: Request{Model: finalModel("done")}},
		{name: "nil model", ctx: t.Context(), request: Request{}},
		{name: "typed nil model", ctx: t.Context(), request: Request{Model: typedNil}},
		{name: "nil tool", ctx: t.Context(), request: Request{Model: finalModel("done"), Tools: []tool.Tool{nil}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Run(test.ctx, test.request)
			if CodeOf(err) != CodeInvalidArgument || !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("error = %v, code = %q", err, CodeOf(err))
			}
		})
	}
}

func TestRunDeletesSessionAndAllowsIndependentRecovery(t *testing.T) {
	tests := []struct {
		name    string
		context func() context.Context
		request func() Request
		code    ErrorCode
	}{
		{
			name: "model error", context: t.Context,
			request: func() Request { return Request{Model: errorModel{err: errors.New("transport failed")}, Prompt: "fail"} },
			code:    CodeExecutionFailed,
		},
		{
			name:    "cancellation",
			context: func() context.Context { ctx, cancel := context.WithCancel(t.Context()); cancel(); return ctx },
			request: func() Request { return Request{Model: cancellationModel{}, Prompt: "cancel"} },
			code:    CodeCanceled,
		},
		{
			name: "lazy model panic", context: t.Context,
			request: func() Request { return Request{Model: lazyPanicModel{}, Prompt: "panic"} },
			code:    CodeModelPanic,
		},
		{
			name: "tool panic", context: t.Context,
			request: func() Request {
				return Request{Model: toolThenFinalModel("explode", "ignored"), Prompt: "panic", Tools: []tool.Tool{&testFunctionTool{name: "explode", panicRun: true}}}
			},
			code: CodeToolPanic,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := newTracingSessionService()
			_, err := runWithSessionService(test.context(), test.request(), service)
			if CodeOf(err) != test.code {
				t.Fatalf("error = %v, code = %q, want %q", err, CodeOf(err), test.code)
			}
			if strings.Contains(fmt.Sprint(err), "TOPSECRET") {
				t.Fatalf("panic value leaked: %v", err)
			}
			service.assertLifecycle(t, 1)

			result, recoveryErr := runWithSessionService(t.Context(), Request{Model: finalModel("recovered"), Prompt: "again"}, service)
			if recoveryErr != nil {
				t.Fatalf("subsequent run: %v", recoveryErr)
			}
			if result.Text != "recovered" {
				t.Fatalf("subsequent result = %#v", result)
			}
			service.assertLifecycle(t, 2)
		})
	}
}

func TestCancellationErrorPreservesCanonicalMatch(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, modelValue := range []model.LLM{cancellationModel{}, wrappedCancellationModel{}} {
		_, err := Run(ctx, Request{Model: modelValue, Prompt: "cancel"})
		if CodeOf(err) != CodeCanceled || !errors.Is(err, ErrCanceled) || !errors.Is(err, context.Canceled) {
			t.Fatalf("model %T: error = %v, code = %q", modelValue, err, CodeOf(err))
		}
	}
}

func TestCallerBoundaryPanicsAreTypedAndRedacted(t *testing.T) {
	tests := []struct {
		name      string
		request   func() Request
		wantCode  ErrorCode
		lifecycle bool
	}{
		{
			name: "model name", request: func() Request { return Request{Model: panicNameModel{}, Prompt: "panic"} },
			wantCode: CodeModelPanic,
		},
		{
			name: "eager model call", request: func() Request { return Request{Model: eagerPanicModel{}, Prompt: "panic"} },
			wantCode: CodeModelPanic, lifecycle: true,
		},
		{
			name: "tool metadata", request: func() Request {
				return Request{Model: finalModel("unused"), Prompt: "panic", Tools: []tool.Tool{panicNameTool{}}}
			},
			wantCode: CodeToolPanic,
		},
		{
			name: "tool request processor", request: func() Request {
				return Request{Model: finalModel("unused"), Prompt: "panic", Tools: []tool.Tool{&panicProcessorTool{}}}
			},
			wantCode: CodeToolPanic, lifecycle: true,
		},
		{
			name: "lazy streaming tool", request: func() Request {
				return Request{Model: toolThenFinalModel("stream_panic", "ignored"), Prompt: "panic", Tools: []tool.Tool{&panicStreamingTool{}}}
			},
			wantCode: CodeToolPanic, lifecycle: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := newTracingSessionService()
			_, err := runWithSessionService(t.Context(), test.request(), service)
			if CodeOf(err) != test.wantCode {
				t.Fatalf("error = %v, code = %q, want %q", err, CodeOf(err), test.wantCode)
			}
			if strings.Contains(fmt.Sprint(err), "TOPSECRET") {
				t.Fatalf("panic value leaked: %v", err)
			}
			if test.lifecycle {
				service.assertLifecycle(t, 1)
			}
		})
	}
}

func TestProtectedFunctionToolPanicsWithQuotedNamesAreRedacted(t *testing.T) {
	tests := []struct {
		name     string
		toolName string
	}{
		{name: "quote", toolName: "quote\"tool"},
		{name: "backslash", toolName: "backslash\\tool"},
		{name: "control", toolName: "control\n\ttool"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			toolValue := &testFunctionTool{name: test.toolName, panicRun: true}
			_, err := Run(t.Context(), Request{
				Model: toolThenFinalModel(test.toolName, "ignored"), Prompt: "panic", Tools: []tool.Tool{toolValue},
			})
			if CodeOf(err) != CodeToolPanic || !errors.Is(err, ErrToolPanic) {
				t.Fatalf("error = %v, code = %q", err, CodeOf(err))
			}
			if message := fmt.Sprint(err); strings.Contains(message, "TOPSECRET") || strings.Contains(message, "stack:") {
				t.Fatalf("panic detail leaked: %v", err)
			}
		})
	}
}

func TestOrdinaryPrefixLikeToolErrorRemainsOrdinary(t *testing.T) {
	const toolName = "ordinary\"tool"
	wantToolError := fmt.Sprintf(
		"panic in tool %q: ordinary failure\nstack: goroutine 999 [running]:\nordinary.frame()",
		toolName,
	)
	toolValue := &testFunctionTool{name: toolName, run: func(agent.Context, any) (map[string]any, error) {
		return nil, errors.New(wantToolError)
	}}
	var receivedToolError string
	modelValue := &scriptedModel{step: func(call int, request *model.LLMRequest, _ bool) (*model.LLMResponse, error) {
		if call == 0 {
			return functionCallResponse(toolName), nil
		}
		receivedToolError = functionResponseError(request, toolName)
		return textResponse("handled"), nil
	}}

	result, err := Run(t.Context(), Request{Model: modelValue, Prompt: "call", Tools: []tool.Tool{toolValue}})
	if err != nil {
		t.Fatalf("ordinary error was promoted to invocation failure: %v", err)
	}
	if receivedToolError != "caller operation failed" {
		t.Fatalf("tool error = %q", receivedToolError)
	}
	if strings.Contains(receivedToolError, wantToolError) {
		t.Fatalf("caller error leaked: %q", receivedToolError)
	}
	if result.Text != "handled" || result.Metadata.ToolCalls != 1 {
		t.Fatalf("result = %#v", result)
	}
}

func TestProtectedToolFromPreviousRunUsesCurrentInvocationRecorders(t *testing.T) {
	source := &testFunctionTool{name: "retained_tool"}
	var retained tool.Tool
	firstModel := &scriptedModel{step: func(_ int, request *model.LLMRequest, _ bool) (*model.LLMResponse, error) {
		registered, ok := request.Tools[source.name].(tool.Tool)
		if !ok {
			return nil, errors.New("protected tool was not registered")
		}
		retained = registered
		return textResponse("first"), nil
	}}
	first, err := Run(t.Context(), Request{Model: firstModel, Prompt: "capture", Tools: []tool.Tool{source}})
	if err != nil {
		t.Fatal(err)
	}
	if first.Text != "first" || retained == nil {
		t.Fatalf("first result = %#v, retained = %T", first, retained)
	}

	source.panicRun = true
	second, err := Run(t.Context(), Request{
		Model: toolThenFinalModel(source.name, "ignored"), Prompt: "call", Tools: []tool.Tool{retained},
	})
	assertSafeReturnedError(t, err, CodeToolPanic, ErrToolPanic, "TOPSECRET", "stack:")
	if second.Metadata.ToolCalls != 1 {
		t.Fatalf("second metadata = %#v", second.Metadata)
	}
}

func TestCallerErrorMethodsCannotEscapeBoundaries(t *testing.T) {
	tests := []struct {
		name     string
		failure  error
		request  func(error) Request
		wantCode ErrorCode
		wantErr  error
	}{
		{
			name: "model error", failure: panicCallerError{method: "Error"},
			request:  func(failure error) Request { return Request{Model: errorModel{err: failure}, Prompt: "fail"} },
			wantCode: CodeModelPanic, wantErr: ErrModelPanic,
		},
		{
			name: "model is", failure: panicCallerError{method: "Is"},
			request:  func(failure error) Request { return Request{Model: errorModel{err: failure}, Prompt: "fail"} },
			wantCode: CodeModelPanic, wantErr: ErrModelPanic,
		},
		{
			name: "processor error", failure: panicCallerError{method: "Error"},
			request: func(failure error) Request {
				return Request{Model: finalModel("unused"), Prompt: "fail", Tools: []tool.Tool{&errorRequestTool{err: failure}}}
			},
			wantCode: CodeToolPanic, wantErr: ErrToolPanic,
		},
		{
			name: "processor is", failure: panicCallerError{method: "Is"},
			request: func(failure error) Request {
				return Request{Model: finalModel("unused"), Prompt: "fail", Tools: []tool.Tool{&errorRequestTool{err: failure}}}
			},
			wantCode: CodeToolPanic, wantErr: ErrToolPanic,
		},
		{
			name: "function error", failure: panicCallerError{method: "Error"},
			request: func(failure error) Request {
				return Request{
					Model: toolThenFinalModel("error_function", "ignored"), Prompt: "call",
					Tools: []tool.Tool{&testFunctionTool{name: "error_function", run: func(agent.Context, any) (map[string]any, error) {
						return nil, failure
					}}},
				}
			},
			wantCode: CodeToolPanic, wantErr: ErrToolPanic,
		},
		{
			name: "streaming error", failure: panicCallerError{method: "Error"},
			request: func(failure error) Request {
				return Request{
					Model: toolThenFinalModel("error_stream", "ignored"), Prompt: "call",
					Tools: []tool.Tool{&testStreamingTool{name: "error_stream", err: failure}},
				}
			},
			wantCode: CodeToolPanic, wantErr: ErrToolPanic,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := Run(t.Context(), test.request(test.failure))
			assertSafeReturnedError(t, err, test.wantCode, test.wantErr, "CALLER_METHOD_SECRET", "stack:")
			if strings.Contains(test.name, "function") || strings.Contains(test.name, "streaming") {
				if result.Metadata.ToolCalls != 1 {
					t.Fatalf("metadata = %#v", result.Metadata)
				}
			}
		})
	}
}

func TestRequestProcessorDelegatesRemainProtected(t *testing.T) {
	tests := []struct {
		name     string
		callName string
		tool     tool.Tool
	}{
		{
			name:     "overwrite same name",
			callName: "same_name",
			tool: &overwritingFunctionTool{
				testFunctionTool: testFunctionTool{name: "same_name"},
				delegate:         &testFunctionTool{name: "same_name", panicRun: true},
			},
		},
		{
			name:     "add function under different name",
			callName: "added_function",
			tool: &registeringRequestTool{
				name: "function_registrar", delegate: &testFunctionTool{name: "added_function", panicRun: true},
			},
		},
		{
			name:     "add streaming tool under different name",
			callName: "added_stream",
			tool: &registeringRequestTool{
				name: "stream_registrar", delegate: &testStreamingTool{name: "added_stream", panicRun: true},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := Run(t.Context(), Request{
				Model: toolThenFinalModel(test.callName, "ignored"), Prompt: "call", Tools: []tool.Tool{test.tool},
			})
			if CodeOf(err) != CodeToolPanic || !errors.Is(err, ErrToolPanic) {
				t.Fatalf("error = %v, code = %q", err, CodeOf(err))
			}
			if result.Metadata.ToolCalls != 1 {
				t.Fatalf("metadata = %#v", result.Metadata)
			}
			if message := fmt.Sprint(err); strings.Contains(message, "TOPSECRET") || strings.Contains(message, "stack:") {
				t.Fatalf("panic detail leaked: %v", err)
			}
		})
	}
}

func TestToolMetadataIsLazyAndDynamic(t *testing.T) {
	probe := &dynamicMetadataTool{name: "dynamic"}
	protected, err := protectTools(
		[]tool.Tool{probe}, &runStatistics{}, &failureRecorder{}, newIdentityStripper(true),
	)
	if err != nil {
		t.Fatal(err)
	}
	if probe.nameCalls != 0 || probe.descriptionCalls != 0 || probe.longRunningCalls != 0 ||
		probe.declarationCalls != 0 || probe.defersCalls != 0 {
		t.Fatalf("eager metadata calls = %#v", probe)
	}
	wrapped := protected[0]
	if first, second := wrapped.Name(), wrapped.Name(); first != "dynamic-1" || second != "dynamic-2" {
		t.Fatalf("names = %q, %q", first, second)
	}
	if first, second := wrapped.Description(), wrapped.Description(); first != "description-1" || second != "description-2" {
		t.Fatalf("descriptions = %q, %q", first, second)
	}
	if first, second := wrapped.IsLongRunning(), wrapped.IsLongRunning(); !first || second {
		t.Fatalf("long-running states = %t, %t", first, second)
	}
	declaration := wrapped.(declarer)
	if first, second := declaration.Declaration(), declaration.Declaration(); first.Description != "declaration-1" || second.Description != "declaration-2" {
		t.Fatalf("declarations = %#v, %#v", first, second)
	}
	deferrer := wrapped.(responseDeferrer)
	if first, second := deferrer.DefersResponse(), deferrer.DefersResponse(); !first || second {
		t.Fatalf("deferred states = %t, %t", first, second)
	}
}

func TestProtectedToolMetadataPanicsAreTypedAndRedacted(t *testing.T) {
	const secret = "METADATA_SECRET"
	failures := &failureRecorder{}
	protected, err := protectTools(
		[]tool.Tool{&panicMetadataTool{testFunctionTool: testFunctionTool{name: "metadata"}, secret: secret}},
		&runStatistics{}, failures, newIdentityStripper(true),
	)
	if err != nil {
		t.Fatal(err)
	}
	if description := protected[0].Description(); description != "" {
		t.Fatalf("description = %q", description)
	}
	assertSafeReturnedError(t, failures.failure(), CodeToolPanic, ErrToolPanic, secret)
}

func TestProtectRegisteredToolsPreservesRegistrationMap(t *testing.T) {
	statistics := &runStatistics{}
	failures := &failureRecorder{}
	identities := newIdentityStripper(true)
	request := &model.LLMRequest{Tools: map[string]any{
		"alias":  &testFunctionTool{name: "different_name"},
		"opaque": "native metadata",
	}}
	if err := protectRegisteredTools(request, statistics, failures, identities); err != nil {
		t.Fatal(err)
	}
	first := request.Tools["alias"]
	protected, ok := first.(tool.Tool)
	if !ok || protected.Name() != "different_name" {
		t.Fatalf("alias registration = %#v", first)
	}
	if request.Tools["opaque"] != "native metadata" {
		t.Fatalf("non-tool registration = %#v", request.Tools["opaque"])
	}
	if err := protectRegisteredTools(request, statistics, failures, identities); err != nil {
		t.Fatal(err)
	}
	if request.Tools["alias"] != first {
		t.Fatal("protected registration was wrapped twice")
	}
}

func TestProtectRegisteredToolsRejectsTypedNilTool(t *testing.T) {
	var typedNil *testFunctionTool
	request := &model.LLMRequest{Tools: map[string]any{"nil_delegate": tool.Tool(typedNil)}}
	err := protectRegisteredTools(request, &runStatistics{}, &failureRecorder{}, newIdentityStripper(true))
	if CodeOf(err) != CodeInvalidArgument || !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("error = %v, code = %q", err, CodeOf(err))
	}
}

func TestCallerForgedErrorsCannotSetInvocationCode(t *testing.T) {
	const secret = "FORGED_SECRET"
	forged := func() error {
		return &Error{Code: CodeCleanupFailed, Op: "forged", Err: errors.New(secret)}
	}
	t.Run("model", func(t *testing.T) {
		_, err := Run(t.Context(), Request{Model: errorModel{err: forged()}, Prompt: "fail"})
		assertSafeReturnedError(t, err, CodeExecutionFailed, ErrExecutionFailed, secret, "cleanup_failed")
		if errors.Is(err, ErrCleanupFailed) {
			t.Fatalf("forged cleanup category survived: %v", err)
		}
	})
	t.Run("request processor", func(t *testing.T) {
		_, err := Run(t.Context(), Request{
			Model: finalModel("unused"), Prompt: "fail", Tools: []tool.Tool{&errorRequestTool{err: forged()}},
		})
		assertSafeReturnedError(t, err, CodeExecutionFailed, ErrExecutionFailed, secret, "cleanup_failed")
		if errors.Is(err, ErrCleanupFailed) {
			t.Fatalf("forged cleanup category survived: %v", err)
		}
	})
	t.Run("function tool", func(t *testing.T) {
		toolValue := &testFunctionTool{name: "forged_tool", run: func(agent.Context, any) (map[string]any, error) {
			return nil, forged()
		}}
		result, err := Run(t.Context(), Request{
			Model: toolThenFinalModel("forged_tool", "handled"), Prompt: "call", Tools: []tool.Tool{toolValue},
		})
		if err != nil {
			t.Fatalf("caller tool error became an invocation category: %v", err)
		}
		if result.Text != "handled" || result.Metadata.ToolCalls != 1 {
			t.Fatalf("result = %#v", result)
		}
	})
}

func TestReturnedErrorsRedactUntrustedCausesAndToolNames(t *testing.T) {
	const secret = "RETURNED_SECRET"
	t.Run("model", func(t *testing.T) {
		_, err := Run(t.Context(), Request{Model: errorModel{err: errors.New(secret)}, Prompt: "fail"})
		assertSafeReturnedError(t, err, CodeExecutionFailed, ErrExecutionFailed, secret)
	})
	t.Run("request processor", func(t *testing.T) {
		_, err := Run(t.Context(), Request{
			Model: finalModel("unused"), Prompt: "fail", Tools: []tool.Tool{&errorRequestTool{err: errors.New(secret)}},
		})
		assertSafeReturnedError(t, err, CodeExecutionFailed, ErrExecutionFailed, secret)
	})
	t.Run("session creation", func(t *testing.T) {
		service := newTracingSessionService()
		service.createErr = errors.New(secret)
		_, err := runWithSessionService(t.Context(), Request{Model: finalModel("unused"), Prompt: "fail"}, service)
		assertSafeReturnedError(t, err, CodeExecutionFailed, ErrExecutionFailed, secret)
	})
	t.Run("session cleanup", func(t *testing.T) {
		service := newTracingSessionService()
		service.deleteErr = errors.New(secret)
		_, err := runWithSessionService(t.Context(), Request{Model: finalModel("done"), Prompt: "answer"}, service)
		assertSafeReturnedError(t, err, CodeCleanupFailed, ErrCleanupFailed, secret)
	})
	t.Run("tool panic and name", func(t *testing.T) {
		toolName := secret + "\n\tcontrol"
		result, err := Run(t.Context(), Request{
			Model: toolThenFinalModel(toolName, "ignored"), Prompt: "panic",
			Tools: []tool.Tool{&testFunctionTool{name: toolName, panicRun: true}},
		})
		if result.Metadata.ToolCalls != 1 {
			t.Fatalf("metadata = %#v", result.Metadata)
		}
		assertSafeReturnedError(t, err, CodeToolPanic, ErrToolPanic, secret, "control", "TOPSECRET", "stack:")
	})
}

func TestCallerGuardsDoNotRecoverConsumerPanics(t *testing.T) {
	statistics := &runStatistics{}
	failures := &failureRecorder{}
	guarded, err := protectModel(finalModel("done"), statistics, failures)
	if err != nil {
		t.Fatal(err)
	}
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		guarded.GenerateContent(t.Context(), &model.LLMRequest{}, false)(func(*model.LLMResponse, error) bool {
			panic("consumer defect")
		})
	}()
	if recovered != "consumer defect" {
		t.Fatalf("recovered = %#v", recovered)
	}
	if failure := failures.failure(); failure != nil {
		t.Fatalf("consumer panic was recorded as caller failure: %v", failure)
	}
}

func TestRunJoinsCleanupFailure(t *testing.T) {
	const modelSecret = "MODEL_JOIN_SECRET"
	const cleanupSecret = "CLEANUP_JOIN_SECRET"
	service := newTracingSessionService()
	service.deleteErr = errors.New(cleanupSecret)
	_, err := runWithSessionService(t.Context(), Request{Model: errorModel{err: errors.New(modelSecret)}, Prompt: "fail"}, service)
	if CodeOf(err) != CodeExecutionFailed || !errors.Is(err, ErrCleanupFailed) {
		t.Fatalf("error = %v, code = %q", err, CodeOf(err))
	}
	assertSafeReturnedError(t, err, CodeExecutionFailed, ErrExecutionFailed, modelSecret, cleanupSecret)
}

func TestRepeatedPublicRunsAreIndependent(t *testing.T) {
	const runs = 8
	var wait sync.WaitGroup
	errorsSeen := make(chan error, runs)
	for range runs {
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, err := Run(t.Context(), Request{Model: finalModel("done"), Prompt: "answer"})
			if err == nil && result.Text != "done" {
				err = fmt.Errorf("result = %#v", result)
			}
			errorsSeen <- err
		}()
	}
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
}

type scriptedModel struct {
	mu    sync.Mutex
	calls int
	step  func(int, *model.LLMRequest, bool) (*model.LLMResponse, error)
}

func (*scriptedModel) Name() string { return "scripted" }

func (m *scriptedModel) GenerateContent(_ context.Context, request *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		m.mu.Lock()
		call := m.calls
		m.calls++
		m.mu.Unlock()
		response, err := m.step(call, request, stream)
		yield(response, err)
	}
}

type errorModel struct{ err error }

func (errorModel) Name() string { return "error" }
func (m errorModel) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) { yield(nil, m.err) }
}

type cancellationModel struct{}

func (cancellationModel) Name() string { return "cancellation" }
func (cancellationModel) GenerateContent(ctx context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) { yield(nil, ctx.Err()) }
}

type wrappedCancellationModel struct{}

func (wrappedCancellationModel) Name() string { return "wrapped-cancellation" }
func (wrappedCancellationModel) GenerateContent(ctx context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(nil, fmt.Errorf("wrapped cancellation: %w", ctx.Err()))
	}
}

type panicCallerError struct{ method string }

func (e panicCallerError) Error() string {
	if e.method == "Error" {
		panic("CALLER_METHOD_SECRET")
	}
	return "caller failure"
}

func (e panicCallerError) Is(error) bool {
	if e.method == "Is" {
		panic("CALLER_METHOD_SECRET")
	}
	return false
}

type lazyPanicModel struct{}

func (lazyPanicModel) Name() string { return "lazy-panic" }
func (lazyPanicModel) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(func(*model.LLMResponse, error) bool) { panic("TOPSECRET") }
}

type panicNameModel struct{}

func (panicNameModel) Name() string { panic("TOPSECRET") }
func (panicNameModel) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(func(*model.LLMResponse, error) bool) {}
}

type eagerPanicModel struct{}

func (eagerPanicModel) Name() string { return "eager-panic" }
func (eagerPanicModel) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	panic("TOPSECRET")
}

type emptyModel struct{}

func (emptyModel) Name() string { return "empty" }
func (emptyModel) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(func(*model.LLMResponse, error) bool) {}
}

func finalModel(text string) model.LLM {
	return &scriptedModel{step: func(int, *model.LLMRequest, bool) (*model.LLMResponse, error) { return textResponse(text), nil }}
}

func toolThenFinalModel(name, text string) model.LLM {
	return &scriptedModel{step: func(call int, _ *model.LLMRequest, _ bool) (*model.LLMResponse, error) {
		if call == 0 {
			return functionCallResponse(name), nil
		}
		return textResponse(text), nil
	}}
}

func textResponse(text string) *model.LLMResponse {
	return &model.LLMResponse{Content: genai.NewContentFromText(text, genai.RoleModel)}
}

func functionCallResponse(name string) *model.LLMResponse {
	return &model.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
		ID: "call-1", Name: name, Args: map[string]any{},
	}}}}}
}

type testFunctionTool struct {
	name        string
	description string
	panicRun    bool
	run         func(agent.Context, any) (map[string]any, error)
}

type overwritingFunctionTool struct {
	testFunctionTool
	delegate toolutils.Tool
}

func (t *overwritingFunctionTool) ProcessRequest(_ agent.Context, request *model.LLMRequest) error {
	if err := toolutils.PackTool(request, t); err != nil {
		return err
	}
	request.Tools[t.Name()] = t.delegate
	return nil
}

type registeringRequestTool struct {
	name     string
	delegate toolutils.Tool
}

func (t *registeringRequestTool) Name() string      { return t.name }
func (*registeringRequestTool) Description() string { return "register delegate" }
func (*registeringRequestTool) IsLongRunning() bool { return false }
func (t *registeringRequestTool) ProcessRequest(_ agent.Context, request *model.LLMRequest) error {
	return toolutils.PackTool(request, t.delegate)
}

type testStreamingTool struct {
	name     string
	panicRun bool
	err      error
}

func (t *testStreamingTool) Name() string      { return t.name }
func (*testStreamingTool) Description() string { return "stream delegate" }
func (*testStreamingTool) IsLongRunning() bool { return false }
func (t *testStreamingTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{Name: t.name, Description: t.Description()}
}
func (t *testStreamingTool) RunStream(agent.Context, any) iter.Seq2[string, error] {
	return func(yield func(string, error) bool) {
		if t.panicRun {
			panic("TOPSECRET")
		}
		yield("done", t.err)
	}
}

type dynamicMetadataTool struct {
	name             string
	nameCalls        int
	descriptionCalls int
	longRunningCalls int
	declarationCalls int
	defersCalls      int
}

func (t *dynamicMetadataTool) Name() string {
	t.nameCalls++
	return fmt.Sprintf("%s-%d", t.name, t.nameCalls)
}
func (t *dynamicMetadataTool) Description() string {
	t.descriptionCalls++
	return fmt.Sprintf("description-%d", t.descriptionCalls)
}
func (t *dynamicMetadataTool) IsLongRunning() bool {
	t.longRunningCalls++
	return t.longRunningCalls%2 == 1
}
func (t *dynamicMetadataTool) Declaration() *genai.FunctionDeclaration {
	t.declarationCalls++
	return &genai.FunctionDeclaration{Description: fmt.Sprintf("declaration-%d", t.declarationCalls)}
}
func (t *dynamicMetadataTool) DefersResponse() bool {
	t.defersCalls++
	return t.defersCalls%2 == 1
}
func (*dynamicMetadataTool) Run(agent.Context, any) (map[string]any, error) {
	return map[string]any{"ok": true}, nil
}

type panicMetadataTool struct {
	testFunctionTool
	secret string
}

func (t *panicMetadataTool) Description() string { panic(t.secret) }

type errorRequestTool struct {
	err error
}

func (*errorRequestTool) Name() string        { return "error_processor" }
func (*errorRequestTool) Description() string { return "return an error" }
func (*errorRequestTool) IsLongRunning() bool { return false }
func (t *errorRequestTool) ProcessRequest(agent.Context, *model.LLMRequest) error {
	return t.err
}

type instructionAugmentingTool struct {
	testFunctionTool
	instruction string
}

type instructionMutatingTool struct {
	testFunctionTool
	seen   *string
	mutate func(string) string
}

func (t *instructionMutatingTool) ProcessRequest(_ agent.Context, request *model.LLMRequest) error {
	if err := toolutils.PackTool(request, t); err != nil {
		return err
	}
	current := systemInstruction(request)
	if t.seen != nil {
		*t.seen = current
	}
	request.Config.SystemInstruction = genai.NewContentFromText(t.mutate(current), genai.RoleUser)
	return nil
}

func (t *instructionAugmentingTool) ProcessRequest(_ agent.Context, request *model.LLMRequest) error {
	if err := toolutils.PackTool(request, t); err != nil {
		return err
	}
	if request.Config == nil {
		request.Config = &genai.GenerateContentConfig{}
	}
	if request.Config.SystemInstruction == nil {
		request.Config.SystemInstruction = genai.NewContentFromText(t.instruction, genai.RoleUser)
		return nil
	}
	parts := request.Config.SystemInstruction.Parts
	if len(parts) != 0 && parts[len(parts)-1] != nil && parts[len(parts)-1].Text != "" {
		parts[len(parts)-1].Text += "\n\n" + t.instruction
		return nil
	}
	request.Config.SystemInstruction.Parts = append(parts, genai.NewPartFromText(t.instruction))
	return nil
}

type panicNameTool struct{}

func (panicNameTool) Name() string        { panic("TOPSECRET") }
func (panicNameTool) Description() string { return "panic" }
func (panicNameTool) IsLongRunning() bool { return false }
func (panicNameTool) ProcessRequest(agent.Context, *model.LLMRequest) error {
	return nil
}

type panicProcessorTool struct{ testFunctionTool }

func (t *panicProcessorTool) Name() string        { return "processor_panic" }
func (t *panicProcessorTool) Description() string { return "panic while packing" }
func (t *panicProcessorTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{Name: t.Name(), Description: t.Description()}
}
func (*panicProcessorTool) ProcessRequest(agent.Context, *model.LLMRequest) error { panic("TOPSECRET") }

type panicStreamingTool struct{}

func (*panicStreamingTool) Name() string        { return "stream_panic" }
func (*panicStreamingTool) Description() string { return "panic while streaming" }
func (*panicStreamingTool) IsLongRunning() bool { return false }
func (t *panicStreamingTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{Name: t.Name(), Description: t.Description()}
}
func (t *panicStreamingTool) ProcessRequest(_ agent.Context, request *model.LLMRequest) error {
	return toolutils.PackTool(request, t)
}
func (*panicStreamingTool) RunStream(agent.Context, any) iter.Seq2[string, error] {
	return func(func(string, error) bool) { panic("TOPSECRET") }
}

func (t *testFunctionTool) Name() string        { return t.name }
func (t *testFunctionTool) Description() string { return t.description }
func (*testFunctionTool) IsLongRunning() bool   { return false }
func (t *testFunctionTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{Name: t.name, Description: t.description}
}
func (t *testFunctionTool) ProcessRequest(_ agent.Context, request *model.LLMRequest) error {
	return toolutils.PackTool(request, t)
}
func (t *testFunctionTool) Run(ctx agent.Context, arguments any) (map[string]any, error) {
	if t.panicRun {
		panic("TOPSECRET")
	}
	if t.run != nil {
		return t.run(ctx, arguments)
	}
	return map[string]any{"ok": true}, nil
}

type tracingSessionService struct {
	session.Service
	mu         sync.Mutex
	createdIDs []string
	deletedIDs []string
	deleteCtx  []error
	createErr  error
	deleteErr  error
}

func newTracingSessionService() *tracingSessionService {
	return &tracingSessionService{Service: session.InMemoryService()}
}

func (s *tracingSessionService) Create(ctx context.Context, request *session.CreateRequest) (*session.CreateResponse, error) {
	s.mu.Lock()
	configuredErr := s.createErr
	s.mu.Unlock()
	if configuredErr != nil {
		return nil, configuredErr
	}
	response, err := s.Service.Create(ctx, request)
	if err == nil {
		s.mu.Lock()
		s.createdIDs = append(s.createdIDs, response.Session.ID())
		s.mu.Unlock()
	}
	return response, err
}

func (s *tracingSessionService) Delete(ctx context.Context, request *session.DeleteRequest) error {
	s.mu.Lock()
	s.deletedIDs = append(s.deletedIDs, request.SessionID)
	s.deleteCtx = append(s.deleteCtx, ctx.Err())
	configuredErr := s.deleteErr
	s.mu.Unlock()
	if configuredErr != nil {
		return configuredErr
	}
	return s.Service.Delete(ctx, request)
}

func (s *tracingSessionService) assertLifecycle(t *testing.T, runs int) {
	t.Helper()
	s.mu.Lock()
	created := append([]string(nil), s.createdIDs...)
	deleted := append([]string(nil), s.deletedIDs...)
	deleteCtx := append([]error(nil), s.deleteCtx...)
	s.mu.Unlock()
	if len(created) != runs || len(deleted) != runs {
		t.Fatalf("created = %v, deleted = %v", created, deleted)
	}
	if !slices.Equal(created, deleted) {
		t.Fatalf("created = %v, deleted = %v", created, deleted)
	}
	for index, ctxErr := range deleteCtx {
		if ctxErr != nil {
			t.Fatalf("delete %d context error = %v", index, ctxErr)
		}
	}
	if runs > 1 && created[runs-1] == created[runs-2] {
		t.Fatalf("sessions were reused: %v", created)
	}
	if _, err := s.Service.Get(t.Context(), &session.GetRequest{AppName: appName, UserID: userID, SessionID: created[len(created)-1]}); err == nil {
		t.Fatalf("session %q remained resumable", created[len(created)-1])
	}
}

func systemInstruction(request *model.LLMRequest) string {
	if request == nil || request.Config == nil || request.Config.SystemInstruction == nil {
		return ""
	}
	return contentText(request.Config.SystemInstruction)
}

func latestUserText(request *model.LLMRequest) string {
	if request == nil {
		return ""
	}
	for index := len(request.Contents) - 1; index >= 0; index-- {
		if request.Contents[index] != nil && request.Contents[index].Role == genai.RoleUser {
			return contentText(request.Contents[index])
		}
	}
	return ""
}

func contentText(content *genai.Content) string {
	var result strings.Builder
	for _, part := range content.Parts {
		if part != nil {
			result.WriteString(part.Text)
		}
	}
	return result.String()
}

func requestToolNames(request *model.LLMRequest) []string {
	result := make([]string, 0, len(request.Tools))
	for name := range request.Tools {
		result = append(result, name)
	}
	slices.Sort(result)
	return result
}

func functionResponseError(request *model.LLMRequest, name string) string {
	if request == nil {
		return ""
	}
	for _, content := range request.Contents {
		if content == nil {
			continue
		}
		for _, part := range content.Parts {
			if part == nil || part.FunctionResponse == nil || part.FunctionResponse.Name != name {
				continue
			}
			value, _ := part.FunctionResponse.Response["error"].(string)
			return value
		}
	}
	return ""
}

func assertSafeReturnedError(
	t *testing.T,
	err error,
	wantCode ErrorCode,
	wantSentinel error,
	forbidden ...string,
) {
	t.Helper()
	if CodeOf(err) != wantCode || !errors.Is(err, wantSentinel) {
		t.Fatalf("error = %v, code = %q, want %q", err, CodeOf(err), wantCode)
	}
	var typed *Error
	if !errors.As(err, &typed) {
		t.Fatalf("error does not expose *Error through errors.As: %T", err)
	}
	if typed.Code != wantCode {
		t.Fatalf("typed error = %#v", typed)
	}
	visible := []string{fmt.Sprint(err), typed.Op, fmt.Sprint(typed.Err)}
	for _, secret := range forbidden {
		for _, text := range visible {
			if strings.Contains(text, secret) {
				t.Fatalf("returned error exposed %q in %q", secret, text)
			}
		}
	}
}

var (
	_ model.LLM = (*scriptedModel)(nil)
	_ model.LLM = errorModel{}
	_ model.LLM = cancellationModel{}
	_ model.LLM = lazyPanicModel{}
	_ model.LLM = panicNameModel{}
	_ model.LLM = eagerPanicModel{}
	_ model.LLM = emptyModel{}
)
