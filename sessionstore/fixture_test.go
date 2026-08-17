package sessionstore

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/plasmid-dev/plasmid/internal/fixture"
	"github.com/plasmid-dev/plasmid/loop"
	"github.com/plasmid-dev/plasmid/warning"
)

type sessionsFixtureMetadata struct {
	Area string `json:"area"`
	ID   string `json:"id"`
	Kind string `json:"kind"`
}

type sessionsFixtureExpected struct {
	AppState       any                     `json:"appState,omitempty"`
	Events         []string                `json:"events,omitempty"`
	RawBase64      []string                `json:"rawBase64,omitempty"`
	RawNil         []bool                  `json:"rawNil,omitempty"`
	RepairedLines  int                     `json:"repairedLines,omitempty"`
	Sidecar        any                     `json:"sidecar,omitempty"`
	SidecarOther   any                     `json:"sidecarOther,omitempty"`
	State          any                     `json:"state,omitempty"`
	Unchanged      bool                    `json:"unchanged,omitempty"`
	Warnings       []fixture.WarningFields `json:"warnings,omitempty"`
	WireVersionOne bool                    `json:"wireVersionOne,omitempty"`
}

func init() {
	fixture.RegisterRunner("sessions", "sessionstore/all", "corrupt-middle", "forward-record", "identifiers", "raw", "round-trip", "sidecar", "state-scoping", "torn-tail", "transient")
}

func TestMain(m *testing.M) {
	os.Exit(fixture.Run(m))
}

func TestSessionsFixtureCoverage(t *testing.T) {
	fixture.AssertCoverage(t, "sessions")
}

func TestSessionsFixtures(t *testing.T) {
	fixture.Walk(t, "sessions", "sessionstore/all", func(t *testing.T, testCase fixture.Case) {
		var metadata sessionsFixtureMetadata
		testCase.Decode(t, "case.json", &metadata)
		if metadata.Area != "sessions" || metadata.ID != testCase.ID {
			t.Fatalf("metadata = %#v", metadata)
		}
		switch metadata.Kind {
		case "round-trip":
			runRoundTripFixture(t, testCase)
		case "state-scoping":
			runStateScopingFixture(t, testCase)
		case "raw":
			runRawFixture(t, testCase)
		case "forward-record":
			runForwardRecordFixture(t, testCase)
		case "torn-tail":
			runTornTailFixture(t, testCase)
		case "corrupt-middle":
			runCorruptMiddleFixture(t, testCase)
		case "sidecar":
			runSidecarFixture(t, testCase)
		case "identifiers":
			runIdentifiersFixture(t, testCase)
		case "transient":
			runTransientFixture(t, testCase)
		default:
			t.Fatalf("unknown sessions fixture kind %q", metadata.Kind)
		}
	})
}

func newFixtureStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, context.Background()
}

func fixtureSession(t *testing.T, store *Store, ctx context.Context, app, user, id string, state map[string]any) loop.SessionRef {
	t.Helper()
	ref, err := store.Create(ctx, loop.CreateSessionRequest{AppName: app, UserID: user, SessionID: id, State: state})
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func runRoundTripFixture(t *testing.T, testCase fixture.Case) {
	var input struct {
		App  string `json:"app"`
		ID   string `json:"id"`
		User string `json:"user"`
	}
	testCase.Decode(t, "input.json", &input)
	store, ctx := newFixtureStore(t)
	ref := fixtureSession(t, store, ctx, input.App, input.User, input.ID, map[string]any{"local": "saved"})
	if err := store.Append(ctx, ref, loop.Event{ID: "event", Kind: loop.EventText, SessionID: ref.ID, Text: "durable", Timestamp: time.Unix(1, 0).UTC()}); err != nil {
		t.Fatal(err)
	}
	name, err := store.paths.sessionLog(input.App, input.User, input.ID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := store.paths.root.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	wireVersionOne := true
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		var record record
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("wire record = %q, %v", line, err)
		}
		wireVersionOne = wireVersionOne && record.V == recordVersion
	}
	dir := store.paths.dir
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	got, events, err := store.Get(ctx, input.App, input.User, input.ID)
	if err != nil {
		t.Fatal(err)
	}
	actual := sessionsFixtureExpected{State: got.State["local"], Events: eventIDs(events), WireVersionOne: wireVersionOne}
	testCase.CompareJSON(t, "expected.json", actual, fixture.Paths{}, fixture.GoldenReadOnly)
}

func runStateScopingFixture(t *testing.T, testCase fixture.Case) {
	var input struct {
		App string `json:"app"`
	}
	testCase.Decode(t, "input.json", &input)
	store, ctx := newFixtureStore(t)
	first := fixtureSession(t, store, ctx, input.App, "one", "one", nil)
	second := fixtureSession(t, store, ctx, input.App, "two", "two", nil)
	if err := store.Append(ctx, first, loop.Event{ID: "first", Kind: loop.EventText, SessionID: first.ID, StateDelta: map[string]any{"app:key": "first", "user:key": "one"}, Timestamp: time.Unix(200, 0)}); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(ctx, second, loop.Event{ID: "second", Kind: loop.EventText, SessionID: second.ID, StateDelta: map[string]any{"app:key": "second", "user:key": "two"}, Timestamp: time.Unix(1, 0)}); err != nil {
		t.Fatal(err)
	}
	got, _, err := store.Get(ctx, input.App, "one", "one")
	if err != nil || got.State["user:key"] != "one" {
		t.Fatalf("scoped state = %#v, %v", got.State, err)
	}
	testCase.CompareJSON(t, "expected.json", sessionsFixtureExpected{AppState: got.State["app:key"]}, fixture.Paths{}, fixture.GoldenReadOnly)
}

func runRawFixture(t *testing.T, testCase fixture.Case) {
	var input struct {
		RawBase64 []string `json:"rawBase64"`
	}
	testCase.Decode(t, "input.json", &input)
	store, ctx := newFixtureStore(t)
	ref := fixtureSession(t, store, ctx, "app", "user", "session", nil)
	for index, value := range input.RawBase64 {
		event := loop.Event{ID: string(rune('a' + index)), Kind: loop.EventText, SessionID: ref.ID}
		if value != "" {
			raw, err := base64.StdEncoding.DecodeString(value)
			if err != nil {
				t.Fatal(err)
			}
			event.Raw = raw
		}
		if err := store.Append(ctx, ref, event); err != nil {
			t.Fatal(err)
		}
	}
	_, events, err := store.Get(ctx, "app", "user", "session")
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(events))
	gotNil := make([]bool, len(events))
	for index, event := range events {
		gotNil[index] = event.Raw == nil
		if event.Raw != nil {
			got[index] = base64.StdEncoding.EncodeToString(event.Raw)
		}
	}
	actual := sessionsFixtureExpected{RawBase64: got, RawNil: gotNil}
	testCase.CompareJSON(t, "expected.json", actual, fixture.Paths{}, fixture.GoldenReadOnly)
}

func runForwardRecordFixture(t *testing.T, testCase fixture.Case) {
	var input struct {
		Records []string `json:"records"`
	}
	testCase.Decode(t, "input.json", &input)
	var warnings warning.SliceSink
	store, err := OpenWith(Options{Dir: t.TempDir(), WarningSink: &warnings})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	ref := fixtureSession(t, store, ctx, "app", "user", "session", nil)
	name, err := store.paths.sessionLog("app", "user", "session")
	if err != nil {
		t.Fatal(err)
	}
	file, err := store.paths.root.OpenFile(name, os.O_APPEND|os.O_WRONLY, fileMode)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range input.Records {
		if _, err := file.WriteString(value + "\n"); err != nil {
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	_, events, err := store.Get(ctx, ref.AppName, ref.UserID, ref.ID)
	if err != nil || len(events) != 0 {
		t.Fatalf("forward records = %#v, %v", events, err)
	}
	actual := sessionsFixtureExpected{Warnings: fixture.StableWarnings(warnings.Warnings())}
	testCase.CompareJSON(t, "expected.json", actual, fixture.Paths{}, fixture.GoldenReadOnly)
}

func runTornTailFixture(t *testing.T, testCase fixture.Case) {
	store, ctx := newFixtureStore(t)
	ref := fixtureSession(t, store, ctx, "app", "user", "session", nil)
	for _, id := range []string{"one", "two"} {
		if err := store.Append(ctx, ref, loop.Event{ID: id, Kind: loop.EventText, SessionID: ref.ID}); err != nil {
			t.Fatal(err)
		}
	}
	name, err := store.paths.sessionLog("app", "user", "session")
	if err != nil {
		t.Fatal(err)
	}
	data, err := store.paths.root.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.paths.root.WriteFile(name, data[:len(data)-4], fileMode); err != nil {
		t.Fatal(err)
	}
	_, events, err := store.Get(ctx, "app", "user", "session")
	if err != nil {
		t.Fatal(err)
	}
	data, err = store.paths.root.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	actual := sessionsFixtureExpected{Events: eventIDs(events), RepairedLines: strings.Count(string(data), "\n")}
	testCase.CompareJSON(t, "expected.json", actual, fixture.Paths{}, fixture.GoldenReadOnly)
}

func runCorruptMiddleFixture(t *testing.T, testCase fixture.Case) {
	store, ctx := newFixtureStore(t)
	ref := fixtureSession(t, store, ctx, "app", "user", "session", nil)
	if err := store.Append(ctx, ref, loop.Event{ID: "event", Kind: loop.EventText, SessionID: ref.ID}); err != nil {
		t.Fatal(err)
	}
	name, err := store.paths.sessionLog("app", "user", "session")
	if err != nil {
		t.Fatal(err)
	}
	data, err := store.paths.root.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	data = append([]byte("not json\n"), data...)
	if err := store.paths.root.WriteFile(name, data, fileMode); err != nil {
		t.Fatal(err)
	}
	_, _, err = store.Get(ctx, "app", "user", "session")
	after, readErr := store.paths.root.ReadFile(name)
	if !errors.Is(err, ErrCorruptLog) || readErr != nil {
		t.Fatalf("corrupt replay = %v; preservation = %v", err, readErr)
	}
	testCase.CompareJSON(t, "expected.json", sessionsFixtureExpected{Unchanged: bytes.Equal(data, after)}, fixture.Paths{}, fixture.GoldenReadOnly)
}

func runSidecarFixture(t *testing.T, testCase fixture.Case) {
	store, ctx := newFixtureStore(t)
	first := fixtureSession(t, store, ctx, "app", "user", "one", nil)
	second := fixtureSession(t, store, ctx, "app", "user", "two", nil)
	for _, value := range []int{1, 2} {
		if err := store.AppendSidecar(ctx, "app", "user", first.ID, "kind", map[string]int{"value": value}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.AppendSidecar(ctx, "app", "user", second.ID, "kind", map[string]int{"value": 3}); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	found, err := store.LoadSidecar(ctx, "app", "user", first.ID, "kind", &got)
	if err != nil || !found {
		t.Fatalf("sidecar = %v, %#v, %v", found, got, err)
	}
	var other map[string]any
	found, err = store.LoadSidecar(ctx, "app", "user", second.ID, "kind", &other)
	if err != nil || !found {
		t.Fatalf("other sidecar = %v, %#v, %v", found, other, err)
	}
	actual := sessionsFixtureExpected{Sidecar: got, SidecarOther: other}
	testCase.CompareJSON(t, "expected.json", actual, fixture.Paths{}, fixture.GoldenReadOnly)
}

func runIdentifiersFixture(t *testing.T, testCase fixture.Case) {
	var input struct {
		Rejected []string `json:"rejected"`
	}
	testCase.Decode(t, "input.json", &input)
	store, ctx := newFixtureStore(t)
	for _, value := range input.Rejected {
		if _, err := decodeSegment(value); !errors.Is(err, ErrInvalidID) {
			t.Fatalf("decodeSegment(%q) = %v", value, err)
		}
	}
	if _, err := store.Create(ctx, loop.CreateSessionRequest{AppName: "app", UserID: "user", SessionID: "id/with space"}); err != nil {
		t.Fatal(err)
	}
	testCase.CompareJSON(t, "expected.json", sessionsFixtureExpected{}, fixture.Paths{}, fixture.GoldenReadOnly)
}

func runTransientFixture(t *testing.T, testCase fixture.Case) {
	store, ctx := newFixtureStore(t)
	ref := fixtureSession(t, store, ctx, "app", "user", "session", nil)
	name, err := store.paths.sessionLog("app", "user", "session")
	if err != nil {
		t.Fatal(err)
	}
	before, err := store.paths.root.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	err = store.Append(ctx, ref, loop.Event{ID: "delta", Kind: loop.EventTextDelta, SessionID: ref.ID})
	after, readErr := store.paths.root.ReadFile(name)
	if !errors.Is(err, ErrInvalidEvent) || readErr != nil {
		t.Fatalf("transient append = %v; preservation = %v", err, readErr)
	}
	testCase.CompareJSON(t, "expected.json", sessionsFixtureExpected{Unchanged: bytes.Equal(before, after)}, fixture.Paths{}, fixture.GoldenReadOnly)
}

func eventIDs(events []loop.Event) []string {
	ids := make([]string, len(events))
	for index, event := range events {
		ids[index] = event.ID
	}
	return ids
}
