package sessionstore_test

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"

	"github.com/plasmid-dev/plasmid/sessionstore"
	"github.com/plasmid-dev/plasmid/warning"
)

const (
	publicSessionID = "session"
	publicWindowsOS = "windows"
)

func TestStorePublicRequestValidation(t *testing.T) {
	t.Run("open", testOpenRequestValidation)
	store := openStore(t, sessionstore.Options{Dir: t.TempDir()})
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	t.Run("create", func(t *testing.T) { testCreateRequestValidation(t, store, canceled) })
	t.Run("get", func(t *testing.T) { testGetRequestValidation(t, store, canceled) })
	t.Run("list", func(t *testing.T) { testListRequestValidation(t, store, canceled) })
	t.Run("delete", func(t *testing.T) { testDeleteRequestValidation(t, store, canceled) })
}

func testOpenRequestValidation(t *testing.T) {
	if _, err := sessionstore.Open(""); !errors.Is(err, sessionstore.ErrInvalidID) {
		t.Fatalf("Open empty directory error = %v, want ErrInvalidID", err)
	}
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := sessionstore.Open(filepath.Join(path, "child")); err == nil {
		t.Fatal("Open beneath a file succeeded")
	}
}

func testCreateRequestValidation(t *testing.T, store *sessionstore.Store, canceled context.Context) {
	if _, err := store.Create(t.Context(), nil); err == nil {
		t.Fatal("Create nil request succeeded")
	}
	if _, err := store.Create(canceled, &session.CreateRequest{AppName: "app", UserID: "user"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Create canceled error = %v", err)
	}
	for _, req := range []*session.CreateRequest{
		{UserID: "user", SessionID: publicSessionID},
		{AppName: "app", SessionID: publicSessionID},
		{AppName: "app", UserID: "user", SessionID: strings.Repeat("x", 201)},
	} {
		if _, err := store.Create(t.Context(), req); !errors.Is(err, sessionstore.ErrInvalidID) {
			t.Fatalf("Create(%+v) error = %v, want ErrInvalidID", req, err)
		}
	}
	if _, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "bad-state", State: map[string]any{"bad": make(chan int)}}); err == nil {
		t.Fatal("Create with non-JSON state succeeded")
	}
}

func testGetRequestValidation(t *testing.T, store *sessionstore.Store, canceled context.Context) {
	if _, err := store.Get(t.Context(), nil); err == nil {
		t.Fatal("Get nil request succeeded")
	}
	valid := &session.GetRequest{AppName: "app", UserID: "user", SessionID: "missing"}
	if _, err := store.Get(canceled, valid); !errors.Is(err, context.Canceled) {
		t.Fatalf("Get canceled error = %v", err)
	}
	if _, err := store.Get(t.Context(), valid); !errors.Is(err, sessionstore.ErrSessionNotFound) {
		t.Fatalf("Get missing error = %v, want ErrSessionNotFound", err)
	}
	for _, req := range []*session.GetRequest{{UserID: "user", SessionID: publicSessionID}, {AppName: "app", SessionID: publicSessionID}, {AppName: "app", UserID: "user"}} {
		if _, err := store.Get(t.Context(), req); !errors.Is(err, sessionstore.ErrInvalidID) {
			t.Fatalf("Get(%+v) error = %v, want ErrInvalidID", req, err)
		}
	}
}

func testListRequestValidation(t *testing.T, store *sessionstore.Store, canceled context.Context) {
	if _, err := store.List(t.Context(), nil); err == nil {
		t.Fatal("List nil request succeeded")
	}
	if _, err := store.List(canceled, &session.ListRequest{AppName: "app"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("List canceled error = %v", err)
	}
	if _, err := store.List(t.Context(), &session.ListRequest{}); !errors.Is(err, sessionstore.ErrInvalidID) {
		t.Fatalf("List empty app error = %v, want ErrInvalidID", err)
	}
	if _, err := store.List(t.Context(), &session.ListRequest{AppName: "app", UserID: strings.Repeat("x", 201)}); !errors.Is(err, sessionstore.ErrInvalidID) {
		t.Fatalf("List invalid user error = %v, want ErrInvalidID", err)
	}
}

func testDeleteRequestValidation(t *testing.T, store *sessionstore.Store, canceled context.Context) {
	if err := store.Delete(t.Context(), nil); err == nil {
		t.Fatal("Delete nil request succeeded")
	}
	valid := &session.DeleteRequest{AppName: "app", UserID: "user", SessionID: "missing"}
	if err := store.Delete(canceled, valid); !errors.Is(err, context.Canceled) {
		t.Fatalf("Delete canceled error = %v", err)
	}
	if err := store.Delete(t.Context(), valid); err != nil {
		t.Fatalf("Delete missing session = %v", err)
	}
	if err := store.Delete(t.Context(), &session.DeleteRequest{}); !errors.Is(err, sessionstore.ErrInvalidID) {
		t.Fatalf("Delete invalid identity error = %v, want ErrInvalidID", err)
	}
}

func TestStorePublicEventAndSidecarValidation(t *testing.T) {
	dir := t.TempDir()
	store := openStore(t, sessionstore.Options{Dir: dir})
	created := createSession(t, store, "app", "user", publicSessionID, nil)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	testAppendEventValidation(t, store, created, canceled)
	testSidecarValidation(t, store, canceled)
	testEventSerializationValidation(t, store, created)
}

func testAppendEventValidation(t *testing.T, store *sessionstore.Store, created session.Session, canceled context.Context) {
	if err := store.AppendEvent(t.Context(), nil, &session.Event{ID: "event"}); err == nil {
		t.Fatal("AppendEvent nil session succeeded")
	}
	if err := store.AppendEvent(t.Context(), created, nil); err == nil {
		t.Fatal("AppendEvent nil event succeeded")
	}
	if err := store.AppendEvent(t.Context(), created, &session.Event{}); !errors.Is(err, sessionstore.ErrInvalidEvent) {
		t.Fatalf("AppendEvent empty ID error = %v, want ErrInvalidEvent", err)
	}
	if err := store.AppendEvent(canceled, created, &session.Event{ID: "event"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("AppendEvent canceled error = %v", err)
	}
	if err := store.AppendEvent(canceled, created, &session.Event{LLMResponse: model.LLMResponse{Partial: true}}); err != nil {
		t.Fatalf("AppendEvent partial response = %v", err)
	}

	other := openStore(t, sessionstore.Options{Dir: t.TempDir()})
	foreign := createSession(t, other, "app", "user", publicSessionID, nil)
	if err := store.AppendEvent(t.Context(), foreign, &session.Event{ID: "foreign"}); !errors.Is(err, sessionstore.ErrInvalidEvent) {
		t.Fatalf("AppendEvent foreign handle error = %v, want ErrInvalidEvent", err)
	}
}

func testSidecarValidation(t *testing.T, store *sessionstore.Store, canceled context.Context) {
	testAppendSidecarValidation(t, store, canceled)
	testLoadSidecarValidation(t, store, canceled)
}

func testAppendSidecarValidation(t *testing.T, store *sessionstore.Store, canceled context.Context) {
	if err := store.AppendSidecar(t.Context(), "app", "user", publicSessionID, "", map[string]any{}); !errors.Is(err, sessionstore.ErrInvalidID) {
		t.Fatalf("AppendSidecar empty kind error = %v, want ErrInvalidID", err)
	}
	if err := store.AppendSidecar(t.Context(), "", "user", publicSessionID, "kind", true); !errors.Is(err, sessionstore.ErrInvalidID) {
		t.Fatalf("AppendSidecar empty app error = %v, want ErrInvalidID", err)
	}
	if err := store.AppendSidecar(canceled, "app", "user", publicSessionID, "kind", map[string]any{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("AppendSidecar canceled error = %v", err)
	}
	if err := store.AppendSidecar(t.Context(), "app", "user", publicSessionID, "kind", make(chan int)); err == nil {
		t.Fatal("AppendSidecar non-JSON value succeeded")
	}
	if err := store.AppendSidecar(t.Context(), "app", "user", "missing", "kind", true); !errors.Is(err, sessionstore.ErrSessionNotFound) {
		t.Fatalf("AppendSidecar missing session error = %v, want ErrSessionNotFound", err)
	}
}

func testLoadSidecarValidation(t *testing.T, store *sessionstore.Store, canceled context.Context) {
	testLoadSidecarErrors(t, store, canceled)
	testLoadSidecarLatestValue(t, store)
}

func testLoadSidecarErrors(t *testing.T, store *sessionstore.Store, canceled context.Context) {
	if ok, err := store.LoadSidecar(t.Context(), "app", "user", publicSessionID, "", new(bool)); ok || !errors.Is(err, sessionstore.ErrInvalidID) {
		t.Fatalf("LoadSidecar empty kind = %v, %v", ok, err)
	}
	if ok, err := store.LoadSidecar(t.Context(), "app", "", publicSessionID, "kind", new(bool)); ok || !errors.Is(err, sessionstore.ErrInvalidID) {
		t.Fatalf("LoadSidecar empty user = %v, %v", ok, err)
	}
	if ok, err := store.LoadSidecar(t.Context(), "app", "user", publicSessionID, "kind", nil); ok || err == nil {
		t.Fatalf("LoadSidecar nil destination = %v, %v", ok, err)
	}
	if ok, err := store.LoadSidecar(canceled, "app", "user", publicSessionID, "kind", new(bool)); ok || !errors.Is(err, context.Canceled) {
		t.Fatalf("LoadSidecar canceled = %v, %v", ok, err)
	}
	if ok, err := store.LoadSidecar(t.Context(), "app", "user", "missing", "kind", new(bool)); ok || !errors.Is(err, sessionstore.ErrSessionNotFound) {
		t.Fatalf("LoadSidecar missing session = %v, %v", ok, err)
	}
	if ok, err := store.LoadSidecar(t.Context(), "app", "user", publicSessionID, "missing", new(bool)); ok || err != nil {
		t.Fatalf("LoadSidecar missing kind = %v, %v", ok, err)
	}
}

func testLoadSidecarLatestValue(t *testing.T, store *sessionstore.Store) {
	if err := store.AppendSidecar(t.Context(), "app", "user", publicSessionID, "kind", map[string]any{"value": "first"}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSidecar(t.Context(), "app", "user", publicSessionID, "kind", map[string]any{"value": "latest"}); err != nil {
		t.Fatal(err)
	}
	var sidecar map[string]any
	if ok, err := store.LoadSidecar(t.Context(), "app", "user", publicSessionID, "kind", &sidecar); !ok || err != nil || sidecar["value"] != "latest" {
		t.Fatalf("LoadSidecar latest = %v, %#v, %v", ok, sidecar, err)
	}
	var wrongShape []string
	if ok, err := store.LoadSidecar(t.Context(), "app", "user", publicSessionID, "kind", &wrongShape); ok || err == nil {
		t.Fatalf("LoadSidecar wrong shape = %v, %v", ok, err)
	}
}

func testEventSerializationValidation(t *testing.T, store *sessionstore.Store, created session.Session) {
	if err := store.AppendEvent(t.Context(), created, &session.Event{
		ID: "non-json", Actions: session.EventActions{StateDelta: map[string]any{"bad": make(chan int)}},
	}); err == nil {
		t.Fatal("AppendEvent non-JSON state succeeded")
	}
	if err := store.AppendEvent(t.Context(), created, &session.Event{
		ID: "non-json-shared", Actions: session.EventActions{StateDelta: map[string]any{"app:bad": make(chan int)}},
	}); err == nil {
		t.Fatal("AppendEvent non-JSON shared state succeeded")
	}
	if err := store.AppendEvent(t.Context(), created, &session.Event{ID: "non-json-shared"}); err != nil {
		t.Fatalf("AppendEvent after rejected shared state = %v", err)
	}
	if err := store.AppendEvent(t.Context(), created, &session.Event{ID: "non-json-output", Output: make(chan int)}); err == nil {
		t.Fatal("AppendEvent non-JSON output succeeded")
	}
	if err := store.AppendEvent(t.Context(), created, &session.Event{ID: "non-json-output"}); err != nil {
		t.Fatalf("AppendEvent after rejected output = %v", err)
	}
}

type transcriptScenario struct {
	name     string
	mutate   func(header, event, sidecar string) string
	wantCode string
	wantErr  error
}

func TestStorePublicTranscriptRecoveryAndValidation(t *testing.T) {
	scenarios := []transcriptScenario{
		{
			name: "torn tail",
			mutate: func(header, event, _ string) string {
				return header + event + `{"v":2,"type":"event"`
			},
			wantCode: warning.WarnSessionLogTornTail,
		},
		{
			name:     "empty transcript",
			mutate:   func(_, _, _ string) string { return "" },
			wantCode: warning.WarnSessionLogCorruptMiddle,
			wantErr:  sessionstore.ErrCorruptLog,
		},
		{
			name:     "blank record",
			mutate:   func(_, _, _ string) string { return " \t\n" },
			wantCode: warning.WarnSessionLogCorruptMiddle,
			wantErr:  sessionstore.ErrCorruptLog,
		},
		{
			name:     "malformed record",
			mutate:   func(_, _, _ string) string { return "{bad}\n" },
			wantCode: warning.WarnSessionLogCorruptMiddle,
			wantErr:  sessionstore.ErrCorruptLog,
		},
		{
			name: "unsupported header",
			mutate: func(_, _, _ string) string {
				return `{"v":99,"type":"session","order":1}` + "\n"
			},
			wantCode: warning.WarnSessionRecordUnsupportedVersion,
			wantErr:  sessionstore.ErrCorruptLog,
		},
		{
			name: "unknown header",
			mutate: func(_, _, _ string) string {
				return `{"v":2,"type":"future","order":1}` + "\n"
			},
			wantCode: warning.WarnSessionRecordUnknown,
			wantErr:  sessionstore.ErrCorruptLog,
		},
		{
			name: "header identity mismatch",
			mutate: func(header, _, _ string) string {
				return strings.Replace(header, `"id":"session"`, `"id":"other"`, 1)
			},
			wantCode: warning.WarnSessionLogCorruptMiddle,
			wantErr:  sessionstore.ErrCorruptLog,
		},
		{
			name: "header zero order",
			mutate: func(header, _, _ string) string {
				return strings.Replace(header, `"order":1`, `"order":0`, 1)
			},
			wantCode: warning.WarnSessionLogCorruptMiddle,
			wantErr:  sessionstore.ErrCorruptLog,
		},
		{
			name:     "duplicate header",
			mutate:   func(header, _, _ string) string { return header + header },
			wantCode: warning.WarnSessionLogCorruptMiddle,
			wantErr:  sessionstore.ErrCorruptLog,
		},
		{
			name: "event without id",
			mutate: func(header, event, _ string) string {
				return header + strings.Replace(event, `"id":"event"`, `"id":""`, 1)
			},
			wantCode: warning.WarnSessionLogCorruptMiddle,
			wantErr:  sessionstore.ErrCorruptLog,
		},
		{
			name: "event payload type mismatch",
			mutate: func(header, _, _ string) string {
				return header + `{"v":2,"type":"event","order":2,"event":{"id":"event","actions":{"stateDelta":"bad"}}}` + "\n"
			},
			wantCode: warning.WarnSessionLogCorruptMiddle,
			wantErr:  sessionstore.ErrCorruptLog,
		},
		{
			name: "missing event payload",
			mutate: func(header, _, _ string) string {
				return header + `{"v":2,"type":"event","order":2}` + "\n"
			},
			wantCode: warning.WarnSessionLogCorruptMiddle,
			wantErr:  sessionstore.ErrCorruptLog,
		},
		{
			name: "missing sidecar payload",
			mutate: func(header, _, _ string) string {
				return header + `{"v":2,"type":"sidecar"}` + "\n"
			},
			wantCode: warning.WarnSessionLogCorruptMiddle,
			wantErr:  sessionstore.ErrCorruptLog,
		},
		{
			name: "missing session payload",
			mutate: func(_, _, _ string) string {
				return `{"v":2,"type":"session","order":1}` + "\n"
			},
			wantCode: warning.WarnSessionLogCorruptMiddle,
			wantErr:  sessionstore.ErrCorruptLog,
		},
		{
			name: "event without order",
			mutate: func(header, event, _ string) string {
				return header + strings.Replace(event, `"order":2`, `"order":0`, 1)
			},
			wantCode: warning.WarnSessionLogCorruptMiddle,
			wantErr:  sessionstore.ErrCorruptLog,
		},
		{
			name:     "duplicate event",
			mutate:   func(header, event, _ string) string { return header + event + event },
			wantCode: warning.WarnSessionLogCorruptMiddle,
			wantErr:  sessionstore.ErrCorruptLog,
		},
		{
			name: "sidecar without kind",
			mutate: func(header, _, sidecar string) string {
				return header + strings.Replace(sidecar, `"kind":"kind"`, `"kind":""`, 1)
			},
			wantCode: warning.WarnSessionLogCorruptMiddle,
			wantErr:  sessionstore.ErrCorruptLog,
		},
		{
			name: "forward compatible records",
			mutate: func(header, _, _ string) string {
				return header + `{"v":99,"type":"future","order":50}` + "\n" + `{"v":2,"type":"future","order":51}` + "\n"
			},
			wantCode: warning.WarnSessionRecordUnsupportedVersion,
		},
	}

	for _, test := range scenarios {
		t.Run(test.name, func(t *testing.T) {
			testTranscriptScenario(t, test)
		})
	}
}

func testTranscriptScenario(t *testing.T, test transcriptScenario) {
	const transcriptSessionID = publicSessionID
	dir := t.TempDir()
	noSync := false
	store := openStore(t, sessionstore.Options{Dir: dir, Fsync: &noSync})
	created := createSession(t, store, "app", "user", transcriptSessionID, nil)
	if err := store.AppendEvent(t.Context(), created, &session.Event{ID: "event", Timestamp: time.Unix(1, 0).UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSidecar(t.Context(), "app", "user", transcriptSessionID, "kind", map[string]any{"ok": true}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	path := transcriptPath(dir, "app", "user", transcriptSessionID)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitAfter(string(data), "\n")
	if len(lines) < 4 {
		t.Fatalf("transcript lines = %q", data)
	}
	if err := os.WriteFile(path, []byte(test.mutate(lines[0], lines[1], lines[2])), 0o600); err != nil {
		t.Fatal(err)
	}
	var warnings warning.SliceSink
	reopened := openStore(t, sessionstore.Options{Dir: dir, Fsync: &noSync, WarningSink: &warnings})
	got, err := reopened.Get(t.Context(), &session.GetRequest{AppName: "app", UserID: "user", SessionID: transcriptSessionID})
	if !errors.Is(err, test.wantErr) {
		t.Fatalf("Get error = %v, want %v", err, test.wantErr)
	}
	if test.wantErr == nil && (got == nil || got.Session.ID() != transcriptSessionID) {
		t.Fatalf("Get = %#v, %v", got, err)
	}
	if test.wantCode != "" && !warningCodePresent(warnings.Warnings(), test.wantCode) {
		t.Fatalf("warnings = %#v, want code %q", warnings.Warnings(), test.wantCode)
	}
}

type journalScenario struct {
	name     string
	mutate   func(string) string
	wantCode string
	wantErr  error
}

func TestStorePublicJournalRecoveryAndValidation(t *testing.T) {
	scenarios := []journalScenario{
		{
			name:     "torn tail",
			mutate:   func(valid string) string { return valid + `{"v":1,"id":"torn"` },
			wantCode: warning.WarnSessionLogTornTail,
		},
		{name: "malformed JSON", mutate: func(string) string { return "{bad}\n" }, wantErr: sessionstore.ErrCorruptLog},
		{name: "invalid version", mutate: func(string) string { return `{"v":2,"id":"bad","order":1,"delta":{}}` + "\n" }, wantErr: sessionstore.ErrCorruptLog},
		{name: "empty id", mutate: func(string) string { return `{"v":1,"id":"","order":1,"delta":{}}` + "\n" }, wantErr: sessionstore.ErrCorruptLog},
		{name: "zero order", mutate: func(string) string { return `{"v":1,"id":"bad","order":0,"delta":{}}` + "\n" }, wantErr: sessionstore.ErrCorruptLog},
		{
			name: "duplicate id",
			mutate: func(valid string) string {
				return valid + valid
			},
			wantErr: sessionstore.ErrCorruptLog,
		},
		{
			name: "duplicate order",
			mutate: func(valid string) string {
				return valid + strings.Replace(valid, `"id":"`, `"id":"other-`, 1)
			},
			wantErr: sessionstore.ErrCorruptLog,
		},
	}

	for _, test := range scenarios {
		t.Run(test.name, func(t *testing.T) {
			testJournalScenario(t, test)
		})
	}
}

func testJournalScenario(t *testing.T, test journalScenario) {
	dir := t.TempDir()
	noSync := false
	store := openStore(t, sessionstore.Options{Dir: dir, Fsync: &noSync})
	createSession(t, store, "app", "user", publicSessionID, map[string]any{"app:key": "value", "user:key": "value"})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(dir, "apps", "app", "app_state.jsonl")
	valid, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(journal, []byte(test.mutate(string(valid))), 0o600); err != nil {
		t.Fatal(err)
	}
	var warnings warning.SliceSink
	reopened := openStore(t, sessionstore.Options{Dir: dir, Fsync: &noSync, WarningSink: &warnings})
	got, err := reopened.Get(t.Context(), &session.GetRequest{AppName: "app", UserID: "user", SessionID: publicSessionID})
	if !errors.Is(err, test.wantErr) {
		t.Fatalf("Get error = %v, want %v", err, test.wantErr)
	}
	if test.wantErr == nil && (got == nil || got.Session.ID() != publicSessionID) {
		t.Fatalf("Get = %#v, %v", got, err)
	}
	if test.wantCode != "" && !warningCodePresent(warnings.Warnings(), test.wantCode) {
		t.Fatalf("warnings = %#v, want %q", warnings.Warnings(), test.wantCode)
	}
}

func TestStorePublicPendingCreateAndDeleteRecovery(t *testing.T) {
	t.Run("resume matching explicit create", testResumeMatchingExplicitCreate)
	t.Run("reject conflicting explicit create", testRejectConflictingExplicitCreate)
	t.Run("reject malformed marker", testRejectMalformedCreateMarker)
	t.Run("delete pending transaction", testDeletePendingCreateTransaction)
	for _, test := range []deleteMarkerScenario{
		{name: "same incarnation", marker: "1", wantPresent: false},
		{name: "stale incarnation", marker: "999", wantPresent: true},
		{name: "missing transcript", marker: "1", removeLog: true, wantPresent: false},
		{name: "invalid marker", marker: "0", wantErr: true, wantPresent: true},
	} {
		t.Run("delete marker "+test.name, func(t *testing.T) {
			testDeleteMarkerRecovery(t, test)
		})
	}
}

type deleteMarkerScenario struct {
	name        string
	marker      string
	removeLog   bool
	wantErr     bool
	wantPresent bool
}

func testResumeMatchingExplicitCreate(t *testing.T) {
	dir := t.TempDir()
	writePendingCreate(t, dir, publicSessionID, false, map[string]any{"initial": "value"})
	store := openStore(t, sessionstore.Options{Dir: dir, Fsync: boolPointer(false)})
	created, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: publicSessionID, State: map[string]any{"initial": "value"}})
	if err != nil || created.Session.ID() != publicSessionID {
		t.Fatalf("Create = %#v, %v", created, err)
	}
	if _, err := os.Stat(transcriptPath(dir, "app", "user", publicSessionID) + ".create-pending"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pending marker still exists: %v", err)
	}
}

func testRejectConflictingExplicitCreate(t *testing.T) {
	dir := t.TempDir()
	writePendingCreate(t, dir, publicSessionID, false, map[string]any{"initial": "value"})
	store := openStore(t, sessionstore.Options{Dir: dir, Fsync: boolPointer(false)})
	_, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: publicSessionID, State: map[string]any{"initial": "different"}})
	if !errors.Is(err, sessionstore.ErrSessionExists) {
		t.Fatalf("Create error = %v, want ErrSessionExists", err)
	}
}

func testRejectMalformedCreateMarker(t *testing.T) {
	dir := t.TempDir()
	path := transcriptPath(dir, "app", "user", publicSessionID) + ".create-pending"
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"v":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store := openStore(t, sessionstore.Options{Dir: dir, Fsync: boolPointer(false)})
	if _, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: publicSessionID}); !errors.Is(err, sessionstore.ErrCorruptLog) {
		t.Fatalf("Create error = %v, want ErrCorruptLog", err)
	}
}

func testDeletePendingCreateTransaction(t *testing.T) {
	dir := t.TempDir()
	writePendingCreate(t, dir, publicSessionID, false, nil)
	store := openStore(t, sessionstore.Options{Dir: dir, Fsync: boolPointer(false)})
	request := &session.DeleteRequest{AppName: "app", UserID: "user", SessionID: publicSessionID}
	if err := store.Delete(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(t.Context(), request); err != nil {
		t.Fatalf("idempotent Delete = %v", err)
	}
}

func testDeleteMarkerRecovery(t *testing.T, test deleteMarkerScenario) {
	dir := t.TempDir()
	store := openStore(t, sessionstore.Options{Dir: dir, Fsync: boolPointer(false)})
	createSession(t, store, "app", "user", publicSessionID, nil)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	logPath := transcriptPath(dir, "app", "user", publicSessionID)
	if test.removeLog {
		if err := os.Remove(logPath); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(logPath+".delete-pending", []byte(test.marker), 0o600); err != nil {
		t.Fatal(err)
	}
	reopened := openStore(t, sessionstore.Options{Dir: dir, Fsync: boolPointer(false)})
	err := reopened.Delete(t.Context(), &session.DeleteRequest{AppName: "app", UserID: "user", SessionID: publicSessionID})
	if (err != nil) != test.wantErr {
		t.Fatalf("Delete error = %v, wantErr %v", err, test.wantErr)
	}
	_, getErr := reopened.Get(t.Context(), &session.GetRequest{AppName: "app", UserID: "user", SessionID: publicSessionID})
	if present := getErr == nil; present != test.wantPresent {
		t.Fatalf("session present = %v, Get error = %v", present, getErr)
	}
}

func TestStorePublicGeneratedCreateRecovery(t *testing.T) {
	t.Run("uses configured id generator", testConfiguredIDGenerator)
	t.Run("rejects invalid generated id", testInvalidGeneratedID)
	t.Run("resumes matching generated marker", testResumeGeneratedMarker)
	t.Run("rejects different generated state", testRejectDifferentGeneratedState)
	t.Run("rejects multiple matching markers", testRejectMultipleGeneratedMarkers)
	t.Run("ignores unrelated directory entries", testIgnoreUnrelatedCreateEntries)
}

func testConfiguredIDGenerator(t *testing.T) {
	const generatedSessionID = "generated"
	store := openStore(t, sessionstore.Options{Dir: t.TempDir(), Fsync: boolPointer(false), NewID: func() string { return generatedSessionID }})
	created, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user"})
	if err != nil || created.Session.ID() != generatedSessionID {
		t.Fatalf("Create = %#v, %v", created, err)
	}
}

func testInvalidGeneratedID(t *testing.T) {
	store := openStore(t, sessionstore.Options{Dir: t.TempDir(), Fsync: boolPointer(false), NewID: func() string { return "" }})
	if _, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user"}); !errors.Is(err, sessionstore.ErrInvalidID) {
		t.Fatalf("Create error = %v, want ErrInvalidID", err)
	}
}

func testResumeGeneratedMarker(t *testing.T) {
	dir := t.TempDir()
	writePendingCreate(t, dir, "pending", true, map[string]any{"initial": "value"})
	store := openStore(t, sessionstore.Options{Dir: dir, Fsync: boolPointer(false), NewID: func() string { return "unused" }})
	created, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", State: map[string]any{"initial": "value"}})
	if err != nil || created.Session.ID() != "pending" {
		t.Fatalf("Create = %#v, %v", created, err)
	}
}

func testRejectDifferentGeneratedState(t *testing.T) {
	dir := t.TempDir()
	writePendingCreate(t, dir, "pending", true, map[string]any{"initial": "value"})
	store := openStore(t, sessionstore.Options{Dir: dir, Fsync: boolPointer(false)})
	if _, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", State: map[string]any{"initial": "different"}}); err == nil {
		t.Fatal("Create with different state succeeded")
	}
}

func testRejectMultipleGeneratedMarkers(t *testing.T) {
	dir := t.TempDir()
	writePendingCreate(t, dir, "one", true, nil)
	writePendingCreate(t, dir, "two", true, nil)
	store := openStore(t, sessionstore.Options{Dir: dir, Fsync: boolPointer(false)})
	if _, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user"}); err == nil {
		t.Fatal("Create with multiple pending markers succeeded")
	}
}

func testIgnoreUnrelatedCreateEntries(t *testing.T) {
	dir := t.TempDir()
	sessions := filepath.Dir(transcriptPath(dir, "app", "user", publicSessionID))
	for _, path := range []string{
		filepath.Join(dir, "apps", "app", "users", "plain-file"),
		filepath.Join(dir, "apps", "app", "users", "%zz", "sessions", "bad.jsonl"),
		filepath.Join(sessions, "nested.jsonl"),
		filepath.Join(sessions, "notes.txt"),
		filepath.Join(sessions, "%zz.jsonl"),
		filepath.Join(sessions, "%zz.jsonl.create-pending"),
	} {
		if strings.HasSuffix(path, "nested.jsonl") {
			if err := os.MkdirAll(path, 0o700); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("junk"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	store := openStore(t, sessionstore.Options{Dir: dir, Fsync: boolPointer(false), NewID: func() string { return publicSessionID }})
	created, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user"})
	if err != nil || created.Session.ID() != publicSessionID {
		t.Fatalf("Create = %#v, %v", created, err)
	}
}

func TestStorePublicDirectoryReadFailures(t *testing.T) {
	for _, test := range []struct {
		name string
		path func(string) string
	}{
		{name: "users is a file", path: func(dir string) string { return filepath.Join(dir, "apps", "app", "users") }},
		{name: "sessions is a file", path: func(dir string) string { return filepath.Join(dir, "apps", "app", "users", "user", "sessions") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			path := test.path(dir)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("not a directory"), 0o600); err != nil {
				t.Fatal(err)
			}
			store := openStore(t, sessionstore.Options{Dir: dir, Fsync: boolPointer(false)})
			if _, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: publicSessionID}); err == nil {
				t.Fatal("Create succeeded with malformed storage tree")
			}
		})
	}
}

func TestStorePublicFilesystemFailures(t *testing.T) {
	if runtime.GOOS == publicWindowsOS {
		t.Skip("Windows does not enforce POSIX owner mode bits")
	}

	t.Run("open protected directory", testOpenProtectedDirectory)
	t.Run("unreadable app journal", func(t *testing.T) { testUnreadableJournal(t, false) })
	t.Run("unreadable user journal", func(t *testing.T) { testUnreadableJournal(t, true) })
	t.Run("read-only torn journal", testReadOnlyTornJournal)
	t.Run("unreadable snapshot falls back to journals", testUnreadableSnapshotFallback)
	t.Run("unreadable marker", testUnreadableCreateMarker)

	for _, test := range []struct {
		name string
		path func(string) string
		run  func(*sessionstore.Store) error
	}{
		{
			name: "unreadable users directory",
			path: func(dir string) string { return filepath.Join(dir, "apps", "app", "users") },
			run: func(store *sessionstore.Store) error {
				_, err := store.List(t.Context(), &session.ListRequest{AppName: "app"})
				return err
			},
		},
		{
			name: "unreadable sessions directory",
			path: func(dir string) string { return filepath.Join(dir, "apps", "app", "users", "user", "sessions") },
			run: func(store *sessionstore.Store) error {
				_, err := store.Get(t.Context(), &session.GetRequest{AppName: "app", UserID: "user", SessionID: publicSessionID})
				return err
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			testUnreadableDirectory(t, test.path, test.run)
		})
	}
	t.Run("read-only storage root", testReadOnlyStorageRoot)
}

func testOpenProtectedDirectory(t *testing.T) {
	if _, err := sessionstore.Open("/proc"); err == nil {
		t.Fatal("Open protected directory succeeded")
	}
}

func testUnreadableJournal(t *testing.T, user bool) {
	dir := populatedStoreDirectory(t)
	journal := filepath.Join(dir, "apps", "app", "app_state.jsonl")
	if user {
		journal = filepath.Join(dir, "apps", "app", "users", "user", "user_state.jsonl")
	}
	chmodForTest(t, journal, 0)
	store := openStore(t, sessionstore.Options{Dir: dir, Fsync: boolPointer(false)})
	if _, err := store.Get(t.Context(), &session.GetRequest{AppName: "app", UserID: "user", SessionID: publicSessionID}); err == nil {
		t.Fatal("Get with unreadable journal succeeded")
	}
}

func testReadOnlyTornJournal(t *testing.T) {
	dir := populatedStoreDirectory(t)
	journal := filepath.Join(dir, "apps", "app", "app_state.jsonl")
	file, err := os.OpenFile(journal, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"v":1`); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	chmodForTest(t, journal, 0o400)
	store := openStore(t, sessionstore.Options{Dir: dir, Fsync: boolPointer(false)})
	if _, err := store.Get(t.Context(), &session.GetRequest{AppName: "app", UserID: "user", SessionID: publicSessionID}); err == nil {
		t.Fatal("Get repaired a read-only torn journal")
	}
}

func testUnreadableSnapshotFallback(t *testing.T) {
	dir := populatedStoreDirectory(t)
	snapshot := filepath.Join(dir, "apps", "app", "app_state.json")
	chmodForTest(t, snapshot, 0)
	var warnings warning.SliceSink
	store := openStore(t, sessionstore.Options{Dir: dir, Fsync: boolPointer(false), WarningSink: &warnings})
	got, err := store.Get(t.Context(), &session.GetRequest{AppName: "app", UserID: "user", SessionID: publicSessionID})
	if err != nil || got.Session.ID() != publicSessionID {
		t.Fatalf("Get = %#v, %v", got, err)
	}
	if !warningCodePresent(warnings.Warnings(), warning.WarnSessionSnapshotRefresh) {
		t.Fatalf("warnings = %#v", warnings.Warnings())
	}
}

func testUnreadableCreateMarker(t *testing.T) {
	dir := t.TempDir()
	writePendingCreate(t, dir, publicSessionID, false, nil)
	chmodForTest(t, transcriptPath(dir, "app", "user", publicSessionID)+".create-pending", 0)
	store := openStore(t, sessionstore.Options{Dir: dir, Fsync: boolPointer(false)})
	if _, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: publicSessionID}); err == nil {
		t.Fatal("Create with unreadable marker succeeded")
	}
}

func testUnreadableDirectory(t *testing.T, path func(string) string, run func(*sessionstore.Store) error) {
	dir := populatedStoreDirectory(t)
	chmodForTest(t, path(dir), 0)
	store := openStore(t, sessionstore.Options{Dir: dir, Fsync: boolPointer(false)})
	if err := run(store); err == nil {
		t.Fatal("operation with unreadable directory succeeded")
	}
}

func testReadOnlyStorageRoot(t *testing.T) {
	dir := t.TempDir()
	store := openStore(t, sessionstore.Options{Dir: dir, Fsync: boolPointer(false)})
	chmodForTest(t, dir, 0o500)
	if _, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: publicSessionID}); err == nil {
		t.Fatal("Create in read-only root succeeded")
	}
}

func TestStorePublicSessionViewsAndFiltering(t *testing.T) {
	store := openStore(t, sessionstore.Options{Dir: t.TempDir()})
	first := createSession(t, store, "app", "z-user", "z-session", map[string]any{"initial": "value"})
	second := createSession(t, store, "app", "a-user", "a-session", nil)

	base := time.Date(2026, 8, 18, 1, 0, 0, 0, time.UTC)
	for index, id := range []string{"one", "two", "three"} {
		event := &session.Event{ID: id, Timestamp: base.Add(time.Duration(index) * time.Minute)}
		if err := store.AppendEvent(t.Context(), first, event); err != nil {
			t.Fatal(err)
		}
	}
	testSessionStateView(t, first)
	testSessionEventView(t, first)
	testSessionFiltering(t, store, first, second, base)
}

func testSessionStateView(t *testing.T, first session.Session) {
	if first.ID() != "z-session" || first.AppName() != "app" || first.UserID() != "z-user" {
		t.Fatalf("session identity = %q/%q/%q", first.AppName(), first.UserID(), first.ID())
	}
	if err := first.State().Set("ephemeral", "visible"); err != nil {
		t.Fatal(err)
	}
	if value, err := first.State().Get("ephemeral"); err != nil || value != "visible" {
		t.Fatalf("State.Get = %#v, %v", value, err)
	}
	if _, err := first.State().Get("missing"); !errors.Is(err, session.ErrStateKeyNotExist) {
		t.Fatalf("State.Get missing error = %v", err)
	}
	state := maps.Collect(first.State().All())
	if state["initial"] != "value" || state["ephemeral"] != "visible" {
		t.Fatalf("State.All = %#v", state)
	}
	for range first.State().All() {
		break
	}
}

func testSessionEventView(t *testing.T, first session.Session) {
	if first.Events().Len() != 3 || first.Events().At(-1) != nil || first.Events().At(3) != nil {
		t.Fatalf("Events bounds/length = %d, %v, %v", first.Events().Len(), first.Events().At(-1), first.Events().At(3))
	}
	if got := slices.Collect(first.Events().All()); len(got) != 3 || got[0].ID != "one" {
		t.Fatalf("Events.All = %#v", got)
	}
	for range first.Events().All() {
		break
	}
}

func testSessionFiltering(t *testing.T, store *sessionstore.Store, first, second session.Session, base time.Time) {
	recent, err := store.Get(t.Context(), &session.GetRequest{
		AppName: "app", UserID: "z-user", SessionID: "z-session", NumRecentEvents: 2, After: base.Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := slices.Collect(recent.Session.Events().All()); len(got) != 1 || got[0].ID != "three" {
		t.Fatalf("filtered events = %#v", got)
	}
	if !recent.Session.LastUpdateTime().Equal(base.Add(2 * time.Minute)) {
		t.Fatalf("filtered LastUpdateTime = %v", recent.Session.LastUpdateTime())
	}

	all, err := store.List(t.Context(), &session.ListRequest{AppName: "app"})
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{all.Sessions[0].ID(), all.Sessions[1].ID()}; !slices.Equal(got, []string{"a-session", "z-session"}) {
		t.Fatalf("List ordering = %v", got)
	}
	filtered, err := store.List(t.Context(), &session.ListRequest{AppName: "app", UserID: second.UserID()})
	if err != nil || len(filtered.Sessions) != 1 || filtered.Sessions[0].ID() != second.ID() {
		t.Fatalf("filtered List = %#v, %v", filtered, err)
	}
}

func TestStorePublicExactEventRetryRefreshesSharedState(t *testing.T) {
	store := openStore(t, sessionstore.Options{Dir: t.TempDir(), Fsync: boolPointer(false)})
	created := createSession(t, store, "app", "user", publicSessionID, nil)
	event := &session.Event{ID: "event", Timestamp: time.Unix(1, 0).UTC(), Actions: session.EventActions{StateDelta: map[string]any{"app:key": "value"}}}
	if err := store.AppendEvent(t.Context(), created, event); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendEvent(t.Context(), created, event); err != nil {
		t.Fatalf("exact retry = %v", err)
	}
	if created.Events().Len() != 1 {
		t.Fatalf("event count after retry = %d", created.Events().Len())
	}
	if value, err := created.State().Get("app:key"); err != nil || value != "value" {
		t.Fatalf("shared state after retry = %#v, %v", value, err)
	}
	conflict := &session.Event{ID: "event", Timestamp: time.Unix(2, 0).UTC()}
	if err := store.AppendEvent(t.Context(), created, conflict); !errors.Is(err, sessionstore.ErrInvalidEvent) {
		t.Fatalf("conflicting retry error = %v, want ErrInvalidEvent", err)
	}
}

func TestStorePublicStaleHandleAndTranscriptFailures(t *testing.T) {
	t.Run("stale incarnation", testStaleIncarnation)
	t.Run("unreadable transcript", testUnreadableTranscript)
}

func testStaleIncarnation(t *testing.T) {
	store := openStore(t, sessionstore.Options{Dir: t.TempDir(), Fsync: boolPointer(false)})
	stale := createSession(t, store, "app", "user", publicSessionID, nil)
	request := &session.DeleteRequest{AppName: "app", UserID: "user", SessionID: publicSessionID}
	if err := store.Delete(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	createSession(t, store, "app", "user", publicSessionID, nil)
	if err := store.AppendEvent(t.Context(), stale, &session.Event{ID: "event"}); !errors.Is(err, sessionstore.ErrInvalidEvent) {
		t.Fatalf("AppendEvent stale handle error = %v, want ErrInvalidEvent", err)
	}
}

func testUnreadableTranscript(t *testing.T) {
	if runtime.GOOS == publicWindowsOS {
		t.Skip("Windows does not enforce POSIX owner mode bits")
	}
	dir := t.TempDir()
	store := openStore(t, sessionstore.Options{Dir: dir, Fsync: boolPointer(false)})
	created := createSession(t, store, "app", "user", publicSessionID, nil)
	path := transcriptPath(dir, "app", "user", publicSessionID)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendEvent(t.Context(), created, &session.Event{ID: "event"}); err == nil {
		t.Fatal("AppendEvent with unreadable transcript succeeded")
	}
	if err := store.AppendSidecar(t.Context(), "app", "user", publicSessionID, "kind", true); err == nil {
		t.Fatal("AppendSidecar with unreadable transcript succeeded")
	}
	if ok, err := store.LoadSidecar(t.Context(), "app", "user", publicSessionID, "kind", new(bool)); ok || err == nil {
		t.Fatalf("LoadSidecar with unreadable transcript = %v, %v", ok, err)
	}
}

func TestStorePublicProjectionWriteRecovery(t *testing.T) {
	if runtime.GOOS == publicWindowsOS {
		t.Skip("Windows does not enforce POSIX owner mode bits")
	}

	t.Run("sequence destination is a directory", testSequenceDestinationDirectory)
	t.Run("snapshot destination is a directory", testSnapshotDestinationDirectory)
}

func testSequenceDestinationDirectory(t *testing.T) {
	dir := populatedStoreDirectory(t)
	sequence := filepath.Join(dir, "apps", "app", "shared_sequence")
	if err := os.Remove(sequence); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(sequence, 0o700); err != nil {
		t.Fatal(err)
	}
	store := openStore(t, sessionstore.Options{Dir: dir, Fsync: boolPointer(false)})
	request := &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "second"}
	if _, err := store.Create(t.Context(), request); err == nil {
		t.Fatal("Create with directory sequence destination succeeded")
	}
	if err := os.Remove(sequence); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(t.Context(), request); err != nil {
		t.Fatalf("Create after restoring sequence parent = %v", err)
	}

}

func testSnapshotDestinationDirectory(t *testing.T) {
	dir := t.TempDir()
	badSnapshot := filepath.Join(dir, "apps", "app", "users", "user", "user_state.json")
	if err := os.MkdirAll(badSnapshot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badSnapshot, "entry"), []byte("blocks replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := openStore(t, sessionstore.Options{Dir: dir, Fsync: boolPointer(false)})
	request := &session.CreateRequest{AppName: "app", UserID: "user", SessionID: publicSessionID}
	if _, err := store.Create(t.Context(), request); err == nil {
		t.Fatal("Create with directory snapshot destination succeeded")
	}
	if err := os.RemoveAll(badSnapshot); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(t.Context(), request); err != nil {
		t.Fatalf("Create retry after snapshot repair = %v", err)
	}
}

func TestStorePublicAuthorityFailureFallback(t *testing.T) {
	t.Run("missing journal beside snapshot is corruption", testMissingAuthorityJournal)
	t.Run("cached authority survives missing journal", testCachedAuthorityMissingJournal)
	t.Run("snapshot read failure falls back to journals", testSnapshotReadFailureFallback)
	t.Run("exact replay uses cached authority", testExactReplayCachedAuthority)
}

func testMissingAuthorityJournal(t *testing.T) {
	for _, journal := range []string{
		filepath.Join("apps", "app", "app_state.jsonl"),
		filepath.Join("apps", "app", "users", "user", "user_state.jsonl"),
	} {
		t.Run(filepath.Base(filepath.Dir(journal))+"_"+filepath.Base(journal), func(t *testing.T) {
			dir := populatedStoreDirectory(t)
			if err := os.Remove(filepath.Join(dir, journal)); err != nil {
				t.Fatal(err)
			}
			store := openStore(t, sessionstore.Options{Dir: dir, Fsync: boolPointer(false)})
			if _, err := store.Get(t.Context(), &session.GetRequest{AppName: "app", UserID: "user", SessionID: publicSessionID}); err == nil {
				t.Fatal("Get with missing authority journal succeeded")
			}
		})
	}

}

func testCachedAuthorityMissingJournal(t *testing.T) {
	dir := populatedStoreDirectory(t)
	store := openStore(t, sessionstore.Options{Dir: dir, Fsync: boolPointer(false)})
	request := &session.GetRequest{AppName: "app", UserID: "user", SessionID: publicSessionID}
	if _, err := store.Get(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "apps", "app", "app_state.jsonl")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(t.Context(), request); err == nil {
		t.Fatal("Get with missing authority journal succeeded")
	}
}

func testSnapshotReadFailureFallback(t *testing.T) {
	dir := populatedStoreDirectory(t)
	snapshot := filepath.Join(dir, "apps", "app", "app_state.json")
	if err := os.Remove(snapshot); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(snapshot, 0o700); err != nil {
		t.Fatal(err)
	}
	store := openStore(t, sessionstore.Options{Dir: dir, Fsync: boolPointer(false)})
	got, err := store.Get(t.Context(), &session.GetRequest{AppName: "app", UserID: "user", SessionID: publicSessionID})
	if err != nil || got.Session.ID() != publicSessionID {
		t.Fatalf("Get with unreadable snapshot = %#v, %v", got, err)
	}
}

func testExactReplayCachedAuthority(t *testing.T) {
	dir := t.TempDir()
	store := openStore(t, sessionstore.Options{Dir: dir, Fsync: boolPointer(false)})
	created := createSession(t, store, "app", "user", publicSessionID, nil)
	event := &session.Event{ID: "event", Actions: session.EventActions{StateDelta: map[string]any{"app:key": "value"}}}
	if err := store.AppendEvent(t.Context(), created, event); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "apps", "app", "app_state.jsonl")); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendEvent(t.Context(), created, event); err != nil {
		t.Fatalf("exact replay with missing authority = %v", err)
	}
}

func TestStorePublicDeleteMarkerReadFailures(t *testing.T) {
	t.Run("delete marker is a directory", func(t *testing.T) {
		dir := populatedStoreDirectory(t)
		marker := transcriptPath(dir, "app", "user", publicSessionID) + ".delete-pending"
		if err := os.Mkdir(marker, 0o700); err != nil {
			t.Fatal(err)
		}
		store := openStore(t, sessionstore.Options{Dir: dir, Fsync: boolPointer(false)})
		if err := store.Delete(t.Context(), &session.DeleteRequest{AppName: "app", UserID: "user", SessionID: publicSessionID}); err == nil {
			t.Fatal("Delete with unreadable marker succeeded")
		}
	})

	t.Run("create marker is a directory", func(t *testing.T) {
		dir := populatedStoreDirectory(t)
		marker := transcriptPath(dir, "app", "user", publicSessionID) + ".create-pending"
		if err := os.Mkdir(marker, 0o700); err != nil {
			t.Fatal(err)
		}
		store := openStore(t, sessionstore.Options{Dir: dir, Fsync: boolPointer(false)})
		if err := store.Delete(t.Context(), &session.DeleteRequest{AppName: "app", UserID: "user", SessionID: publicSessionID}); err == nil {
			t.Fatal("Delete with unreadable create marker succeeded")
		}
	})

	t.Run("pending delete preserves corrupt transcript", func(t *testing.T) {
		dir := populatedStoreDirectory(t)
		path := transcriptPath(dir, "app", "user", publicSessionID)
		if err := os.WriteFile(path, []byte("{bad}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path+".delete-pending", []byte("1"), 0o600); err != nil {
			t.Fatal(err)
		}
		store := openStore(t, sessionstore.Options{Dir: dir, Fsync: boolPointer(false)})
		if err := store.Delete(t.Context(), &session.DeleteRequest{AppName: "app", UserID: "user", SessionID: publicSessionID}); !errors.Is(err, sessionstore.ErrCorruptLog) {
			t.Fatalf("Delete error = %v, want ErrCorruptLog", err)
		}
	})
}

func TestStorePublicInventoryToleratesUnidentifiedTranscriptRecords(t *testing.T) {
	dir := t.TempDir()
	store := openStore(t, sessionstore.Options{Dir: dir, Fsync: boolPointer(false)})
	createSession(t, store, "app", "user", "first", nil)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	path := transcriptPath(dir, "app", "user", "first")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data,
		[]byte("\n{bad}\n"+`{"v":2,"type":"event","order":0,"event":{"id":"zero"}}`+"\n"+
			`{"v":2,"type":"event","order":40,"event":{"id":"late"}}`+"\n"+
			`{"v":2,"type":"future","order":41}`+"\n"+
			`{"v":2,"type":"sidecar","order":42,"sidecar":{"kind":"kind","data":true}}`+"\n"+
			`{"v":2,"type":"future"`)...)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	reopened := openStore(t, sessionstore.Options{Dir: dir, Fsync: boolPointer(false)})
	if _, err := reopened.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "second"}); err != nil {
		t.Fatalf("Create with unidentified transcript records = %v", err)
	}
}

func TestStorePublicExactReplayRejectsTranscriptJournalConflict(t *testing.T) {
	dir := t.TempDir()
	store := openStore(t, sessionstore.Options{Dir: dir, Fsync: boolPointer(false)})
	created := createSession(t, store, "app", "user", publicSessionID, nil)
	original := &session.Event{ID: "event", Actions: session.EventActions{StateDelta: map[string]any{"app:key": "original"}}}
	if err := store.AppendEvent(t.Context(), created, original); err != nil {
		t.Fatal(err)
	}
	path := transcriptPath(dir, "app", "user", publicSessionID)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), `"original"`, `"contradiction"`, 1))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	conflicting := &session.Event{ID: "event", Actions: session.EventActions{StateDelta: map[string]any{"app:key": "contradiction"}}}
	if err := store.AppendEvent(t.Context(), created, conflicting); !errors.Is(err, sessionstore.ErrCorruptLog) {
		t.Fatalf("AppendEvent error = %v, want ErrCorruptLog", err)
	}
}

func TestStorePublicExactReplayRejectsUserJournalConflict(t *testing.T) {
	dir := t.TempDir()
	store := openStore(t, sessionstore.Options{Dir: dir, Fsync: boolPointer(false)})
	created := createSession(t, store, "app", "user", publicSessionID, nil)
	original := &session.Event{ID: "event", Actions: session.EventActions{StateDelta: map[string]any{"user:key": "original"}}}
	if err := store.AppendEvent(t.Context(), created, original); err != nil {
		t.Fatal(err)
	}
	path := transcriptPath(dir, "app", "user", publicSessionID)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), `"original"`, `"contradiction"`, 1))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	conflicting := &session.Event{ID: "event", Actions: session.EventActions{StateDelta: map[string]any{"user:key": "contradiction"}}}
	if err := store.AppendEvent(t.Context(), created, conflicting); !errors.Is(err, sessionstore.ErrCorruptLog) {
		t.Fatalf("AppendEvent error = %v, want ErrCorruptLog", err)
	}
}

func TestStorePublicDuplicateTranscriptOrderIsCorrupt(t *testing.T) {
	dir := t.TempDir()
	store := openStore(t, sessionstore.Options{Dir: dir, Fsync: boolPointer(false)})
	createSession(t, store, "app", "user", "first", nil)
	createSession(t, store, "app", "user", "second", nil)
	secondPath := transcriptPath(dir, "app", "user", "second")
	data, err := os.ReadFile(secondPath)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &raw); err != nil {
		t.Fatal(err)
	}
	raw["order"] = float64(1)
	header := raw[publicSessionID].(map[string]any)
	header["incarnation"] = float64(1)
	data, err = json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secondPath, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(t.Context(), &session.GetRequest{AppName: "app", UserID: "user", SessionID: "first"}); !errors.Is(err, sessionstore.ErrCorruptLog) {
		t.Fatalf("Get error = %v, want ErrCorruptLog", err)
	}
}

func TestStorePublicCreateMarkerPathIdentityIsAuthoritative(t *testing.T) {
	dir := t.TempDir()
	writePendingCreate(t, dir, "pending", false, nil)
	path := transcriptPath(dir, "app", "user", "pending") + ".create-pending"
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), `"id":"pending"`, `"id":"other"`, 1))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	store := openStore(t, sessionstore.Options{Dir: dir, Fsync: boolPointer(false)})
	if _, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "new"}); !errors.Is(err, sessionstore.ErrCorruptLog) {
		t.Fatalf("Create error = %v, want ErrCorruptLog", err)
	}
}

func TestStorePublicBlankJournalRecordsAreIgnored(t *testing.T) {
	dir := t.TempDir()
	writeJournal(t, dir, "app", "", "\n"+`{"v":1,"id":"retained","order":10,"delta":{}}`+"\n")
	store := openStore(t, sessionstore.Options{Dir: dir, Fsync: boolPointer(false)})
	if _, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: publicSessionID}); err != nil {
		t.Fatalf("Create with blank journal record = %v", err)
	}
}

func TestStorePublicResumeRepairsInvalidSequence(t *testing.T) {
	dir := t.TempDir()
	writePendingCreate(t, dir, publicSessionID, false, nil)
	sequence := filepath.Join(dir, "apps", "app", "shared_sequence")
	if err := os.WriteFile(sequence, []byte("invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var warnings warning.SliceSink
	store := openStore(t, sessionstore.Options{Dir: dir, Fsync: boolPointer(false), WarningSink: &warnings})
	created, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: publicSessionID})
	if err != nil || created.Session.ID() != publicSessionID {
		t.Fatalf("Create = %#v, %v", created, err)
	}
	if !warningCodePresent(warnings.Warnings(), warning.WarnSessionSnapshotRefresh) {
		t.Fatalf("warnings = %#v", warnings.Warnings())
	}
}

func TestStorePublicAuthorityPathFailures(t *testing.T) {
	for _, relative := range []string{
		filepath.Join("apps", "app", "app_state.jsonl"),
		filepath.Join("apps", "app", "users", "user", "user_state.jsonl"),
	} {
		t.Run(relative, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, relative)
			if err := os.MkdirAll(path, 0o700); err != nil {
				t.Fatal(err)
			}
			store := openStore(t, sessionstore.Options{Dir: dir, Fsync: boolPointer(false)})
			if _, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: publicSessionID}); err == nil {
				t.Fatal("Create with directory journal succeeded")
			}
		})
	}
}

func TestStorePublicInventoryPathFailures(t *testing.T) {
	if runtime.GOOS == publicWindowsOS {
		t.Skip("symlink behavior differs on Windows")
	}
	for _, suffix := range []string{".jsonl", ".jsonl.create-pending"} {
		t.Run(suffix, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "apps", "app", "users", "user", "sessions", "broken"+suffix)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(dir, "outside"), path); err != nil {
				t.Fatal(err)
			}
			store := openStore(t, sessionstore.Options{Dir: dir, Fsync: boolPointer(false)})
			if _, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: publicSessionID}); err == nil {
				t.Fatal("Create with unreadable inventory entry succeeded")
			}
		})
	}
}

func TestStorePublicGeneratedCreateIgnoresExplicitPendingMarker(t *testing.T) {
	const generatedSessionID = "generated"
	dir := t.TempDir()
	writePendingCreate(t, dir, "explicit", false, nil)
	store := openStore(t, sessionstore.Options{Dir: dir, Fsync: boolPointer(false), NewID: func() string { return generatedSessionID }})
	created, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user"})
	if err != nil || created.Session.ID() != generatedSessionID {
		t.Fatalf("Create = %#v, %v", created, err)
	}
}

func TestStorePublicSequenceRecovery(t *testing.T) {
	for _, test := range []sequenceScenario{
		{name: "invalid sequence", sequence: "invalid\n", wantCode: warning.WarnSessionSnapshotRefresh},
		{name: "exhausted sequence", sequence: "18446744073709551615\n", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			testSequenceValue(t, test)
		})
	}
	t.Run("sequence path is a directory", testSequencePathDirectory)
}

type sequenceScenario struct {
	name      string
	sequence  string
	wantError bool
	wantCode  string
}

func testSequenceValue(t *testing.T, test sequenceScenario) {
	dir := populatedStoreDirectory(t)
	sequence := filepath.Join(dir, "apps", "app", "shared_sequence")
	if err := os.WriteFile(sequence, []byte(test.sequence), 0o600); err != nil {
		t.Fatal(err)
	}
	var warnings warning.SliceSink
	store := openStore(t, sessionstore.Options{Dir: dir, Fsync: boolPointer(false), WarningSink: &warnings})
	created, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "second"})
	if (err != nil) != test.wantError {
		t.Fatalf("Create = %#v, %v", created, err)
	}
	if test.wantCode != "" && !warningCodePresent(warnings.Warnings(), test.wantCode) {
		t.Fatalf("warnings = %#v, want %q", warnings.Warnings(), test.wantCode)
	}
}

func testSequencePathDirectory(t *testing.T) {
	dir := populatedStoreDirectory(t)
	sequence := filepath.Join(dir, "apps", "app", "shared_sequence")
	if err := os.Remove(sequence); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(sequence, 0o700); err != nil {
		t.Fatal(err)
	}
	store := openStore(t, sessionstore.Options{Dir: dir, Fsync: boolPointer(false)})
	if _, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "second"}); err == nil {
		t.Fatal("Create with sequence directory succeeded")
	}
}

func TestStorePublicLogicalRecordInventoryValidation(t *testing.T) {
	t.Run("retains app-only logical order", func(t *testing.T) { testRetainedLogicalOrder(t, false) })
	t.Run("retains user-only logical order", func(t *testing.T) { testRetainedLogicalOrder(t, true) })
	for _, test := range []logicalRecordScenario{
		{
			name:  "same id claims different orders",
			app:   `{"v":1,"id":"same","order":40,"delta":{}}` + "\n",
			users: map[string]string{"user": `{"v":1,"id":"same","order":41,"delta":{}}` + "\n"},
		},
		{
			name:  "same order claims different ids",
			app:   `{"v":1,"id":"app-id","order":40,"delta":{}}` + "\n",
			users: map[string]string{"user": `{"v":1,"id":"user-id","order":40,"delta":{}}` + "\n"},
		},
		{
			name: "same user record appears under another user",
			users: map[string]string{
				"user":  `{"v":1,"id":"shared","order":40,"delta":{"key":"value"}}` + "\n",
				"other": `{"v":1,"id":"shared","order":40,"delta":{"key":"value"}}` + "\n",
			},
		},
		{
			name: "marker contradicts app journal",
			app: func() string {
				id := logicalRecordID("create", "user", "pending", 1, "")
				return fmt.Sprintf("{\"v\":1,\"id\":%q,\"order\":1,\"delta\":{\"key\":\"journal\"}}\n", id)
			}(),
			users: map[string]string{"user": func() string {
				id := logicalRecordID("create", "user", "pending", 1, "")
				return fmt.Sprintf("{\"v\":1,\"id\":%q,\"order\":1,\"delta\":{}}\n", id)
			}()},
			addMarker: func(t *testing.T, dir string) {
				writePendingCreateWithDeltas(t, dir, "pending", map[string]any{"key": "marker"}, nil)
			},
		},
		{
			name: "marker contradicts user journal",
			app: func() string {
				id := logicalRecordID("create", "user", "pending", 1, "")
				return fmt.Sprintf("{\"v\":1,\"id\":%q,\"order\":1,\"delta\":{}}\n", id)
			}(),
			users: map[string]string{"user": func() string {
				id := logicalRecordID("create", "user", "pending", 1, "")
				return fmt.Sprintf("{\"v\":1,\"id\":%q,\"order\":1,\"delta\":{\"key\":\"journal\"}}\n", id)
			}()},
			addMarker: func(t *testing.T, dir string) {
				writePendingCreateWithDeltas(t, dir, "pending", nil, map[string]any{"key": "marker"})
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			testLogicalRecordConflict(t, test)
		})
	}
}

type logicalRecordScenario struct {
	name      string
	app       string
	users     map[string]string
	addMarker func(*testing.T, string)
}

func testRetainedLogicalOrder(t *testing.T, user bool) {
	dir := t.TempDir()
	if user {
		writeJournal(t, dir, "app", "user", `{"v":1,"id":"user-only","order":40,"delta":{"key":"value"}}`+"\n")
	} else {
		writeJournal(t, dir, "app", "", `{"v":1,"id":"app-only","order":40,"delta":{"key":"value"}}`+"\n")
	}
	store := openStore(t, sessionstore.Options{Dir: dir, Fsync: boolPointer(false)})
	created, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: publicSessionID})
	if err != nil || created.Session.ID() != publicSessionID {
		t.Fatalf("Create = %#v, %v", created, err)
	}
}

func testLogicalRecordConflict(t *testing.T, test logicalRecordScenario) {
	dir := t.TempDir()
	if test.app != "" {
		writeJournal(t, dir, "app", "", test.app)
	}
	for user, data := range test.users {
		writeJournal(t, dir, "app", user, data)
	}
	if test.addMarker != nil {
		test.addMarker(t, dir)
	}
	store := openStore(t, sessionstore.Options{Dir: dir, Fsync: boolPointer(false)})
	if _, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: publicSessionID}); !errors.Is(err, sessionstore.ErrCorruptLog) {
		t.Fatalf("Create error = %v, want ErrCorruptLog", err)
	}
}

func TestStorePublicCreateFailureRecovery(t *testing.T) {
	t.Run("delete marker blocks create", testDeleteMarkerBlocksCreate)
	t.Run("dangling transcript rolls back marker", testDanglingTranscriptRollback)
	t.Run("unwritable session parent releases reservation", testUnwritableSessionParent)
	t.Run("resume rejects transcript with different local state", testResumeRejectsDifferentLocalState)
	for _, operation := range []string{"create", "delete"} {
		t.Run(operation+" rejects corrupt journal", func(t *testing.T) {
			testOperationRejectsCorruptJournal(t, operation)
		})
	}
}

func testDeleteMarkerBlocksCreate(t *testing.T) {
	dir := t.TempDir()
	path := transcriptPath(dir, "app", "user", publicSessionID) + ".delete-pending"
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := openStore(t, sessionstore.Options{Dir: dir, Fsync: boolPointer(false)})
	if _, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: publicSessionID}); !errors.Is(err, sessionstore.ErrSessionExists) {
		t.Fatalf("Create error = %v, want ErrSessionExists", err)
	}
}

func testDanglingTranscriptRollback(t *testing.T) {
	if runtime.GOOS == publicWindowsOS {
		t.Skip("symlink semantics differ on Windows")
	}
	dir := t.TempDir()
	path := transcriptPath(dir, "app", "user", publicSessionID)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing-target", path); err != nil {
		t.Fatal(err)
	}
	store := openStore(t, sessionstore.Options{Dir: dir, Fsync: boolPointer(false)})
	if _, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: publicSessionID}); err == nil {
		t.Fatal("Create over dangling transcript succeeded")
	}
	if _, err := os.Stat(path + ".create-pending"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("create marker remains after rollback: %v", err)
	}
}

func testUnwritableSessionParent(t *testing.T) {
	if runtime.GOOS == publicWindowsOS {
		t.Skip("Windows does not enforce POSIX owner mode bits")
	}
	dir := populatedStoreDirectory(t)
	oldLog := transcriptPath(dir, "app", "user", publicSessionID)
	if err := os.Remove(oldLog); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Dir(oldLog)); err != nil {
		t.Fatal(err)
	}
	userDir := filepath.Dir(filepath.Dir(oldLog))
	chmodForTest(t, userDir, 0o500)
	store := openStore(t, sessionstore.Options{Dir: dir, Fsync: boolPointer(false)})
	if _, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "new"}); err == nil {
		t.Fatal("Create with unwritable session parent succeeded")
	}
}

func testResumeRejectsDifferentLocalState(t *testing.T) {
	dir := t.TempDir()
	store := openStore(t, sessionstore.Options{Dir: dir, Fsync: boolPointer(false)})
	createSession(t, store, "app", "user", publicSessionID, map[string]any{"local": "transcript"})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	writePendingCreate(t, dir, publicSessionID, false, map[string]any{"local": "marker"})
	reopened := openStore(t, sessionstore.Options{Dir: dir, Fsync: boolPointer(false)})
	if _, err := reopened.Create(t.Context(), &session.CreateRequest{
		AppName: "app", UserID: "user", SessionID: publicSessionID, State: map[string]any{"local": "marker"},
	}); !errors.Is(err, sessionstore.ErrCorruptLog) {
		t.Fatalf("Create error = %v, want ErrCorruptLog", err)
	}
}

func testOperationRejectsCorruptJournal(t *testing.T, operation string) {
	dir := populatedStoreDirectory(t)
	journal := filepath.Join(dir, "apps", "app", "app_state.jsonl")
	if err := os.WriteFile(journal, []byte("{bad}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := openStore(t, sessionstore.Options{Dir: dir, Fsync: boolPointer(false)})
	var err error
	if operation == "create" {
		_, err = store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "second"})
	} else {
		err = store.Delete(t.Context(), &session.DeleteRequest{AppName: "app", UserID: "user", SessionID: publicSessionID})
	}
	if !errors.Is(err, sessionstore.ErrCorruptLog) {
		t.Fatalf("%s error = %v, want ErrCorruptLog", operation, err)
	}
}

func TestClosedStoreRejectsPublicOperations(t *testing.T) {
	store := openStore(t, sessionstore.Options{Dir: t.TempDir()})
	created := createSession(t, store, "app", "user", publicSessionID, nil)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}

	checks := []struct {
		name string
		run  func() error
	}{
		{"create", func() error {
			_, err := store.Create(t.Context(), &session.CreateRequest{AppName: "app", UserID: "user"})
			return err
		}},
		{"get", func() error {
			_, err := store.Get(t.Context(), &session.GetRequest{AppName: "app", UserID: "user", SessionID: publicSessionID})
			return err
		}},
		{"list", func() error { _, err := store.List(t.Context(), &session.ListRequest{AppName: "app"}); return err }},
		{"delete", func() error {
			return store.Delete(t.Context(), &session.DeleteRequest{AppName: "app", UserID: "user", SessionID: publicSessionID})
		}},
		{"append event", func() error { return store.AppendEvent(t.Context(), created, &session.Event{ID: "event"}) }},
		{"append sidecar", func() error { return store.AppendSidecar(t.Context(), "app", "user", publicSessionID, "kind", true) }},
		{"load sidecar", func() error {
			_, err := store.LoadSidecar(t.Context(), "app", "user", publicSessionID, "kind", new(bool))
			return err
		}},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			if err := check.run(); !errors.Is(err, sessionstore.ErrClosed) {
				t.Fatalf("error = %v, want ErrClosed", err)
			}
		})
	}
}

func openStore(t *testing.T, options sessionstore.Options) *sessionstore.Store {
	t.Helper()
	store, err := sessionstore.OpenWith(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func createSession(t *testing.T, store *sessionstore.Store, app, user, id string, state map[string]any) session.Session {
	t.Helper()
	created, err := store.Create(t.Context(), &session.CreateRequest{AppName: app, UserID: user, SessionID: id, State: state})
	if err != nil {
		t.Fatal(err)
	}
	return created.Session
}

func transcriptPath(dir, app, user, id string) string {
	return filepath.Join(dir, "apps", app, "users", user, "sessions", id+".jsonl")
}

func warningCodePresent(warnings []warning.Warning, code string) bool {
	return slices.ContainsFunc(warnings, func(value warning.Warning) bool { return value.Code == code })
}

func boolPointer(value bool) *bool { return &value }

func writePendingCreate(t *testing.T, dir, id string, generated bool, state map[string]any) {
	t.Helper()
	stateData, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	marker := map[string]any{
		"v":         1,
		"generated": generated,
		"stateHash": fmt.Sprintf("%x", sha256.Sum256(stateData)),
		"header": map[string]any{
			"id": id, "appName": "app", "userId": "user", "state": state,
			"createdAt": time.Date(2026, 8, 18, 2, 0, 0, 0, time.UTC), "incarnation": 1,
		},
	}
	data, err := json.Marshal(marker)
	if err != nil {
		t.Fatal(err)
	}
	path := transcriptPath(dir, "app", "user", id) + ".create-pending"
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func populatedStoreDirectory(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	store, err := sessionstore.OpenWith(sessionstore.Options{Dir: dir, Fsync: boolPointer(false)})
	if err != nil {
		t.Fatal(err)
	}
	createSession(t, store, "app", "user", publicSessionID, map[string]any{"app:key": "value", "user:key": "value"})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

func chmodForTest(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	restore := os.FileMode(0o600)
	if info.IsDir() {
		restore = 0o700
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, restore) })
}

func writeJournal(t *testing.T, dir, app, user, data string) {
	t.Helper()
	path := filepath.Join(dir, "apps", app, "app_state.jsonl")
	if user != "" {
		path = filepath.Join(dir, "apps", app, "users", user, "user_state.jsonl")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writePendingCreateWithDeltas(t *testing.T, dir, id string, appDelta, userDelta map[string]any) {
	t.Helper()
	nilState, err := json.Marshal(map[string]any(nil))
	if err != nil {
		t.Fatal(err)
	}
	marker := map[string]any{
		"v": 1, "generated": false, "stateHash": fmt.Sprintf("%x", sha256.Sum256(nilState)),
		"header": map[string]any{
			"id": id, "appName": "app", "userId": "user", "appDelta": appDelta, "userDelta": userDelta,
			"createdAt": time.Date(2026, 8, 18, 2, 0, 0, 0, time.UTC), "incarnation": 1,
		},
	}
	data, err := json.Marshal(marker)
	if err != nil {
		t.Fatal(err)
	}
	path := transcriptPath(dir, "app", "user", id) + ".create-pending"
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func logicalRecordID(kind, userID, sessionID string, incarnation uint64, eventID string) string {
	digest := sha256.New()
	for _, value := range []string{kind, userID, sessionID, strconv.FormatUint(incarnation, 10), eventID} {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		_, _ = digest.Write(size[:])
		_, _ = digest.Write([]byte(value))
	}
	return kind + ":" + fmt.Sprintf("%x", digest.Sum(nil))
}
