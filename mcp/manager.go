// Package mcp owns lifecycle-aware native ADK toolsets over the first-party MCP SDK.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/plasmid-dev/plasmid/config"
	"github.com/plasmid-dev/plasmid/extensions"
	"github.com/plasmid-dev/plasmid/outputlimit"
	"github.com/plasmid-dev/plasmid/warning"
)

const (
	defaultFailureLimit            = 3
	defaultCloseGrace              = 5 * time.Second
	defaultConnectTimeout          = 10 * time.Second
	defaultListTimeout             = 10 * time.Second
	defaultMaxToolPages            = 64
	defaultMaxTools                = 256
	defaultMaxToolSchemaBytes      = 64 << 10
	defaultMaxToolDescriptionBytes = 8 << 10
	defaultMaxToolMetadataBytes    = 1 << 20
	defaultMaxResultItems          = 128
	defaultMaxMessageBytes         = 1 << 20
	maxWireToolNameBytes           = 64
)

var (
	errSuppressed       = errors.New("MCP server reconnect is suppressed after repeated failures")
	errMessageTooLarge  = errors.New("MCP response exceeds the configured message limit")
	errActivationFailed = errors.New("MCP server activation failed")
	errToolCallFailed   = errors.New("MCP tool call failed")
	errCloseFailed      = errors.New("MCP connection close failed")
)

// Options configures one Harness-owned MCP manager.
type Options struct {
	Catalogs                *extensions.Store
	WorkingDir              string
	Warnings                warning.Sink
	FailureLimit            int
	ConnectTimeout          time.Duration
	ListTimeout             time.Duration
	CloseGrace              time.Duration
	MaxToolPages            int
	MaxTools                int
	MaxToolSchemaBytes      int
	MaxToolDescriptionBytes int
	MaxToolMetadataBytes    int
	MaxResultItems          int
	MaxMessageBytes         int
	Output                  outputlimit.Policy
	Budget                  *outputlimit.Budget
}

// Manager is a native ADK toolset and the sole owner of MCP client sessions,
// transports, child processes, HTTP clients, and active-call cancellation.
type Manager struct {
	options Options
	client  *sdkmcp.Client
	root    context.Context
	cancel  context.CancelFunc

	mu          sync.Mutex
	closed      bool
	connections map[connectionKey]*connection
	locks       map[string]*sync.Mutex
	failures    map[string]int
	suppressed  map[string]bool
	collisions  map[string]bool
	active      sync.WaitGroup
	operations  sync.WaitGroup
	teardowns   sync.WaitGroup
	closeDone   chan struct{}
	closeErr    error
}

// Format prevents secret-bearing transport configuration from appearing in
// diagnostic formatting.
func (*Manager) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, "mcp.Manager{redacted}")
}

// LogValue prevents structured logging from reflecting transport internals.
func (*Manager) LogValue() slog.Value { return slog.StringValue("mcp.Manager{redacted}") }

type connection struct {
	key        connectionKey
	serverName string
	session    *sdkmcp.ClientSession
	tools      []tool.Tool
	httpClient *http.Client
	httpWire   *headerTransport
	transport  *ownedTransport
	manager    *Manager
	root       context.Context
	cancel     context.CancelFunc
	callMu     sync.Mutex
	closing    bool
	active     sync.WaitGroup
	closeOnce  sync.Once
	closeErr   error
}

type connectionKey struct {
	sessionID string
	server    string
}

// New constructs an inert manager. No server is contacted until Tools is called.
func New(options Options) (*Manager, error) {
	if options.Catalogs == nil || options.WorkingDir == "" {
		return nil, errors.New("construct MCP manager: catalogs and working directory are required")
	}
	if options.Warnings == nil {
		options.Warnings = warning.SlogSink{}
	}
	if options.FailureLimit <= 0 {
		options.FailureLimit = defaultFailureLimit
	}
	if options.ConnectTimeout <= 0 {
		options.ConnectTimeout = defaultConnectTimeout
	}
	if options.ListTimeout <= 0 {
		options.ListTimeout = defaultListTimeout
	}
	if options.CloseGrace <= 0 {
		options.CloseGrace = defaultCloseGrace
	}
	if options.MaxToolPages <= 0 {
		options.MaxToolPages = defaultMaxToolPages
	}
	if options.MaxTools <= 0 {
		options.MaxTools = defaultMaxTools
	}
	if options.MaxToolSchemaBytes <= 0 {
		options.MaxToolSchemaBytes = defaultMaxToolSchemaBytes
	}
	if options.MaxToolDescriptionBytes <= 0 {
		options.MaxToolDescriptionBytes = defaultMaxToolDescriptionBytes
	}
	if options.MaxToolMetadataBytes <= 0 {
		options.MaxToolMetadataBytes = defaultMaxToolMetadataBytes
	}
	if options.MaxResultItems <= 0 {
		options.MaxResultItems = defaultMaxResultItems
	}
	if options.MaxMessageBytes <= 0 {
		options.MaxMessageBytes = defaultMaxMessageBytes
	}
	if options.Output == (outputlimit.Policy{}) {
		options.Output = outputlimit.Defaults()
	}
	if options.Budget == nil {
		options.Budget = outputlimit.NewBudget(outputlimit.DefaultPerSession)
	}
	root, cancel := context.WithCancel(context.Background())
	return &Manager{
		options: options,
		client:  sdkmcp.NewClient(&sdkmcp.Implementation{Name: "plasmid", Version: "1"}, nil),
		root:    root, cancel: cancel, connections: make(map[connectionKey]*connection), locks: make(map[string]*sync.Mutex),
		failures: make(map[string]int), suppressed: make(map[string]bool), collisions: make(map[string]bool),
		closeDone: make(chan struct{}),
	}, nil
}

// Name implements native ADK tool.Toolset.
func (*Manager) Name() string { return "mcp" }

// Tools lazily connects only exact consent-gated servers for this session.
func (m *Manager) Tools(ctx agent.ReadonlyContext) ([]tool.Tool, error) {
	if err := m.options.Catalogs.StartSession(ctx, ctx.SessionID()); err != nil {
		return nil, err
	}
	catalog, ok := m.options.Catalogs.Snapshot(ctx.SessionID())
	if !ok {
		return nil, errors.New("MCP catalog snapshot is unavailable")
	}
	var result []tool.Tool
	indices := make(map[string]int)
	blocked := make(map[string]bool)
	for _, name := range catalog.AllowedMCPNames() {
		connection, err := m.connection(ctx, ctx.SessionID(), name, catalog)
		if err != nil {
			continue
		}
		for _, candidate := range connection.tools {
			wireName := candidate.Name()
			if blocked[wireName] {
				continue
			}
			if index, exists := indices[wireName]; exists {
				result[index] = nil
				delete(indices, wireName)
				blocked[wireName] = true
				m.warnCollision(wireName)
				continue
			}
			indices[wireName] = len(result)
			result = append(result, candidate)
		}
	}
	result = compactTools(result)
	sort.SliceStable(result, func(i, j int) bool { return result[i].Name() < result[j].Name() })
	return result, nil
}

func (m *Manager) connection(ctx context.Context, sessionID, name string, catalog extensions.Catalog) (*connection, error) {
	if err := m.beginOperation(); err != nil {
		return nil, err
	}
	defer m.operations.Done()
	key := connectionKey{sessionID: sessionID, server: name}
	lock := m.keyLock(name)
	lock.Lock()
	defer lock.Unlock()
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, errors.New("MCP manager is closed")
	}
	if existing := m.connections[key]; existing != nil {
		m.mu.Unlock()
		return existing, nil
	}
	if m.suppressed[name] {
		m.mu.Unlock()
		return nil, errSuppressed
	}
	m.mu.Unlock()
	server, err := catalog.ResolveMCP(name)
	if err != nil {
		m.fail(name, err)
		return nil, err
	}
	connectionContext, stop := linkedContext(ctx, m.root)
	defer stop()
	connected, err := m.connect(connectionContext, key, name, server)
	if err != nil {
		err = redactedRuntimeError(errActivationFailed, ctx.Err(), m.root.Err())
		if ctx.Err() == nil && m.root.Err() == nil {
			m.fail(name, err)
		}
		return nil, err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		_ = connected.close()
		return nil, errors.New("MCP manager closed during connection")
	}
	m.connections[key] = connected
	m.mu.Unlock()
	return connected, nil
}

func (m *Manager) connect(ctx context.Context, key connectionKey, qualified string, server config.MCPServer) (*connection, error) {
	var transport sdkmcp.Transport
	var httpClient *http.Client
	switch server.Transport {
	case config.MCPStdio:
		command := exec.Command(server.Command, server.Args...)
		command.Dir = m.options.WorkingDir
		command.Env = mergeEnvironment(os.Environ(), server.Env)
		commandTransport, err := newCommandTransport(command, int64(m.options.MaxMessageBytes))
		if err != nil {
			return nil, err
		}
		transport = commandTransport
	case config.MCPHTTP:
		endpoint, err := url.Parse(server.URL)
		if err != nil {
			return nil, err
		}
		closeTimeout := m.options.CloseGrace / 2
		if closeTimeout <= 0 {
			closeTimeout = m.options.CloseGrace
		}
		httpWire := &headerTransport{
			base: cloneDefaultHTTPTransport(), headers: cloneStrings(server.Headers), scheme: endpoint.Scheme, host: endpoint.Host,
			closeTimeout: closeTimeout, maxResponseBytes: int64(m.options.MaxMessageBytes),
			active: make(map[uint64]context.CancelFunc),
		}
		httpClient = &http.Client{
			Timeout:   m.options.ConnectTimeout,
			Transport: httpWire,
			CheckRedirect: func(request *http.Request, via []*http.Request) error {
				if len(via) >= 10 {
					return errors.New("MCP redirect limit exceeded")
				}
				if request.URL.Scheme != endpoint.Scheme || request.URL.Host != endpoint.Host {
					return errors.New("MCP redirect crossed the configured origin")
				}
				return nil
			},
		}
		transport = &sdkmcp.StreamableClientTransport{
			Endpoint: server.URL, HTTPClient: httpClient, MaxRetries: -1, DisableStandaloneSSE: true,
		}
	default:
		return nil, fmt.Errorf("unsupported MCP transport %q", server.Transport)
	}
	owned := &ownedTransport{base: transport}
	connectContext, cancelConnect := context.WithTimeout(ctx, m.options.ConnectTimeout)
	session, err := m.client.Connect(connectContext, owned, nil)
	cancelConnect()
	if err != nil {
		_ = owned.Close()
		if httpClient != nil {
			httpClient.CloseIdleConnections()
		}
		return nil, err
	}
	connectionRoot, cancelConnection := context.WithCancel(m.root)
	connected := &connection{
		key: key, serverName: qualified, session: session, transport: owned, httpClient: httpClient, manager: m,
		root: connectionRoot, cancel: cancelConnection,
	}
	if httpClient != nil {
		httpClient.Timeout = 0
		connected.httpWire = httpClient.Transport.(*headerTransport)
	}
	listContext, cancelList := context.WithTimeout(ctx, m.options.ListTimeout)
	remoteTools, err := connected.loadTools(listContext)
	cancelList()
	if err != nil {
		_ = connected.close()
		return nil, err
	}
	connected.tools = remoteTools
	return connected, nil
}

type ownedTransport struct {
	base       sdkmcp.Transport
	mu         sync.Mutex
	connection sdkmcp.Connection
}

func (transport *ownedTransport) Connect(ctx context.Context) (sdkmcp.Connection, error) {
	connection, err := transport.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	transport.mu.Lock()
	transport.connection = connection
	transport.mu.Unlock()
	return connection, nil
}

func (transport *ownedTransport) Close() error {
	transport.mu.Lock()
	connection := transport.connection
	transport.mu.Unlock()
	if connection == nil {
		return nil
	}
	return connection.Close()
}

func (c *connection) loadTools(ctx context.Context) ([]tool.Tool, error) {
	type projectedTool struct {
		wireName    string
		remoteName  string
		description string
		input       *jsonschema.Schema
	}
	projected := make(map[string]projectedTool)
	collisions := make(map[string]bool)
	cursor := ""
	toolCount := 0
	metadataBytes := 0
	for page := 1; ; page++ {
		if page > c.manager.options.MaxToolPages {
			return nil, fmt.Errorf("MCP tool discovery exceeded %d pages", c.manager.options.MaxToolPages)
		}
		response, err := c.session.ListTools(ctx, &sdkmcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, err
		}
		for _, remote := range response.Tools {
			toolCount++
			if toolCount > c.manager.options.MaxTools {
				return nil, fmt.Errorf("MCP tool discovery exceeded %d tools", c.manager.options.MaxTools)
			}
			if remote == nil || strings.TrimSpace(remote.Name) == "" {
				continue
			}
			input, schemaBytes, err := projectSchema(remote.InputSchema, c.manager.options.MaxToolSchemaBytes)
			if err != nil {
				c.manager.warn(warning.WarnMCPToolInvalid, c.serverName, "remote MCP tool schema is invalid or exceeds its size limit")
				continue
			}
			wireName := remoteToolName(c.serverName, remote.Name)
			if collisions[wireName] {
				continue
			}
			if _, exists := projected[wireName]; exists {
				delete(projected, wireName)
				collisions[wireName] = true
				c.manager.warnCollision(wireName)
				continue
			}
			description := truncateUTF8(remote.Description, c.manager.options.MaxToolDescriptionBytes)
			metadataBytes += len(remote.Name) + len(wireName) + len(description) + schemaBytes
			if metadataBytes > c.manager.options.MaxToolMetadataBytes {
				return nil, fmt.Errorf("MCP tool metadata exceeded %d bytes", c.manager.options.MaxToolMetadataBytes)
			}
			projected[wireName] = projectedTool{
				wireName: wireName, remoteName: remote.Name,
				description: description, input: input,
			}
		}
		if response.NextCursor == "" {
			break
		}
		if response.NextCursor == cursor {
			return nil, errors.New("MCP tool discovery returned a repeated cursor")
		}
		cursor = response.NextCursor
	}
	names := make([]string, 0, len(projected))
	for name := range projected {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]tool.Tool, 0, len(names))
	for _, name := range names {
		remote := projected[name]
		native, err := functiontool.New[map[string]any, map[string]any](functiontool.Config{
			Name: remote.wireName, Description: remote.description, InputSchema: remote.input,
			OutputSchema: &jsonschema.Schema{Type: "object"},
		}, func(ctx agent.Context, arguments map[string]any) (map[string]any, error) {
			return c.call(ctx, ctx.SessionID(), remote.remoteName, arguments)
		})
		if err != nil {
			return nil, err
		}
		result = append(result, native)
	}
	return result, nil
}

func (c *connection) call(ctx context.Context, sessionID, name string, arguments map[string]any) (resultMap map[string]any, err error) {
	if err := c.beginCall(); err != nil {
		return nil, err
	}
	defer c.finishCall()
	teardownTransferred := false
	defer func() {
		if !teardownTransferred {
			c.manager.teardowns.Done()
		}
	}()
	reservation := c.manager.options.Budget.Reserve(sessionID, c.manager.options.Output.MaxBytes)
	emitted := 0
	defer func() { c.manager.options.Budget.Consume(sessionID, reservation.ID, emitted) }()
	callContext, stop := linkedContext(ctx, c.root)
	defer stop()
	result, err := c.session.CallTool(callContext, &sdkmcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		err = redactedRuntimeError(errToolCallFailed, callContext.Err())
		if ctx.Err() == nil && c.root.Err() == nil {
			c.manager.markBroken(c, err)
			teardownTransferred = true
		}
		return nil, err
	}
	c.manager.succeed(c.serverName)
	maximumItems := min(len(result.Content), c.manager.options.MaxResultItems)
	content := make([]any, 0, maximumItems)
	for _, item := range result.Content[:maximumItems] {
		data, marshalErr := item.MarshalJSON()
		if marshalErr != nil {
			return nil, marshalErr
		}
		var value any
		if unmarshalErr := json.Unmarshal(data, &value); unmarshalErr != nil {
			return nil, unmarshalErr
		}
		content = append(content, value)
	}
	projected := map[string]any{"content": content, "structured_content": result.StructuredContent, "is_error": result.IsError}
	if omitted := len(result.Content) - maximumItems; omitted > 0 {
		projected["truncated"] = true
		projected["omitted_content_items"] = omitted
	}
	bounded, emittedBytes, err := c.manager.boundResult(projected, result.IsError, reservation.Grant)
	if err != nil {
		return nil, err
	}
	emitted = emittedBytes
	return bounded, nil
}

func (c *connection) beginCall() error {
	c.callMu.Lock()
	defer c.callMu.Unlock()
	if c.closing {
		return errors.New("MCP connection is closing")
	}
	if err := c.manager.beginCall(); err != nil {
		return err
	}
	c.active.Add(1)
	return nil
}

func (c *connection) finishCall() {
	c.active.Done()
	c.manager.active.Done()
}

func (c *connection) startDraining() {
	c.callMu.Lock()
	if !c.closing {
		c.closing = true
		c.cancel()
	}
	c.callMu.Unlock()
}

func (m *Manager) boundResult(projected map[string]any, isError bool, grant int) (map[string]any, int, error) {
	return outputlimit.BoundJSON(projected, grant, m.options.Output, func(limited string, _ outputlimit.Report) map[string]any {
		content := []any{}
		if limited != "" {
			content = append(content, map[string]any{"type": "text", "text": limited})
		}
		bounded := map[string]any{"content": content, "truncated": true}
		if isError {
			bounded["is_error"] = true
		}
		return bounded
	})
}

func (m *Manager) markBroken(connection *connection, cause error) {
	connection.startDraining()
	lock := m.keyLock(connection.serverName)
	lock.Lock()
	m.mu.Lock()
	if m.connections[connection.key] == connection {
		delete(m.connections, connection.key)
	}
	m.mu.Unlock()
	m.fail(connection.serverName, cause)
	go func() {
		defer m.teardowns.Done()
		defer lock.Unlock()
		_ = connection.close()
	}()
}

func (m *Manager) fail(name string, cause error) {
	m.mu.Lock()
	m.failures[name]++
	count := m.failures[name]
	if count >= m.options.FailureLimit {
		m.suppressed[name] = true
	}
	m.mu.Unlock()
	m.warn(warning.WarnMCPConnectFailed, name, fmt.Sprintf("MCP server failed (%d/%d)", count, m.options.FailureLimit))
}

func (m *Manager) succeed(name string) {
	m.mu.Lock()
	delete(m.failures, name)
	delete(m.suppressed, name)
	m.mu.Unlock()
}

func (m *Manager) beginOperation() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errors.New("MCP manager is closed")
	}
	m.operations.Add(1)
	return nil
}

func (m *Manager) beginCall() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errors.New("MCP manager is closed")
	}
	m.active.Add(1)
	m.teardowns.Add(1)
	return nil
}

func (m *Manager) warn(code, path, message string) {
	m.options.Warnings.Warn(warning.Warning{Code: code, Source: "mcp", Path: path, Message: message})
}

func (m *Manager) warnCollision(wireName string) {
	m.mu.Lock()
	if m.collisions[wireName] {
		m.mu.Unlock()
		return
	}
	m.collisions[wireName] = true
	m.mu.Unlock()
	m.warn(warning.WarnMCPToolCollision, wireName, "MCP tools map to the same ADK wire name")
}

func (m *Manager) keyLock(key string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	lock := m.locks[key]
	if lock == nil {
		lock = &sync.Mutex{}
		m.locks[key] = lock
	}
	return lock
}

// DropSession closes and forgets every transport owned by one durable session
// before that session's extension catalog is refreshed.
func (m *Manager) DropSession(ctx context.Context, sessionID string) error {
	if m == nil || ctx == nil || sessionID == "" {
		return errors.New("drop MCP session: manager, context, and session id are required")
	}
	if err := m.beginOperation(); err != nil {
		return err
	}
	defer m.operations.Done()
	m.mu.Lock()
	connections := make([]*connection, 0)
	for key, connection := range m.connections {
		if key.sessionID == sessionID {
			connections = append(connections, connection)
			delete(m.connections, key)
		}
	}
	m.mu.Unlock()
	if len(connections) == 0 {
		return nil
	}
	for _, connection := range connections {
		connection.startDraining()
	}
	results := make(chan error, len(connections))
	for _, current := range connections {
		lock := m.keyLock(current.serverName)
		lock.Lock()
		m.teardowns.Add(1)
		go func(connection *connection, lock *sync.Mutex) {
			defer m.teardowns.Done()
			defer lock.Unlock()
			results <- connection.close()
		}(current, lock)
	}
	timer := time.NewTimer(m.options.CloseGrace)
	defer timer.Stop()
	var failures []error
	for range connections {
		select {
		case err := <-results:
			if err != nil {
				failures = append(failures, err)
			}
		case <-ctx.Done():
			return errors.Join(errors.Join(failures...), ctx.Err())
		case <-timer.C:
			for _, connection := range connections {
				connection.abort()
			}
			return errors.Join(errors.Join(failures...), fmt.Errorf("drop MCP session: timed out after %s", m.options.CloseGrace))
		}
	}
	return errors.Join(failures...)
}

// Close cancels calls before closing every session and transport. It is idempotent.
func (m *Manager) Close() (returnErr error) {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	if m.closed {
		done := m.closeDone
		m.mu.Unlock()
		<-done
		m.mu.Lock()
		err := m.closeErr
		m.mu.Unlock()
		return err
	}
	m.closed = true
	m.cancel()
	defer func() {
		m.mu.Lock()
		m.closeErr = returnErr
		close(m.closeDone)
		m.mu.Unlock()
	}()
	deadline := time.NewTimer(m.options.CloseGrace)
	defer deadline.Stop()
	keys := make([]connectionKey, 0, len(m.connections))
	for key := range m.connections {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].sessionID != keys[j].sessionID {
			return keys[i].sessionID > keys[j].sessionID
		}
		return keys[i].server > keys[j].server
	})
	connections := make([]*connection, 0, len(keys))
	for _, key := range keys {
		connections = append(connections, m.connections[key])
	}
	m.connections = make(map[connectionKey]*connection)
	m.mu.Unlock()
	for _, connection := range connections {
		connection.startDraining()
	}
	waitsDone := make(chan struct{})
	go func() {
		m.operations.Wait()
		m.active.Wait()
		m.teardowns.Wait()
		close(waitsDone)
	}()
	type closeResult struct {
		connection *connection
		err        error
	}
	results := make(chan closeResult, len(connections))
	startClose := func(force bool) {
		if force {
			for _, connection := range connections {
				connection.abort()
			}
		}
		for _, connection := range connections {
			go func() { results <- closeResult{connection: connection, err: connection.close()} }()
		}
	}
	select {
	case <-waitsDone:
		startClose(false)
	case <-deadline.C:
		startClose(true)
		return fmt.Errorf("close MCP servers: timed out after %s", m.options.CloseGrace)
	}
	var failures []error
	closePending := len(connections)
	for closePending > 0 {
		select {
		case result := <-results:
			closePending--
			if result.err != nil {
				failures = append(failures, fmt.Errorf("close MCP server %q: %w", result.connection.serverName, result.err))
			}
		case <-deadline.C:
			failures = append(failures, fmt.Errorf("close MCP servers: timed out after %s", m.options.CloseGrace))
			return errors.Join(failures...)
		}
	}
	return errors.Join(failures...)
}

func (c *connection) close() error {
	c.closeOnce.Do(func() {
		c.startDraining()
		c.active.Wait()
		c.abort()
		if c.httpWire != nil {
			c.httpWire.closeIdleConnections()
		}
		if c.transport != nil {
			c.closeErr = c.transport.Close()
		}
		c.closeErr = errors.Join(c.closeErr, c.session.Close())
		if c.closeErr != nil {
			c.closeErr = errCloseFailed
		}
		if c.httpClient != nil {
			c.httpClient.CloseIdleConnections()
		}
	})
	return c.closeErr
}

func (c *connection) abort() {
	if c.httpWire != nil {
		c.httpWire.beginShutdown()
		c.httpWire.abort()
	}
}

func redactedRuntimeError(fallback error, contexts ...error) error {
	for _, err := range contexts {
		if err != nil {
			return errors.Join(fallback, err)
		}
	}
	return fallback
}

type headerTransport struct {
	base             http.RoundTripper
	headers          map[string]string
	scheme           string
	host             string
	closeTimeout     time.Duration
	maxResponseBytes int64
	mu               sync.Mutex
	active           map[uint64]context.CancelFunc
	nextRequest      uint64
	shuttingDown     bool
}

func cloneDefaultHTTPTransport() http.RoundTripper {
	return http.DefaultTransport.(*http.Transport).Clone()
}

func (t *headerTransport) closeIdleConnections() {
	t.base.(*http.Transport).CloseIdleConnections()
}

func (t *headerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	requestContext, cancelRequest := context.WithCancel(request.Context())
	cancel := context.CancelFunc(cancelRequest)
	if request.Method == http.MethodDelete && t.closeTimeout > 0 {
		var cancelTimeout context.CancelFunc
		requestContext, cancelTimeout = context.WithTimeout(requestContext, t.closeTimeout)
		cancel = func() {
			cancelTimeout()
			cancelRequest()
		}
	}
	requestID := t.track(cancel)
	clone := request.Clone(requestContext)
	clone.Header = request.Header.Clone()
	if request.URL.Scheme == t.scheme && request.URL.Host == t.host {
		for name, value := range t.headers {
			clone.Header.Set(name, value)
		}
	}
	response, err := t.base.RoundTrip(clone)
	if err != nil {
		if request.Method != http.MethodDelete || !t.isShuttingDown() || requestContext.Err() == nil {
			t.finish(requestID)
			return nil, err
		}
		response = &http.Response{
			Status: "204 No Content", StatusCode: http.StatusNoContent,
			Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: clone,
		}
	}
	if request.Method != http.MethodDelete && response.Body != nil && t.maxResponseBytes > 0 {
		response.Body = &boundedResponseBody{source: response.Body, remaining: t.maxResponseBytes}
	}
	if response.Body == nil {
		t.finish(requestID)
	} else {
		response.Body = &trackedResponseBody{source: response.Body, finish: func() { t.finish(requestID) }}
	}
	return response, nil
}

func (t *headerTransport) track(cancel context.CancelFunc) uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.nextRequest++
	if t.nextRequest == 0 {
		t.nextRequest++
	}
	t.active[t.nextRequest] = cancel
	return t.nextRequest
}

func (t *headerTransport) finish(requestID uint64) {
	t.mu.Lock()
	cancel := t.active[requestID]
	delete(t.active, requestID)
	t.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (t *headerTransport) abort() {
	t.mu.Lock()
	cancellations := make([]context.CancelFunc, 0, len(t.active))
	for _, cancel := range t.active {
		cancellations = append(cancellations, cancel)
	}
	t.mu.Unlock()
	for _, cancel := range cancellations {
		cancel()
	}
}

func (t *headerTransport) beginShutdown() {
	t.mu.Lock()
	t.shuttingDown = true
	t.mu.Unlock()
}

func (t *headerTransport) isShuttingDown() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.shuttingDown
}

type trackedResponseBody struct {
	source io.ReadCloser
	finish func()
	once   sync.Once
}

func (body *trackedResponseBody) Read(buffer []byte) (int, error) { return body.source.Read(buffer) }

func (body *trackedResponseBody) Close() error {
	err := body.source.Close()
	body.once.Do(body.finish)
	return err
}

type boundedResponseBody struct {
	source    io.ReadCloser
	remaining int64
}

func (body *boundedResponseBody) Read(buffer []byte) (int, error) {
	if body.remaining <= 0 {
		var probe [1]byte
		read, err := body.source.Read(probe[:])
		if read > 0 {
			return 0, errMessageTooLarge
		}
		return 0, err
	}
	if int64(len(buffer)) > body.remaining {
		buffer = buffer[:body.remaining]
	}
	read, err := body.source.Read(buffer)
	body.remaining -= int64(read)
	return read, err
}

func (body *boundedResponseBody) Close() error { return body.source.Close() }

func linkedContext(ctx, root context.Context) (context.Context, func()) {
	linked, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(root, cancel)
	return linked, func() { stop(); cancel() }
}

func projectSchema(value any, maximum int) (*jsonschema.Schema, int, error) {
	if value == nil {
		return &jsonschema.Schema{Type: "object"}, len(`{"type":"object"}`), nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, 0, err
	}
	if len(data) > maximum {
		return nil, 0, fmt.Errorf("schema is %d bytes, limit is %d", len(data), maximum)
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(data, &schema); err != nil {
		return nil, 0, err
	}
	return &schema, len(data), nil
}

func remoteToolName(server, name string) string {
	value := "mcp__" + sanitizeName(server) + "__" + sanitizeName(name)
	if len(value) > maxWireToolNameBytes {
		value = value[:maxWireToolNameBytes]
	}
	return value
}

func sanitizeName(value string) string {
	var result strings.Builder
	for _, character := range []byte(value) {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '_' {
			result.WriteByte(character)
		} else {
			result.WriteByte('_')
		}
	}
	return result.String()
}

func truncateUTF8(value string, maximum int) string {
	if maximum <= 0 || len(value) <= maximum {
		return value
	}
	end := maximum
	for end > 0 && !utf8.ValidString(value[:end]) {
		end--
	}
	return value[:end]
}

func compactTools(values []tool.Tool) []tool.Tool {
	result := values[:0]
	for _, value := range values {
		if value != nil {
			result = append(result, value)
		}
	}
	return result
}

func sortedEnvironment(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, len(keys))
	for index, key := range keys {
		result[index] = key + "=" + values[key]
	}
	return result
}

func mergeEnvironment(base []string, overrides map[string]string) []string {
	if len(overrides) == 0 {
		return append([]string(nil), base...)
	}
	result := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		name, _, _ := strings.Cut(entry, "=")
		if _, replaced := overrides[name]; !replaced {
			result = append(result, entry)
		}
	}
	return append(result, sortedEnvironment(overrides)...)
}

func cloneStrings(values map[string]string) map[string]string {
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}
