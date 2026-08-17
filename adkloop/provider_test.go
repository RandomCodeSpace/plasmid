package adkloop

import (
	"context"
	"errors"
	"iter"
	"log/slog"
	"maps"
	"reflect"
	"sync"
	"testing"

	"google.golang.org/genai"

	"google.golang.org/adk/v2/model"

	"github.com/plasmid-dev/plasmid/loop"
)

func TestProviderAcceptsAndAppliesLoggers(t *testing.T) {
	constructorLogger := slog.New(slog.DiscardHandler)
	configureLogger := slog.New(slog.DiscardHandler)
	providerInterface, err := New(Config{Model: &singleResponseModel{text: "done"}, Logger: constructorLogger})
	if err != nil {
		t.Fatal(err)
	}
	provider := providerInterface.(*provider)
	if provider.logger != constructorLogger {
		t.Fatalf("constructor logger = %p, want %p", provider.logger, constructorLogger)
	}
	if err := provider.Configure(t.Context(), loop.RunnerConfig{
		AppName: "app", AgentName: "agent", Sessions: newRecordingSessionStore(), Logger: configureLogger,
	}); err != nil {
		t.Fatal(err)
	}
	if provider.logger != configureLogger {
		t.Fatalf("configured logger = %p, want %p", provider.logger, configureLogger)
	}
}

func TestProviderRunsRealADKToolTurn(t *testing.T) {
	model := &scriptedModel{}
	store := newRecordingSessionStore()
	if _, err := store.Create(t.Context(), loop.CreateSessionRequest{AppName: "app", UserID: "user-1", SessionID: "session-1"}); err != nil {
		t.Fatal(err)
	}

	toolCalls := 0
	lookup := &testTool{
		name:        "lookup",
		description: "look up a value",
		schema:      []byte(`{"type":"object","properties":{"key":{"type":"string"}},"required":["key"],"additionalProperties":false}`),
		call: func(_ context.Context, call loop.ToolCall) (loop.ToolResult, error) {
			toolCalls++
			if call.ID != "call-1" || call.Name != "lookup" || call.SessionID != "session-1" || call.InvocationID == "" {
				t.Fatalf("tool call identity = %#v", call)
			}
			if !reflect.DeepEqual(call.Args, map[string]any{"key": "alpha"}) {
				t.Fatalf("tool call args = %#v", call.Args)
			}
			return loop.ToolResult{CallID: "call-1", Content: map[string]any{"value": "beta"}}, nil
		},
	}

	providerInterface, err := New(Config{Model: model})
	if err != nil {
		t.Fatal(err)
	}
	if err := providerInterface.Configure(t.Context(), loop.RunnerConfig{
		AppName: "app", AgentName: "agent", Sessions: store, Tools: []loop.Tool{lookup},
	}); err != nil {
		t.Fatal(err)
	}

	events, runErrs := collectProviderRun(providerInterface.Run(t.Context(), loop.RunRequest{
		UserID: "user-1", SessionID: "session-1", Input: loop.Message{Role: loop.RoleUser, Text: "find alpha"},
	}))
	if len(runErrs) != 0 {
		t.Fatalf("run errors = %v", runErrs)
	}
	if toolCalls != 1 || model.calls != 2 || len(model.requests) != 2 {
		t.Fatalf("tool calls = %d, model calls = %d, requests = %d", toolCalls, model.calls, len(model.requests))
	}

	first := model.requests[0]
	if len(first.Contents) != 1 || first.Contents[0].Role != genai.RoleUser || len(first.Contents[0].Parts) != 1 || first.Contents[0].Parts[0].Text != "find alpha" {
		t.Fatalf("first request contents = %#v", first.Contents)
	}
	second := model.requests[1]
	if len(second.Contents) != 3 {
		t.Fatalf("second request contents = %#v", second.Contents)
	}
	if second.Contents[0].Role != genai.RoleUser || second.Contents[0].Parts[0].Text != "find alpha" {
		t.Fatalf("second request user content = %#v", second.Contents[0])
	}
	functionCall := second.Contents[1].Parts[0].FunctionCall
	if second.Contents[1].Role != genai.RoleModel || functionCall == nil || functionCall.ID != "call-1" || functionCall.Name != "lookup" || !reflect.DeepEqual(functionCall.Args, map[string]any{"key": "alpha"}) {
		t.Fatalf("second request function call = %#v", second.Contents[1])
	}
	functionResponse := second.Contents[2].Parts[0].FunctionResponse
	if second.Contents[2].Role != genai.RoleUser || functionResponse == nil || functionResponse.ID != "call-1" || functionResponse.Name != "lookup" || functionResponse.Response["value"] != "beta" {
		t.Fatalf("second request function response = %#v", second.Contents[2])
	}

	var toolCallEvent, toolResultEvent, finalText *loop.Event
	for index := range events {
		event := &events[index]
		switch event.Kind {
		case loop.EventToolCall:
			toolCallEvent = event
		case loop.EventToolResult:
			toolResultEvent = event
		case loop.EventText:
			if event.Text == "lookup completed" {
				finalText = event
			}
		}
	}
	if toolCallEvent == nil || toolCallEvent.Tool == nil || toolCallEvent.Tool.ID != "call-1" || toolCallEvent.Tool.Name != "lookup" {
		t.Fatalf("tool call event missing from %#v", events)
	}
	if toolResultEvent == nil || toolResultEvent.Tool == nil || toolResultEvent.Tool.CallID != "call-1" || toolResultEvent.Tool.Name != "lookup" || toolResultEvent.Tool.Content["value"] != "beta" {
		t.Fatalf("tool result event missing from %#v", events)
	}
	if finalText == nil || !finalText.Final || finalText.SessionID != "session-1" || finalText.InvocationID == "" || finalText.Text != "lookup completed" {
		t.Fatalf("final text event = %#v", finalText)
	}
	if toolCallEvent.InvocationID != toolResultEvent.InvocationID || toolCallEvent.InvocationID != finalText.InvocationID {
		t.Fatalf("invocation IDs = %q, %q, %q", toolCallEvent.InvocationID, toolResultEvent.InvocationID, finalText.InvocationID)
	}
}

func TestProviderLifecycle(t *testing.T) {
	newProvider := func(t *testing.T) (loop.Provider, *recordingSessionStore) {
		t.Helper()
		provider, err := New(Config{Model: &singleResponseModel{text: "done"}})
		if err != nil {
			t.Fatal(err)
		}
		return provider, newRecordingSessionStore()
	}
	validConfig := func(store loop.SessionStore) loop.RunnerConfig {
		return loop.RunnerConfig{AppName: "app", AgentName: "agent", Sessions: store}
	}

	t.Run("run before configure", func(t *testing.T) {
		provider, _ := newProvider(t)
		_, errs := collectProviderRun(provider.Run(t.Context(), loop.RunRequest{}))
		if len(errs) != 1 || !errors.Is(errs[0], ErrNotConfigured) {
			t.Fatalf("errors = %v", errs)
		}
	})

	t.Run("invalid configure does not consume slot", func(t *testing.T) {
		provider, store := newProvider(t)
		if err := provider.Configure(t.Context(), loop.RunnerConfig{}); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("invalid Configure error = %v", err)
		}
		if err := provider.Configure(t.Context(), validConfig(store)); err != nil {
			t.Fatalf("valid Configure error = %v", err)
		}
		if err := provider.Configure(t.Context(), validConfig(store)); !errors.Is(err, ErrAlreadyConfigured) {
			t.Fatalf("second Configure error = %v", err)
		}
	})

	t.Run("canceled configure does not consume slot", func(t *testing.T) {
		provider, store := newProvider(t)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := provider.Configure(ctx, validConfig(store)); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled Configure error = %v", err)
		}
		if err := provider.Configure(t.Context(), validConfig(store)); err != nil {
			t.Fatalf("valid Configure error = %v", err)
		}
	})

	t.Run("close is idempotent and terminal", func(t *testing.T) {
		provider, store := newProvider(t)
		if err := provider.Configure(t.Context(), validConfig(store)); err != nil {
			t.Fatal(err)
		}
		if err := provider.Close(); err != nil {
			t.Fatal(err)
		}
		if err := provider.Close(); err != nil {
			t.Fatal(err)
		}
		_, errs := collectProviderRun(provider.Run(t.Context(), loop.RunRequest{}))
		if len(errs) != 1 || !errors.Is(errs[0], ErrClosed) {
			t.Fatalf("run-after-close errors = %v", errs)
		}
		if err := provider.Configure(t.Context(), validConfig(store)); !errors.Is(err, ErrClosed) {
			t.Fatalf("configure-after-close error = %v", err)
		}
	})
}

type singleResponseModel struct {
	text string
}

func (*singleResponseModel) Name() string { return "single" }
func (m *singleResponseModel) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{Content: genai.NewContentFromText(m.text, genai.RoleModel)}, nil)
	}
}

func TestProviderEarlyBreakCancelsAndRemovesRun(t *testing.T) {
	model := &contextCaptureModel{}
	store := newRecordingSessionStore()
	if _, err := store.Create(t.Context(), loop.CreateSessionRequest{AppName: "app", UserID: "user", SessionID: "session"}); err != nil {
		t.Fatal(err)
	}
	providerInterface, err := New(Config{Model: model})
	if err != nil {
		t.Fatal(err)
	}
	if err := providerInterface.Configure(t.Context(), loop.RunnerConfig{AppName: "app", AgentName: "agent", Sessions: store}); err != nil {
		t.Fatal(err)
	}
	provider := providerInterface.(*provider)
	seen := 0
	provider.Run(t.Context(), loop.RunRequest{UserID: "user", SessionID: "session", Input: loop.Message{Role: loop.RoleUser, Text: "hello"}})(func(loop.Event, error) bool {
		seen++
		return false
	})
	if seen != 1 || model.ctx == nil || !errors.Is(model.ctx.Err(), context.Canceled) {
		t.Fatalf("seen = %d, model context error = %v", seen, model.contextErr())
	}
	provider.mu.Lock()
	active := len(provider.active)
	provider.mu.Unlock()
	if active != 0 {
		t.Fatalf("active runs = %d", active)
	}
}

type contextCaptureModel struct {
	ctx context.Context
}

func (*contextCaptureModel) Name() string { return "capture" }
func (m *contextCaptureModel) GenerateContent(ctx context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	m.ctx = ctx
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{Content: genai.NewContentFromText("one", genai.RoleModel)}, nil)
	}
}
func (m *contextCaptureModel) contextErr() error {
	if m.ctx == nil {
		return nil
	}
	return m.ctx.Err()
}

func TestProviderCloseCancelsActiveRun(t *testing.T) {
	model := &blockingModel{entered: make(chan struct{})}
	store := newRecordingSessionStore()
	if _, err := store.Create(t.Context(), loop.CreateSessionRequest{AppName: "app", UserID: "user", SessionID: "session"}); err != nil {
		t.Fatal(err)
	}
	provider, err := New(Config{Model: model})
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.Configure(t.Context(), loop.RunnerConfig{AppName: "app", AgentName: "agent", Sessions: store}); err != nil {
		t.Fatal(err)
	}
	done := make(chan []error, 1)
	go func() {
		_, errs := collectProviderRun(provider.Run(context.Background(), loop.RunRequest{UserID: "user", SessionID: "session", Input: loop.Message{Role: loop.RoleUser, Text: "wait"}}))
		done <- errs
	}()
	<-model.entered
	if err := provider.Close(); err != nil {
		t.Fatal(err)
	}
	errs := <-done
	if len(errs) == 0 || !errors.Is(errs[len(errs)-1], context.Canceled) {
		t.Fatalf("run errors = %v", errs)
	}
}

type blockingModel struct {
	entered chan struct{}
	once    sync.Once
}

func (*blockingModel) Name() string { return "blocking" }
func (m *blockingModel) GenerateContent(ctx context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	m.once.Do(func() { close(m.entered) })
	return func(yield func(*model.LLMResponse, error) bool) {
		<-ctx.Done()
		yield(nil, ctx.Err())
	}
}

func TestProviderConcurrentDistinctSessions(t *testing.T) {
	model := newConcurrentSessionModel(2)
	store := newRecordingSessionStore()
	for _, id := range []string{"session-a", "session-b"} {
		if _, err := store.Create(t.Context(), loop.CreateSessionRequest{AppName: "app", UserID: "user", SessionID: id}); err != nil {
			t.Fatal(err)
		}
	}
	provider, err := New(Config{Model: model})
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.Configure(t.Context(), loop.RunnerConfig{AppName: "app", AgentName: "agent", Sessions: store}); err != nil {
		t.Fatal(err)
	}

	type result struct {
		session string
		events  []loop.Event
		errs    []error
	}
	results := make(chan result, 2)
	var group sync.WaitGroup
	for _, id := range []string{"session-a", "session-b"} {
		group.Add(1)
		go func(sessionID string) {
			defer group.Done()
			events, errs := collectProviderRun(provider.Run(context.Background(), loop.RunRequest{
				UserID: "user", SessionID: sessionID, Input: loop.Message{Role: loop.RoleUser, Text: sessionID},
			}))
			results <- result{session: sessionID, events: events, errs: errs}
		}(id)
	}
	group.Wait()
	close(results)
	for run := range results {
		if len(run.errs) != 0 {
			t.Errorf("%s errors = %v", run.session, run.errs)
		}
		found := false
		for _, event := range run.events {
			if event.SessionID != run.session {
				t.Errorf("%s received cross-session event %#v", run.session, event)
			}
			if event.Kind == loop.EventText && event.Text == "reply:"+run.session {
				found = true
			}
		}
		if !found {
			t.Errorf("%s events = %#v", run.session, run.events)
		}
	}
	if got := model.promptsSnapshot(); !maps.Equal(got, map[string]int{"session-a": 1, "session-b": 1}) {
		t.Fatalf("model prompts = %#v", got)
	}
}

type concurrentSessionModel struct {
	mu      sync.Mutex
	want    int
	entered int
	release chan struct{}
	prompts map[string]int
}

func newConcurrentSessionModel(want int) *concurrentSessionModel {
	return &concurrentSessionModel{want: want, release: make(chan struct{}), prompts: make(map[string]int)}
}

func (*concurrentSessionModel) Name() string { return "concurrent" }
func (m *concurrentSessionModel) GenerateContent(_ context.Context, request *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	prompt := request.Contents[len(request.Contents)-1].Parts[0].Text
	m.mu.Lock()
	m.prompts[prompt]++
	m.entered++
	if m.entered == m.want {
		close(m.release)
	}
	release := m.release
	m.mu.Unlock()
	return func(yield func(*model.LLMResponse, error) bool) {
		<-release
		yield(&model.LLMResponse{Content: genai.NewContentFromText("reply:"+prompt, genai.RoleModel)}, nil)
	}
}
func (m *concurrentSessionModel) promptsSnapshot() map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return maps.Clone(m.prompts)
}

var _ model.LLM = (*singleResponseModel)(nil)
var _ model.LLM = (*contextCaptureModel)(nil)
var _ model.LLM = (*blockingModel)(nil)
var _ model.LLM = (*concurrentSessionModel)(nil)
