package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RandomCodeSpace/plasmid/warning"
	"go.lsp.dev/protocol"
)

const managerTestServerPath = "/bin/gopls"

type fakeTransport struct {
	mu            sync.Mutex
	done          chan struct{}
	closeOnce     sync.Once
	call          func(context.Context, string, any, any) error
	notifications []string
	parameters    []any
}

func newFakeTransport() *fakeTransport {
	transport := &fakeTransport{done: make(chan struct{})}
	transport.call = func(_ context.Context, method string, _ any, result any) error {
		if method == "initialize" {
			value := result.(*protocol.InitializeResult)
			value.Capabilities.PositionEncoding = protocol.PositionEncodingKindUTF8
		}
		return nil
	}
	return transport
}

func (transport *fakeTransport) Call(ctx context.Context, method string, params, result any) error {
	transport.mu.Lock()
	call := transport.call
	transport.mu.Unlock()
	return call(ctx, method, params, result)
}

func (transport *fakeTransport) Notify(_ context.Context, method string, params any) error {
	transport.mu.Lock()
	transport.notifications = append(transport.notifications, method)
	transport.parameters = append(transport.parameters, params)
	transport.mu.Unlock()
	return nil
}

func (transport *fakeTransport) Done() <-chan struct{} { return transport.done }

func (transport *fakeTransport) Close() error {
	transport.closeOnce.Do(func() { close(transport.done) })
	return nil
}

func TestManagerIsLazyAndDeduplicatesConcurrentStart(t *testing.T) {
	var lookups atomic.Int32
	var starts atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	transport := newFakeTransport()
	root := t.TempDir()
	manager, err := NewManager(context.Background(), DefaultRegistry(), ManagerOptions{
		Warnings: warning.DiscardSink{},
		LookPath: func(command string) (string, error) {
			lookups.Add(1)
			return "/bin/" + command, nil
		},
		Start: func(_ context.Context, _ string, _ []string, _ string, _ int64, _ MessageHandler) (Transport, error) {
			starts.Add(1)
			close(started)
			<-release
			return transport, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = manager.Close() }()
	if lookups.Load() != 0 || starts.Load() != 0 {
		t.Fatal("manager performed eager detection or spawn")
	}

	const callers = 16
	errorsSeen := make(chan error, callers)
	var wait sync.WaitGroup
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			client, err := manager.Start(context.Background(), "gopls", root)
			if err == nil && (client == nil || client.PositionEncoding() != protocol.PositionEncodingKindUTF8) {
				err = errors.New("missing initialized client")
			}
			errorsSeen <- err
		}()
	}
	<-started
	close(release)
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
	if lookups.Load() != 1 || starts.Load() != 1 {
		t.Fatalf("lookups = %d, starts = %d", lookups.Load(), starts.Load())
	}
}

func TestManagerMissingExecutableWarnsOnceAndNeverStarts(t *testing.T) {
	sink := &warning.SliceSink{}
	var lookups atomic.Int32
	var starts atomic.Int32
	root := t.TempDir()
	manager, err := NewManager(context.Background(), DefaultRegistry(), ManagerOptions{
		Warnings: sink,
		LookPath: func(string) (string, error) {
			lookups.Add(1)
			return "", execNotFoundError{}
		},
		Start: func(context.Context, string, []string, string, int64, MessageHandler) (Transport, error) {
			starts.Add(1)
			return nil, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = manager.Close() }()
	for range 3 {
		client, err := manager.Start(context.Background(), "gopls", root)
		if err != nil || client != nil {
			t.Fatalf("Start = %#v, %v", client, err)
		}
	}
	if lookups.Load() != 1 || starts.Load() != 0 {
		t.Fatalf("lookups = %d, starts = %d", lookups.Load(), starts.Load())
	}
	warnings := sink.Warnings()
	if len(warnings) != 1 || warnings[0].Code != warning.WarnLSPUnavailable {
		t.Fatalf("warnings = %#v", warnings)
	}
}

func TestManagerContainsStarterPanic(t *testing.T) {
	sink := &warning.SliceSink{}
	manager, err := NewManager(context.Background(), DefaultRegistry(), ManagerOptions{
		Warnings: sink,
		LookPath: func(string) (string, error) { return managerTestServerPath, nil },
		Start: func(context.Context, string, []string, string, int64, MessageHandler) (Transport, error) {
			panic("boom")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = manager.Close() }()
	client, err := manager.Start(context.Background(), "gopls", t.TempDir())
	if err != nil || client != nil {
		t.Fatalf("Start = %#v, %v", client, err)
	}
	warnings := sink.Warnings()
	if len(warnings) != 1 || warnings[0].Code != warning.WarnLSPStartFailed {
		t.Fatalf("warnings = %#v", warnings)
	}
}

func TestManagerInitializationTimeoutDegradesToNoOp(t *testing.T) {
	sink := &warning.SliceSink{}
	transport := newFakeTransport()
	transport.call = func(ctx context.Context, _ string, _ any, _ any) error {
		<-ctx.Done()
		return ctx.Err()
	}
	manager, err := NewManager(context.Background(), DefaultRegistry(), ManagerOptions{
		Warnings: sink, InitializeTimeout: 10 * time.Millisecond,
		LookPath: func(string) (string, error) { return managerTestServerPath, nil },
		Start: func(context.Context, string, []string, string, int64, MessageHandler) (Transport, error) {
			return transport, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = manager.Close() }()
	client, err := manager.Start(context.Background(), "gopls", t.TempDir())
	if err != nil || client != nil {
		t.Fatalf("Start = %#v, %v", client, err)
	}
	warnings := sink.Warnings()
	if len(warnings) != 1 || warnings[0].Code != warning.WarnLSPInitializeFailed {
		t.Fatalf("warnings = %#v", warnings)
	}
}

func TestManagerStartHonorsCallerCancellation(t *testing.T) {
	sink := &warning.SliceSink{}
	entered := make(chan struct{})
	transport := newFakeTransport()
	transport.call = func(ctx context.Context, _ string, _ any, _ any) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}
	manager, err := NewManager(context.Background(), DefaultRegistry(), ManagerOptions{
		Warnings: sink,
		LookPath: func(string) (string, error) { return managerTestServerPath, nil },
		Start: func(context.Context, string, []string, string, int64, MessageHandler) (Transport, error) {
			return transport, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = manager.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	root := t.TempDir()
	go func() {
		_, err := manager.Start(ctx, "gopls", root)
		result <- err
	}()
	<-entered
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("Start error = %v", err)
	}
	if warnings := sink.Warnings(); len(warnings) != 0 {
		t.Fatalf("warnings = %#v", warnings)
	}
}

func TestManagerDoesNotPublishDisconnectedClient(t *testing.T) {
	sink := &warning.SliceSink{}
	transport := newFakeTransport()
	_ = transport.Close()
	manager, err := NewManager(context.Background(), DefaultRegistry(), ManagerOptions{
		Warnings: sink,
		LookPath: func(string) (string, error) { return managerTestServerPath, nil },
		Start: func(context.Context, string, []string, string, int64, MessageHandler) (Transport, error) {
			return transport, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = manager.Close() }()
	client, err := manager.Start(context.Background(), "gopls", t.TempDir())
	if err != nil || client != nil {
		t.Fatalf("Start = %#v, %v", client, err)
	}
	warnings := sink.Warnings()
	if len(warnings) != 1 || warnings[0].Code != warning.WarnLSPRequestFailed {
		t.Fatalf("warnings = %#v", warnings)
	}
}

type execNotFoundError struct{}

func (execNotFoundError) Error() string { return "not found" }

func TestManagerRequestTimeoutAndFailureLimit(t *testing.T) {
	sink := &warning.SliceSink{}
	transport := newFakeTransport()
	manager, err := NewManager(context.Background(), DefaultRegistry(), ManagerOptions{
		Warnings: sink, RequestTimeout: 10 * time.Millisecond,
		LookPath: func(string) (string, error) { return managerTestServerPath, nil },
		Start: func(context.Context, string, []string, string, int64, MessageHandler) (Transport, error) {
			return transport, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = manager.Close() }()
	client, err := manager.Start(context.Background(), "gopls", t.TempDir())
	if err != nil || client == nil {
		t.Fatalf("Start = %#v, %v", client, err)
	}
	transport.mu.Lock()
	transport.call = func(ctx context.Context, _ string, _ any, _ any) error {
		<-ctx.Done()
		return ctx.Err()
	}
	transport.mu.Unlock()
	for range DefaultFailureLimit {
		if client.Call(context.Background(), "blocked", nil, new(any)) {
			t.Fatal("timed out call reported success")
		}
	}
	if got, err := manager.Start(context.Background(), "gopls", client.Root()); err != nil || got != nil {
		t.Fatalf("disabled Start = %#v, %v", got, err)
	}
	warnings := sink.Warnings()
	if len(warnings) != 1 || warnings[0].Code != warning.WarnLSPRequestFailed {
		t.Fatalf("warnings = %#v", warnings)
	}
}

func TestOnlySuccessfulOutboundCallResetsFailureCount(t *testing.T) {
	transport := newFakeTransport()
	manager, err := NewManager(context.Background(), DefaultRegistry(), ManagerOptions{
		Warnings: warning.DiscardSink{},
		LookPath: func(string) (string, error) { return managerTestServerPath, nil },
		Start: func(context.Context, string, []string, string, int64, MessageHandler) (Transport, error) {
			return transport, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = manager.Close() }()
	root := t.TempDir()
	path := filepath.Join(root, "main.go")
	if err := os.WriteFile(path, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	client, err := manager.Start(context.Background(), "gopls", root)
	if err != nil || client == nil {
		t.Fatalf("Start = %#v, %v", client, err)
	}
	transport.mu.Lock()
	transport.call = func(context.Context, string, any, any) error { return errors.New("request failed") }
	transport.mu.Unlock()
	if client.Call(context.Background(), "first failure", nil, nil) {
		t.Fatal("failed call reported success")
	}
	if !client.Notify(context.Background(), "successful notification", nil) {
		t.Fatal("notification failed")
	}
	uri, err := PathToFileURI(path)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{"uri": uri, "diagnostics": []any{}})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = client.handleMessage(context.Background(), "textDocument/publishDiagnostics", raw)
	for range 2 {
		if client.Call(context.Background(), "later failure", nil, nil) {
			t.Fatal("failed call reported success")
		}
	}
	if got, err := manager.Start(context.Background(), "gopls", root); err != nil || got != nil {
		t.Fatalf("disabled Start = %#v, %v", got, err)
	}
}

func TestSuccessfulOutboundCallResetsFailureCount(t *testing.T) {
	transport := newFakeTransport()
	manager, err := NewManager(context.Background(), DefaultRegistry(), ManagerOptions{
		Warnings: warning.DiscardSink{},
		LookPath: func(string) (string, error) { return managerTestServerPath, nil },
		Start: func(context.Context, string, []string, string, int64, MessageHandler) (Transport, error) {
			return transport, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = manager.Close() }()
	root := t.TempDir()
	client, err := manager.Start(context.Background(), "gopls", root)
	if err != nil || client == nil {
		t.Fatalf("Start = %#v, %v", client, err)
	}
	setCall := func(call func(context.Context, string, any, any) error) {
		transport.mu.Lock()
		transport.call = call
		transport.mu.Unlock()
	}
	setCall(func(context.Context, string, any, any) error { return errors.New("request failed") })
	for range 2 {
		client.Call(context.Background(), "failure", nil, nil)
	}
	setCall(func(context.Context, string, any, any) error { return nil })
	if !client.Call(context.Background(), "success", nil, nil) {
		t.Fatal("successful call reported failure")
	}
	setCall(func(context.Context, string, any, any) error { return errors.New("request failed") })
	for range 2 {
		client.Call(context.Background(), "failure", nil, nil)
	}
	if got, err := manager.Start(context.Background(), "gopls", root); err != nil || got != client {
		t.Fatalf("active Start = %#v, %v", got, err)
	}
}

func TestManagerCloseCancelsInFlightRequest(t *testing.T) {
	transport := newFakeTransport()
	manager, err := NewManager(context.Background(), DefaultRegistry(), ManagerOptions{
		Warnings: warning.DiscardSink{},
		LookPath: func(string) (string, error) { return managerTestServerPath, nil },
		Start: func(context.Context, string, []string, string, int64, MessageHandler) (Transport, error) {
			return transport, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	client, err := manager.Start(context.Background(), "gopls", t.TempDir())
	if err != nil || client == nil {
		t.Fatalf("Start = %#v, %v", client, err)
	}
	entered := make(chan struct{})
	transport.mu.Lock()
	transport.call = func(ctx context.Context, _ string, _ any, _ any) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}
	transport.mu.Unlock()
	result := make(chan bool, 1)
	go func() { result <- client.Call(context.Background(), "blocked", nil, nil) }()
	<-entered
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	if <-result {
		t.Fatal("request succeeded after manager close")
	}
}

func TestClientContainsTransportPanic(t *testing.T) {
	sink := &warning.SliceSink{}
	transport := newFakeTransport()
	manager, err := NewManager(context.Background(), DefaultRegistry(), ManagerOptions{
		Warnings: sink,
		LookPath: func(string) (string, error) { return managerTestServerPath, nil },
		Start: func(context.Context, string, []string, string, int64, MessageHandler) (Transport, error) {
			return transport, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = manager.Close() }()
	client, err := manager.Start(context.Background(), "gopls", t.TempDir())
	if err != nil || client == nil {
		t.Fatalf("Start = %#v, %v", client, err)
	}
	transport.mu.Lock()
	transport.call = func(context.Context, string, any, any) error { panic("boom") }
	transport.mu.Unlock()
	if client.Call(context.Background(), "panic", nil, nil) {
		t.Fatal("transport panic reported success")
	}
	if warnings := sink.Warnings(); len(warnings) != 1 || warnings[0].Code != warning.WarnLSPRequestFailed {
		t.Fatalf("warnings = %#v", warnings)
	}
}

func TestManagerCloseWaitsForInFlightStart(t *testing.T) {
	started := make(chan struct{})
	manager, err := NewManager(context.Background(), DefaultRegistry(), ManagerOptions{
		Warnings: warning.DiscardSink{}, InitializeTimeout: 100 * time.Millisecond,
		LookPath: func(string) (string, error) { return managerTestServerPath, nil },
		Start: func(ctx context.Context, _ string, _ []string, _ string, _ int64, _ MessageHandler) (Transport, error) {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	startResult := make(chan error, 1)
	go func() {
		_, err := manager.Start(context.Background(), "gopls", root)
		startResult <- err
	}()
	<-started
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-startResult; !errors.Is(err, ErrManagerClosed) {
		t.Fatalf("Start error = %v", err)
	}
}

func TestManagerProcessExitWarnsAndAllowsRetry(t *testing.T) {
	sink := &warning.SliceSink{}
	var starts atomic.Int32
	transports := make(chan *fakeTransport, 2)
	manager, err := NewManager(context.Background(), DefaultRegistry(), ManagerOptions{
		Warnings: sink,
		LookPath: func(string) (string, error) { return managerTestServerPath, nil },
		Start: func(context.Context, string, []string, string, int64, MessageHandler) (Transport, error) {
			starts.Add(1)
			transport := newFakeTransport()
			transports <- transport
			return transport, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = manager.Close() }()
	root := t.TempDir()
	client, err := manager.Start(context.Background(), "gopls", root)
	if err != nil || client == nil {
		t.Fatalf("Start = %#v, %v", client, err)
	}
	first := <-transports
	_ = first.Close()
	eventually(t, func() bool { return len(sink.Warnings()) == 1 })
	retried, err := manager.Start(context.Background(), "gopls", root)
	if err != nil || retried == nil || retried == client || starts.Load() != 2 {
		t.Fatalf("retry = %#v, %v, starts %d", retried, err, starts.Load())
	}
}

func TestClientDocumentVersions(t *testing.T) {
	transport := newFakeTransport()
	manager, err := NewManager(context.Background(), DefaultRegistry(), ManagerOptions{
		Warnings: warning.DiscardSink{},
		LookPath: func(string) (string, error) { return managerTestServerPath, nil },
		Start: func(context.Context, string, []string, string, int64, MessageHandler) (Transport, error) {
			return transport, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = manager.Close() }()
	client, err := manager.Start(context.Background(), "gopls", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !client.DidOpen(context.Background(), "main.go", "go", []byte("package main\n")) ||
		!client.DidChange(context.Background(), "main.go", []byte("package changed\n")) ||
		!client.DidClose(context.Background(), "main.go") {
		t.Fatal("document lifecycle failed")
	}
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if len(transport.notifications) != 4 || transport.notifications[0] != "initialized" || transport.notifications[1] != "textDocument/didOpen" || transport.notifications[2] != "textDocument/didChange" || transport.notifications[3] != "textDocument/didClose" {
		t.Fatalf("notifications = %#v", transport.notifications)
	}
	change := transport.parameters[2].(didChangeParams)
	if change.TextDocument.Version != 2 {
		t.Fatalf("change version = %d", change.TextDocument.Version)
	}
}

func eventually(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(time.Millisecond)
	}
}
