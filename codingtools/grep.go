package codingtools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/RandomCodeSpace/plasmid/codingtools/internal/walk"
	"github.com/RandomCodeSpace/plasmid/internal/pathglob"
	adktool "google.golang.org/adk/v2/tool"

	"github.com/RandomCodeSpace/plasmid/outputlimit"
	"github.com/RandomCodeSpace/plasmid/warning"
	"github.com/RandomCodeSpace/plasmid/workspace"
)

const (
	defaultMaxGrepFileBytes int64 = 10 << 20
	defaultGrepResults            = 200
	grepLineLimit                 = 1 << 20
)

// grepHandler searches regular workspace text files behind the native ADK tool.
type grepHandler struct {
	root             *workspace.Root
	touch            *workspace.TouchBus
	output           outputlimit.Policy
	budget           *outputlimit.Budget
	maxGrepFileBytes int64
	maxTouchEvents   int
	warnings         warning.Warner
}

// NewGrepTool validates shared search dependencies and constructs a grep tool.
func NewGrepTool(cfg Config) (adktool.Tool, error) {
	handler, err := newGrepHandler(cfg)
	if err != nil {
		return nil, err
	}
	return newNativeTool("grep", GrepDescription, GrepInputSchema(), handler.call)
}

func newGrepHandler(cfg Config) (*grepHandler, error) {
	if cfg.Root == nil {
		return nil, errors.New("construct grep tool: workspace root is required; provide the harness workspace root")
	}
	if cfg.Touch == nil {
		return nil, errors.New("construct grep tool: touch bus is required; provide the shared workspace touch bus")
	}
	if cfg.Budget == nil {
		return nil, errors.New("construct grep tool: output budget is required; provide the shared session budget")
	}
	if cfg.MaxGrepFileBytes <= 0 {
		cfg.MaxGrepFileBytes = defaultMaxGrepFileBytes
	}
	if cfg.MaxTouchEvents <= 0 {
		cfg.MaxTouchEvents = MaxTouchEvents
	}
	if cfg.Output == (outputlimit.Policy{}) {
		cfg.Output = outputlimit.Defaults()
	}
	if _, err := outputlimit.NewWriter(cfg.Output); err != nil {
		return nil, fmt.Errorf("construct grep tool: invalid output policy: %w; provide non-negative output limits", err)
	}
	if cfg.Output.MaxLines <= 0 {
		return nil, errors.New("construct grep tool: output max lines must be positive; provide a positive output limit")
	}
	return &grepHandler{root: cfg.Root, touch: cfg.Touch, output: cfg.Output, budget: cfg.Budget, maxGrepFileBytes: cfg.MaxGrepFileBytes, maxTouchEvents: cfg.MaxTouchEvents, warnings: configWarningSink(cfg)}, nil
}

// call searches a file or the regular files below a workspace directory.
func (t *grepHandler) call(ctx context.Context, sessionID string, args GrepArgs) (result map[string]any, err error) {
	reservation := t.budget.Reserve(sessionID, t.output.MaxBytes)
	emitted := 0
	defer func() { t.budget.Consume(sessionID, reservation.ID, emitted) }()
	if err := grepContextError(ctx); err != nil {
		return result, err
	}
	if args.Path == "" {
		args.Path = "."
	}
	if args.MaxResults == 0 {
		args.MaxResults = defaultGrepResults
	}
	re, err := compileGrepPattern(args)
	if err != nil {
		return result, err
	}
	abs, err := t.root.ResolveExisting(args.Path)
	if err != nil {
		return result, grepResolveError(err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return result, fmt.Errorf("grep workspace path: %w; verify the path is readable and retry", err)
	}

	state := grepState{tool: t, re: re, contextLines: args.ContextLines}
	if err := t.searchPath(ctx, args, abs, info, &state); err != nil {
		return result, err
	}
	return t.finish(ctx, sessionID, args.MaxResults, state, reservation.Grant, &emitted)
}

func (t *grepHandler) searchPath(ctx context.Context, args GrepArgs, absolute string, info os.FileInfo, state *grepState) error {
	if info.Mode().IsRegular() {
		return t.searchRegularFile(ctx, args.Glob, absolute, info, state)
	}
	if info.IsDir() {
		return t.searchDirectory(ctx, args.Glob, absolute, state)
	}
	return fmt.Errorf("grep workspace path: %w; select a regular file or directory", workspace.ErrNotRegularFile)
}

func (t *grepHandler) searchRegularFile(ctx context.Context, glob, absolute string, info os.FileInfo, state *grepState) error {
	if glob != "" {
		matcher, err := pathglob.CompileOne(glob)
		if err != nil {
			return fmt.Errorf("grep arguments: %w: %v; provide a valid slash-separated glob", ErrUnsupportedPattern, err)
		}
		if !matcher.Match(t.root.Rel(absolute)) {
			return nil
		}
	}
	return state.searchFile(ctx, absolute, t.root.Rel(absolute), info.Mode(), info.Size())
}

func (t *grepHandler) searchDirectory(ctx context.Context, glob, absolute string, state *grepState) error {
	searchRoot := t.root.Rel(absolute)
	filter := &walk.Filter{Root: t.root, WarningSink: t.warnings, IncludeGlobs: nonEmpty(glob), SkipHidden: true, SkipVCS: true, RespectGitignore: true, MaxDepth: -1}
	err := walk.Walk(ctx, filter, func(entry walk.Entry) error {
		if !underSearchRoot(entry.Path, searchRoot) || entry.IsDir || entry.IsSymlink {
			return nil
		}
		path := filepath.Join(t.root.Dir(), filepath.FromSlash(entry.Path))
		return state.searchFile(ctx, path, entry.Path, entry.Mode, entry.Size)
	})
	if err != nil {
		return fmt.Errorf("grep workspace directory: %w; retry with an active workspace path", err)
	}
	return nil
}

func (t *grepHandler) finish(ctx context.Context, sessionID string, maximum int, state grepState, grant int, emitted *int) (map[string]any, error) {
	if err := grepContextError(ctx); err != nil {
		return nil, err
	}
	sort.Slice(state.matches, func(i, j int) bool {
		if state.matches[i].Path == state.matches[j].Path {
			return state.matches[i].Line < state.matches[j].Line
		}
		return state.matches[i].Path < state.matches[j].Path
	})
	truncated := len(state.matches) > maximum
	if len(state.matches) > maximum {
		state.matches = state.matches[:maximum]
	}
	grepResult := GrepResult{Matches: state.matches, MatchCount: len(state.matches), Files: state.files, Truncated: truncated, SkippedBinary: state.skippedBinary, SkippedTooLarge: state.skippedTooLarge, SkippedLongLines: state.skippedLongLines}
	content := boundedGrepResult(grepResult, grant, t.output.MaxBytes)
	encoded, _ := json.Marshal(content)
	*emitted = len(encoded)
	publishSearchTouches(ctx, t.touch, t.warnings, sessionID, state.matchedPaths, t.maxTouchEvents)
	return content, nil
}

func boundedGrepResult(value GrepResult, grant, configured int) map[string]any {
	if grant == 0 {
		value.Matches = nil
		value.MatchCount = 0
		value.Truncated = true
		return resultObject(value)
	}
	limit := configured
	if limit <= 0 || (grant > 0 && grant < limit) {
		limit = grant
	}
	for {
		object := resultObject(value)
		encoded, _ := json.Marshal(object)
		if limit <= 0 || len(encoded) <= limit || len(value.Matches) == 0 {
			return object
		}
		value.Matches = value.Matches[:len(value.Matches)-1]
		value.MatchCount = len(value.Matches)
		value.Truncated = true
	}
}

type grepState struct {
	tool                                                    *grepHandler
	re                                                      *regexp.Regexp
	contextLines                                            int
	matches                                                 []GrepMatch
	matchedPaths                                            []string
	files, skippedBinary, skippedTooLarge, skippedLongLines int
}

func (s *grepState) searchFile(ctx context.Context, abs, relative string, mode os.FileMode, size int64) error {
	if err := grepContextError(ctx); err != nil {
		return err
	}
	if !mode.IsRegular() {
		return nil
	}
	if size > s.tool.maxGrepFileBytes {
		s.skippedTooLarge++
		return nil
	}
	file, err := os.Open(abs)
	if err != nil {
		return fmt.Errorf("grep file %q: %w", relative, err)
	}
	defer func() { _ = file.Close() }()
	prefix := make([]byte, 8000)
	n, readErr := io.ReadFull(file, prefix)
	if readErr != nil && !errors.Is(readErr, io.ErrUnexpectedEOF) && !errors.Is(readErr, io.EOF) {
		return readErr
	}
	if isBinaryText(prefix[:n]) {
		s.skippedBinary++
		return nil
	}
	lines, longLines, err := grepLines(ctx, io.MultiReader(bytes.NewReader(prefix[:n]), file))
	if err != nil {
		return err
	}
	s.skippedLongLines += longLines
	s.files++
	matched, err := s.collectMatches(ctx, relative, lines)
	if err != nil {
		return err
	}
	if matched {
		s.matchedPaths = append(s.matchedPaths, relative)
	}
	return nil
}

func (s *grepState) collectMatches(ctx context.Context, relative string, lines []grepLine) (bool, error) {
	matched := false
	for index, line := range lines {
		if err := grepContextError(ctx); err != nil {
			return false, err
		}
		if line.long || !s.re.MatchString(line.text) {
			continue
		}
		s.matches = append(s.matches, newGrepMatch(relative, index, lines, s.contextLines))
		matched = true
	}
	return matched, nil
}

func newGrepMatch(relative string, index int, lines []grepLine, contextLines int) GrepMatch {
	match := GrepMatch{Path: relative, Line: index + 1, Text: lines[index].text}
	start := max(0, index-contextLines)
	end := min(len(lines), index+contextLines+1)
	for before := start; before < index; before++ {
		if !lines[before].long {
			match.Before = append(match.Before, lines[before].text)
		}
	}
	for after := index + 1; after < end; after++ {
		if !lines[after].long {
			match.After = append(match.After, lines[after].text)
		}
	}
	return match
}

type grepLine struct {
	text string
	long bool
}

func grepLines(ctx context.Context, reader io.Reader) ([]grepLine, int, error) {
	buffer := bufio.NewReaderSize(reader, 64<<10)
	var lines []grepLine
	longLines := 0
	for {
		if err := grepContextError(ctx); err != nil {
			return nil, 0, err
		}
		line, err := buffer.ReadString('\n')
		if len(line) > grepLineLimit {
			lines = append(lines, grepLine{long: true})
			longLines++
		} else if len(line) > 0 {
			lines = append(lines, grepLine{text: strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")})
		}
		if err == nil {
			continue
		}
		if errors.Is(err, io.EOF) {
			return lines, longLines, nil
		}
		return nil, 0, err
	}
}

func compileGrepPattern(args GrepArgs) (*regexp.Regexp, error) {
	pattern := args.Pattern
	if args.Literal {
		pattern = regexp.QuoteMeta(pattern)
	} else if construct := unsupportedRegexpConstruct(pattern); construct != "" {
		return nil, fmt.Errorf("grep pattern: %w (%s); use the portable RE2 subset without backreferences or lookaround", ErrUnsupportedPattern, construct)
	}
	if args.CaseInsensitive {
		pattern = "(?i)" + pattern
	}
	compiled, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("grep pattern: %w: %v; provide a valid portable RE2 expression", ErrUnsupportedPattern, err)
	}
	return compiled, nil
}

func unsupportedRegexpConstruct(pattern string) string {
	for i := 0; i < len(pattern); i++ {
		if backreferenceAt(pattern, i) {
			return "backreference"
		}
		if pattern[i] == '\\' && i+1 < len(pattern) {
			i++
			continue
		}
		if i+2 < len(pattern) && (pattern[i:i+3] == "(?=" || pattern[i:i+3] == "(?!") {
			return "lookahead"
		}
		if i+3 < len(pattern) && (pattern[i:i+4] == "(?<=" || pattern[i:i+4] == "(?<!") {
			return "lookbehind"
		}
	}
	return ""
}

func backreferenceAt(pattern string, index int) bool {
	return index+1 < len(pattern) && pattern[index] == '\\' && pattern[index+1] >= '1' && pattern[index+1] <= '9'
}

func underSearchRoot(path, root string) bool {
	return root == "." || path == root || strings.HasPrefix(path, root+"/")
}
func nonEmpty(value string) []string {
	if value == "" {
		return nil
	}
	return []string{value}
}
func grepContextError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("grep cancelled: %w; retry with an active context", err)
	}
	return nil
}
func grepResolveError(err error) error {
	if errors.Is(err, workspace.ErrOutsideRoot) {
		return fmt.Errorf("grep workspace path: %w; use a workspace-relative path inside the working directory", ErrPathOutsideRoot)
	}
	if errors.Is(err, workspace.ErrNotFound) {
		return fmt.Errorf("grep workspace path: %w; verify the path or use ls to inspect the working directory", ErrFileNotFound)
	}
	return fmt.Errorf("grep workspace path: %w; verify the path is readable and retry", err)
}
func max(left, right int) int {
	if left > right {
		return left
	}
	return right
}
func min(left, right int) int {
	if left < right {
		return left
	}
	return right
}
