// Package warning defines Plasmid's framework-free non-fatal warning contract.
package warning

import (
	"fmt"
	"log/slog"
	"sync"
)

// Warning is the single warning shape emitted by every subsystem.
type Warning struct {
	Code    string `json:"code"`
	Source  string `json:"source"`
	Path    string `json:"path"`
	Line    int    `json:"line"`
	Message string `json:"message"`
}

// String renders the stable warning line.
func (w Warning) String() string {
	return fmt.Sprintf("%s:%d: %s: %s", w.Path, w.Line, w.Code, w.Message)
}

// Warner receives non-fatal degradation notices.
type Warner interface {
	Warn(Warning)
}

// Sink preserves source compatibility for existing hosts.
type Sink = Warner

// DiscardSink ignores warnings.
type DiscardSink struct{}

// Warn implements Warner.
func (DiscardSink) Warn(Warning) {
	// Discarding warnings is the explicit behavior of this sink.
}

// SlogSink writes warnings as structured slog records.
type SlogSink struct {
	Logger *slog.Logger
}

// Warn implements Warner.
func (s SlogSink) Warn(value Warning) {
	logger := s.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Warn(
		"plasmid warning",
		"code", value.Code,
		"source", value.Source,
		"path", value.Path,
		"line", value.Line,
		"message", value.Message,
	)
}

// SliceSink collects warnings safely in append order.
type SliceSink struct {
	mu       sync.RWMutex
	warnings []Warning
}

// Warn appends a warning to the sink.
func (s *SliceSink) Warn(value Warning) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.warnings = append(s.warnings, value)
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
