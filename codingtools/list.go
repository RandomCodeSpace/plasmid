package codingtools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/plasmid-dev/plasmid/codingtools/internal/walk"
	adktool "google.golang.org/adk/v2/tool"

	"github.com/plasmid-dev/plasmid/outputlimit"
	"github.com/plasmid-dev/plasmid/workspace"
)

const (
	defaultListDepth   = 1
	defaultListResults = 20000
)

// listHandler lists bounded workspace directory entries behind the native ADK tool.
type listHandler struct {
	root   *workspace.Root
	touch  *workspace.TouchBus
	output outputlimit.Policy
	budget *outputlimit.Budget
}

// NewListTool validates the listing dependencies and constructs an ls tool.
func NewListTool(cfg Config) (adktool.Tool, error) {
	handler, err := newListHandler(cfg)
	if err != nil {
		return nil, err
	}
	return newNativeTool("ls", ListDescription, ListInputSchema(), handler.call)
}

func newListHandler(cfg Config) (*listHandler, error) {
	if cfg.Root == nil {
		return nil, errors.New("construct ls tool: workspace root is required; provide the harness workspace root")
	}
	if cfg.Touch == nil {
		return nil, errors.New("construct ls tool: touch bus is required; provide the shared workspace touch bus")
	}
	if cfg.Budget == nil {
		return nil, errors.New("construct ls tool: output budget is required; provide the shared session budget")
	}
	if cfg.Output == (outputlimit.Policy{}) {
		cfg.Output = outputlimit.Defaults()
	}
	if _, err := outputlimit.NewWriter(cfg.Output); err != nil {
		return nil, fmt.Errorf("construct ls tool: invalid output policy: %w; provide non-negative output limits", err)
	}
	return &listHandler{root: cfg.Root, touch: cfg.Touch, output: cfg.Output, budget: cfg.Budget}, nil
}

// call lists descendants of a workspace directory, retaining bounded walk
// traversal as a successful truncated result.
func (t *listHandler) call(ctx context.Context, sessionID string, args ListArgs) (result map[string]any, err error) {
	reservation := t.budget.Reserve(sessionID, t.output.MaxBytes)
	emitted := 0
	defer func() { t.budget.Consume(sessionID, reservation.ID, emitted) }()
	if err := listContextError(ctx); err != nil {
		return result, err
	}
	if args.Path == "" {
		args.Path = "."
	}
	if args.MaxDepth == 0 {
		args.MaxDepth = defaultListDepth
	}
	if args.MaxResults == 0 {
		args.MaxResults = defaultListResults
	}
	absolute, err := t.root.ResolveExisting(args.Path)
	if err != nil {
		return result, listResolveError(err)
	}
	relative := t.root.Rel(absolute)

	entries, err := collectListEntries(ctx, absolute, relative, args)
	truncated := errors.Is(err, walk.ErrWalkTruncated)
	if err != nil && !truncated {
		return result, err
	}
	if err := listContextError(ctx); err != nil {
		return result, err
	}
	sortListEntries(entries)
	if len(entries) > args.MaxResults {
		entries = entries[:args.MaxResults]
		truncated = true
	}
	encoded := boundedListResult(ListResult{Entries: entries, Truncated: truncated}, reservation.Grant, t.output.MaxBytes)
	if err := listContextError(ctx); err != nil {
		return result, err
	}
	marshaled, _ := json.Marshal(encoded)
	emitted = len(marshaled)
	t.touch.Publish(ctx, workspace.Touch{SessionID: sessionID, InvocationID: invocationID(ctx), Path: relative, Kind: workspace.TouchList})
	return encoded, nil
}

func collectListEntries(ctx context.Context, absolute, relative string, args ListArgs) ([]ListEntry, error) {
	walkRoot, err := workspace.NewRoot(absolute)
	if err != nil {
		return nil, fmt.Errorf("ls workspace path: %w; verify the directory is readable and retry", err)
	}
	entries := make([]ListEntry, 0)
	err = walk.Walk(ctx, &walk.Filter{
		Root: walkRoot, MaxDepth: args.MaxDepth, MaxResults: -1, SkipHidden: !args.ShowHidden,
	}, func(entry walk.Entry) error {
		if err := listContextError(ctx); err != nil {
			return err
		}
		path := entry.Path
		if relative != "." {
			path = relative + "/" + path
		}
		entries = append(entries, ListEntry{Path: path, Type: listEntryType(entry), Size: entry.Size, ModTime: entry.ModTime.UTC().Format("2006-01-02T15:04:05Z07:00")})
		return nil
	})
	return entries, err
}

func sortListEntries(entries []ListEntry) {
	sort.Slice(entries, func(left, right int) bool {
		leftDirectory := entries[left].Type == entryTypeDirectory
		rightDirectory := entries[right].Type == entryTypeDirectory
		if leftDirectory != rightDirectory {
			return leftDirectory
		}
		return entries[left].Path < entries[right].Path
	})
}

func boundedListResult(value ListResult, grant, configured int) map[string]any {
	limit := resultLimit(grant, configured)
	for {
		object := resultObject(value)
		encoded, _ := json.Marshal(object)
		if limit > 0 && len(encoded) <= limit || len(value.Entries) == 0 {
			return object
		}
		value.Entries = value.Entries[:len(value.Entries)-1]
		value.Truncated = true
	}
}

func listEntryType(entry walk.Entry) string {
	if entry.IsSymlink {
		return entryTypeSymlink
	}
	if entry.IsDir {
		return entryTypeDirectory
	}
	return entryTypeFile
}

func listContextError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("ls cancelled: %w; retry with an active context", err)
	}
	return nil
}

func listResolveError(err error) error {
	switch {
	case errors.Is(err, workspace.ErrOutsideRoot):
		return fmt.Errorf("ls workspace path: %w; use a workspace-relative path inside the working directory", ErrPathOutsideRoot)
	case errors.Is(err, workspace.ErrNotFound):
		return fmt.Errorf("ls workspace path: %w; verify the path and retry", ErrFileNotFound)
	default:
		return fmt.Errorf("ls workspace path: %w; verify the directory is readable and retry", err)
	}
}
