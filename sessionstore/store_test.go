package sessionstore

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/plasmid-dev/plasmid/loop"
)

func TestStoreRoundTripStateAndSidecars(t *testing.T) {
	ctx := context.Background()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ref, err := store.Create(ctx, loop.CreateSessionRequest{AppName: "app", UserID: "user", SessionID: "id/with space", State: map[string]any{"local": map[string]any{"v": 1}, "app:shared": 2, "user:shared": 3, "temp:no": 4}})
	if err != nil {
		t.Fatal(err)
	}
	when := time.Unix(42, 0).UTC()
	event := loop.Event{ID: "event", Kind: loop.EventText, SessionID: ref.ID, Timestamp: when, Raw: []byte(" \n {\"opaque\":true}\t"), StateDelta: map[string]any{"later": []any{"x"}, "app:next": true, "user:next": false, "temp:drop": "x"}}
	if err := store.Append(ctx, ref, event); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSidecar(ctx, "app", "user", ref.ID, "compaction", map[string]any{"n": 1}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSidecar(ctx, "app", "user", ref.ID, "compaction", map[string]any{"n": 2}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(store.paths.dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	got, events, err := store.Get(ctx, "app", "user", ref.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastUpdate != when || got.State["temp:no"] != nil || got.State["temp:drop"] != nil || got.State["app:next"] != true || got.State["user:next"] != false {
		t.Fatalf("reference = %#v", got)
	}
	if len(events) != 1 || string(events[0].Raw) != string(event.Raw) || !reflect.DeepEqual(events[0].StateDelta, map[string]any{"later": []any{"x"}, "app:next": true, "user:next": false}) {
		t.Fatalf("events = %#v", events)
	}
	var sidecar map[string]any
	found, err := store.LoadSidecar(ctx, "app", "user", ref.ID, "compaction", &sidecar)
	if err != nil || !found || sidecar["n"] != float64(2) {
		t.Fatalf("sidecar = %v, %#v, %v", found, sidecar, err)
	}
	if found, err := store.LoadSidecar(ctx, "app", "user", ref.ID, "missing", &sidecar); err != nil || found {
		t.Fatalf("missing sidecar = %v, %v", found, err)
	}
}

func TestStoreValidationListDeleteAndClose(t *testing.T) {
	ctx := context.Background()
	store, err := OpenWith(Options{Dir: t.TempDir(), NewID: func() string { return "550e8400-e29b-41d4-a716-446655440000" }})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := store.Create(ctx, loop.CreateSessionRequest{AppName: "app", UserID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	if ref.ID != "550e8400-e29b-41d4-a716-446655440000" {
		t.Fatalf("id = %q", ref.ID)
	}
	if err := store.Append(ctx, ref, loop.Event{ID: "", SessionID: ref.ID}); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("append empty id = %v", err)
	}
	if err := store.Append(ctx, ref, loop.Event{ID: "event", SessionID: "other"}); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("append mismatched session = %v", err)
	}
	refs, err := store.List(ctx, "app", "")
	if err != nil || len(refs) != 1 || refs[0].State != nil {
		t.Fatalf("list = %#v, %v", refs, err)
	}
	refs, err = store.List(ctx, "app", "u1")
	if err != nil || len(refs) != 1 || refs[0].ID != ref.ID {
		t.Fatalf("user list = %#v, %v", refs, err)
	}
	if err := store.Delete(ctx, "app", "u1", ref.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, "app", "u1", ref.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Get(ctx, "app", "u1", ref.ID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("get deleted = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.List(ctx, "app", ""); !errors.Is(err, ErrClosed) {
		t.Fatalf("list closed = %v", err)
	}
}

func TestAppendRejectsTransientTextDeltaBeforePersistence(t *testing.T) {
	ctx := context.Background()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ref, err := store.Create(ctx, loop.CreateSessionRequest{AppName: "app", UserID: "user", SessionID: "session"})
	if err != nil {
		t.Fatal(err)
	}
	name, err := store.paths.sessionLog("app", "user", "session")
	if err != nil {
		t.Fatal(err)
	}
	before, err := store.paths.root.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	err = store.Append(ctx, ref, loop.Event{ID: "delta", Kind: loop.EventTextDelta, SessionID: ref.ID, Text: "delta"})
	if !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("Append(delta) = %v", err)
	}
	after, err := store.paths.root.ReadFile(name)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("log changed after transient event: %v", err)
	}
	_, events, err := store.Get(ctx, "app", "user", ref.ID)
	if err != nil || len(events) != 0 {
		t.Fatalf("Get after transient event = %#v, %v", events, err)
	}
	if err := store.Append(ctx, ref, loop.Event{ID: "text", Kind: loop.EventText, SessionID: ref.ID, Text: "delta"}); err != nil {
		t.Fatalf("Append(text) = %v", err)
	}
}

func TestSharedStateUsesPersistedOrderAcrossSessionsAndUsers(t *testing.T) {
	ctx := context.Background()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	first, err := store.Create(ctx, loop.CreateSessionRequest{AppName: "app", UserID: "one", SessionID: "one"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Create(ctx, loop.CreateSessionRequest{AppName: "app", UserID: "two", SessionID: "two"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(ctx, first, loop.Event{ID: "later-clock", Kind: loop.EventText, SessionID: first.ID, Timestamp: time.Unix(200, 0), StateDelta: map[string]any{"app:key": "first"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(ctx, second, loop.Event{ID: "earlier-clock", Kind: loop.EventText, SessionID: second.ID, Timestamp: time.Unix(1, 0), StateDelta: map[string]any{"app:key": "second"}}); err != nil {
		t.Fatal(err)
	}
	got, _, err := store.Get(ctx, "app", "one", first.ID)
	if err != nil || got.State["app:key"] != "second" {
		t.Fatalf("shared state = %#v, %v", got.State, err)
	}
}

func TestStoreRepairsTornTailAndPreservesCorruptMiddle(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := store.Create(ctx, loop.CreateSessionRequest{AppName: "app", UserID: "user", SessionID: "session"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"one", "two"} {
		if err := store.Append(ctx, ref, loop.Event{ID: id, Kind: loop.EventText, SessionID: ref.ID}); err != nil {
			t.Fatal(err)
		}
	}
	name, err := store.paths.sessionLog("app", "user", "session")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	path := dir + "/" + name
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data[:len(data)-4], fileMode); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, events, err := store.Get(ctx, "app", "user", "session")
	if err != nil || len(events) != 1 || events[0].ID != "one" {
		t.Fatalf("torn replay = %#v, %v", events, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := 0
	for _, byte := range data {
		if byte == '\n' {
			lines++
		}
	}
	if lines != 2 {
		t.Fatalf("repaired lines = %d", lines)
	}
	if err := os.WriteFile(path, append([]byte("not json\n"), data...), fileMode); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Get(ctx, "app", "user", "session"); !errors.Is(err, ErrCorruptLog) {
		t.Fatalf("corrupt replay = %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("corrupt middle was modified")
	}
}
