package plasmid

import (
	"context"
	"errors"
	"iter"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/adk/v2/tool/toolutils"
	"google.golang.org/genai"
)

func TestHarnessContextScopeReleasesOnEveryRunExit(t *testing.T) {
	for _, test := range []struct {
		name   string
		mode   string
		early  bool
		cancel bool
	}{
		{name: "normal", mode: "normal"},
		{name: "model error", mode: "error"},
		{name: "cancellation", mode: "block", cancel: true},
		{name: "early iterator stop", mode: "normal", early: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			testHarnessContextScopeExit(t, test.mode, test.early, test.cancel)
		})
	}
}

func testHarnessContextScopeExit(t *testing.T, mode string, early, cancelRun bool) {
	t.Helper()
	model := &scopeExitModel{mode: mode, started: make(chan struct{})}
	harness, err := New(t.Context(), WithModel(model), WithWorkingDir(t.TempDir()), WithSessionDir(filepath.Join(t.TempDir(), "sessions")))
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestResource(t, harness)
	sessionID, err := harness.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if cancelRun {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()
		done := make(chan struct{})
		go func() {
			<-model.started
			cancel()
			close(done)
		}()
		defer func() { <-done }()
	}
	for range harness.Run(ctx, sessionID, "run") {
		if early {
			break
		}
	}
	if got := harness.contexts.ActiveScopes(); got != 0 {
		t.Fatalf("active context scopes = %d", got)
	}
}

func TestScopedToolsetPreservesSourceRequestProcessor(t *testing.T) {
	for _, confirmation := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "confirmation"}[confirmation], func(t *testing.T) {
			source := &processingToolset{}
			compiled := &lifecyclePlugin{name: "processor", init: func(h *Harness) error {
				return h.RegisterToolsets(source)
			}}
			harness, err := New(t.Context(),
				WithModel(lifecycleModel{}), WithWorkingDir(t.TempDir()),
				WithSessionDir(filepath.Join(t.TempDir(), "sessions")),
				WithPlugins(compiled), WithToolConfirmation(confirmation),
			)
			if err != nil {
				t.Fatal(err)
			}
			defer closeTestResource(t, harness)
			sessionID, err := harness.NewSession(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := harness.Ask(t.Context(), sessionID, "run"); err != nil {
				t.Fatal(err)
			}
			if source.processed != 1 {
				t.Fatalf("source ProcessRequest calls = %d, want 1", source.processed)
			}
		})
	}
}

func TestDynamicToolProcessingPreservesDelegateAndConfirmation(t *testing.T) {
	tests := []struct {
		name    string
		options func(tool.Tool) []Option
	}{
		{
			name: "tool request processor delegate",
			options: func(delegate tool.Tool) []Option {
				return []Option{WithTools(delegate)}
			},
		},
		{
			name: "toolset request processor injection",
			options: func(delegate tool.Tool) []Option {
				compiled := &lifecyclePlugin{name: "injector", init: func(h *Harness) error {
					return h.RegisterToolsets(&injectingToolset{value: delegate.(nativeFunctionTool)})
				}}
				return []Option{WithPlugins(compiled)}
			},
		},
	}
	for _, test := range tests {
		for _, confirmation := range []bool{false, true} {
			name := map[bool]string{false: "plain", true: "confirmation"}[confirmation]
			t.Run(test.name+"/"+name, func(t *testing.T) {
				testDynamicToolProcessing(t, test.name, confirmation, test.options)
			})
		}
	}
}

func testDynamicToolProcessing(t *testing.T, variant string, confirmation bool, optionsFor func(tool.Tool) []Option) {
	t.Helper()
	var sourceExecutions atomic.Int32
	var delegateExecutions atomic.Int32
	source, err := functiontool.New[map[string]any, map[string]any](functiontool.Config{
		Name: "dynamic", Description: "source",
		InputSchema: &jsonschema.Schema{Type: "object"}, OutputSchema: &jsonschema.Schema{Type: "object"},
	}, func(agent.Context, map[string]any) (map[string]any, error) {
		sourceExecutions.Add(1)
		return map[string]any{"source": true}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	delegate, err := functiontool.New[map[string]any, map[string]any](functiontool.Config{
		Name: "dynamic", Description: "delegate",
		InputSchema: &jsonschema.Schema{Type: "object"}, OutputSchema: &jsonschema.Schema{Type: "object"},
	}, func(agent.Context, map[string]any) (map[string]any, error) {
		delegateExecutions.Add(1)
		return map[string]any{"delegate": true}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	registered := delegate
	if variant == "tool request processor delegate" {
		registered = &delegatingFunctionTool{nativeFunctionTool: source.(nativeFunctionTool), delegate: delegate.(nativeFunctionTool)}
	}
	model := &dynamicToolModel{name: "dynamic"}
	options := []Option{
		WithModel(model), WithWorkingDir(t.TempDir()),
		WithSessionDir(filepath.Join(t.TempDir(), "sessions")), WithToolConfirmation(confirmation),
	}
	harness, err := New(t.Context(), append(options, optionsFor(registered)...)...)
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestResource(t, harness)
	sessionID, err := harness.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	requestedConfirmation := false
	for event, runErr := range harness.Run(t.Context(), sessionID, "run") {
		if runErr != nil {
			t.Fatal(runErr)
		}
		requestedConfirmation = requestedConfirmation || len(event.Actions.RequestedToolConfirmations) != 0
	}
	assertDynamicToolProcessing(t, model.packed, sourceExecutions.Load(), delegateExecutions.Load(), requestedConfirmation, confirmation)
}

func assertDynamicToolProcessing(t *testing.T, packed bool, sourceExecutions, delegateExecutions int32, requestedConfirmation, confirmation bool) {
	t.Helper()
	if !packed {
		t.Fatal("dynamic tool declaration was not packed")
	}
	if sourceExecutions != 0 {
		t.Fatalf("source executions = %d", sourceExecutions)
	}
	wantDelegate := int32(1)
	if confirmation {
		wantDelegate = 0
	}
	if delegateExecutions != wantDelegate {
		t.Fatalf("delegate executions = %d, want %d", delegateExecutions, wantDelegate)
	}
	if requestedConfirmation != confirmation {
		t.Fatalf("requested confirmation = %t, want %t", requestedConfirmation, confirmation)
	}
}

func TestHarnessCloseTimeoutDoesNotRaceContextRelease(t *testing.T) {
	model := &closeResistantModel{started: make(chan struct{}), release: make(chan struct{})}
	harness, err := New(t.Context(), WithModel(model), WithWorkingDir(t.TempDir()), WithSessionDir(filepath.Join(t.TempDir(), "sessions")))
	if err != nil {
		t.Fatal(err)
	}
	harness.closeWaitTimeout = 20 * time.Millisecond
	sessionID, err := harness.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		for event, runErr := range harness.Run(context.Background(), sessionID, "run") {
			_, _ = event, runErr
		}
	}()
	select {
	case <-model.started:
	case <-time.After(time.Second):
		t.Fatal("model did not start")
	}
	closeErr := harness.Close()
	if !errors.Is(closeErr, ErrCloseTimeout) {
		t.Fatalf("Close error = %v", closeErr)
	}
	close(model.release)
	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatal("run did not exit after model release")
	}
	if got := harness.contexts.ActiveScopes(); got != 0 {
		t.Fatalf("active context scopes = %d", got)
	}
}

func TestCanceledLateModelToolCallFailsClosed(t *testing.T) {
	var executions atomic.Int32
	hostTool, err := functiontool.New[map[string]any, map[string]any](functiontool.Config{
		Name: "late_host", Description: "records execution",
		InputSchema: &jsonschema.Schema{Type: "object"}, OutputSchema: &jsonschema.Schema{Type: "object"},
	}, func(agent.Context, map[string]any) (map[string]any, error) {
		executions.Add(1)
		return map[string]any{"executed": true}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	model := &lateToolCallModel{started: make(chan struct{}), release: make(chan struct{})}
	harness, err := New(t.Context(), WithModel(model), WithTools(hostTool), WithWorkingDir(t.TempDir()), WithSessionDir(filepath.Join(t.TempDir(), "sessions")))
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestResource(t, harness)
	sessionID, err := harness.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	runContext, cancel := context.WithCancel(t.Context())
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		for event, runErr := range harness.Run(runContext, sessionID, "run") {
			_, _ = event, runErr
		}
	}()
	select {
	case <-model.started:
	case <-time.After(time.Second):
		t.Fatal("model did not start")
	}
	cancel()
	close(model.release)
	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatal("canceled run did not exit")
	}
	if executions.Load() != 0 {
		t.Fatalf("host tool executions after cancellation = %d", executions.Load())
	}
}

type scopeExitModel struct {
	mode    string
	started chan struct{}
}

type closeResistantModel struct {
	started chan struct{}
	release chan struct{}
}

type lateToolCallModel struct {
	started chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

type processingToolset struct{ processed int }

type delegatingFunctionTool struct {
	nativeFunctionTool
	delegate nativeFunctionTool
}

func (t *delegatingFunctionTool) ProcessRequest(_ agent.Context, request *model.LLMRequest) error {
	return toolutils.PackTool(request, t.delegate)
}

type injectingToolset struct{ value nativeFunctionTool }

func (*injectingToolset) Name() string                                     { return "injecting" }
func (*injectingToolset) Tools(agent.ReadonlyContext) ([]tool.Tool, error) { return nil, nil }
func (s *injectingToolset) ProcessRequest(_ agent.Context, request *model.LLMRequest) error {
	return toolutils.PackTool(request, s.value)
}

type dynamicToolModel struct {
	name   string
	calls  int
	packed bool
}

func (*processingToolset) Name() string                                     { return "processing" }
func (*processingToolset) Tools(agent.ReadonlyContext) ([]tool.Tool, error) { return nil, nil }
func (s *processingToolset) ProcessRequest(agent.Context, *model.LLMRequest) error {
	s.processed++
	return nil
}

func (*scopeExitModel) Name() string { return "scope-exit" }

func (*closeResistantModel) Name() string { return "close-resistant" }

func (*lateToolCallModel) Name() string { return "late-tool-call" }

func (*dynamicToolModel) Name() string { return "dynamic-tool" }

func (m *dynamicToolModel) GenerateContent(_ context.Context, request *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if m.calls == 0 {
			if request.Tools[m.name] == nil {
				yield(nil, errors.New("dynamic tool missing from request map"))
				return
			}
			for _, group := range request.Config.Tools {
				for _, declaration := range group.FunctionDeclarations {
					m.packed = m.packed || declaration.Name == m.name
				}
			}
			m.calls++
			yield(&model.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
				ID: "dynamic-1", Name: m.name, Args: map[string]any{},
			}}}}}, nil)
			return
		}
		m.calls++
		yield(&model.LLMResponse{Content: genai.NewContentFromText("done", genai.RoleModel)}, nil)
	}
}

func (m *lateToolCallModel) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if m.calls.Add(1) == 1 {
			close(m.started)
			<-m.release
			yield(&model.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
				ID: "late-1", Name: "late_host", Args: map[string]any{},
			}}}}}, nil)
			return
		}
		yield(&model.LLMResponse{Content: genai.NewContentFromText("done", genai.RoleModel)}, nil)
	}
}

func (m *closeResistantModel) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		close(m.started)
		<-m.release
		yield(&model.LLMResponse{Content: genai.NewContentFromText("ok", genai.RoleModel)}, nil)
	}
}

func (m *scopeExitModel) GenerateContent(ctx context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		switch m.mode {
		case "error":
			yield(nil, errors.New("model failed"))
		case "block":
			close(m.started)
			select {
			case <-ctx.Done():
				yield(nil, ctx.Err())
			case <-time.After(2 * time.Second):
				yield(nil, errors.New("cancellation did not arrive"))
			}
		default:
			yield(&model.LLMResponse{Content: genai.NewContentFromText("ok", genai.RoleModel)}, nil)
		}
	}
}
