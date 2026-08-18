package lsp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/plasmid-dev/plasmid/outputlimit"
	"github.com/plasmid-dev/plasmid/warning"
	"github.com/plasmid-dev/plasmid/workspace"
	"go.lsp.dev/protocol"
)

type enforcerTransport struct {
	mu        sync.Mutex
	done      chan struct{}
	closeOnce sync.Once
	handler   MessageHandler
	notified  []any
	publish   func(context.Context, *enforcerTransport, string, any)
}

func newEnforcerTransport(handler MessageHandler) *enforcerTransport {
	return &enforcerTransport{done: make(chan struct{}), handler: handler}
}

func (transport *enforcerTransport) Call(_ context.Context, method string, _ any, result any) error {
	if method == "initialize" {
		value := result.(*protocol.InitializeResult)
		value.Capabilities.PositionEncoding = protocol.PositionEncodingKindUTF16
	}
	return nil
}

func (transport *enforcerTransport) Notify(ctx context.Context, method string, params any) error {
	transport.mu.Lock()
	transport.notified = append(transport.notified, params)
	publish := transport.publish
	transport.mu.Unlock()
	if publish != nil {
		publish(ctx, transport, method, params)
	}
	return nil
}

func (transport *enforcerTransport) Done() <-chan struct{} { return transport.done }

func (transport *enforcerTransport) Close() error {
	transport.closeOnce.Do(func() { close(transport.done) })
	return nil
}

func (transport *enforcerTransport) publishDiagnostics(ctx context.Context, documentURI string, version int32, diagnostics []protocol.Diagnostic) {
	raw, _ := protocol.Marshal(map[string]any{
		"uri": documentURI, "version": version, "diagnostics": diagnostics,
	})
	_, _ = transport.handler(ctx, "textDocument/publishDiagnostics", raw)
}

func (transport *enforcerTransport) publishUnversionedDiagnostics(ctx context.Context, documentURI string, diagnostics []protocol.Diagnostic) {
	raw, _ := protocol.Marshal(map[string]any{"uri": documentURI, "diagnostics": diagnostics})
	_, _ = transport.handler(ctx, "textDocument/publishDiagnostics", raw)
}

func TestEnforcerDecoratesMatchingWriteAfterCurrentDiagnostics(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "main.go")
	content := []byte("package main\n")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}

	var transport *enforcerTransport
	manager, err := NewManager(t.Context(), DefaultRegistry(), ManagerOptions{
		Warnings: warning.DiscardSink{},
		LookPath: func(string) (string, error) { return "/test/gopls", nil },
		Start: func(_ context.Context, _ string, _ []string, _ string, _ int64, handler MessageHandler) (Transport, error) {
			transport = newEnforcerTransport(handler)
			transport.publish = func(ctx context.Context, transport *enforcerTransport, method string, params any) {
				if method != "textDocument/didOpen" {
					return
				}
				opened := params.(didOpenParams).TextDocument
				transport.publishDiagnostics(ctx, opened.URI, opened.Version, []protocol.Diagnostic{{
					Range:    protocol.Range{Start: protocol.Position{Line: 1, Character: 2}, End: protocol.Position{Line: 1, Character: 5}},
					Severity: protocol.DiagnosticSeverityError,
					Code:     protocol.String("E1"), Source: protocol.NewOptional("gopls"), Message: protocol.String("broken"),
				}})
			}
			return transport, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })

	bus := workspace.NewTouchBus()
	enforcer, err := NewEnforcer(EnforcerOptions{
		WorkspaceDir:  root,
		Touches:       bus,
		Registry:      DefaultRegistry(),
		Manager:       manager,
		SettleTimeout: 100 * time.Millisecond,
		Output:        outputlimit.Defaults(),
		Warnings:      warning.DiscardSink{},
		Maximum:       20,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = enforcer.Close() })

	bus.Publish(t.Context(), workspace.Touch{
		SessionID: "session", InvocationID: "write-1", Path: "main.go",
		Kind: workspace.TouchWrite, Content: content,
	})
	decoration, ok := enforcer.Await(t.Context(), "session", "write-1")
	if !ok {
		t.Fatal("Await did not return current diagnostics")
	}
	want := []Diagnostic{{
		Path: "main.go", Start: protocol.Position{Line: 1, Character: 2}, End: protocol.Position{Line: 1, Character: 5},
		Severity: protocol.DiagnosticSeverityError, Code: "E1", Source: "gopls", Message: "broken",
	}}
	if !reflect.DeepEqual(decoration.Diagnostics, want) {
		t.Fatalf("diagnostics = %#v, want %#v", decoration.Diagnostics, want)
	}
	if decoration.Text != "main.go:2:3: error E1 (gopls): broken" {
		t.Fatalf("diagnostics text = %q", decoration.Text)
	}
	if status := enforcer.Status(); status != "LSP: gopls" {
		t.Fatalf("status = %q", status)
	}
	transport.mu.Lock()
	opened := transport.notified[1].(didOpenParams).TextDocument
	transport.mu.Unlock()
	if opened.LanguageID != "go" || opened.Version != 1 || opened.Text != string(content) {
		t.Fatalf("didOpen = %#v", opened)
	}
}

func TestEnforcerRejectsStalePublicationAndAcceptsCurrentClear(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "main.go")
	if err := os.WriteFile(path, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	manager, err := NewManager(t.Context(), DefaultRegistry(), ManagerOptions{
		Warnings: warning.DiscardSink{},
		LookPath: func(string) (string, error) { return "/test/gopls", nil },
		Start: func(_ context.Context, _ string, _ []string, _ string, _ int64, handler MessageHandler) (Transport, error) {
			transport := newEnforcerTransport(handler)
			transport.publish = func(ctx context.Context, transport *enforcerTransport, method string, params any) {
				switch method {
				case "textDocument/didOpen":
					opened := params.(didOpenParams).TextDocument
					transport.publishDiagnostics(ctx, opened.URI, opened.Version, diagnosticValues("first"))
				case "textDocument/didChange":
					changed := params.(didChangeParams).TextDocument
					transport.publishDiagnostics(ctx, changed.URI, changed.Version-1, diagnosticValues("stale"))
					transport.publishUnversionedDiagnostics(ctx, changed.URI, diagnosticValues("unversioned stale"))
					go func() {
						time.Sleep(10 * time.Millisecond)
						transport.publishDiagnostics(context.Background(), changed.URI, changed.Version, nil)
					}()
				}
			}
			return transport, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	bus := workspace.NewTouchBus()
	enforcer, err := NewEnforcer(EnforcerOptions{
		WorkspaceDir: root, Touches: bus, Registry: DefaultRegistry(), Manager: manager,
		SettleTimeout: 100 * time.Millisecond, Output: outputlimit.Defaults(), Warnings: warning.DiscardSink{}, Maximum: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = enforcer.Close() })

	bus.Publish(t.Context(), workspace.Touch{SessionID: "session", InvocationID: "write-1", Path: "main.go", Kind: workspace.TouchWrite, Content: []byte("package main\n")})
	first, ok := enforcer.Await(t.Context(), "session", "write-1")
	if !ok || len(first.Diagnostics) != 1 || first.Diagnostics[0].Message != "first" {
		t.Fatalf("first decoration = %#v, %t", first, ok)
	}

	bus.Publish(t.Context(), workspace.Touch{SessionID: "session", InvocationID: "edit-2", Path: "main.go", Kind: workspace.TouchEdit, Content: []byte("package changed\n")})
	cleared, ok := enforcer.Await(t.Context(), "session", "edit-2")
	if !ok || len(cleared.Diagnostics) != 0 || cleared.Text != "" {
		t.Fatalf("clear decoration = %#v, %t", cleared, ok)
	}
}

func TestEnforcerSettleTimeoutWarnsButCallerCancellationDoesNot(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sink := &warning.SliceSink{}
	manager, err := NewManager(t.Context(), DefaultRegistry(), ManagerOptions{
		Warnings: sink,
		LookPath: func(string) (string, error) { return "/test/gopls", nil },
		Start: func(_ context.Context, _ string, _ []string, _ string, _ int64, handler MessageHandler) (Transport, error) {
			return newEnforcerTransport(handler), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	bus := workspace.NewTouchBus()
	enforcer, err := NewEnforcer(EnforcerOptions{
		WorkspaceDir: root, Touches: bus, Registry: DefaultRegistry(), Manager: manager,
		SettleTimeout: 10 * time.Millisecond, Output: outputlimit.Defaults(), Warnings: sink,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = enforcer.Close() })

	bus.Publish(t.Context(), workspace.Touch{SessionID: "session", InvocationID: "timeout", Path: "main.go", Kind: workspace.TouchWrite, Content: []byte("package main\n")})
	if decoration, ok := enforcer.Await(t.Context(), "session", "timeout"); ok {
		t.Fatalf("timeout decoration = %#v", decoration)
	}
	warnings := sink.Warnings()
	if len(warnings) != 1 || warnings[0].Code != warning.WarnLSPDiagnosticsUnsettled {
		t.Fatalf("timeout warnings = %#v", warnings)
	}
	bus.Publish(t.Context(), workspace.Touch{SessionID: "session", InvocationID: "timeout-again", Path: "main.go", Kind: workspace.TouchEdit, Content: []byte("package timeout\n")})
	if decoration, ok := enforcer.Await(t.Context(), "session", "timeout-again"); ok {
		t.Fatalf("second timeout decoration = %#v", decoration)
	}
	if warnings := sink.Warnings(); len(warnings) != 1 {
		t.Fatalf("duplicate timeout warnings = %#v", warnings)
	}

	bus.Publish(t.Context(), workspace.Touch{SessionID: "session", InvocationID: "cancel", Path: "main.go", Kind: workspace.TouchEdit, Content: []byte("package changed\n")})
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if decoration, ok := enforcer.Await(canceled, "session", "cancel"); ok {
		t.Fatalf("cancel decoration = %#v", decoration)
	}
	if warnings := sink.Warnings(); len(warnings) != 1 {
		t.Fatalf("cancellation warnings = %#v", warnings)
	}
}

func TestEnforcerFiltersTouchesAndCorrelatesConcurrentInvocations(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var starts int
	manager := newCorrelatingManager(t, &starts)
	bus := workspace.NewTouchBus()
	enforcer, err := NewEnforcer(EnforcerOptions{
		WorkspaceDir: root, Touches: bus, Registry: DefaultRegistry(), Manager: manager,
		SettleTimeout: 100 * time.Millisecond, Output: outputlimit.Defaults(), Warnings: warning.DiscardSink{},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = enforcer.Close() })

	publishFilteredTouches(t, bus)
	if starts != 0 {
		t.Fatalf("filtered touches started %d servers", starts)
	}

	const invocations = 24
	publishConcurrentEnforcerTouches(t, bus, root, invocations)
	if starts != 1 {
		t.Fatalf("concurrent touches started %d servers, want 1", starts)
	}
	awaitConcurrentEnforcerTouches(t, enforcer, invocations)
	if _, ok := enforcer.Await(t.Context(), "session", "write-00"); ok {
		t.Fatal("consumed invocation receipt remained available")
	}
}

func newCorrelatingManager(t *testing.T, starts *int) *Manager {
	t.Helper()
	manager, err := NewManager(t.Context(), DefaultRegistry(), ManagerOptions{
		Warnings: warning.DiscardSink{},
		LookPath: func(string) (string, error) { return "/test/gopls", nil },
		Start: func(_ context.Context, _ string, _ []string, _ string, _ int64, handler MessageHandler) (Transport, error) {
			*starts++
			transport := newEnforcerTransport(handler)
			transport.publish = func(ctx context.Context, transport *enforcerTransport, method string, params any) {
				if method != "textDocument/didOpen" {
					return
				}
				opened := params.(didOpenParams).TextDocument
				transport.publishDiagnostics(ctx, opened.URI, opened.Version, diagnosticValues(filepath.Base(opened.URI)))
			}
			return transport, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	return manager
}

func publishFilteredTouches(t *testing.T, bus *workspace.TouchBus) {
	t.Helper()
	for _, touch := range []workspace.Touch{
		{SessionID: "session", InvocationID: "read", Path: "ignored.go", Kind: workspace.TouchRead},
		{SessionID: "session", Path: "ignored.go", Kind: workspace.TouchWrite},
		{SessionID: "session", InvocationID: "text", Path: "ignored.txt", Kind: workspace.TouchEdit},
	} {
		bus.Publish(t.Context(), touch)
	}
}

func publishConcurrentEnforcerTouches(t *testing.T, bus *workspace.TouchBus, root string, invocations int) {
	t.Helper()
	var publishGroup sync.WaitGroup
	for index := range invocations {
		index := index
		publishGroup.Go(func() {
			path := fmt.Sprintf("file-%02d.go", index)
			content := []byte("package fixture\n")
			if err := os.WriteFile(filepath.Join(root, path), content, 0o644); err != nil {
				t.Errorf("write %s: %v", path, err)
				return
			}
			bus.Publish(t.Context(), workspace.Touch{
				SessionID: "session", InvocationID: fmt.Sprintf("write-%02d", index),
				Path: path, Kind: workspace.TouchWrite, Content: content,
			})
		})
	}
	publishGroup.Wait()
}

func awaitConcurrentEnforcerTouches(t *testing.T, enforcer *Enforcer, invocations int) {
	t.Helper()
	var awaitGroup sync.WaitGroup
	for index := range invocations {
		index := index
		awaitGroup.Go(func() {
			invocationID := fmt.Sprintf("write-%02d", index)
			decoration, ok := enforcer.Await(t.Context(), "session", invocationID)
			wantPath := fmt.Sprintf("file-%02d.go", index)
			if !ok || len(decoration.Diagnostics) != 1 || decoration.Diagnostics[0].Path != wantPath {
				t.Errorf("%s decoration = %#v, %t", invocationID, decoration, ok)
			}
		})
	}
	awaitGroup.Wait()
}

func diagnosticValues(message string) []protocol.Diagnostic {
	return []protocol.Diagnostic{{
		Range:    protocol.Range{Start: protocol.Position{}, End: protocol.Position{Character: 1}},
		Severity: protocol.DiagnosticSeverityError, Message: protocol.String(message),
	}}
}
