package loop

import (
	"context"
	"iter"
	"log/slog"
)

// InstructionFunc resolves the system instruction for a session.
type InstructionFunc func(context.Context, SessionRef) (string, error)

// RunnerConfig configures a Provider once, before its first Run.
type RunnerConfig struct {
	AppName     string
	AgentName   string
	Instruction InstructionFunc
	Tools       []Tool
	Toolsets    []Toolset
	Hooks       Hooks
	Sessions    SessionStore
	Streaming   bool
	Logger      *slog.Logger
}

// RunRequest describes one model turn.
type RunRequest struct {
	UserID    string
	SessionID string
	Input     Message
	Stream    bool
}

// Provider runs model turns behind the normative boundary. Configure is called
// exactly once. Run implementations must support concurrent distinct sessions.
type Provider interface {
	Name() string
	Configure(context.Context, RunnerConfig) error
	Run(context.Context, RunRequest) iter.Seq2[Event, error]
	Close() error
}
