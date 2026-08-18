package extensions

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/plasmid-dev/plasmid/config"
	"github.com/plasmid-dev/plasmid/foreign"
	"github.com/plasmid-dev/plasmid/warning"
	"github.com/plasmid-dev/plasmid/workspace"
)

// Options configures stable per-session extension snapshots.
type Options struct {
	WorkingDir       string
	HomeDir          string
	SkillRoots       []string
	Foreign          foreign.Options
	Claude           bool
	Codex            bool
	Copilot          bool
	MCP              config.MCP
	Instructions     []Instruction
	CompiledPlugins  []CompiledPlugin
	MaxResourceBytes int64
	MaxEntries       int
	WarningSink      warning.Sink
}

// Store owns immutable catalog snapshots keyed by session.
type Store struct {
	options    Options
	done       <-chan struct{}
	cancel     context.CancelFunc
	discover   func(context.Context, Options) (Catalog, error)
	mu         sync.RWMutex
	views      map[string]Catalog
	pending    map[string]*pendingDiscovery
	operations sync.WaitGroup
	closed     bool
}

// Format prevents secret-bearing activation configuration from appearing in
// diagnostic formatting.
func (*Store) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, "extensions.Store{redacted}")
}

// LogValue prevents structured logging from reflecting activation internals.
func (*Store) LogValue() slog.Value {
	return slog.StringValue("extensions.Store{redacted}")
}

type pendingDiscovery struct {
	done chan struct{}
	err  error
}

func NewStore(options Options) (*Store, error) {
	if options.WorkingDir == "" {
		return nil, errors.New("construct extension store: working directory is required")
	}
	if options.WarningSink == nil {
		options.WarningSink = warning.SlogSink{}
	}
	options.SkillRoots = append([]string(nil), options.SkillRoots...)
	options.MCP.AllowForeign = append([]string(nil), options.MCP.AllowForeign...)
	options.MCP.Servers = append([]config.MCPServer(nil), options.MCP.Servers...)
	for index := range options.MCP.Servers {
		options.MCP.Servers[index] = cloneMCPConfig(options.MCP.Servers[index])
	}
	options.Instructions = cloneInstructions(options.Instructions)
	options.CompiledPlugins = cloneCompiledPlugins(options.CompiledPlugins)
	if options.MaxEntries <= 0 {
		options.MaxEntries = 4096
	}
	if options.Foreign.MaxEntries <= 0 {
		options.Foreign.MaxEntries = options.MaxEntries
	}
	options.Foreign.WorkingDir = options.WorkingDir
	if options.Foreign.HomeDir == "" {
		options.Foreign.HomeDir = options.HomeDir
	}
	root, cancel := context.WithCancel(context.Background())
	return &Store{options: options, done: root.Done(), cancel: cancel, discover: discover, views: make(map[string]Catalog), pending: make(map[string]*pendingDiscovery)}, nil
}

func cloneInstructions(values []Instruction) []Instruction {
	result := make([]Instruction, len(values))
	for index, value := range values {
		result[index] = Instruction{Name: value.Name, Provenance: append([]Provenance(nil), value.Provenance...)}
	}
	return result
}

func cloneCompiledPlugins(values []CompiledPlugin) []CompiledPlugin {
	result := make([]CompiledPlugin, len(values))
	for index, value := range values {
		result[index] = CompiledPlugin{Name: value.Name, Provenance: append([]Provenance(nil), value.Provenance...)}
	}
	return result
}

func (s *Store) StartSession(ctx context.Context, sessionID string) error {
	return s.startSession(ctx, sessionID, nil)
}

// StartSessionWithInstructions captures one snapshot and appends instruction
// metadata discovered by the Harness-owned context resolver.
func (s *Store) StartSessionWithInstructions(ctx context.Context, sessionID string, instructions []Instruction) error {
	return s.startSession(ctx, sessionID, cloneInstructions(instructions))
}

func (s *Store) startSession(ctx context.Context, sessionID string, instructions []Instruction) error {
	if ctx == nil || sessionID == "" {
		return errors.New("start extension session: context and session id are required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrClosed
	}
	s.operations.Add(1)
	if _, exists := s.views[sessionID]; exists {
		s.mu.Unlock()
		s.operations.Done()
		return nil
	}
	if pending := s.pending[sessionID]; pending != nil {
		s.mu.Unlock()
		defer s.operations.Done()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.done:
			return ErrClosed
		case <-pending.done:
			return pending.err
		}
	}
	pending := &pendingDiscovery{done: make(chan struct{})}
	s.pending[sessionID] = pending
	s.mu.Unlock()
	defer s.operations.Done()

	discoveryCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(closeContext{done: s.done}, cancel)
	options := s.options
	options.Instructions = append(cloneInstructions(options.Instructions), instructions...)
	catalog, err := s.discover(discoveryCtx, options)
	stop()
	cancel()
	s.mu.Lock()
	if s.closed {
		err = ErrClosed
	} else if err == nil {
		s.views[sessionID] = catalog
	}
	pending.err = err
	delete(s.pending, sessionID)
	close(pending.done)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	return nil
}

type closeContext struct {
	done <-chan struct{}
}

func (c closeContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c closeContext) Done() <-chan struct{}       { return c.done }
func (c closeContext) Err() error {
	select {
	case <-c.done:
		return context.Canceled
	default:
		return nil
	}
}
func (closeContext) Value(any) any { return nil }

func (s *Store) DropSession(sessionID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	delete(s.views, sessionID)
	s.mu.Unlock()
}

func (s *Store) Snapshot(sessionID string) (Catalog, bool) {
	s.mu.RLock()
	value, ok := s.views[sessionID]
	s.mu.RUnlock()
	return value, ok
}

// ObserveTouch activates path-scoped skills in the touched session. Catalog
// records remain stable; only model exposure changes monotonically.
func (s *Store) ObserveTouch(_ context.Context, touch workspace.Touch) {
	if s == nil || touch.SessionID == "" {
		return
	}
	path := workspace.NormalizeTouchPath(touch.Path)
	s.mu.Lock()
	catalog, ok := s.views[touch.SessionID]
	if !ok {
		s.mu.Unlock()
		return
	}
	active := append([]bool(nil), catalog.activeSkills...)
	changed := false
	for index, matcher := range catalog.skillMatchers {
		if index >= len(active) || active[index] || matcher == nil {
			continue
		}
		if matcher.Match(path) {
			active[index] = true
			changed = true
		}
	}
	if changed {
		catalog.activeSkills = active
		s.views[touch.SessionID] = catalog
	}
	s.mu.Unlock()
}

func (s *Store) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		s.operations.Wait()
		return
	}
	s.closed = true
	s.cancel()
	s.mu.Unlock()
	s.operations.Wait()
	s.mu.Lock()
	s.views = make(map[string]Catalog)
	s.mu.Unlock()
}
