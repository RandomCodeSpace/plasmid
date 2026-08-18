package compaction

import (
	"context"
	"strings"
	"sync"
	"testing"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/plasmid-dev/plasmid/config"
	"github.com/plasmid-dev/plasmid/outputlimit"
	"github.com/plasmid-dev/plasmid/sessionstore"
	"github.com/plasmid-dev/plasmid/warning"
)

func TestManagerNativeCallbacks(t *testing.T) {
	ctx := &managerAgentContext{
		StrictContextMock: agent.NewStrictContextMock(t.Context()),
		app:               "app",
		user:              "user",
		session:           "session",
		invocation:        "invocation",
	}
	var nilManager *Manager
	if replacement, err := nilManager.BeforeModel(ctx, &model.LLMRequest{}); replacement != nil || err != nil {
		t.Fatalf("nil BeforeModel() = %#v, %v", replacement, err)
	}
	if replacement, err := nilManager.AfterModel(ctx, nil, nil); replacement != nil || err != nil {
		t.Fatalf("nil AfterModel() = %#v, %v", replacement, err)
	}

	disabled := New(Config{})
	if replacement, err := disabled.BeforeModel(ctx, nil); replacement != nil || err != nil {
		t.Fatalf("disabled BeforeModel() = %#v, %v", replacement, err)
	}
	if replacement, err := disabled.AfterModel(ctx, nil, nil); replacement != nil || err != nil {
		t.Fatalf("disabled AfterModel() = %#v, %v", replacement, err)
	}

	enabled := New(Config{Policy: config.Compaction{
		ContextTokens: 1000, TriggerFraction: 0.85, TargetFraction: 0.6,
		MinimumElisionTokens: 1, Calibration: true,
	}})
	request := responseRequest("call", "read", "body")
	if replacement, err := enabled.BeforeModel(ctx, request); replacement != nil || err != nil {
		t.Fatalf("BeforeModel() = %#v, %v", replacement, err)
	}
	response := &model.LLMResponse{UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 10}}
	if replacement, err := enabled.AfterModel(ctx, response, nil); replacement != nil || err != nil {
		t.Fatalf("AfterModel() = %#v, %v", replacement, err)
	}
}

type managerAgentContext struct {
	agent.StrictContextMock
	app        string
	user       string
	session    string
	invocation string
}

func (c *managerAgentContext) AppName() string      { return c.app }
func (c *managerAgentContext) UserID() string       { return c.user }
func (c *managerAgentContext) SessionID() string    { return c.session }
func (c *managerAgentContext) InvocationID() string { return c.invocation }

func TestManagerPersistsStickyStateAndCalibrationAcrossReopen(t *testing.T) {
	directory := t.TempDir()
	store := openTestStore(t, directory)
	createTestSession(t, store, "session")
	policy := config.Compaction{ContextTokens: 1000, TriggerFraction: 0.5, TargetFraction: 0.4, MinimumElisionTokens: 1, Calibration: true}
	first := New(Config{Policy: policy, Store: store, WarningSink: warning.DiscardSink{}})
	current := identity{app: "app", user: "user", session: "session", invocation: "one"}
	request := responseRequest("sticky", "read", strings.Repeat("large ", 800))
	result := first.before(context.Background(), current, request)
	if !result.Triggered {
		t.Fatal("first request did not trigger")
	}
	first.after(context.Background(), current, &model.LLMResponse{UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: int32(result.Estimate.Tokens * 2)}}, nil)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store = openTestStore(t, directory)
	defer store.Close()
	second := New(Config{Policy: policy, Store: store, WarningSink: warning.DiscardSink{}})
	fresh := responseRequest("sticky", "read", "durable body")
	second.before(context.Background(), identity{app: "app", user: "user", session: "session", invocation: "two"}, fresh)
	assertResponseBody(t, fresh, ElisionMarker)
	state := second.session(current)
	state.mu.Lock()
	factor := state.durable.Calibration
	state.mu.Unlock()
	if factor <= 1 || factor > 2 {
		t.Fatalf("restored calibration = %v, want (1,2]", factor)
	}
}

func TestManagerResetsOutputBudgetOnTrigger(t *testing.T) {
	budget := outputlimit.NewBudget(10_000)
	reservation := budget.Reserve("session", 1000)
	budget.Consume("session", reservation.ID, 1000)
	manager := New(Config{
		Policy: config.Compaction{ContextTokens: 1, TriggerFraction: 0.5, TargetFraction: 0.1, MinimumElisionTokens: 1},
		Budget: budget,
	})
	manager.before(context.Background(), identity{session: "session", invocation: "one"}, responseRequest("call", "read", "large"))
	if used, _ := budget.Report("session"); used != 0 {
		t.Fatalf("budget used = %d, want reset", used)
	}
}

func TestManagerDoesNotTrackPendingUsageWhenCalibrationIsDisabled(t *testing.T) {
	manager := New(Config{Policy: config.Compaction{
		ContextTokens: 1000, TriggerFraction: 0.85, TargetFraction: 0.6,
	}})
	for index := range 10 {
		manager.before(context.Background(), identity{session: "session", invocation: sessionName(index)}, &model.LLMRequest{})
	}
	if len(manager.pending) != 0 {
		t.Fatalf("pending usage count = %d, want 0", len(manager.pending))
	}
}

func TestManagerSidecarFailureWarnsAndKeepsMemoryState(t *testing.T) {
	store := openTestStore(t, t.TempDir())
	createTestSession(t, store, "session")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	warnings := &warning.SliceSink{}
	manager := New(Config{
		Policy: config.Compaction{ContextTokens: 1, TriggerFraction: 0.5, TargetFraction: 0.1, MinimumElisionTokens: 1},
		Store:  store, WarningSink: warnings,
	})
	current := identity{app: "app", user: "user", session: "session", invocation: "one"}
	manager.before(context.Background(), current, responseRequest("call", "read", strings.Repeat("body", 100)))
	manager.before(context.Background(), current, responseRequest("call", "read", "restored"))
	got := warnings.Warnings()
	if len(got) != 3 || got[0].Code != warning.WarnCompactionSidecarLoad || got[1].Code != warning.WarnCompactionBudgetExhausted || got[2].Code != warning.WarnCompactionSidecarSave {
		t.Fatalf("warnings = %#v", got)
	}
}

func TestManagerConcurrentSessions(t *testing.T) {
	store := openTestStore(t, t.TempDir())
	defer store.Close()
	manager := New(Config{
		Policy: config.Compaction{ContextTokens: 1, TriggerFraction: 0.5, TargetFraction: 0.1, MinimumElisionTokens: 1, Calibration: true},
		Store:  store, WarningSink: warning.DiscardSink{},
	})
	const sessions = 16
	for index := range sessions {
		createTestSession(t, store, sessionName(index))
	}
	var group sync.WaitGroup
	for index := range sessions {
		group.Add(1)
		go func() {
			defer group.Done()
			name := sessionName(index)
			current := identity{app: "app", user: "user", session: name, invocation: "one"}
			result := manager.before(context.Background(), current, responseRequest(name, "read", strings.Repeat("body", 100)))
			manager.after(context.Background(), current, &model.LLMResponse{UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: int32(result.Estimate.Tokens)}}, nil)
		}()
	}
	group.Wait()
	if len(manager.sessions) != sessions {
		t.Fatalf("session state count = %d, want %d", len(manager.sessions), sessions)
	}
}

func openTestStore(t *testing.T, directory string) *sessionstore.Store {
	t.Helper()
	fsync := false
	store, err := sessionstore.OpenWith(sessionstore.Options{Dir: directory, Fsync: &fsync, WarningSink: warning.DiscardSink{}})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func createTestSession(t *testing.T, store *sessionstore.Store, id string) {
	t.Helper()
	if _, err := store.Create(context.Background(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: id}); err != nil {
		t.Fatal(err)
	}
}

func sessionName(index int) string {
	return "session-" + string(rune('a'+index))
}
