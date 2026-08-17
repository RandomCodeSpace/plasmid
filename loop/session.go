package loop

import (
	"context"
	"time"
)

// SessionRef identifies persisted loop state.
type SessionRef struct {
	ID         string         `json:"id"`
	AppName    string         `json:"appName"`
	UserID     string         `json:"userId"`
	State      map[string]any `json:"state"`
	LastUpdate time.Time      `json:"lastUpdate"`
}

// CreateSessionRequest describes a new persisted session. An empty SessionID
// delegates ID generation to the store.
type CreateSessionRequest struct {
	AppName   string
	UserID    string
	SessionID string
	State     map[string]any
}

// SessionStore is the framework-free session persistence port. Append must
// preserve the serialized bytes in Event.Raw so Get can reconstruct an
// adapter-equivalent event. Sidecar values are opaque to the store and the
// last value written for a kind is the value loaded. Close must be idempotent.
type SessionStore interface {
	Create(ctx context.Context, request CreateSessionRequest) (SessionRef, error)
	Get(ctx context.Context, appName, userID, sessionID string) (SessionRef, []Event, error)
	List(ctx context.Context, appName, userID string) ([]SessionRef, error)
	Delete(ctx context.Context, appName, userID, sessionID string) error
	Append(ctx context.Context, ref SessionRef, event Event) error
	AppendSidecar(ctx context.Context, appName, userID, sessionID, kind string, value any) error
	LoadSidecar(ctx context.Context, appName, userID, sessionID, kind string, destination any) (bool, error)
	Close() error
}
