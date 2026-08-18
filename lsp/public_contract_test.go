package lsp_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/plasmid-dev/plasmid/lsp"
	"github.com/plasmid-dev/plasmid/outputlimit"
	"github.com/plasmid-dev/plasmid/warning"
	"github.com/plasmid-dev/plasmid/workspace"
	"github.com/sourcegraph/jsonrpc2"
	"go.lsp.dev/protocol"
)

type publicTransport struct {
	mu        sync.Mutex
	done      chan struct{}
	closeOnce sync.Once
	handler   lsp.MessageHandler
	call      func(context.Context, string, any, any) error
	notify    func(context.Context, string, any) error
	methods   []string
	params    []any
}

type panicDoneTransport struct {
	*publicTransport
}

type publicTransportSlot struct {
	transport *publicTransport
}

func (*panicDoneTransport) Done() <-chan struct{} { panic("done") }

func newPublicTransport(handler lsp.MessageHandler) *publicTransport {
	transport := &publicTransport{done: make(chan struct{}), handler: handler}
	transport.call = func(_ context.Context, method string, _ any, result any) error {
		if method == "initialize" {
			initialized := result.(*protocol.InitializeResult)
			initialized.Capabilities.PositionEncoding = protocol.PositionEncodingKindUTF8
		}
		return nil
	}
	transport.notify = func(context.Context, string, any) error { return nil }
	return transport
}

func (transport *publicTransport) Call(ctx context.Context, method string, params, result any) error {
	transport.mu.Lock()
	call := transport.call
	transport.mu.Unlock()
	return call(ctx, method, params, result)
}

func (transport *publicTransport) Notify(ctx context.Context, method string, params any) error {
	transport.mu.Lock()
	transport.methods = append(transport.methods, method)
	transport.params = append(transport.params, params)
	notify := transport.notify
	transport.mu.Unlock()
	return notify(ctx, method, params)
}

func (transport *publicTransport) Done() <-chan struct{} { return transport.done }

func (transport *publicTransport) Close() error {
	transport.closeOnce.Do(func() {
		if transport.done != nil {
			close(transport.done)
		}
	})
	return nil
}

func TestManagerAndClientPublicLifecycle(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "main.go")
	if err := os.WriteFile(file, []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sink := &warning.SliceSink{}
	var transport *publicTransport
	manager, err := lsp.NewManager(t.Context(), lsp.DefaultRegistry(), lsp.ManagerOptions{
		Warnings: sink, FailureLimit: 20,
		LookPath: func(string) (string, error) { return "/test/gopls", nil },
		Start: func(_ context.Context, _ string, _ []string, _ string, _ int64, handler lsp.MessageHandler) (lsp.Transport, error) {
			transport = newPublicTransport(handler)
			return transport, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	client, err := manager.Start(t.Context(), "gopls", root)
	if err != nil || client == nil {
		t.Fatalf("Start() = %#v, %v", client, err)
	}
	assertClientMetadata(t, manager, client, root)
	assertClientRequests(t, client)
	assertClientDocuments(t, client, transport, file)
	assertManagerDisconnect(t, manager, client, transport, root)
}

func assertClientMetadata(t *testing.T, manager *lsp.Manager, client *lsp.Client, root string) {
	t.Helper()
	server := client.Server()
	server.Extensions[0] = ".changed"
	if got := client.Server(); got.ID != "gopls" || got.Extensions[0] != ".go" {
		t.Fatalf("Server() = %#v", got)
	}
	if client.Root() != root || client.PositionEncoding() != protocol.PositionEncodingKindUTF8 {
		t.Fatalf("client root/encoding = %q/%q", client.Root(), client.PositionEncoding())
	}
	if got := manager.ActiveServers(); !reflect.DeepEqual(got, []string{"gopls"}) {
		t.Fatalf("ActiveServers() = %#v", got)
	}
}

func assertClientRequests(t *testing.T, client *lsp.Client) {
	t.Helper()
	if client.Call(nilContext(), "nil", nil, nil) || client.Notify(nilContext(), "nil", nil) {
		t.Fatal("nil context succeeded")
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if client.Call(cancelled, "cancelled", nil, nil) || client.Notify(cancelled, "cancelled", nil) {
		t.Fatal("cancelled context succeeded")
	}
	var result map[string]any
	if !client.Call(t.Context(), "ok", nil, &result) || !client.Notify(t.Context(), "ok", nil) {
		t.Fatal("healthy client call failed")
	}
}

func assertClientDocuments(t *testing.T, client *lsp.Client, transport *publicTransport, file string) {
	t.Helper()
	if !client.DidOpen(t.Context(), "main.go", "go", []byte("package main\n")) {
		t.Fatal("DidOpen failed")
	}
	if client.DidOpen(t.Context(), "main.go", "go", nil) || client.DidOpen(t.Context(), "../outside.go", "go", nil) || client.DidOpen(t.Context(), "bad.go", "go", []byte{0xff}) {
		t.Fatal("invalid DidOpen succeeded")
	}
	if client.DidChange(t.Context(), "missing.go", nil) || client.DidChange(t.Context(), "../outside.go", nil) || client.DidChange(t.Context(), "main.go", []byte{0xff}) {
		t.Fatal("invalid DidChange succeeded")
	}
	if !client.DidChange(t.Context(), file, []byte("package changed\n")) {
		t.Fatal("DidChange failed")
	}

	uri, err := lsp.PathToFileURI(file)
	if err != nil {
		t.Fatal(err)
	}
	published, _ := protocol.Marshal(map[string]any{
		"uri": uri, "version": 2, "diagnostics": []protocol.Diagnostic{{
			Range: protocol.Range{End: protocol.Position{Character: 1}}, Severity: protocol.DiagnosticSeverityWarning,
			Code: protocol.Integer(7), Source: protocol.NewOptional("gopls"), Message: protocol.String("warning\rmessage"),
		}},
	})
	if _, err := transport.handler(t.Context(), "textDocument/publishDiagnostics", published); err != nil {
		t.Fatal(err)
	}
	_, _ = transport.handler(t.Context(), "workspace/unknown", nil)
	_, _ = transport.handler(t.Context(), "textDocument/publishDiagnostics", json.RawMessage("{"))
	invalidPublished, _ := protocol.Marshal(map[string]any{"uri": "https://example.test/main.go", "diagnostics": []protocol.Diagnostic{}})
	_, _ = transport.handler(t.Context(), "textDocument/publishDiagnostics", invalidPublished)
	diagnostics := client.Diagnostics(file)
	if len(diagnostics) != 1 || diagnostics[0].Code != "7" || diagnostics[0].Message != "warning\nmessage" {
		t.Fatalf("Diagnostics() = %#v", diagnostics)
	}
	diagnostics[0].Message = "mutated"
	if got := client.Diagnostics("main.go"); len(got) != 1 || got[0].Message == "mutated" {
		t.Fatalf("Diagnostics() aliased storage: %#v", got)
	}
	if !client.DidClose(t.Context(), "main.go") || client.DidClose(t.Context(), "main.go") || client.DidClose(t.Context(), "../outside.go") {
		t.Fatal("DidClose lifecycle mismatch")
	}
	if got := client.Diagnostics("main.go"); len(got) != 0 {
		t.Fatalf("closed diagnostics = %#v", got)
	}
}

func assertManagerDisconnect(t *testing.T, manager *lsp.Manager, client *lsp.Client, transport *publicTransport, root string) {
	t.Helper()
	if err := transport.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for len(manager.ActiveServers()) != 0 && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if len(manager.ActiveServers()) != 0 || !client.Call(t.Context(), "stale", nil, nil) {
		t.Fatal("disconnected client did not remain safely callable")
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Start(t.Context(), "gopls", root); !errors.Is(err, lsp.ErrManagerClosed) {
		t.Fatalf("Start after Close error = %v", err)
	}
}

func TestClientContainsTransportFailuresAndRollsBackDocuments(t *testing.T) {
	root := t.TempDir()
	sink := &warning.SliceSink{}
	transport := newPublicTransport(nil)
	manager, err := lsp.NewManager(t.Context(), lsp.DefaultRegistry(), lsp.ManagerOptions{
		Warnings: sink, FailureLimit: 20,
		LookPath: func(string) (string, error) { return "/test/gopls", nil },
		Start: func(context.Context, string, []string, string, int64, lsp.MessageHandler) (lsp.Transport, error) {
			return transport, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	client, err := manager.Start(t.Context(), "gopls", root)
	if err != nil {
		t.Fatal(err)
	}
	transport.mu.Lock()
	transport.call = func(context.Context, string, any, any) error { return errors.New("call failed") }
	transport.notify = func(context.Context, string, any) error { return errors.New("notify failed") }
	transport.mu.Unlock()
	if client.Call(t.Context(), "fail", nil, nil) || client.Notify(t.Context(), "fail", nil) {
		t.Fatal("transport error succeeded")
	}
	if client.DidOpen(t.Context(), "main.go", "go", nil) {
		t.Fatal("failed DidOpen succeeded")
	}
	transport.mu.Lock()
	transport.notify = func(context.Context, string, any) error { return nil }
	transport.mu.Unlock()
	if !client.DidOpen(t.Context(), "main.go", "go", nil) {
		t.Fatal("DidOpen did not recover after rollback")
	}
	transport.mu.Lock()
	transport.notify = func(context.Context, string, any) error { panic("notify panic") }
	transport.call = func(context.Context, string, any, any) error { panic("call panic") }
	transport.mu.Unlock()
	if client.Call(t.Context(), "panic", nil, nil) || client.Notify(t.Context(), "panic", nil) || client.DidChange(t.Context(), "main.go", nil) || client.DidClose(t.Context(), "main.go") {
		t.Fatal("transport panic succeeded")
	}
	if len(sink.Warnings()) == 0 {
		t.Fatal("transport failures emitted no warning")
	}
}

func TestManagerPublicValidationAndDisabledServer(t *testing.T) {
	assertInvalidManagerOptions(t)
	assertDisabledManagerLifecycle(t)
}

func assertInvalidManagerOptions(t *testing.T) {
	t.Helper()
	if _, err := lsp.NewManager(nilContext(), lsp.Registry{}, lsp.ManagerOptions{}); err == nil {
		t.Fatal("nil manager context succeeded")
	}
	for _, options := range []lsp.ManagerOptions{
		{InitializeTimeout: -1}, {RequestTimeout: -1}, {FailureLimit: -1}, {MaxMessageBytes: -1}, {DiagnosticsPerFile: -1},
	} {
		if _, err := lsp.NewManager(t.Context(), lsp.Registry{}, options); err == nil {
			t.Fatalf("invalid manager bounds succeeded: %#v", options)
		}
	}
}

func assertDisabledManagerLifecycle(t *testing.T) {
	t.Helper()
	registry := lsp.MergeRegistry([]lsp.Server{{ID: "gopls", Disabled: true}}, warning.DiscardSink{})
	manager, err := lsp.NewManager(t.Context(), registry, lsp.ManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if client, err := manager.Start(t.Context(), "gopls", t.TempDir()); err != nil || client != nil {
		t.Fatalf("disabled Start() = %#v, %v", client, err)
	}
	if _, err := manager.Start(nilContext(), "gopls", t.TempDir()); err == nil {
		t.Fatal("nil Start context succeeded")
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := manager.Start(cancelled, "gopls", t.TempDir()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Start error = %v", err)
	}
	if _, err := manager.Start(t.Context(), "missing", t.TempDir()); !errors.Is(err, lsp.ErrUnknownServer) {
		t.Fatalf("unknown Start error = %v", err)
	}
	if _, err := manager.Start(t.Context(), "gopls", filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Fatalf("disabled server should not inspect root: %v", err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Start(t.Context(), "missing", t.TempDir()); !errors.Is(err, lsp.ErrUnknownServer) {
		t.Fatalf("validation precedence changed: %v", err)
	}
	enabled, err := lsp.NewManager(t.Context(), lsp.DefaultRegistry(), lsp.ManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = enabled.Close() }()
	if _, err := enabled.Start(t.Context(), "gopls", filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("enabled server accepted a missing root")
	}
	stagedCancel := &cancelOnErrContext{Context: t.Context(), after: 2, done: make(chan struct{})}
	if _, err := enabled.Start(stagedCancel, "gopls", t.TempDir()); !errors.Is(err, context.Canceled) {
		t.Fatalf("staged cancellation error = %v", err)
	}
}

func TestManagerContainsPublicStartupFailures(t *testing.T) {
	tests := []struct {
		name     string
		lookPath func(string) (string, error)
		start    func(context.Context, string, []string, string, int64, lsp.MessageHandler) (lsp.Transport, error)
	}{
		{name: "lookup", lookPath: func(string) (string, error) { return "", errors.New("missing") }},
		{name: "start error", start: func(context.Context, string, []string, string, int64, lsp.MessageHandler) (lsp.Transport, error) {
			return nil, errors.New("start")
		}},
		{name: "nil transport", start: func(context.Context, string, []string, string, int64, lsp.MessageHandler) (lsp.Transport, error) {
			return nil, nil
		}},
		{name: "nil done", start: func(context.Context, string, []string, string, int64, lsp.MessageHandler) (lsp.Transport, error) {
			return &publicTransport{}, nil
		}},
		{name: "closed transport", start: func(context.Context, string, []string, string, int64, lsp.MessageHandler) (lsp.Transport, error) {
			transport := newPublicTransport(nil)
			_ = transport.Close()
			return transport, nil
		}},
		{name: "panicking done", start: func(context.Context, string, []string, string, int64, lsp.MessageHandler) (lsp.Transport, error) {
			return &panicDoneTransport{publicTransport: newPublicTransport(nil)}, nil
		}},
		{name: "initialize call", start: func(context.Context, string, []string, string, int64, lsp.MessageHandler) (lsp.Transport, error) {
			transport := newPublicTransport(nil)
			transport.call = func(context.Context, string, any, any) error { return errors.New("initialize") }
			return transport, nil
		}},
		{name: "initialized notify", start: func(context.Context, string, []string, string, int64, lsp.MessageHandler) (lsp.Transport, error) {
			transport := newPublicTransport(nil)
			transport.notify = func(context.Context, string, any) error { return errors.New("initialized") }
			return transport, nil
		}},
		{name: "panic", start: func(context.Context, string, []string, string, int64, lsp.MessageHandler) (lsp.Transport, error) {
			panic("start")
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sink := &warning.SliceSink{}
			lookPath := test.lookPath
			if lookPath == nil {
				lookPath = func(string) (string, error) { return "/test/gopls", nil }
			}
			manager, err := lsp.NewManager(t.Context(), lsp.DefaultRegistry(), lsp.ManagerOptions{
				Warnings: sink, FailureLimit: 1, LookPath: lookPath, Start: test.start,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = manager.Close() }()
			client, err := manager.Start(t.Context(), "gopls", t.TempDir())
			if err != nil || client != nil || len(sink.Warnings()) != 1 {
				t.Fatalf("Start() = %#v, %v; warnings = %#v", client, err, sink.Warnings())
			}
		})
	}
}

func TestManagerConcurrentStartWaitAndCloseTimeout(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	manager, err := lsp.NewManager(t.Context(), lsp.DefaultRegistry(), lsp.ManagerOptions{
		InitializeTimeout: time.Millisecond,
		LookPath:          func(string) (string, error) { return "/test/gopls", nil },
		Start: func(context.Context, string, []string, string, int64, lsp.MessageHandler) (lsp.Transport, error) {
			close(started)
			<-release
			return newPublicTransport(nil), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	firstDone := make(chan error, 1)
	go func() {
		_, startErr := manager.Start(t.Context(), "gopls", root)
		firstDone <- startErr
	}()
	<-started
	waitContext, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := manager.Start(waitContext, "gopls", root); !errors.Is(err, context.Canceled) {
		t.Fatalf("waiting Start error = %v", err)
	}
	if err := manager.Close(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close timeout error = %v", err)
	}
	close(release)
	if err := <-firstDone; !errors.Is(err, lsp.ErrManagerClosed) {
		t.Fatalf("first Start error = %v", err)
	}
}

func TestManagerContainsDefaultProcessStartupFailure(t *testing.T) {
	sink := &warning.SliceSink{}
	manager, err := lsp.NewManager(t.Context(), lsp.DefaultRegistry(), lsp.ManagerOptions{
		Warnings: sink,
		LookPath: func(string) (string, error) {
			return filepath.Join(t.TempDir(), "missing-gopls"), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = manager.Close() }()
	if client, err := manager.Start(t.Context(), "gopls", t.TempDir()); err != nil || client != nil || len(sink.Warnings()) != 1 {
		t.Fatalf("default process Start = %#v, %v; warnings = %#v", client, err, sink.Warnings())
	}
}

func TestRPCTransportPublicValidationAndPayloads(t *testing.T) {
	assertNilRPCTransport(t)
	client, cleanup := newRPCTransportPair(t)
	defer cleanup()
	assertRPCTransportPayloads(t, client)
	if err := client.Close(); err != nil || client.Close() != nil {
		t.Fatal("idempotent close failed")
	}
}

func assertNilRPCTransport(t *testing.T) {
	t.Helper()
	var nilTransport *lsp.RPCTransport
	if !errors.Is(nilTransport.Call(t.Context(), "x", nil, nil), jsonrpc2.ErrClosed) || !errors.Is(nilTransport.Notify(t.Context(), "x", nil), jsonrpc2.ErrClosed) {
		t.Fatal("nil transport was open")
	}
	select {
	case <-nilTransport.Done():
	default:
		t.Fatal("nil transport Done was open")
	}
	if err := nilTransport.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := lsp.NewRPCTransport(nilContext(), nil, 0, nil); err == nil {
		t.Fatal("nil transport construction succeeded")
	}
}

func newRPCTransportPair(t *testing.T) (*lsp.RPCTransport, func()) {
	t.Helper()
	clientConnection, serverConnection := net.Pipe()
	ctx, cancel := context.WithCancel(t.Context())
	server, err := lsp.NewRPCTransport(ctx, serverConnection, 0, func(_ context.Context, method string, raw json.RawMessage) (any, error) {
		if method == "panic" {
			panic("server panic")
		}
		if method == "error" {
			return nil, errors.New("server error")
		}
		if method == "null" {
			return nil, nil
		}
		return append(json.RawMessage(nil), raw...), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	client, err := lsp.NewRPCTransport(ctx, clientConnection, 0, nil)
	if err != nil {
		cancel()
		_ = server.Close()
		t.Fatal(err)
	}
	return client, func() {
		_ = client.Close()
		_ = server.Close()
		cancel()
	}
}

func assertRPCTransportPayloads(t *testing.T, client *lsp.RPCTransport) {
	t.Helper()
	if err := client.Call(nilContext(), "nil", nil, nil); err == nil || client.Notify(nilContext(), "nil", nil) == nil {
		t.Fatal("nil request context succeeded")
	}
	var echoed map[string]int
	if err := client.Call(t.Context(), "echo", json.RawMessage(`{"value":1}`), &echoed); err != nil || echoed["value"] != 1 {
		t.Fatalf("raw Call() = %#v, %v", echoed, err)
	}
	if err := client.Call(t.Context(), "null", nil, new(any)); err != nil {
		t.Fatal(err)
	}
	if err := client.Call(t.Context(), "error", nil, nil); err == nil {
		t.Fatal("server error succeeded")
	}
	if err := client.Call(t.Context(), "panic", nil, nil); err == nil {
		t.Fatal("server panic request succeeded")
	}
	if err := client.Notify(t.Context(), "panic", nil); err != nil {
		t.Fatalf("panic notification error = %v", err)
	}
	bad := map[string]any{"function": func() {}}
	if err := client.Call(t.Context(), "bad", bad, nil); err == nil || client.Notify(t.Context(), "bad", bad) == nil {
		t.Fatal("unmarshalable payload succeeded")
	}
	if err := client.Notify(t.Context(), "bad-raw", json.RawMessage("{")); err == nil {
		t.Fatal("invalid raw payload succeeded")
	}
}

func TestRPCTransportRejectsMalformedPublicFrames(t *testing.T) {
	frames := []string{
		"",
		strings.Repeat("x", 9000) + "\n",
		"Content-Length: 1\n\nx",
		"Broken\r\n\r\n",
		"Content-Length:\r\n\r\n",
		"X-Test: value\r\n\r\n",
		"Content-Length: 1\r\n\r\n{",
	}
	for _, frame := range frames {
		clientConnection, peer := net.Pipe()
		transport, err := lsp.NewRPCTransport(t.Context(), clientConnection, 32, nil)
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			_, _ = io.WriteString(peer, frame)
			_ = peer.Close()
		}()
		select {
		case <-transport.Done():
		case <-time.After(time.Second):
			t.Fatalf("malformed frame did not disconnect: %q", frame)
		}
		_ = transport.Close()
	}
}

func TestManagerUsesPublicProcessTransport(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	registry := lsp.MergeRegistry([]lsp.Server{{
		ID: "helper", Command: "helper", Args: []string{"-test.run=TestPublicLSPHelperProcess", "--", "plasmid-lsp-helper"}, Extensions: []string{".helper"},
	}}, warning.DiscardSink{})
	manager, err := lsp.NewManager(t.Context(), registry, lsp.ManagerOptions{
		LookPath:          func(string) (string, error) { return executable, nil },
		InitializeTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	client, err := manager.Start(t.Context(), "helper", t.TempDir())
	if err != nil || client == nil {
		t.Fatalf("Start() = %#v, %v", client, err)
	}
	if !client.Call(t.Context(), "echo", map[string]int{"value": 1}, nil) || !client.Notify(t.Context(), "observed", nil) {
		t.Fatal("process transport request failed")
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPublicLSPHelperProcess(t *testing.T) {
	if !strings.Contains(strings.Join(os.Args, "\x00"), "plasmid-lsp-helper") {
		return
	}
	reader := bufio.NewReader(os.Stdin)
	for {
		payload, status := readLSPHelperFrame(reader)
		if status >= 0 {
			os.Exit(status)
		}
		writeLSPHelperResponse(payload)
	}
}

func readLSPHelperFrame(reader *bufio.Reader) ([]byte, int) {
	length, status := readLSPHelperLength(reader)
	if status >= 0 {
		return nil, status
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return nil, 2
	}
	return payload, -1
}

func readLSPHelperLength(reader *bufio.Reader) (int, int) {
	length := -1
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return 0, 0
		}
		if line == "\r\n" {
			break
		}
		name, value, found := strings.Cut(strings.TrimSuffix(line, "\r\n"), ":")
		if found && strings.EqualFold(name, "Content-Length") {
			length, _ = strconv.Atoi(strings.TrimSpace(value))
		}
	}
	if length < 0 {
		return 0, 2
	}
	return length, -1
}

func writeLSPHelperResponse(payload []byte) {
	var request struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if json.Unmarshal(payload, &request) != nil || len(request.ID) == 0 {
		return
	}
	result := json.RawMessage("null")
	if request.Method == "initialize" {
		result = json.RawMessage(`{"capabilities":{"positionEncoding":"utf-8"}}`)
	}
	response := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":%s}`, request.ID, result)
	_, _ = fmt.Fprintf(os.Stdout, "Content-Length: %d\r\n\r\n%s", len(response), response)
}

func TestPublicPathDiagnosticRegistryAndRootEdges(t *testing.T) {
	assertPublicPathAndPositionEdges(t)
	assertPublicPositionEdges(t)
	assertPublicDiagnosticEdges(t)
	assertPublicRegistryEdges(t)
	assertPublicRootSelectionEdges(t)
}

func assertPublicPathAndPositionEdges(t *testing.T) {
	t.Helper()
	for _, path := range []string{"", "relative", "bad\x00path", `\\server`, `//server/`} {
		if _, err := lsp.PathToFileURI(path); !errors.Is(err, lsp.ErrInvalidURI) {
			t.Errorf("PathToFileURI(%q) error = %v", path, err)
		}
	}
	for _, uri := range []string{"file:///a#fragment", "file://user@example/a", "file:///a\x00b", "file:///a%00b"} {
		if _, err := lsp.FileURIToPath(uri); !errors.Is(err, lsp.ErrInvalidURI) {
			t.Errorf("FileURIToPath(%q) error = %v", uri, err)
		}
	}
}

func assertPublicPositionEdges(t *testing.T) {
	t.Helper()
	invalidUTF8 := []byte{0xff}
	if _, err := lsp.PositionForOffset(invalidUTF8, 0, protocol.PositionEncodingKindUTF8); !errors.Is(err, lsp.ErrInvalidPosition) {
		t.Fatal(err)
	}
	if _, err := lsp.OffsetForPosition(invalidUTF8, protocol.Position{}, protocol.PositionEncodingKindUTF8); !errors.Is(err, lsp.ErrInvalidPosition) {
		t.Fatal(err)
	}
	content := []byte("a😀\n")
	for _, encoding := range []protocol.PositionEncodingKind{"unknown", ""} {
		position, err := lsp.PositionForOffset(content, 5, encoding)
		if encoding == "unknown" && !errors.Is(err, lsp.ErrInvalidPosition) {
			t.Fatalf("unknown encoding position = %#v, %v", position, err)
		}
	}
	if _, err := lsp.OffsetForPosition(content, protocol.Position{Line: 9}, protocol.PositionEncodingKindUTF8); !errors.Is(err, lsp.ErrInvalidPosition) {
		t.Fatal(err)
	}
	if _, err := lsp.OffsetForPosition(content, protocol.Position{Character: 99}, protocol.PositionEncodingKindUTF8); !errors.Is(err, lsp.ErrInvalidPosition) {
		t.Fatal(err)
	}
	if offset, err := lsp.OffsetForPosition(content, protocol.Position{Character: 3}, ""); err != nil || offset != 5 {
		t.Fatalf("default encoded offset = %d, %v", offset, err)
	}
	if _, err := lsp.OffsetForPosition(content, protocol.Position{Character: 1}, "unknown"); !errors.Is(err, lsp.ErrInvalidPosition) {
		t.Fatal(err)
	}
}

func assertPublicDiagnosticEdges(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	file := filepath.Join(root, "main.go")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	uri, _ := lsp.PathToFileURI(file)
	values := []protocol.Diagnostic{
		{Range: protocol.Range{End: protocol.Position{Character: 1}}, Message: protocol.String("same")},
		{Range: protocol.Range{End: protocol.Position{Character: 1}}, Message: protocol.String("same")},
		{Range: protocol.Range{Start: protocol.Position{Character: 1}, End: protocol.Position{Character: 3}}, Severity: protocol.DiagnosticSeverityWarning, Code: protocol.String("B"), Source: protocol.NewOptional("z"), Message: protocol.String("z")},
		{Range: protocol.Range{Start: protocol.Position{Character: 1}, End: protocol.Position{Character: 2}}, Severity: protocol.DiagnosticSeverityWarning, Code: protocol.String("B"), Source: protocol.NewOptional("z"), Message: protocol.String("z")},
		{Range: protocol.Range{Start: protocol.Position{Character: 1}, End: protocol.Position{Character: 2}}, Severity: protocol.DiagnosticSeverityError, Code: protocol.String("B"), Source: protocol.NewOptional("z"), Message: protocol.String("z")},
		{Range: protocol.Range{Start: protocol.Position{Character: 1}, End: protocol.Position{Character: 2}}, Severity: protocol.DiagnosticSeverityError, Code: protocol.String("A"), Source: protocol.NewOptional("z"), Message: protocol.String("z")},
		{Range: protocol.Range{Start: protocol.Position{Character: 1}, End: protocol.Position{Character: 2}}, Severity: protocol.DiagnosticSeverityError, Code: protocol.String("A"), Source: protocol.NewOptional("a"), Message: protocol.String("z")},
		{Range: protocol.Range{Start: protocol.Position{Character: 1}, End: protocol.Position{Character: 2}}, Severity: protocol.DiagnosticSeverityError, Code: protocol.String("A"), Source: protocol.NewOptional("a"), Message: protocol.String("a")},
		{Range: protocol.Range{Start: protocol.Position{Line: 2}, End: protocol.Position{Line: 1}}, Message: protocol.String("backwards")},
		{Range: protocol.Range{Start: protocol.Position{Line: 1}, End: protocol.Position{Line: 1}}, Code: protocol.String("S"), Message: &protocol.MarkupContent{Value: "markup\r\nline"}},
	}
	got, err := lsp.NormalizeDiagnostics(root, uri, values, 0)
	if err != nil || len(got) != 8 || got[7].Message != "markup\nline" {
		t.Fatalf("NormalizeDiagnostics() = %#v, %v", got, err)
	}
	if _, err := lsp.NormalizeDiagnostics(filepath.Join(root, "missing"), uri, nil, 1); err == nil {
		t.Fatal("missing diagnostic root succeeded")
	}
	if _, err := lsp.NormalizeDiagnostics(root, "https://example.test/a", nil, 1); err == nil {
		t.Fatal("non-file diagnostic URI succeeded")
	}
}

func assertPublicRegistryEdges(t *testing.T) {
	t.Helper()
	sink := &warning.SliceSink{}
	registry := lsp.MergeRegistry([]lsp.Server{
		{ID: "gopls", Args: []string{"-remote=auto"}, Extensions: []string{".GO", ".go"}, RootMarkers: []string{"", "go.mod", "go.mod"}},
		{ID: "bad id", Command: "bad", Extensions: []string{".bad"}},
		{ID: "nul", Command: "bad\x00command", Extensions: []string{".bad"}},
		{ID: "arg", Command: "arg", Args: []string{"bad\x00arg"}, Extensions: []string{".arg"}},
		{ID: "ext", Command: "ext", Extensions: []string{"bad"}},
		{ID: "marker", Command: "marker", Extensions: []string{".mark"}, RootMarkers: []string{"../bad"}},
		{ID: "empty", Command: "empty"},
	}, sink)
	if len(sink.Warnings()) != 6 || len(registry.Match("MAIN.GO")) != 1 {
		t.Fatalf("registry warnings/matches = %#v/%#v", sink.Warnings(), registry.Match("MAIN.GO"))
	}
	servers := registry.Servers()
	servers[0].Args = nil
	if original, ok := registry.Server("gopls"); !ok || len(original.Args) != 1 {
		t.Fatalf("registry defensive copy = %#v, %t", original, ok)
	}
	if _, ok := registry.Server("missing"); ok {
		t.Fatal("missing registry server found")
	}
}

func assertPublicRootSelectionEdges(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	if _, err := lsp.SelectWorkspaceRoot(filepath.Join(root, "missing"), ".", nil); err == nil {
		t.Fatal("missing workspace root succeeded")
	}
	nested := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	nestedFile := filepath.Join(nested, "main.go")
	if err := os.WriteFile(nestedFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if selected, err := lsp.SelectWorkspaceRoot(root, nestedFile, []string{"go.mod"}); err != nil || selected != root {
		t.Fatalf("SelectWorkspaceRoot() = %q, %v", selected, err)
	}
	if _, err := lsp.SelectWorkspaceRoot(root, "missing.go", nil); err == nil {
		t.Fatal("missing root path succeeded")
	}
	if _, err := lsp.SelectWorkspaceRoot(root, nestedFile, []string{"../bad"}); err == nil {
		t.Fatal("invalid root marker succeeded")
	}
}

func TestEnforcerPublicLifecycle(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "main.go")
	content := []byte("package main\n")
	if err := os.WriteFile(file, content, 0o600); err != nil {
		t.Fatal(err)
	}
	assertInvalidEnforcerOptions(t, root)
	manager, transportSlot := newPublicEnforcerManager(t)
	defer func() { _ = manager.Close() }()
	bus := workspace.NewTouchBus()
	enforcer, err := lsp.NewEnforcer(lsp.EnforcerOptions{
		WorkspaceDir: root, Touches: bus, Manager: manager, Output: outputlimit.Defaults(), Warnings: warning.DiscardSink{}, SettleTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defaultEnforcer, err := lsp.NewEnforcer(lsp.EnforcerOptions{WorkspaceDir: root, Touches: workspace.NewTouchBus(), Manager: manager})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = defaultEnforcer.Close() }()
	assertEnforcerLifecycle(t, enforcer, bus, transportSlot, content)
}

func assertInvalidEnforcerOptions(t *testing.T, root string) {
	t.Helper()
	for _, options := range []lsp.EnforcerOptions{{}, {WorkspaceDir: root}, {WorkspaceDir: root, Touches: workspace.NewTouchBus()}} {
		if _, err := lsp.NewEnforcer(options); err == nil {
			t.Fatalf("invalid enforcer options succeeded: %#v", options)
		}
	}
}

func newPublicEnforcerManager(t *testing.T) (*lsp.Manager, *publicTransportSlot) {
	t.Helper()
	slot := &publicTransportSlot{}
	manager, err := lsp.NewManager(t.Context(), lsp.DefaultRegistry(), lsp.ManagerOptions{
		Warnings: warning.DiscardSink{}, LookPath: func(string) (string, error) { return "/test/gopls", nil },
		Start: func(_ context.Context, _ string, _ []string, _ string, _ int64, handler lsp.MessageHandler) (lsp.Transport, error) {
			slot.transport = newPublicTransport(handler)
			slot.transport.notify = publicEnforcerNotifier(slot.transport)
			return slot.transport, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return manager, slot
}

func publicEnforcerNotifier(transport *publicTransport) func(context.Context, string, any) error {
	return func(ctx context.Context, method string, params any) error {
		if method != "textDocument/didOpen" {
			return nil
		}
		encoded, _ := json.Marshal(params)
		var opened struct {
			TextDocument struct {
				URI     string `json:"uri"`
				Version int32  `json:"version"`
			} `json:"textDocument"`
		}
		_ = json.Unmarshal(encoded, &opened)
		raw, _ := protocol.Marshal(map[string]any{
			"uri": opened.TextDocument.URI, "version": opened.TextDocument.Version,
			"diagnostics": []protocol.Diagnostic{
				{Range: protocol.Range{End: protocol.Position{Character: 1}}, Severity: protocol.DiagnosticSeverityError, Message: protocol.String("broken")},
				{Range: protocol.Range{Start: protocol.Position{Character: 1}, End: protocol.Position{Character: 2}}, Severity: protocol.DiagnosticSeverityInformation, Message: protocol.String("info")},
				{Range: protocol.Range{Start: protocol.Position{Character: 2}, End: protocol.Position{Character: 3}}, Severity: protocol.DiagnosticSeverityHint, Message: protocol.String("hint")},
			},
		})
		_, _ = transport.handler(ctx, "textDocument/publishDiagnostics", raw)
		return nil
	}
}

func assertEnforcerLifecycle(t *testing.T, enforcer *lsp.Enforcer, bus *workspace.TouchBus, slot *publicTransportSlot, content []byte) {
	t.Helper()
	if enforcer.Status() != "LSP: none detected" {
		t.Fatalf("initial status = %q", enforcer.Status())
	}
	bus.Publish(t.Context(), workspace.Touch{SessionID: "session", InvocationID: "write", Path: "main.go", Kind: workspace.TouchWrite, Content: content})
	decoration, ok := enforcer.Await(t.Context(), "session", "write")
	if !ok || len(decoration.Diagnostics) != 3 || !strings.Contains(decoration.Text, "error: broken") || !strings.Contains(decoration.Text, "information: info") || !strings.Contains(decoration.Text, "hint: hint") {
		t.Fatalf("Await() = %#v, %t", decoration, ok)
	}
	if enforcer.Status() != "LSP: gopls" {
		t.Fatalf("active status = %q", enforcer.Status())
	}
	enforcer.ObserveTouch(t.Context(), workspace.Touch{InvocationID: "invalid-utf8", Path: "main.go", Kind: workspace.TouchEdit, Content: []byte{0xff}})
	transport := slot.transport
	transport.mu.Lock()
	transport.notify = func(context.Context, string, any) error { return errors.New("sync failed") }
	transport.mu.Unlock()
	enforcer.ObserveTouch(t.Context(), workspace.Touch{InvocationID: "notify-failed", Path: "main.go", Kind: workspace.TouchEdit, Content: content})
	enforcer.Drop("session", "missing")
	if _, ok := enforcer.Await(nilContext(), "session", "write"); ok {
		t.Fatal("nil Await context succeeded")
	}
	if err := enforcer.Close(); err != nil || enforcer.Close() != nil {
		t.Fatal("idempotent enforcer close failed")
	}
	enforcer.ObserveTouch(t.Context(), workspace.Touch{InvocationID: "closed", Path: "main.go", Kind: workspace.TouchWrite})
	bus.Publish(t.Context(), workspace.Touch{SessionID: "session", InvocationID: "closed", Path: "main.go", Kind: workspace.TouchEdit, Content: content})
	if _, ok := enforcer.Await(t.Context(), "session", "closed"); ok {
		t.Fatal("closed enforcer retained touch")
	}
	var nilEnforcer *lsp.Enforcer
	if nilEnforcer.Status() != "" || nilEnforcer.Close() != nil {
		t.Fatal("nil enforcer lifecycle failed")
	}
	nilEnforcer.Drop("session", "call")
}

func TestEnforcerContainsPublicBoundaryFailures(t *testing.T) {
	_ = lsp.MergeRegistry([]lsp.Server{{ID: "empty", Command: "empty"}}, nil)
	root := t.TempDir()
	file := filepath.Join(root, "main.go")
	if err := os.WriteFile(file, []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	baseManager, err := lsp.NewManager(t.Context(), lsp.DefaultRegistry(), lsp.ManagerOptions{
		LookPath: func(string) (string, error) { return "", errors.New("missing") },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = baseManager.Close() }()
	if _, err := lsp.NewEnforcer(lsp.EnforcerOptions{
		WorkspaceDir: filepath.Join(root, "missing"), Touches: workspace.NewTouchBus(), Manager: baseManager,
	}); err == nil {
		t.Fatal("missing enforcer workspace succeeded")
	}
	enforcer, err := lsp.NewEnforcer(lsp.EnforcerOptions{
		WorkspaceDir: root, Touches: workspace.NewTouchBus(), Manager: baseManager, Warnings: warning.DiscardSink{},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = enforcer.Close() }()
	enforcer.ObserveTouch(t.Context(), workspace.Touch{InvocationID: "missing", Path: "missing.go", Kind: workspace.TouchWrite})
	enforcer.ObserveTouch(t.Context(), workspace.Touch{InvocationID: "unavailable", Path: "main.go", Kind: workspace.TouchWrite})

	closedManager, err := lsp.NewManager(t.Context(), lsp.DefaultRegistry(), lsp.ManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_ = closedManager.Close()
	closedEnforcer, err := lsp.NewEnforcer(lsp.EnforcerOptions{
		WorkspaceDir: root, Touches: workspace.NewTouchBus(), Manager: closedManager, Warnings: warning.DiscardSink{},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closedEnforcer.Close() }()
	closedEnforcer.ObserveTouch(t.Context(), workspace.Touch{InvocationID: "closed", Path: "main.go", Kind: workspace.TouchWrite})

	registry := lsp.MergeRegistry([]lsp.Server{{ID: "other", Command: "other", Extensions: []string{".go"}}}, warning.DiscardSink{})
	mismatchEnforcer, err := lsp.NewEnforcer(lsp.EnforcerOptions{
		WorkspaceDir: root, Touches: workspace.NewTouchBus(), Manager: baseManager, Registry: registry, Warnings: warning.DiscardSink{},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mismatchEnforcer.Close() }()
	mismatchEnforcer.ObserveTouch(t.Context(), workspace.Touch{InvocationID: "mismatch", Path: "main.go", Kind: workspace.TouchWrite})
}

var _ lsp.Transport = (*publicTransport)(nil)
var _ lsp.Transport = (*panicDoneTransport)(nil)

type cancelOnErrContext struct {
	context.Context
	mu    sync.Mutex
	once  sync.Once
	after int
	calls int
	done  chan struct{}
}

func nilContext() context.Context { return nil }

func (c *cancelOnErrContext) Done() <-chan struct{} { return c.done }

func (c *cancelOnErrContext) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.calls >= c.after {
		c.once.Do(func() { close(c.done) })
		return context.Canceled
	}
	return c.Context.Err()
}
