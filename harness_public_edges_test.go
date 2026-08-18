package plasmid_test

import (
	"context"
	"errors"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	adkplugin "google.golang.org/adk/v2/plugin"
	"google.golang.org/adk/v2/session"
	adktool "google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"

	"github.com/plasmid-dev/plasmid"
	"github.com/plasmid-dev/plasmid/warning"
)

func TestHarnessRejectsInvalidPublicConstructionInputs(t *testing.T) {
	workingFile := filepath.Join(t.TempDir(), "workspace-file")
	if err := os.WriteFile(workingFile, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	sessionFile := filepath.Join(t.TempDir(), "session-file")
	if err := os.WriteFile(sessionFile, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	var nilOption plasmid.Option
	var nilModel *scriptedModel
	tests := []struct {
		name    string
		options []plasmid.Option
		code    plasmid.ErrorCode
	}{
		{name: "nil option", options: []plasmid.Option{nilOption}, code: plasmid.CodeInvalidArgument},
		{name: "typed nil model", options: []plasmid.Option{plasmid.WithModel(nilModel)}, code: plasmid.CodeInvalidArgument},
		{name: "missing config", options: publicHarnessOptions(t, plasmid.WithConfig(filepath.Join(t.TempDir(), "missing.json"))), code: plasmid.CodeConstructionFailed},
		{name: "workspace is a file", options: publicHarnessOptions(t, plasmid.WithWorkingDir(workingFile)), code: plasmid.CodeConstructionFailed},
		{name: "session path is a file", options: publicHarnessOptions(t, plasmid.WithSessionDir(sessionFile)), code: plasmid.CodeConstructionFailed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertHarnessConstructionCode(t, test.code, test.options...)
		})
	}
	var nilContext context.Context
	if _, err := plasmid.New(nilContext, plasmid.WithModel(emptyModel{})); plasmid.CodeOf(err) != plasmid.CodeInvalidArgument {
		t.Fatalf("New(nil) error = %v", err)
	}
}

func TestHarnessRejectsInvalidPublicRegistrations(t *testing.T) {
	validNative, err := adkplugin.New(adkplugin.Config{Name: "native"})
	if err != nil {
		t.Fatal(err)
	}
	var nilPlugin *compiledPlugin
	var nilTool *pointerTool
	var nilToolset *publicToolset
	tests := []struct {
		name   string
		option plasmid.Option
		code   plasmid.ErrorCode
	}{
		{name: "typed nil compiled plugin", option: plasmid.WithPlugins(nilPlugin), code: plasmid.CodeInvalidArgument},
		{name: "panicking compiled plugin name", option: plasmid.WithPlugins(panicNamePlugin{}), code: plasmid.CodeInvalidArgument},
		{name: "empty compiled plugin name", option: plasmid.WithPlugins(&compiledPlugin{}), code: plasmid.CodeInvalidArgument},
		{name: "duplicate compiled plugin name", option: plasmid.WithPlugins(&compiledPlugin{name: "same"}, &compiledPlugin{name: "same"}), code: plasmid.CodeDuplicate},
		{name: "typed nil tool", option: plasmid.WithTools(nilTool), code: plasmid.CodeInvalidArgument},
		{name: "empty tool name", option: plasmid.WithTools(namedTool("")), code: plasmid.CodeInvalidArgument},
		{name: "panicking tool name", option: plasmid.WithTools(panicNameTool{}), code: plasmid.CodeConstructionFailed},
		{name: "duplicate built-in tool name", option: plasmid.WithTools(namedTool("read")), code: plasmid.CodeDuplicate},
		{name: "reserved skill tool name", option: plasmid.WithTools(namedTool("list_skills")), code: plasmid.CodeDuplicate},
		{name: "typed nil toolset", option: registrationOption("nil-toolset", func(h *plasmid.Harness) error { return h.RegisterToolsets(nilToolset) }), code: plasmid.CodeInvalidArgument},
		{name: "empty toolset name", option: registrationOption("empty-toolset", func(h *plasmid.Harness) error { return h.RegisterToolsets(publicToolset{}) }), code: plasmid.CodeInvalidArgument},
		{name: "panicking toolset name", option: registrationOption("panic-toolset", func(h *plasmid.Harness) error { return h.RegisterToolsets(publicToolset{panicName: true}) }), code: plasmid.CodeConstructionFailed},
		{name: "duplicate toolset name", option: registrationOption("duplicate-toolset", func(h *plasmid.Harness) error {
			return h.RegisterToolsets(publicToolset{name: "same"}, publicToolset{name: "same"})
		}), code: plasmid.CodeDuplicate},
		{name: "nil native plugin", option: plasmid.WithADKPlugins(nil), code: plasmid.CodeInvalidArgument},
		{name: "duplicate native plugin", option: plasmid.WithADKPlugins(validNative, validNative), code: plasmid.CodeDuplicate},
		{name: "blank prompt fragment", option: registrationOption("blank-fragment", func(h *plasmid.Harness) error { return h.RegisterPromptFragments(plasmid.PromptFragment{}) }), code: plasmid.CodeInvalidArgument},
		{name: "duplicate prompt fragment", option: registrationOption("duplicate-fragment", func(h *plasmid.Harness) error {
			return h.RegisterPromptFragments(
				plasmid.PromptFragment{Name: "same", Content: "first"},
				plasmid.PromptFragment{Name: "same", Content: "second"},
			)
		}), code: plasmid.CodeDuplicate},
		{name: "blank warning code", option: registrationOption("blank-warning", func(h *plasmid.Harness) error { return h.RegisterWarnings(warning.Warning{}) }), code: plasmid.CodeInvalidArgument},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertHarnessConstructionCode(t, test.code, publicHarnessOptions(t, test.option)...)
		})
	}
}

func TestHarnessRegistrationAndLifecycleGuardsArePubliclyObservable(t *testing.T) {
	harness := newHarness(t, emptyModel{})
	if err := harness.Close(); err != nil {
		t.Fatal(err)
	}
	validNative, err := adkplugin.New(adkplugin.Config{Name: "late-native"})
	if err != nil {
		t.Fatal(err)
	}
	lateRegistrations := []struct {
		name string
		run  func() error
	}{
		{name: "tool", run: func() error { return harness.RegisterTools(namedTool("late")) }},
		{name: "toolset", run: func() error { return harness.RegisterToolsets(publicToolset{name: "late"}) }},
		{name: "ADK plugin", run: func() error { return harness.RegisterADKPlugins(validNative) }},
		{name: "prompt fragment", run: func() error {
			return harness.RegisterPromptFragments(plasmid.PromptFragment{Name: "late", Content: "late"})
		}},
		{name: "warning", run: func() error { return harness.RegisterWarnings(warning.Warning{Code: "late"}) }},
	}
	for _, registration := range lateRegistrations {
		t.Run(registration.name, func(t *testing.T) {
			if err := registration.run(); plasmid.CodeOf(err) != plasmid.CodeRegistrationSealed {
				t.Fatalf("late registration error = %v", err)
			}
		})
	}

	var nilHarness *plasmid.Harness
	nilRegistrations := []func() error{
		func() error { return nilHarness.RegisterTools(namedTool("nil")) },
		func() error { return nilHarness.RegisterToolsets(publicToolset{name: "nil"}) },
		func() error { return nilHarness.RegisterADKPlugins(validNative) },
		func() error {
			return nilHarness.RegisterPromptFragments(plasmid.PromptFragment{Name: "nil", Content: "nil"})
		},
		func() error { return nilHarness.RegisterWarnings(warning.Warning{Code: "nil"}) },
	}
	for index, register := range nilRegistrations {
		if err := register(); plasmid.CodeOf(err) != plasmid.CodeClosed {
			t.Fatalf("nil registration %d error = %v", index, err)
		}
	}
	if err := nilHarness.Close(); err != nil {
		t.Fatalf("nil Harness Close() = %v", err)
	}
	if _, err := nilHarness.NewSession(t.Context()); plasmid.CodeOf(err) != plasmid.CodeClosed {
		t.Fatalf("nil Harness NewSession() error = %v", err)
	}
	if err := nilHarness.ResumeSession(t.Context(), "session"); plasmid.CodeOf(err) != plasmid.CodeClosed {
		t.Fatalf("nil Harness ResumeSession() error = %v", err)
	}
}

func TestHarnessReportsNoFinalPublicResponse(t *testing.T) {
	harness := newHarness(t, emptyModel{})
	defer closeTestResource(t, harness)
	sessionID, err := harness.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if answer, err := harness.Ask(t.Context(), sessionID, "silent"); answer != "" || plasmid.CodeOf(err) != plasmid.CodeNoFinalResponse {
		t.Fatalf("Ask = %q, %v", answer, err)
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	prompts := filepath.Join(home, ".codex", "prompts")
	if err := os.MkdirAll(prompts, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prompts, "silent.md"), []byte("silent\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := writeHarnessConfig(t, map[string]any{
		"foreign": map[string]any{"enabled": true, "codex": true},
		"lsp":     map[string]any{"mode": "off"},
	})
	templateHarness := newHarnessWithOptions(t, emptyModel{}, plasmid.WithConfig(configPath))
	defer closeTestResource(t, templateHarness)
	templateSession, err := templateHarness.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if answer, err := templateHarness.AskTemplate(t.Context(), templateSession, "silent", ""); answer != "" || plasmid.CodeOf(err) != plasmid.CodeNoFinalResponse {
		t.Fatalf("AskTemplate = %q, %v", answer, err)
	}
	if _, err := templateHarness.GetTemplate(t.Context(), templateSession, "silent", `"unterminated`); plasmid.CodeOf(err) != plasmid.CodeInvalidArgument {
		t.Fatalf("GetTemplate(invalid arguments) error = %v", err)
	}
}

func TestHarnessProjectsAllNativeCallbackKinds(t *testing.T) {
	native, err := adkplugin.New(adkplugin.Config{
		Name: "all-callbacks",
		OnUserMessageCallback: func(agent.InvocationContext, *genai.Content) (*genai.Content, error) {
			return nil, nil
		},
		OnEventCallback: func(agent.InvocationContext, *session.Event) (*session.Event, error) {
			return nil, nil
		},
		BeforeRunCallback: func(agent.InvocationContext) (*genai.Content, error) { return nil, nil },
		AfterRunCallback:  func(agent.InvocationContext) {},
		BeforeAgentCallback: func(agent.Context) (*genai.Content, error) {
			return nil, nil
		},
		AfterAgentCallback: func(agent.Context) (*genai.Content, error) { return nil, nil },
		BeforeModelCallback: func(agent.Context, *model.LLMRequest) (*model.LLMResponse, error) {
			return nil, nil
		},
		AfterModelCallback: func(agent.Context, *model.LLMResponse, error) (*model.LLMResponse, error) {
			return nil, nil
		},
		OnModelErrorCallback: func(agent.Context, *model.LLMRequest, error) (*model.LLMResponse, error) {
			return nil, nil
		},
		BeforeToolCallback: func(agent.Context, adktool.Tool, map[string]any) (map[string]any, error) {
			return nil, nil
		},
		AfterToolCallback: func(agent.Context, adktool.Tool, map[string]any, map[string]any, error) (map[string]any, error) {
			return nil, nil
		},
		OnToolErrorCallback: func(agent.Context, adktool.Tool, map[string]any, error) (map[string]any, error) {
			return nil, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	harness := newHarnessWithOptions(t, emptyModel{}, plasmid.WithADKPlugins(native))
	if err := harness.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestHarnessConstructionObservesCancellationAtOwnedBoundaries(t *testing.T) {
	t.Run("after compiled plugin initialization", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		plugin := &compiledPlugin{name: "cancel-init", init: func(*plasmid.Harness) error {
			cancel()
			return nil
		}}
		_, err := plasmid.New(ctx, publicHarnessOptions(t, plasmid.WithPlugins(plugin))...)
		if plasmid.CodeOf(err) != plasmid.CodeConstructionFailed || !errors.Is(err, context.Canceled) {
			t.Fatalf("New cancellation error = %v", err)
		}
	})

	t.Run("after registry validation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		toolset := &cancelingToolset{name: "cancel-late", cancel: cancel}
		plugin := &compiledPlugin{name: "cancel-late", init: func(h *plasmid.Harness) error {
			return h.RegisterToolsets(toolset)
		}}
		_, err := plasmid.New(ctx, publicHarnessOptions(t, plasmid.WithPlugins(plugin))...)
		if plasmid.CodeOf(err) != plasmid.CodeConstructionFailed || !errors.Is(err, context.Canceled) {
			t.Fatalf("New late cancellation error = %v; toolset name calls = %d", err, toolset.calls.Load())
		}
	})
}

func TestHarnessCloseJoinsPublicPluginFailures(t *testing.T) {
	nativeFailure := errors.New("native close failed")
	compiledFailure := errors.New("compiled close failed")
	native, err := adkplugin.New(adkplugin.Config{
		Name:      "failing-native-close",
		CloseFunc: func() error { return nativeFailure },
	})
	if err != nil {
		t.Fatal(err)
	}
	compiled := &closeNamePanicPlugin{name: "failing-compiled-close", closeErr: compiledFailure}
	harness := newHarnessWithOptions(t, emptyModel{}, plasmid.WithPlugins(compiled), plasmid.WithADKPlugins(native))
	closeErr := harness.Close()
	if plasmid.CodeOf(closeErr) != plasmid.CodeCloseFailed || !errors.Is(closeErr, nativeFailure) || !errors.Is(closeErr, compiledFailure) {
		t.Fatalf("Close error = %v", closeErr)
	}
	if second := harness.Close(); !errors.Is(second, nativeFailure) || !errors.Is(second, compiledFailure) {
		t.Fatalf("second Close error = %v", second)
	}
}

func TestHarnessContainsNativeCloseAndNormalAfterRunCallbacks(t *testing.T) {
	afterRuns := atomic.Int32{}
	native, err := adkplugin.New(adkplugin.Config{
		Name:             "after-run-and-close-panic",
		AfterRunCallback: func(agent.InvocationContext) { afterRuns.Add(1) },
		CloseFunc:        func() error { panic("native close panic") },
	})
	if err != nil {
		t.Fatal(err)
	}
	harness := newHarnessWithOptions(t, finalOnlyModel{}, plasmid.WithADKPlugins(native))
	sessionID, err := harness.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if answer, err := harness.Ask(t.Context(), sessionID, "run"); err != nil || answer != "final" {
		t.Fatalf("Ask = %q, %v", answer, err)
	}
	if afterRuns.Load() != 1 {
		t.Fatalf("after-run callbacks = %d", afterRuns.Load())
	}
	if err := harness.Close(); plasmid.CodeOf(err) != plasmid.CodeCloseFailed {
		t.Fatalf("Close error = %v", err)
	}
}

func TestHarnessRejectsPluginWhoseNameBecomesInvalidDuringInitialization(t *testing.T) {
	plugin := &changingNamePlugin{name: "changing-name", panicAt: 3}
	_, err := plasmid.New(t.Context(), publicHarnessOptions(t, plasmid.WithPlugins(plugin))...)
	if plasmid.CodeOf(err) != plasmid.CodeConstructionFailed || !strings.Contains(err.Error(), "Name panicked") {
		t.Fatalf("New changing-name error = %v; name calls = %d", err, plugin.calls.Load())
	}
}

func TestHarnessUsesTrustedWorkspaceAndRejectsInvalidOperations(t *testing.T) {
	workingDir := t.TempDir()
	harness, err := plasmid.New(t.Context(),
		plasmid.WithModel(emptyModel{}),
		plasmid.WithWorkingDir(workingDir),
		plasmid.WithSessionDir(filepath.Join(t.TempDir(), "sessions")),
		plasmid.WithForeignResolution(plasmid.ForeignResolution{TrustedRoots: []string{workingDir}}),
		plasmid.WithLSP(plasmid.LSPOff),
	)
	if err != nil {
		t.Fatal(err)
	}
	var nilContext context.Context
	if _, err := harness.NewSession(nilContext); plasmid.CodeOf(err) != plasmid.CodeInvalidArgument {
		t.Fatalf("NewSession(nil) error = %v", err)
	}
	if err := harness.ResumeSession(t.Context(), ""); plasmid.CodeOf(err) != plasmid.CodeInvalidArgument {
		t.Fatalf("ResumeSession(empty) error = %v", err)
	}
	if err := templateRunError(harness.Run(nilContext, "session", "prompt")); plasmid.CodeOf(err) != plasmid.CodeInvalidArgument {
		t.Fatalf("Run(nil) error = %v", err)
	}
	if err := harness.ResumeSession(t.Context(), "missing"); plasmid.CodeOf(err) != plasmid.CodeUnknownSession {
		t.Fatalf("ResumeSession(missing) error = %v", err)
	}
	cancelledContext, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := harness.NewSession(cancelledContext); !errors.Is(err, context.Canceled) {
		t.Fatalf("NewSession(cancelled) error = %v", err)
	}
	if err := harness.Close(); err != nil {
		t.Fatal(err)
	}
	if err := templateRunError(harness.Run(t.Context(), "session", "prompt")); plasmid.CodeOf(err) != plasmid.CodeClosed {
		t.Fatalf("Run(closed) error = %v", err)
	}
	if err := templateRunError(harness.RunTemplate(t.Context(), "session", "template", "")); plasmid.CodeOf(err) != plasmid.CodeClosed {
		t.Fatalf("RunTemplate(closed) error = %v", err)
	}
}

func TestHarnessResumeRejectsBusySession(t *testing.T) {
	blocking := newBlockingModel()
	harness := newHarness(t, blocking)
	sessionID, err := harness.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		for event, runErr := range harness.Run(ctx, sessionID, "block") {
			_, _ = event, runErr
		}
	}()
	blocking.waitStarted(t)
	if err := harness.ResumeSession(t.Context(), sessionID); plasmid.CodeOf(err) != plasmid.CodeSessionBusy {
		t.Fatalf("ResumeSession(busy) error = %v", err)
	}
	cancel()
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("busy session run did not stop")
	}
	if err := harness.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestHarnessExposesGuardedNativeToolSurfaceToModels(t *testing.T) {
	captured := inspectGuardedNativeToolSurface(t)
	assertPublicToolCanBeRewrapped(t, captured["surface_function"], true)
	assertPublicToolCanBeRewrapped(t, captured["surface_stream"], false)
	assertConfirmedStreamIsRejected(t, captured["surface_stream"])
}

func inspectGuardedNativeToolSurface(t *testing.T) map[string]adktool.Tool {
	t.Helper()
	function, err := functiontool.New[map[string]any, map[string]any](functiontool.Config{
		Name: "surface_function", Description: "function surface",
		InputSchema: &jsonschema.Schema{Type: "object"}, OutputSchema: &jsonschema.Schema{Type: "object"},
	}, func(agent.Context, map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := functiontool.NewStreaming[publicStreamArguments](functiontool.Config{
		Name: "surface_stream", Description: "stream surface",
	}, func(agent.Context, publicStreamArguments) iter.Seq2[string, error] {
		return func(yield func(string, error) bool) { yield("chunk", nil) }
	})
	if err != nil {
		t.Fatal(err)
	}
	model := &toolSurfaceModel{}
	harness := newHarnessWithOptions(t, model,
		plasmid.WithTools(
			deferredPublicFunctionTool{publicNativeFunctionTool: function.(publicNativeFunctionTool)},
			deferredPublicStreamingTool{publicNativeStreamingTool: stream.(publicNativeStreamingTool)},
		),
	)
	sessionID, err := harness.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if answer, err := harness.Ask(t.Context(), sessionID, "inspect"); err != nil || answer != "inspected" {
		t.Fatalf("Ask = %q, %v", answer, err)
	}
	if !model.declared["surface_function"] || !model.declared["surface_stream"] {
		t.Fatalf("declarations = %#v", model.declared)
	}
	if !model.deferred["surface_function"] || !model.deferred["surface_stream"] {
		t.Fatalf("deferred response flags = %#v", model.deferred)
	}
	if err := harness.Close(); err != nil {
		t.Fatal(err)
	}
	return model.captured
}

func assertPublicToolCanBeRewrapped(t *testing.T, current adktool.Tool, confirmation bool) {
	t.Helper()
	functionModel := &toolSurfaceModel{}
	functionHarness := newHarnessWithOptions(t, functionModel,
		plasmid.WithTools(current),
		plasmid.WithToolConfirmation(confirmation),
	)
	functionSession, err := functionHarness.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := functionHarness.Ask(t.Context(), functionSession, "rewrap"); err != nil {
		t.Fatal(err)
	}
	if err := functionHarness.Close(); err != nil {
		t.Fatal(err)
	}
}

func assertConfirmedStreamIsRejected(t *testing.T, current adktool.Tool) {
	t.Helper()
	confirmedStream := newHarnessWithOptions(t, emptyModel{},
		plasmid.WithTools(current),
		plasmid.WithToolConfirmation(true),
	)
	confirmedSession, err := confirmedStream.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := confirmedStream.Ask(t.Context(), confirmedSession, "reject"); err == nil || !strings.Contains(err.Error(), "streaming") {
		t.Fatalf("confirmed streaming Ask error = %v", err)
	}
	if err := confirmedStream.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestHarnessGuardedNativeToolsRejectInvalidArgumentShapes(t *testing.T) {
	function, err := functiontool.New[map[string]any, map[string]any](functiontool.Config{
		Name: "invalid_function_arguments", Description: "function argument guard",
		InputSchema: &jsonschema.Schema{Type: "object"}, OutputSchema: &jsonschema.Schema{Type: "object"},
	}, func(agent.Context, map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := functiontool.NewStreaming[publicStreamArguments](functiontool.Config{
		Name: "invalid_stream_arguments", Description: "stream argument guard",
	}, func(agent.Context, publicStreamArguments) iter.Seq2[string, error] {
		return func(yield func(string, error) bool) { yield("chunk", nil) }
	})
	if err != nil {
		t.Fatal(err)
	}
	var invoked atomic.Int32
	native, err := adkplugin.New(adkplugin.Config{
		Name: "invoke-invalid-tool-arguments",
		BeforeModelCallback: func(ctx agent.Context, request *model.LLMRequest) (*model.LLMResponse, error) {
			if current, ok := request.Tools[function.Name()].(publicNativeFunctionTool); ok {
				_, _ = current.Run(ctx, "not-an-object")
				_, _ = current.Run(canceledAgentContext{Context: ctx}, map[string]any{})
				invoked.Add(2)
			}
			if current, ok := request.Tools[stream.Name()].(publicNativeStreamingTool); ok {
				for chunk, streamErr := range current.RunStream(ctx, "not-an-object") {
					_, _ = chunk, streamErr
				}
				invoked.Add(1)
			}
			return nil, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	harness := newHarnessWithOptions(t, finalOnlyModel{},
		plasmid.WithTools(function, stream),
		plasmid.WithADKPlugins(native),
	)
	defer closeTestResource(t, harness)
	sessionID, err := harness.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if answer, err := harness.Ask(t.Context(), sessionID, "invoke"); err != nil || answer != "final" {
		t.Fatalf("Ask = %q, %v", answer, err)
	}
	if invoked.Load() != 3 {
		t.Fatalf("guarded tools invoked = %d, want 3", invoked.Load())
	}
}

func TestHarnessConstructsWithoutHostShell(t *testing.T) {
	t.Setenv("PATH", "")
	model := &toolSurfaceModel{}
	harness := newHarnessWithOptions(t, model)
	defer closeTestResource(t, harness)
	sessionID, err := harness.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if answer, err := harness.Ask(t.Context(), sessionID, "inspect"); err != nil || answer != "inspected" {
		t.Fatalf("Ask = %q, %v", answer, err)
	}
	if model.captured["bash"] != nil {
		t.Fatal("bash tool was exposed without a host shell")
	}
}

func TestHarnessReportsUnsupportedOpaquePublicTools(t *testing.T) {
	model := &toolSurfaceModel{}
	harness := newHarnessWithOptions(t, model, plasmid.WithTools(namedTool("opaque")))
	defer closeTestResource(t, harness)
	sessionID, err := harness.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := harness.Ask(t.Context(), sessionID, "inspect"); err == nil || !strings.Contains(err.Error(), "does not implement RequestProcessor") {
		t.Fatalf("Ask error = %v", err)
	}
}

func TestHarnessPropagatesPublicToolsetFailures(t *testing.T) {
	toolFailure := errors.New("tool discovery failed")
	processorFailure := errors.New("tool request processing failed")
	tests := []struct {
		name    string
		toolset adktool.Toolset
		want    error
	}{
		{name: "tool discovery", toolset: publicToolset{name: "discovery", toolsErr: toolFailure}, want: toolFailure},
		{name: "request processing", toolset: &failingProcessorToolset{name: "processor", err: processorFailure}, want: processorFailure},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plugin := &compiledPlugin{name: test.name, init: func(h *plasmid.Harness) error {
				return h.RegisterToolsets(test.toolset)
			}}
			harness := newHarnessWithOptions(t, emptyModel{}, plasmid.WithPlugins(plugin))
			defer closeTestResource(t, harness)
			sessionID, err := harness.NewSession(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := harness.Ask(t.Context(), sessionID, "fail"); !errors.Is(err, test.want) {
				t.Fatalf("Ask error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestHarnessPropagatesPublicToolRequestProcessorFailures(t *testing.T) {
	base, err := functiontool.New[map[string]any, map[string]any](functiontool.Config{
		Name: "processor_failure", Description: "processor failure",
		InputSchema: &jsonschema.Schema{Type: "object"}, OutputSchema: &jsonschema.Schema{Type: "object"},
	}, func(agent.Context, map[string]any) (map[string]any, error) { return nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	processorFailure := errors.New("source processor failed")
	stream, err := functiontool.NewStreaming[publicStreamArguments](functiontool.Config{
		Name: "processor_failure", Description: "processor streaming replacement",
	}, func(agent.Context, publicStreamArguments) iter.Seq2[string, error] {
		return func(yield func(string, error) bool) { yield("chunk", nil) }
	})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name         string
		mode         processorFailureMode
		want         string
		confirmation bool
		packed       adktool.Tool
	}{
		{name: "returned error", mode: processorReturnsError, want: processorFailure.Error()},
		{name: "packed non-tool", mode: processorPacksNonTool, want: "packed a non-tool value"},
		{name: "packed unsupported streaming tool", mode: processorPacksTool, want: "does not support native confirmation", confirmation: true, packed: stream},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tool := &publicProcessingFunctionTool{
				publicNativeFunctionTool: base.(publicNativeFunctionTool),
				mode:                     test.mode,
				err:                      processorFailure,
				packed:                   test.packed,
			}
			harness := newHarnessWithOptions(t, emptyModel{}, plasmid.WithTools(tool), plasmid.WithToolConfirmation(test.confirmation))
			defer closeTestResource(t, harness)
			sessionID, err := harness.NewSession(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := harness.Ask(t.Context(), sessionID, "fail"); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Ask error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestHarnessAcceptsPublicToolProcessorWithoutPackedReplacement(t *testing.T) {
	base, err := functiontool.New[map[string]any, map[string]any](functiontool.Config{
		Name: "processor_no_replacement", Description: "processor leaves tool packing to ADK",
		InputSchema: &jsonschema.Schema{Type: "object"}, OutputSchema: &jsonschema.Schema{Type: "object"},
	}, func(agent.Context, map[string]any) (map[string]any, error) { return nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	tool := &publicProcessingFunctionTool{
		publicNativeFunctionTool: base.(publicNativeFunctionTool),
		mode:                     processorPacksNothing,
	}
	harness := newHarnessWithOptions(t, finalOnlyModel{}, plasmid.WithTools(tool))
	defer closeTestResource(t, harness)
	sessionID, err := harness.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if answer, err := harness.Ask(t.Context(), sessionID, "run"); err != nil || answer != "final" {
		t.Fatalf("Ask = %q, %v", answer, err)
	}
}

func TestHarnessAcceptsPublicToolsetThatPrepacksItsTool(t *testing.T) {
	base, err := functiontool.New[map[string]any, map[string]any](functiontool.Config{
		Name: "prepacked", Description: "prepacked tool",
		InputSchema: &jsonschema.Schema{Type: "object"}, OutputSchema: &jsonschema.Schema{Type: "object"},
	}, func(agent.Context, map[string]any) (map[string]any, error) { return nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	toolset := &prepackingToolset{name: "prepacking", value: base}
	plugin := &compiledPlugin{name: "prepacking", init: func(h *plasmid.Harness) error {
		return h.RegisterToolsets(toolset)
	}}
	harness := newHarnessWithOptions(t, finalOnlyModel{}, plasmid.WithPlugins(plugin))
	defer closeTestResource(t, harness)
	sessionID, err := harness.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if answer, err := harness.Ask(t.Context(), sessionID, "run"); err != nil || answer != "final" {
		t.Fatalf("Ask = %q, %v", answer, err)
	}
}

func TestHarnessRejectsStreamingReplacementUnderGlobalConfirmation(t *testing.T) {
	function, err := functiontool.New[map[string]any, map[string]any](functiontool.Config{
		Name: "replacement", Description: "function before replacement",
		InputSchema: &jsonschema.Schema{Type: "object"}, OutputSchema: &jsonschema.Schema{Type: "object"},
	}, func(agent.Context, map[string]any) (map[string]any, error) { return nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	stream, err := functiontool.NewStreaming[publicStreamArguments](functiontool.Config{
		Name: "replacement", Description: "streaming replacement",
	}, func(agent.Context, publicStreamArguments) iter.Seq2[string, error] {
		return func(yield func(string, error) bool) { yield("chunk", nil) }
	})
	if err != nil {
		t.Fatal(err)
	}
	toolset := &replacingToolset{name: "replacement", exposed: function, packed: stream}
	plugin := &compiledPlugin{name: "replacement", init: func(h *plasmid.Harness) error {
		return h.RegisterToolsets(toolset)
	}}
	harness := newHarnessWithOptions(t, emptyModel{}, plasmid.WithPlugins(plugin), plasmid.WithToolConfirmation(true))
	defer closeTestResource(t, harness)
	sessionID, err := harness.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := harness.Ask(t.Context(), sessionID, "run"); err == nil || !strings.Contains(err.Error(), "does not support native confirmation") {
		t.Fatalf("Ask error = %v", err)
	}
}

func publicHarnessOptions(t *testing.T, extra ...plasmid.Option) []plasmid.Option {
	t.Helper()
	options := []plasmid.Option{
		plasmid.WithModel(emptyModel{}),
		plasmid.WithWorkingDir(t.TempDir()),
		plasmid.WithSessionDir(filepath.Join(t.TempDir(), "sessions")),
		plasmid.WithLSP(plasmid.LSPOff),
	}
	return append(options, extra...)
}

func assertHarnessConstructionCode(t *testing.T, want plasmid.ErrorCode, options ...plasmid.Option) {
	t.Helper()
	harness, err := plasmid.New(t.Context(), options...)
	if harness != nil {
		_ = harness.Close()
	}
	if got := plasmid.CodeOf(err); got != want {
		t.Fatalf("construction code = %q, want %q; error = %v", got, want, err)
	}
}

func registrationOption(name string, register func(*plasmid.Harness) error) plasmid.Option {
	return plasmid.WithPlugins(&compiledPlugin{name: name, init: register})
}

type panicNamePlugin struct{}

func (panicNamePlugin) Name() string                { panic("plugin name") }
func (panicNamePlugin) Init(*plasmid.Harness) error { return nil }
func (panicNamePlugin) Close() error                { return nil }

type pointerTool struct{}

func (*pointerTool) Name() string        { return "pointer" }
func (*pointerTool) Description() string { return "pointer tool" }
func (*pointerTool) IsLongRunning() bool { return false }

type panicNameTool struct{}

func (panicNameTool) Name() string        { panic("tool name") }
func (panicNameTool) Description() string { return "panic tool" }
func (panicNameTool) IsLongRunning() bool { return false }

type publicToolset struct {
	name      string
	panicName bool
	toolsErr  error
}

func (s publicToolset) Name() string {
	if s.panicName {
		panic("toolset name")
	}
	return s.name
}

func (s publicToolset) Tools(agent.ReadonlyContext) ([]adktool.Tool, error) { return nil, s.toolsErr }

type cancelingToolset struct {
	name   string
	cancel context.CancelFunc
	calls  atomic.Int32
}

func (s *cancelingToolset) Name() string {
	if s.calls.Add(1) == 2 {
		s.cancel()
	}
	return s.name
}

func (*cancelingToolset) Tools(agent.ReadonlyContext) ([]adktool.Tool, error) { return nil, nil }

type closeNamePanicPlugin struct {
	name     string
	closeErr error
	panicNow atomic.Bool
}

func (p *closeNamePanicPlugin) Name() string {
	if p.panicNow.Load() {
		panic("late plugin name")
	}
	return p.name
}

func (p *closeNamePanicPlugin) Init(*plasmid.Harness) error {
	p.panicNow.Store(true)
	return nil
}

func (p *closeNamePanicPlugin) Close() error { return p.closeErr }

type changingNamePlugin struct {
	name    string
	panicAt int32
	calls   atomic.Int32
}

func (p *changingNamePlugin) Name() string {
	if p.calls.Add(1) == p.panicAt {
		panic("changing plugin name")
	}
	return p.name
}

func (*changingNamePlugin) Init(*plasmid.Harness) error { return nil }
func (*changingNamePlugin) Close() error                { return nil }

type publicNativeFunctionTool interface {
	adktool.Tool
	Declaration() *genai.FunctionDeclaration
	Run(agent.Context, any) (map[string]any, error)
}

type publicNativeStreamingTool interface {
	adktool.Tool
	Declaration() *genai.FunctionDeclaration
	RunStream(agent.Context, any) iter.Seq2[string, error]
}

type canceledAgentContext struct{ agent.Context }

func (canceledAgentContext) Err() error { return context.Canceled }

type deferredPublicFunctionTool struct{ publicNativeFunctionTool }

func (deferredPublicFunctionTool) DefersResponse() bool { return true }

type deferredPublicStreamingTool struct{ publicNativeStreamingTool }

func (deferredPublicStreamingTool) DefersResponse() bool { return true }

type publicStreamArguments struct {
	Value string `json:"value"`
}

type toolSurfaceModel struct {
	declared map[string]bool
	deferred map[string]bool
	captured map[string]adktool.Tool
}

func (*toolSurfaceModel) Name() string { return "tool-surface" }

func (m *toolSurfaceModel) GenerateContent(_ context.Context, request *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		m.declared = make(map[string]bool)
		m.deferred = make(map[string]bool)
		m.captured = make(map[string]adktool.Tool)
		for name, value := range request.Tools {
			current, ok := value.(adktool.Tool)
			if !ok {
				continue
			}
			m.captured[name] = current
			if declaration, ok := value.(interface {
				Declaration() *genai.FunctionDeclaration
			}); ok {
				m.declared[name] = declaration.Declaration() != nil
			}
			if deferrer, ok := value.(interface{ DefersResponse() bool }); ok {
				m.deferred[name] = deferrer.DefersResponse()
			}
		}
		yield(&model.LLMResponse{Content: genai.NewContentFromText("inspected", genai.RoleModel)}, nil)
	}
}

type failingProcessorToolset struct {
	name string
	err  error
}

func (s *failingProcessorToolset) Name() string { return s.name }
func (*failingProcessorToolset) Tools(agent.ReadonlyContext) ([]adktool.Tool, error) {
	return nil, nil
}
func (s *failingProcessorToolset) ProcessRequest(agent.Context, *model.LLMRequest) error {
	return s.err
}

type processorFailureMode int

const (
	processorReturnsError processorFailureMode = iota
	processorPacksNonTool
	processorPacksNothing
	processorPacksTool
)

type publicProcessingFunctionTool struct {
	publicNativeFunctionTool
	mode   processorFailureMode
	err    error
	packed adktool.Tool
}

func (t *publicProcessingFunctionTool) ProcessRequest(_ agent.Context, request *model.LLMRequest) error {
	switch t.mode {
	case processorReturnsError:
		return t.err
	case processorPacksNonTool:
		request.Tools[t.Name()] = "not-a-tool"
	case processorPacksTool:
		request.Tools[t.Name()] = t.packed
	}
	return nil
}

type prepackingToolset struct {
	name  string
	value adktool.Tool
}

func (s *prepackingToolset) Name() string { return s.name }
func (s *prepackingToolset) Tools(agent.ReadonlyContext) ([]adktool.Tool, error) {
	return []adktool.Tool{s.value}, nil
}
func (s *prepackingToolset) ProcessRequest(_ agent.Context, request *model.LLMRequest) error {
	request.Tools[s.value.Name()] = s.value
	return nil
}

type replacingToolset struct {
	name    string
	exposed adktool.Tool
	packed  adktool.Tool
}

func (s *replacingToolset) Name() string { return s.name }
func (s *replacingToolset) Tools(agent.ReadonlyContext) ([]adktool.Tool, error) {
	return []adktool.Tool{s.exposed}, nil
}
func (s *replacingToolset) ProcessRequest(_ agent.Context, request *model.LLMRequest) error {
	request.Tools[s.packed.Name()] = s.packed
	return nil
}

type finalOnlyModel struct{}

func (finalOnlyModel) Name() string { return "final-only" }
func (finalOnlyModel) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{Content: genai.NewContentFromText("final", genai.RoleModel)}, nil)
	}
}

var (
	_ plasmid.Plugin  = panicNamePlugin{}
	_ adktool.Tool    = (*pointerTool)(nil)
	_ adktool.Tool    = panicNameTool{}
	_ adktool.Toolset = publicToolset{}
	_ adktool.Toolset = (*cancelingToolset)(nil)
	_ model.LLM       = finalOnlyModel{}
	_ plasmid.Plugin  = (*closeNamePanicPlugin)(nil)
	_ plasmid.Plugin  = (*changingNamePlugin)(nil)
	_ model.LLM       = (*toolSurfaceModel)(nil)
	_ adktool.Toolset = (*failingProcessorToolset)(nil)
	_ adktool.Toolset = (*prepackingToolset)(nil)
	_ adktool.Toolset = (*replacingToolset)(nil)
)
