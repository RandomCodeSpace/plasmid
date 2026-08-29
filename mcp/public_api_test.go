package mcp_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"

	"github.com/RandomCodeSpace/plasmid/config"
	"github.com/RandomCodeSpace/plasmid/extensions"
	plasmidmcp "github.com/RandomCodeSpace/plasmid/mcp"
	"github.com/RandomCodeSpace/plasmid/outputlimit"
	"github.com/RandomCodeSpace/plasmid/warning"
)

const toolsListMethod = "tools/list"

func TestManagerPublicLifecycleValidatesAndCloses(t *testing.T) {
	var nilContext context.Context
	var nilManager *plasmidmcp.Manager
	if err := nilManager.Close(); err != nil {
		t.Fatalf("nil Close() = %v", err)
	}
	if err := nilManager.DropSession(t.Context(), "session"); err == nil {
		t.Fatal("nil DropSession() succeeded")
	}

	manager, catalogs := newPublicManager(t, nil, plasmidmcp.Options{})
	if err := manager.DropSession(nilContext, "session"); err == nil {
		t.Fatal("DropSession(nil context) succeeded")
	}
	if err := manager.DropSession(t.Context(), ""); err == nil {
		t.Fatal("DropSession(empty session) succeeded")
	}
	if err := manager.DropSession(t.Context(), "missing"); err != nil {
		t.Fatalf("DropSession(missing) = %v", err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(); err != nil {
		t.Fatalf("second Close() = %v", err)
	}
	ctx := newMCPReadonlyContext(t, "closed")
	if tools, err := manager.Tools(ctx); err != nil || len(tools) != 0 {
		t.Fatalf("Tools(closed) = %#v, %v", tools, err)
	}
	if err := manager.DropSession(t.Context(), "closed"); err == nil {
		t.Fatal("DropSession(closed manager) succeeded")
	}

	catalogs.Close()
	if _, err := manager.Tools(ctx); err == nil {
		t.Fatal("Tools(closed catalog) succeeded")
	}
}

func TestManagerPublicToolsReuseConnection(t *testing.T) {
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "reuse", Version: "1"}, nil)
	sdkmcp.AddTool(server, &sdkmcp.Tool{Name: "echo", Description: "echo input"}, func(_ context.Context, _ *sdkmcp.CallToolRequest, input publicEchoInput) (*sdkmcp.CallToolResult, publicEchoOutput, error) {
		return nil, publicEchoOutput(input), nil
	})
	httpServer := newPublicMCPServer(t, server)
	manager, _ := newPublicManager(t, []config.MCPServer{{ID: "reuse", Transport: config.MCPHTTP, URL: httpServer.URL}}, plasmidmcp.Options{})
	ctx := newMCPReadonlyContext(t, "session")
	for range 2 {
		tools, err := manager.Tools(ctx)
		if err != nil || len(tools) != 1 || tools[0].Name() != "mcp__plasmid_configured_reuse__echo" {
			t.Fatalf("Tools() = %#v, %v", tools, err)
		}
	}
}

func TestManagerPublicToolsSuppressRepeatedActivationFailure(t *testing.T) {
	warnings := &warning.SliceSink{}
	manager, _ := newPublicManager(t, []config.MCPServer{{
		ID: "offline", Transport: config.MCPHTTP, URL: "http://127.0.0.1:1",
	}}, plasmidmcp.Options{
		Warnings: warnings, FailureLimit: 1,
		ConnectTimeout: 50 * time.Millisecond, ListTimeout: 50 * time.Millisecond,
	})
	ctx := newMCPReadonlyContext(t, "session")
	for range 2 {
		tools, err := manager.Tools(ctx)
		if err != nil || len(tools) != 0 {
			t.Fatalf("Tools() = %#v, %v", tools, err)
		}
	}
	got := warnings.Warnings()
	if len(got) != 1 || got[0].Code != warning.WarnMCPConnectFailed {
		t.Fatalf("warnings = %#v, want one activation failure", got)
	}
}

func TestManagerPublicToolsBlockThirdWireCollision(t *testing.T) {
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "collision", Version: "1"}, nil)
	server.AddTool(&sdkmcp.Tool{Name: "same", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		return &sdkmcp.CallToolResult{}, nil
	})
	httpServer := newPublicMCPServer(t, server)
	servers := []config.MCPServer{
		{ID: "a-b", Transport: config.MCPHTTP, URL: httpServer.URL},
		{ID: "a_b", Transport: config.MCPHTTP, URL: httpServer.URL},
		{ID: "a b", Transport: config.MCPHTTP, URL: httpServer.URL},
	}
	warnings := &warning.SliceSink{}
	manager, _ := newPublicManager(t, servers, plasmidmcp.Options{Warnings: warnings})
	tools, err := manager.Tools(newMCPReadonlyContext(t, "session"))
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 0 {
		t.Fatalf("colliding tools exposed: %#v", tools)
	}
	got := warnings.Warnings()
	if len(got) != 1 || got[0].Code != warning.WarnMCPToolCollision {
		t.Fatalf("warnings = %#v, want one collision", got)
	}
}

func TestManagerPublicToolRunAndStaleToolRejection(t *testing.T) {
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "run", Version: "1"}, nil)
	sdkmcp.AddTool(server, &sdkmcp.Tool{Name: "echo", Description: "echo input"}, func(_ context.Context, _ *sdkmcp.CallToolRequest, input publicEchoInput) (*sdkmcp.CallToolResult, publicEchoOutput, error) {
		return nil, publicEchoOutput(input), nil
	})
	httpServer := newPublicMCPServer(t, server)
	manager, _ := newPublicManager(t, []config.MCPServer{{ID: "run", Transport: config.MCPHTTP, URL: httpServer.URL}}, plasmidmcp.Options{})
	ctx := newMCPReadonlyContext(t, "session")
	tools, err := manager.Tools(ctx)
	if err != nil || len(tools) != 1 {
		t.Fatalf("Tools() = %#v, %v", tools, err)
	}
	runnable, ok := tools[0].(interface {
		Run(agent.Context, any) (map[string]any, error)
	})
	if !ok {
		t.Fatalf("tool %T does not expose native Run", tools[0])
	}
	result, err := runnable.Run(ctx, map[string]any{"value": "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if result["is_error"] != false {
		t.Fatalf("Run() result = %#v", result)
	}
	if err := manager.DropSession(t.Context(), "session"); err != nil {
		t.Fatal(err)
	}
	if _, err := runnable.Run(ctx, map[string]any{"value": "stale"}); err == nil {
		t.Fatal("stale tool Run() succeeded after DropSession")
	}
}

func TestManagerPublicToolRunRejectsExhaustedOutputBudget(t *testing.T) {
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "budget", Version: "1"}, nil)
	sdkmcp.AddTool(server, &sdkmcp.Tool{Name: "echo", Description: "echo input"}, func(_ context.Context, _ *sdkmcp.CallToolRequest, input publicEchoInput) (*sdkmcp.CallToolResult, publicEchoOutput, error) {
		return nil, publicEchoOutput(input), nil
	})
	httpServer := newPublicMCPServer(t, server)
	budget := outputlimit.NewBudget(1)
	reservation := budget.Reserve("session", 1)
	budget.Consume("session", reservation.ID, reservation.Grant)
	manager, _ := newPublicManager(t, []config.MCPServer{{ID: "budget", Transport: config.MCPHTTP, URL: httpServer.URL}}, plasmidmcp.Options{Budget: budget})
	ctx := newMCPReadonlyContext(t, "session")
	tools, err := manager.Tools(ctx)
	if err != nil || len(tools) != 1 {
		t.Fatalf("Tools() = %#v, %v", tools, err)
	}
	runnable := tools[0].(interface {
		Run(agent.Context, any) (map[string]any, error)
	})
	if _, err := runnable.Run(ctx, map[string]any{"value": "hello"}); err == nil {
		t.Fatal("Run() succeeded with exhausted output budget")
	}
}

func TestManagerPublicDiscoveryHandlesSparseAndInvalidTools(t *testing.T) {
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "sparse", Version: "1"}, &sdkmcp.ServerOptions{HasTools: true})
	server.AddReceivingMiddleware(func(next sdkmcp.MethodHandler) sdkmcp.MethodHandler {
		return func(ctx context.Context, method string, request sdkmcp.Request) (sdkmcp.Result, error) {
			if method != toolsListMethod {
				return next(ctx, method, request)
			}
			return &sdkmcp.ListToolsResult{Tools: []*sdkmcp.Tool{
				{Name: ""},
				{Name: "schema-less", Description: "usable without a schema"},
				{Name: "invalid-schema", InputSchema: "not-an-object"},
			}}, nil
		}
	})
	httpServer := newPublicMCPServer(t, server)
	warnings := &warning.SliceSink{}
	manager, _ := newPublicManager(t, []config.MCPServer{{ID: "sparse", Transport: config.MCPHTTP, URL: httpServer.URL}}, plasmidmcp.Options{Warnings: warnings})
	tools, err := manager.Tools(newMCPReadonlyContext(t, "session"))
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0].Name() != "mcp__plasmid_configured_sparse__schema_less" {
		t.Fatalf("Tools() = %#v", tools)
	}
	assertPublicWarning(t, warnings, warning.WarnMCPToolInvalid)
}

func TestManagerPublicDiscoveryRejectsRepeatedCursor(t *testing.T) {
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "cursor", Version: "1"}, &sdkmcp.ServerOptions{HasTools: true})
	server.AddReceivingMiddleware(func(next sdkmcp.MethodHandler) sdkmcp.MethodHandler {
		return func(ctx context.Context, method string, request sdkmcp.Request) (sdkmcp.Result, error) {
			if method != toolsListMethod {
				return next(ctx, method, request)
			}
			return &sdkmcp.ListToolsResult{NextCursor: "same", Tools: []*sdkmcp.Tool{}}, nil
		}
	})
	httpServer := newPublicMCPServer(t, server)
	warnings := &warning.SliceSink{}
	manager, _ := newPublicManager(t, []config.MCPServer{{ID: "cursor", Transport: config.MCPHTTP, URL: httpServer.URL}}, plasmidmcp.Options{Warnings: warnings})
	tools, err := manager.Tools(newMCPReadonlyContext(t, "session"))
	if err != nil || len(tools) != 0 {
		t.Fatalf("Tools() = %#v, %v", tools, err)
	}
	assertPublicWarning(t, warnings, warning.WarnMCPConnectFailed)
}

func TestManagerPublicInvalidTransportConfigurationsFailSoft(t *testing.T) {
	warnings := &warning.SliceSink{}
	servers := []config.MCPServer{
		{ID: "unsupported", Transport: config.MCPTransport("invalid")},
		{ID: "bad-url", Transport: config.MCPHTTP, URL: "://bad"},
		{ID: "missing-command", Transport: config.MCPStdio, Command: "/plasmid-does-not-exist"},
	}
	manager, _ := newPublicManager(t, servers, plasmidmcp.Options{Warnings: warnings, ConnectTimeout: 100 * time.Millisecond})
	tools, err := manager.Tools(newMCPReadonlyContext(t, "session"))
	if err != nil || len(tools) != 0 {
		t.Fatalf("Tools() = %#v, %v", tools, err)
	}
	got := warnings.Warnings()
	if len(got) != len(servers) {
		t.Fatalf("warnings = %#v, want %d activation failures", got, len(servers))
	}
	for _, item := range got {
		if item.Code != warning.WarnMCPConnectFailed {
			t.Fatalf("warning = %#v", item)
		}
	}
}

func TestManagerPublicStdioFrameLimitRejectsHandshake(t *testing.T) {
	warnings := &warning.SliceSink{}
	manager, _ := newPublicManager(t, []config.MCPServer{{
		ID: "small-frame", Transport: config.MCPStdio, Command: os.Args[0],
		Args: []string{"-test.run=^TestMCPStdioHelper$"}, Env: map[string]string{"PLASMID_MCP_HELPER": "1"},
	}}, plasmidmcp.Options{
		Warnings: warnings, MaxMessageBytes: 64,
		ConnectTimeout: time.Second, ListTimeout: time.Second,
	})
	tools, err := manager.Tools(newMCPReadonlyContext(t, "session"))
	if err != nil || len(tools) != 0 {
		t.Fatalf("Tools() = %#v, %v", tools, err)
	}
	assertPublicWarning(t, warnings, warning.WarnMCPConnectFailed)
}

func TestManagerPublicStdioReadFailuresFailSoft(t *testing.T) {
	for _, mode := range []string{"eof", "invalid", "blank", "silent"} {
		t.Run(mode, func(t *testing.T) {
			warnings := &warning.SliceSink{}
			manager, _ := newPublicManager(t, []config.MCPServer{{
				ID: mode, Transport: config.MCPStdio, Command: os.Args[0],
				Args: []string{"-test.run=^TestPublicMCPStdioHelper$"},
				Env:  map[string]string{"PLASMID_PUBLIC_MCP_HELPER": mode},
			}}, plasmidmcp.Options{
				Warnings: warnings, ConnectTimeout: 200 * time.Millisecond, ListTimeout: 200 * time.Millisecond,
			})
			tools, err := manager.Tools(newMCPReadonlyContext(t, "session"))
			if err != nil || len(tools) != 0 {
				t.Fatalf("Tools() = %#v, %v", tools, err)
			}
			assertPublicWarning(t, warnings, warning.WarnMCPConnectFailed)
		})
	}
}

func TestManagerPublicClosedManagerRejectsConfiguredTools(t *testing.T) {
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "closed", Version: "1"}, nil)
	httpServer := newPublicMCPServer(t, server)
	manager, _ := newPublicManager(t, []config.MCPServer{{ID: "closed", Transport: config.MCPHTTP, URL: httpServer.URL}}, plasmidmcp.Options{})
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	tools, err := manager.Tools(newMCPReadonlyContext(t, "session"))
	if err != nil || len(tools) != 0 {
		t.Fatalf("Tools() = %#v, %v", tools, err)
	}
}

func TestManagerPublicRemoteCollisionWarningIsDeduplicated(t *testing.T) {
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "remote-collision", Version: "1"}, nil)
	for _, name := range []string{"a-b", "a_b", "a b"} {
		server.AddTool(&sdkmcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}}, func(context.Context, *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
			return &sdkmcp.CallToolResult{}, nil
		})
	}
	httpServer := newPublicMCPServer(t, server)
	warnings := &warning.SliceSink{}
	manager, _ := newPublicManager(t, []config.MCPServer{
		{ID: "a-b", Transport: config.MCPHTTP, URL: httpServer.URL},
		{ID: "a_b", Transport: config.MCPHTTP, URL: httpServer.URL},
	}, plasmidmcp.Options{Warnings: warnings})
	tools, err := manager.Tools(newMCPReadonlyContext(t, "session"))
	if err != nil || len(tools) != 0 {
		t.Fatalf("Tools() = %#v, %v", tools, err)
	}
	got := warnings.Warnings()
	if len(got) != 1 || got[0].Code != warning.WarnMCPToolCollision {
		t.Fatalf("warnings = %#v", got)
	}
}

func TestManagerPublicAmbiguousConfiguredServerFailsSoft(t *testing.T) {
	warnings := &warning.SliceSink{}
	duplicate := config.MCPServer{ID: "duplicate", Transport: config.MCPHTTP, URL: "http://127.0.0.1:1"}
	manager, _ := newPublicManager(t, []config.MCPServer{duplicate, duplicate}, plasmidmcp.Options{Warnings: warnings})
	tools, err := manager.Tools(newMCPReadonlyContext(t, "session"))
	if err != nil || len(tools) != 0 {
		t.Fatalf("Tools() = %#v, %v", tools, err)
	}
	assertPublicWarning(t, warnings, warning.WarnMCPConnectFailed)
}

func TestManagerPublicErrorResultPreservesErrorFlagWhenBounded(t *testing.T) {
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "error-result", Version: "1"}, nil)
	server.AddTool(&sdkmcp.Tool{Name: "fail", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		return &sdkmcp.CallToolResult{
			IsError: true,
			Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: strings.Repeat("failure ", 100)}},
		}, nil
	})
	httpServer := newPublicMCPServer(t, server)
	manager, _ := newPublicManager(t, []config.MCPServer{{ID: "error-result", Transport: config.MCPHTTP, URL: httpServer.URL}}, plasmidmcp.Options{
		Output: outputlimit.Policy{MaxBytes: 512, MaxLines: 20, MaxLineBytes: 512, HeadFraction: 0.6},
		Budget: outputlimit.NewBudget(512),
	})
	ctx := newMCPReadonlyContext(t, "session")
	tools, err := manager.Tools(ctx)
	if err != nil || len(tools) != 1 {
		t.Fatalf("Tools() = %#v, %v", tools, err)
	}
	runnable := tools[0].(interface {
		Run(agent.Context, any) (map[string]any, error)
	})
	result, err := runnable.Run(ctx, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if result["truncated"] != true || result["is_error"] != true {
		t.Fatalf("bounded error result = %#v", result)
	}
}

func TestManagerPublicCloseOrdersMultipleSessions(t *testing.T) {
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "sessions", Version: "1"}, nil)
	httpServer := newPublicMCPServer(t, server)
	manager, _ := newPublicManager(t, []config.MCPServer{{ID: "sessions", Transport: config.MCPHTTP, URL: httpServer.URL}}, plasmidmcp.Options{})
	for _, sessionID := range []string{"a", "b"} {
		if _, err := manager.Tools(newMCPReadonlyContext(t, sessionID)); err != nil {
			t.Fatal(err)
		}
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestManagerPublicCloseRejectsQueuedConnection(t *testing.T) {
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "queued", Version: "1"}, &sdkmcp.ServerOptions{HasTools: true})
	listStarted := make(chan struct{})
	releaseList := make(chan struct{})
	var startOnce sync.Once
	server.AddReceivingMiddleware(func(next sdkmcp.MethodHandler) sdkmcp.MethodHandler {
		return func(ctx context.Context, method string, request sdkmcp.Request) (sdkmcp.Result, error) {
			if method != toolsListMethod {
				return next(ctx, method, request)
			}
			startOnce.Do(func() { close(listStarted) })
			<-releaseList
			return &sdkmcp.ListToolsResult{Tools: []*sdkmcp.Tool{}}, nil
		}
	})
	httpServer := newPublicMCPServer(t, server)
	manager, _ := newPublicManager(t, []config.MCPServer{{ID: "queued", Transport: config.MCPHTTP, URL: httpServer.URL}}, plasmidmcp.Options{CloseGrace: time.Second})
	ctx := newMCPReadonlyContext(t, "session")
	firstDone := make(chan struct{})
	go func() {
		_, _ = manager.Tools(ctx)
		close(firstDone)
	}()
	select {
	case <-listStarted:
	case <-time.After(time.Second):
		t.Fatal("first tools/list did not start")
	}
	secondDone := make(chan struct{})
	go func() {
		_, _ = manager.Tools(ctx)
		close(secondDone)
	}()
	select {
	case <-secondDone:
		t.Fatal("queued Tools() returned before the first connection released its server lock")
	case <-time.After(20 * time.Millisecond):
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- manager.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close() did not reject the queued connection")
	}
	close(releaseList)
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("first Tools() did not return")
	}
	select {
	case <-secondDone:
	case <-time.After(time.Second):
		t.Fatal("queued Tools() did not return")
	}
}

func TestPublicMCPStdioHelper(t *testing.T) {
	switch os.Getenv("PLASMID_PUBLIC_MCP_HELPER") {
	case "eof":
		os.Exit(0)
	case "invalid":
		_, _ = fmt.Fprintln(os.Stdout, "{not-json")
		os.Exit(0)
	case "blank":
		_, _ = os.Stdout.Write([]byte("\r\n"))
		os.Exit(0)
	case "silent":
		time.Sleep(30 * time.Second)
		os.Exit(0)
	default:
		t.Skip("stdio helper process")
	}
}

type publicEchoInput struct {
	Value string `json:"value"`
}

type publicEchoOutput struct {
	Value string `json:"value"`
}

type mcpReadonlyContext struct {
	agent.StrictContextMock
	sessionID string
}

func newMCPReadonlyContext(t *testing.T, sessionID string) *mcpReadonlyContext {
	t.Helper()
	return &mcpReadonlyContext{StrictContextMock: agent.NewStrictContextMock(t.Context()), sessionID: sessionID}
}

func (*mcpReadonlyContext) UserContent() *genai.Content          { return nil }
func (*mcpReadonlyContext) InvocationID() string                 { return "invocation" }
func (*mcpReadonlyContext) AgentName() string                    { return "agent" }
func (*mcpReadonlyContext) ReadonlyState() session.ReadonlyState { return nil }
func (*mcpReadonlyContext) UserID() string                       { return "user" }
func (*mcpReadonlyContext) AppName() string                      { return "app" }
func (c *mcpReadonlyContext) SessionID() string                  { return c.sessionID }
func (*mcpReadonlyContext) Branch() string                       { return "" }
func (*mcpReadonlyContext) ToolConfirmation() *toolconfirmation.ToolConfirmation {
	return nil
}

func newPublicManager(t *testing.T, servers []config.MCPServer, options plasmidmcp.Options) (*plasmidmcp.Manager, *extensions.Store) {
	t.Helper()
	root := t.TempDir()
	catalogs, err := extensions.NewStore(extensions.Options{WorkingDir: root, MCP: config.MCP{Servers: servers}})
	if err != nil {
		t.Fatal(err)
	}
	options.Catalogs = catalogs
	options.WorkingDir = root
	manager, err := plasmidmcp.New(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = manager.Close()
		catalogs.Close()
	})
	return manager, catalogs
}

func newPublicMCPServer(t *testing.T, server *sdkmcp.Server) *httptest.Server {
	t.Helper()
	handler := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return server }, nil)
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)
	return httpServer
}

func assertPublicWarning(t *testing.T, sink *warning.SliceSink, code string) {
	t.Helper()
	for _, item := range sink.Warnings() {
		if item.Code == code {
			return
		}
	}
	t.Fatalf("warning %q not found in %#v", code, sink.Warnings())
}
