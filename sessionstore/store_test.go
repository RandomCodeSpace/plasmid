package sessionstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/platform"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/session/sessiontestsuite"
	"google.golang.org/genai"

	"github.com/plasmid-dev/plasmid/warning"
)

type reentrantWarningSink struct {
	warn func(warning.Warning)
}

func (s reentrantWarningSink) Warn(value warning.Warning) { s.warn(value) }

func TestServiceConformance(t *testing.T) {
	sessiontestsuite.RunServiceTests(t, sessiontestsuite.SuiteOptions{
		SupportsUserProvidedSessionID: true,
		ProvidesServerAssignedEventID: false,
	}, func(t *testing.T) session.Service {
		store, err := Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		return store
	})
}

func TestStoreRestartPreservesFullNativeEventAndCreationTime(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	createdAt := time.Date(2026, 8, 17, 1, 2, 3, 4, time.UTC)
	ctx := platform.WithTimeProvider(t.Context(), func() time.Time { return createdAt })
	created, err := store.Create(ctx, &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "session", State: map[string]any{"initial": "state"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := created.Session.LastUpdateTime(); !got.Equal(createdAt) {
		t.Fatalf("creation time = %v, want %v", got, createdAt)
	}
	event := &session.Event{
		ID: "event", InvocationID: "invocation", Timestamp: createdAt.Add(time.Second), Author: "agent", Branch: "root.child", IsolationScope: "scope",
		LLMResponse:        model.LLMResponse{Content: genai.NewContentFromText("durable", genai.RoleModel), UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 3}},
		Actions:            session.EventActions{StateDelta: map[string]any{"updated": "state", "temp:drop": true}, ArtifactDelta: map[string]int64{"file": 2}, TransferToAgent: "next", Escalate: true},
		LongRunningToolIDs: []string{"tool"}, Routes: []string{"route"},
		RequestedInput: &session.RequestInput{InterruptID: "interrupt", Message: "approve", Payload: map[string]any{"n": float64(1)}},
		Output:         map[string]any{"answer": "yes"}, NodeInfo: &session.NodeInfo{Path: "workflow/node", MessageAsOutput: true, OutputFor: []string{"parent"}},
	}
	if err := store.AppendEvent(ctx, created.Session, event); err != nil {
		t.Fatal(err)
	}
	if _, exists := event.Actions.StateDelta["temp:drop"]; !exists {
		t.Fatal("AppendEvent mutated caller event")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	got, err := store.Get(t.Context(), &session.GetRequest{AppName: "app", UserID: "user", SessionID: "session"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Session.Events().Len() != 1 {
		t.Fatalf("event count = %d", got.Session.Events().Len())
	}
	restored := got.Session.Events().At(0)
	wantEvent := cloneEvent(event)
	wantEvent.Actions.StateDelta = withoutTemporaryState(wantEvent.Actions.StateDelta)
	if !reflect.DeepEqual(restored, wantEvent) {
		t.Fatalf("restored event differs:\n got: %#v\nwant: %#v", restored, wantEvent)
	}
	if _, err := got.Session.State().Get("temp:drop"); !errors.Is(err, session.ErrStateKeyNotExist) {
		t.Fatalf("temporary state persisted: %v", err)
	}
}

func TestCreateUsesPlatformProvidersAndListIncludesMergedState(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	wantTime := time.Date(2026, 8, 17, 4, 5, 6, 0, time.UTC)
	ctx := platform.WithUUIDProvider(t.Context(), func() string { return "generated" })
	ctx = platform.WithTimeProvider(ctx, func() time.Time { return wantTime })
	created, err := store.Create(ctx, &session.CreateRequest{AppName: "app", UserID: "user", State: map[string]any{"local": "value", "app:shared": "app", "user:shared": "user"}})
	if err != nil {
		t.Fatal(err)
	}
	if created.Session.ID() != "generated" || !created.Session.LastUpdateTime().Equal(wantTime) {
		t.Fatalf("created identity/time = %q/%v", created.Session.ID(), created.Session.LastUpdateTime())
	}
	listed, err := store.List(t.Context(), &session.ListRequest{AppName: "app", UserID: "user"})
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{"local": "value", "app:shared": "app", "user:shared": "user"} {
		got, err := listed.Sessions[0].State().Get(key)
		if err != nil || got != want {
			t.Fatalf("listed state %q = %#v, %v", key, got, err)
		}
	}
}

func TestCreateDirectorySyncFailureRetriesGeneratedTransactionAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	var warnings warning.SliceSink
	store, err := OpenWith(Options{Dir: dir, WarningSink: &warnings})
	if err != nil {
		t.Fatal(err)
	}
	ctx := platform.WithUUIDProvider(t.Context(), func() string { return "generated" })
	store.dirSyncHook = func(string) error { return errors.New("injected directory sync failure") }
	if created, err := store.Create(ctx, &session.CreateRequest{AppName: "app", UserID: "user"}); err == nil || created != nil {
		t.Fatalf("first Create = %#v, %v", created, err)
	}
	if got := warnings.Warnings(); len(got) != 1 || got[0].Code != warning.WarnSessionDurabilityRetry {
		t.Fatalf("warnings = %#v", got)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(ctx, &session.CreateRequest{AppName: "app", UserID: "user"})
	if err != nil || created.Session.ID() != "generated" {
		t.Fatalf("retry Create = %#v, %v", created, err)
	}
	if _, err := store.Get(ctx, &session.GetRequest{AppName: "app", UserID: "user", SessionID: "generated"}); err != nil {
		t.Fatalf("Get committed session = %v", err)
	}
	if _, err := store.Create(ctx, &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "generated"}); !errors.Is(err, ErrSessionExists) {
		t.Fatalf("explicit retry = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
}

func TestCreateDirectorySyncFailureRetriesExplicitTransactionConsistently(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	store.dirSyncHook = func(string) error { return errors.New("injected directory sync failure") }
	req := &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "explicit", State: map[string]any{"initial": "value"}}
	if _, err := store.Create(t.Context(), req); err == nil {
		t.Fatal("first Create succeeded before its directory barrier")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	conflict := &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "explicit", State: map[string]any{"initial": "different"}}
	if _, err := store.Create(t.Context(), conflict); !errors.Is(err, ErrSessionExists) {
		t.Fatalf("conflicting retry = %v", err)
	}
	created, err := store.Create(t.Context(), req)
	if err != nil || created.Session.ID() != "explicit" {
		t.Fatalf("matching retry = %#v, %v", created, err)
	}
}

func TestPostCommitProjectionFailureReturnsSuccessAndRepairsOnAccess(t *testing.T) {
	var warnings warning.SliceSink
	store, err := OpenWith(Options{Dir: t.TempDir(), WarningSink: &warnings})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	created, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "session"})
	if err != nil {
		t.Fatal(err)
	}
	var fail atomic.Bool
	fail.Store(true)
	store.projectHook = func() error {
		if fail.Load() {
			return errors.New("injected projection failure")
		}
		return nil
	}
	event := &session.Event{ID: "event", Actions: session.EventActions{StateDelta: map[string]any{"app:key": "durable"}}}
	if err := store.AppendEvent(t.Context(), created.Session, event); err != nil {
		t.Fatalf("post-commit append = %v", err)
	}
	if len(warnings.Warnings()) == 0 || warnings.Warnings()[len(warnings.Warnings())-1].Code != warning.WarnSessionSnapshotRefresh {
		t.Fatalf("warnings = %#v", warnings.Warnings())
	}
	fail.Store(false)
	got, err := store.Get(t.Context(), &session.GetRequest{AppName: "app", UserID: "user", SessionID: "session"})
	if err != nil {
		t.Fatal(err)
	}
	value, err := got.Session.State().Get("app:key")
	if err != nil || value != "durable" {
		t.Fatalf("repaired state = %#v, %v", value, err)
	}
	journal, _ := store.paths.appJournal("app")
	if data, err := store.paths.root.ReadFile(journal); err != nil || len(data) == 0 {
		t.Fatalf("repaired journal = %q, %v", data, err)
	}
}

func TestPostCommitCorruptJournalReturnsStoreWideSharedStateAndRepairsAfterRestart(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	first, err := store.Create(ctx, &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "first"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Create(ctx, &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "second"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AppendEvent(ctx, second.Session, &session.Event{ID: "shared", Actions: session.EventActions{StateDelta: map[string]any{
		"app:from-second":  "app-value",
		"user:from-second": "user-value",
	}}}); err != nil {
		t.Fatal(err)
	}
	appJournal, err := store.paths.appJournal("app")
	if err != nil {
		t.Fatal(err)
	}
	validJournal, err := store.paths.root.ReadFile(appJournal)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.paths.root.WriteFile(appJournal, []byte("corrupt\n"), fileMode); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendEvent(ctx, first.Session, &session.Event{ID: "local", Actions: session.EventActions{StateDelta: map[string]any{
		"local": "first-value",
	}}}); err != nil {
		t.Fatalf("post-commit append = %v", err)
	}
	for key, want := range map[string]any{
		"app:from-second":  "app-value",
		"user:from-second": "user-value",
		"local":            "first-value",
	} {
		got, err := first.Session.State().Get(key)
		if err != nil || got != want {
			t.Fatalf("returned state %q = %#v, %v", key, got, err)
		}
	}
	if err := store.paths.root.WriteFile(appJournal, validJournal, fileMode); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, &session.GetRequest{AppName: "app", UserID: "user", SessionID: "first"}); err != nil {
		t.Fatalf("repair Get = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	restarted, err := store.Get(ctx, &session.GetRequest{AppName: "app", UserID: "user", SessionID: "first"})
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{
		"app:from-second":  "app-value",
		"user:from-second": "user-value",
		"local":            "first-value",
	} {
		got, err := restarted.Session.State().Get(key)
		if err != nil || got != want {
			t.Fatalf("restarted state %q = %#v, %v", key, got, err)
		}
	}
}

func TestPostCommitCorruptJournalPreservesNewerDeletedSessionSharedState(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	active, err := store.Create(ctx, &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AppendEvent(ctx, active.Session, &session.Event{ID: "old", Actions: session.EventActions{StateDelta: map[string]any{
		"app:key":  "old",
		"user:key": "old",
	}}}); err != nil {
		t.Fatal(err)
	}
	deleted, err := store.Create(ctx, &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "deleted"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AppendEvent(ctx, deleted.Session, &session.Event{ID: "new", Actions: session.EventActions{StateDelta: map[string]any{
		"app:key":  "new",
		"user:key": "new",
	}}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, &session.DeleteRequest{AppName: "app", UserID: "user", SessionID: "deleted"}); err != nil {
		t.Fatal(err)
	}
	appJournal, err := store.paths.appJournal("app")
	if err != nil {
		t.Fatal(err)
	}
	validJournal, err := store.paths.root.ReadFile(appJournal)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.paths.root.WriteFile(appJournal, []byte("corrupt\n"), fileMode); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendEvent(ctx, active.Session, &session.Event{ID: "local", Actions: session.EventActions{StateDelta: map[string]any{"local": "value"}}}); err != nil {
		t.Fatalf("post-commit append = %v", err)
	}
	for key, want := range map[string]any{"app:key": "new", "user:key": "new", "local": "value"} {
		got, err := active.Session.State().Get(key)
		if err != nil || got != want {
			t.Fatalf("returned state %q = %#v, %v", key, got, err)
		}
	}
	if err := store.paths.root.WriteFile(appJournal, validJournal, fileMode); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, &session.GetRequest{AppName: "app", UserID: "user", SessionID: "active"}); err != nil {
		t.Fatalf("repair Get = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	restarted, err := store.Get(ctx, &session.GetRequest{AppName: "app", UserID: "user", SessionID: "active"})
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{"app:key": "new", "user:key": "new", "local": "value"} {
		got, err := restarted.Session.State().Get(key)
		if err != nil || got != want {
			t.Fatalf("restarted state %q = %#v, %v", key, got, err)
		}
	}
}

func TestVersionedProjectionHandlesLateLowerOrderRecords(t *testing.T) {
	known := sharedProjection{
		App:          map[string]any{"same": "new"},
		User:         map[string]any{},
		AppVersions:  map[string]keyVersion{"same": {Order: 3, RecordID: "new"}},
		UserVersions: map[string]keyVersion{},
		Records:      []stateJournalRecord{{V: 1, ID: "new", Order: 3, Delta: map[string]any{"same": "new"}}},
	}
	got, err := mergeKnownSharedRecords(known, []stateRecord{{
		ID: "late", Order: 2, UserID: "user", AppDelta: map[string]any{"same": "old", "unseen": "applied"},
	}}, "user")
	if err != nil {
		t.Fatal(err)
	}
	if got.App["same"] != "new" || got.App["unseen"] != "applied" {
		t.Fatalf("projection = %#v", got.App)
	}
	if got.AppVersions["unseen"] != (keyVersion{Order: 2, RecordID: "late"}) {
		t.Fatalf("unseen version = %#v", got.AppVersions["unseen"])
	}
	if _, err := mergeKnownSharedRecords(known, []stateRecord{{
		ID: "new", Order: 3, UserID: "user", AppDelta: map[string]any{"same": "new"},
	}}, "user"); err != nil {
		t.Fatalf("idempotent equal-version record = %v", err)
	}
	if _, err := mergeKnownSharedRecords(known, []stateRecord{{
		ID: "different", Order: 3, UserID: "user", AppDelta: map[string]any{"same": "conflict"},
	}}, "user"); err == nil {
		t.Fatal("equal-order conflicting record was accepted")
	}
}

func TestAppendRejectsCorruptAuthorityBeforeCommitWithoutProjectionCache(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	created, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "session"})
	if err != nil {
		t.Fatal(err)
	}
	app := store.appLocks("app")
	app.cacheMu.Lock()
	app.appKnown = false
	app.users = nil
	app.cacheMu.Unlock()
	journal, err := store.paths.appJournal("app")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.paths.root.WriteFile(journal, []byte("corrupt\n"), fileMode); err != nil {
		t.Fatal(err)
	}
	name := store.paths.sessionLog("app", "user", "session")
	before, err := store.paths.root.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AppendEvent(t.Context(), created.Session, &session.Event{ID: "must-not-commit"}); err == nil {
		t.Fatal("AppendEvent committed without an authoritative projection baseline")
	}
	after, err := store.paths.root.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatalf("transcript changed before baseline: before %q, after %q", before, after)
	}
}

func TestGetAndListFallbackRetainDeletedSessionSharedState(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	_, err = store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "deleted", State: map[string]any{"app:deleted": "app", "user:deleted": "user"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(t.Context(), &session.DeleteRequest{AppName: "app", UserID: "user", SessionID: "deleted"}); err != nil {
		t.Fatal(err)
	}
	_, err = store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "active"})
	if err != nil {
		t.Fatal(err)
	}
	store.projectHook = func() error { return errors.New("injected projection failure") }
	got, err := store.Get(t.Context(), &session.GetRequest{AppName: "app", UserID: "user", SessionID: "active"})
	if err != nil {
		t.Fatal(err)
	}
	listed, err := store.List(t.Context(), &session.ListRequest{AppName: "app", UserID: "user"})
	if err != nil {
		t.Fatal(err)
	}
	for _, current := range []session.Session{got.Session, listed.Sessions[0]} {
		for key, want := range map[string]any{"app:deleted": "app", "user:deleted": "user"} {
			value, err := current.State().Get(key)
			if err != nil || value != want {
				t.Fatalf("%s = %#v, %v", key, value, err)
			}
		}
	}
}

func TestGetAndListReturnErrorWhenAuthoritativeJournalsAreUnreadable(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	_, err = store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "deleted", State: map[string]any{"app:retained": "value"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(t.Context(), &session.DeleteRequest{AppName: "app", UserID: "user", SessionID: "deleted"}); err != nil {
		t.Fatal(err)
	}
	_, err = store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "session"})
	if err != nil {
		t.Fatal(err)
	}
	journal, _ := store.paths.appJournal("app")
	if err := store.paths.root.WriteFile(journal, []byte("corrupt\n"), fileMode); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(t.Context(), &session.GetRequest{AppName: "app", UserID: "user", SessionID: "session"}); err == nil {
		t.Fatal("Get reconstructed state without its authoritative journal")
	}
	if _, err := store.List(t.Context(), &session.ListRequest{AppName: "app", UserID: "user"}); err == nil {
		t.Fatal("List reconstructed state without its authoritative journal")
	}
}

func TestGetAndListRejectMissingJournalInsteadOfOverwritingRetainedSnapshot(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	_, err = store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "deleted", State: map[string]any{"app:retained": "value"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(t.Context(), &session.DeleteRequest{AppName: "app", UserID: "user", SessionID: "deleted"}); err != nil {
		t.Fatal(err)
	}
	_, err = store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "active"})
	if err != nil {
		t.Fatal(err)
	}
	journal, _ := store.paths.appJournal("app")
	snapshot, _ := store.paths.appState("app")
	wantSnapshot, err := store.paths.root.ReadFile(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.paths.root.Remove(journal); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(t.Context(), &session.GetRequest{AppName: "app", UserID: "user", SessionID: "active"}); err == nil {
		t.Fatal("Get accepted a missing authoritative journal")
	}
	if _, err := store.List(t.Context(), &session.ListRequest{AppName: "app", UserID: "user"}); err == nil {
		t.Fatal("List accepted a missing authoritative journal")
	}
	gotSnapshot, err := store.paths.root.ReadFile(snapshot)
	if err != nil || !bytes.Equal(gotSnapshot, wantSnapshot) {
		t.Fatalf("snapshot changed after failed repair: %q, %v", gotSnapshot, err)
	}
}

func TestWarningSinkCanReenterStoreAfterProjectionFailure(t *testing.T) {
	var store *Store
	var entered atomic.Bool
	reentered := make(chan error, 1)
	sink := reentrantWarningSink{warn: func(warning.Warning) {
		if !entered.CompareAndSwap(false, true) {
			return
		}
		store.projectHook = nil
		_, err := store.Get(context.Background(), &session.GetRequest{AppName: "app", UserID: "user", SessionID: "session"})
		reentered <- err
	}}
	var err error
	store, err = OpenWith(Options{Dir: t.TempDir(), WarningSink: sink})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	created, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "session"})
	if err != nil {
		t.Fatal(err)
	}
	store.projectHook = func() error { return errors.New("injected projection failure") }
	done := make(chan error, 1)
	go func() {
		done <- store.AppendEvent(context.Background(), created.Session, &session.Event{ID: "event"})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("AppendEvent deadlocked while emitting a warning")
	}
	select {
	case err := <-reentered:
		if err != nil {
			t.Fatalf("reentrant Get = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("warning sink did not reenter Store")
	}
}

func TestWarningSinkCanCloseStoreAfterProjectionFailure(t *testing.T) {
	var store *Store
	closed := make(chan error, 1)
	var entered atomic.Bool
	sink := reentrantWarningSink{warn: func(warning.Warning) {
		if entered.CompareAndSwap(false, true) {
			closed <- store.Close()
		}
	}}
	var err error
	store, err = OpenWith(Options{Dir: t.TempDir(), WarningSink: sink})
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "session"})
	if err != nil {
		t.Fatal(err)
	}
	store.projectHook = func() error { return errors.New("injected projection failure") }
	done := make(chan error, 1)
	go func() {
		done <- store.AppendEvent(context.Background(), created.Session, &session.Event{ID: "event"})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("AppendEvent deadlocked while WarningSink closed Store")
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WarningSink did not close Store")
	}
}

func TestDerivedSnapshotIsRebuiltFromAuthoritativeJournal(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	_, err = store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "session", State: map[string]any{"app:key": "journal"}})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, _ := store.paths.appState("app")
	if err := store.paths.root.WriteFile(snapshot, []byte("corrupt"), fileMode); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(t.Context(), &session.GetRequest{AppName: "app", UserID: "user", SessionID: "session"}); err != nil {
		t.Fatal(err)
	}
	data, err := store.paths.root.ReadFile(snapshot)
	if err != nil || !json.Valid(data) {
		t.Fatalf("repaired snapshot = %q, %v", data, err)
	}
}

func TestMissingOrderSequenceIsRebuiltFromTranscripts(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	created, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "session"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AppendEvent(t.Context(), created.Session, &session.Event{ID: "one"}); err != nil {
		t.Fatal(err)
	}
	sequence := store.paths.appSequence("app")
	if err := store.paths.root.Remove(sequence); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendEvent(t.Context(), created.Session, &session.Event{ID: "two"}); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(t.Context(), &session.GetRequest{AppName: "app", UserID: "user", SessionID: "session"})
	if err != nil || !reflect.DeepEqual(eventIDs(got.Session), []string{"one", "two"}) {
		t.Fatalf("recovered sequence events = %#v, %v", eventIDs(got.Session), err)
	}
}

func TestJournalIdentityIncludesUserAndSessionWithoutDelimiterAmbiguity(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for _, user := range []string{"first:user", "second"} {
		if _, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: user, SessionID: "same:id", State: map[string]any{"app:key": user}}); err != nil {
			t.Fatal(err)
		}
	}
	name, _ := store.paths.appJournal("app")
	journal, err := store.loadJournal(name, new(warningBuffer))
	if err != nil || len(journal) != 2 || journal[0].ID == journal[1].ID {
		t.Fatalf("journal identities = %#v, %v", journal, err)
	}
}

func TestDeleteAndRecreateSameIdentityUsesNewJournalIncarnation(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	first, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "same", State: map[string]any{"app:key": "first"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(t.Context(), &session.DeleteRequest{AppName: "app", UserID: "user", SessionID: first.Session.ID()}); err != nil {
		t.Fatal(err)
	}
	second, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "same", State: map[string]any{"app:key": "second"}})
	if err != nil {
		t.Fatal(err)
	}
	value, err := second.Session.State().Get("app:key")
	if err != nil || value != "second" {
		t.Fatalf("recreated state = %#v, %v", value, err)
	}
}

func TestProjectionReplaysJournalByLogicalOrder(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	name, _ := store.paths.appJournal("app")
	if err := store.paths.ensureParent(name); err != nil {
		t.Fatal(err)
	}
	for _, record := range []stateJournalRecord{
		{V: 1, ID: "later", Order: 2, Delta: map[string]any{"key": "later"}},
		{V: 1, ID: "earlier", Order: 1, Delta: map[string]any{"key": "earlier"}},
	} {
		data := marshalJournalRecord(record)
		if err := store.appendFile(name, data); err != nil {
			t.Fatal(err)
		}
	}
	created, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "session"})
	if err != nil {
		t.Fatal(err)
	}
	value, err := created.Session.State().Get("app:key")
	if err != nil || value != "later" {
		t.Fatalf("ordered projection = %#v, %v", value, err)
	}
}

func TestSequenceRecoveryIncludesSkippedFutureRecordOrder(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "first"})
	if err != nil {
		t.Fatal(err)
	}
	name := store.paths.sessionLog("app", "user", "first")
	file, _ := store.paths.root.OpenFile(name, os.O_APPEND|os.O_WRONLY, fileMode)
	_, _ = file.WriteString("{\"v\":99,\"type\":\"future\",\"order\":100}\n")
	_ = file.Close()
	sequence := store.paths.appSequence("app")
	if err := store.paths.root.Remove(sequence); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	_, err = store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "second"})
	if err != nil {
		t.Fatal(err)
	}
	secondName := store.paths.sessionLog("app", "user", "second")
	log, err := loadSessionLog(store.paths, secondName, "app", "user", "second", store.fsync, new(warningBuffer))
	if err != nil || log.header.Incarnation <= 100 {
		t.Fatalf("recovered incarnation = %d, %v", log.header.Incarnation, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "third"}); err != nil {
		t.Fatalf("Create after future record projection restart = %v", err)
	}
}

func TestOrderRecoveryIncludesDeletedTranscriptJournals(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	created, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "deleted"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AppendEvent(t.Context(), created.Session, &session.Event{ID: "highest"}); err != nil {
		t.Fatal(err)
	}
	journalName, _ := store.paths.appJournal("app")
	journal, _ := store.loadJournal(journalName, new(warningBuffer))
	retained := journal[len(journal)-1].Order
	if err := store.Delete(t.Context(), &session.DeleteRequest{AppName: "app", UserID: "user", SessionID: "deleted"}); err != nil {
		t.Fatal(err)
	}
	sequence := store.paths.appSequence("app")
	if err := store.paths.root.Remove(sequence); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "replacement"}); err != nil {
		t.Fatal(err)
	}
	name := store.paths.sessionLog("app", "user", "replacement")
	log, err := loadSessionLog(store.paths, name, "app", "user", "replacement", store.fsync, new(warningBuffer))
	if err != nil || log.stateRecords[0].Order <= retained {
		t.Fatalf("replacement order = %d, retained = %d, err = %v", log.stateRecords[0].Order, retained, err)
	}
}

func TestCreateRepairsStaleSequenceFromRetainedDeletedRecordBeforeTranscript(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	deleted, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "deleted"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(t.Context(), &session.DeleteRequest{AppName: "app", UserID: "user", SessionID: deleted.Session.ID()}); err != nil {
		t.Fatal(err)
	}
	journalName, _ := store.paths.appJournal("app")
	journal, err := store.loadJournal(journalName, new(warningBuffer))
	if err != nil {
		t.Fatal(err)
	}
	retainedOrder := journal[len(journal)-1].Order
	sequence := store.paths.appSequence("app")
	if err := writeUint64File(store.paths.root, sequence, 0, store.fsync); err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "created"})
	if err != nil {
		t.Fatalf("Create with stale sequence = %v", err)
	}
	name := store.paths.sessionLog("app", "user", created.Session.ID())
	log, err := loadSessionLog(store.paths, name, "app", "user", created.Session.ID(), store.fsync, new(warningBuffer))
	if err != nil || log.header.Incarnation <= retainedOrder || len(log.events) != 0 {
		t.Fatalf("created transcript = incarnation %d, events %d, retained %d, err %v", log.header.Incarnation, len(log.events), retainedOrder, err)
	}
	if _, err := store.scanApp("app", new(warningBuffer)); err != nil {
		t.Fatalf("scan after Create = %v", err)
	}
}

func TestAppendRepairsStaleSequenceFromRetainedCacheBeforeTranscript(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	deleted, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "deleted"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(t.Context(), &session.DeleteRequest{AppName: "app", UserID: "user", SessionID: deleted.Session.ID()}); err != nil {
		t.Fatal(err)
	}
	active, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "active"})
	if err != nil {
		t.Fatal(err)
	}
	name := store.paths.sessionLog("app", "user", active.Session.ID())
	before, err := store.paths.root.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	sequence := store.paths.appSequence("app")
	if err := writeUint64File(store.paths.root, sequence, 0, store.fsync); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendEvent(t.Context(), active.Session, &session.Event{ID: "event"}); err != nil {
		t.Fatalf("AppendEvent with stale sequence = %v", err)
	}
	after, err := store.paths.root.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(after, []byte{'\n'}) != bytes.Count(before, []byte{'\n'})+1 {
		t.Fatalf("transcript line count changed unexpectedly: before %d, after %d", bytes.Count(before, []byte{'\n'}), bytes.Count(after, []byte{'\n'}))
	}
	log, err := loadSessionLog(store.paths, name, "app", "user", active.Session.ID(), store.fsync, new(warningBuffer))
	if err != nil || len(log.events) != 1 || log.stateRecords[len(log.stateRecords)-1].Order <= log.header.Incarnation {
		t.Fatalf("appended transcript = records %#v, events %d, err %v", log.stateRecords, len(log.events), err)
	}
	if _, err := store.scanApp("app", new(warningBuffer)); err != nil {
		t.Fatalf("scan after AppendEvent = %v", err)
	}
}

func TestCreateRecoversCommittedUnprojectedTranscriptBeforeReservingOrder(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	active, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "active"})
	if err != nil {
		t.Fatal(err)
	}
	name := store.paths.sessionLog("app", "user", active.Session.ID())
	appendTranscriptRecord(t, store, name, record{V: recordVersion, Type: recordEvent, Order: 2, Event: &session.Event{ID: "crash-committed"}})
	sequence := store.paths.appSequence("app")
	if err := writeUint64File(store.paths.root, sequence, 1, store.fsync); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	created, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "created"})
	if err != nil {
		t.Fatalf("Create after crash gap = %v", err)
	}
	createdName := store.paths.sessionLog("app", "user", created.Session.ID())
	log, err := loadSessionLog(store.paths, createdName, "app", "user", created.Session.ID(), store.fsync, new(warningBuffer))
	if err != nil || log.header.Incarnation != 3 {
		t.Fatalf("created incarnation = %d, %v", log.header.Incarnation, err)
	}
	if _, err := store.scanApp("app", new(warningBuffer)); err != nil {
		t.Fatalf("scan after Create = %v", err)
	}
}

func TestAppendRecoversCommittedUnprojectedTranscriptBeforeReservingOrder(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	active, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "active"})
	if err != nil {
		t.Fatal(err)
	}
	name := store.paths.sessionLog("app", "user", active.Session.ID())
	appendTranscriptRecord(t, store, name, record{V: recordVersion, Type: recordEvent, Order: 2, Event: &session.Event{ID: "crash-committed"}})
	sequence := store.paths.appSequence("app")
	if err := writeUint64File(store.paths.root, sequence, 1, store.fsync); err != nil {
		t.Fatal(err)
	}
	header := cloneHeader(active.Session.(*durableSession).header)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	restartedHandle := newDurableSession(store, header, nil, header.State, header.CreatedAt)
	if err := store.AppendEvent(t.Context(), restartedHandle, &session.Event{ID: "after-restart-gap"}); err != nil {
		t.Fatalf("AppendEvent after crash gap = %v", err)
	}
	log, err := loadSessionLog(store.paths, name, "app", "user", restartedHandle.ID(), store.fsync, new(warningBuffer))
	if err != nil || len(log.events) != 2 || log.stateRecords[len(log.stateRecords)-1].Order != 3 {
		t.Fatalf("appended records = %#v, events %d, err %v", log.stateRecords, len(log.events), err)
	}
	if _, err := store.scanApp("app", new(warningBuffer)); err != nil {
		t.Fatalf("scan after AppendEvent = %v", err)
	}
}

func TestPendingCreateMarkerReservesOrderAndValidatesBeforeResumeTranscript(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	state := map[string]any{"app:key": "pending"}
	stateHash, err := createStateHash(state)
	if err != nil {
		t.Fatal(err)
	}
	local, appDelta, userDelta := splitState(state)
	header := header{ID: "pending", AppName: "app", UserID: "user", State: local, AppDelta: appDelta, UserDelta: userDelta, CreatedAt: time.Now().UTC(), Incarnation: 1}
	name := store.paths.sessionLog("app", "user", header.ID)
	if err := store.paths.ensureParent(name); err != nil {
		t.Fatal(err)
	}
	if err := store.writeCreateMarker(name, createMarker{V: createMarkerVersion, StateHash: stateHash, Header: header}); err != nil {
		t.Fatal(err)
	}
	sequence := store.paths.appSequence("app")
	if err := writeUint64File(store.paths.root, sequence, 0, store.fsync); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "other"})
	if err != nil {
		t.Fatalf("Create beside pending marker = %v", err)
	}
	otherName := store.paths.sessionLog("app", "user", other.Session.ID())
	otherLog, err := loadSessionLog(store.paths, otherName, "app", "user", other.Session.ID(), store.fsync, new(warningBuffer))
	if err != nil || otherLog.header.Incarnation != 2 {
		t.Fatalf("other incarnation = %d, %v", otherLog.header.Incarnation, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	resumed, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "pending", State: state})
	if err != nil || resumed.Session.ID() != "pending" {
		t.Fatalf("resume pending Create = %#v, %v", resumed, err)
	}
	pendingLog, err := loadSessionLog(store.paths, name, "app", "user", "pending", store.fsync, new(warningBuffer))
	if err != nil || pendingLog.header.Incarnation != 1 {
		t.Fatalf("pending incarnation = %d, %v", pendingLog.header.Incarnation, err)
	}
	if _, err := store.scanApp("app", new(warningBuffer)); err != nil {
		t.Fatalf("scan after resume = %v", err)
	}
}

func TestResumeCreateRejectsMismatchedMarkerFingerprintBeforeTranscriptWrite(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "session", State: map[string]any{"app:key": "original", "user:key": "original"}})
	if err != nil {
		t.Fatal(err)
	}
	name := store.paths.sessionLog("app", "user", created.Session.ID())
	before, err := store.paths.root.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	changed := map[string]any{"app:key": "changed", "user:key": "changed"}
	stateHash, err := createStateHash(changed)
	if err != nil {
		t.Fatal(err)
	}
	header := cloneHeader(created.Session.(*durableSession).header)
	_, header.AppDelta, header.UserDelta = splitState(changed)
	if err := store.writeCreateMarker(name, createMarker{V: createMarkerVersion, StateHash: stateHash, Header: header}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	_, err = store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "session", State: changed})
	if !errors.Is(err, ErrCorruptLog) {
		t.Fatalf("Create with contradictory marker = %v, want ErrCorruptLog", err)
	}
	after, readErr := store.paths.root.ReadFile(name)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("transcript changed before marker fingerprint validation")
	}
}

func TestDistinctSessionReservationDoesNotRepeatSlowInventoryScan(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	first, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "first"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "second"})
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	var scans atomic.Int32
	store.inventoryHook = func(string) {
		scans.Add(1)
		<-release
	}
	committed := make(chan string, 2)
	store.commitHook = func(id string) { committed <- id }
	errs := make(chan error, 2)
	go func() { errs <- store.AppendEvent(t.Context(), first.Session, &session.Event{ID: "first-event"}) }()
	go func() { errs <- store.AppendEvent(t.Context(), second.Session, &session.Event{ID: "second-event"}) }()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	seen := make(map[string]bool)
	for len(seen) < 2 {
		select {
		case id := <-committed:
			seen[id] = true
		case <-timer.C:
			close(release)
			t.Fatalf("distinct reservations stalled behind repeated inventory scan; commits = %v", seen)
		}
	}
	close(release)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if got := scans.Load(); got != 0 {
		t.Fatalf("inventory rescans = %d, want 0 after initialization", got)
	}
}

func TestAppendSequenceWriteFailureReleasesReservationForExactRetry(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	created, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "session"})
	if err != nil {
		t.Fatal(err)
	}
	name := store.paths.sessionLog("app", "user", "session")
	before, err := store.paths.root.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	var failed atomic.Bool
	store.sequenceHook = func() error {
		if failed.CompareAndSwap(false, true) {
			return errors.New("injected sequence fsync failure")
		}
		return nil
	}
	event := &session.Event{ID: "retry-event", Actions: session.EventActions{StateDelta: map[string]any{"app:key": "value"}}}
	if err := store.AppendEvent(t.Context(), created.Session, event); err == nil {
		t.Fatal("AppendEvent acknowledged injected sequence failure")
	}
	after, err := store.paths.root.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("transcript changed before sequence reservation was durable")
	}
	if err := store.AppendEvent(t.Context(), created.Session, event); err != nil {
		t.Fatalf("exact AppendEvent retry = %v", err)
	}
	got, err := store.Get(t.Context(), &session.GetRequest{AppName: "app", UserID: "user", SessionID: "session"})
	if err != nil || got.Session.Events().Len() != 1 {
		t.Fatalf("events after retry = %d, %v", got.Session.Events().Len(), err)
	}
}

func TestCreateSequenceWriteFailureReleasesReservationForExactRetry(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	var failed atomic.Bool
	store.sequenceHook = func() error {
		if failed.CompareAndSwap(false, true) {
			return errors.New("injected sequence write failure")
		}
		return nil
	}
	req := &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "retry", State: map[string]any{"app:key": "value"}}
	if _, err := store.Create(t.Context(), req); err == nil {
		t.Fatal("Create acknowledged injected sequence failure")
	}
	name := store.paths.sessionLog("app", "user", "retry")
	if _, err := store.paths.root.Stat(name); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("transcript after failed reservation = %v", err)
	}
	created, err := store.Create(t.Context(), req)
	if err != nil {
		t.Fatalf("exact Create retry = %v", err)
	}
	log, err := loadSessionLog(store.paths, name, "app", "user", created.Session.ID(), store.fsync, new(warningBuffer))
	if err != nil || log.header.Incarnation != 2 {
		t.Fatalf("retry incarnation = %d, %v", log.header.Incarnation, err)
	}
}

func TestInventoryInitializationAndDeleteShareConsistentProjectionCut(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	target, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "target"})
	if err != nil {
		t.Fatal(err)
	}
	targetName := store.paths.sessionLog("app", "user", "target")
	appendTranscriptRecord(t, store, targetName, record{V: recordVersion, Type: recordEvent, Order: 2, Event: &session.Event{ID: "crash-committed"}})
	sequence := store.paths.appSequence("app")
	if err := writeUint64File(store.paths.root, sequence, 1, store.fsync); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	inventoryEntered := make(chan struct{})
	deleteScanned := make(chan struct{})
	releaseInventory := make(chan struct{})
	var inventoryOnce sync.Once
	var deleteOnce sync.Once
	store.inventoryHook = func(name string) {
		if name == targetName {
			inventoryOnce.Do(func() { close(inventoryEntered) })
			<-releaseInventory
		}
	}
	store.scanEntryHook = func(name string) {
		if name == targetName {
			deleteOnce.Do(func() { close(deleteScanned) })
		}
	}
	createdCh := make(chan *session.CreateResponse, 1)
	createErr := make(chan error, 1)
	go func() {
		created, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "new"})
		createdCh <- created
		createErr <- err
	}()
	select {
	case <-inventoryEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("inventory initialization did not reach target transcript")
	}
	deleteErr := make(chan error, 1)
	go func() {
		deleteErr <- store.Delete(t.Context(), &session.DeleteRequest{AppName: "app", UserID: "user", SessionID: target.Session.ID()})
	}()
	select {
	case <-deleteScanned:
	case <-time.After(3 * time.Second):
		close(releaseInventory)
		t.Fatal("Delete did not scan target before projection cut")
	}
	close(releaseInventory)
	created := <-createdCh
	if err := <-createErr; err != nil {
		t.Fatalf("concurrent Create = %v", err)
	}
	if err := <-deleteErr; err != nil {
		t.Fatalf("concurrent Delete = %v", err)
	}
	newName := store.paths.sessionLog("app", "user", created.Session.ID())
	log, err := loadSessionLog(store.paths, newName, "app", "user", created.Session.ID(), store.fsync, new(warningBuffer))
	if err != nil || log.header.Incarnation <= 2 {
		t.Fatalf("new incarnation = %d, want > 2; err = %v", log.header.Incarnation, err)
	}
	if _, err := store.scanApp("app", new(warningBuffer)); err != nil {
		t.Fatalf("scan after concurrent initialization/Delete = %v", err)
	}
}

func appendTranscriptRecord(t *testing.T, store *Store, name string, value record) {
	t.Helper()
	data, err := recordLine(value)
	if err != nil {
		t.Fatal(err)
	}
	file, err := store.paths.root.OpenFile(name, os.O_WRONLY|os.O_APPEND, fileMode)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(data); err == nil && store.fsync {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		t.Fatal(err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
}

func TestPostCommitScanFailurePreservesKnownSharedStateAndAppliesDelta(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	current, _ := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "current", State: map[string]any{"app:known": "kept"}})
	broken, _ := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "other", SessionID: "broken"})
	name := store.paths.sessionLog("app", "other", broken.Session.ID())
	if err := store.paths.root.WriteFile(name, []byte("corrupt\n"), fileMode); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendEvent(t.Context(), current.Session, &session.Event{ID: "committed", Actions: session.EventActions{StateDelta: map[string]any{"app:new": "applied"}}}); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{"app:known": "kept", "app:new": "applied"} {
		got, err := current.Session.State().Get(key)
		if err != nil || got != want {
			t.Fatalf("state %q = %#v, %v", key, got, err)
		}
	}
}

func TestDeleteRetriesDirectorySyncAfterTranscriptRemoval(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	_, err = store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "session"})
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	store.dirSyncHook = func(string) error {
		if calls.Add(1) == 1 {
			return errors.New("injected directory sync failure")
		}
		return nil
	}
	req := &session.DeleteRequest{AppName: "app", UserID: "user", SessionID: "session"}
	if err := store.Delete(t.Context(), req); err == nil {
		t.Fatal("Delete succeeded before directory sync")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	store.dirSyncHook = func(string) error {
		calls.Add(1)
		return nil
	}
	if err := store.Delete(t.Context(), req); err != nil {
		t.Fatalf("Delete retry = %v", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("directory sync calls = %d", calls.Load())
	}
}

func TestDeleteRetriesMarkerRemovalSyncAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "session"})
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	store.dirSyncHook = func(string) error {
		if calls.Add(1) == 2 {
			return errors.New("marker removal sync")
		}
		return nil
	}
	req := &session.DeleteRequest{AppName: "app", UserID: "user", SessionID: "session"}
	if err := store.Delete(t.Context(), req); err == nil {
		t.Fatal("Delete acknowledged failed marker barrier")
	}
	_ = store.Close()
	store, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Delete(t.Context(), req); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteDurablyRetiresPendingCreateTransaction(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	store.dirSyncHook = func(string) error { return errors.New("injected create directory sync") }
	req := &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "pending", State: map[string]any{"local": "old"}}
	if _, err := store.Create(t.Context(), req); err == nil {
		t.Fatal("Create acknowledged failed directory barrier")
	}
	name := store.paths.sessionLog("app", "user", "pending")
	marker, exists, err := store.readCreateMarker(name)
	if err != nil || !exists {
		t.Fatalf("pending marker = %#v, %v, %v", marker, exists, err)
	}
	store.dirSyncHook = nil
	if err := store.Delete(t.Context(), &session.DeleteRequest{AppName: "app", UserID: "user", SessionID: "pending"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.paths.root.Stat(createMarkerName(name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("create marker survived Delete: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	replacement, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "pending", State: map[string]any{"local": "new"}})
	if err != nil {
		t.Fatal(err)
	}
	if replacement.Session.(*durableSession).header.Incarnation == marker.Header.Incarnation {
		t.Fatal("Create resumed the deleted transaction")
	}
}

func TestConsecutiveProjectionFailuresRetainEveryCommittedDeltaAfterRestart(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "session"})
	if err != nil {
		t.Fatal(err)
	}
	store.journalHook = func() error { return errors.New("injected journal failure") }
	for _, event := range []*session.Event{
		{ID: "one", Actions: session.EventActions{StateDelta: map[string]any{"app:first": "one"}}},
		{ID: "two", Actions: session.EventActions{StateDelta: map[string]any{"app:second": "two"}}},
	} {
		if err := store.AppendEvent(t.Context(), created.Session, event); err != nil {
			t.Fatal(err)
		}
	}
	for key, want := range map[string]any{"app:first": "one", "app:second": "two"} {
		got, err := created.Session.State().Get(key)
		if err != nil || got != want {
			t.Fatalf("live %s = %#v, %v", key, got, err)
		}
	}
	_ = store.Close()
	store, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	got, err := store.Get(t.Context(), &session.GetRequest{AppName: "app", UserID: "user", SessionID: "session"})
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{"app:first": "one", "app:second": "two"} {
		value, err := got.Session.State().Get(key)
		if err != nil || value != want {
			t.Fatalf("restart %s = %#v, %v", key, value, err)
		}
	}
}

func TestConsecutiveProjectionFailuresRecoverCommittedDeltasAcrossSessions(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "first"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "second"})
	if err != nil {
		t.Fatal(err)
	}
	store.journalHook = func() error { return errors.New("injected journal failure") }
	if err := store.AppendEvent(t.Context(), first.Session, &session.Event{ID: "first-event", Actions: session.EventActions{StateDelta: map[string]any{"app:first": "one", "user:first": "one"}}}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendEvent(t.Context(), second.Session, &session.Event{ID: "second-event", Actions: session.EventActions{StateDelta: map[string]any{"app:second": "two", "user:second": "two"}}}); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"app:first": "one", "user:first": "one", "app:second": "two", "user:second": "two"}
	for key, value := range want {
		got, err := second.Session.State().Get(key)
		if err != nil || got != value {
			t.Fatalf("live %s = %#v, %v", key, got, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	got, err := store.Get(t.Context(), &session.GetRequest{AppName: "app", UserID: "user", SessionID: "second"})
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range want {
		state, err := got.Session.State().Get(key)
		if err != nil || state != value {
			t.Fatalf("restart %s = %#v, %v", key, state, err)
		}
	}
}

func TestPendingSharedDeltasSurviveLaterCrossSessionScanFailure(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	first, _ := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "first"})
	second, _ := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "second"})
	broken, _ := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "other", SessionID: "broken"})
	store.journalHook = func() error { return errors.New("injected journal failure") }
	if err := store.AppendEvent(t.Context(), first.Session, &session.Event{ID: "first-event", Actions: session.EventActions{StateDelta: map[string]any{"app:first": "one", "user:first": "one"}}}); err != nil {
		t.Fatal(err)
	}
	store.journalHook = nil
	brokenName := store.paths.sessionLog("app", "other", broken.Session.ID())
	if err := store.paths.root.WriteFile(brokenName, []byte("corrupt\n"), fileMode); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendEvent(t.Context(), second.Session, &session.Event{ID: "second-event", Actions: session.EventActions{StateDelta: map[string]any{"app:second": "two", "user:second": "two"}}}); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{"app:first": "one", "user:first": "one", "app:second": "two", "user:second": "two"} {
		value, err := second.Session.State().Get(key)
		if err != nil || value != want {
			t.Fatalf("%s = %#v, %v", key, value, err)
		}
	}
}

func TestAppendEventRequiresOwnedDurableSession(t *testing.T) {
	first, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	created, err := first.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "session"})
	if err != nil {
		t.Fatal(err)
	}
	foreign := struct{ session.Session }{Session: created.Session}
	if err := first.AppendEvent(t.Context(), foreign, &session.Event{ID: "foreign"}); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("foreign implementation = %v", err)
	}
	second, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	if err := second.AppendEvent(t.Context(), created.Session, &session.Event{ID: "other-store"}); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("other Store = %v", err)
	}
}

func TestAppendEventRetryAfterCommittedCloseFailureIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "session"})
	if err != nil {
		t.Fatal(err)
	}
	var failed atomic.Bool
	store.appendCloseHook = func() error {
		if failed.CompareAndSwap(false, true) {
			return errors.New("injected close failure")
		}
		return nil
	}
	event := &session.Event{ID: "event", Author: "agent", Actions: session.EventActions{StateDelta: map[string]any{"local": "value", "app:key": "shared"}}}
	if err := store.AppendEvent(t.Context(), created.Session, event); err == nil {
		t.Fatal("first AppendEvent acknowledged a close failure")
	}
	if err := store.AppendEvent(t.Context(), created.Session, event); err != nil {
		t.Fatalf("byte-identical retry = %v", err)
	}
	conflict := cloneEvent(event)
	conflict.Author = "different"
	if err := store.AppendEvent(t.Context(), created.Session, conflict); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("conflicting reuse = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	got, err := store.Get(t.Context(), &session.GetRequest{AppName: "app", UserID: "user", SessionID: "session"})
	if err != nil || got.Session.Events().Len() != 1 {
		t.Fatalf("events after restart = %d, %v", got.Session.Events().Len(), err)
	}
}

func TestAppendEventSucceedsAfterCommitWhenJournalIsCorrupt(t *testing.T) {
	var warnings warning.SliceSink
	store, err := OpenWith(Options{Dir: t.TempDir(), WarningSink: &warnings})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	created, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "session", State: map[string]any{"app:known": "kept"}})
	if err != nil {
		t.Fatal(err)
	}
	journal, _ := store.paths.appJournal("app")
	validJournal, err := store.paths.root.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.paths.root.WriteFile(journal, []byte("corrupt\n"), fileMode); err != nil {
		t.Fatal(err)
	}
	event := &session.Event{ID: "committed", Actions: session.EventActions{StateDelta: map[string]any{"local": "value", "app:new": "applied"}}}
	if err := store.AppendEvent(t.Context(), created.Session, event); err != nil {
		t.Fatalf("postcommit AppendEvent = %v", err)
	}
	for key, want := range map[string]any{"local": "value", "app:known": "kept", "app:new": "applied"} {
		value, err := created.Session.State().Get(key)
		if err != nil || value != want {
			t.Fatalf("%s = %#v, %v", key, value, err)
		}
	}
	if got := warnings.Warnings(); len(got) == 0 || got[len(got)-1].Code != warning.WarnSessionSnapshotRefresh {
		t.Fatalf("warnings = %#v", got)
	}
	if err := store.paths.root.WriteFile(journal, validJournal, fileMode); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(t.Context(), &session.GetRequest{AppName: "app", UserID: "user", SessionID: "session"})
	if err != nil {
		t.Fatal(err)
	}
	if value, err := got.Session.State().Get("app:new"); err != nil || value != "applied" {
		t.Fatalf("repaired app:new = %#v, %v", value, err)
	}
}

func TestJournalLoaderRejectsDuplicatePositiveOrderAfterTranscriptDeletion(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	_, err = store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "deleted", State: map[string]any{"app:retained": "value"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(t.Context(), &session.DeleteRequest{AppName: "app", UserID: "user", SessionID: "deleted"}); err != nil {
		t.Fatal(err)
	}
	_, err = store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "active"})
	if err != nil {
		t.Fatal(err)
	}
	journal, _ := store.paths.appJournal("app")
	records, err := store.loadJournal(journal, new(warningBuffer))
	if err != nil {
		t.Fatal(err)
	}
	duplicate := marshalJournalRecord(stateJournalRecord{V: 1, ID: "different-id", Order: records[0].Order, Delta: map[string]any{"other": true}})
	file, err := store.paths.root.OpenFile(journal, os.O_APPEND|os.O_WRONLY, fileMode)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(duplicate); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(t.Context(), &session.GetRequest{AppName: "app", UserID: "user", SessionID: "active"}); !errors.Is(err, ErrCorruptLog) {
		t.Fatalf("Get duplicate journal order = %v", err)
	}
}

func TestRepairJournalRejectsDuplicateLogicalOrderBeforeAppendingDisjointRecords(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	records := []stateRecord{
		{ID: "app-record", Order: 1, UserID: "user", AppDelta: map[string]any{"app": "value"}, UserDelta: map[string]any{}},
		{ID: "user-record", Order: 1, UserID: "user", AppDelta: map[string]any{}, UserDelta: map[string]any{"user": "value"}},
	}
	tests := []struct {
		name    string
		journal func(string) (string, error)
		delta   func(stateRecord) map[string]any
	}{
		{name: "app", journal: store.paths.appJournal, delta: func(record stateRecord) map[string]any { return record.AppDelta }},
		{name: "user", journal: func(app string) (string, error) { return store.paths.userJournal(app, "user"), nil }, delta: func(record stateRecord) map[string]any { return record.UserDelta }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			name, err := test.journal("app")
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := store.repairJournal(name, records, test.delta, func(stateRecord) bool { return true }, true, new(warningBuffer)); !errors.Is(err, ErrCorruptLog) || !errors.Is(err, errLogicalRecordConflict) {
				t.Fatalf("repairJournal error = %v", err)
			}
			if _, err := store.paths.root.Stat(name); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("journal was appended before record-set validation: %v", err)
			}
		})
	}
}

func TestAuthoritativeFallbackRejectsDuplicateOrderWithEmptyDelta(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	created, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "session"})
	if err != nil {
		t.Fatal(err)
	}
	app := store.appLocks("app")
	name := store.paths.sessionLog("app", "user", created.Session.ID())
	log, err := loadSessionLog(store.paths, name, "app", "user", created.Session.ID(), store.fsync, new(warningBuffer))
	if err != nil {
		t.Fatal(err)
	}
	journal, err := store.paths.appJournal("app")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.paths.root.WriteFile(journal, []byte("corrupt\n"), fileMode); err != nil {
		t.Fatal(err)
	}
	pending := []stateRecord{{ID: "different-empty-record", Order: log.stateRecords[0].Order, UserID: "user", AppDelta: map[string]any{}, UserDelta: map[string]any{}}}
	app.project.Lock()
	_, err = store.authoritativeFallbackLocked("app", "user", app, pending, new(warningBuffer))
	app.project.Unlock()
	if !errors.Is(err, ErrCorruptLog) || !errors.Is(err, errLogicalRecordConflict) {
		t.Fatalf("authoritativeFallbackLocked error = %v", err)
	}
}

func TestOperationDeduplicatesForwardRecordWarningAcrossLoadAndScan(t *testing.T) {
	var warnings warning.SliceSink
	store, err := OpenWith(Options{Dir: t.TempDir(), WarningSink: &warnings})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	created, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "session"})
	if err != nil {
		t.Fatal(err)
	}
	name := store.paths.sessionLog("app", "user", "session")
	file, err := store.paths.root.OpenFile(name, os.O_APPEND|os.O_WRONLY, fileMode)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("{\"v\":99,\"type\":\"future\",\"order\":99}\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendEvent(t.Context(), created.Session, &session.Event{ID: "event"}); err != nil {
		t.Fatal(err)
	}
	got := warnings.Warnings()
	if len(got) != 1 || got[0].Code != warning.WarnSessionRecordUnsupportedVersion {
		t.Fatalf("warnings = %#v", got)
	}
}

func TestStaleHandleCannotAppendIntoRecreatedSession(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	old, _ := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "same"})
	if err := store.Delete(t.Context(), &session.DeleteRequest{AppName: "app", UserID: "user", SessionID: "same"}); err != nil {
		t.Fatal(err)
	}
	replacement, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "same"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AppendEvent(t.Context(), old.Session, &session.Event{ID: "stale"}); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("stale append = %v", err)
	}
	got, err := store.Get(t.Context(), &session.GetRequest{AppName: "app", UserID: "user", SessionID: replacement.Session.ID()})
	if err != nil || got.Session.Events().Len() != 0 {
		t.Fatalf("replacement events = %d, %v", got.Session.Events().Len(), err)
	}
}

func TestFilteredGetAndListRetainTranscriptLastUpdateTime(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	created, _ := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "session"})
	want := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	if err := store.AppendEvent(t.Context(), created.Session, &session.Event{ID: "event", Timestamp: want}); err != nil {
		t.Fatal(err)
	}
	filtered, err := store.Get(t.Context(), &session.GetRequest{AppName: "app", UserID: "user", SessionID: "session", After: want.Add(time.Second)})
	if err != nil || filtered.Session.Events().Len() != 0 || !filtered.Session.LastUpdateTime().Equal(want) {
		t.Fatalf("filtered session = %d events, %v, %v", filtered.Session.Events().Len(), filtered.Session.LastUpdateTime(), err)
	}
	listed, err := store.List(t.Context(), &session.ListRequest{AppName: "app", UserID: "user"})
	if err != nil || !listed.Sessions[0].LastUpdateTime().Equal(want) {
		t.Fatalf("listed update = %v, %v", listed.Sessions[0].LastUpdateTime(), err)
	}
}

func TestDeleteProjectsSharedStateBeforeRemovingTranscriptAndSidecars(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	created, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "one"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AppendEvent(t.Context(), created.Session, &session.Event{ID: "event", Actions: session.EventActions{StateDelta: map[string]any{"app:key": "survives"}}}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSidecar(t.Context(), "app", "user", "one", "cache", map[string]any{"x": true}); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(t.Context(), &session.DeleteRequest{AppName: "app", UserID: "user", SessionID: "one"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(t.Context(), &session.GetRequest{AppName: "app", UserID: "user", SessionID: "one"}); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("deleted session get = %v", err)
	}
	second, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "other", SessionID: "two"})
	if err != nil {
		t.Fatal(err)
	}
	value, err := second.Session.State().Get("app:key")
	if err != nil || value != "survives" {
		t.Fatalf("shared state after delete = %#v, %v", value, err)
	}
}

func TestSameSessionConcurrentAppendEightByOneHundred(t *testing.T) {
	fsync := false
	store, err := OpenWith(Options{Dir: t.TempDir(), Fsync: &fsync})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	created, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "session"})
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	errorsFound := make(chan error, 8)
	for worker := range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			for index := range 100 {
				id := fmt.Sprintf("%d-%03d", worker, index)
				if err := store.AppendEvent(context.Background(), created.Session, &session.Event{ID: id, Timestamp: time.Unix(int64(index), 0)}); err != nil {
					errorsFound <- err
					return
				}
			}
		}()
	}
	group.Wait()
	close(errorsFound)
	for err := range errorsFound {
		t.Fatal(err)
	}
	got, err := store.Get(t.Context(), &session.GetRequest{AppName: "app", UserID: "user", SessionID: "session"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Session.Events().Len() != 800 {
		t.Fatalf("event count = %d, want 800", got.Session.Events().Len())
	}
}

func TestDistinctSessionTranscriptProgressIsNotBlockedByProjection(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	first, _ := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "first"})
	second, _ := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "second"})
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	store.projectHook = func() error {
		once.Do(func() { close(entered); <-release })
		return nil
	}
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- store.AppendEvent(context.Background(), first.Session, &session.Event{ID: "first-event", Actions: session.EventActions{StateDelta: map[string]any{"app:first": true}}})
	}()
	<-entered
	secondCommitted := make(chan struct{})
	store.commitHook = func(id string) {
		if id == "second" {
			close(secondCommitted)
		}
	}
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- store.AppendEvent(context.Background(), second.Session, &session.Event{ID: "second-event", Actions: session.EventActions{StateDelta: map[string]any{"app:second": true}}})
	}()
	select {
	case <-secondCommitted:
	case <-time.After(2 * time.Second):
		t.Fatal("second transcript did not commit while first projection was blocked")
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
}

func TestScanSkipsSessionRemovedAfterDirectoryEnumeration(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	_, err = store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "a-vanishing"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "z-keep"})
	if err != nil {
		t.Fatal(err)
	}
	vanishing := store.paths.sessionLog("app", "user", "a-vanishing")
	var removed atomic.Bool
	store.scanEntryHook = func(name string) {
		if name == vanishing && removed.CompareAndSwap(false, true) {
			if err := store.paths.root.Remove(name); err != nil {
				t.Errorf("remove enumerated session: %v", err)
			}
		}
	}
	if _, err := store.Get(t.Context(), &session.GetRequest{AppName: "app", UserID: "user", SessionID: "z-keep"}); err != nil {
		t.Fatalf("Get unrelated session = %v", err)
	}
	listed, err := store.List(t.Context(), &session.ListRequest{AppName: "app", UserID: "user"})
	if err != nil || len(listed.Sessions) != 1 || listed.Sessions[0].ID() != "z-keep" {
		t.Fatalf("List after concurrent removal = %#v, %v", listed, err)
	}
}

func TestConcurrentCloseWaitsAndSharesResult(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("injected close failure")
	entered := make(chan struct{})
	release := make(chan struct{})
	store.closeHook = func() error {
		close(entered)
		<-release
		return wantErr
	}
	first := make(chan error, 1)
	second := make(chan error, 1)
	go func() { first <- store.Close() }()
	<-entered
	go func() { second <- store.Close() }()
	select {
	case err := <-second:
		t.Fatalf("concurrent Close returned early: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	for index, result := range []<-chan error{first, second} {
		if err := <-result; !errors.Is(err, wantErr) {
			t.Fatalf("Close %d = %v", index+1, err)
		}
	}
	if err := store.Close(); !errors.Is(err, wantErr) {
		t.Fatalf("later Close = %v", err)
	}
}

func TestFsyncCoversCreatedAndDeletedDirectoryHierarchy(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	var mu sync.Mutex
	var synced []string
	store.dirSyncHook = func(name string) error {
		mu.Lock()
		defer mu.Unlock()
		synced = append(synced, name)
		return nil
	}
	created, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "session"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(t.Context(), &session.DeleteRequest{AppName: created.Session.AppName(), UserID: created.Session.UserID(), SessionID: created.Session.ID()}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(synced) == 0 {
		t.Fatal("no directory durability barriers recorded")
	}
}

func TestStoreRepairsTornTailAndPreservesCorruptMiddle(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	created, _ := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "session"})
	for _, id := range []string{"one", "two"} {
		if err := store.AppendEvent(t.Context(), created.Session, &session.Event{ID: id}); err != nil {
			t.Fatal(err)
		}
	}
	name := store.paths.sessionLog("app", "user", "session")
	path := dir + string(os.PathSeparator) + name
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if err := os.WriteFile(path, data[:len(data)-4], fileMode); err != nil {
		t.Fatal(err)
	}
	store, _ = Open(dir)
	got, err := store.Get(t.Context(), &session.GetRequest{AppName: "app", UserID: "user", SessionID: "session"})
	if err != nil || !reflect.DeepEqual(eventIDs(got.Session), []string{"one"}) {
		t.Fatalf("torn replay = %#v, %v", eventIDs(got.Session), err)
	}
	_ = store.Close()
	data, _ = os.ReadFile(path)
	corrupt := append([]byte("not json\n"), data...)
	if err := os.WriteFile(path, corrupt, fileMode); err != nil {
		t.Fatal(err)
	}
	store, _ = Open(dir)
	t.Cleanup(func() { _ = store.Close() })
	_, err = store.Get(t.Context(), &session.GetRequest{AppName: "app", UserID: "user", SessionID: "session"})
	after, _ := os.ReadFile(path)
	if !errors.Is(err, ErrCorruptLog) || !reflect.DeepEqual(corrupt, after) {
		t.Fatalf("corrupt replay = %v; modified = %v", err, !reflect.DeepEqual(corrupt, after))
	}
}

func TestStateAllReturnsSnapshot(t *testing.T) {
	store, _ := Open(t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	created, _ := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "session", State: map[string]any{"key": "before"}})
	all := maps.Collect(created.Session.State().All())
	_ = created.Session.State().Set("key", "after")
	if all["key"] != "before" {
		t.Fatalf("state iterator was not a snapshot: %#v", all)
	}
}
