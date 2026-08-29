package lsp

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/RandomCodeSpace/plasmid/outputlimit"
	"github.com/RandomCodeSpace/plasmid/warning"
	"github.com/RandomCodeSpace/plasmid/workspace"
	"go.lsp.dev/protocol"
)

// EnforcerOptions binds the framework-free LSP lifecycle to workspace touches.
type EnforcerOptions struct {
	WorkspaceDir  string
	Touches       *workspace.TouchBus
	Registry      Registry
	Manager       enforcementManager
	SettleTimeout time.Duration
	Output        outputlimit.Policy
	Warnings      warning.Warner
	Maximum       int
}

type enforcementManager interface {
	synchronize(context.Context, string, string, string, string, []byte) (enforcementTicket, bool, error)
	waitForDiagnostics(context.Context, enforcementTicket) ([]Diagnostic, bool)
	ActiveServers() []string
}

type enforcementTicket struct {
	key        serverKey
	clientID   uint64
	diagnostic diagnosticTicket
}

// Decoration is the deterministic LSP addition for one successful mutation.
type Decoration struct {
	Diagnostics []Diagnostic
	Text        string
}

type invocationKey struct {
	sessionID    string
	invocationID string
}

type enforcerWarningKey struct {
	code     string
	serverID string
}

type diagnosticWait struct {
	ticket   enforcementTicket
	serverID string
	path     string
}

type diagnosticReceipt struct {
	waits []diagnosticWait
}

// Enforcer synchronizes successful write/edit touches and correlates their
// diagnostics with the exact tool invocation that caused the document version.
type Enforcer struct {
	workspaceDir string
	registry     Registry
	manager      enforcementManager
	settle       time.Duration
	output       outputlimit.Policy
	warnings     warning.Warner
	maximum      int
	unsubscribe  func()

	mu       sync.Mutex
	closed   bool
	receipts map[invocationKey]diagnosticReceipt
	warned   map[enforcerWarningKey]bool
}

// NewEnforcer subscribes to the shared touch bus without starting a server.
func NewEnforcer(options EnforcerOptions) (*Enforcer, error) {
	if options.WorkspaceDir == "" || options.Touches == nil || options.Manager == nil {
		return nil, errors.New("new LSP enforcer: workspace, touch bus, and manager are required")
	}
	root, err := workspace.NewRoot(options.WorkspaceDir)
	if err != nil {
		return nil, fmt.Errorf("new LSP enforcer: %w", err)
	}
	if len(options.Registry.servers) == 0 {
		options.Registry = DefaultRegistry()
	}
	if options.SettleTimeout <= 0 {
		options.SettleTimeout = 1500 * time.Millisecond
	}
	if options.Output == (outputlimit.Policy{}) {
		options.Output = outputlimit.Defaults()
	}
	if options.Warnings == nil {
		options.Warnings = warning.SlogSink{}
	}
	if options.Maximum <= 0 {
		options.Maximum = DefaultDiagnosticsPerFile
	}
	enforcer := &Enforcer{
		workspaceDir: root.Dir(), registry: options.Registry, manager: options.Manager,
		settle: options.SettleTimeout, output: options.Output, warnings: options.Warnings,
		maximum: options.Maximum, receipts: make(map[invocationKey]diagnosticReceipt),
		warned: make(map[enforcerWarningKey]bool),
	}
	enforcer.unsubscribe = options.Touches.Subscribe(enforcer)
	return enforcer, nil
}

// ObserveTouch implements workspace.TouchObserver.
func (enforcer *Enforcer) ObserveTouch(ctx context.Context, touch workspace.Touch) {
	if enforcer == nil || ctx == nil || ctx.Err() != nil || touch.InvocationID == "" ||
		(touch.Kind != workspace.TouchWrite && touch.Kind != workspace.TouchEdit) {
		return
	}
	enforcer.mu.Lock()
	closed := enforcer.closed
	enforcer.mu.Unlock()
	if closed {
		return
	}

	waits := enforcer.collectDiagnosticWaits(ctx, touch)
	if len(waits) == 0 {
		return
	}
	key := invocationKey{sessionID: touch.SessionID, invocationID: touch.InvocationID}
	enforcer.mu.Lock()
	if !enforcer.closed {
		receipt := enforcer.receipts[key]
		receipt.waits = append(receipt.waits, waits...)
		enforcer.receipts[key] = receipt
	}
	enforcer.mu.Unlock()
}

func (enforcer *Enforcer) collectDiagnosticWaits(ctx context.Context, touch workspace.Touch) []diagnosticWait {
	waits := make([]diagnosticWait, 0)
	for _, server := range enforcer.registry.Match(touch.Path) {
		if wait, ok := enforcer.synchronizeServer(ctx, touch, server); ok {
			waits = append(waits, wait)
		}
	}
	return waits
}

func (enforcer *Enforcer) synchronizeServer(ctx context.Context, touch workspace.Touch, server Server) (diagnosticWait, bool) {
	root, err := SelectWorkspaceRoot(enforcer.workspaceDir, touch.Path, server.RootMarkers)
	if err != nil {
		enforcer.warn(warning.WarnLSPRequestFailed, server.ID, touch.Path)
		return diagnosticWait{}, false
	}
	ticket, ok, err := enforcer.manager.synchronize(ctx, server.ID, root, touch.Path, languageID(touch.Path), touch.Content)
	if err != nil {
		if ctx.Err() == nil && !errors.Is(err, ErrManagerClosed) {
			enforcer.warn(warning.WarnLSPRequestFailed, server.ID, touch.Path)
		}
		return diagnosticWait{}, false
	}
	if !ok {
		return diagnosticWait{}, false
	}
	return diagnosticWait{ticket: ticket, serverID: server.ID, path: ticket.diagnostic.path}, true
}

// Await consumes one invocation receipt and waits within the configured settle bound.
func (enforcer *Enforcer) Await(ctx context.Context, sessionID, invocationID string) (Decoration, bool) {
	if enforcer == nil || ctx == nil || invocationID == "" {
		return Decoration{}, false
	}
	key := invocationKey{sessionID: sessionID, invocationID: invocationID}
	enforcer.mu.Lock()
	receipt, exists := enforcer.receipts[key]
	delete(enforcer.receipts, key)
	closed := enforcer.closed
	enforcer.mu.Unlock()
	if !exists || closed {
		return Decoration{}, false
	}

	settleContext, cancel := context.WithTimeout(ctx, enforcer.settle)
	defer cancel()
	type waitResult struct {
		wait        diagnosticWait
		diagnostics []Diagnostic
		ok          bool
	}
	results := make(chan waitResult, len(receipt.waits))
	for _, wait := range receipt.waits {
		go func() {
			diagnostics, ok := enforcer.manager.waitForDiagnostics(settleContext, wait.ticket)
			results <- waitResult{wait: wait, diagnostics: diagnostics, ok: ok}
		}()
	}
	values := make([]Diagnostic, 0)
	succeeded := false
	for range receipt.waits {
		result := <-results
		if result.ok {
			succeeded = true
			values = append(values, result.diagnostics...)
			continue
		}
		if ctx.Err() == nil && errors.Is(settleContext.Err(), context.DeadlineExceeded) {
			enforcer.warn(warning.WarnLSPDiagnosticsUnsettled, result.wait.serverID, result.wait.path)
		}
	}
	if !succeeded {
		return Decoration{}, false
	}
	values = combineDiagnostics(values, enforcer.maximum)
	return Decoration{Diagnostics: values, Text: renderDiagnostics(values, enforcer.output)}, true
}

// Drop releases any receipt that cannot reach a successful after-tool callback.
func (enforcer *Enforcer) Drop(sessionID, invocationID string) {
	if enforcer == nil {
		return
	}
	enforcer.mu.Lock()
	delete(enforcer.receipts, invocationKey{sessionID: sessionID, invocationID: invocationID})
	enforcer.mu.Unlock()
}

// Status returns the current prompt line for automatic LSP mode.
func (enforcer *Enforcer) Status() string {
	if enforcer == nil {
		return ""
	}
	active := enforcer.manager.ActiveServers()
	if len(active) == 0 {
		return "LSP: none detected"
	}
	return "LSP: " + strings.Join(active, ", ")
}

// Close idempotently unsubscribes and releases pending invocation receipts.
func (enforcer *Enforcer) Close() error {
	if enforcer == nil {
		return nil
	}
	enforcer.mu.Lock()
	if enforcer.closed {
		enforcer.mu.Unlock()
		return nil
	}
	enforcer.closed = true
	clear(enforcer.receipts)
	unsubscribe := enforcer.unsubscribe
	enforcer.unsubscribe = nil
	enforcer.mu.Unlock()
	if unsubscribe != nil {
		unsubscribe()
	}
	return nil
}

func (enforcer *Enforcer) warn(code, serverID, path string) {
	key := enforcerWarningKey{code: code, serverID: serverID}
	enforcer.mu.Lock()
	if enforcer.warned[key] {
		enforcer.mu.Unlock()
		return
	}
	enforcer.warned[key] = true
	enforcer.mu.Unlock()
	enforcer.warnings.Warn(warning.Warning{
		Code: code, Source: "lsp.enforcer", Path: path,
		Message: "language server " + serverID + " degraded to no-op",
	})
}

func languageID(path string) string {
	return strings.TrimPrefix(strings.ToLower(filepath.Ext(filepath.ToSlash(path))), ".")
}

func combineDiagnostics(values []Diagnostic, maximum int) []Diagnostic {
	result := append([]Diagnostic(nil), values...)
	slices.SortFunc(result, compareDiagnostic)
	result = slices.Compact(result)
	if maximum > 0 && len(result) > maximum {
		result = result[:maximum]
	}
	return result
}

func renderDiagnostics(values []Diagnostic, policy outputlimit.Policy) string {
	lines := make([]string, 0, len(values))
	for _, value := range values {
		kind := "diagnostic"
		switch value.Severity {
		case protocol.DiagnosticSeverityError:
			kind = "error"
		case protocol.DiagnosticSeverityWarning:
			kind = "warning"
		case protocol.DiagnosticSeverityInformation:
			kind = "information"
		case protocol.DiagnosticSeverityHint:
			kind = "hint"
		}
		label := kind
		if value.Code != "" {
			label += " " + value.Code
		}
		if value.Source != "" {
			label += " (" + value.Source + ")"
		}
		lines = append(lines, fmt.Sprintf("%s:%d:%d: %s: %s", value.Path, value.Start.Line+1, value.Start.Character+1, label, value.Message))
	}
	rendered, _ := policy.Apply(strings.Join(lines, "\n"))
	return rendered
}
