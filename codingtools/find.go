package codingtools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/plasmid-dev/plasmid/codingtools/internal/walk"
	"github.com/plasmid-dev/plasmid/internal/pathglob"
	"github.com/plasmid-dev/plasmid/loop"
	"github.com/plasmid-dev/plasmid/outputlimit"
	"github.com/plasmid-dev/plasmid/workspace"
)

const (
	defaultFindMaxResults = 200
)

// FindTool locates workspace entries without following symlinks.
type FindTool struct {
	root   *workspace.Root
	touch  *workspace.TouchBus
	output outputlimit.Policy
	budget *outputlimit.Budget
	logger *slog.Logger
}

var _ loop.Tool = (*FindTool)(nil)

// NewFindTool constructs a provider-neutral find tool.
func NewFindTool(cfg Config) (loop.Tool, error) {
	if cfg.Root == nil {
		return nil, errors.New("construct find tool: workspace root is required; provide the harness workspace root")
	}
	if cfg.Touch == nil {
		return nil, errors.New("construct find tool: touch bus is required; provide the shared workspace touch bus")
	}
	if cfg.Budget == nil {
		return nil, errors.New("construct find tool: output budget is required; provide the shared session budget")
	}
	if cfg.Output == (outputlimit.Policy{}) {
		cfg.Output = outputlimit.Defaults()
	}
	if _, err := outputlimit.NewWriter(cfg.Output); err != nil {
		return nil, fmt.Errorf("construct find tool: invalid output policy: %w; provide non-negative output limits", err)
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &FindTool{root: cfg.Root, touch: cfg.Touch, output: cfg.Output, budget: cfg.Budget, logger: cfg.Logger}, nil
}

func (*FindTool) Name() string                 { return "find" }
func (*FindTool) Description() string          { return FindDescription }
func (*FindTool) InputSchema() json.RawMessage { return FindInputSchema() }

// Call walks the workspace, sorts all matching entries, and only then applies
// the response limit so ordering is stable regardless of traversal caps.
func (t *FindTool) Call(ctx context.Context, call loop.ToolCall) (result loop.ToolResult, err error) {
	result.CallID = call.ID
	reservation := t.budget.Reserve(call.SessionID, t.output.MaxBytes)
	emitted := 0
	defer func() { t.budget.Consume(call.SessionID, reservation.ID, emitted) }()
	if err := findContextError(ctx); err != nil {
		return result, err
	}
	args, err := decodeFindArgs(call.Args)
	if err != nil {
		return result, err
	}

	absolute, err := t.root.ResolveExisting(args.Path)
	if err != nil {
		return result, findResolveError(err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return result, fmt.Errorf("find workspace path: %w; verify the directory is readable and retry", err)
	}
	if !info.IsDir() {
		return result, fmt.Errorf("find workspace path: %w; provide a directory path", workspace.ErrNotDirectory)
	}
	base := t.root.Rel(absolute)
	if !safeRelative(base) && base != "." {
		return result, errors.New("find workspace path: could not form a safe relative search path; use a path inside the working directory")
	}

	matcher, matcherErr := pathglob.CompileOne(args.Glob)
	if matcherErr != nil {
		return result, fmt.Errorf("find arguments: %w: %v; provide a supported slash-separated glob", ErrUnsupportedPattern, matcherErr)
	}
	entries := make([]findEntry, 0)
	matchedPaths := make([]string, 0)
	truncated := false
	walkErr := walk.Walk(ctx, &walk.Filter{
		Root:       t.root,
		SkipHidden: true,
		SkipVCS:    true,
		MaxDepth:   -1,
		MaxResults: -1,
	}, func(entry walk.Entry) error {
		if err := findContextError(ctx); err != nil {
			return err
		}
		if !findWithinBase(entry.Path, base) || !matcher.Match(entry.Path) || !findMatchesType(entry, args.Type) {
			return nil
		}
		entries = append(entries, findEntry{path: entry.Path, modTime: entry.ModTime})
		if !entry.IsDir {
			matchedPaths = append(matchedPaths, entry.Path)
		}
		return nil
	})
	if walkErr != nil {
		if errors.Is(walkErr, walk.ErrWalkTruncated) {
			truncated = true
		} else if errors.Is(walkErr, context.Canceled) || errors.Is(walkErr, context.DeadlineExceeded) {
			return result, findContextError(ctx)
		} else {
			return result, fmt.Errorf("find workspace: %w; verify the workspace is readable and retry", walkErr)
		}
	}
	if err := findContextError(ctx); err != nil {
		return result, err
	}

	sort.Slice(entries, func(i, j int) bool {
		if args.SortBy == "modified" && !entries[i].modTime.Equal(entries[j].modTime) {
			return entries[i].modTime.After(entries[j].modTime)
		}
		return entries[i].path < entries[j].path
	})
	if len(entries) > args.MaxResults {
		truncated = true
	}
	if len(entries) > args.MaxResults {
		entries = entries[:args.MaxResults]
	}
	paths := make([]string, len(entries))
	for index, entry := range entries {
		paths[index] = entry.path
	}
	content, err := boundedFindResult(FindResult{Paths: paths, Truncated: truncated}, reservation.Grant, t.output.MaxBytes)
	if err != nil {
		return result, fmt.Errorf("encode find result: %w; retry the find", err)
	}
	result.Content = content
	encoded, _ := json.Marshal(content)
	emitted = len(encoded)
	publishSearchTouches(ctx, t.touch, t.logger, call.SessionID, matchedPaths)
	return result, nil
}

func boundedFindResult(value FindResult, grant, configured int) (map[string]any, error) {
	limit := resultLimit(grant, configured)
	for {
		object, err := resultObject(value)
		if err != nil {
			return nil, err
		}
		encoded, _ := json.Marshal(object)
		if limit > 0 && len(encoded) <= limit || len(value.Paths) == 0 {
			return object, nil
		}
		value.Paths = value.Paths[:len(value.Paths)-1]
		value.Truncated = true
	}
}

type findEntry struct {
	path    string
	modTime time.Time
}

func decodeFindArgs(raw map[string]any) (FindArgs, error) {
	object, err := decodeArgumentObject(raw)
	if err != nil {
		return FindArgs{}, fmt.Errorf("find arguments: %w; provide a JSON object matching the find schema", err)
	}
	for key := range object {
		switch key {
		case "path", "glob", "type", "sort_by", "max_results":
		default:
			return FindArgs{}, fmt.Errorf("find arguments: unknown argument %q; remove unsupported arguments and retry", key)
		}
	}
	glob, ok := object["glob"].(string)
	if !ok || glob == "" {
		return FindArgs{}, errors.New("find arguments: glob is required and must be a non-empty string; provide a slash-separated glob")
	}
	path, err := findStringArgument(object, "path", ".")
	if err != nil {
		return FindArgs{}, err
	}
	if path == "" {
		return FindArgs{}, errors.New("find arguments: path must not be empty; provide a workspace-relative directory path")
	}
	typeFilter, err := findStringArgument(object, "type", "any")
	if err != nil {
		return FindArgs{}, err
	}
	if typeFilter != "file" && typeFilter != "dir" && typeFilter != "symlink" && typeFilter != "any" {
		return FindArgs{}, errors.New("find arguments: type must be file, dir, symlink, or any; provide a supported entry type")
	}
	sortBy, err := findStringArgument(object, "sort_by", "path")
	if err != nil {
		return FindArgs{}, err
	}
	if sortBy != "path" && sortBy != "modified" {
		return FindArgs{}, errors.New("find arguments: sort_by must be path or modified; provide a supported sort order")
	}
	maxResults, err := integerArgument(object, "max_results", defaultFindMaxResults)
	if err != nil {
		return FindArgs{}, fmt.Errorf("find arguments: %w; provide max_results as a positive JSON integer", err)
	}
	if maxResults < 1 {
		return FindArgs{}, errors.New("find arguments: max_results must be at least 1; provide a positive result limit")
	}
	return FindArgs{Path: path, Glob: glob, Type: typeFilter, SortBy: sortBy, MaxResults: maxResults}, nil
}

func findStringArgument(object map[string]any, name, fallback string) (string, error) {
	value, exists := object[name]
	if !exists {
		return fallback, nil
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("find arguments: %s must be a string; provide a valid value", name)
	}
	return text, nil
}

func findWithinBase(path, base string) bool {
	return base == "." || path == base || strings.HasPrefix(path, base+"/")
}

func findMatchesType(entry walk.Entry, filter string) bool {
	switch filter {
	case "dir":
		return entry.IsDir
	case "symlink":
		return entry.IsSymlink
	case "file":
		return !entry.IsDir && !entry.IsSymlink && entry.Mode.IsRegular()
	default:
		return true
	}
}

func findContextError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("find cancelled: %w; retry with an active context", err)
	}
	return nil
}

func findResolveError(err error) error {
	switch {
	case errors.Is(err, workspace.ErrOutsideRoot):
		return fmt.Errorf("find workspace path: %w; use a workspace-relative directory inside the working directory", ErrPathOutsideRoot)
	case errors.Is(err, workspace.ErrNotFound):
		return fmt.Errorf("find workspace path: %w; verify the path or use ls to inspect the working directory", ErrFileNotFound)
	default:
		return fmt.Errorf("find workspace path: %w; verify the path is readable and retry", err)
	}
}
