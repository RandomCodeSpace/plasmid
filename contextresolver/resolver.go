// Package contextresolver discovers and assembles session-scoped coding-agent
// instructions without depending on an agent framework.
package contextresolver

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/plasmid-dev/plasmid/config"
	"github.com/plasmid-dev/plasmid/internal/pathglob"
	"github.com/plasmid-dev/plasmid/internal/syntax"
	"github.com/plasmid-dev/plasmid/shellexec"
	"github.com/plasmid-dev/plasmid/warning"
	"github.com/plasmid-dev/plasmid/workspace"
)

const (
	defaultMaxFileBytes        = 16 << 10
	defaultMaxBytes            = 256 << 10
	defaultMaxImportDepth      = 4
	defaultMaxDiscoveryEntries = 100_000
)

// TrustLevel classifies command authority without changing instruction
// visibility.
type TrustLevel uint8

const (
	TrustUntrusted TrustLevel = iota
	TrustRepository
	TrustUser
)

func (t TrustLevel) String() string {
	switch t {
	case TrustUser:
		return userScope
	case TrustRepository:
		return "repository"
	default:
		return "untrusted"
	}
}

// HostSelection enables instruction inputs owned by each compatible host.
type HostSelection struct {
	Claude  bool
	Codex   bool
	Copilot bool
}

// Options configures one resolver owned by a Harness.
type Options struct {
	Root                *workspace.Root
	HomeDir             string
	ImportRoots         []string
	TrustedRoots        []string
	MaxFileBytes        int
	MaxBytes            int
	MaxImportDepth      int
	MaxImportDepthSet   bool
	MaxDiscoveryEntries int
	PromptCommands      config.PromptCommandMode
	CommandTimeout      time.Duration
	DocumentTimeout     time.Duration
	CommandOutputBytes  int
	DocumentOutputBytes int
	Executor            *shellexec.Executor
	Hosts               *HostSelection
	WarningSink         warning.Sink
}

type commandOptions struct {
	Mode                config.PromptCommandMode
	CommandTimeout      time.Duration
	DocumentTimeout     time.Duration
	CommandOutputBytes  int
	DocumentOutputBytes int
}

// Resolver owns immutable session snapshots, lazy activation, and turn scope.
type Resolver struct {
	options Options
	scopes  syntax.ScopeStore

	mu         sync.RWMutex
	views      map[string]*sessionView
	closed     bool
	operations sync.WaitGroup
}

type sessionView struct {
	mu          sync.Mutex
	documents   []document
	active      []bool
	rendered    []string
	expanded    []bool
	renderedFor string
	generation  uint64
	cache       assembledCache
	runPolicy   *syntax.ToolPolicy
}

type assembledCache struct {
	invocation string
	generation uint64
	text       string
	valid      bool
}

type document struct {
	parts       []documentPart
	displayPath string
	provenance  []InstructionProvenance
	matcher     pathglob.Matcher
	policy      syntax.ToolPolicy
	prefix      string
	scope       int
}

// InstructionRecord is secret-free metadata for one normalized instruction
// declaration retained by a session snapshot.
type InstructionRecord struct {
	Name       string                  `json:"name"`
	Provenance []InstructionProvenance `json:"provenance"`
}

// InstructionProvenance identifies one retained instruction source.
type InstructionProvenance struct {
	Host           string `json:"host"`
	Scope          string `json:"scope"`
	SourcePath     string `json:"source_path"`
	Enabled        bool   `json:"enabled"`
	Trusted        bool   `json:"trusted"`
	Classification string `json:"classification"`
}

type documentPart struct {
	body        string
	displayPath string
	trust       TrustLevel
}

// New validates resolver configuration. Discovery remains inert until a
// session starts.
func New(options Options) (*Resolver, error) {
	if options.Root == nil {
		return nil, errors.New("construct context resolver: workspace root is required")
	}
	if options.MaxFileBytes <= 0 {
		options.MaxFileBytes = defaultMaxFileBytes
	}
	if options.MaxBytes <= 0 {
		options.MaxBytes = defaultMaxBytes
	}
	if options.MaxImportDepth < 0 {
		return nil, errors.New("construct context resolver: maximum import depth must not be negative")
	}
	if options.MaxImportDepth == 0 && !options.MaxImportDepthSet {
		options.MaxImportDepth = defaultMaxImportDepth
	}
	if options.MaxDiscoveryEntries <= 0 {
		options.MaxDiscoveryEntries = defaultMaxDiscoveryEntries
	}
	if options.WarningSink == nil {
		options.WarningSink = warning.SlogSink{}
	}
	return &Resolver{options: options, views: make(map[string]*sessionView)}, nil
}

// StartSession captures a stable catalog snapshot for a session.
func (r *Resolver) StartSession(ctx context.Context, sessionID string) error {
	if !r.beginOperation() {
		return errors.New("start context session: resolver is closed")
	}
	defer r.operations.Done()
	return r.startSession(ctx, sessionID)
}

func (r *Resolver) startSession(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return errors.New("start context session: session id is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.RLock()
	_, exists := r.views[sessionID]
	closed := r.closed
	r.mu.RUnlock()
	if closed {
		return errors.New("start context session: resolver is closed")
	}
	if exists {
		return nil
	}
	documents, err := r.discover(ctx)
	if err != nil {
		return err
	}
	view := &sessionView{
		documents: documents,
		active:    make([]bool, len(documents)),
		rendered:  make([]string, len(documents)),
		expanded:  make([]bool, len(documents)),
	}
	for index, item := range documents {
		view.active[index] = item.prefix == "" && item.matcher == nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("start context session: resolver is closed")
	}
	if _, exists := r.views[sessionID]; !exists {
		r.views[sessionID] = view
	}
	return nil
}

// Instructions returns assembled instructions and records the exact invocation
// policy used by the native before-tool callback.
func (r *Resolver) Instructions(ctx context.Context, sessionID, invocationID string) (string, error) {
	if !r.beginOperation() {
		return "", errors.New("assemble context: resolver is closed")
	}
	defer r.operations.Done()
	if sessionID == "" || invocationID == "" {
		return "", errors.New("assemble context: session and invocation ids are required")
	}
	if err := r.startSession(ctx, sessionID); err != nil {
		return "", err
	}
	r.mu.RLock()
	view := r.views[sessionID]
	r.mu.RUnlock()
	if view == nil {
		return "", errors.New("assemble context: session view is unavailable")
	}

	view.mu.Lock()
	defer view.mu.Unlock()
	if view.renderedFor != invocationID {
		view.rendered = make([]string, len(view.documents))
		view.expanded = make([]bool, len(view.documents))
		view.renderedFor = invocationID
	}
	if view.cache.valid && view.cache.invocation == invocationID && view.cache.generation == view.generation {
		return view.cache.text, nil
	}
	text, policy, err := r.assemble(ctx, sessionID, view)
	if err != nil {
		return "", err
	}
	if view.runPolicy != nil {
		policy = policy.Intersect(*view.runPolicy)
	}
	if err := r.scopes.SetOrIntersectPolicy(syntax.ScopeKey{SessionID: sessionID, InvocationID: invocationID}, policy); err != nil {
		return "", err
	}
	view.cache = assembledCache{invocation: invocationID, generation: view.generation, text: text, valid: true}
	return text, nil
}

// InstructionRecords returns a defensive copy of normalized metadata from a
// started session snapshot.
func (r *Resolver) InstructionRecords(sessionID string) []InstructionRecord {
	if !r.beginOperation() {
		return nil
	}
	defer r.operations.Done()
	r.mu.RLock()
	view := r.views[sessionID]
	r.mu.RUnlock()
	if view == nil {
		return nil
	}
	view.mu.Lock()
	defer view.mu.Unlock()
	result := make([]InstructionRecord, len(view.documents))
	for index, document := range view.documents {
		result[index] = InstructionRecord{Name: document.displayPath, Provenance: append([]InstructionProvenance(nil), document.provenance...)}
	}
	return result
}

// SetRunPolicy installs one additional deny-wins layer for the next active
// run in a session. The Harness serializes runs per session.
func (r *Resolver) SetRunPolicy(sessionID string, policy syntax.ToolPolicy) error {
	if r == nil || r.Closed() {
		return errors.New("set run policy: resolver is closed")
	}
	r.mu.RLock()
	view := r.views[sessionID]
	r.mu.RUnlock()
	if view == nil {
		return errors.New("set run policy: session view is unavailable")
	}
	view.mu.Lock()
	copy := policy
	view.runPolicy = &copy
	view.cache = assembledCache{}
	view.mu.Unlock()
	return nil
}

func (r *Resolver) assemble(ctx context.Context, sessionID string, view *sessionView) (string, syntax.ToolPolicy, error) {
	active := make([]document, 0, len(view.documents))
	fragments := make([]string, 0, len(view.documents))
	commandBudgets := make(map[string]*commandDocumentBudget)
	renderer := documentRenderer{
		resolver: r, sessionID: sessionID, commandBudgets: commandBudgets, contextError: ctx.Err,
		expand: func(input commandExpansion) string { return expandCommandsWithBudget(ctx, input) },
	}
	policy := syntax.NewToolPolicy(nil, nil)
	for index, item := range view.documents {
		if !view.active[index] {
			continue
		}
		if err := ctx.Err(); err != nil {
			return "", syntax.ToolPolicy{}, err
		}
		if !view.expanded[index] {
			rendered, err := renderer.render(item)
			if err != nil {
				return "", syntax.ToolPolicy{}, err
			}
			view.rendered[index] = rendered
			view.expanded[index] = true
		}
		active = append(active, item)
		fragments = append(fragments, view.rendered[index])
		policy = policy.Intersect(item.policy)
	}
	for assembledLength(fragments) > r.options.MaxBytes && len(fragments) > 0 {
		dropped := active[0]
		active = active[1:]
		fragments = fragments[1:]
		r.options.WarningSink.Warn(warning.Warning{
			Code: warning.WarnContextBudgetDropped, Source: "contextresolver", Path: dropped.displayPath,
			Message: "least-specific instruction content dropped to fit the assembled context budget",
		})
	}
	return strings.Join(nonEmpty(fragments), "\n\n"), policy, nil
}

type documentRenderer struct {
	resolver       *Resolver
	sessionID      string
	commandBudgets map[string]*commandDocumentBudget
	contextError   func() error
	expand         func(commandExpansion) string
}

func (d documentRenderer) render(item document) (string, error) {
	var rendered strings.Builder
	options := commandOptions{
		Mode: d.resolver.options.PromptCommands, CommandTimeout: d.resolver.options.CommandTimeout,
		DocumentTimeout: d.resolver.options.DocumentTimeout, CommandOutputBytes: d.resolver.options.CommandOutputBytes,
		DocumentOutputBytes: d.resolver.options.DocumentOutputBytes,
	}
	for _, part := range item.parts {
		body, notices := syntax.Substitute(part.body, part.displayPath, syntax.Substitutions{Variables: syntax.Variables{
			SessionID: d.sessionID, ProjectDir: d.resolver.options.Root.Dir(), Effort: "normal",
		}})
		for _, notice := range notices {
			d.resolver.options.WarningSink.Warn(notice)
		}
		budget := d.commandBudgets[part.displayPath]
		if budget == nil {
			budget = newCommandDocumentBudget(options)
			d.commandBudgets[part.displayPath] = budget
		}
		body = d.expand(commandExpansion{
			source: body, path: part.displayPath, trust: part.trust, options: options,
			executor: d.resolver.options.Executor, sink: d.resolver.options.WarningSink, budget: budget,
		})
		if err := d.contextError(); err != nil {
			return "", err
		}
		rendered.WriteString(body)
	}
	return strings.TrimSpace(rendered.String()), nil
}

func assembledLength(values []string) int {
	length := 0
	for _, value := range nonEmpty(values) {
		if length != 0 {
			length += 2
		}
		length += len(value)
	}
	return length
}

func nonEmpty(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" {
			result = append(result, value)
		}
	}
	return result
}

// ObserveTouch activates nested and path-scoped documents once per session.
func (r *Resolver) ObserveTouch(_ context.Context, touch workspace.Touch) {
	r.mu.RLock()
	view := r.views[touch.SessionID]
	r.mu.RUnlock()
	if view == nil {
		return
	}
	path := workspace.NormalizeTouchPath(touch.Path)
	view.mu.Lock()
	changed := false
	for index, item := range view.documents {
		if view.active[index] {
			continue
		}
		prefixMatch := item.prefix == "" || path == item.prefix || strings.HasPrefix(path, item.prefix+"/")
		globMatch := item.matcher == nil || item.matcher.Match(path)
		if prefixMatch && globMatch {
			view.active[index] = true
			changed = true
		}
	}
	if changed {
		view.generation++
		view.cache.valid = false
	}
	view.mu.Unlock()
}

// Allows rechecks the argument-aware policy for one native tool invocation.
func (r *Resolver) Allows(sessionID, invocationID, toolName string, args map[string]any) bool {
	scope, ok := r.scopes.Get(syntax.ScopeKey{SessionID: sessionID, InvocationID: invocationID})
	if !ok {
		return false
	}
	name, argument := syntax.NativeToolInvocation(toolName, args)
	return scope.Policy.Allows(name, argument)
}

// IntersectPolicy atomically narrows the active invocation scope. It cannot
// create a scope or widen any existing instruction or skill policy.
func (r *Resolver) IntersectPolicy(sessionID, invocationID string, policy syntax.ToolPolicy) error {
	if r == nil || r.Closed() {
		return errors.New("intersect tool policy: resolver is closed")
	}
	return r.scopes.IntersectPolicy(syntax.ScopeKey{SessionID: sessionID, InvocationID: invocationID}, policy)
}

// Closed reports whether resource teardown has started.
func (r *Resolver) Closed() bool {
	if r == nil {
		return true
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.closed
}

// Visible reports name-level model visibility for one invocation scope.
func (r *Resolver) Visible(sessionID, invocationID, toolName string) bool {
	scope, ok := r.scopes.Get(syntax.ScopeKey{SessionID: sessionID, InvocationID: invocationID})
	return !ok || scope.Policy.Visible(toolName)
}

// ReleaseRun removes every invocation scope belonging to a completed or
// aborted session run.
func (r *Resolver) ReleaseRun(sessionID string) {
	r.scopes.ReleaseSession(sessionID)
	r.mu.RLock()
	view := r.views[sessionID]
	r.mu.RUnlock()
	if view != nil {
		view.mu.Lock()
		view.cache = assembledCache{}
		view.runPolicy = nil
		view.mu.Unlock()
	}
}

// DropSession releases one snapshot after transactional session-start failure.
func (r *Resolver) DropSession(sessionID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	delete(r.views, sessionID)
	r.mu.Unlock()
	r.scopes.ReleaseSession(sessionID)
}

// ActiveScopes reports the current scope count for conformance tests.
func (r *Resolver) ActiveScopes() int { return r.scopes.Len() }

// Close releases all session snapshots. It is idempotent.
func (r *Resolver) Close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	ids := make([]string, 0, len(r.views))
	for id := range r.views {
		ids = append(ids, id)
	}
	r.views = make(map[string]*sessionView)
	r.mu.Unlock()
	r.operations.Wait()
	sort.Strings(ids)
	for _, id := range ids {
		r.scopes.ReleaseSession(id)
	}
}

func (r *Resolver) beginOperation() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return false
	}
	r.operations.Add(1)
	return true
}

func contextWarning(code, path, message string) warning.Warning {
	return warning.Warning{Code: code, Source: "contextresolver", Path: path, Message: message}
}
