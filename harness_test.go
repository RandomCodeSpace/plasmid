package plasmid_test

import (
	"context"
	"errors"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	adkplugin "google.golang.org/adk/v2/plugin"
	adktool "google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"github.com/plasmid-dev/plasmid"
)

func TestHarnessRunsNativeToolAndResumesDurableSession(t *testing.T) {
	workingDir := t.TempDir()
	sessionDir := filepath.Join(t.TempDir(), "sessions")
	if err := os.WriteFile(filepath.Join(workingDir, "message.txt"), []byte("durable\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	firstModel := &scriptedModel{responses: []*genai.Content{
		{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
			ID: "read-1", Name: "read", Args: map[string]any{"path": "message.txt"},
		}}}},
		genai.NewContentFromText("first answer", genai.RoleModel),
	}}
	first, err := plasmid.New(t.Context(),
		plasmid.WithModel(firstModel),
		plasmid.WithWorkingDir(workingDir),
		plasmid.WithSessionDir(sessionDir),
	)
	if err != nil {
		t.Fatal(err)
	}
	sessionID, err := first.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if sessionID == "" {
		t.Fatal("NewSession returned an empty id")
	}
	answer, err := first.Ask(t.Context(), sessionID, "read the message")
	if err != nil {
		t.Fatal(err)
	}
	if answer != "first answer" {
		t.Fatalf("Ask = %q, want %q", answer, "first answer")
	}
	if firstModel.calls != 2 || !firstModel.sawTool("read") {
		t.Fatalf("model calls = %d, tools = %#v", firstModel.calls, firstModel.tools)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	secondModel := &scriptedModel{responses: []*genai.Content{
		genai.NewContentFromText("resumed answer", genai.RoleModel),
	}}
	second, err := plasmid.New(t.Context(),
		plasmid.WithModel(secondModel),
		plasmid.WithWorkingDir(workingDir),
		plasmid.WithSessionDir(sessionDir),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if err := second.ResumeSession(t.Context(), sessionID); err != nil {
		t.Fatal(err)
	}
	answer, err = second.Ask(t.Context(), sessionID, "continue")
	if err != nil {
		t.Fatal(err)
	}
	if answer != "resumed answer" {
		t.Fatalf("resumed Ask = %q, want %q", answer, "resumed answer")
	}
	if secondModel.historyContents < 2 {
		t.Fatalf("resumed model history contents = %d, want at least 2", secondModel.historyContents)
	}
}

type scriptedModel struct {
	responses       []*genai.Content
	calls           int
	tools           map[string]bool
	historyContents int
}

func (*scriptedModel) Name() string { return "scripted" }

func (m *scriptedModel) GenerateContent(_ context.Context, request *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		m.tools = make(map[string]bool, len(request.Tools))
		for name := range request.Tools {
			m.tools[name] = true
		}
		m.historyContents = len(request.Contents)
		response := m.responses[m.calls]
		m.calls++
		yield(&model.LLMResponse{Content: response}, nil)
	}
}

func (m *scriptedModel) sawTool(name string) bool { return m.tools[name] }

var _ model.LLM = (*scriptedModel)(nil)

func TestHarnessRejectsUnknownBusyAndClosedSessions(t *testing.T) {
	blocking := newBlockingModel()
	harness := newHarness(t, blocking)
	sessionID, err := harness.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	var unknown error
	for _, runErr := range harness.Run(t.Context(), "missing", "hello") {
		unknown = runErr
	}
	if plasmid.CodeOf(unknown) != plasmid.CodeUnknownSession || !errors.Is(unknown, plasmid.ErrUnknownSession) {
		t.Fatalf("unknown error = %v, code = %q", unknown, plasmid.CodeOf(unknown))
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		var runErr error
		for _, err := range harness.Run(ctx, sessionID, "block") {
			if err != nil {
				runErr = err
			}
		}
		done <- runErr
	}()
	blocking.waitStarted(t)
	var busy error
	for _, runErr := range harness.Run(t.Context(), sessionID, "second") {
		busy = runErr
	}
	if plasmid.CodeOf(busy) != plasmid.CodeSessionBusy || !errors.Is(busy, plasmid.ErrSessionBusy) {
		t.Fatalf("busy error = %v, code = %q", busy, plasmid.CodeOf(busy))
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled run did not stop")
	}
	if err := harness.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.NewSession(t.Context()); plasmid.CodeOf(err) != plasmid.CodeClosed || !errors.Is(err, plasmid.ErrClosed) {
		t.Fatalf("closed error = %v, code = %q", err, plasmid.CodeOf(err))
	}
}

func TestHarnessAllowsDistinctSessionsConcurrently(t *testing.T) {
	blocking := newBlockingModel()
	harness := newHarness(t, blocking)
	defer harness.Close()
	first, err := harness.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	second, err := harness.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var group sync.WaitGroup
	for _, sessionID := range []string{first, second} {
		group.Add(1)
		go func(id string) {
			defer group.Done()
			for range harness.Run(ctx, id, "block") {
			}
		}(sessionID)
	}
	blocking.waitStarted(t)
	blocking.waitStarted(t)
	if got := blocking.active.Load(); got != 2 {
		t.Fatalf("active model calls = %d, want 2", got)
	}
	cancel()
	completed := make(chan struct{})
	go func() {
		group.Wait()
		close(completed)
	}()
	select {
	case <-completed:
	case <-time.After(2 * time.Second):
		t.Fatal("distinct session runs did not stop")
	}
}

func TestHarnessEarlyBreakCancelsRunContext(t *testing.T) {
	model := &cancellationModel{cancelled: make(chan struct{})}
	harness := newHarness(t, model)
	defer harness.Close()
	sessionID, err := harness.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for event, runErr := range harness.Run(t.Context(), sessionID, "answer") {
		if runErr != nil {
			t.Fatal(runErr)
		}
		if event != nil {
			break
		}
	}
	select {
	case <-model.cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("run context was not cancelled after early iterator break")
	}
}

func TestHarnessRunOutlivesConstructionContextAndCloseCancelsActiveRun(t *testing.T) {
	constructionContext, cancelConstruction := context.WithCancel(t.Context())
	model := &scriptedModel{responses: []*genai.Content{genai.NewContentFromText("independent", genai.RoleModel)}}
	harness, err := plasmid.New(constructionContext,
		plasmid.WithModel(model),
		plasmid.WithWorkingDir(t.TempDir()),
		plasmid.WithSessionDir(filepath.Join(t.TempDir(), "sessions")),
	)
	if err != nil {
		t.Fatal(err)
	}
	cancelConstruction()
	sessionID, err := harness.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	answer, err := harness.Ask(t.Context(), sessionID, "still alive")
	if err != nil || answer != "independent" {
		t.Fatalf("Ask after construction cancellation = %q, %v", answer, err)
	}
	if err := harness.Close(); err != nil {
		t.Fatal(err)
	}

	blocking := newBlockingModel()
	harness = newHarness(t, blocking)
	sessionID, err = harness.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		for range harness.Run(t.Context(), sessionID, "block") {
		}
	}()
	blocking.waitStarted(t)
	if err := harness.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not cancel the active run")
	}
}

func TestHarnessConstructionUnwindsPluginsAndRejectsCollisions(t *testing.T) {
	var orderMu sync.Mutex
	var order []string
	first := &compiledPlugin{name: "first", close: func() error {
		orderMu.Lock()
		order = append(order, "first")
		orderMu.Unlock()
		return nil
	}}
	second := &compiledPlugin{name: "second", init: func(*plasmid.Harness) error {
		return errors.New("init failed")
	}, close: func() error {
		orderMu.Lock()
		order = append(order, "second")
		orderMu.Unlock()
		return nil
	}}
	_, err := plasmid.New(t.Context(),
		plasmid.WithModel(&scriptedModel{responses: []*genai.Content{genai.NewContentFromText("unused", genai.RoleModel)}}),
		plasmid.WithWorkingDir(t.TempDir()),
		plasmid.WithSessionDir(filepath.Join(t.TempDir(), "sessions")),
		plasmid.WithPlugins(first, second),
	)
	if plasmid.CodeOf(err) != plasmid.CodeConstructionFailed || !errors.Is(err, plasmid.ErrConstructionFailed) {
		t.Fatalf("construction error = %v, code = %q", err, plasmid.CodeOf(err))
	}
	if len(order) != 2 || order[0] != "second" || order[1] != "first" {
		t.Fatalf("close order = %#v, want [second first]", order)
	}

	collision := &compiledPlugin{name: "collision", init: func(h *plasmid.Harness) error {
		registered, pluginErr := adkplugin.New(adkplugin.Config{
			Name: "collision-native",
			CloseFunc: func() error {
				orderMu.Lock()
				order = append(order, "collision-native")
				orderMu.Unlock()
				return nil
			},
		})
		if pluginErr != nil {
			return pluginErr
		}
		if err := h.RegisterADKPlugins(registered); err != nil {
			return err
		}
		return h.RegisterTools(namedTool("read"))
	}}
	_, err = plasmid.New(t.Context(),
		plasmid.WithModel(&scriptedModel{responses: []*genai.Content{genai.NewContentFromText("unused", genai.RoleModel)}}),
		plasmid.WithWorkingDir(t.TempDir()),
		plasmid.WithSessionDir(filepath.Join(t.TempDir(), "sessions")),
		plasmid.WithPlugins(collision),
	)
	if plasmid.CodeOf(err) != plasmid.CodeDuplicate || !errors.Is(err, plasmid.ErrDuplicate) || !errors.Is(err, plasmid.ErrConstructionFailed) {
		t.Fatalf("collision error = %v, code = %q", err, plasmid.CodeOf(err))
	}
	if order[len(order)-1] != "collision-native" {
		t.Fatalf("construction failure did not close registered native plugin: %#v", order)
	}

	panicking := &compiledPlugin{
		name: "panicking",
		init: func(*plasmid.Harness) error {
			panic("init panic")
		},
		close: func() error {
			panic("close panic")
		},
	}
	_, err = plasmid.New(t.Context(),
		plasmid.WithModel(&scriptedModel{responses: []*genai.Content{genai.NewContentFromText("unused", genai.RoleModel)}}),
		plasmid.WithWorkingDir(t.TempDir()),
		plasmid.WithSessionDir(filepath.Join(t.TempDir(), "sessions")),
		plasmid.WithPlugins(panicking),
	)
	if plasmid.CodeOf(err) != plasmid.CodeConstructionFailed || !strings.Contains(err.Error(), "init panic") || !strings.Contains(err.Error(), "close panic") {
		t.Fatalf("panicking construction error = %v", err)
	}
}

func TestHarnessNativePluginCallbackRunsOnceAndCloseIsConcurrentSafe(t *testing.T) {
	var callbackOrderMu sync.Mutex
	var callbackOrder []string
	var callbacks atomic.Int64
	var compiledCallbacks atomic.Int64
	var nativeCloses atomic.Int64
	native, err := adkplugin.New(adkplugin.Config{
		Name: "native",
		BeforeRunCallback: func(agent.InvocationContext) (*genai.Content, error) {
			callbackOrderMu.Lock()
			callbackOrder = append(callbackOrder, "native-run")
			callbackOrderMu.Unlock()
			return nil, nil
		},
		BeforeModelCallback: func(_ agent.Context, request *model.LLMRequest) (*model.LLMResponse, error) {
			callbacks.Add(1)
			callbackOrderMu.Lock()
			callbackOrder = append(callbackOrder, "native-model")
			callbackOrderMu.Unlock()
			if len(request.Contents) == 0 || request.Contents[len(request.Contents)-1].Parts[0].Text != "compiled mutation" {
				return nil, errors.New("native callback did not observe compiled mutation")
			}
			return &model.LLMResponse{Content: genai.NewContentFromText("short-circuited", genai.RoleModel)}, nil
		},
		CloseFunc: func() error {
			nativeCloses.Add(1)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var compiledCloses atomic.Int64
	compiledNative, err := adkplugin.New(adkplugin.Config{
		Name: "compiled-native",
		BeforeRunCallback: func(agent.InvocationContext) (*genai.Content, error) {
			callbackOrderMu.Lock()
			callbackOrder = append(callbackOrder, "compiled-run")
			callbackOrderMu.Unlock()
			return nil, nil
		},
		BeforeModelCallback: func(_ agent.Context, request *model.LLMRequest) (*model.LLMResponse, error) {
			compiledCallbacks.Add(1)
			callbackOrderMu.Lock()
			callbackOrder = append(callbackOrder, "compiled-model")
			callbackOrderMu.Unlock()
			request.Contents = append(request.Contents, genai.NewContentFromText("compiled mutation", genai.RoleUser))
			return nil, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	compiled := &compiledPlugin{
		name: "compiled",
		init: func(h *plasmid.Harness) error { return h.RegisterADKPlugins(compiledNative) },
		close: func() error {
			compiledCloses.Add(1)
			return nil
		},
	}
	underlying := &scriptedModel{responses: []*genai.Content{genai.NewContentFromText("model should not run", genai.RoleModel)}}
	harness := newHarnessWithOptions(t,
		underlying,
		plasmid.WithPlugins(compiled), plasmid.WithADKPlugins(native),
	)
	sessionID, err := harness.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	answer, err := harness.Ask(t.Context(), sessionID, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if answer != "short-circuited" || underlying.calls != 0 {
		t.Fatalf("Ask = %q, model calls = %d", answer, underlying.calls)
	}
	if callbacks.Load() != 1 || compiledCallbacks.Load() != 1 {
		t.Fatalf("before-model callbacks native=%d compiled=%d, want 1 each", callbacks.Load(), compiledCallbacks.Load())
	}
	callbackOrderMu.Lock()
	if strings.Join(callbackOrder, ",") != "compiled-run,native-run,compiled-model,native-model" {
		t.Fatalf("callback order = %v", callbackOrder)
	}
	callbackOrderMu.Unlock()

	const closers = 16
	errorsSeen := make(chan error, closers)
	var group sync.WaitGroup
	for range closers {
		group.Add(1)
		go func() {
			defer group.Done()
			errorsSeen <- harness.Close()
		}()
	}
	group.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
	if compiledCloses.Load() != 1 || nativeCloses.Load() != 1 {
		t.Fatalf("close counts compiled=%d native=%d, want 1 each", compiledCloses.Load(), nativeCloses.Load())
	}
	if err := harness.RegisterTools(namedTool("late")); plasmid.CodeOf(err) != plasmid.CodeRegistrationSealed {
		t.Fatalf("late registration error = %v, code = %q", err, plasmid.CodeOf(err))
	}
}

func TestHarnessWrapsRuntimeErrors(t *testing.T) {
	cause := errors.New("model failed")
	harness := newHarness(t, failingModel{err: cause})
	defer harness.Close()
	sessionID, err := harness.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var runErr error
	for _, err := range harness.Run(t.Context(), sessionID, "fail") {
		if err != nil {
			runErr = err
		}
	}
	if plasmid.CodeOf(runErr) != plasmid.CodeRuntimeFailed || !errors.Is(runErr, plasmid.ErrRuntimeFailed) || !errors.Is(runErr, cause) {
		t.Fatalf("runtime error = %v, code = %q", runErr, plasmid.CodeOf(runErr))
	}
}

func newHarness(t *testing.T, value model.LLM) *plasmid.Harness {
	t.Helper()
	return newHarnessWithOptions(t, value)
}

func newHarnessWithOptions(t *testing.T, value model.LLM, extra ...plasmid.Option) *plasmid.Harness {
	t.Helper()
	options := []plasmid.Option{
		plasmid.WithModel(value),
		plasmid.WithWorkingDir(t.TempDir()),
		plasmid.WithSessionDir(filepath.Join(t.TempDir(), "sessions")),
	}
	options = append(options, extra...)
	harness, err := plasmid.New(t.Context(), options...)
	if err != nil {
		t.Fatal(err)
	}
	return harness
}

type blockingModel struct {
	started chan struct{}
	active  atomic.Int64
}

func newBlockingModel() *blockingModel { return &blockingModel{started: make(chan struct{}, 8)} }
func (*blockingModel) Name() string    { return "blocking" }
func (m *blockingModel) GenerateContent(ctx context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		m.active.Add(1)
		defer m.active.Add(-1)
		m.started <- struct{}{}
		<-ctx.Done()
		yield(nil, ctx.Err())
	}
}
func (m *blockingModel) waitStarted(t *testing.T) {
	t.Helper()
	select {
	case <-m.started:
	case <-time.After(2 * time.Second):
		t.Fatal("model did not start")
	}
}

type cancellationModel struct {
	cancelled chan struct{}
	once      sync.Once
}

type failingModel struct{ err error }

func (failingModel) Name() string { return "failing" }
func (m failingModel) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) { yield(nil, m.err) }
}

func (*cancellationModel) Name() string { return "cancellation" }
func (m *cancellationModel) GenerateContent(ctx context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	go func() {
		<-ctx.Done()
		m.once.Do(func() { close(m.cancelled) })
	}()
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{Content: genai.NewContentFromText("answer", genai.RoleModel)}, nil)
	}
}

type compiledPlugin struct {
	name  string
	init  func(*plasmid.Harness) error
	close func() error
}

func (p *compiledPlugin) Name() string { return p.name }
func (p *compiledPlugin) Init(h *plasmid.Harness) error {
	if p.init != nil {
		return p.init(h)
	}
	return nil
}
func (p *compiledPlugin) Close() error {
	if p.close != nil {
		return p.close()
	}
	return nil
}

type namedTool string

func (t namedTool) Name() string      { return string(t) }
func (namedTool) Description() string { return "test tool" }
func (namedTool) IsLongRunning() bool { return false }

var _ adktool.Tool = namedTool("")
