package codingtools

import (
	"bufio"
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

	"github.com/plasmid-dev/plasmid/codingtools/internal/walk"
	"github.com/plasmid-dev/plasmid/internal/pathglob"
	adktool "google.golang.org/adk/v2/tool"

	"github.com/plasmid-dev/plasmid/outputlimit"
	"github.com/plasmid-dev/plasmid/warning"
	"github.com/plasmid-dev/plasmid/workspace"
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
	warnings         warning.Sink
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
	if cfg.Output == (outputlimit.Policy{}) {
		cfg.Output = outputlimit.Defaults()
	}
	if _, err := outputlimit.NewWriter(cfg.Output); err != nil {
		return nil, fmt.Errorf("construct grep tool: invalid output policy: %w; provide non-negative output limits", err)
	}
	if cfg.Output.MaxLines <= 0 {
		return nil, errors.New("construct grep tool: output max lines must be positive; provide a positive output limit")
	}
	return &grepHandler{root: cfg.Root, touch: cfg.Touch, output: cfg.Output, budget: cfg.Budget, maxGrepFileBytes: cfg.MaxGrepFileBytes, warnings: configWarningSink(cfg)}, nil
}

// call searches a file or the regular files below a workspace directory.
func (t *grepHandler) call(ctx context.Context, sessionID string, rawArgs map[string]any) (result map[string]any, err error) {
	reservation := t.budget.Reserve(sessionID, t.output.MaxBytes)
	emitted := 0
	defer func() { t.budget.Consume(sessionID, reservation.ID, emitted) }()
	if err := grepContextError(ctx); err != nil {
		return result, err
	}
	args, err := decodeGrepArgs(rawArgs)
	if err != nil {
		return result, err
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

	state := grepState{tool: t, ctx: ctx, re: re, contextLines: args.ContextLines}
	if info.Mode().IsRegular() {
		if args.Glob != "" {
			matcher, globErr := pathglob.CompileOne(args.Glob)
			if globErr != nil {
				return result, fmt.Errorf("grep arguments: %w: %v; provide a valid slash-separated glob", ErrUnsupportedPattern, globErr)
			}
			if !matcher.Match(t.root.Rel(abs)) {
				return t.finish(ctx, sessionID, args.MaxResults, state, reservation.Grant, &emitted)
			}
		}
		if err := state.searchFile(abs, t.root.Rel(abs)); err != nil {
			return result, err
		}
	} else if info.IsDir() {
		searchRoot := t.root.Rel(abs)
		filter := &walk.Filter{Root: t.root, WarningSink: t.warnings, IncludeGlobs: nonEmpty(args.Glob), SkipHidden: true, SkipVCS: true, RespectGitignore: true, MaxDepth: -1}
		err := walk.Walk(ctx, filter, func(entry walk.Entry) error {
			if !underSearchRoot(entry.Path, searchRoot) || entry.IsDir || entry.IsSymlink {
				return nil
			}
			return state.searchFile(filepath.Join(t.root.Dir(), filepath.FromSlash(entry.Path)), entry.Path)
		})
		if errors.Is(err, walk.ErrWalkTruncated) {
			state.walkTruncated = true
		} else if err != nil {
			return result, fmt.Errorf("grep workspace directory: %w; retry with an active workspace path", err)
		}
	} else {
		return result, fmt.Errorf("grep workspace path: %w; select a regular file or directory", workspace.ErrNotRegularFile)
	}
	return t.finish(ctx, sessionID, args.MaxResults, state, reservation.Grant, &emitted)
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
	truncated := state.walkTruncated || len(state.matches) > maximum
	if len(state.matches) > maximum {
		state.matches = state.matches[:maximum]
	}
	grepResult := GrepResult{Matches: state.matches, MatchCount: len(state.matches), Files: state.files, Truncated: truncated, SkippedBinary: state.skippedBinary, SkippedTooLarge: state.skippedTooLarge, SkippedLongLines: state.skippedLongLines}
	content, err := boundedGrepResult(grepResult, grant, t.output.MaxBytes)
	if err != nil {
		return nil, err
	}
	encoded, _ := json.Marshal(content)
	*emitted = len(encoded)
	publishSearchTouches(ctx, t.touch, t.warnings, sessionID, state.matchedPaths)
	return content, nil
}

func boundedGrepResult(value GrepResult, grant, configured int) (map[string]any, error) {
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
		object, err := resultObject(value)
		if err != nil {
			return nil, fmt.Errorf("encode grep result: %w; retry the search", err)
		}
		encoded, _ := json.Marshal(object)
		if limit <= 0 || len(encoded) <= limit || len(value.Matches) == 0 {
			return object, nil
		}
		value.Matches = value.Matches[:len(value.Matches)-1]
		value.MatchCount = len(value.Matches)
		value.Truncated = true
	}
}

type grepState struct {
	tool                                                    *grepHandler
	ctx                                                     context.Context
	re                                                      *regexp.Regexp
	contextLines                                            int
	matches                                                 []GrepMatch
	matchedPaths                                            []string
	walkTruncated                                           bool
	files, skippedBinary, skippedTooLarge, skippedLongLines int
}

func (s *grepState) searchFile(abs, relative string) error {
	if err := grepContextError(s.ctx); err != nil {
		return err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("grep file %q: %w", relative, err)
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	if info.Size() > s.tool.maxGrepFileBytes {
		s.skippedTooLarge++
		return nil
	}
	file, err := os.Open(abs)
	if err != nil {
		return fmt.Errorf("grep file %q: %w", relative, err)
	}
	defer file.Close()
	prefix := make([]byte, 8000)
	n, readErr := io.ReadFull(file, prefix)
	if readErr != nil && !errors.Is(readErr, io.ErrUnexpectedEOF) && !errors.Is(readErr, io.EOF) {
		return readErr
	}
	if isBinaryText(prefix[:n]) {
		s.skippedBinary++
		return nil
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	lines, longLines, err := grepLines(s.ctx, file)
	if err != nil {
		return err
	}
	s.skippedLongLines += longLines
	s.files++
	matched := false
	for index, line := range lines {
		if err := grepContextError(s.ctx); err != nil {
			return err
		}
		if line.long || !s.re.MatchString(line.text) {
			continue
		}
		match := GrepMatch{Path: relative, Line: index + 1, Text: line.text}
		start := max(0, index-s.contextLines)
		end := min(len(lines), index+s.contextLines+1)
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
		s.matches = append(s.matches, match)
		matched = true
	}
	if matched {
		s.matchedPaths = append(s.matchedPaths, relative)
	}
	return nil
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

func decodeGrepArgs(raw map[string]any) (GrepArgs, error) {
	object, err := decodeArgumentObject(raw)
	if err != nil {
		return GrepArgs{}, fmt.Errorf("grep arguments: %w; provide a JSON object matching the grep schema", err)
	}
	for key := range object {
		switch key {
		case "pattern", "path", "glob", "literal", "case_insensitive", "context_lines", "max_results":
		default:
			return GrepArgs{}, fmt.Errorf("grep arguments: unknown argument %q; remove unsupported arguments and retry", key)
		}
	}
	pattern, ok := object["pattern"].(string)
	if !ok {
		return GrepArgs{}, errors.New("grep arguments: pattern is required and must be a string; provide a portable regular expression or literal")
	}
	path, err := grepString(object, "path", ".")
	if err != nil {
		return GrepArgs{}, err
	}
	if path == "" {
		return GrepArgs{}, errors.New("grep arguments: path must not be empty; provide a workspace-relative path")
	}
	glob, err := grepString(object, "glob", "")
	if err != nil {
		return GrepArgs{}, err
	}
	literal, err := grepBool(object, "literal")
	if err != nil {
		return GrepArgs{}, err
	}
	insensitive, err := grepBool(object, "case_insensitive")
	if err != nil {
		return GrepArgs{}, err
	}
	contextLines, err := integerArgument(object, "context_lines", 0)
	if err != nil {
		return GrepArgs{}, fmt.Errorf("grep arguments: %w; provide context_lines as a non-negative JSON integer", err)
	}
	maximum, err := integerArgument(object, "max_results", defaultGrepResults)
	if err != nil {
		return GrepArgs{}, fmt.Errorf("grep arguments: %w; provide max_results as a positive JSON integer", err)
	}
	if contextLines < 0 {
		return GrepArgs{}, errors.New("grep arguments: context_lines must be non-negative; provide zero or more surrounding lines")
	}
	if maximum < 1 {
		return GrepArgs{}, errors.New("grep arguments: max_results must be at least 1; provide a positive result limit")
	}
	return GrepArgs{Pattern: pattern, Path: path, Glob: glob, Literal: literal, CaseInsensitive: insensitive, ContextLines: contextLines, MaxResults: maximum}, nil
}

func grepString(object map[string]any, name, fallback string) (string, error) {
	value, exists := object[name]
	if !exists {
		return fallback, nil
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("grep arguments: %s must be a string; provide a JSON string", name)
	}
	return text, nil
}
func grepBool(object map[string]any, name string) (bool, error) {
	value, exists := object[name]
	if !exists {
		return false, nil
	}
	flag, ok := value.(bool)
	if !ok {
		return false, fmt.Errorf("grep arguments: %s must be a boolean; provide true or false", name)
	}
	return flag, nil
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
		if pattern[i] == '\\' && i+1 < len(pattern) {
			if pattern[i+1] >= '1' && pattern[i+1] <= '9' {
				return "backreference"
			}
			i++
			continue
		}
		if i+2 < len(pattern) && pattern[i:i+3] == "(?=" {
			return "lookahead"
		}
		if i+3 < len(pattern) && (pattern[i:i+4] == "(?!" || pattern[i:i+4] == "(?<") {
			return "lookaround"
		}
	}
	return ""
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
