package compaction

import (
	"context"
	"fmt"
	"math"
	"sync"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"

	"github.com/plasmid-dev/plasmid/config"
	"github.com/plasmid-dev/plasmid/outputlimit"
	"github.com/plasmid-dev/plasmid/sessionstore"
	"github.com/plasmid-dev/plasmid/warning"
)

const sidecarKind = "compaction.v1"

// Config supplies the validated policy and Harness-owned durability resources.
type Config struct {
	Policy      config.Compaction
	Store       *sessionstore.Store
	Budget      *outputlimit.Budget
	WarningSink warning.Sink
}

// Manager owns native before-model and after-model compaction state.
type Manager struct {
	policy   config.Compaction
	store    *sessionstore.Store
	budget   *outputlimit.Budget
	warnings warning.Sink

	mu       sync.Mutex
	sessions map[string]*sessionState
	pending  map[string]int
}

type sessionState struct {
	mu sync.Mutex

	loaded     bool
	loadWarned bool
	saveWarned bool
	durable    durableState
}

type identity struct {
	app        string
	user       string
	session    string
	invocation string
}

// New constructs a callback manager. A zero context budget leaves both
// callbacks installed as deterministic no-ops.
func New(cfg Config) *Manager {
	warnings := cfg.WarningSink
	if warnings == nil {
		warnings = warning.DiscardSink{}
	}
	return &Manager{
		policy: cfg.Policy, store: cfg.Store, budget: cfg.Budget, warnings: warnings,
		sessions: make(map[string]*sessionState), pending: make(map[string]int),
	}
}

// BeforeModel compacts one assembled native ADK request in place.
func (m *Manager) BeforeModel(ctx agent.Context, request *model.LLMRequest) (*model.LLMResponse, error) {
	if m == nil || request == nil || m.policy.ContextTokens <= 0 {
		return nil, nil
	}
	current := identity{app: ctx.AppName(), user: ctx.UserID(), session: ctx.SessionID(), invocation: ctx.InvocationID()}
	m.before(ctx, current, request)
	return nil, nil
}

// AfterModel calibrates future estimates from native prompt usage.
func (m *Manager) AfterModel(ctx agent.Context, response *model.LLMResponse, responseError error) (*model.LLMResponse, error) {
	if m == nil || m.policy.ContextTokens <= 0 || !m.policy.Calibration {
		return nil, nil
	}
	current := identity{app: ctx.AppName(), user: ctx.UserID(), session: ctx.SessionID(), invocation: ctx.InvocationID()}
	m.after(ctx, current, response, responseError)
	return nil, nil
}

func (m *Manager) before(ctx context.Context, current identity, request *model.LLMRequest) policyResult {
	state := m.session(current)
	state.mu.Lock()
	m.load(ctx, current, state)
	result, err := applyPolicy(m.policy, &state.durable, request)
	if err != nil {
		m.warn(warning.WarnCompactionEstimateFailed, current.session, "request estimation failed; model call proceeds without compaction: "+err.Error())
		state.mu.Unlock()
		return policyResult{}
	}
	if result.Triggered && m.budget != nil {
		m.budget.Reset(current.session)
	}
	if result.Exhausted && !state.durable.ExhaustedWarned {
		state.durable.ExhaustedWarned = true
		result.StateChanged = true
		m.warn(warning.WarnCompactionBudgetExhausted, current.session, "compaction target could not be reached; model call proceeds")
	}
	if result.StateChanged {
		m.save(ctx, current, state)
	}
	state.mu.Unlock()

	if m.policy.Calibration {
		m.mu.Lock()
		m.pending[current.pendingKey()] = result.Estimate.Tokens
		m.mu.Unlock()
	}
	return result
}

func (m *Manager) after(ctx context.Context, current identity, response *model.LLMResponse, responseError error) {
	m.mu.Lock()
	raw, ok := m.pending[current.pendingKey()]
	delete(m.pending, current.pendingKey())
	m.mu.Unlock()
	if !ok || raw <= 0 || responseError != nil || response == nil || response.UsageMetadata == nil || response.UsageMetadata.PromptTokenCount <= 0 {
		return
	}
	state := m.session(current)
	state.mu.Lock()
	m.load(ctx, current, state)
	observed := float64(response.UsageMetadata.PromptTokenCount) / float64(raw)
	next := 0.7*state.durable.Calibration + 0.3*observed
	next = math.Max(0.5, math.Min(2.0, next))
	if next != state.durable.Calibration {
		state.durable.Calibration = next
		m.save(ctx, current, state)
	}
	state.mu.Unlock()
}

func (m *Manager) session(current identity) *sessionState {
	key := current.sessionKey()
	m.mu.Lock()
	state := m.sessions[key]
	if state == nil {
		state = &sessionState{}
		m.sessions[key] = state
	}
	m.mu.Unlock()
	return state
}

func (m *Manager) load(ctx context.Context, current identity, state *sessionState) {
	if state.loaded {
		return
	}
	state.loaded = true
	if m.store == nil {
		return
	}
	var persisted durableState
	found, err := m.store.LoadSidecar(ctx, current.app, current.user, current.session, sidecarKind, &persisted)
	if err != nil {
		if !state.loadWarned {
			state.loadWarned = true
			m.warn(warning.WarnCompactionSidecarLoad, current.session, err.Error())
		}
		return
	}
	if !found {
		return
	}
	if persisted.Version != sidecarVersion || persisted.Calibration <= 0 || math.IsNaN(persisted.Calibration) || math.IsInf(persisted.Calibration, 0) {
		if !state.loadWarned {
			state.loadWarned = true
			m.warn(warning.WarnCompactionSidecarLoad, current.session, fmt.Sprintf("unsupported or invalid compaction sidecar version %d", persisted.Version))
		}
		return
	}
	state.durable = persisted
}

func (m *Manager) save(ctx context.Context, current identity, state *sessionState) {
	if m.store == nil {
		return
	}
	if err := m.store.AppendSidecar(ctx, current.app, current.user, current.session, sidecarKind, state.durable); err != nil && !state.saveWarned {
		state.saveWarned = true
		m.warn(warning.WarnCompactionSidecarSave, current.session, err.Error())
	}
}

func (m *Manager) warn(code, path, message string) {
	m.warnings.Warn(warning.Warning{Code: code, Source: "compaction", Path: path, Message: message})
}

func (i identity) sessionKey() string {
	return i.app + "\x00" + i.user + "\x00" + i.session
}

func (i identity) pendingKey() string {
	return i.sessionKey() + "\x00" + i.invocation
}
