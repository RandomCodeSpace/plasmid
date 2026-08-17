package adkloop

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"maps"
	"strings"
	"sync"
	"time"

	"google.golang.org/genai"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"

	"github.com/plasmid-dev/plasmid/loop"
)

type sessionService struct {
	store loop.SessionStore
}

// NewSessionService adapts a framework-free session store to ADK.
func NewSessionService(store loop.SessionStore) session.Service {
	return &sessionService{store: store}
}

func (s *sessionService) Create(ctx context.Context, request *session.CreateRequest) (*session.CreateResponse, error) {
	if request == nil {
		return nil, fmt.Errorf("create session: request is nil")
	}
	if nilInterface(s.store) {
		return nil, fmt.Errorf("create session: store is nil")
	}
	ref, err := s.store.Create(ctx, loop.CreateSessionRequest{
		AppName:   request.AppName,
		UserID:    request.UserID,
		SessionID: request.SessionID,
		State:     maps.Clone(request.State),
	})
	if err != nil {
		return nil, err
	}
	if ref.ID == "" || ref.AppName != request.AppName || ref.UserID != request.UserID || request.SessionID != "" && ref.ID != request.SessionID {
		return nil, fmt.Errorf("%w: store returned session identity %q/%q/%q for %q/%q/%q", ErrFidelity, ref.AppName, ref.UserID, ref.ID, request.AppName, request.UserID, request.SessionID)
	}
	return &session.CreateResponse{Session: newADKSession(ref, nil)}, nil
}

func (s *sessionService) Get(ctx context.Context, request *session.GetRequest) (*session.GetResponse, error) {
	if request == nil {
		return nil, fmt.Errorf("get session: request is nil")
	}
	if nilInterface(s.store) {
		return nil, fmt.Errorf("get session: store is nil")
	}
	ref, storedEvents, err := s.store.Get(ctx, request.AppName, request.UserID, request.SessionID)
	if err != nil {
		return nil, err
	}
	if ref.ID != request.SessionID || ref.AppName != request.AppName || ref.UserID != request.UserID {
		return nil, fmt.Errorf("%w: store returned session identity %q/%q/%q for %q/%q/%q", ErrFidelity, ref.AppName, ref.UserID, ref.ID, request.AppName, request.UserID, request.SessionID)
	}
	nativeEvents := make([]*session.Event, 0, len(storedEvents))
	for index, stored := range storedEvents {
		if stored.SessionID != ref.ID {
			return nil, fmt.Errorf("restore event %d: %w: event session %q does not match session %q", index, ErrFidelity, stored.SessionID, ref.ID)
		}
		native, err := loopEventToADK(stored)
		if err != nil {
			return nil, fmt.Errorf("restore event %d: %w", index, err)
		}
		nativeEvents = append(nativeEvents, native)
	}
	if request.NumRecentEvents > 0 {
		start := max(len(nativeEvents)-request.NumRecentEvents, 0)
		nativeEvents = nativeEvents[start:]
	}
	if !request.After.IsZero() {
		filtered := make([]*session.Event, 0, len(nativeEvents))
		for _, event := range nativeEvents {
			if !event.Timestamp.Before(request.After) {
				filtered = append(filtered, event)
			}
		}
		nativeEvents = filtered
	}
	return &session.GetResponse{Session: newADKSession(ref, nativeEvents)}, nil
}

func (s *sessionService) List(ctx context.Context, request *session.ListRequest) (*session.ListResponse, error) {
	if request == nil {
		return nil, fmt.Errorf("list sessions: request is nil")
	}
	if nilInterface(s.store) {
		return nil, fmt.Errorf("list sessions: store is nil")
	}
	refs, err := s.store.List(ctx, request.AppName, request.UserID)
	if err != nil {
		return nil, err
	}
	sessions := make([]session.Session, len(refs))
	for index, ref := range refs {
		if ref.ID == "" || ref.AppName != request.AppName || ref.UserID == "" || request.UserID != "" && ref.UserID != request.UserID {
			return nil, fmt.Errorf("%w: store returned list session identity %q/%q/%q for %q/%q", ErrFidelity, ref.AppName, ref.UserID, ref.ID, request.AppName, request.UserID)
		}
		sessions[index] = newADKSession(ref, nil)
	}
	return &session.ListResponse{Sessions: sessions}, nil
}

func (s *sessionService) Delete(ctx context.Context, request *session.DeleteRequest) error {
	if request == nil {
		return fmt.Errorf("delete session: request is nil")
	}
	if nilInterface(s.store) {
		return fmt.Errorf("delete session: store is nil")
	}
	return s.store.Delete(ctx, request.AppName, request.UserID, request.SessionID)
}

func (s *sessionService) AppendEvent(ctx context.Context, current session.Session, event *session.Event) error {
	if current == nil {
		return fmt.Errorf("append event: session is nil")
	}
	if event == nil {
		return fmt.Errorf("append event: event is nil")
	}
	local, ok := current.(*adkSession)
	if !ok || local == nil {
		return fmt.Errorf("%w: append event received %T", ErrForeignSession, current)
	}
	if event.Partial {
		return nil
	}
	if nilInterface(s.store) {
		return fmt.Errorf("append event: store is nil")
	}
	persistedEvent := trimTemporaryStateDelta(event)
	stored, err := adkEventToStored(local.ID(), persistedEvent)
	if err != nil {
		return err
	}
	localEvent, err := loopEventToADK(stored)
	if err != nil {
		return fmt.Errorf("clone appended event: %w", err)
	}
	return local.appendPersisted(ctx, s.store, stored, localEvent)
}

func trimTemporaryStateDelta(event *session.Event) *session.Event {
	if len(event.Actions.StateDelta) == 0 {
		return event
	}
	filtered := make(map[string]any, len(event.Actions.StateDelta))
	for key, value := range event.Actions.StateDelta {
		if !strings.HasPrefix(key, session.KeyPrefixTemp) {
			filtered[key] = value
		}
	}
	if len(filtered) == len(event.Actions.StateDelta) {
		return event
	}
	clone := *event
	clone.Actions.StateDelta = filtered
	return &clone
}

func adkEventToStored(sessionID string, event *session.Event) (loop.Event, error) {
	if event == nil || event.ID == "" {
		return loop.Event{}, fmt.Errorf("%w: incoming event has no id", ErrFidelity)
	}
	raw, err := json.Marshal(event)
	if err != nil {
		return loop.Event{}, fmt.Errorf("marshal ADK event: %w", err)
	}
	projected, err := toLoopEvents(sessionID, event)
	if err != nil {
		return loop.Event{}, err
	}
	stored := loop.Event{
		ID:             event.ID,
		Kind:           loop.EventNotice,
		SessionID:      sessionID,
		InvocationID:   event.InvocationID,
		Author:         event.Author,
		Branch:         event.Branch,
		IsolationScope: event.IsolationScope,
		StateDelta:     maps.Clone(event.Actions.StateDelta),
		Timestamp:      event.Timestamp,
		Final:          event.IsFinalResponse(),
		Usage:          toLoopUsage(event.UsageMetadata),
	}
	if len(projected) == 1 {
		stored = projected[0]
	}
	stored.Raw = append(json.RawMessage(nil), raw...)
	return stored, nil
}

func loopEventToADK(stored loop.Event) (*session.Event, error) {
	if stored.ID == "" {
		return nil, fmt.Errorf("%w: incoming event has no id", ErrFidelity)
	}
	if stored.SessionID == "" {
		return nil, fmt.Errorf("%w: incoming event has no session id", ErrFidelity)
	}
	if stored.Raw != nil {
		raw := append([]byte(nil), stored.Raw...)
		if !json.Valid(raw) {
			return nil, fmt.Errorf("%w: event Raw is not valid JSON", ErrFidelity)
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil || object == nil {
			return nil, fmt.Errorf("%w: event Raw is not a non-null ADK event object", ErrFidelity)
		}
		var event session.Event
		if err := json.Unmarshal(raw, &event); err != nil {
			return nil, fmt.Errorf("decode event Raw: %w", err)
		}
		if err := validateRawEventEnvelope(stored, event, object); err != nil {
			return nil, err
		}
		return &event, nil
	}

	event := &session.Event{
		ID:             stored.ID,
		InvocationID:   stored.InvocationID,
		Branch:         stored.Branch,
		IsolationScope: stored.IsolationScope,
		Timestamp:      stored.Timestamp,
		Author:         stored.Author,
		Actions:        session.EventActions{StateDelta: maps.Clone(stored.StateDelta)},
		LLMResponse: model.LLMResponse{
			UsageMetadata: toADKUsage(stored.Usage),
		},
	}
	switch stored.Kind {
	case loop.EventText, loop.EventTextDelta:
		var role genai.Role = genai.RoleModel
		if stored.Author == "user" {
			role = genai.RoleUser
		}
		event.Content = genai.NewContentFromText(stored.Text, role)
		event.Partial = stored.Kind == loop.EventTextDelta
	case loop.EventToolCall:
		if stored.Tool == nil || stored.Tool.ID == "" || stored.Tool.Name == "" {
			return nil, fmt.Errorf("%w: malformed portable tool call", ErrFidelity)
		}
		event.LLMResponse.Content = &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
			ID: stored.Tool.ID, Name: stored.Tool.Name, Args: cloneMap(stored.Tool.Args),
		}}}}
	case loop.EventToolResult:
		if stored.Tool == nil || stored.Tool.CallID == "" || stored.Tool.Name == "" || stored.Tool.Content == nil {
			return nil, fmt.Errorf("%w: malformed portable tool result", ErrFidelity)
		}
		if stored.Tool.IsError {
			if _, ok := stored.Tool.Content["error"]; !ok {
				return nil, fmt.Errorf("%w: portable error result has no error field", ErrFidelity)
			}
		}
		event.LLMResponse.Content = &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{
			ID: stored.Tool.CallID, Name: stored.Tool.Name, Response: cloneMap(stored.Tool.Content),
		}}}}
	case loop.EventError:
		event.ErrorMessage = stored.Text
		if event.ErrorMessage == "" && stored.Err != nil {
			event.ErrorMessage = stored.Err.Error()
		}
	case loop.EventTurnComplete:
		event.TurnComplete = true
	case loop.EventNotice:
		event.CustomMetadata = map[string]any{portableNoticeMetadataKey: stored.Text}
	case loop.EventWarning:
		if stored.Warning == nil {
			return nil, fmt.Errorf("%w: portable warning has no warning value", ErrFidelity)
		}
		event.CustomMetadata = map[string]any{portableWarningMetadataKey: *stored.Warning}
	default:
		return nil, fmt.Errorf("%w: event kind %q has no portable ADK reconstruction", ErrFidelity, stored.Kind)
	}
	if stored.Final && !event.IsFinalResponse() {
		event.Actions.SkipSummarization = true
	}
	return event, nil
}

func validateRawEventEnvelope(stored loop.Event, event session.Event, object map[string]json.RawMessage) error {
	if event.ID != stored.ID || event.InvocationID != stored.InvocationID || event.Branch != stored.Branch || event.IsolationScope != stored.IsolationScope {
		return fmt.Errorf("%w: ADK event identity contradicts portable envelope", ErrFidelity)
	}
	if rawSessionID, ok := object["sessionId"]; ok {
		var sessionID string
		if err := json.Unmarshal(rawSessionID, &sessionID); err != nil || sessionID != stored.SessionID {
			return fmt.Errorf("%w: ADK event session contradicts portable envelope", ErrFidelity)
		}
	}
	return nil
}

const (
	portableNoticeMetadataKey  = "loop.notice"
	portableWarningMetadataKey = "loop.warning"
)

type adkSession struct {
	mu      sync.RWMutex
	ref     loop.SessionRef
	state   map[string]any
	events  []*session.Event
	updated time.Time
}

func newADKSession(ref loop.SessionRef, events []*session.Event) *adkSession {
	state := maps.Clone(ref.State)
	if state == nil {
		state = make(map[string]any)
	}
	return &adkSession{ref: ref, state: state, events: append([]*session.Event(nil), events...), updated: ref.LastUpdate}
}

func (s *adkSession) ID() string      { return s.ref.ID }
func (s *adkSession) AppName() string { return s.ref.AppName }
func (s *adkSession) UserID() string  { return s.ref.UserID }

func (s *adkSession) State() session.State { return &adkState{session: s} }

func (s *adkSession) Events() session.Events {
	s.mu.RLock()
	events := append(eventList(nil), s.events...)
	s.mu.RUnlock()
	return events
}

func (s *adkSession) LastUpdateTime() time.Time {
	s.mu.RLock()
	updated := s.updated
	s.mu.RUnlock()
	return updated
}

func (s *adkSession) append(event *session.Event) {
	s.mu.Lock()
	s.appendLocked(event)
	s.mu.Unlock()
}

func (s *adkSession) appendPersisted(ctx context.Context, store loop.SessionStore, stored loop.Event, event *session.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ref := loop.SessionRef{
		ID:         s.ref.ID,
		AppName:    s.ref.AppName,
		UserID:     s.ref.UserID,
		State:      maps.Clone(s.state),
		LastUpdate: s.updated,
	}
	if err := store.Append(ctx, ref, stored); err != nil {
		return err
	}
	s.appendLocked(event)
	return nil
}

func (s *adkSession) appendLocked(event *session.Event) {
	s.events = append(s.events, event)
	for key, value := range event.Actions.StateDelta {
		if strings.HasPrefix(key, session.KeyPrefixTemp) {
			continue
		}
		s.state[key] = value
	}
	s.updated = event.Timestamp
}

type adkState struct{ session *adkSession }

func (s *adkState) Get(key string) (any, error) {
	s.session.mu.RLock()
	value, ok := s.session.state[key]
	s.session.mu.RUnlock()
	if !ok {
		return nil, session.ErrStateKeyNotExist
	}
	return value, nil
}

func (s *adkState) Set(key string, value any) error {
	s.session.mu.Lock()
	s.session.state[key] = value
	s.session.mu.Unlock()
	return nil
}

func (s *adkState) All() iter.Seq2[string, any] {
	s.session.mu.RLock()
	state := maps.Clone(s.session.state)
	s.session.mu.RUnlock()
	return func(yield func(string, any) bool) {
		for key, value := range state {
			if !yield(key, value) {
				return
			}
		}
	}
}

type eventList []*session.Event

func (events eventList) All() iter.Seq[*session.Event] {
	return func(yield func(*session.Event) bool) {
		for _, event := range events {
			if !yield(event) {
				return
			}
		}
	}
}

func (events eventList) Len() int { return len(events) }

func (events eventList) At(index int) *session.Event {
	if index < 0 || index >= len(events) {
		return nil
	}
	return events[index]
}

func stateMap(state session.State) map[string]any {
	if state == nil {
		return nil
	}
	values := make(map[string]any)
	for key, value := range state.All() {
		values[key] = value
	}
	return values
}
