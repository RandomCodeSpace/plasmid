package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/RandomCodeSpace/plasmid/config"
	"github.com/RandomCodeSpace/plasmid/extensions"
	"github.com/RandomCodeSpace/plasmid/outputlimit"
	"github.com/RandomCodeSpace/plasmid/warning"
)

type echoInput struct {
	Value string `json:"value"`
}

const toolsListMethod = "tools/list"

type echoOutput struct {
	Value string `json:"value"`
}

func closeTestResource(t *testing.T, resource io.Closer) {
	t.Helper()
	if err := resource.Close(); err != nil {
		t.Error(err)
	}
}

func TestManagerRequiresCatalogAndWorkingDirectory(t *testing.T) {
	root := t.TempDir()
	catalogs, err := extensions.NewStore(extensions.Options{WorkingDir: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(catalogs.Close)
	for _, options := range []Options{{}, {Catalogs: catalogs}, {WorkingDir: root}} {
		if _, err := New(options); err == nil {
			t.Fatalf("New(%#v) returned nil error", options)
		}
	}
}

func TestManagerLifecycle(t *testing.T) {
	tests := []struct {
		name string
		run  func(*testing.T)
	}{
		{name: "HTTP is lazy, reconnects, and cancels", run: testHTTPIsLazyInjectsHeadersReconnectsAndCancels},
		{name: "close cancels active HTTP call before session delete", run: testCloseCancelsActiveHTTPCallBeforeSessionDelete},
		{name: "drop session cancels active HTTP call before session delete", run: testDropSessionCancelsActiveHTTPCallBeforeSessionDelete},
		{name: "drop session timeout forces cleanup before replacement", run: testDropSessionTimeoutForcesCleanupBeforeReplacement},
		{name: "drop session does not wait for unrelated calls", run: testDropSessionDoesNotWaitForUnrelatedCalls},
		{name: "failed tool discovery finishes cancellation before session delete", run: testFailedToolDiscoveryFinishesCancellationBeforeSessionDelete},
		{name: "stdio process closes with session", run: testStdioProcessClosesWithSession},
		{name: "close waits for tool discovery", run: testToolDiscoveryTimeoutsAndCloseWaitsForList},
		{name: "connections close concurrently", run: testCloseTearsDownConnectionsConcurrently},
		{name: "session refresh drops owned connections", run: testDropSessionClosesOwnedConnections},
		{name: "canceled session refresh retains teardown serialization", run: testCanceledDropSessionSerializesReplacement},
		{name: "session and server keys are delimiter safe", run: testDropSessionUsesExactStructuredIdentity},
	}
	for _, test := range tests {
		t.Run(test.name, test.run)
	}
}

func testDropSessionTimeoutForcesCleanupBeforeReplacement(t *testing.T) {
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "drop-timeout", Version: "1"}, nil)
	started := make(chan struct{})
	forceRelease := make(chan struct{})
	defer close(forceRelease)
	sdkmcp.AddTool(server, &sdkmcp.Tool{Name: "block", Description: "ignore protocol cancellation"}, func(context.Context, *sdkmcp.CallToolRequest, struct{}) (*sdkmcp.CallToolResult, echoOutput, error) {
		close(started)
		<-forceRelease
		return nil, echoOutput{}, context.Canceled
	})
	stream := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return server }, nil)
	cancellationStarted := make(chan struct{}, 1)
	var interceptedCancellation atomic.Bool
	httpServer := httptest.NewServer(&dropTimeoutHTTPState{
		stream: stream, started: started, forceRelease: forceRelease,
		cancellationStarted: cancellationStarted, intercepted: &interceptedCancellation,
	})
	t.Cleanup(httpServer.Close)

	manager, catalog := configuredManagerWithOptions(t, config.MCPServer{ID: "drop-timeout", Transport: config.MCPHTTP, URL: httpServer.URL}, Options{CloseGrace: 50 * time.Millisecond})
	qualified := "plasmid:configured:drop-timeout"
	connection, err := manager.connection(t.Context(), "session", qualified, catalog)
	if err != nil {
		t.Fatal(err)
	}
	callDone := make(chan error, 1)
	go func() {
		_, callErr := connection.call(context.Background(), "session", "block", nil)
		callDone <- callErr
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("blocking MCP call did not start")
	}
	if err := manager.DropSession(context.Background(), "session"); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("DropSession error = %v, want timeout", err)
	}
	select {
	case <-cancellationStarted:
	default:
		t.Fatal("DropSession did not attempt protocol cancellation before forcing cleanup")
	}
	reconnected := make(chan error, 1)
	go func() {
		_, reconnectErr := manager.connection(context.Background(), "session", qualified, catalog)
		reconnected <- reconnectErr
	}()
	select {
	case err := <-reconnected:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("replacement connection remained blocked after DropSession timed out")
	}
	select {
	case err := <-callDone:
		if err == nil {
			t.Fatal("forced dropped-session MCP call returned nil")
		}
	case <-time.After(time.Second):
		t.Fatal("forced dropped-session MCP call did not return")
	}
}

type dropTimeoutHTTPState struct {
	stream              http.Handler
	started             <-chan struct{}
	forceRelease        <-chan struct{}
	cancellationStarted chan<- struct{}
	intercepted         *atomic.Bool
}

func (s *dropTimeoutHTTPState) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodPost && s.interceptPost(request) {
		return
	}
	s.stream.ServeHTTP(response, request)
}

func (s *dropTimeoutHTTPState) interceptPost(request *http.Request) bool {
	select {
	case <-s.started:
		if !s.intercepted.CompareAndSwap(false, true) {
			return false
		}
		s.cancellationStarted <- struct{}{}
		select {
		case <-request.Context().Done():
		case <-s.forceRelease:
		}
		return true
	default:
		return false
	}
}

func testDropSessionDoesNotWaitForUnrelatedCalls(t *testing.T) {
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "drop-scope", Version: "1"}, nil)
	started := make(chan struct{})
	inspect := make(chan struct{})
	inspectionResult := make(chan error, 1)
	var inspectOnce sync.Once
	releaseInspection := func() { inspectOnce.Do(func() { close(inspect) }) }
	defer releaseInspection()
	sdkmcp.AddTool(server, &sdkmcp.Tool{Name: "block", Description: "wait for caller cancellation"}, func(ctx context.Context, _ *sdkmcp.CallToolRequest, _ struct{}) (*sdkmcp.CallToolResult, echoOutput, error) {
		close(started)
		select {
		case <-inspect:
		case <-ctx.Done():
		}
		inspectionResult <- ctx.Err()
		<-ctx.Done()
		return nil, echoOutput{}, ctx.Err()
	})
	httpServer := newMCPHTTPServer(t, server)
	manager, catalog := configuredManager(t, config.MCPServer{ID: "drop-scope", Transport: config.MCPHTTP, URL: httpServer.URL})
	qualified := "plasmid:configured:drop-scope"
	if _, err := manager.connection(t.Context(), "target", qualified, catalog); err != nil {
		t.Fatal(err)
	}
	unrelated, err := manager.connection(t.Context(), "unrelated", qualified, catalog)
	if err != nil {
		t.Fatal(err)
	}
	callContext, cancelCall := context.WithCancel(context.Background())
	defer cancelCall()
	callDone := make(chan error, 1)
	go func() {
		_, callErr := unrelated.call(callContext, "unrelated", "block", nil)
		callDone <- callErr
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("unrelated MCP call did not start")
	}
	dropDone := make(chan error, 1)
	go func() { dropDone <- manager.DropSession(context.Background(), "target") }()
	select {
	case err := <-dropDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("DropSession waited for an unrelated session call")
	}
	releaseInspection()
	select {
	case err := <-inspectionResult:
		if err != nil {
			t.Fatalf("DropSession interrupted an unrelated call: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("unrelated MCP call did not report its state")
	}
	cancelCall()
	select {
	case err := <-callDone:
		if err == nil {
			t.Fatal("canceled unrelated MCP call returned nil")
		}
	case <-time.After(time.Second):
		t.Fatal("unrelated MCP call did not stop after caller cancellation")
	}
}

func testDropSessionCancelsActiveHTTPCallBeforeSessionDelete(t *testing.T) {
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "drop-order", Version: "1"}, nil)
	started := make(chan struct{})
	canceled := make(chan struct{})
	forceRelease := make(chan struct{})
	defer close(forceRelease)
	sdkmcp.AddTool(server, &sdkmcp.Tool{Name: "block", Description: "wait for cancellation"}, func(ctx context.Context, _ *sdkmcp.CallToolRequest, _ struct{}) (*sdkmcp.CallToolResult, echoOutput, error) {
		close(started)
		select {
		case <-ctx.Done():
			close(canceled)
		case <-forceRelease:
		}
		return nil, echoOutput{}, context.Canceled
	})
	stream := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return server }, nil)
	cancellationStarted := make(chan struct{}, 1)
	deleteStarted := make(chan struct{}, 1)
	var orderingViolation atomic.Bool
	httpServer := httptest.NewServer(&dropOrderHTTPState{
		stream: stream, started: started, canceled: canceled, forceRelease: forceRelease,
		cancellationStarted: cancellationStarted, deleteStarted: deleteStarted, violation: &orderingViolation,
	})
	t.Cleanup(httpServer.Close)

	manager, catalog := configuredManager(t, config.MCPServer{ID: "drop-order", Transport: config.MCPHTTP, URL: httpServer.URL})
	connection, err := manager.connection(t.Context(), "session", "plasmid:configured:drop-order", catalog)
	if err != nil {
		t.Fatal(err)
	}
	callDone := make(chan error, 1)
	go func() {
		_, callErr := connection.call(context.Background(), "session", "block", nil)
		callDone <- callErr
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("blocking MCP call did not start")
	}
	dropDone := make(chan error, 1)
	go func() { dropDone <- manager.DropSession(context.Background(), "session") }()
	select {
	case err := <-dropDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("DropSession did not return")
	}
	select {
	case <-cancellationStarted:
	default:
		t.Fatal("DropSession did not send cancellation")
	}
	select {
	case <-canceled:
	default:
		t.Fatal("DropSession returned before the server tool observed cancellation")
	}
	select {
	case err := <-callDone:
		if err == nil {
			t.Fatal("dropped-session MCP call returned nil")
		}
	case <-time.After(time.Second):
		t.Fatal("dropped-session MCP call did not return")
	}
	select {
	case <-deleteStarted:
	default:
		t.Fatal("DropSession did not delete the MCP session")
	}
	if orderingViolation.Load() {
		t.Fatal("MCP session DELETE started before dropped-session tool cancellation completed")
	}
}

type dropOrderHTTPState struct {
	stream              http.Handler
	started             <-chan struct{}
	canceled            <-chan struct{}
	forceRelease        <-chan struct{}
	cancellationStarted chan<- struct{}
	deleteStarted       chan<- struct{}
	violation           *atomic.Bool
}

func (s *dropOrderHTTPState) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodPost && s.handlePost(response, request) {
		return
	}
	if request.Method == http.MethodDelete && s.rejectEarlyDelete(response) {
		return
	}
	s.stream.ServeHTTP(response, request)
}

func (s *dropOrderHTTPState) handlePost(response http.ResponseWriter, request *http.Request) bool {
	select {
	case <-s.started:
		select {
		case s.cancellationStarted <- struct{}{}:
		default:
		}
		s.stream.ServeHTTP(response, request)
		select {
		case <-s.canceled:
		case <-s.forceRelease:
		}
		return true
	default:
		return false
	}
}

func (s *dropOrderHTTPState) rejectEarlyDelete(response http.ResponseWriter) bool {
	select {
	case s.deleteStarted <- struct{}{}:
	default:
	}
	select {
	case <-s.canceled:
		return false
	default:
		s.violation.Store(true)
		http.Error(response, "tool call is still active", http.StatusConflict)
		return true
	}
}

func testFailedToolDiscoveryFinishesCancellationBeforeSessionDelete(t *testing.T) {
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "list-order", Version: "1"}, &sdkmcp.ServerOptions{HasTools: true})
	listStarted := make(chan struct{})
	listReturned := make(chan struct{})
	forceRelease := make(chan struct{})
	defer close(forceRelease)
	server.AddReceivingMiddleware(listOrderMiddleware(listStarted, listReturned, forceRelease))
	stream := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return server }, nil)
	cancellationFinished := make(chan struct{})
	deleteStarted := make(chan struct{}, 1)
	var orderingViolation atomic.Bool
	httpServer := httptest.NewServer(&listOrderHTTPState{
		stream: stream, listStarted: listStarted, listReturned: listReturned, forceRelease: forceRelease,
		cancellationFinished: cancellationFinished, deleteStarted: deleteStarted, violation: &orderingViolation,
	})
	t.Cleanup(httpServer.Close)

	manager, _ := configuredManagerWithOptions(t, config.MCPServer{ID: "list-order", Transport: config.MCPHTTP, URL: httpServer.URL}, Options{
		ListTimeout: 20 * time.Millisecond,
		Warnings:    warning.DiscardSink{},
	})
	tools, err := manager.Tools(newFakeReadonlyContext(context.Background(), "session"))
	if err != nil || len(tools) != 0 {
		t.Fatalf("Tools = %#v, err = %v", tools, err)
	}
	select {
	case <-deleteStarted:
	default:
		t.Fatal("failed tool discovery did not close its MCP session")
	}
	if orderingViolation.Load() {
		t.Fatal("failed tool discovery closed its MCP session before cancellation finished")
	}
}

func listOrderMiddleware(listStarted, listReturned chan<- struct{}, forceRelease <-chan struct{}) sdkmcp.Middleware {
	return func(next sdkmcp.MethodHandler) sdkmcp.MethodHandler {
		return func(ctx context.Context, method string, request sdkmcp.Request) (sdkmcp.Result, error) {
			if method != toolsListMethod {
				return next(ctx, method, request)
			}
			close(listStarted)
			select {
			case <-ctx.Done():
				close(listReturned)
			case <-forceRelease:
			}
			return nil, context.Canceled
		}
	}
}

type listOrderHTTPState struct {
	stream               http.Handler
	listStarted          <-chan struct{}
	listReturned         <-chan struct{}
	forceRelease         <-chan struct{}
	cancellationFinished chan struct{}
	deleteStarted        chan<- struct{}
	violation            *atomic.Bool
}

func (s *listOrderHTTPState) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodPost && s.handlePost(response, request) {
		return
	}
	if request.Method == http.MethodDelete && !s.deleteReady() {
		http.Error(response, "tool discovery is still active", http.StatusConflict)
		return
	}
	s.stream.ServeHTTP(response, request)
}

func (s *listOrderHTTPState) handlePost(response http.ResponseWriter, request *http.Request) bool {
	select {
	case <-s.listStarted:
		s.stream.ServeHTTP(response, request)
		select {
		case <-s.listReturned:
			close(s.cancellationFinished)
		case <-s.forceRelease:
		}
		return true
	default:
		return false
	}
}

func (s *listOrderHTTPState) deleteReady() bool {
	select {
	case s.deleteStarted <- struct{}{}:
	default:
	}
	ready := channelClosed(s.listReturned) && channelClosed(s.cancellationFinished)
	if !ready {
		s.violation.Store(true)
	}
	return ready
}

func channelClosed(channel <-chan struct{}) bool {
	select {
	case <-channel:
		return true
	default:
		return false
	}
}

func testCloseCancelsActiveHTTPCallBeforeSessionDelete(t *testing.T) {
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "close-order", Version: "1"}, nil)
	started := make(chan struct{})
	canceled := make(chan struct{})
	forceRelease := make(chan struct{})
	defer close(forceRelease)
	sdkmcp.AddTool(server, &sdkmcp.Tool{Name: "block", Description: "wait for cancellation"}, func(ctx context.Context, _ *sdkmcp.CallToolRequest, _ struct{}) (*sdkmcp.CallToolResult, echoOutput, error) {
		close(started)
		select {
		case <-ctx.Done():
			close(canceled)
		case <-forceRelease:
		}
		return nil, echoOutput{}, context.Canceled
	})
	stream := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return server }, nil)
	cancellationStarted := make(chan struct{}, 1)
	var orderingViolation atomic.Bool
	state := &closeOrderHTTPState{
		stream: stream, started: started, canceled: canceled, forceRelease: forceRelease,
		cancellationStarted: cancellationStarted, violation: &orderingViolation,
	}
	httpServer := httptest.NewServer(state)
	t.Cleanup(httpServer.Close)

	manager, catalog := configuredManager(t, config.MCPServer{ID: "close-order", Transport: config.MCPHTTP, URL: httpServer.URL})
	connection, err := manager.connection(t.Context(), "session", "plasmid:configured:close-order", catalog)
	if err != nil {
		t.Fatal(err)
	}
	state.wire = connection.httpWire
	callDone := make(chan error, 1)
	go func() {
		_, callErr := connection.call(context.Background(), "session", "block", nil)
		callDone <- callErr
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("blocking MCP call did not start")
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- manager.Close() }()
	select {
	case <-cancellationStarted:
	case <-time.After(time.Second):
		t.Fatal("manager close did not send cancellation")
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("server tool handler did not observe cancellation")
	}
	select {
	case err := <-callDone:
		if err == nil {
			t.Fatal("canceled MCP call returned nil")
		}
	case <-time.After(time.Second):
		t.Fatal("canceled MCP call did not return")
	}
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("manager close did not return")
	}
	if orderingViolation.Load() {
		t.Fatal("MCP session DELETE started before active tool cancellation completed")
	}
}

type closeOrderHTTPState struct {
	stream              http.Handler
	started             <-chan struct{}
	canceled            <-chan struct{}
	forceRelease        <-chan struct{}
	cancellationStarted chan<- struct{}
	violation           *atomic.Bool
	wire                *headerTransport
}

func (s *closeOrderHTTPState) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodPost && s.handlePost(response, request) {
		return
	}
	if request.Method == http.MethodDelete && s.rejectEarlyDelete(response) {
		return
	}
	s.stream.ServeHTTP(response, request)
}

func (s *closeOrderHTTPState) handlePost(response http.ResponseWriter, request *http.Request) bool {
	select {
	case <-s.started:
		signal(s.cancellationStarted)
		s.recordShutdownViolation()
		s.stream.ServeHTTP(response, request)
		select {
		case <-s.canceled:
			s.recordShutdownViolation()
		case <-s.forceRelease:
		}
		return true
	default:
		return false
	}
}

func (s *closeOrderHTTPState) rejectEarlyDelete(response http.ResponseWriter) bool {
	if channelClosed(s.canceled) {
		return false
	}
	s.violation.Store(true)
	http.Error(response, "tool call is still active", http.StatusConflict)
	return true
}

func (s *closeOrderHTTPState) recordShutdownViolation() {
	if s.wire != nil && s.wire.isShuttingDown() {
		s.violation.Store(true)
	}
}

func signal(channel chan<- struct{}) {
	select {
	case channel <- struct{}{}:
	default:
	}
}

func testDropSessionClosesOwnedConnections(t *testing.T) {
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "refresh", Version: "1"}, nil)
	sdkmcp.AddTool(server, &sdkmcp.Tool{Name: "echo", Description: "echo input"}, func(_ context.Context, _ *sdkmcp.CallToolRequest, input echoInput) (*sdkmcp.CallToolResult, echoOutput, error) {
		return nil, echoOutput(input), nil
	})
	httpServer := newMCPHTTPServer(t, server)
	manager, catalog := configuredManager(t, config.MCPServer{ID: "refresh", Transport: config.MCPHTTP, URL: httpServer.URL})
	first, err := manager.connection(t.Context(), "session", "plasmid:configured:refresh", catalog)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.DropSession(t.Context(), "session"); err != nil {
		t.Fatal(err)
	}
	second, err := manager.connection(t.Context(), "session", "plasmid:configured:refresh", catalog)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("session refresh reused a stale MCP connection")
	}
}

func testCanceledDropSessionSerializesReplacement(t *testing.T) {
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "drop-overlap", Version: "1"}, nil)
	sdkmcp.AddTool(server, &sdkmcp.Tool{Name: "echo", Description: "echo input"}, func(_ context.Context, _ *sdkmcp.CallToolRequest, input echoInput) (*sdkmcp.CallToolResult, echoOutput, error) {
		return nil, echoOutput(input), nil
	})
	stream := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return server }, nil)
	deleteStarted := make(chan struct{})
	releaseDelete := make(chan struct{})
	var once sync.Once
	httpServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodDelete {
			once.Do(func() { close(deleteStarted) })
			<-releaseDelete
		}
		stream.ServeHTTP(response, request)
	}))
	defer httpServer.Close()
	manager, catalog := configuredManager(t, config.MCPServer{ID: "drop-overlap", Transport: config.MCPHTTP, URL: httpServer.URL})
	qualified := "plasmid:configured:drop-overlap"
	if _, err := manager.connection(t.Context(), "session", qualified, catalog); err != nil {
		t.Fatal(err)
	}
	dropContext, cancelDrop := context.WithCancel(t.Context())
	dropped := make(chan error, 1)
	go func() { dropped <- manager.DropSession(dropContext, "session") }()
	select {
	case <-deleteStarted:
	case <-time.After(time.Second):
		t.Fatal("session teardown did not start")
	}
	cancelDrop()
	if err := <-dropped; !errors.Is(err, context.Canceled) {
		t.Fatalf("DropSession error = %v", err)
	}
	reconnected := make(chan error, 1)
	go func() {
		_, err := manager.connection(context.Background(), "session", qualified, catalog)
		reconnected <- err
	}()
	select {
	case err := <-reconnected:
		t.Fatalf("replacement connected before canceled teardown completed: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseDelete)
	select {
	case err := <-reconnected:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("replacement did not connect after canceled teardown completed")
	}
}

func testDropSessionUsesExactStructuredIdentity(t *testing.T) {
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "key-identity", Version: "1"}, nil)
	sdkmcp.AddTool(server, &sdkmcp.Tool{Name: "echo", Description: "echo input"}, func(_ context.Context, _ *sdkmcp.CallToolRequest, input echoInput) (*sdkmcp.CallToolResult, echoOutput, error) {
		return nil, echoOutput(input), nil
	})
	httpServer := newMCPHTTPServer(t, server)
	root := t.TempDir()
	servers := []config.MCPServer{
		{ID: "b\x00c", Transport: config.MCPHTTP, URL: httpServer.URL},
		{ID: "c", Transport: config.MCPHTTP, URL: httpServer.URL},
	}
	catalogs, err := extensions.NewStore(extensions.Options{WorkingDir: root, MCP: config.MCP{Servers: servers}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(catalogs.Close)
	if err := catalogs.StartSession(t.Context(), "catalog"); err != nil {
		t.Fatal(err)
	}
	catalog, _ := catalogs.Snapshot("catalog")
	manager, err := New(Options{Catalogs: catalogs, WorkingDir: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	firstSession := "a"
	firstServer := "plasmid:configured:b\x00c"
	secondSession := "a\x00plasmid:configured:b"
	secondServer := "c"
	if _, err := manager.connection(t.Context(), firstSession, firstServer, catalog); err != nil {
		t.Fatal(err)
	}
	second, err := manager.connection(t.Context(), secondSession, secondServer, catalog)
	if err != nil {
		t.Fatal(err)
	}
	if len(manager.connections) != 2 {
		t.Fatalf("structured connection keys collapsed: %#v", manager.connections)
	}
	if err := manager.DropSession(t.Context(), firstSession); err != nil {
		t.Fatal(err)
	}
	if len(manager.connections) != 1 || manager.connections[connectionKey{sessionID: secondSession, server: secondServer}] != second {
		t.Fatalf("DropSession removed a distinct NUL-bearing identity: %#v", manager.connections)
	}
}

func TestReconnectSuppression(t *testing.T) {
	tests := []struct {
		name string
		run  func(*testing.T)
	}{
		{name: "failure threshold suppresses retries", run: testFailureThresholdSuppressesFurtherConnections},
		{name: "concurrent sessions share threshold", run: testConcurrentSessionsShareReconnectSuppression},
		{name: "successful call resets failures", run: testFailureCountResetsOnlyAfterSuccessfulCall},
		{name: "replacement waits for teardown", run: testReconnectWaitsForBrokenConnectionTeardown},
	}
	for _, test := range tests {
		t.Run(test.name, test.run)
	}
}

func TestResultOutputLimits(t *testing.T) {
	tests := []struct {
		name string
		run  func(*testing.T)
	}{
		{name: "shared byte budget", run: testCallResultUsesSharedOutputBudgetAndBoundsContentItems},
		{name: "content item count", run: testCallResultItemCountIsBoundedWithoutByteTruncation},
	}
	for _, test := range tests {
		t.Run(test.name, test.run)
	}
}

func TestTransportSafety(t *testing.T) {
	tests := []struct {
		name string
		run  func(*testing.T)
	}{
		{name: "cross-origin redirect drops configured headers", run: testHTTPHeadersNeverCrossConfiguredOriginRedirect},
		{name: "same-origin redirect cycle is bounded", run: testHTTPSameOriginRedirectCycleIsBounded},
		{name: "formatting and errors redact secrets", run: testManagerFormattingRedactsConnectedTransportSecrets},
	}
	for _, test := range tests {
		t.Run(test.name, test.run)
	}
}

func testHTTPIsLazyInjectsHeadersReconnectsAndCancels(t *testing.T) {
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "fake-http", Version: "1"}, nil)
	sdkmcp.AddTool(server, &sdkmcp.Tool{Name: "echo", Description: "echo input"}, func(_ context.Context, _ *sdkmcp.CallToolRequest, input echoInput) (*sdkmcp.CallToolResult, echoOutput, error) {
		return nil, echoOutput(input), nil
	})
	started := make(chan struct{})
	sdkmcp.AddTool(server, &sdkmcp.Tool{Name: "block", Description: "wait for cancellation"}, func(ctx context.Context, _ *sdkmcp.CallToolRequest, _ struct{}) (*sdkmcp.CallToolResult, echoOutput, error) {
		close(started)
		<-ctx.Done()
		return nil, echoOutput{}, ctx.Err()
	})
	stream := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return server }, nil)
	var requests atomic.Int64
	var fail atomic.Bool
	httpServer := httptest.NewServer(&authenticatedFailureHandler{stream: stream, requests: &requests, fail: &fail})
	defer httpServer.Close()

	manager, catalog := configuredManager(t, config.MCPServer{ID: "http", Transport: config.MCPHTTP, URL: httpServer.URL, Headers: map[string]string{"Authorization": "Bearer test"}})
	if requests.Load() != 0 {
		t.Fatalf("construction contacted server: requests = %d", requests.Load())
	}
	connection, err := manager.connection(t.Context(), "session", "plasmid:configured:http", catalog)
	if err != nil {
		t.Fatal(err)
	}
	result, err := connection.call(t.Context(), "session", "echo", map[string]any{"value": "hello"})
	if err != nil || result["is_error"] != false {
		t.Fatalf("echo result = %#v, err = %v", result, err)
	}

	fail.Store(true)
	if _, err := connection.call(t.Context(), "session", "echo", map[string]any{"value": "break"}); err == nil {
		t.Fatal("transport failure succeeded")
	}
	fail.Store(false)
	reconnected, err := manager.connection(t.Context(), "session", "plasmid:configured:http", catalog)
	if err != nil || reconnected == connection {
		t.Fatalf("reconnect = %#v, err = %v", reconnected, err)
	}

	done := make(chan error, 1)
	go func() {
		_, callErr := reconnected.call(context.Background(), "session", "block", nil)
		done <- callErr
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("blocking MCP call did not start")
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled MCP call returned nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("MCP call survived manager close")
	}
}

type authenticatedFailureHandler struct {
	stream   http.Handler
	requests *atomic.Int64
	fail     *atomic.Bool
}

func (h *authenticatedFailureHandler) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	h.requests.Add(1)
	if request.Header.Get("Authorization") != "Bearer test" {
		http.Error(response, "missing authorization", http.StatusUnauthorized)
		return
	}
	if h.fail.Load() {
		http.Error(response, "temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	h.stream.ServeHTTP(response, request)
}

func testStdioProcessClosesWithSession(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "closed")
	manager, catalog := configuredManager(t, config.MCPServer{
		ID: "stdio", Transport: config.MCPStdio, Command: os.Args[0],
		Args: []string{"-test.run=^TestMCPStdioHelper$"}, Env: map[string]string{"PLASMID_MCP_HELPER": "1", "PLASMID_MCP_MARKER": marker},
	})
	connection, err := manager.connection(t.Context(), "session", "plasmid:configured:stdio", catalog)
	if err != nil {
		t.Fatal(err)
	}
	result, err := connection.call(t.Context(), "session", "echo", map[string]any{"value": "stdio"})
	if err != nil || result["is_error"] != false {
		t.Fatalf("stdio result = %#v, err = %v", result, err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("stdio MCP child did not finish after close")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func testFailureThresholdSuppressesFurtherConnections(t *testing.T) {
	var requests atomic.Int64
	httpServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(response, "unavailable", http.StatusServiceUnavailable)
	}))
	defer httpServer.Close()
	root := t.TempDir()
	catalogs, err := extensions.NewStore(extensions.Options{WorkingDir: root, MCP: config.MCP{Servers: []config.MCPServer{{ID: "bad", Transport: config.MCPHTTP, URL: httpServer.URL}}}})
	if err != nil {
		t.Fatal(err)
	}
	warnings := &warning.SliceSink{}
	manager, err := New(Options{Catalogs: catalogs, WorkingDir: root, Warnings: warnings, FailureLimit: 3})
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestResource(t, manager)
	ctx := newFakeReadonlyContext(t.Context(), "session")
	for range 3 {
		tools, toolsErr := manager.Tools(ctx)
		if toolsErr != nil || len(tools) != 0 {
			t.Fatalf("Tools = %#v, err = %v", tools, toolsErr)
		}
	}
	afterThreshold := requests.Load()
	if afterThreshold == 0 {
		t.Fatal("connection was never attempted")
	}
	for range 2 {
		if _, err := manager.Tools(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if requests.Load() != afterThreshold {
		t.Fatalf("suppressed server retried: requests = %d, threshold requests = %d", requests.Load(), afterThreshold)
	}
	if notices := warnings.Warnings(); len(notices) != 3 {
		t.Fatalf("warnings = %#v", notices)
	}
}

func testConcurrentSessionsShareReconnectSuppression(t *testing.T) {
	var requests atomic.Int64
	httpServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(response, "unavailable", http.StatusServiceUnavailable)
	}))
	defer httpServer.Close()
	root := t.TempDir()
	catalogs, err := extensions.NewStore(extensions.Options{WorkingDir: root, MCP: config.MCP{Servers: []config.MCPServer{{ID: "bad", Transport: config.MCPHTTP, URL: httpServer.URL}}}})
	if err != nil {
		t.Fatal(err)
	}
	warnings := &warning.SliceSink{}
	manager, err := New(Options{Catalogs: catalogs, WorkingDir: root, Warnings: warnings, FailureLimit: 3})
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestResource(t, manager)

	start := make(chan struct{})
	var group sync.WaitGroup
	for index := range 12 {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			_, toolsErr := manager.Tools(newFakeReadonlyContext(t.Context(), "session-"+string(rune('a'+index))))
			if toolsErr != nil {
				t.Errorf("Tools: %v", toolsErr)
			}
		}()
	}
	close(start)
	group.Wait()
	if notices := warnings.Warnings(); len(notices) != 3 {
		t.Fatalf("failure warnings = %d, want 3: %#v", len(notices), notices)
	}
	requestsAtThreshold := requests.Load()
	if _, err := manager.Tools(newFakeReadonlyContext(t.Context(), "later")); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != requestsAtThreshold {
		t.Fatalf("suppressed concurrent server retried: requests = %d, want %d", requests.Load(), requestsAtThreshold)
	}
}

func testFailureCountResetsOnlyAfterSuccessfulCall(t *testing.T) {
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "fail-reset", Version: "1"}, nil)
	server.AddTool(&sdkmcp.Tool{Name: "fail", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		return nil, errors.New("remote failure")
	})
	server.AddTool(&sdkmcp.Tool{Name: "ok", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "ok"}}}, nil
	})
	httpServer := newMCPHTTPServer(t, server)
	manager, catalog := configuredManagerWithOptions(t, config.MCPServer{ID: "reset", Transport: config.MCPHTTP, URL: httpServer.URL}, Options{FailureLimit: 3})
	qualified := "plasmid:configured:reset"
	for range 2 {
		connection, err := manager.connection(t.Context(), "session", qualified, catalog)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := connection.call(t.Context(), "session", "fail", nil); err == nil {
			t.Fatal("remote failure succeeded")
		}
	}
	connection, err := manager.connection(t.Context(), "session", qualified, catalog)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connection.call(t.Context(), "session", "ok", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.call(t.Context(), "session", "fail", nil); err == nil {
		t.Fatal("remote failure after success succeeded")
	}
	if _, err := manager.connection(t.Context(), "session", qualified, catalog); err != nil {
		t.Fatalf("successful call did not reset reconnect failures: %v", err)
	}
}

func testReconnectWaitsForBrokenConnectionTeardown(t *testing.T) {
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "serialized-reconnect", Version: "1"}, nil)
	server.AddTool(&sdkmcp.Tool{Name: "fail", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		return nil, errors.New("remote failure")
	})
	stream := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return server }, nil)
	deleteStarted := make(chan struct{})
	releaseDelete := make(chan struct{})
	var once sync.Once
	httpServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodDelete {
			once.Do(func() { close(deleteStarted) })
			<-releaseDelete
		}
		stream.ServeHTTP(response, request)
	}))
	defer httpServer.Close()
	manager, catalog := configuredManagerWithOptions(t, config.MCPServer{ID: "serialized", Transport: config.MCPHTTP, URL: httpServer.URL}, Options{})
	qualified := "plasmid:configured:serialized"
	connection, err := manager.connection(t.Context(), "first", qualified, catalog)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connection.call(t.Context(), "first", "fail", nil); err == nil {
		t.Fatal("remote failure succeeded")
	}
	select {
	case <-deleteStarted:
	case <-time.After(time.Second):
		t.Fatal("broken connection teardown did not start")
	}
	reconnected := make(chan error, 1)
	go func() {
		_, reconnectErr := manager.connection(context.Background(), "second", qualified, catalog)
		reconnected <- reconnectErr
	}()
	select {
	case err := <-reconnected:
		t.Fatalf("replacement connected before old teardown completed: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseDelete)
	select {
	case err := <-reconnected:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("replacement did not connect after old teardown completed")
	}
}

type toolDiscoveryCase struct {
	name        string
	options     Options
	pageSize    int
	tools       []*sdkmcp.Tool
	wantTools   int
	wantWarning string
}

func TestToolDiscoveryBoundsPagesCountSchemaAndDescription(t *testing.T) {
	tests := []toolDiscoveryCase{
		{
			name: "pages", options: Options{MaxToolPages: 1, MaxTools: 10}, pageSize: 1,
			tools: []*sdkmcp.Tool{{Name: "one", InputSchema: map[string]any{"type": "object"}}, {Name: "two", InputSchema: map[string]any{"type": "object"}}},
		},
		{
			name: "count", options: Options{MaxToolPages: 10, MaxTools: 1},
			tools: []*sdkmcp.Tool{{Name: "one", InputSchema: map[string]any{"type": "object"}}, {Name: "two", InputSchema: map[string]any{"type": "object"}}},
		},
		{
			name: "schema", options: Options{MaxToolSchemaBytes: 64}, wantWarning: warning.WarnMCPToolInvalid,
			tools: []*sdkmcp.Tool{{Name: "large", InputSchema: map[string]any{"type": "object", "description": strings.Repeat("x", 128)}}},
		},
		{
			name: "description", options: Options{MaxToolDescriptionBytes: 17}, wantTools: 1,
			tools: []*sdkmcp.Tool{{Name: "described", Description: strings.Repeat("λ", 32), InputSchema: map[string]any{"type": "object"}}},
		},
		{
			name: "aggregate metadata", options: Options{MaxToolMetadataBytes: 150},
			tools: []*sdkmcp.Tool{
				{Name: "one", Description: strings.Repeat("x", 100), InputSchema: map[string]any{"type": "object"}},
				{Name: "two", Description: strings.Repeat("x", 100), InputSchema: map[string]any{"type": "object"}},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) { runToolDiscoveryCase(t, test) })
	}
}

func runToolDiscoveryCase(t *testing.T, test toolDiscoveryCase) {
	t.Helper()
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: test.name, Version: "1"}, &sdkmcp.ServerOptions{PageSize: test.pageSize})
	for _, remote := range test.tools {
		server.AddTool(remote, func(context.Context, *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
			return &sdkmcp.CallToolResult{}, nil
		})
	}
	httpServer := newMCPHTTPServer(t, server)
	warnings := &warning.SliceSink{}
	test.options.Warnings = warnings
	manager, _ := configuredManagerWithOptions(t, config.MCPServer{ID: test.name, Transport: config.MCPHTTP, URL: httpServer.URL}, test.options)
	tools, err := manager.Tools(newFakeReadonlyContext(t.Context(), "session"))
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != test.wantTools {
		t.Fatalf("tools = %d, want %d", len(tools), test.wantTools)
	}
	if test.name == "description" {
		assertBoundedDescription(t, tools[0].Description())
	}
	if test.wantWarning != "" {
		assertOnlyWarningCode(t, warnings, test.wantWarning)
	}
}

func assertBoundedDescription(t *testing.T, got string) {
	t.Helper()
	if len(got) > 17 || !utf8.ValidString(got) {
		t.Fatalf("description = %q (%d bytes)", got, len(got))
	}
}

func assertOnlyWarningCode(t *testing.T, warnings *warning.SliceSink, want string) {
	t.Helper()
	notices := warnings.Warnings()
	if len(notices) != 1 || notices[0].Code != want {
		t.Fatalf("warnings = %#v", notices)
	}
}

func testToolDiscoveryTimeoutsAndCloseWaitsForList(t *testing.T) {
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "slow", Version: "1"}, &sdkmcp.ServerOptions{HasTools: true})
	listStarted := make(chan struct{})
	server.AddReceivingMiddleware(func(next sdkmcp.MethodHandler) sdkmcp.MethodHandler {
		return func(ctx context.Context, method string, request sdkmcp.Request) (sdkmcp.Result, error) {
			if method == toolsListMethod {
				select {
				case <-listStarted:
				default:
					close(listStarted)
				}
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return next(ctx, method, request)
		}
	})
	httpServer := newMCPHTTPServer(t, server)
	manager, _ := configuredManagerWithOptions(t, config.MCPServer{ID: "slow", Transport: config.MCPHTTP, URL: httpServer.URL}, Options{ListTimeout: 5 * time.Second})
	done := make(chan error, 1)
	go func() {
		_, err := manager.Tools(newFakeReadonlyContext(context.Background(), "slow-session"))
		done <- err
	}()
	select {
	case <-listStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("tools/list did not start")
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Tools returned construction error after close: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close returned before the in-flight tools/list operation")
	}
}

func TestConnectAndListTimeoutsAreDeterministic(t *testing.T) {
	tests := []struct {
		name string
		run  func(*testing.T)
	}{
		{name: "connect", run: testConnectTimeout},
		{name: "list", run: testListTimeout},
	}
	for _, test := range tests {
		t.Run(test.name, test.run)
	}
}

func testConnectTimeout(t *testing.T) {
	started := make(chan struct{})
	var once sync.Once
	httpServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodDelete {
			response.WriteHeader(http.StatusNoContent)
			return
		}
		once.Do(func() { close(started) })
		select {
		case <-request.Context().Done():
		case <-time.After(500 * time.Millisecond):
			http.Error(response, "stalled", http.StatusGatewayTimeout)
		}
	}))
	defer func() {
		httpServer.CloseClientConnections()
		httpServer.Close()
	}()
	manager, catalog := configuredManagerWithOptions(t, config.MCPServer{ID: "connect-timeout", Transport: config.MCPHTTP, URL: httpServer.URL}, Options{ConnectTimeout: 50 * time.Millisecond})
	before := time.Now()
	if _, err := manager.connection(t.Context(), "session", "plasmid:configured:connect-timeout", catalog); err == nil {
		t.Fatal("stalled connect succeeded")
	}
	if elapsed := time.Since(before); elapsed > time.Second {
		t.Fatalf("connect timeout took %s", elapsed)
	}
	select {
	case <-started:
	default:
		t.Fatal("connect request never reached server")
	}
}

func testListTimeout(t *testing.T) {
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "list-timeout", Version: "1"}, &sdkmcp.ServerOptions{HasTools: true})
	server.AddReceivingMiddleware(func(next sdkmcp.MethodHandler) sdkmcp.MethodHandler {
		return func(ctx context.Context, method string, request sdkmcp.Request) (sdkmcp.Result, error) {
			if method == toolsListMethod {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return next(ctx, method, request)
		}
	})
	httpServer := newMCPHTTPServer(t, server)
	manager, catalog := configuredManagerWithOptions(t, config.MCPServer{ID: "list-timeout", Transport: config.MCPHTTP, URL: httpServer.URL}, Options{ListTimeout: 50 * time.Millisecond})
	before := time.Now()
	if _, err := manager.connection(t.Context(), "session", "plasmid:configured:list-timeout", catalog); err == nil {
		t.Fatal("stalled tools/list succeeded")
	}
	if elapsed := time.Since(before); elapsed > time.Second {
		t.Fatalf("list timeout took %s", elapsed)
	}
}

func testCloseTearsDownConnectionsConcurrently(t *testing.T) {
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "close", Version: "1"}, nil)
	server.AddTool(&sdkmcp.Tool{Name: "noop", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		return &sdkmcp.CallToolResult{}, nil
	})
	stream := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return server }, nil)
	var deletes atomic.Int64
	httpServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodDelete {
			deletes.Add(1)
			time.Sleep(150 * time.Millisecond)
		}
		stream.ServeHTTP(response, request)
	}))
	defer httpServer.Close()
	root := t.TempDir()
	servers := make([]config.MCPServer, 4)
	for index := range servers {
		servers[index] = config.MCPServer{ID: "server-" + string(rune('a'+index)), Transport: config.MCPHTTP, URL: httpServer.URL}
	}
	catalogs, err := extensions.NewStore(extensions.Options{WorkingDir: root, MCP: config.MCP{Servers: servers}})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := New(Options{Catalogs: catalogs, WorkingDir: root, CloseGrace: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	tools, err := manager.Tools(newFakeReadonlyContext(t.Context(), "session"))
	if err != nil || len(tools) != 4 {
		t.Fatalf("Tools = %d, err = %v", len(tools), err)
	}
	before := time.Now()
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(before); elapsed > 400*time.Millisecond {
		t.Fatalf("connection teardown was serialized: %s", elapsed)
	}
	if deletes.Load() != 4 {
		t.Fatalf("DELETE requests = %d, want 4", deletes.Load())
	}
}

func TestToolWireNameCollisionsAreRejectedAndLengthIsValid(t *testing.T) {
	t.Run("same server", testSameServerToolWireCollision)
	t.Run("cross server", testCrossServerToolWireCollision)
	t.Run("length", testToolWireNameLength)
}

func testSameServerToolWireCollision(t *testing.T) {
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "collision", Version: "1"}, nil)
	for _, name := range []string{"a-b", "a_b"} {
		server.AddTool(&sdkmcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}}, func(context.Context, *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
			return &sdkmcp.CallToolResult{}, nil
		})
	}
	httpServer := newMCPHTTPServer(t, server)
	warnings := &warning.SliceSink{}
	manager, _ := configuredManagerWithOptions(t, config.MCPServer{ID: "collision", Transport: config.MCPHTTP, URL: httpServer.URL}, Options{Warnings: warnings})
	tools, err := manager.Tools(newFakeReadonlyContext(t.Context(), "session"))
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 0 {
		t.Fatalf("ambiguous tools exposed: %#v", tools)
	}
	assertWarningCode(t, warnings, warning.WarnMCPToolCollision)
}

func testCrossServerToolWireCollision(t *testing.T) {
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "collision", Version: "1"}, nil)
	server.AddTool(&sdkmcp.Tool{Name: "same", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		return &sdkmcp.CallToolResult{}, nil
	})
	httpServer := newMCPHTTPServer(t, server)
	root := t.TempDir()
	catalogs, err := extensions.NewStore(extensions.Options{WorkingDir: root, MCP: config.MCP{Servers: []config.MCPServer{
		{ID: "a-b", Transport: config.MCPHTTP, URL: httpServer.URL},
		{ID: "a_b", Transport: config.MCPHTTP, URL: httpServer.URL},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	warnings := &warning.SliceSink{}
	manager, err := New(Options{Catalogs: catalogs, WorkingDir: root, Warnings: warnings})
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestResource(t, manager)
	tools, err := manager.Tools(newFakeReadonlyContext(t.Context(), "session"))
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 0 {
		t.Fatalf("cross-server ambiguous tools exposed: %#v", tools)
	}
	assertWarningCode(t, warnings, warning.WarnMCPToolCollision)
}

func testToolWireNameLength(t *testing.T) {
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "length", Version: "1"}, nil)
	server.AddTool(&sdkmcp.Tool{Name: strings.Repeat("a", 200), InputSchema: map[string]any{"type": "object"}}, func(context.Context, *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		return &sdkmcp.CallToolResult{}, nil
	})
	httpServer := newMCPHTTPServer(t, server)
	manager, _ := configuredManagerWithOptions(t, config.MCPServer{ID: "length", Transport: config.MCPHTTP, URL: httpServer.URL}, Options{})
	tools, err := manager.Tools(newFakeReadonlyContext(t.Context(), "session"))
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || len(tools[0].Name()) > 64 {
		t.Fatalf("tool names = %#v", tools)
	}
}

func testCallResultUsesSharedOutputBudgetAndBoundsContentItems(t *testing.T) {
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "results", Version: "1"}, nil)
	server.AddTool(&sdkmcp.Tool{Name: "large", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		return &sdkmcp.CallToolResult{
			Content: []sdkmcp.Content{
				&sdkmcp.TextContent{Text: strings.Repeat("a", 200)},
				&sdkmcp.TextContent{Text: "second"},
				&sdkmcp.TextContent{Text: "third"},
			},
			StructuredContent: map[string]any{"secret_tail": strings.Repeat("z", 200) + "END_SECRET"},
		}, nil
	})
	httpServer := newMCPHTTPServer(t, server)
	budget := outputlimit.NewBudget(128)
	manager, catalog := configuredManagerWithOptions(t, config.MCPServer{ID: "results", Transport: config.MCPHTTP, URL: httpServer.URL}, Options{
		Output: outputlimit.Policy{MaxBytes: 128, MaxLines: 20, MaxLineBytes: 128, HeadFraction: 0.6}, Budget: budget, MaxResultItems: 2,
	})
	connection, err := manager.connection(t.Context(), "session", "plasmid:configured:results", catalog)
	if err != nil {
		t.Fatal(err)
	}
	result, err := connection.call(t.Context(), "session", "large", nil)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if result["truncated"] != true || strings.Contains(string(encoded), "END_SECRET") {
		t.Fatalf("unbounded MCP result = %s", encoded)
	}
	used, limit := budget.Report("session")
	if used != len(encoded) || len(encoded) > 128 || used > limit {
		t.Fatalf("budget = %d/%d, encoded bytes = %d", used, limit, len(encoded))
	}
}

func testCallResultItemCountIsBoundedWithoutByteTruncation(t *testing.T) {
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "item-count", Version: "1"}, nil)
	server.AddTool(&sdkmcp.Tool{Name: "items", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{
			&sdkmcp.TextContent{Text: "one"}, &sdkmcp.TextContent{Text: "two"}, &sdkmcp.TextContent{Text: "three"},
		}}, nil
	})
	httpServer := newMCPHTTPServer(t, server)
	manager, catalog := configuredManagerWithOptions(t, config.MCPServer{ID: "items", Transport: config.MCPHTTP, URL: httpServer.URL}, Options{MaxResultItems: 2})
	connection, err := manager.connection(t.Context(), "session", "plasmid:configured:items", catalog)
	if err != nil {
		t.Fatal(err)
	}
	result, err := connection.call(t.Context(), "session", "items", nil)
	if err != nil {
		t.Fatal(err)
	}
	content, ok := result["content"].([]any)
	if !ok || len(content) != 2 || result["omitted_content_items"] != 1 {
		t.Fatalf("bounded result = %#v", result)
	}
}

func TestInboundMCPMessagesAreBoundedBeforeProjection(t *testing.T) {
	tests := []struct {
		name string
		run  func(*testing.T)
	}{
		{name: "HTTP list", run: testInboundHTTPListBound},
		{name: "HTTP call", run: testInboundHTTPCallBound},
		{name: "stdio call", run: testInboundStdioCallBound},
	}
	for _, test := range tests {
		t.Run(test.name, test.run)
	}
}

func testInboundHTTPListBound(t *testing.T) {
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "large-list", Version: "1"}, nil)
	server.AddTool(&sdkmcp.Tool{
		Name: "large", Description: strings.Repeat("d", 4096), InputSchema: map[string]any{"type": "object"},
	}, func(context.Context, *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		return &sdkmcp.CallToolResult{}, nil
	})
	httpServer := newMCPHTTPServer(t, server)
	manager, _ := configuredManagerWithOptions(t, config.MCPServer{ID: "large-list", Transport: config.MCPHTTP, URL: httpServer.URL}, Options{MaxMessageBytes: 2048})
	tools, err := manager.Tools(newFakeReadonlyContext(t.Context(), "session"))
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 0 {
		t.Fatalf("oversized tool list exposed %d tools", len(tools))
	}
}

func testInboundHTTPCallBound(t *testing.T) {
	server := largeResultServer()
	httpServer := newMCPHTTPServer(t, server)
	manager, catalog := configuredManagerWithOptions(t, config.MCPServer{ID: "large-call", Transport: config.MCPHTTP, URL: httpServer.URL}, Options{MaxMessageBytes: 2048})
	connection, err := manager.connection(t.Context(), "session", "plasmid:configured:large-call", catalog)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connection.call(t.Context(), "session", "large", nil); err == nil {
		t.Fatal("oversized HTTP call response succeeded")
	}
}

func testInboundStdioCallBound(t *testing.T) {
	manager, catalog := configuredManagerWithOptions(t, config.MCPServer{
		ID: "large-stdio", Transport: config.MCPStdio, Command: os.Args[0],
		Args: []string{"-test.run=^TestMCPStdioHelper$"}, Env: map[string]string{"PLASMID_MCP_HELPER": "1"},
	}, Options{MaxMessageBytes: 2048})
	connection, err := manager.connection(t.Context(), "session", "plasmid:configured:large-stdio", catalog)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connection.call(t.Context(), "session", "large", nil); err == nil {
		t.Fatal("oversized stdio call response succeeded")
	}
}

func TestHTTPResponseBodyLimitAcceptsExactBoundaryAndRejectsLargerBody(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr error
	}{
		{name: "exact", value: "1234"},
		{name: "over", value: "12345", wantErr: errMessageTooLarge},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := &boundedResponseBody{source: io.NopCloser(strings.NewReader(test.value)), remaining: 4}
			value, err := io.ReadAll(body)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("ReadAll error = %v, want %v", err, test.wantErr)
			}
			if test.wantErr == nil && string(value) != test.value {
				t.Fatalf("ReadAll = %q, want %q", value, test.value)
			}
		})
	}
}

func testHTTPHeadersNeverCrossConfiguredOriginRedirect(t *testing.T) {
	var leaked atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "" {
			leaked.Store(true)
		}
		http.Error(response, "unexpected redirect target", http.StatusBadRequest)
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.Redirect(response, request, target.URL, http.StatusFound)
	}))
	defer redirect.Close()
	manager, catalog := configuredManager(t, config.MCPServer{ID: "redirect", Transport: config.MCPHTTP, URL: redirect.URL, Headers: map[string]string{"Authorization": "Bearer TOPSECRET"}})
	if _, err := manager.connection(t.Context(), "session", "plasmid:configured:redirect", catalog); err == nil {
		t.Fatal("cross-origin redirect connected")
	}
	if leaked.Load() {
		t.Fatal("configured MCP header leaked across redirect origin")
	}
}

func testManagerFormattingRedactsConnectedTransportSecrets(t *testing.T) {
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "format", Version: "1"}, nil)
	server.AddTool(&sdkmcp.Tool{Name: "fail", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		return nil, errors.New("remote failure TOPSECRET")
	})
	handler := httptest.NewServer(sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return server }, nil))
	defer handler.Close()
	manager, catalog := configuredManager(t, config.MCPServer{
		ID: "secret", Transport: config.MCPHTTP, URL: handler.URL + "/mcp?token=TOPSECRET",
		Headers: map[string]string{"Authorization": "Bearer TOPSECRET"},
	})
	connection, err := manager.connection(t.Context(), "session", "plasmid:configured:secret", catalog)
	if err != nil {
		t.Fatal(err)
	}
	var logged bytes.Buffer
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("manager", "value", manager)
	for name, value := range map[string]string{
		"v": fmt.Sprintf("%v", manager), "plus": fmt.Sprintf("%+v", manager),
		"sharp": fmt.Sprintf("%#v", manager), "slog": logged.String(),
	} {
		if strings.Contains(value, "TOPSECRET") || strings.Contains(value, "Bearer") {
			t.Fatalf("%s formatting leaked transport secret: %s", name, value)
		}
	}
	if _, err := connection.call(t.Context(), "session", "fail", map[string]any{}); err == nil || strings.Contains(err.Error(), "TOPSECRET") || strings.Contains(err.Error(), "token=") {
		t.Fatalf("tool error was not redacted: %v", err)
	}
}

func testHTTPSameOriginRedirectCycleIsBounded(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		http.Redirect(response, request, "/", http.StatusFound)
	}))
	defer server.Close()
	manager, catalog := configuredManager(t, config.MCPServer{ID: "cycle", Transport: config.MCPHTTP, URL: server.URL})
	if _, err := manager.connection(t.Context(), "session", "plasmid:configured:cycle", catalog); err == nil {
		t.Fatal("same-origin redirect cycle connected")
	}
	if requests.Load() == 0 || requests.Load() > 20 {
		t.Fatalf("redirect requests = %d, want a finite maximum of 20", requests.Load())
	}
}

func TestMCPStdioHelper(t *testing.T) {
	if os.Getenv("PLASMID_MCP_HELPER") != "1" {
		t.Skip("stdio helper process")
	}
	if os.Getenv("PLASMID_MCP_DESCENDANT_HELPER") == "1" {
		time.Sleep(30 * time.Second)
		return
	}
	if pidFile := os.Getenv("PLASMID_MCP_DESCENDANT_PID"); pidFile != "" {
		descendant := exec.Command(os.Args[0], "-test.run=^TestMCPStdioHelper$")
		descendant.Env = append(os.Environ(), "PLASMID_MCP_DESCENDANT_HELPER=1")
		descendant.Stdout = os.Stdout
		if err := descendant.Start(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(pidFile, []byte(strconv.Itoa(descendant.Process.Pid)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if os.Getenv("PLASMID_MCP_INVALID_HANDSHAKE") == "1" {
		runInvalidMCPHandshake(t)
		return
	}
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "fake-stdio", Version: "1"}, nil)
	sdkmcp.AddTool(server, &sdkmcp.Tool{Name: "echo", Description: "echo input"}, func(_ context.Context, _ *sdkmcp.CallToolRequest, input echoInput) (*sdkmcp.CallToolResult, echoOutput, error) {
		return nil, echoOutput(input), nil
	})
	server.AddTool(&sdkmcp.Tool{Name: "large", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: strings.Repeat("x", 4096)}}}, nil
	})
	err := server.Run(context.Background(), &sdkmcp.StdioTransport{})
	if marker := os.Getenv("PLASMID_MCP_MARKER"); marker != "" {
		_ = os.WriteFile(marker, []byte("closed"), 0o600)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func runInvalidMCPHandshake(t *testing.T) {
	t.Helper()
	reader := bufio.NewReader(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}
		var request map[string]any
		if err := json.Unmarshal(line, &request); err != nil {
			t.Fatal(err)
		}
		method, _ := request["method"].(string)
		if method == "notifications/initialized" {
			continue
		}
		response := map[string]any{"jsonrpc": "2.0", "id": request["id"]}
		if method == "initialize" {
			response["result"] = map[string]any{
				"protocolVersion": "unsupported", "capabilities": map[string]any{},
				"serverInfo": map[string]any{"name": "invalid", "version": "1"},
			}
		} else {
			response["error"] = map[string]any{"code": -32601, "message": "method not found"}
		}
		if err := encoder.Encode(response); err != nil {
			return
		}
	}
}

func largeResultServer() *sdkmcp.Server {
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "large-result", Version: "1"}, nil)
	server.AddTool(&sdkmcp.Tool{Name: "large", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: strings.Repeat("x", 4096)}}}, nil
	})
	return server
}

func configuredManager(t *testing.T, server config.MCPServer) (*Manager, extensions.Catalog) {
	return configuredManagerWithOptions(t, server, Options{})
}

func configuredManagerWithOptions(t *testing.T, server config.MCPServer, options Options) (*Manager, extensions.Catalog) {
	t.Helper()
	root := t.TempDir()
	catalogs, err := extensions.NewStore(extensions.Options{WorkingDir: root, MCP: config.MCP{Servers: []config.MCPServer{server}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := catalogs.StartSession(t.Context(), "session"); err != nil {
		t.Fatal(err)
	}
	catalog, ok := catalogs.Snapshot("session")
	if !ok {
		t.Fatal("missing catalog")
	}
	options.Catalogs = catalogs
	options.WorkingDir = root
	manager, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	if manager.Name() != "mcp" {
		t.Fatalf("Manager.Name() = %q, want %q", manager.Name(), "mcp")
	}
	t.Cleanup(func() { _ = manager.Close() })
	return manager, catalog
}

func newMCPHTTPServer(t *testing.T, server *sdkmcp.Server) *httptest.Server {
	t.Helper()
	stream := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return server }, nil)
	httpServer := httptest.NewServer(stream)
	t.Cleanup(httpServer.Close)
	return httpServer
}

func assertWarningCode(t *testing.T, sink *warning.SliceSink, code string) {
	t.Helper()
	for _, notice := range sink.Warnings() {
		if notice.Code == code {
			return
		}
	}
	t.Fatalf("warning %q not found in %#v", code, sink.Warnings())
}

type fakeReadonlyContext struct {
	agent.StrictContextMock
	sessionID string
}

func newFakeReadonlyContext(ctx context.Context, sessionID string) *fakeReadonlyContext {
	return &fakeReadonlyContext{StrictContextMock: agent.NewStrictContextMock(ctx), sessionID: sessionID}
}

func (fakeReadonlyContext) UserContent() *genai.Content          { return nil }
func (fakeReadonlyContext) InvocationID() string                 { return "invocation" }
func (fakeReadonlyContext) AgentName() string                    { return "agent" }
func (fakeReadonlyContext) ReadonlyState() session.ReadonlyState { return nil }
func (fakeReadonlyContext) UserID() string                       { return "user" }
func (fakeReadonlyContext) AppName() string                      { return "app" }
func (c fakeReadonlyContext) SessionID() string                  { return c.sessionID }
func (fakeReadonlyContext) Branch() string                       { return "" }
