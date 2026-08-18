package plasmid

import (
	"context"
	"errors"
	"iter"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	adkplugin "google.golang.org/adk/v2/plugin"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"github.com/plasmid-dev/plasmid/sessionstore"
)

func TestConstructionFailureClosesEveryInitializedStage(t *testing.T) {
	tests := []struct {
		name      string
		configure func(context.CancelFunc, *[]string, **Harness) []Option
		wantOrder []string
		wantCause error
	}{
		{
			name: "compiled plugin init",
			configure: func(_ context.CancelFunc, order *[]string, captured **Harness) []Option {
				first := &lifecyclePlugin{name: "first", init: func(h *Harness) error {
					*captured = h
					return nil
				}, close: func() error { *order = append(*order, "compiled-first"); return nil }}
				second := &lifecyclePlugin{name: "second", init: func(h *Harness) error {
					*captured = h
					return errors.New("init failed")
				}, close: func() error { *order = append(*order, "compiled-second"); return nil }}
				return []Option{WithPlugins(first, second)}
			},
			wantOrder: []string{"compiled-second", "compiled-first"},
			wantCause: ErrConstructionFailed,
		},
		{
			name: "registry name panic",
			configure: func(_ context.CancelFunc, order *[]string, captured **Harness) []Option {
				compiled := pluginWithNativeResource(t, order, captured)
				return []Option{WithTools(panickingNameTool{}), WithPlugins(compiled)}
			},
			wantOrder: []string{"native", "compiled"},
			wantCause: ErrConstructionFailed,
		},
		{
			name: "toolset name panic",
			configure: func(_ context.CancelFunc, order *[]string, captured **Harness) []Option {
				compiled := pluginWithNativeResource(t, order, captured)
				compiled.init = func(h *Harness) error {
					*captured = h
					native, err := nativeLifecycleResource(order)
					if err != nil {
						return err
					}
					if err := h.RegisterADKPlugins(native); err != nil {
						return err
					}
					return h.RegisterToolsets(panickingNameToolset{})
				}
				return []Option{WithPlugins(compiled)}
			},
			wantOrder: []string{"native", "compiled"},
			wantCause: ErrConstructionFailed,
		},
		{
			name: "post runner cancellation",
			configure: func(cancel context.CancelFunc, order *[]string, captured **Harness) []Option {
				compiled := pluginWithNativeResource(t, order, captured)
				return []Option{WithTools(&cancelNameTool{name: "cancel", cancel: cancel}), WithPlugins(compiled)}
			},
			wantOrder: []string{"native", "compiled"},
			wantCause: context.Canceled,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var order []string
			var captured *Harness
			options := []Option{
				WithModel(lifecycleModel{}),
				WithWorkingDir(t.TempDir()),
				WithSessionDir(filepath.Join(t.TempDir(), "sessions")),
			}
			options = append(options, test.configure(cancel, &order, &captured)...)
			_, err := New(ctx, options...)
			if err == nil || CodeOf(err) != CodeConstructionFailed || !errors.Is(err, test.wantCause) {
				t.Fatalf("New error = %v, code = %q", err, CodeOf(err))
			}
			if !reflect.DeepEqual(order, test.wantOrder) {
				t.Fatalf("close order = %v, want %v", order, test.wantOrder)
			}
			if captured == nil || captured.sessions == nil {
				t.Fatal("plugin did not capture initialized session store")
			}
			_, storeErr := captured.sessions.Create(context.Background(), &session.CreateRequest{AppName: "plasmid", UserID: "default"})
			if !errors.Is(storeErr, sessionstore.ErrClosed) {
				t.Fatalf("session store remained open after construction failure: %v", storeErr)
			}
		})
	}
}

func TestCloseTimesOutThenTearsDownCancellationResistantRunResourcesOnce(t *testing.T) {
	pluginCloses := 0
	h := &Harness{
		rootContext:      context.Background(),
		cancelRoot:       func() {},
		active:           map[string]context.CancelFunc{"busy": func() {}},
		plugins:          []Plugin{&lifecyclePlugin{name: "timed-out", close: func() error { pluginCloses++; return nil }}},
		closeWaitTimeout: 20 * time.Millisecond,
	}
	h.activeRuns.Add(1)
	err := h.Close()
	if CodeOf(err) != CodeCloseFailed || !errors.Is(err, ErrCloseFailed) || !errors.Is(err, ErrCloseTimeout) {
		t.Fatalf("Close error = %v, code = %q", err, CodeOf(err))
	}
	if pluginCloses != 1 {
		t.Fatalf("plugin closes = %d, want 1 after timeout", pluginCloses)
	}
	select {
	case <-h.closeDone:
	default:
		t.Fatal("Close returned before teardown completion")
	}
	secondErr := h.Close()
	if !errors.Is(secondErr, ErrCloseTimeout) || pluginCloses != 1 {
		t.Fatalf("second Close = %v, plugin closes = %d", secondErr, pluginCloses)
	}
	h.activeRuns.Done()
}

func TestCloseOrderIncludesCompiledNativeAndSessionStore(t *testing.T) {
	store, err := sessionstore.Open(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	assertStoreOpen := func(name string) func() error {
		return func() error {
			if _, err := store.Create(t.Context(), &session.CreateRequest{AppName: "plasmid", UserID: "default"}); err != nil {
				return err
			}
			order = append(order, name)
			return nil
		}
	}
	nativeFirst, err := adkplugin.New(adkplugin.Config{Name: "native-first", CloseFunc: assertStoreOpen("native-first")})
	if err != nil {
		t.Fatal(err)
	}
	nativeSecond, err := adkplugin.New(adkplugin.Config{Name: "native-second", CloseFunc: assertStoreOpen("native-second")})
	if err != nil {
		t.Fatal(err)
	}
	h := &Harness{
		rootContext: context.Background(),
		cancelRoot:  func() {},
		active:      make(map[string]context.CancelFunc),
		plugins: []Plugin{
			&lifecyclePlugin{name: "compiled-first", close: assertStoreOpen("compiled-first")},
			&lifecyclePlugin{name: "compiled-second", close: assertStoreOpen("compiled-second")},
		},
		adkPlugins:       []*adkplugin.Plugin{nativeFirst, nativeSecond},
		sessions:         store,
		closeWaitTimeout: time.Second,
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	want := []string{"native-second", "native-first", "compiled-second", "compiled-first"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("close order = %v, want %v", order, want)
	}
	if _, err := store.Create(t.Context(), &session.CreateRequest{AppName: "plasmid", UserID: "default"}); !errors.Is(err, sessionstore.ErrClosed) {
		t.Fatalf("session store remained open after Close: %v", err)
	}
}

func TestPublicSessionAndTemplateAPIsRaceCloseWithoutPanicking(t *testing.T) {
	h, err := New(t.Context(), WithModel(lifecycleModel{}), WithWorkingDir(t.TempDir()), WithSessionDir(filepath.Join(t.TempDir(), "sessions")), WithLSP(LSPOff))
	if err != nil {
		t.Fatal(err)
	}
	sessionID, err := h.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := make(chan struct{})
	var workers sync.WaitGroup
	for index := 0; index < 12; index++ {
		workers.Add(1)
		go func(worker int) {
			defer workers.Done()
			<-start
			for ctx.Err() == nil {
				var callErr error
				switch worker % 3 {
				case 0:
					_, callErr = h.ListTemplates(ctx, sessionID)
				case 1:
					_, callErr = h.GetTemplate(ctx, sessionID, "missing", "")
				default:
					callErr = h.ResumeSession(ctx, sessionID)
				}
				if errors.Is(callErr, ErrClosed) {
					return
				}
			}
		}(index)
	}
	close(start)
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	workers.Wait()
}

func pluginWithNativeResource(t *testing.T, order *[]string, captured **Harness) *lifecyclePlugin {
	t.Helper()
	return &lifecyclePlugin{name: "compiled", init: func(h *Harness) error {
		*captured = h
		native, err := nativeLifecycleResource(order)
		if err != nil {
			return err
		}
		return h.RegisterADKPlugins(native)
	}, close: func() error { *order = append(*order, "compiled"); return nil }}
}

func nativeLifecycleResource(order *[]string) (*adkplugin.Plugin, error) {
	return adkplugin.New(adkplugin.Config{
		Name:      "native",
		CloseFunc: func() error { *order = append(*order, "native"); return nil },
	})
}

type lifecyclePlugin struct {
	name  string
	init  func(*Harness) error
	close func() error
}

func (p *lifecyclePlugin) Name() string { return p.name }
func (p *lifecyclePlugin) Init(h *Harness) error {
	if p.init != nil {
		return p.init(h)
	}
	return nil
}
func (p *lifecyclePlugin) Close() error {
	if p.close != nil {
		return p.close()
	}
	return nil
}

type lifecycleModel struct{}

func (lifecycleModel) Name() string { return "lifecycle" }
func (lifecycleModel) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{Content: genai.NewContentFromText("ok", genai.RoleModel)}, nil)
	}
}

type panickingNameTool struct{}

func (panickingNameTool) Name() string        { panic("tool name panic") }
func (panickingNameTool) Description() string { return "panic" }
func (panickingNameTool) IsLongRunning() bool { return false }

type panickingNameToolset struct{}

func (panickingNameToolset) Name() string { panic("toolset name panic") }
func (panickingNameToolset) Tools(agent.ReadonlyContext) ([]tool.Tool, error) {
	return nil, nil
}

type cancelNameTool struct {
	name   string
	cancel context.CancelFunc
	once   sync.Once
}

func (t *cancelNameTool) Name() string {
	t.once.Do(t.cancel)
	return t.name
}
func (*cancelNameTool) Description() string { return "cancel construction" }
func (*cancelNameTool) IsLongRunning() bool { return false }
