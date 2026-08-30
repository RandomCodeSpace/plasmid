package oneshot

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RandomCodeSpace/plasmid/internal/toolcallrecovery"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

func TestProbeToolCallingMakesOneDirectInertRequest(t *testing.T) {
	var calls atomic.Int64
	modelValue := probeModelFunc(func(_ context.Context, request *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
		calls.Add(1)
		if stream {
			t.Error("probe requested streaming")
		}
		assertToolCallingProbeRequest(t, request, probeMaxOutputTokens)
		return probeSequence(probeYield{response: probeSuccessResponse()})
	})

	result, err := ProbeToolCalling(t.Context(), modelValue)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("GenerateContent calls = %d, want 1", calls.Load())
	}
	if result.Text != "" || len(result.ToolResults) != 0 || result.Metadata.ModelCalls != 1 || result.Metadata.ToolCalls != 0 {
		t.Fatalf("result = %#v", result)
	}
}

func TestProbeAppliesConfiguredOutputTokenBudget(t *testing.T) {
	const maxOutputTokens = int32(256)
	var calls atomic.Int64
	modelValue := probeModelFunc(func(_ context.Context, request *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
		calls.Add(1)
		if stream {
			t.Error("probe requested streaming")
		}
		assertToolCallingProbeRequest(t, request, maxOutputTokens)
		return probeSequence(probeYield{response: probeSuccessResponse()})
	})

	result, err := Probe(t.Context(), ProbeRequest{Model: modelValue, MaxOutputTokens: maxOutputTokens})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || result.Metadata.ModelCalls != 1 || result.Metadata.ToolCalls != 0 {
		t.Fatalf("calls = %d, result = %#v", calls.Load(), result)
	}
}

func TestProbeRejectsInvalidOutputTokenBudgetBeforeCallingModel(t *testing.T) {
	for _, maxOutputTokens := range []int32{0, -1} {
		t.Run(fmt.Sprintf("max output tokens %d", maxOutputTokens), func(t *testing.T) {
			modelValue := &countingProbeModel{}
			result, err := Probe(t.Context(), ProbeRequest{Model: modelValue, MaxOutputTokens: maxOutputTokens})
			assertSafeReturnedError(t, err, CodeInvalidArgument, ErrInvalidArgument)
			if modelValue.calls.Load() != 0 || result.Text != "" || len(result.ToolResults) != 0 || result.Metadata != (Metadata{}) {
				t.Fatalf("calls = %d, result = %#v", modelValue.calls.Load(), result)
			}
		})
	}
}

func TestProbeToolCallingClassifiesResponses(t *testing.T) {
	longText := strings.Repeat("s", probeMaxReturnedTextBytes+1)
	tests := []struct {
		name     string
		yields   []probeYield
		wantCode ErrorCode
		wantErr  error
		wantText string
	}{
		{name: "exact call", yields: []probeYield{{response: probeSuccessResponse()}}},
		{name: "thought then exact call", yields: []probeYield{{response: probeResponse(
			&genai.Part{Text: "hidden", Thought: true}, probeSuccessPart(),
		)}}},
		{name: "signed exact call", yields: []probeYield{{response: probeResponse(func() *genai.Part {
			part := probeSuccessPart()
			part.ThoughtSignature = []byte("opaque")
			return part
		}())}}},
		{name: "no response", wantCode: CodeNoFinalResponse, wantErr: ErrNoFinalResponse},
		{name: "nil response", yields: []probeYield{{}}, wantCode: CodeExecutionFailed, wantErr: ErrExecutionFailed},
		{name: "nil content", yields: []probeYield{{response: &model.LLMResponse{FinishReason: genai.FinishReasonStop}}}, wantCode: CodeNoFinalResponse, wantErr: ErrNoFinalResponse},
		{name: "empty parts", yields: []probeYield{{response: probeResponse()}}, wantCode: CodeNoFinalResponse, wantErr: ErrNoFinalResponse},
		{name: "thought only", yields: []probeYield{{response: probeResponse(&genai.Part{Text: "hidden", Thought: true})}}, wantCode: CodeNoFinalResponse, wantErr: ErrNoFinalResponse},
		{name: "text answer", yields: []probeYield{{response: probeResponse(&genai.Part{Text: "pong"})}}, wantCode: CodeToolCallingUnsupported, wantErr: ErrToolCallingUnsupported, wantText: "pong"},
		{name: "unrelated call", yields: []probeYield{{response: probeResponse(probeCallPart("other", probeMarkerValue))}}, wantCode: CodeToolCallingUnsupported, wantErr: ErrToolCallingUnsupported},
		{name: "missing arguments", yields: []probeYield{{response: probeResponse(&genai.Part{FunctionCall: &genai.FunctionCall{Name: probeToolName}})}}, wantCode: CodeToolCallingUnsupported, wantErr: ErrToolCallingUnsupported},
		{name: "malformed arguments", yields: []probeYield{{response: func() *model.LLMResponse {
			response := probeResponse(&genai.Part{FunctionCall: &genai.FunctionCall{ID: "call-1", Name: probeToolName}})
			response.CustomMetadata = map[string]any{toolcallrecovery.MetadataKey: toolcallrecovery.Failures{"call-1": toolcallrecovery.InvalidArgumentsMessage}}
			return response
		}()}}, wantCode: CodeExecutionFailed, wantErr: ErrExecutionFailed},
		{name: "wrong marker", yields: []probeYield{{response: probeResponse(probeCallPart(probeToolName, "wrong"))}}, wantCode: CodeToolCallingUnsupported, wantErr: ErrToolCallingUnsupported},
		{name: "extra argument", yields: []probeYield{{response: probeResponse(&genai.Part{FunctionCall: &genai.FunctionCall{
			Name: probeToolName, Args: map[string]any{probeMarkerName: probeMarkerValue, "extra": true},
		}})}}, wantCode: CodeToolCallingUnsupported, wantErr: ErrToolCallingUnsupported},
		{name: "text and call", yields: []probeYield{{response: probeResponse(&genai.Part{Text: "pong"}, probeSuccessPart())}}, wantCode: CodeToolCallingUnsupported, wantErr: ErrToolCallingUnsupported, wantText: "pong"},
		{name: "custom call", yields: []probeYield{{response: probeResponse(&genai.Part{ToolCall: &genai.ToolCall{}})}}, wantCode: CodeToolCallingUnsupported, wantErr: ErrToolCallingUnsupported},
		{name: "partial function call", yields: []probeYield{{response: probeResponse(&genai.Part{FunctionCall: &genai.FunctionCall{
			Name: probeToolName, Args: map[string]any{probeMarkerName: probeMarkerValue}, PartialArgs: []*genai.PartialArg{{}},
		}})}}, wantCode: CodeToolCallingUnsupported, wantErr: ErrToolCallingUnsupported},
		{name: "wrong role", yields: []probeYield{{response: func() *model.LLMResponse {
			response := probeSuccessResponse()
			response.Content.Role = genai.RoleUser
			return response
		}()}}, wantCode: CodeToolCallingUnsupported, wantErr: ErrToolCallingUnsupported},
		{name: "missing role", yields: []probeYield{{response: func() *model.LLMResponse {
			response := probeSuccessResponse()
			response.Content.Role = ""
			return response
		}()}}, wantCode: CodeToolCallingUnsupported, wantErr: ErrToolCallingUnsupported},
		{name: "multiple calls", yields: []probeYield{{response: probeResponse(probeSuccessPart(), probeSuccessPart())}}, wantCode: CodeToolCallLimit, wantErr: ErrToolCallLimit},
		{name: "multiple responses", yields: []probeYield{{response: probeSuccessResponse()}, {response: probeSuccessResponse()}}, wantCode: CodeToolCallingUnsupported, wantErr: ErrToolCallingUnsupported},
		{name: "output truncated", yields: []probeYield{{response: func() *model.LLMResponse {
			response := probeSuccessResponse()
			response.FinishReason = genai.FinishReasonMaxTokens
			return response
		}()}}, wantCode: CodeOutputTruncated, wantErr: ErrOutputTruncated},
		{name: "text cap", yields: []probeYield{{response: probeResponse(&genai.Part{Text: longText})}}, wantCode: CodeTextTruncated, wantErr: ErrTextTruncated, wantText: longText[:probeMaxReturnedTextBytes]},
		{name: "safety finish", yields: []probeYield{{response: &model.LLMResponse{Content: probeSuccessResponse().Content, FinishReason: genai.FinishReasonSafety}}}, wantCode: CodeExecutionFailed, wantErr: ErrExecutionFailed},
		{name: "interrupted", yields: []probeYield{{response: &model.LLMResponse{Content: probeSuccessResponse().Content, FinishReason: genai.FinishReasonStop, Interrupted: true}}}, wantCode: CodeExecutionFailed, wantErr: ErrExecutionFailed},
		{name: "partial response", yields: []probeYield{{response: &model.LLMResponse{Content: probeSuccessResponse().Content, FinishReason: genai.FinishReasonStop, Partial: true}}}, wantCode: CodeExecutionFailed, wantErr: ErrExecutionFailed},
		{name: "response error", yields: []probeYield{{response: &model.LLMResponse{ErrorCode: "secret-code", ErrorMessage: "secret-message"}}}, wantCode: CodeExecutionFailed, wantErr: ErrExecutionFailed},
		{name: "upstream error", yields: []probeYield{{err: errors.New("upstream secret")}}, wantCode: CodeExecutionFailed, wantErr: ErrExecutionFailed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := ProbeToolCalling(t.Context(), probeSequenceModel{yields: test.yields})
			if test.wantCode == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				assertSafeReturnedError(t, err, test.wantCode, test.wantErr, "secret")
			}
			if result.Text != test.wantText || len(result.ToolResults) != 0 || result.Metadata.ModelCalls != 1 || result.Metadata.ToolCalls != 0 {
				t.Fatalf("result = %#v", result)
			}
		})
	}
}

func TestProbeToolCallingSharesValidationAndModelBoundary(t *testing.T) {
	var typedNil *probeSequenceModel
	for _, test := range []struct {
		name     string
		ctx      context.Context
		model    model.LLM
		wantCode ErrorCode
		wantErr  error
	}{
		{name: "nil context", model: probeSequenceModel{}, wantCode: CodeInvalidArgument, wantErr: ErrInvalidArgument},
		{name: "nil model", ctx: t.Context(), wantCode: CodeInvalidArgument, wantErr: ErrInvalidArgument},
		{name: "typed nil model", ctx: t.Context(), model: typedNil, wantCode: CodeInvalidArgument, wantErr: ErrInvalidArgument},
		{name: "panicking name", ctx: t.Context(), model: panicNameModel{}, wantCode: CodeModelPanic, wantErr: ErrModelPanic},
		{name: "panicking request", ctx: t.Context(), model: eagerPanicModel{}, wantCode: CodeModelPanic, wantErr: ErrModelPanic},
		{name: "panicking iterator", ctx: t.Context(), model: lazyPanicModel{}, wantCode: CodeModelPanic, wantErr: ErrModelPanic},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := ProbeToolCalling(test.ctx, test.model)
			assertSafeReturnedError(t, err, test.wantCode, test.wantErr, "TOPSECRET", "stack:")
			if result.Metadata.ToolCalls != 0 || len(result.ToolResults) != 0 {
				t.Fatalf("result = %#v", result)
			}
		})
	}
}

func TestProbeToolCallingClassifiesCancellationAndTimeout(t *testing.T) {
	for _, test := range []struct {
		name string
		ctx  func() context.Context
		want error
	}{
		{name: "canceled before request", ctx: canceledProbeContext, want: context.Canceled},
		{name: "timeout before request", ctx: expiredProbeContext, want: context.DeadlineExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			modelValue := &countingProbeModel{}
			result, err := ProbeToolCalling(test.ctx(), modelValue)
			assertSafeReturnedError(t, err, CodeCanceled, ErrCanceled)
			if !errors.Is(err, test.want) || modelValue.calls.Load() != 0 || result.Metadata.ModelCalls != 0 {
				t.Fatalf("calls = %d, result = %#v, err = %v", modelValue.calls.Load(), result, err)
			}
		})
	}

	t.Run("canceled during request", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		modelValue := probeModelFunc(func(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
			return func(yield func(*model.LLMResponse, error) bool) {
				cancel()
				yield(nil, context.Canceled)
			}
		})
		result, err := ProbeToolCalling(ctx, modelValue)
		assertSafeReturnedError(t, err, CodeCanceled, ErrCanceled)
		if !errors.Is(err, context.Canceled) || result.Metadata.ModelCalls != 1 {
			t.Fatalf("result = %#v, err = %v", result, err)
		}
	})
}

func TestProbeToolCallingReportsUsageWithoutExecutingTool(t *testing.T) {
	response := probeSuccessResponse()
	response.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{
		PromptTokenCount: 3, CandidatesTokenCount: 2, TotalTokenCount: 5,
	}
	result, err := ProbeToolCalling(t.Context(), probeSequenceModel{yields: []probeYield{{response: response}}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Metadata != (Metadata{ModelCalls: 1, Usage: Usage{InputTokens: 3, OutputTokens: 2, TotalTokens: 5}}) {
		t.Fatalf("metadata = %#v", result.Metadata)
	}
}

func TestProbeToolCallingStopsAfterSecondResponse(t *testing.T) {
	var yielded atomic.Int64
	modelValue := probeModelFunc(func(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
		return func(yield func(*model.LLMResponse, error) bool) {
			for range 3 {
				yielded.Add(1)
				if !yield(probeSuccessResponse(), nil) {
					return
				}
			}
		}
	})
	result, err := ProbeToolCalling(t.Context(), modelValue)
	assertSafeReturnedError(t, err, CodeToolCallingUnsupported, ErrToolCallingUnsupported)
	if yielded.Load() != 2 || result.Metadata.ModelCalls != 1 || result.Metadata.ToolCalls != 0 {
		t.Fatalf("yielded = %d, result = %#v", yielded.Load(), result)
	}
}

func assertToolCallingProbeRequest(t *testing.T, request *model.LLMRequest, maxOutputTokens int32) {
	t.Helper()
	if request == nil || request.Config == nil || len(request.Contents) != 1 {
		t.Fatalf("request = %#v", request)
	}
	if request.Tools != nil {
		t.Fatalf("executable tools = %#v, want nil", request.Tools)
	}
	if request.Config.MaxOutputTokens != maxOutputTokens || len(request.Config.Tools) != 1 ||
		len(request.Config.Tools[0].FunctionDeclarations) != 1 {
		t.Fatalf("config = %#v", request.Config)
	}
	declaration := request.Config.Tools[0].FunctionDeclarations[0]
	if declaration.Name != probeToolName {
		t.Fatalf("tool name = %q", declaration.Name)
	}
	schema, _ := declaration.ParametersJsonSchema.(map[string]any)
	properties, _ := schema["properties"].(map[string]any)
	marker, _ := properties[probeMarkerName].(map[string]any)
	required, _ := schema["required"].([]string)
	values, _ := marker["enum"].([]string)
	if schema["type"] != "object" || schema["additionalProperties"] != false || len(required) != 1 ||
		required[0] != probeMarkerName || len(values) != 1 || values[0] != probeMarkerValue {
		t.Fatalf("ping schema = %#v", schema)
	}
	calling := request.Config.ToolConfig.FunctionCallingConfig
	if calling.Mode != genai.FunctionCallingConfigModeAny || len(calling.AllowedFunctionNames) != 1 ||
		calling.AllowedFunctionNames[0] != probeToolName {
		t.Fatalf("function calling config = %#v", calling)
	}
}

func probeSuccessResponse() *model.LLMResponse { return probeResponse(probeSuccessPart()) }

func probeSuccessPart() *genai.Part { return probeCallPart(probeToolName, probeMarkerValue) }

func probeCallPart(name, marker string) *genai.Part {
	return &genai.Part{FunctionCall: &genai.FunctionCall{Name: name, Args: map[string]any{probeMarkerName: marker}}}
}

func probeResponse(parts ...*genai.Part) *model.LLMResponse {
	return &model.LLMResponse{
		Content: &genai.Content{Role: genai.RoleModel, Parts: parts}, FinishReason: genai.FinishReasonStop,
	}
}

type probeYield struct {
	response *model.LLMResponse
	err      error
}

type probeSequenceModel struct{ yields []probeYield }

func (probeSequenceModel) Name() string { return "probe-sequence" }

func (m probeSequenceModel) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return probeSequence(m.yields...)
}

func probeSequence(yields ...probeYield) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		for _, value := range yields {
			if !yield(value.response, value.err) {
				return
			}
		}
	}
}

type probeModelFunc func(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error]

func (probeModelFunc) Name() string { return "probe-func" }

func (fn probeModelFunc) GenerateContent(ctx context.Context, request *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return fn(ctx, request, stream)
}

type countingProbeModel struct{ calls atomic.Int64 }

func (*countingProbeModel) Name() string { return "counting-probe" }

func (m *countingProbeModel) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	m.calls.Add(1)
	return probeSequence()
}

func canceledProbeContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func expiredProbeContext() context.Context {
	ctx, cancel := context.WithDeadline(context.Background(), time.Unix(1, 0))
	cancel()
	return ctx
}
