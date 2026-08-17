package loop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type memorySessionStore struct {
	mu       sync.Mutex
	ref      SessionRef
	events   [][]byte
	raw      []json.RawMessage
	sidecars map[string][]byte
	closed   bool
}

func (s *memorySessionStore) Create(_ context.Context, request CreateSessionRequest) (SessionRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sessionID := request.SessionID
	if sessionID == "" {
		sessionID = "session"
	}
	s.ref = SessionRef{ID: sessionID, AppName: request.AppName, UserID: request.UserID, State: request.State, LastUpdate: time.Unix(1, 0).UTC()}
	return s.ref, nil
}

func (s *memorySessionStore) Get(context.Context, string, string, string) (SessionRef, []Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	events := make([]Event, len(s.events))
	for index, encoded := range s.events {
		if err := json.Unmarshal(encoded, &events[index]); err != nil {
			return SessionRef{}, nil, err
		}
		if s.raw[index] != nil {
			events[index].Raw = append(json.RawMessage(nil), s.raw[index]...)
		}
	}
	return s.ref, events, nil
}

func (s *memorySessionStore) List(context.Context, string, string) ([]SessionRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ref.ID == "" {
		return nil, nil
	}
	return []SessionRef{s.ref}, nil
}

func (s *memorySessionStore) Delete(context.Context, string, string, string) error {
	s.mu.Lock()
	s.ref = SessionRef{}
	s.events = nil
	s.raw = nil
	s.mu.Unlock()
	return nil
}

func (s *memorySessionStore) Append(_ context.Context, _ SessionRef, event Event) error {
	var raw json.RawMessage
	if event.Raw != nil {
		raw = event.Raw
		raw = append(json.RawMessage(nil), raw...)
		event.Raw = nil
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.events = append(s.events, encoded)
	s.raw = append(s.raw, raw)
	s.mu.Unlock()
	return nil
}

func (s *memorySessionStore) AppendSidecar(_ context.Context, _, _, _, kind string, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	s.mu.Lock()
	if s.sidecars == nil {
		s.sidecars = make(map[string][]byte)
	}
	s.sidecars[kind] = append([]byte(nil), encoded...)
	s.mu.Unlock()
	return nil
}

func (s *memorySessionStore) LoadSidecar(_ context.Context, _, _, _, kind string, destination any) (bool, error) {
	s.mu.Lock()
	encoded, ok := s.sidecars[kind]
	encoded = append([]byte(nil), encoded...)
	s.mu.Unlock()
	if !ok {
		return false, nil
	}
	return true, json.Unmarshal(encoded, destination)
}

func (s *memorySessionStore) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return nil
}

type conformanceProvider struct {
	configured atomic.Int32
	active     atomic.Int32
	maximum    atomic.Int32
}

func (*conformanceProvider) Name() string { return "conformance" }

func (p *conformanceProvider) Configure(context.Context, RunnerConfig) error {
	if p.configured.Add(1) != 1 {
		return errors.New("configured more than once")
	}
	return nil
}

func (p *conformanceProvider) Run(ctx context.Context, request RunRequest) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		active := p.active.Add(1)
		defer p.active.Add(-1)
		for {
			maximum := p.maximum.Load()
			if active <= maximum || p.maximum.CompareAndSwap(maximum, active) {
				break
			}
		}
		select {
		case <-ctx.Done():
			yield(Event{}, ctx.Err())
		case <-time.After(time.Millisecond):
			yield(Event{Kind: EventTurnComplete, SessionID: request.SessionID}, nil)
		}
	}
}

func (*conformanceProvider) Close() error { return nil }

var _ SessionStore = (*memorySessionStore)(nil)
var _ Provider = (*conformanceProvider)(nil)

func TestSessionStoreContractRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := &memorySessionStore{}
	ref, err := store.Create(ctx, CreateSessionRequest{AppName: "app", UserID: "user", State: map[string]any{"count": float64(1)}})
	if err != nil {
		t.Fatal(err)
	}
	raw := json.RawMessage("{\n  \"provider\": \"raw\"\n}")
	event := Event{Kind: EventText, Text: "hello", Raw: raw, Err: errors.New("runtime only")}
	if err := store.Append(ctx, ref, event); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSidecar(ctx, "app", "user", ref.ID, "compaction", map[string]any{"scale": 1}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSidecar(ctx, "app", "user", ref.ID, "compaction", map[string]any{"scale": 2}); err != nil {
		t.Fatal(err)
	}
	gotRef, events, err := store.Get(ctx, "app", "user", ref.ID)
	if err != nil {
		t.Fatal(err)
	}
	gotRaw := events[0].Raw
	if gotRef.ID != ref.ID || len(events) != 1 || events[0].Err != nil || !bytes.Equal(gotRaw, raw) {
		t.Fatalf("ref = %#v, events = %#v", gotRef, events)
	}
	var sidecar map[string]any
	found, err := store.LoadSidecar(ctx, "app", "user", ref.ID, "compaction", &sidecar)
	if err != nil || !found || sidecar["scale"] != float64(2) {
		t.Fatalf("found = %v, sidecar = %#v, err = %v", found, sidecar, err)
	}
	found, err = store.LoadSidecar(ctx, "app", "user", ref.ID, "missing", &sidecar)
	if err != nil || found {
		t.Fatalf("missing sidecar found = %v, err = %v", found, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestProviderContractConcurrentDistinctSessions(t *testing.T) {
	t.Parallel()
	provider := &conformanceProvider{}
	if err := provider.Configure(context.Background(), RunnerConfig{}); err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for index := range 8 {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			for event, err := range provider.Run(context.Background(), RunRequest{SessionID: fmt.Sprintf("session-%d", index)}) {
				if err != nil {
					t.Errorf("run: %v", err)
				}
				if event.SessionID == "" {
					t.Error("run lost session identity")
				}
			}
		}(index)
	}
	group.Wait()
	if got := provider.configured.Load(); got != 1 {
		t.Fatalf("Configure calls = %d", got)
	}
	if got := provider.maximum.Load(); got < 2 {
		t.Fatalf("maximum concurrent runs = %d", got)
	}
}
