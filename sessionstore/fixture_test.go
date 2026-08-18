package sessionstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/plasmid-dev/plasmid/internal/fixture"
	"github.com/plasmid-dev/plasmid/warning"
)

type sessionsFixtureMetadata struct {
	Area string `json:"area"`
	ID   string `json:"id"`
	Kind string `json:"kind"`
}

type sessionsFixtureExpected struct {
	AppState      any                     `json:"appState,omitempty"`
	Events        []string                `json:"events,omitempty"`
	RepairedLines int                     `json:"repairedLines,omitempty"`
	Sidecar       any                     `json:"sidecar,omitempty"`
	SidecarOther  any                     `json:"sidecarOther,omitempty"`
	State         any                     `json:"state,omitempty"`
	Unchanged     bool                    `json:"unchanged,omitempty"`
	Warnings      []fixture.WarningFields `json:"warnings,omitempty"`
	WireVersion   int                     `json:"wireVersion,omitempty"`
	FirstFailed   bool                    `json:"firstFailed,omitempty"`
	Created       bool                    `json:"created,omitempty"`
	Generated     bool                    `json:"generated,omitempty"`
	Reachable     bool                    `json:"reachable,omitempty"`
	Reopened      bool                    `json:"reopened,omitempty"`
	RetryExists   bool                    `json:"retryExists,omitempty"`
	WarningCode   string                  `json:"warningCode,omitempty"`
	Permissions   map[string]string       `json:"permissions,omitempty"`
	FullEvent     map[string]any          `json:"fullEvent,omitempty"`
}

type sessionsFixtureScenario struct {
	Operations []sessionsFixtureOperation `json:"operations"`
}

type sessionsFixtureOperation struct {
	Op          string         `json:"op"`
	App         string         `json:"app,omitempty"`
	User        string         `json:"user,omitempty"`
	Session     string         `json:"session,omitempty"`
	State       map[string]any `json:"state,omitempty"`
	Event       *session.Event `json:"event,omitempty"`
	Key         string         `json:"key,omitempty"`
	GeneratedID string         `json:"generatedId,omitempty"`
}

func init() {
	fixture.RegisterRunner("sessions", "sessionstore/all", "corrupt-middle", "create-directory-sync", "delete-recreation", "directory-sync", "forward-record", "full-event", "identifiers", "permissions", "raw", "repair", "round-trip", "sidecar", "state-scoping", "torn-tail", "transient")
}

func TestMain(m *testing.M) { os.Exit(fixture.Run(m)) }

func TestSessionsFixtureCoverage(t *testing.T) { fixture.AssertCoverage(t, "sessions") }

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
			runFullEventFixture(t, testCase)
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
		case "repair":
			runRepairFixture(t, testCase)
		case "directory-sync":
			runDirectorySyncFixture(t, testCase)
		case "permissions":
			runPermissionsFixture(t, testCase)
		case "delete-recreation":
			runDeleteRecreationFixture(t, testCase)
		case "full-event":
			runNativeFullEventFixture(t, testCase)
		case "create-directory-sync":
			runCreateDirectorySyncFixture(t, testCase)
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
	return store, t.Context()
}

func fixtureSession(t *testing.T, store *Store, ctx context.Context, app, user, id string, state map[string]any) session.Session {
	t.Helper()
	response, err := store.Create(ctx, &session.CreateRequest{AppName: app, UserID: user, SessionID: id, State: state})
	if err != nil {
		t.Fatal(err)
	}
	return response.Session
}

func getFixtureSession(t *testing.T, store *Store, ctx context.Context, app, user, id string) session.Session {
	t.Helper()
	response, err := store.Get(ctx, &session.GetRequest{AppName: app, UserID: user, SessionID: id})
	if err != nil {
		t.Fatal(err)
	}
	return response.Session
}

func runRoundTripFixture(t *testing.T, testCase fixture.Case) {
	var input struct{ App, ID, User string }
	testCase.Decode(t, "input.json", &input)
	store, ctx := newFixtureStore(t)
	current := fixtureSession(t, store, ctx, input.App, input.User, input.ID, map[string]any{"local": "saved"})
	if err := store.AppendEvent(ctx, current, &session.Event{ID: "event", Timestamp: time.Unix(1, 0).UTC()}); err != nil {
		t.Fatal(err)
	}
	name := store.paths.sessionLog(input.App, input.User, input.ID)
	data, err := store.paths.root.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	version := recordVersion
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		var record record
		if err := json.Unmarshal([]byte(line), &record); err != nil || record.V != recordVersion {
			version = 0
		}
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
	got := getFixtureSession(t, store, ctx, input.App, input.User, input.ID)
	state, _ := got.State().Get("local")
	testCase.CompareJSON(t, "expected.json", sessionsFixtureExpected{State: state, Events: eventIDs(got), WireVersion: version}, fixture.Paths{}, fixture.GoldenReadOnly)
}

func runStateScopingFixture(t *testing.T, testCase fixture.Case) {
	var input struct{ App string }
	testCase.Decode(t, "input.json", &input)
	store, ctx := newFixtureStore(t)
	first := fixtureSession(t, store, ctx, input.App, "one", "one", nil)
	second := fixtureSession(t, store, ctx, input.App, "two", "two", nil)
	if err := store.AppendEvent(ctx, first, &session.Event{ID: "first", Timestamp: time.Unix(200, 0), Actions: session.EventActions{StateDelta: map[string]any{"app:key": "first", "user:key": "one"}}}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendEvent(ctx, second, &session.Event{ID: "second", Timestamp: time.Unix(1, 0), Actions: session.EventActions{StateDelta: map[string]any{"app:key": "second", "user:key": "two"}}}); err != nil {
		t.Fatal(err)
	}
	got := getFixtureSession(t, store, ctx, input.App, "one", "one")
	appState, _ := got.State().Get("app:key")
	userState, _ := got.State().Get("user:key")
	if userState != "one" {
		t.Fatalf("user state = %#v", userState)
	}
	testCase.CompareJSON(t, "expected.json", sessionsFixtureExpected{AppState: appState}, fixture.Paths{}, fixture.GoldenReadOnly)
}

func runFullEventFixture(t *testing.T, testCase fixture.Case) {
	store, ctx := newFixtureStore(t)
	current := fixtureSession(t, store, ctx, "app", "user", "session", nil)
	for index, text := range []string{"", "null", "provider"} {
		event := &session.Event{ID: string(rune('a' + index)), LLMResponse: model.LLMResponse{Content: genai.NewContentFromText(text, genai.RoleModel)}}
		if err := store.AppendEvent(ctx, current, event); err != nil {
			t.Fatal(err)
		}
	}
	got := getFixtureSession(t, store, ctx, "app", "user", "session")
	testCase.CompareJSON(t, "expected.json", sessionsFixtureExpected{Events: eventIDs(got)}, fixture.Paths{}, fixture.GoldenReadOnly)
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
	ctx := t.Context()
	current := fixtureSession(t, store, ctx, "app", "user", "session", nil)
	name := store.paths.sessionLog("app", "user", "session")
	file, err := store.paths.root.OpenFile(name, os.O_APPEND|os.O_WRONLY, fileMode)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range input.Records {
		if _, err := file.WriteString(value + "\n"); err != nil {
			t.Fatal(err)
		}
	}
	_ = file.Close()
	got := getFixtureSession(t, store, ctx, current.AppName(), current.UserID(), current.ID())
	if got.Events().Len() != 0 {
		t.Fatalf("forward records restored events")
	}
	testCase.CompareJSON(t, "expected.json", sessionsFixtureExpected{Warnings: fixture.StableWarnings(warnings.Warnings())}, fixture.Paths{}, fixture.GoldenReadOnly)
}

func runTornTailFixture(t *testing.T, testCase fixture.Case) {
	store, ctx := newFixtureStore(t)
	current := fixtureSession(t, store, ctx, "app", "user", "session", nil)
	for _, id := range []string{"one", "two"} {
		if err := store.AppendEvent(ctx, current, &session.Event{ID: id}); err != nil {
			t.Fatal(err)
		}
	}
	name := store.paths.sessionLog("app", "user", "session")
	data, _ := store.paths.root.ReadFile(name)
	if err := store.paths.root.WriteFile(name, data[:len(data)-4], fileMode); err != nil {
		t.Fatal(err)
	}
	got := getFixtureSession(t, store, ctx, "app", "user", "session")
	data, _ = store.paths.root.ReadFile(name)
	testCase.CompareJSON(t, "expected.json", sessionsFixtureExpected{Events: eventIDs(got), RepairedLines: strings.Count(string(data), "\n")}, fixture.Paths{}, fixture.GoldenReadOnly)
}

func runCorruptMiddleFixture(t *testing.T, testCase fixture.Case) {
	store, ctx := newFixtureStore(t)
	current := fixtureSession(t, store, ctx, "app", "user", "session", nil)
	if err := store.AppendEvent(ctx, current, &session.Event{ID: "event"}); err != nil {
		t.Fatal(err)
	}
	name := store.paths.sessionLog("app", "user", "session")
	data, _ := store.paths.root.ReadFile(name)
	data = append([]byte("not json\n"), data...)
	if err := store.paths.root.WriteFile(name, data, fileMode); err != nil {
		t.Fatal(err)
	}
	_, err := store.Get(ctx, &session.GetRequest{AppName: "app", UserID: "user", SessionID: "session"})
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
		if err := store.AppendSidecar(ctx, "app", "user", first.ID(), "kind", map[string]int{"value": value}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.AppendSidecar(ctx, "app", "user", second.ID(), "kind", map[string]int{"value": 3}); err != nil {
		t.Fatal(err)
	}
	var got, other map[string]any
	_, err := store.LoadSidecar(ctx, "app", "user", first.ID(), "kind", &got)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.LoadSidecar(ctx, "app", "user", second.ID(), "kind", &other)
	if err != nil {
		t.Fatal(err)
	}
	testCase.CompareJSON(t, "expected.json", sessionsFixtureExpected{Sidecar: got, SidecarOther: other}, fixture.Paths{}, fixture.GoldenReadOnly)
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
	fixtureSession(t, store, ctx, "app", "user", "id/with space", nil)
	testCase.CompareJSON(t, "expected.json", sessionsFixtureExpected{}, fixture.Paths{}, fixture.GoldenReadOnly)
}

func runTransientFixture(t *testing.T, testCase fixture.Case) {
	store, ctx := newFixtureStore(t)
	current := fixtureSession(t, store, ctx, "app", "user", "session", nil)
	name := store.paths.sessionLog("app", "user", "session")
	before, _ := store.paths.root.ReadFile(name)
	if err := store.AppendEvent(ctx, current, &session.Event{ID: "partial", LLMResponse: model.LLMResponse{Partial: true}}); err != nil {
		t.Fatal(err)
	}
	after, err := store.paths.root.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	testCase.CompareJSON(t, "expected.json", sessionsFixtureExpected{Unchanged: bytes.Equal(before, after)}, fixture.Paths{}, fixture.GoldenReadOnly)
}

func eventIDs(current session.Session) []string {
	ids := make([]string, 0, current.Events().Len())
	for event := range current.Events().All() {
		ids = append(ids, event.ID)
	}
	return ids
}

func runRepairFixture(t *testing.T, testCase fixture.Case) {
	var input sessionsFixtureScenario
	testCase.Decode(t, "input.json", &input)
	var warnings warning.SliceSink
	store, err := OpenWith(Options{Dir: t.TempDir(), WarningSink: &warnings})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := t.Context()
	var current session.Session
	var got session.Session
	for _, operation := range input.Operations {
		switch operation.Op {
		case "create":
			current = fixtureSession(t, store, ctx, operation.App, operation.User, operation.Session, operation.State)
		case "inject-journal-failure":
			store.journalHook = func() error { return errors.New("injected journal failure") }
		case "append":
			if err := store.AppendEvent(ctx, current, operation.Event); err != nil {
				t.Fatal(err)
			}
		case "clear-fault":
			store.journalHook = nil
		case "get":
			got = getFixtureSession(t, store, ctx, operation.App, operation.User, operation.Session)
		default:
			t.Fatalf("unsupported repair operation %q", operation.Op)
		}
	}
	value, _ := got.State().Get("app:key")
	code := ""
	if all := warnings.Warnings(); len(all) > 0 {
		code = all[len(all)-1].Code
	}
	testCase.CompareJSON(t, "expected.json", sessionsFixtureExpected{State: value, WarningCode: code}, fixture.Paths{}, fixture.GoldenReadOnly)
}

func runCreateDirectorySyncFixture(t *testing.T, testCase fixture.Case) {
	var input sessionsFixtureScenario
	testCase.Decode(t, "input.json", &input)
	dir := t.TempDir()
	var warnings warning.SliceSink
	store, err := OpenWith(Options{Dir: dir, WarningSink: &warnings})
	if err != nil {
		t.Fatal(err)
	}
	var firstErr, createErr, getErr, retryErr, openErr error
	var created *session.CreateResponse
	for _, operation := range input.Operations {
		switch operation.Op {
		case "inject-directory-sync-failure":
			store.dirSyncHook = func(string) error { return errors.New("injected create directory sync") }
		case "set-generated-id":
			id := operation.GeneratedID
			store.newID = func() string { return id }
		case "create-expect-failure":
			_, firstErr = store.Create(t.Context(), &session.CreateRequest{AppName: operation.App, UserID: operation.User, SessionID: operation.Session, State: operation.State})
		case "restart":
			_ = store.Close()
			store, openErr = Open(dir)
			if openErr != nil {
				t.Fatal(openErr)
			}
		case "create":
			created, createErr = store.Create(t.Context(), &session.CreateRequest{AppName: operation.App, UserID: operation.User, SessionID: operation.Session, State: operation.State})
		case "get":
			_, getErr = store.Get(t.Context(), &session.GetRequest{AppName: operation.App, UserID: operation.User, SessionID: operation.Session})
		case "create-expect-exists":
			_, retryErr = store.Create(t.Context(), &session.CreateRequest{AppName: operation.App, UserID: operation.User, SessionID: operation.Session, State: operation.State})
		default:
			t.Fatalf("unsupported create directory-sync operation %q", operation.Op)
		}
	}
	generated := createErr == nil && created.Session.ID() == "generated"
	t.Cleanup(func() { _ = store.Close() })
	actual := sessionsFixtureExpected{
		FirstFailed: firstErr != nil,
		Created:     createErr == nil,
		Generated:   generated,
		Reachable:   getErr == nil,
		Reopened:    openErr == nil,
		RetryExists: errors.Is(retryErr, ErrSessionExists),
		Warnings:    fixture.StableWarnings(warnings.Warnings()),
	}
	testCase.CompareJSON(t, "expected.json", actual, fixture.Paths{}, fixture.GoldenReadOnly)
}

func runDirectorySyncFixture(t *testing.T, testCase fixture.Case) {
	var input sessionsFixtureScenario
	testCase.Decode(t, "input.json", &input)
	dir := t.TempDir()
	store, _ := Open(dir)
	ctx := t.Context()
	failed := false
	retried := false
	for _, operation := range input.Operations {
		switch operation.Op {
		case "create":
			fixtureSession(t, store, ctx, operation.App, operation.User, operation.Session, operation.State)
		case "inject-directory-sync-failure-once":
			store.dirSyncHook = func(string) error { store.dirSyncHook = nil; return errors.New("injected") }
		case "delete-expect-failure":
			failed = store.Delete(ctx, &session.DeleteRequest{AppName: operation.App, UserID: operation.User, SessionID: operation.Session}) != nil
		case "restart":
			_ = store.Close()
			store, _ = Open(dir)
		case "delete":
			retried = store.Delete(ctx, &session.DeleteRequest{AppName: operation.App, UserID: operation.User, SessionID: operation.Session}) == nil
		default:
			t.Fatalf("unsupported directory-sync operation %q", operation.Op)
		}
	}
	t.Cleanup(func() { _ = store.Close() })
	testCase.CompareJSON(t, "expected.json", sessionsFixtureExpected{FirstFailed: failed, Unchanged: retried}, fixture.Paths{}, fixture.GoldenReadOnly)
}

func runPermissionsFixture(t *testing.T, testCase fixture.Case) {
	var input sessionsFixtureScenario
	testCase.Decode(t, "input.json", &input)
	store, ctx := newFixtureStore(t)
	var name string
	for _, operation := range input.Operations {
		switch operation.Op {
		case "create":
			fixtureSession(t, store, ctx, operation.App, operation.User, operation.Session, operation.State)
			name = store.paths.sessionLog(operation.App, operation.User, operation.Session)
		case "inspect-permissions":
		default:
			t.Fatalf("unsupported permissions operation %q", operation.Op)
		}
	}
	fileInfo, _ := store.paths.root.Stat(name)
	dirInfo, _ := store.paths.root.Stat(strings.TrimSuffix(name, "/session.jsonl"))
	testCase.CompareJSON(t, "expected.json", sessionsFixtureExpected{Permissions: map[string]string{"file": fileInfo.Mode().Perm().String(), "directory": dirInfo.Mode().Perm().String()}}, fixture.Paths{}, fixture.GoldenReadOnly)
}

func runDeleteRecreationFixture(t *testing.T, testCase fixture.Case) {
	var input sessionsFixtureScenario
	testCase.Decode(t, "input.json", &input)
	store, ctx := newFixtureStore(t)
	var current session.Session
	var value any
	firstFailed := false
	for _, operation := range input.Operations {
		switch operation.Op {
		case "inject-directory-sync-failure":
			store.dirSyncHook = func(string) error { return errors.New("injected create directory sync") }
		case "create-expect-failure":
			_, err := store.Create(ctx, &session.CreateRequest{AppName: operation.App, UserID: operation.User, SessionID: operation.Session, State: operation.State})
			firstFailed = err != nil
		case "clear-fault":
			store.dirSyncHook = nil
		case "create":
			current = fixtureSession(t, store, ctx, operation.App, operation.User, operation.Session, operation.State)
		case "delete":
			if err := store.Delete(ctx, &session.DeleteRequest{AppName: operation.App, UserID: operation.User, SessionID: operation.Session}); err != nil {
				t.Fatal(err)
			}
		case "read-state":
			value, _ = current.State().Get(operation.Key)
		default:
			t.Fatalf("unsupported delete-recreation operation %q", operation.Op)
		}
	}
	testCase.CompareJSON(t, "expected.json", sessionsFixtureExpected{FirstFailed: firstFailed, State: value}, fixture.Paths{}, fixture.GoldenReadOnly)
}

func runNativeFullEventFixture(t *testing.T, testCase fixture.Case) {
	var input sessionsFixtureScenario
	testCase.Decode(t, "input.json", &input)
	store, ctx := newFixtureStore(t)
	dir := store.paths.dir
	var current session.Session
	var got *session.Event
	for _, operation := range input.Operations {
		switch operation.Op {
		case "create":
			current = fixtureSession(t, store, ctx, operation.App, operation.User, operation.Session, operation.State)
		case "append":
			if err := store.AppendEvent(ctx, current, operation.Event); err != nil {
				t.Fatal(err)
			}
		case "restart":
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			var err error
			store, err = Open(dir)
			if err != nil {
				t.Fatal(err)
			}
		case "get-event":
			got = getFixtureSession(t, store, ctx, operation.App, operation.User, operation.Session).Events().At(0)
		default:
			t.Fatalf("unsupported full-event operation %q", operation.Op)
		}
	}
	t.Cleanup(func() { _ = store.Close() })
	actual := map[string]any{"id": got.ID, "invocationId": got.InvocationID, "author": got.Author, "branch": got.Branch, "isolationScope": got.IsolationScope, "timestamp": got.Timestamp.Format(time.RFC3339), "parts": []string{got.Content.Parts[0].Text, got.Content.Parts[1].Text}, "state": got.Actions.StateDelta["local"], "artifact": got.Actions.ArtifactDelta["artifact"], "transfer": got.Actions.TransferToAgent, "skipSummarization": got.Actions.SkipSummarization, "longRunningToolIds": got.LongRunningToolIDs, "routes": got.Routes, "requestedInput": map[string]any{"id": got.RequestedInput.InterruptID, "message": got.RequestedInput.Message, "payload": got.RequestedInput.Payload}, "usage": map[string]any{"prompt": got.UsageMetadata.PromptTokenCount, "candidates": got.UsageMetadata.CandidatesTokenCount, "total": got.UsageMetadata.TotalTokenCount}, "output": got.Output, "nodePath": got.NodeInfo.Path, "messageAsOutput": got.NodeInfo.MessageAsOutput}
	testCase.CompareJSON(t, "expected.json", sessionsFixtureExpected{FullEvent: actual}, fixture.Paths{}, fixture.GoldenReadOnly)
}
