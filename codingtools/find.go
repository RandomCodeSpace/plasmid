package codingtools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/plasmid-dev/plasmid/codingtools/internal/walk"
	"github.com/plasmid-dev/plasmid/internal/pathglob"
	adktool "google.golang.org/adk/v2/tool"

	"github.com/plasmid-dev/plasmid/outputlimit"
	"github.com/plasmid-dev/plasmid/warning"
	"github.com/plasmid-dev/plasmid/workspace"
)

const (
	defaultFindMaxResults = 200
)

// findHandler locates workspace entries without following symlinks.
type findHandler struct {
	root           *workspace.Root
	touch          *workspace.TouchBus
	output         outputlimit.Policy
	budget         *outputlimit.Budget
	maxTouchEvents int
	warnings       warning.Warner
}

// NewFindTool constructs the native ADK find tool.
func NewFindTool(cfg Config) (adktool.Tool, error) {
	handler, err := newFindHandler(cfg)
	if err != nil {
		return nil, err
	}
	return newNativeTool("find", FindDescription, FindInputSchema(), handler.call)
}

func newFindHandler(cfg Config) (*findHandler, error) {
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
	if cfg.MaxTouchEvents <= 0 {
		cfg.MaxTouchEvents = MaxTouchEvents
	}
	if _, err := outputlimit.NewWriter(cfg.Output); err != nil {
		return nil, fmt.Errorf("construct find tool: invalid output policy: %w; provide non-negative output limits", err)
	}
	return &findHandler{root: cfg.Root, touch: cfg.Touch, output: cfg.Output, budget: cfg.Budget, maxTouchEvents: cfg.MaxTouchEvents, warnings: configWarningSink(cfg)}, nil
}

// call walks the workspace, sorts all matching entries, and only then applies
// the response limit so ordering is stable regardless of traversal caps.
func (t *findHandler) call(ctx context.Context, sessionID string, args FindArgs) (result map[string]any, err error) {
	reservation := t.budget.Reserve(sessionID, t.output.MaxBytes)
	emitted := 0
	defer func() { t.budget.Consume(sessionID, reservation.ID, emitted) }()
	if err := findContextError(ctx); err != nil {
		return result, err
	}
	if args.Glob == "" {
		return result, errors.New("find arguments: glob is required and must be non-empty; provide a slash-separated glob")
	}
	applyFindDefaults(&args)
	base, matcher, err := t.resolveFindBase(args)
	if err != nil {
		return result, err
	}
	entries, matchedPaths, err := t.collectFindEntries(ctx, args.Type, base, matcher)
	if err != nil {
		return result, err
	}
	if err := findContextError(ctx); err != nil {
		return result, err
	}
	paths, truncated := sortedFindPaths(entries, args.SortBy, args.MaxResults)
	content := boundedFindResult(FindResult{Paths: paths, Truncated: truncated}, reservation.Grant, t.output.MaxBytes)
	encoded, _ := json.Marshal(content)
	emitted = len(encoded)
	publishSearchTouches(ctx, t.touch, t.warnings, sessionID, matchedPaths, t.maxTouchEvents)
	return content, nil
}

func applyFindDefaults(args *FindArgs) {
	if args.Path == "" {
		args.Path = "."
	}
	if args.Type == "" {
		args.Type = "any"
	}
	if args.SortBy == "" {
		args.SortBy = "path"
	}
	if args.MaxResults == 0 {
		args.MaxResults = defaultFindMaxResults
	}
}

func (t *findHandler) resolveFindBase(args FindArgs) (string, pathglob.Matcher, error) {
	absolute, err := t.root.ResolveExisting(args.Path)
	if err != nil {
		return "", nil, findResolveError(err)
	}
	if _, err := workspace.NewRoot(absolute); err != nil {
		return "", nil, fmt.Errorf("find workspace path: %w; provide a readable directory path", err)
	}
	matcher, err := pathglob.CompileOne(args.Glob)
	if err != nil {
		return "", nil, fmt.Errorf("find arguments: %w: %v; provide a supported slash-separated glob", ErrUnsupportedPattern, err)
	}
	return t.root.Rel(absolute), matcher, nil
}

func (t *findHandler) collectFindEntries(ctx context.Context, entryType, base string, matcher pathglob.Matcher) ([]findEntry, []string, error) {
	entries := make([]findEntry, 0)
	matchedPaths := make([]string, 0)
	err := walk.Walk(ctx, &walk.Filter{
		Root:        t.root,
		WarningSink: t.warnings,
		SkipHidden:  true,
		SkipVCS:     true,
		MaxDepth:    -1,
		MaxResults:  -1,
	}, func(entry walk.Entry) error {
		if err := findContextError(ctx); err != nil {
			return err
		}
		if !findWithinBase(entry.Path, base) || !matcher.Match(entry.Path) || !findMatchesType(entry, entryType) {
			return nil
		}
		entries = append(entries, findEntry{path: entry.Path, modTime: entry.ModTime})
		if !entry.IsDir {
			matchedPaths = append(matchedPaths, entry.Path)
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, nil, findContextError(ctx)
		}
		return nil, nil, fmt.Errorf("find workspace: %w; verify the workspace is readable and retry", err)
	}
	return entries, matchedPaths, nil
}

func sortedFindPaths(entries []findEntry, sortBy string, maximum int) ([]string, bool) {
	sort.Slice(entries, func(i, j int) bool {
		if sortBy == "modified" && !entries[i].modTime.Equal(entries[j].modTime) {
			return entries[i].modTime.After(entries[j].modTime)
		}
		return entries[i].path < entries[j].path
	})
	truncated := len(entries) > maximum
	if truncated {
		entries = entries[:maximum]
	}
	paths := make([]string, len(entries))
	for index, entry := range entries {
		paths[index] = entry.path
	}
	return paths, truncated
}

func boundedFindResult(value FindResult, grant, configured int) map[string]any {
	limit := resultLimit(grant, configured)
	for {
		object := resultObject(value)
		encoded, _ := json.Marshal(object)
		if limit > 0 && len(encoded) <= limit || len(value.Paths) == 0 {
			return object
		}
		value.Paths = value.Paths[:len(value.Paths)-1]
		value.Truncated = true
	}
}

type findEntry struct {
	path    string
	modTime time.Time
}

func findWithinBase(path, base string) bool {
	return base == "." || path == base || strings.HasPrefix(path, base+"/")
}

func findMatchesType(entry walk.Entry, filter string) bool {
	switch filter {
	case entryTypeDirectory:
		return entry.IsDir
	case entryTypeSymlink:
		return entry.IsSymlink
	case entryTypeFile:
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
