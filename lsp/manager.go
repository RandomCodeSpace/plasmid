package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/plasmid-dev/plasmid/warning"
	"github.com/plasmid-dev/plasmid/workspace"
	"go.lsp.dev/protocol"
)

const (
	DefaultInitializeTimeout = 10 * time.Second
	DefaultRequestTimeout    = 5 * time.Second
	DefaultFailureLimit      = 3
)

var (
	ErrManagerClosed = errors.New("LSP manager is closed")
	ErrUnknownServer = errors.New("unknown LSP server")
)

// LookPathFunc is the executable-detection seam. Production uses
// exec.LookPath; tests can prove that detection never installs anything.
type LookPathFunc func(string) (string, error)

// StartFunc is the lazy process/transport seam.
type StartFunc func(context.Context, string, []string, string, int64, MessageHandler) (Transport, error)

// ManagerOptions controls bounded LSP lifecycle behavior.
type ManagerOptions struct {
	Warnings           warning.Warner
	LookPath           LookPathFunc
	Start              StartFunc
	InitializeTimeout  time.Duration
	RequestTimeout     time.Duration
	FailureLimit       int
	MaxMessageBytes    int64
	DiagnosticsPerFile int
}

// Manager owns lazy language-server processes independently of any Harness.
type Manager struct {
	done     <-chan struct{}
	cancel   context.CancelFunc
	registry Registry
	options  ManagerOptions

	mu     sync.Mutex
	closed bool
	states map[serverKey]*serverState
	nextID uint64
}

type serverKey struct {
	id   string
	root string
}

type serverState struct {
	starting bool
	ready    chan struct{}
	client   *Client
	failures int
	disabled bool
	warned   map[string]bool
}

// NewManager creates a lifecycle owner. No executable lookup or process start
// occurs until Start is called.
func NewManager(parent context.Context, registry Registry, options ManagerOptions) (*Manager, error) {
	if parent == nil {
		return nil, fmt.Errorf("new LSP manager: nil context")
	}
	if len(registry.servers) == 0 {
		registry = DefaultRegistry()
	}
	if options.Warnings == nil {
		options.Warnings = warning.SlogSink{}
	}
	if options.LookPath == nil {
		options.LookPath = exec.LookPath
	}
	if options.Start == nil {
		options.Start = startStdioProcess
	}
	if options.InitializeTimeout == 0 {
		options.InitializeTimeout = DefaultInitializeTimeout
	}
	if options.RequestTimeout == 0 {
		options.RequestTimeout = DefaultRequestTimeout
	}
	if options.FailureLimit == 0 {
		options.FailureLimit = DefaultFailureLimit
	}
	if options.MaxMessageBytes == 0 {
		options.MaxMessageBytes = DefaultMaxMessageBytes
	}
	if options.DiagnosticsPerFile == 0 {
		options.DiagnosticsPerFile = DefaultDiagnosticsPerFile
	}
	if options.InitializeTimeout < 0 || options.RequestTimeout < 0 || options.FailureLimit < 1 || options.MaxMessageBytes < 1 || options.DiagnosticsPerFile < 1 {
		return nil, fmt.Errorf("new LSP manager: invalid bounds")
	}
	ctx, cancel := context.WithCancel(parent)
	return &Manager{
		done: ctx.Done(), cancel: cancel, registry: registry, options: options,
		states: make(map[serverKey]*serverState),
	}, nil
}

// Start returns the lazily initialized client for a server/root pair. Missing
// executables and server failures return (nil, nil) after one structured
// warning; configuration and caller cancellation remain explicit errors.
func (manager *Manager) Start(ctx context.Context, serverID, rootDir string) (*Client, error) {
	if ctx == nil {
		return nil, fmt.Errorf("start LSP server: nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	server, exists := manager.registry.Server(serverID)
	if !exists {
		return nil, fmt.Errorf("start LSP server %q: %w", serverID, ErrUnknownServer)
	}
	if server.Disabled {
		return nil, nil
	}
	root, err := workspace.NewRoot(rootDir)
	if err != nil {
		return nil, fmt.Errorf("start LSP server: %w", err)
	}
	key := serverKey{id: server.ID, root: root.Dir()}

	for {
		client, ready, finished, beginErr := manager.beginStart(key)
		if finished {
			return client, beginErr
		}
		if ready != nil {
			select {
			case <-ready:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

		client, code := manager.startClient(ctx, server, root, key)
		callerErr := ctx.Err()
		client, code = rejectDisconnectedClient(client, code, callerErr)
		closed, warn := manager.completeStart(key, client, code, callerErr)
		return manager.finishStart(client, code, key, callerErr, closed, warn)
	}
}

func (manager *Manager) beginStart(key serverKey) (*Client, <-chan struct{}, bool, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.closed {
		return nil, nil, true, ErrManagerClosed
	}
	state := manager.states[key]
	if state == nil {
		state = &serverState{warned: make(map[string]bool)}
		manager.states[key] = state
	}
	if state.client != nil {
		return state.client, nil, true, nil
	}
	if state.disabled {
		return nil, nil, true, nil
	}
	if state.starting {
		return nil, state.ready, false, nil
	}
	state.starting = true
	state.ready = make(chan struct{})
	return nil, nil, false, nil
}

func rejectDisconnectedClient(client *Client, code string, callerErr error) (*Client, string) {
	if client == nil || callerErr != nil {
		return client, code
	}
	select {
	case <-client.transport.Done():
		_ = client.transport.Close()
		return nil, warning.WarnLSPRequestFailed
	default:
		return client, code
	}
}

func (manager *Manager) completeStart(key serverKey, client *Client, code string, callerErr error) (closed, warn bool) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	state := manager.states[key]
	state.starting = false
	closed = manager.closed
	if client != nil && !closed && callerErr == nil {
		manager.nextID++
		client.identity = manager.nextID
		state.client = client
		state.failures = 0
		clear(state.warned)
	} else if callerErr == nil {
		state.failures++
		state.disabled = code == warning.WarnLSPUnavailable || state.failures >= manager.options.FailureLimit
	}
	ready := state.ready
	state.ready = nil
	warn = code != "" && !state.warned[code]
	if warn {
		state.warned[code] = true
	}
	close(ready)
	return closed, warn
}

func (manager *Manager) finishStart(client *Client, code string, key serverKey, callerErr error, closed, warn bool) (*Client, error) {
	if callerErr != nil || closed {
		if client != nil {
			_ = client.transport.Close()
		}
		if callerErr != nil {
			return nil, callerErr
		}
		return nil, ErrManagerClosed
	}
	if warn {
		manager.emit(code, key)
	}
	if client != nil {
		go manager.watch(client)
	}
	return client, nil
}

func (manager *Manager) startClient(ctx context.Context, server Server, root *workspace.Root, key serverKey) (client *Client, code string) {
	var started Transport
	defer func() {
		if recover() != nil {
			if started != nil {
				_ = started.Close()
			}
			client = nil
			code = warning.WarnLSPStartFailed
		}
	}()
	if ctx.Err() != nil {
		return nil, ""
	}
	executable, err := manager.options.LookPath(server.Command)
	if err != nil || executable == "" {
		return nil, warning.WarnLSPUnavailable
	}
	client = &Client{
		key: key, server: cloneServer(server), root: root,
		diagnostics: make(map[string]*diagnosticState), versions: make(map[string]int32),
		encoding: protocol.PositionEncodingKindUTF16, lifecycleDone: manager.done,
		requestTimeout:     manager.options.RequestTimeout,
		diagnosticsPerFile: manager.options.DiagnosticsPerFile,
	}
	client.recordSuccess = func() { manager.recordSuccess(client) }
	client.recordFailure = func(code string, disconnected bool) { manager.recordFailure(client, code, disconnected) }
	lifecycle := lifecycleContext{done: manager.done}
	started, err = manager.options.Start(lifecycle, executable, append([]string(nil), server.Args...), root.Dir(), manager.options.MaxMessageBytes, client.handleMessage)
	if err != nil || started == nil || started.Done() == nil {
		if started != nil {
			_ = started.Close()
		}
		return nil, warning.WarnLSPStartFailed
	}
	client.transport = started
	if ctx.Err() != nil {
		_ = started.Close()
		return nil, ""
	}
	if code := manager.initializeClient(ctx, lifecycle, root, client, started); code != "" {
		_ = started.Close()
		return nil, code
	}
	return client, ""
}

func (manager *Manager) initializeClient(ctx context.Context, lifecycle context.Context, root *workspace.Root, client *Client, transport Transport) string {
	initializeContext, cancel := context.WithTimeout(lifecycle, manager.options.InitializeTimeout)
	stopCallerCancel := context.AfterFunc(ctx, cancel)
	defer stopCallerCancel()
	defer cancel()
	rootURI, _ := PathToFileURI(root.Dir())
	processID := int32(os.Getpid())
	params := initializeParams{
		ProcessID: &processID, RootURI: rootURI,
		ClientInfo: clientInfo{Name: "plasmid"},
		Capabilities: initializeCapabilities{General: generalCapabilities{
			PositionEncodings: []protocol.PositionEncodingKind{protocol.PositionEncodingKindUTF16, protocol.PositionEncodingKindUTF8},
		}},
	}
	var result protocol.InitializeResult
	if err := transport.Call(initializeContext, "initialize", params, &result); err != nil {
		return warning.WarnLSPInitializeFailed
	}
	if err := transport.Notify(initializeContext, "initialized", protocol.InitializedParams{}); err != nil {
		return warning.WarnLSPInitializeFailed
	}
	if result.Capabilities.PositionEncoding == protocol.PositionEncodingKindUTF8 {
		client.encoding = protocol.PositionEncodingKindUTF8
	}
	return ""
}

type initializeParams struct {
	ProcessID    *int32                 `json:"processId"`
	ClientInfo   clientInfo             `json:"clientInfo"`
	RootURI      string                 `json:"rootUri"`
	Capabilities initializeCapabilities `json:"capabilities"`
}

type clientInfo struct {
	Name string `json:"name"`
}

type initializeCapabilities struct {
	General generalCapabilities `json:"general"`
}

type generalCapabilities struct {
	PositionEncodings []protocol.PositionEncodingKind `json:"positionEncodings"`
}

func (manager *Manager) watch(client *Client) {
	select {
	case <-client.transport.Done():
		if (lifecycleContext{done: manager.done}).Err() == nil {
			manager.recordFailure(client, warning.WarnLSPRequestFailed, true)
		}
	case <-manager.done:
	}
}

func (manager *Manager) recordSuccess(client *Client) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	state := manager.states[client.key]
	if state == nil || state.client != client {
		return
	}
	state.failures = 0
	state.warned[warning.WarnLSPRequestFailed] = false
}

func (manager *Manager) recordFailure(client *Client, code string, disconnected bool) {
	manager.mu.Lock()
	state := manager.states[client.key]
	if state == nil || state.client != client || manager.closed {
		manager.mu.Unlock()
		return
	}
	state.failures++
	if disconnected || state.failures >= manager.options.FailureLimit {
		state.client = nil
	}
	if state.failures >= manager.options.FailureLimit {
		state.disabled = true
	}
	disable := state.disabled
	warn := !state.warned[code]
	state.warned[code] = true
	manager.mu.Unlock()
	if warn {
		manager.emit(code, client.key)
	}
	if disable && !disconnected {
		_ = client.transport.Close()
	}
}

func (manager *Manager) emit(code string, key serverKey) {
	manager.options.Warnings.Warn(warning.Warning{
		Code: code, Source: "lsp.manager", Path: key.id,
		Message: "language server degraded to no-op",
	})
}

// ActiveServers returns deterministic server IDs for live clients.
func (manager *Manager) ActiveServers() []string {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	active := make([]string, 0, len(manager.states))
	for _, state := range manager.states {
		if state.client != nil {
			active = append(active, state.client.server.ID)
		}
	}
	slices.Sort(active)
	return slices.Compact(active)
}

func (manager *Manager) synchronize(ctx context.Context, serverID, rootDir, path, languageID string, text []byte) (enforcementTicket, bool, error) {
	client, err := manager.Start(ctx, serverID, rootDir)
	if err != nil || client == nil {
		return enforcementTicket{}, false, err
	}
	ticket, ok := client.syncDocument(ctx, path, languageID, text)
	if !ok {
		return enforcementTicket{}, false, nil
	}
	return enforcementTicket{key: client.key, clientID: client.identity, diagnostic: ticket}, true, nil
}

func (manager *Manager) waitForDiagnostics(ctx context.Context, ticket enforcementTicket) ([]Diagnostic, bool) {
	manager.mu.Lock()
	state := manager.states[ticket.key]
	var client *Client
	if state != nil && state.client != nil && state.client.identity == ticket.clientID {
		client = state.client
	}
	manager.mu.Unlock()
	if client == nil {
		return nil, false
	}
	return client.waitDiagnostics(ctx, ticket.diagnostic)
}

// Close idempotently stops all owned transports.
func (manager *Manager) Close() error {
	manager.mu.Lock()
	if manager.closed {
		manager.mu.Unlock()
		return nil
	}
	manager.closed = true
	manager.cancel()
	clients := make([]*Client, 0, len(manager.states))
	var starting []<-chan struct{}
	for _, state := range manager.states {
		if state.starting && state.ready != nil {
			starting = append(starting, state.ready)
		}
		if state.client != nil {
			clients = append(clients, state.client)
			state.client = nil
		}
	}
	manager.mu.Unlock()
	var errs []error
	for _, client := range clients {
		errs = append(errs, client.transport.Close())
	}
	waitContext, cancel := context.WithTimeout(context.Background(), manager.options.InitializeTimeout)
	defer cancel()
	for _, ready := range starting {
		select {
		case <-ready:
		case <-waitContext.Done():
			errs = append(errs, waitContext.Err())
			return errors.Join(errs...)
		}
	}
	return errors.Join(errs...)
}

// Client is one initialized server/root lifecycle.
type Client struct {
	identity           uint64
	key                serverKey
	server             Server
	root               *workspace.Root
	transport          Transport
	encoding           protocol.PositionEncodingKind
	lifecycleDone      <-chan struct{}
	requestTimeout     time.Duration
	diagnosticsPerFile int
	recordSuccess      func()
	recordFailure      func(string, bool)

	diagnosticMu sync.RWMutex
	diagnostics  map[string]*diagnosticState
	documentMu   sync.Mutex
	versions     map[string]int32
}

type diagnosticState struct {
	values             []Diagnostic
	documentVersion    int32
	publicationVersion int32
	versioned          bool
	acceptsUnversioned bool
	generation         uint64
	changed            chan struct{}
}

type diagnosticTicket struct {
	path       string
	version    int32
	generation uint64
}

// Server returns the immutable server configuration.
func (client *Client) Server() Server { return cloneServer(client.server) }

// Root returns the resolved workspace root used by the server.
func (client *Client) Root() string { return client.root.Dir() }

// PositionEncoding returns the encoding negotiated during initialize.
func (client *Client) PositionEncoding() protocol.PositionEncodingKind { return client.encoding }

func (client *Client) lifecycleErr() error {
	return (lifecycleContext{done: client.lifecycleDone}).Err()
}

// Call performs a bounded request. Runtime failure returns false after warning
// instead of leaking a server outage into the host operation.
func (client *Client) Call(ctx context.Context, method string, params, result any) (success bool) {
	defer func() {
		if recover() != nil && ctx != nil && ctx.Err() == nil && client.lifecycleErr() == nil {
			client.recordFailure(warning.WarnLSPRequestFailed, false)
			success = false
		}
	}()
	if ctx == nil || ctx.Err() != nil {
		return false
	}
	requestContext, cancel := context.WithTimeout(ctx, client.requestTimeout)
	stopLifecycleCancel := context.AfterFunc(lifecycleContext{done: client.lifecycleDone}, cancel)
	defer stopLifecycleCancel()
	defer cancel()
	err := client.transport.Call(requestContext, method, params, result)
	if err == nil {
		client.recordSuccess()
		return true
	}
	if ctx.Err() == nil && client.lifecycleErr() == nil {
		client.recordFailure(warning.WarnLSPRequestFailed, false)
	}
	return false
}

// Notify performs a bounded notification with the same no-op degradation.
func (client *Client) Notify(ctx context.Context, method string, params any) (success bool) {
	defer func() {
		if recover() != nil && ctx != nil && ctx.Err() == nil && client.lifecycleErr() == nil {
			client.recordFailure(warning.WarnLSPRequestFailed, false)
			success = false
		}
	}()
	if ctx == nil || ctx.Err() != nil {
		return false
	}
	requestContext, cancel := context.WithTimeout(ctx, client.requestTimeout)
	stopLifecycleCancel := context.AfterFunc(lifecycleContext{done: client.lifecycleDone}, cancel)
	defer stopLifecycleCancel()
	defer cancel()
	err := client.transport.Notify(requestContext, method, params)
	if err == nil {
		return true
	}
	if ctx.Err() == nil && client.lifecycleErr() == nil {
		client.recordFailure(warning.WarnLSPRequestFailed, false)
	}
	return false
}

func (client *Client) handleMessage(ctx context.Context, method string, raw json.RawMessage) (result any, err error) {
	defer func() {
		if recover() != nil {
			client.recordFailure(warning.WarnLSPRequestFailed, false)
			result = nil
			err = nil
		}
	}()
	if method != "textDocument/publishDiagnostics" {
		return nil, nil
	}
	var published protocol.PublishDiagnosticsParams
	if err := protocol.Unmarshal(raw, &published); err != nil {
		client.recordFailure(warning.WarnLSPRequestFailed, false)
		return nil, nil
	}
	diagnostics, err := NormalizeDiagnostics(client.root.Dir(), string(published.URI), published.Diagnostics, client.diagnosticsPerFile)
	if err != nil {
		client.recordFailure(warning.WarnLSPRequestFailed, false)
		return nil, nil
	}
	path := client.diagnosticPath(string(published.URI), diagnostics)
	if path != "" {
		client.publishDiagnostics(path, published.Version, diagnostics)
	}
	return nil, nil
}

func (client *Client) diagnosticPath(uri string, diagnostics []Diagnostic) string {
	if len(diagnostics) != 0 {
		return diagnostics[0].Path
	}
	uriPath, _ := FileURIToPath(uri)
	resolved, _ := client.root.Resolve(uriPath)
	return client.root.Rel(resolved)
}

func (client *Client) publishDiagnostics(path string, publishedVersion protocol.Optional[int32], diagnostics []Diagnostic) {
	client.diagnosticMu.Lock()
	defer client.diagnosticMu.Unlock()
	state := client.diagnosticStateLocked(path)
	version, versioned := publishedVersion.Get()
	if versioned && state.documentVersion != 0 && version != state.documentVersion {
		return
	}
	if !versioned && (!state.acceptsUnversioned || state.documentVersion > 1) {
		return
	}
	state.values = append([]Diagnostic(nil), diagnostics...)
	state.publicationVersion = version
	state.versioned = versioned
	state.generation++
	close(state.changed)
	state.changed = make(chan struct{})
}

// Diagnostics returns a defensive deterministic snapshot for path.
func (client *Client) Diagnostics(path string) []Diagnostic {
	relative := filepath.ToSlash(filepath.Clean(path))
	if filepath.IsAbs(path) {
		if rel, err := filepath.Rel(client.root.Dir(), path); err == nil {
			relative = filepath.ToSlash(rel)
		}
	}
	client.diagnosticMu.RLock()
	state := client.diagnostics[relative]
	var values []Diagnostic
	if state != nil {
		values = append([]Diagnostic(nil), state.values...)
	}
	client.diagnosticMu.RUnlock()
	return values
}

func (client *Client) diagnosticStateLocked(path string) *diagnosticState {
	state := client.diagnostics[path]
	if state == nil {
		state = &diagnosticState{acceptsUnversioned: true, changed: make(chan struct{})}
		client.diagnostics[path] = state
	}
	return state
}

func (client *Client) prepareDiagnostics(path string, version int32) (uint64, int32) {
	client.diagnosticMu.Lock()
	state := client.diagnosticStateLocked(path)
	generation := state.generation
	previous := state.documentVersion
	state.documentVersion = version
	client.diagnosticMu.Unlock()
	return generation, previous
}

func (client *Client) rollbackDiagnostics(path string, version, previous int32) {
	client.diagnosticMu.Lock()
	state := client.diagnosticStateLocked(path)
	if state.documentVersion == version {
		state.documentVersion = previous
	}
	client.diagnosticMu.Unlock()
}

func (client *Client) syncDocument(ctx context.Context, path, languageID string, text []byte) (diagnosticTicket, bool) {
	if !utf8.Valid(text) {
		return diagnosticTicket{}, false
	}
	uri, relative, _ := client.documentURI(path)
	client.documentMu.Lock()
	defer client.documentMu.Unlock()
	version, exists := client.versions[relative]
	version++
	if !exists {
		version = 1
	}
	generation, previous := client.prepareDiagnostics(relative, version)
	var notified bool
	if exists {
		notified = client.Notify(ctx, "textDocument/didChange", didChangeParams{
			TextDocument:   versionedDocument{URI: uri, Version: version},
			ContentChanges: []contentChange{{Text: string(text)}},
		})
	} else {
		notified = client.Notify(ctx, "textDocument/didOpen", didOpenParams{TextDocument: textDocumentItem{
			URI: uri, LanguageID: languageID, Version: version, Text: string(text),
		}})
	}
	if !notified {
		client.rollbackDiagnostics(relative, version, previous)
		return diagnosticTicket{}, false
	}
	client.versions[relative] = version
	return diagnosticTicket{path: relative, version: version, generation: generation}, true
}

func (client *Client) waitDiagnostics(ctx context.Context, ticket diagnosticTicket) ([]Diagnostic, bool) {
	for {
		client.diagnosticMu.RLock()
		state := client.diagnostics[ticket.path]
		if state == nil || state.documentVersion != ticket.version {
			client.diagnosticMu.RUnlock()
			return nil, false
		}
		if state.generation > ticket.generation && (!state.versioned || state.publicationVersion == ticket.version) {
			values := append([]Diagnostic(nil), state.values...)
			client.diagnosticMu.RUnlock()
			return values, true
		}
		changed := state.changed
		client.diagnosticMu.RUnlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return nil, false
		}
	}
}

// DidOpen starts full-text synchronization at document version one.
func (client *Client) DidOpen(ctx context.Context, path, languageID string, text []byte) bool {
	if !utf8.Valid(text) {
		return false
	}
	uri, relative, ok := client.documentURI(path)
	if !ok {
		return false
	}
	client.documentMu.Lock()
	defer client.documentMu.Unlock()
	if _, exists := client.versions[relative]; exists {
		return false
	}
	_, previous := client.prepareDiagnostics(relative, 1)
	params := didOpenParams{TextDocument: textDocumentItem{
		URI: uri, LanguageID: languageID, Version: 1, Text: string(text),
	}}
	if !client.Notify(ctx, "textDocument/didOpen", params) {
		client.rollbackDiagnostics(relative, 1, previous)
		return false
	}
	client.versions[relative] = 1
	return true
}

// DidChange publishes a full-text change with a monotonic version.
func (client *Client) DidChange(ctx context.Context, path string, text []byte) bool {
	if !utf8.Valid(text) {
		return false
	}
	uri, relative, ok := client.documentURI(path)
	if !ok {
		return false
	}
	client.documentMu.Lock()
	defer client.documentMu.Unlock()
	version, exists := client.versions[relative]
	if !exists {
		return false
	}
	version++
	_, previous := client.prepareDiagnostics(relative, version)
	params := didChangeParams{
		TextDocument:   versionedDocument{URI: uri, Version: version},
		ContentChanges: []contentChange{{Text: string(text)}},
	}
	if !client.Notify(ctx, "textDocument/didChange", params) {
		client.rollbackDiagnostics(relative, version, previous)
		return false
	}
	client.versions[relative] = version
	return true
}

// DidClose ends synchronization and clears cached diagnostics.
func (client *Client) DidClose(ctx context.Context, path string) bool {
	uri, relative, ok := client.documentURI(path)
	if !ok {
		return false
	}
	client.documentMu.Lock()
	defer client.documentMu.Unlock()
	if _, exists := client.versions[relative]; !exists {
		return false
	}
	if !client.Notify(ctx, "textDocument/didClose", didCloseParams{TextDocument: textDocumentIdentifier{URI: uri}}) {
		return false
	}
	delete(client.versions, relative)
	client.diagnosticMu.Lock()
	state := client.diagnosticStateLocked(relative)
	state.values = nil
	state.documentVersion = 0
	state.publicationVersion = 0
	state.versioned = true
	state.acceptsUnversioned = false
	state.generation++
	close(state.changed)
	state.changed = make(chan struct{})
	client.diagnosticMu.Unlock()
	return true
}

func (client *Client) documentURI(path string) (string, string, bool) {
	resolved, err := client.root.Resolve(path)
	if err != nil {
		return "", "", false
	}
	uri, _ := PathToFileURI(resolved)
	return uri, client.root.Rel(resolved), true
}

type didOpenParams struct {
	TextDocument textDocumentItem `json:"textDocument"`
}

type textDocumentItem struct {
	URI        string `json:"uri"`
	LanguageID string `json:"languageId"`
	Version    int32  `json:"version"`
	Text       string `json:"text"`
}

type didChangeParams struct {
	TextDocument   versionedDocument `json:"textDocument"`
	ContentChanges []contentChange   `json:"contentChanges"`
}

type versionedDocument struct {
	URI     string `json:"uri"`
	Version int32  `json:"version"`
}

type contentChange struct {
	Text string `json:"text"`
}

type didCloseParams struct {
	TextDocument textDocumentIdentifier `json:"textDocument"`
}

type textDocumentIdentifier struct {
	URI string `json:"uri"`
}
