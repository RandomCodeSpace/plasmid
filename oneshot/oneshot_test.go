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
	"google.golang.org/adk/v2/tool/functiontool"
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

func TestRecoveredADKFunctionToolPanicsUseQuotedNames(t *testing.T) {
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
			toolValue := newNativeFunctionTool(t, test.toolName, func(agent.Context, map[string]any) (map[string]any, error) {
				panic("TOPSECRET")
			})
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
	wantToolError := fmt.Sprintf("panic in tool %q: ordinary failure", toolName)
	toolValue := newNativeFunctionTool(t, toolName, func(agent.Context, map[string]any) (map[string]any, error) {
		return nil, errors.New(wantToolError)
	})
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
	if receivedToolError != wantToolError {
		t.Fatalf("tool error = %q, want %q", receivedToolError, wantToolError)
	}
	if result.Text != "handled" || result.Metadata.ToolCalls != 1 {
		t.Fatalf("result = %#v", result)
	}
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
	service := newTracingSessionService()
	service.deleteErr = errors.New("delete failed")
	_, err := runWithSessionService(t.Context(), Request{Model: errorModel{err: errors.New("model failed")}, Prompt: "fail"}, service)
	if CodeOf(err) != CodeExecutionFailed || !errors.Is(err, ErrCleanupFailed) {
		t.Fatalf("error = %v, code = %q", err, CodeOf(err))
	}
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

type instructionAugmentingTool struct {
	testFunctionTool
	instruction string
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
	deleteErr  error
}

func newTracingSessionService() *tracingSessionService {
	return &tracingSessionService{Service: session.InMemoryService()}
}

func (s *tracingSessionService) Create(ctx context.Context, request *session.CreateRequest) (*session.CreateResponse, error) {
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

func newNativeFunctionTool(
	t *testing.T,
	name string,
	handler func(agent.Context, map[string]any) (map[string]any, error),
) tool.Tool {
	t.Helper()
	value, err := functiontool.New(functiontool.Config{Name: name, Description: "test boundary"}, handler)
	if err != nil {
		t.Fatal(err)
	}
	return value
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
