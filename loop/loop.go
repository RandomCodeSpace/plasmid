// Package loop defines Plasmid's framework-free agent loop contract.
package loop

import (
	"encoding/json"
	"fmt"
	"sync"
)

// Role identifies a message participant. Unknown values are retained so newer
// providers remain forward compatible with older hosts.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleSystem    Role = "system"
	RoleTool      Role = "tool"
)

// Message is the portable model-message projection. Raw may contain a
// JSON-compatible provider representation needed for lossless persistence or
// adapter round trips. Non-portable provider values are for in-memory use only.
type Message struct {
	Role         Role         `json:"role"`
	Text         string       `json:"text"`
	ToolCalls    []ToolCall   `json:"toolCalls"`
	ToolResults  []ToolResult `json:"toolResults"`
	ApproxTokens int          `json:"approxTokens"`
	Raw          any          `json:"raw"`
}

// ToolCall is the framework-free input supplied to a Tool.
type ToolCall struct {
	ID           string         `json:"id"`
	Name         string         `json:"name"`
	Args         map[string]any `json:"args"`
	SessionID    string         `json:"sessionId"`
	InvocationID string         `json:"invocationId"`
}

// ToolResult is the portable result of a tool call. Content is always a JSON
// object. The top-level keys diagnostics and diagnostics_text are reserved for
// LSP result decoration.
type ToolResult struct {
	CallID  string         `json:"callId"`
	Content map[string]any `json:"content"`
	IsError bool           `json:"isError"`
}

// ToolSchema is a provider-neutral JSON Schema declaration.
type ToolSchema struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// ModelRequest is the mutable value passed through before-model hooks.
type ModelRequest struct {
	Model    string       `json:"model"`
	System   string       `json:"system"`
	Messages []Message    `json:"messages"`
	Tools    []ToolSchema `json:"tools"`
	Raw      any          `json:"raw"`
}

// ModelResponse is the portable result passed through model hooks. Err is a Go
// runtime concern and is deliberately absent from portable JSON.
type ModelResponse struct {
	Message Message `json:"message"`
	Usage   *Usage  `json:"usage"`
	Err     error   `json:"-"`
	Raw     any     `json:"raw"`
}

// Usage contains the smallest counters shared by the documented event bridge.
// A zero counter means unavailable; a nil *Usage distinguishes absent usage.
type Usage struct {
	PromptTokens     int `json:"promptTokens"`
	CompletionTokens int `json:"completionTokens"`
	TotalTokens      int `json:"totalTokens"`
}

// View describes turn-scoped tool visibility.
type View struct {
	SessionID       string   `json:"sessionId"`
	InvocationID    string   `json:"invocationId"`
	AllowedTools    []string `json:"allowedTools"`
	DisallowedTools []string `json:"disallowedTools"`
}

// Warning is the single warning shape emitted by every core subsystem.
type Warning struct {
	Code    string `json:"code"`
	Source  string `json:"source"`
	Path    string `json:"path"`
	Line    int    `json:"line"`
	Message string `json:"message"`
}

// String renders the normative warning line.
func (w Warning) String() string {
	return fmt.Sprintf("%s:%d: %s: %s", w.Path, w.Line, w.Code, w.Message)
}

// WarningSink receives non-fatal degradation notices.
type WarningSink interface {
	Warn(Warning)
}

// DiscardSink ignores warnings.
type DiscardSink struct{}

// Warn implements WarningSink.
func (DiscardSink) Warn(Warning) {}

// SliceSink collects warnings safely in append order.
type SliceSink struct {
	mu       sync.RWMutex
	warnings []Warning
}

// Warn appends warning to the sink.
func (s *SliceSink) Warn(warning Warning) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.warnings = append(s.warnings, warning)
	s.mu.Unlock()
}

// Warnings returns a defensive copy in append order.
func (s *SliceSink) Warnings() []Warning {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	warnings := append([]Warning(nil), s.warnings...)
	s.mu.RUnlock()
	return warnings
}
