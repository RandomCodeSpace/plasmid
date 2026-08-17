package adkloop

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/session/sessiontestsuite"
	"google.golang.org/genai"

	"github.com/plasmid-dev/plasmid/loop"
	"github.com/plasmid-dev/plasmid/sessionstore"
)

func TestSessionServiceConformance(t *testing.T) {
	sessiontestsuite.RunServiceTests(t, sessiontestsuite.SuiteOptions{
		SupportsUserProvidedSessionID: true,
		ProvidesServerAssignedEventID: false,
	}, func(t *testing.T) session.Service {
		store, err := sessionstore.OpenWith(sessionstore.Options{Dir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		return NewSessionService(store)
	})
}

func TestDurableSessionServiceRejectsRawEnvelopeContradiction(t *testing.T) {
	store := openConformanceStore(t)
	ref, err := store.Create(t.Context(), loop.CreateSessionRequest{AppName: "app", UserID: "user", SessionID: "session"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(t.Context(), ref, loop.Event{
		ID: "event", SessionID: ref.ID, InvocationID: "portable", Kind: loop.EventNotice,
		Raw: json.RawMessage(`{"id":"event","invocationId":"raw"}`),
	}); err != nil {
		t.Fatal(err)
	}
	_, err = NewSessionService(store).Get(t.Context(), &session.GetRequest{AppName: ref.AppName, UserID: ref.UserID, SessionID: ref.ID})
	if !errors.Is(err, ErrFidelity) {
		t.Fatalf("Get error = %v, want ErrFidelity", err)
	}
}

func TestDurableSessionServiceRecentBeforeInclusiveAfter(t *testing.T) {
	service, ref := newDurableService(t)
	base := time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)
	for index, offset := range []time.Duration{3 * time.Second, time.Second, 4 * time.Second, 2 * time.Second} {
		if err := service.AppendEvent(t.Context(), ref, &session.Event{ID: string(rune('a' + index)), Timestamp: base.Add(offset)}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := service.Get(t.Context(), &session.GetRequest{
		AppName: ref.AppName(), UserID: ref.UserID(), SessionID: ref.ID(), NumRecentEvents: 2, After: base.Add(3 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"c"}; !reflect.DeepEqual(conformanceEventIDs(got.Session), want) {
		t.Fatalf("event IDs = %#v, want %#v", conformanceEventIDs(got.Session), want)
	}
}

func TestDurableSessionServiceSuppressesPartialEvents(t *testing.T) {
	service, ref := newDurableService(t)
	if err := service.AppendEvent(t.Context(), ref, &session.Event{ID: "partial", LLMResponse: model.LLMResponse{Partial: true}}); err != nil {
		t.Fatal(err)
	}
	got, err := service.Get(t.Context(), &session.GetRequest{AppName: ref.AppName(), UserID: ref.UserID(), SessionID: ref.ID()})
	if err != nil {
		t.Fatal(err)
	}
	if got.Session.Events().Len() != 0 {
		t.Fatalf("persisted partial events = %d, want 0", got.Session.Events().Len())
	}
}

func TestDurableSessionServiceListsAllUsersForEmptyUserID(t *testing.T) {
	store := openConformanceStore(t)
	service := NewSessionService(store)
	for _, userID := range []string{"first", "second"} {
		if _, err := service.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: userID, SessionID: userID + "-session"}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := service.List(t.Context(), &session.ListRequest{AppName: "app"})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(got.Sessions); got != 2 {
		t.Fatalf("empty-user list length = %d, want 2", got)
	}
}

func TestDurableSessionServiceRestoresAfterRestart(t *testing.T) {
	dir := t.TempDir()
	store, err := sessionstore.OpenWith(sessionstore.Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	service := NewSessionService(store)
	created, err := service.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "session", State: map[string]any{"initial": "state"}})
	if err != nil {
		t.Fatal(err)
	}
	native := &session.Event{
		ID: "event", InvocationID: "invocation", Timestamp: time.Date(2026, 8, 17, 0, 0, 1, 0, time.UTC),
		Actions:     session.EventActions{StateDelta: map[string]any{"updated": "state"}},
		LLMResponse: model.LLMResponse{Content: genai.NewContentFromText("durable", genai.RoleModel)},
	}
	if err := service.AppendEvent(t.Context(), created.Session, native); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = sessionstore.OpenWith(sessionstore.Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	got, err := NewSessionService(store).Get(t.Context(), &session.GetRequest{AppName: "app", UserID: "user", SessionID: "session"})
	if err != nil {
		t.Fatal(err)
	}
	if value, err := got.Session.State().Get("initial"); err != nil || value != "state" {
		t.Fatalf("initial state = %#v, %v", value, err)
	}
	if value, err := got.Session.State().Get("updated"); err != nil || value != "state" {
		t.Fatalf("updated state = %#v, %v", value, err)
	}
	if want := []string{"event"}; !reflect.DeepEqual(conformanceEventIDs(got.Session), want) {
		t.Fatalf("event IDs = %#v, want %#v", conformanceEventIDs(got.Session), want)
	}
}

func openConformanceStore(t *testing.T) *sessionstore.Store {
	t.Helper()
	store, err := sessionstore.OpenWith(sessionstore.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func newDurableService(t *testing.T) (session.Service, session.Session) {
	t.Helper()
	service := NewSessionService(openConformanceStore(t))
	created, err := service.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "session"})
	if err != nil {
		t.Fatal(err)
	}
	return service, created.Session
}

func conformanceEventIDs(current session.Session) []string {
	ids := make([]string, 0, current.Events().Len())
	for event := range current.Events().All() {
		ids = append(ids, event.ID)
	}
	return ids
}
