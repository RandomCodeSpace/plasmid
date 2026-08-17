package adkloop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"google.golang.org/genai"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"

	"github.com/plasmid-dev/plasmid/loop"
	warningpkg "github.com/plasmid-dev/plasmid/warning"
)

func TestSessionServiceDelegatesIdentityAndClonesState(t *testing.T) {
	store := newRecordingSessionStore()
	service := NewSessionService(store)
	state := map[string]any{"count": float64(1)}
	created, err := service.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "session", State: state})
	if err != nil {
		t.Fatal(err)
	}
	state["count"] = float64(2)
	if len(store.created) != 1 || store.created[0].AppName != "app" || store.created[0].UserID != "user" || store.created[0].SessionID != "session" || store.created[0].State["count"] != float64(1) {
		t.Fatalf("create requests = %#v", store.created)
	}
	if got, _ := created.Session.State().Get("count"); got != float64(1) {
		t.Fatalf("created state count = %#v", got)
	}

	got, err := service.Get(t.Context(), &session.GetRequest{AppName: "app", UserID: "user", SessionID: "session"})
	if err != nil {
		t.Fatal(err)
	}
	if len(store.gets) != 1 || store.gets[0] != [3]string{"app", "user", "session"} || got.Session.ID() != "session" {
		t.Fatalf("gets = %#v, session = %#v", store.gets, got.Session)
	}

	listed, err := service.List(t.Context(), &session.ListRequest{AppName: "app", UserID: "user"})
	if err != nil {
		t.Fatal(err)
	}
	if len(store.lists) != 1 || store.lists[0] != [2]string{"app", "user"} || len(listed.Sessions) != 1 || listed.Sessions[0].ID() != "session" {
		t.Fatalf("lists = %#v, response = %#v", store.lists, listed)
	}

	if err := service.Delete(t.Context(), &session.DeleteRequest{AppName: "app", UserID: "user", SessionID: "session"}); err != nil {
		t.Fatal(err)
	}
	if len(store.deletes) != 1 || store.deletes[0] != [3]string{"app", "user", "session"} {
		t.Fatalf("deletes = %#v", store.deletes)
	}
}

func TestSessionServiceRejectsStoreIdentityDrift(t *testing.T) {
	tests := []struct {
		name string
		run  func(*recordingSessionStore) error
	}{
		{
			name: "create",
			run: func(store *recordingSessionStore) error {
				store.createOverride = &loop.SessionRef{ID: "other", AppName: "app", UserID: "user"}
				_, err := NewSessionService(store).Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "session"})
				return err
			},
		},
		{
			name: "create generated empty ID",
			run: func(store *recordingSessionStore) error {
				store.createOverride = &loop.SessionRef{AppName: "app", UserID: "user"}
				_, err := NewSessionService(store).Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user"})
				return err
			},
		},
		{
			name: "get",
			run: func(store *recordingSessionStore) error {
				store.getOverride = &loop.SessionRef{ID: "other", AppName: "app", UserID: "user"}
				_, err := NewSessionService(store).Get(t.Context(), &session.GetRequest{AppName: "app", UserID: "user", SessionID: "session"})
				return err
			},
		},
		{
			name: "list",
			run: func(store *recordingSessionStore) error {
				store.listOverride = []loop.SessionRef{{ID: "session", AppName: "other", UserID: "user"}}
				_, err := NewSessionService(store).List(t.Context(), &session.ListRequest{AppName: "app", UserID: "user"})
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.run(newRecordingSessionStore()); !errors.Is(err, ErrFidelity) {
				t.Fatalf("error = %v, want ErrFidelity", err)
			}
		})
	}
}

func TestSessionServiceGetAppliesRecentBeforeInclusiveAfter(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	times := []time.Time{base.Add(3 * time.Second), base.Add(time.Second), base.Add(4 * time.Second), base.Add(2 * time.Second)}
	stored := make([]loop.Event, len(times))
	for index, timestamp := range times {
		native := &session.Event{ID: string(rune('a' + index)), Timestamp: timestamp, Author: "agent", LLMResponse: model.LLMResponse{Content: genai.NewContentFromText(string(rune('a'+index)), genai.RoleModel)}}
		stored[index] = storedADKEvent(t, "session", native)
	}
	tests := []struct {
		name   string
		after  time.Time
		recent int
		want   []string
	}{
		{name: "no filters", want: []string{"a", "b", "c", "d"}},
		{name: "after is inclusive", after: base.Add(2 * time.Second), want: []string{"a", "c", "d"}},
		{name: "recent before timestamp", after: base.Add(3 * time.Second), recent: 2, want: []string{"c"}},
		{name: "recent only", recent: 2, want: []string{"c", "d"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := newRecordingSessionStore()
			store.getOverride = &loop.SessionRef{ID: "session", AppName: "app", UserID: "user"}
			store.getEvents = stored
			response, err := NewSessionService(store).Get(t.Context(), &session.GetRequest{
				AppName: "app", UserID: "user", SessionID: "session", After: test.after, NumRecentEvents: test.recent,
			})
			if err != nil {
				t.Fatal(err)
			}
			got := make([]string, response.Session.Events().Len())
			for index := range got {
				got[index] = response.Session.Events().At(index).ID
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("event IDs = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestSessionServiceAppendEventCopiesAndScopesState(t *testing.T) {
	store := newRecordingSessionStore()
	if _, err := store.Create(t.Context(), loop.CreateSessionRequest{AppName: "app", UserID: "user", SessionID: "session", State: map[string]any{"initial": "kept"}}); err != nil {
		t.Fatal(err)
	}
	current := newADKSession(loop.SessionRef{ID: "session", AppName: "app", UserID: "user", State: map[string]any{"initial": "kept"}}, nil)
	service := NewSessionService(store)
	timestamp := time.Date(2026, 8, 17, 1, 2, 3, 0, time.UTC)
	event := &session.Event{
		ID: "event", InvocationID: "invocation", Author: "agent", Timestamp: timestamp,
		Actions:     session.EventActions{StateDelta: map[string]any{"persisted": "value", "temp:transient": "discard"}},
		LLMResponse: model.LLMResponse{Content: genai.NewContentFromText("done", genai.RoleModel)},
	}
	if err := service.AppendEvent(t.Context(), current, event); err != nil {
		t.Fatal(err)
	}
	if len(store.appends) != 1 {
		t.Fatalf("appends = %#v", store.appends)
	}
	appendCall := store.appends[0]
	if appendCall.ref.ID != "session" || appendCall.ref.AppName != "app" || appendCall.ref.UserID != "user" || appendCall.ref.State["initial"] != "kept" {
		t.Fatalf("append ref = %#v", appendCall.ref)
	}
	if appendCall.event.SessionID != "session" || appendCall.event.InvocationID != "invocation" {
		t.Fatalf("append event = %#v", appendCall.event)
	}
	persistedRaw := appendCall.event.Raw
	var persisted session.Event
	if err := json.Unmarshal(persistedRaw, &persisted); err != nil {
		t.Fatal(err)
	}
	if _, ok := persisted.Actions.StateDelta["temp:transient"]; ok {
		t.Fatalf("temporary state persisted in Raw = %#v", persisted.Actions.StateDelta)
	}
	if persisted.Actions.StateDelta["persisted"] != "value" {
		t.Fatalf("persisted Raw state = %#v", persisted.Actions.StateDelta)
	}
	if got, err := current.State().Get("persisted"); err != nil || got != "value" {
		t.Fatalf("persisted state = %#v, %v", got, err)
	}
	if got, err := current.State().Get("temp:transient"); !errors.Is(err, session.ErrStateKeyNotExist) || got != nil {
		t.Fatalf("temporary state persisted = %#v, %v", got, err)
	}
	if current.LastUpdateTime() != timestamp || current.Events().Len() != 1 {
		t.Fatalf("updated = %v, events = %d", current.LastUpdateTime(), current.Events().Len())
	}
	if _, ok := current.Events().At(0).Actions.StateDelta["temp:transient"]; ok {
		t.Fatalf("temporary state retained in local event = %#v", current.Events().At(0).Actions.StateDelta)
	}
	if event.Actions.StateDelta["temp:transient"] != "discard" {
		t.Fatalf("input event was mutated = %#v", event.Actions.StateDelta)
	}

	event.Author = "mutated"
	event.Actions.StateDelta["persisted"] = "mutated"
	if current.Events().At(0).Author != "agent" {
		t.Fatalf("local event aliased caller pointer: %#v", current.Events().At(0))
	}
	if got, _ := current.State().Get("persisted"); got != "value" {
		t.Fatalf("local state aliased caller map: %#v", got)
	}
}

func TestSessionServiceRejectsForeignSessions(t *testing.T) {
	service := NewSessionService(newRecordingSessionStore())
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprintf("partial=%t", partial), func(t *testing.T) {
			foreign := &foreignSession{}
			err := service.AppendEvent(t.Context(), foreign, &session.Event{LLMResponse: model.LLMResponse{Partial: partial}})
			if !errors.Is(err, ErrForeignSession) {
				t.Fatalf("error = %v, want ErrForeignSession", err)
			}
		})
	}
}

type foreignSession struct{ session.Session }

func TestSessionServiceAppendIgnoresPartialAndFailedPersistence(t *testing.T) {
	t.Run("partial", func(t *testing.T) {
		store := newRecordingSessionStore()
		current := newADKSession(loop.SessionRef{ID: "session", AppName: "app", UserID: "user"}, nil)
		if err := NewSessionService(store).AppendEvent(t.Context(), current, &session.Event{LLMResponse: model.LLMResponse{Partial: true}}); err != nil {
			t.Fatal(err)
		}
		if len(store.appends) != 0 || current.Events().Len() != 0 {
			t.Fatalf("appends = %d, events = %d", len(store.appends), current.Events().Len())
		}
	})

	t.Run("store error", func(t *testing.T) {
		store := newRecordingSessionStore()
		store.appendErr = errors.New("write failed")
		current := newADKSession(loop.SessionRef{ID: "session", AppName: "app", UserID: "user"}, nil)
		err := NewSessionService(store).AppendEvent(t.Context(), current, &session.Event{LLMResponse: model.LLMResponse{Content: genai.NewContentFromText("done", genai.RoleModel)}})
		if err == nil || current.Events().Len() != 0 {
			t.Fatalf("error = %v, events = %d", err, current.Events().Len())
		}
	})
}

func TestSessionServiceAppendSerializesPersistenceAndProjection(t *testing.T) {
	store := &blockingAppendStore{entered: make(chan struct{}, 2), release: make(chan struct{})}
	current := newADKSession(loop.SessionRef{ID: "session", AppName: "app", UserID: "user"}, nil)
	service := NewSessionService(store)
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- service.AppendEvent(t.Context(), current, &session.Event{ID: "first", Timestamp: time.Unix(1, 0).UTC()})
	}()
	<-store.entered
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- service.AppendEvent(t.Context(), current, &session.Event{ID: "second", Timestamp: time.Unix(2, 0).UTC()})
	}()
	secondReachedPersistence := false
	select {
	case <-store.entered:
		secondReachedPersistence = true
	case <-time.After(20 * time.Millisecond):
	}
	close(store.release)
	if secondReachedPersistence {
		t.Fatal("second append reached persistence before the first projection completed")
	}
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	if got := current.Events(); got.Len() != 2 || got.At(0).ID != "first" || got.At(1).ID != "second" {
		t.Fatalf("local events = %#v", got)
	}
}

type blockingAppendStore struct {
	entered chan struct{}
	release chan struct{}
}

func (*blockingAppendStore) Create(context.Context, loop.CreateSessionRequest) (loop.SessionRef, error) {
	return loop.SessionRef{}, nil
}
func (*blockingAppendStore) Get(context.Context, string, string, string) (loop.SessionRef, []loop.Event, error) {
	return loop.SessionRef{}, nil, nil
}
func (*blockingAppendStore) List(context.Context, string, string) ([]loop.SessionRef, error) {
	return nil, nil
}
func (*blockingAppendStore) Delete(context.Context, string, string, string) error { return nil }
func (s *blockingAppendStore) Append(context.Context, loop.SessionRef, loop.Event) error {
	s.entered <- struct{}{}
	<-s.release
	return nil
}
func (*blockingAppendStore) AppendSidecar(context.Context, string, string, string, string, any) error {
	return nil
}
func (*blockingAppendStore) LoadSidecar(context.Context, string, string, string, string, any) (bool, error) {
	return false, nil
}
func (*blockingAppendStore) Close() error { return nil }

func TestSessionServiceRejectsNilInputs(t *testing.T) {
	service := NewSessionService(newRecordingSessionStore())
	current := newADKSession(loop.SessionRef{ID: "session", AppName: "app", UserID: "user"}, nil)
	tests := []struct {
		name string
		run  func() error
	}{
		{name: "create", run: func() error { _, err := service.Create(t.Context(), nil); return err }},
		{name: "get", run: func() error { _, err := service.Get(t.Context(), nil); return err }},
		{name: "list", run: func() error { _, err := service.List(t.Context(), nil); return err }},
		{name: "delete", run: func() error { return service.Delete(t.Context(), nil) }},
		{name: "append session", run: func() error { return service.AppendEvent(t.Context(), nil, &session.Event{}) }},
		{name: "append event", run: func() error { return service.AppendEvent(t.Context(), current, nil) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.run(); err == nil {
				t.Fatal("accepted nil input")
			}
		})
	}
}

func TestSessionServiceGetRejectsStoredEventSessionDrift(t *testing.T) {
	store := newRecordingSessionStore()
	store.getOverride = &loop.SessionRef{ID: "session", AppName: "app", UserID: "user"}
	store.getEvents = []loop.Event{{ID: "event", SessionID: "other", Kind: loop.EventNotice}}
	_, err := NewSessionService(store).Get(t.Context(), &session.GetRequest{AppName: "app", UserID: "user", SessionID: "session"})
	if !errors.Is(err, ErrFidelity) {
		t.Fatalf("error = %v, want ErrFidelity", err)
	}
}

func TestADKSessionStateConcurrentAccess(t *testing.T) {
	current := newADKSession(loop.SessionRef{ID: "session", AppName: "app", UserID: "user"}, nil)
	const writers = 32
	var group sync.WaitGroup
	for index := range writers {
		group.Add(1)
		go func() {
			defer group.Done()
			key := "key-" + string(rune('a'+index))
			if err := current.State().Set(key, index); err != nil {
				t.Errorf("Set(%q): %v", key, err)
				return
			}
			if got, err := current.State().Get(key); err != nil || got != index {
				t.Errorf("Get(%q) = %#v, %v", key, got, err)
			}
			for range current.State().All() {
			}
		}()
	}
	group.Wait()
	if got := len(collectState(current.State())); got != writers {
		t.Fatalf("state entries = %d, want %d", got, writers)
	}
}

func collectState(state session.State) map[string]any {
	values := make(map[string]any)
	for key, value := range state.All() {
		values[key] = value
	}
	return values
}

func TestSessionEventRawRoundTrips(t *testing.T) {
	timestamp := time.Date(2026, 8, 17, 1, 2, 3, 456000000, time.UTC)
	native := &session.Event{
		ID: "event-1", InvocationID: "invocation-1", Timestamp: timestamp, Author: "agent",
		Branch: "root.child", IsolationScope: "private",
		Actions: session.EventActions{
			StateDelta: map[string]any{"state": "updated"}, ArtifactDelta: map[string]int64{"artifact": 2},
			TransferToAgent: "next", Escalate: true, SkipSummarization: true,
		},
		LongRunningToolIDs: []string{"call-1"}, Routes: []string{"route-a"},
		RequestedInput: &session.RequestInput{InterruptID: "interrupt", Message: "approve", Payload: map[string]any{"risk": "low"}},
		Output:         map[string]any{"answer": "beta"},
		NodeInfo:       &session.NodeInfo{Path: "node/path", MessageAsOutput: true, OutputFor: []string{"node/path", "parent"}},
		LLMResponse: model.LLMResponse{
			Content: genai.NewContentFromText("complete", genai.RoleModel),
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount: 2, CandidatesTokenCount: 3, TotalTokenCount: 5,
			},
			CustomMetadata: map[string]any{"provider": "value"}, ModelVersion: "test-model", FinishReason: genai.FinishReasonStop, AvgLogprobs: -0.25,
		},
	}
	stored := storedADKEvent(t, "session", native)
	if stored.ID != native.ID || stored.Branch != native.Branch || stored.IsolationScope != native.IsolationScope || stored.StateDelta["state"] != "updated" {
		t.Fatalf("stored durable fields = %#v", stored)
	}
	native.Actions.StateDelta["state"] = "mutated"
	if stored.StateDelta["state"] != "updated" {
		t.Fatalf("stored StateDelta aliases native event: %#v", stored.StateDelta)
	}
	native.Actions.StateDelta["state"] = "updated"
	rawJSON := stored.Raw
	tests := []struct {
		name string
		raw  json.RawMessage
	}{
		{name: "json raw message", raw: rawJSON},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			storedCopy := stored
			storedCopy.Raw = test.raw
			restored, err := loopEventToADK(storedCopy)
			if err != nil {
				t.Fatal(err)
			}
			assertADKEventJSONEqual(t, restored, native)
		})
	}
}

func TestSessionEventRawRejectsMalformedValues(t *testing.T) {
	tests := []struct {
		name   string
		stored loop.Event
	}{
		{name: "malformed Raw", stored: loop.Event{Raw: json.RawMessage(`{"id":`)}},
		{name: "null Raw", stored: loop.Event{Raw: json.RawMessage(`null`)}},
		{name: "array Raw", stored: loop.Event{Raw: json.RawMessage(`[]`)}},
		{name: "missing session ID", stored: loop.Event{Kind: loop.EventNotice}},
		{name: "Raw identity mismatch", stored: loop.Event{ID: "event", SessionID: "session", InvocationID: "portable", Raw: json.RawMessage(`{"id":"event","invocationId":"native"}`)}},
		{name: "Raw session mismatch", stored: loop.Event{ID: "event", SessionID: "session", Raw: json.RawMessage(`{"id":"event","sessionId":"other"}`)}},
		{name: "warning missing value", stored: loop.Event{Kind: loop.EventWarning}},
		{name: "tool call missing tool", stored: loop.Event{Kind: loop.EventToolCall}},
		{name: "tool call missing ID", stored: loop.Event{Kind: loop.EventToolCall, Tool: &loop.ToolInvocation{Name: "tool"}}},
		{name: "tool result missing content", stored: loop.Event{Kind: loop.EventToolResult, Tool: &loop.ToolInvocation{CallID: "call", Name: "tool"}}},
		{name: "error result missing error field", stored: loop.Event{Kind: loop.EventToolResult, Tool: &loop.ToolInvocation{CallID: "call", Name: "tool", Content: map[string]any{}, IsError: true}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.stored.ID == "" {
				test.stored.ID = "event"
			}
			if test.stored.SessionID == "" && test.name != "missing session ID" {
				test.stored.SessionID = "session"
			}
			if _, err := loopEventToADK(test.stored); err == nil {
				t.Fatal("loopEventToADK accepted malformed stored event")
			}
		})
	}
}

func TestSessionEventPortableProjection(t *testing.T) {
	tests := []struct {
		name   string
		stored loop.Event
		check  func(*testing.T, *session.Event)
	}{
		{
			name: "text", stored: loop.Event{Kind: loop.EventText, Author: "agent", Text: "hello"},
			check: func(t *testing.T, event *session.Event) {
				if event.Content.Role != genai.RoleModel || event.Content.Parts[0].Text != "hello" || event.Partial {
					t.Fatalf("event = %#v", event)
				}
			},
		},
		{
			name: "text delta", stored: loop.Event{Kind: loop.EventTextDelta, Author: "user", Text: "delta"},
			check: func(t *testing.T, event *session.Event) {
				if event.Content.Role != genai.RoleUser || !event.Partial {
					t.Fatalf("event = %#v", event)
				}
			},
		},
		{
			name: "tool call", stored: loop.Event{Kind: loop.EventToolCall, Tool: &loop.ToolInvocation{ID: "call", Name: "tool", Args: map[string]any{"key": "alpha"}}, InvocationID: "invocation"},
			check: func(t *testing.T, event *session.Event) {
				call := event.Content.Parts[0].FunctionCall
				if call == nil || call.ID != "call" || call.Name != "tool" || call.Args["key"] != "alpha" {
					t.Fatalf("event = %#v", event)
				}
			},
		},
		{
			name: "tool result", stored: loop.Event{Kind: loop.EventToolResult, Tool: &loop.ToolInvocation{CallID: "call", Name: "tool", Content: map[string]any{"value": "beta"}}},
			check: func(t *testing.T, event *session.Event) {
				response := event.Content.Parts[0].FunctionResponse
				if response == nil || response.ID != "call" || response.Name != "tool" || response.Response["value"] != "beta" {
					t.Fatalf("event = %#v", event)
				}
			},
		},
		{
			name: "error", stored: loop.Event{Kind: loop.EventError, Text: "provider failed"},
			check: func(t *testing.T, event *session.Event) {
				if event.ErrorMessage != "provider failed" {
					t.Fatalf("event = %#v", event)
				}
			},
		},
		{
			name: "turn complete", stored: loop.Event{Kind: loop.EventTurnComplete},
			check: func(t *testing.T, event *session.Event) {
				if !event.TurnComplete {
					t.Fatalf("event = %#v", event)
				}
			},
		},
		{
			name: "notice", stored: loop.Event{Kind: loop.EventNotice, Text: "checkpoint"},
			check: func(t *testing.T, event *session.Event) {
				if event.CustomMetadata["loop.notice"] != "checkpoint" {
					t.Fatalf("event metadata = %#v", event.CustomMetadata)
				}
			},
		},
		{
			name: "warning", stored: loop.Event{Kind: loop.EventWarning, Warning: &warningpkg.Warning{Code: "test.warning", Source: "test", Path: "file.go", Line: 7, Message: "degraded"}},
			check: func(t *testing.T, event *session.Event) {
				warning, ok := event.CustomMetadata["loop.warning"].(warningpkg.Warning)
				if !ok || warning.Code != "test.warning" || warning.Message != "degraded" {
					t.Fatalf("event metadata = %#v", event.CustomMetadata)
				}
			},
		},
		{
			name: "usage", stored: loop.Event{Kind: loop.EventNotice, Usage: &loop.Usage{PromptTokens: 2, CompletionTokens: 3, TotalTokens: 5}},
			check: func(t *testing.T, event *session.Event) {
				if event.UsageMetadata == nil || event.UsageMetadata.PromptTokenCount != 2 || event.UsageMetadata.CandidatesTokenCount != 3 || event.UsageMetadata.TotalTokenCount != 5 {
					t.Fatalf("usage = %#v", event.UsageMetadata)
				}
			},
		},
		{
			name: "final tool call", stored: loop.Event{Kind: loop.EventToolCall, Final: true, Tool: &loop.ToolInvocation{ID: "call", Name: "tool", Args: map[string]any{}}},
			check: func(t *testing.T, event *session.Event) {
				if !event.IsFinalResponse() || !event.Actions.SkipSummarization {
					t.Fatalf("event = %#v", event)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.stored.ID == "" {
				test.stored.ID = "event"
			}
			if test.stored.SessionID == "" {
				test.stored.SessionID = "session"
			}
			event, err := loopEventToADK(test.stored)
			if err != nil {
				t.Fatal(err)
			}
			test.check(t, event)
		})
	}
}

func TestMultiPartStoredEventRequiresRawForReconstruction(t *testing.T) {
	native := &session.Event{ID: "event", InvocationID: "invocation", LLMResponse: model.LLMResponse{Content: &genai.Content{Parts: []*genai.Part{
		genai.NewPartFromText("one"), genai.NewPartFromText("two"),
	}}}}
	stored, err := adkEventToStored("session", native)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Kind != loop.EventNotice || stored.Raw == nil {
		t.Fatalf("stored = %#v", stored)
	}
	restored, err := loopEventToADK(stored)
	if err != nil {
		t.Fatal(err)
	}
	assertADKEventJSONEqual(t, restored, native)
}

func storedADKEvent(t *testing.T, sessionID string, native *session.Event) loop.Event {
	t.Helper()
	stored, err := adkEventToStored(sessionID, native)
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

func assertADKEventJSONEqual(t *testing.T, got, want *session.Event) {
	t.Helper()
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var gotValue, wantValue any
	if err := json.Unmarshal(gotJSON, &gotValue); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(wantJSON, &wantValue); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("event JSON = %s, want %s", gotJSON, wantJSON)
	}
}
