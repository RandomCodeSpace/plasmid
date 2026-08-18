package lsp

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/plasmid-dev/plasmid/workspace"
	"go.lsp.dev/protocol"
)

const DefaultDiagnosticsPerFile = 20

// Diagnostic is Plasmid's deterministic, JSON-safe diagnostic projection.
type Diagnostic struct {
	Path     string                      `json:"path"`
	Start    protocol.Position           `json:"start"`
	End      protocol.Position           `json:"end"`
	Severity protocol.DiagnosticSeverity `json:"severity"`
	Code     string                      `json:"code,omitempty"`
	Source   string                      `json:"source,omitempty"`
	Message  string                      `json:"message"`
}

// NormalizeDiagnostics confines, sorts, deduplicates, and bounds diagnostics
// for one document URI.
func NormalizeDiagnostics(rootDir, documentURI string, values []protocol.Diagnostic, maximum int) ([]Diagnostic, error) {
	path, err := FileURIToPath(documentURI)
	if err != nil {
		return nil, fmt.Errorf("normalize diagnostics: %w", err)
	}
	root, err := workspace.NewRoot(rootDir)
	if err != nil {
		return nil, fmt.Errorf("normalize diagnostics: %w", err)
	}
	absolute, err := root.Resolve(path)
	if err != nil {
		return nil, fmt.Errorf("normalize diagnostics: %w", err)
	}
	if maximum <= 0 {
		maximum = DefaultDiagnosticsPerFile
	}
	path = root.Rel(absolute)
	normalized := make([]Diagnostic, 0, min(len(values), maximum))
	for _, value := range values {
		if comparePosition(value.Range.End, value.Range.Start) < 0 {
			continue
		}
		diagnostic := Diagnostic{
			Path: path, Start: value.Range.Start, End: value.Range.End,
			Severity: value.Severity, Code: diagnosticCode(value.Code),
			Message: normalizeDiagnosticMessage(value.Message),
		}
		diagnostic.Source, _ = value.Source.Get()
		normalized = append(normalized, diagnostic)
	}
	slices.SortFunc(normalized, compareDiagnostic)
	normalized = slices.Compact(normalized)
	if len(normalized) > maximum {
		normalized = normalized[:maximum]
	}
	return normalized, nil
}

func compareDiagnostic(left, right Diagnostic) int {
	if value := comparePosition(left.Start, right.Start); value != 0 {
		return value
	}
	if value := comparePosition(left.End, right.End); value != 0 {
		return value
	}
	if left.Severity != right.Severity {
		return int(left.Severity) - int(right.Severity)
	}
	if value := strings.Compare(left.Code, right.Code); value != 0 {
		return value
	}
	if value := strings.Compare(left.Source, right.Source); value != 0 {
		return value
	}
	return strings.Compare(left.Message, right.Message)
}

func comparePosition(left, right protocol.Position) int {
	if left.Line != right.Line {
		if left.Line < right.Line {
			return -1
		}
		return 1
	}
	if left.Character < right.Character {
		return -1
	}
	if left.Character > right.Character {
		return 1
	}
	return 0
}

func diagnosticCode(value protocol.ProgressToken) string {
	switch code := value.(type) {
	case protocol.String:
		return string(code)
	case protocol.Integer:
		return strconv.FormatInt(int64(code), 10)
	default:
		return ""
	}
}

func normalizeDiagnosticMessage(value protocol.InlayHintTooltip) string {
	var message string
	switch value := value.(type) {
	case protocol.String:
		message = string(value)
	case *protocol.MarkupContent:
		if value != nil {
			message = value.Value
		}
	}
	message = strings.ReplaceAll(message, "\r\n", "\n")
	return strings.ReplaceAll(message, "\r", "\n")
}
