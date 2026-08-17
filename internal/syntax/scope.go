package syntax

import (
	"errors"
	"sync"
)

// ScopeKey uniquely identifies one invocation within a session.
type ScopeKey struct {
	SessionID    string
	InvocationID string
}

// TurnScope contains immutable-by-convention per-invocation syntax state.
type TurnScope struct {
	Policy        ToolPolicy
	Arguments     Arguments
	Variables     Variables
	DocumentPath  string
	DocumentTrust bool
}

// ScopeStore owns concurrent turn scopes. Its zero value is ready for use.
type ScopeStore struct {
	mu     sync.RWMutex
	scopes map[ScopeKey]TurnScope
}

// Set records or replaces one invocation scope atomically.
func (s *ScopeStore) Set(key ScopeKey, scope TurnScope) error {
	if key.SessionID == "" || key.InvocationID == "" {
		return errors.New("turn scope requires session and invocation IDs")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.scopes == nil {
		s.scopes = make(map[ScopeKey]TurnScope)
	}
	s.scopes[key] = cloneTurnScope(scope)
	return nil
}

// Begin records a new scope and rejects accidental key reuse.
func (s *ScopeStore) Begin(key ScopeKey, scope TurnScope) error {
	if key.SessionID == "" || key.InvocationID == "" {
		return errors.New("turn scope requires session and invocation IDs")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.scopes == nil {
		s.scopes = make(map[ScopeKey]TurnScope)
	}
	if _, exists := s.scopes[key]; exists {
		return errors.New("turn scope already exists")
	}
	s.scopes[key] = cloneTurnScope(scope)
	return nil
}

// Get returns a defensive copy of a scope.
func (s *ScopeStore) Get(key ScopeKey) (TurnScope, bool) {
	s.mu.RLock()
	scope, ok := s.scopes[key]
	s.mu.RUnlock()
	if !ok {
		return TurnScope{}, false
	}
	return cloneTurnScope(scope), true
}

// Release deletes a scope and reports whether it existed.
func (s *ScopeStore) Release(key ScopeKey) bool {
	s.mu.Lock()
	_, exists := s.scopes[key]
	delete(s.scopes, key)
	s.mu.Unlock()
	return exists
}

// ReleaseSession removes every invocation scope for one session.
func (s *ScopeStore) ReleaseSession(sessionID string) int {
	s.mu.Lock()
	removed := 0
	for key := range s.scopes {
		if key.SessionID == sessionID {
			delete(s.scopes, key)
			removed++
		}
	}
	s.mu.Unlock()
	return removed
}

// Len returns the number of active scopes.
func (s *ScopeStore) Len() int {
	s.mu.RLock()
	length := len(s.scopes)
	s.mu.RUnlock()
	return length
}

func cloneTurnScope(scope TurnScope) TurnScope {
	scope.Policy = ToolPolicy{layers: clonePolicyLayers(scope.Policy.layers)}
	scope.Arguments.Declared = append([]string(nil), scope.Arguments.Declared...)
	scope.Arguments.Positionals = append([]string(nil), scope.Arguments.Positionals...)
	scope.Arguments.Named = append([]NamedArgument(nil), scope.Arguments.Named...)
	return scope
}
