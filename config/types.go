// Package config owns Plasmid's versioned configuration, defaults, discovery,
// path normalization, validation, repair, and config warnings.
package config

import (
	"time"

	"github.com/plasmid-dev/plasmid/warning"
)

const CurrentVersion = 1

type LSPMode string

const (
	LSPAuto LSPMode = "auto"
	LSPOff  LSPMode = "off"
)

type PromptCommandMode string

const (
	PromptCommandsOff     PromptCommandMode = "off"
	PromptCommandsTrusted PromptCommandMode = "trusted"
	PromptCommandsOn      PromptCommandMode = "on"
)

type MCPTransport string

const (
	MCPStdio MCPTransport = "stdio"
	MCPHTTP  MCPTransport = "http"
)

// Config is the complete validated configuration consumed by Plasmid
// subsystems. Runtime paths and identity are included here but are not JSON
// config keys.
type Config struct {
	Version    int
	WorkingDir string
	SessionDir string
	AppName    string
	UserID     string
	LSP        LSP
	MCP        MCP
	Skills     Skills
	Foreign    Foreign
	Syntax     Syntax
	Context    Context
	Tools      Tools
	Compaction Compaction
}

type LSP struct {
	Mode                  LSPMode
	SettleTimeout         time.Duration
	InitializeTimeout     time.Duration
	RequestTimeout        time.Duration
	FailureThreshold      int
	MaxDiagnosticsPerFile int
	Servers               []LSPServer
}

type LSPServer struct {
	ID          string
	Command     string
	Args        []string
	Extensions  []string
	RootMarkers []string
	Disabled    bool
}

type MCP struct {
	InheritForeign bool
	AllowForeign   []string
	Servers        []MCPServer
}

type MCPServer struct {
	ID        string
	Transport MCPTransport
	Command   string
	Args      []string
	Env       map[string]string
	URL       string
	Headers   map[string]string
}

type Skills struct {
	Roots []string
}

type Foreign struct {
	Enabled      bool
	Claude       bool
	Codex        bool
	Copilot      bool
	TrustedRoots []string
}

type Syntax struct {
	PromptCommands      PromptCommandMode
	CommandTimeout      time.Duration
	DocumentTimeout     time.Duration
	CommandOutputBytes  int
	DocumentOutputBytes int
}

type Context struct {
	MaxFileBytes       int
	MaxBytes           int
	MaxImportDepth     int
	ImportRoots        []string
	TouchesPerToolCall int
}

type Tools struct {
	CallOutputBytes    int
	SessionOutputBytes int
	BashTimeout        time.Duration
	BashMaxTimeout     time.Duration
	Confirmation       bool
}

type Compaction struct {
	ContextTokens        int
	TriggerFraction      float64
	TargetFraction       float64
	KeepRecentContents   int
	MinimumElisionTokens int
	PreserveToolNames    []string
	Calibration          bool
}

// Options contains the configuration overrides accumulated by the root
// Harness's public functional options.
type Options struct {
	ConfigPath       string
	WorkingDir       string
	SessionDir       string
	UserID           string
	AppName          *string
	LSPMode          *LSPMode
	Foreign          *Foreign
	ToolConfirmation *bool
}

// Result includes the resolved source path and all non-fatal repair warnings.
// SourcePath is empty when discovery found no file.
type Result struct {
	Config     Config
	SourcePath string
	Warnings   []warning.Warning
}
