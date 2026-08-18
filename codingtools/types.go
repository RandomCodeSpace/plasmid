// Package codingtools defines Plasmid's native ADK coding tools and their wire
// arguments and result objects.
package codingtools

import "github.com/plasmid-dev/plasmid/outputlimit"

const (
	// DiagnosticsResultKey is reserved for structured LSP diagnostics added by
	// the native after-tool callback.
	DiagnosticsResultKey = "diagnostics"
	// DiagnosticsTextResultKey is reserved for the bounded model-facing LSP
	// diagnostic rendering added by the native after-tool callback.
	DiagnosticsTextResultKey = "diagnostics_text"
)

// ReadArgs selects a one-based line window from a text file. Zero Offset and
// Limit values select the schema defaults.
type ReadArgs struct {
	Path   string `json:"path"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
}

// ReadResult contains numbered text and the actual source window rendered from
// a workspace-relative path.
type ReadResult struct {
	Path       string             `json:"path"`
	Content    string             `json:"content"`
	StartLine  int                `json:"start_line"`
	EndLine    int                `json:"end_line"`
	TotalLines int                `json:"total_lines"`
	Truncated  bool               `json:"truncated"`
	Report     outputlimit.Report `json:"report"`
}

// WriteArgs supplies the complete contents of a file.
type WriteArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// WriteResult reports the durable content size and bounded unified diff.
type WriteResult struct {
	Path         string             `json:"path"`
	BytesWritten int                `json:"bytes_written"`
	Diff         string             `json:"diff"`
	Truncated    bool               `json:"truncated"`
	Report       outputlimit.Report `json:"report"`
}

// EditArgs describes one deterministic text replacement. ReplaceAll defaults
// to false.
type EditArgs struct {
	Path       string `json:"path"`
	OldText    string `json:"old_text"`
	NewText    string `json:"new_text"`
	ReplaceAll bool   `json:"replace_all"`
}

// EditResult reports the selected matching tier and bounded unified diff.
type EditResult struct {
	Path         string             `json:"path"`
	Replacements int                `json:"replacements"`
	MatchTier    string             `json:"match_tier"`
	Diff         string             `json:"diff"`
	Truncated    bool               `json:"truncated"`
	Report       outputlimit.Report `json:"report"`
}

// BashArgs describes one fresh, non-interactive shell invocation. An empty Dir
// and a zero TimeoutMS select the schema defaults.
type BashArgs struct {
	Command   string `json:"command"`
	Dir       string `json:"dir"`
	TimeoutMS int    `json:"timeout_ms"`
}

// BashResult is the portable projection of shellexec.Result. Process duration
// and the resolved host directory are deliberately excluded from the wire.
type BashResult struct {
	Stdout       string             `json:"stdout"`
	Stderr       string             `json:"stderr"`
	ExitCode     int                `json:"exit_code"`
	Signal       string             `json:"signal"`
	TimedOut     bool               `json:"timed_out"`
	Killed       bool               `json:"killed"`
	Truncated    bool               `json:"truncated"`
	StdoutReport outputlimit.Report `json:"stdout_report"`
	StderrReport outputlimit.Report `json:"stderr_report"`
}

// GrepArgs configures a bounded text search. Zero optional values select the
// schema defaults.
type GrepArgs struct {
	Pattern         string `json:"pattern"`
	Path            string `json:"path"`
	Glob            string `json:"glob"`
	Literal         bool   `json:"literal"`
	CaseInsensitive bool   `json:"case_insensitive"`
	ContextLines    int    `json:"context_lines"`
	MaxResults      int    `json:"max_results"`
}

// GrepMatch contains a full matching line and its requested surrounding
// context. Path is slash-separated and workspace-relative.
type GrepMatch struct {
	Path   string   `json:"path"`
	Line   int      `json:"line"`
	Text   string   `json:"text"`
	Before []string `json:"before"`
	After  []string `json:"after"`
}

// GrepResult contains sorted matches and explicit skip counts.
type GrepResult struct {
	Matches          []GrepMatch `json:"matches"`
	MatchCount       int         `json:"match_count"`
	Files            int         `json:"files"`
	Truncated        bool        `json:"truncated"`
	SkippedBinary    int         `json:"skipped_binary"`
	SkippedTooLarge  int         `json:"skipped_too_large"`
	SkippedLongLines int         `json:"skipped_long_lines"`
}

// FindArgs configures a bounded workspace glob search. Zero optional values
// select the schema defaults.
type FindArgs struct {
	Path       string `json:"path"`
	Glob       string `json:"glob"`
	Type       string `json:"type"`
	SortBy     string `json:"sort_by"`
	MaxResults int    `json:"max_results"`
}

// FindResult contains sorted slash-separated workspace-relative paths.
type FindResult struct {
	Paths     []string `json:"paths"`
	Truncated bool     `json:"truncated"`
}

// ListArgs configures a bounded directory listing. Zero optional values
// select the schema defaults.
type ListArgs struct {
	Path       string `json:"path"`
	MaxDepth   int    `json:"max_depth"`
	ShowHidden bool   `json:"show_hidden"`
	MaxResults int    `json:"max_results"`
}

// ListEntry is a portable projection of one workspace entry. ModTime is
// encoded as RFC3339 text; host file modes are deliberately excluded.
type ListEntry struct {
	Path    string `json:"path"`
	Type    string `json:"type"`
	Size    int64  `json:"size"`
	ModTime string `json:"mod_time"`
}

// ListResult contains directory-first entries in deterministic name order.
type ListResult struct {
	Entries   []ListEntry `json:"entries"`
	Truncated bool        `json:"truncated"`
}
