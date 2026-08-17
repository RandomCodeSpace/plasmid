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
			model := &scopeExitModel{mode: test.mode, started: make(chan struct{})}
			harness, err := New(t.Context(), WithModel(model), WithWorkingDir(t.TempDir()), WithSessionDir(filepath.Join(t.TempDir(), "sessions")))
			if err != nil {
				t.Fatal(err)
			}
			defer harness.Close()
			sessionID, err := harness.NewSession(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			ctx := t.Context()
			if test.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				done := make(chan struct{})
				go func() {
					<-model.started
					cancel()
					close(done)
				}()
				defer func() { <-done }()
			}
			for range harness.Run(ctx, sessionID, "run") {
				if test.early {
					break
				}
			}
			if got := harness.contexts.ActiveScopes(); got != 0 {
				t.Fatalf("active context scopes = %d", got)
			}
		})
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
			defer harness.Close()
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
		for range harness.Run(context.Background(), sessionID, "run") {
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
	defer harness.Close()
	sessionID, err := harness.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	runContext, cancel := context.WithCancel(t.Context())
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		for range harness.Run(runContext, sessionID, "run") {
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

func (*processingToolset) Name() string                                     { return "processing" }
func (*processingToolset) Tools(agent.ReadonlyContext) ([]tool.Tool, error) { return nil, nil }
func (s *processingToolset) ProcessRequest(agent.Context, *model.LLMRequest) error {
	s.processed++
	return nil
}

func (*scopeExitModel) Name() string { return "scope-exit" }

func (*closeResistantModel) Name() string { return "close-resistant" }

func (*lateToolCallModel) Name() string { return "late-tool-call" }

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
