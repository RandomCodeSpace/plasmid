package adkloop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"maps"
	"sync"
	"time"

	"google.golang.org/genai"

	"google.golang.org/adk/v2/model"

	"github.com/plasmid-dev/plasmid/loop"
)

type testTool struct {
	name        string
	description string
	schema      json.RawMessage
	call        func(context.Context, loop.ToolCall) (loop.ToolResult, error)
}

func (t *testTool) Name() string                 { return t.name }
func (t *testTool) Description() string          { return t.description }
func (t *testTool) InputSchema() json.RawMessage { return t.schema }
func (t *testTool) Call(ctx context.Context, call loop.ToolCall) (loop.ToolResult, error) {
	if t.call == nil {
		return loop.ToolResult{}, nil
	}
	return t.call(ctx, call)
}

type scriptedModel struct {
	requests []*model.LLMRequest
	calls    int
}

func (m *scriptedModel) Name() string { return "scripted" }

func (m *scriptedModel) GenerateContent(
	_ context.Context,
	req *model.LLMRequest,
	_ bool,
) iter.Seq2[*model.LLMResponse, error] {
	m.requests = append(m.requests, req)
	index := m.calls
	m.calls++

	return func(yield func(*model.LLMResponse, error) bool) {
		switch index {
		case 0:
			yield(&model.LLMResponse{
				Content: &genai.Content{
					Role: genai.RoleModel,
					Parts: []*genai.Part{{
						FunctionCall: &genai.FunctionCall{
							ID:   "call-1",
							Name: "lookup",
							Args: map[string]any{"key": "alpha"},
						},
					}},
				},
			}, nil)
		case 1:
			yield(&model.LLMResponse{
				Content: &genai.Content{
					Role: genai.RoleModel,
					Parts: []*genai.Part{
						genai.NewPartFromText("lookup completed"),
					},
				},
			}, nil)
		default:
			yield(nil, fmt.Errorf("unexpected GenerateContent call %d", index))
		}
	}
}

type storedTestSession struct {
	ref    loop.SessionRef
	events []loop.Event
}

type recordingSessionStore struct {
	mu sync.Mutex

	sessions map[string]*storedTestSession
	created  []loop.CreateSessionRequest
	gets     [][3]string
	lists    [][2]string
	deletes  [][3]string
	appends  []struct {
		ref   loop.SessionRef
		event loop.Event
	}
	closed int

	createOverride *loop.SessionRef
	getOverride    *loop.SessionRef
	listOverride   []loop.SessionRef
	getEvents      []loop.Event
	appendErr      error
}

func newRecordingSessionStore() *recordingSessionStore {
	return &recordingSessionStore{sessions: make(map[string]*storedTestSession)}
}

func sessionKey(appName, userID, sessionID string) string {
	return appName + "\x00" + userID + "\x00" + sessionID
}

func (s *recordingSessionStore) Create(_ context.Context, request loop.CreateSessionRequest) (loop.SessionRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	request.State = maps.Clone(request.State)
	s.created = append(s.created, request)
	if s.createOverride != nil {
		return cloneSessionRef(*s.createOverride), nil
	}
	id := request.SessionID
	if id == "" {
		id = fmt.Sprintf("session-%d", len(s.sessions)+1)
	}
	ref := loop.SessionRef{
		ID:         id,
		AppName:    request.AppName,
		UserID:     request.UserID,
		State:      maps.Clone(request.State),
		LastUpdate: time.Unix(1, 0).UTC(),
	}
	s.sessions[sessionKey(ref.AppName, ref.UserID, ref.ID)] = &storedTestSession{ref: cloneSessionRef(ref)}
	return cloneSessionRef(ref), nil
}

func (s *recordingSessionStore) Get(_ context.Context, appName, userID, sessionID string) (loop.SessionRef, []loop.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets = append(s.gets, [3]string{appName, userID, sessionID})
	if s.getOverride != nil {
		return cloneSessionRef(*s.getOverride), cloneLoopEvents(s.getEvents), nil
	}
	stored, ok := s.sessions[sessionKey(appName, userID, sessionID)]
	if !ok {
		return loop.SessionRef{}, nil, errors.New("session not found")
	}
	return cloneSessionRef(stored.ref), cloneLoopEvents(stored.events), nil
}

func (s *recordingSessionStore) List(_ context.Context, appName, userID string) ([]loop.SessionRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lists = append(s.lists, [2]string{appName, userID})
	if s.listOverride != nil {
		return cloneSessionRefs(s.listOverride), nil
	}
	refs := make([]loop.SessionRef, 0)
	for _, stored := range s.sessions {
		if stored.ref.AppName == appName && (userID == "" || stored.ref.UserID == userID) {
			refs = append(refs, cloneSessionRef(stored.ref))
		}
	}
	return refs, nil
}

func (s *recordingSessionStore) Delete(_ context.Context, appName, userID, sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deletes = append(s.deletes, [3]string{appName, userID, sessionID})
	delete(s.sessions, sessionKey(appName, userID, sessionID))
	return nil
}

func (s *recordingSessionStore) Append(_ context.Context, ref loop.SessionRef, event loop.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.appendErr != nil {
		return s.appendErr
	}
	ref = cloneSessionRef(ref)
	event = cloneLoopEvent(event)
	s.appends = append(s.appends, struct {
		ref   loop.SessionRef
		event loop.Event
	}{ref: ref, event: event})
	stored, ok := s.sessions[sessionKey(ref.AppName, ref.UserID, ref.ID)]
	if ok {
		stored.events = append(stored.events, cloneLoopEvent(event))
		stored.ref.LastUpdate = event.Timestamp
	}
	return nil
}

func (*recordingSessionStore) AppendSidecar(context.Context, string, string, string, string, any) error {
	return nil
}

func (*recordingSessionStore) LoadSidecar(context.Context, string, string, string, string, any) (bool, error) {
	return false, nil
}

func (s *recordingSessionStore) Close() error {
	s.mu.Lock()
	s.closed++
	s.mu.Unlock()
	return nil
}

func cloneSessionRef(ref loop.SessionRef) loop.SessionRef {
	ref.State = maps.Clone(ref.State)
	return ref
}

func cloneSessionRefs(refs []loop.SessionRef) []loop.SessionRef {
	clones := make([]loop.SessionRef, len(refs))
	for index, ref := range refs {
		clones[index] = cloneSessionRef(ref)
	}
	return clones
}

func cloneLoopEvents(events []loop.Event) []loop.Event {
	clones := make([]loop.Event, len(events))
	for index, event := range events {
		clones[index] = cloneLoopEvent(event)
	}
	return clones
}

func cloneLoopEvent(event loop.Event) loop.Event {
	if event.Tool != nil {
		tool := *event.Tool
		tool.Args = maps.Clone(tool.Args)
		tool.Content = maps.Clone(tool.Content)
		event.Tool = &tool
	}
	if event.Usage != nil {
		usage := *event.Usage
		event.Usage = &usage
	}
	if event.Warning != nil {
		warning := *event.Warning
		event.Warning = &warning
	}
	event.Raw = append(json.RawMessage(nil), event.Raw...)
	return event
}

func collectProviderRun(sequence iter.Seq2[loop.Event, error]) ([]loop.Event, []error) {
	var events []loop.Event
	var errs []error
	for event, err := range sequence {
		if err != nil {
			errs = append(errs, err)
			continue
		}
		events = append(events, event)
	}
	return events, errs
}

var _ loop.Tool = (*testTool)(nil)
var _ loop.SessionStore = (*recordingSessionStore)(nil)
var _ model.LLM = (*scriptedModel)(nil)
