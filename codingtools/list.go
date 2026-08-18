package codingtools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
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
func (t *listHandler) call(ctx context.Context, sessionID string, rawArgs map[string]any) (result map[string]any, err error) {
	reservation := t.budget.Reserve(sessionID, t.output.MaxBytes)
	emitted := 0
	defer func() { t.budget.Consume(sessionID, reservation.ID, emitted) }()
	if err := listContextError(ctx); err != nil {
		return result, err
	}
	args, err := decodeListArgs(rawArgs)
	if err != nil {
		return result, err
	}
	absolute, err := t.root.ResolveExisting(args.Path)
	if err != nil {
		return result, listResolveError(err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return result, listFilesystemError("stat", err)
	}
	if !info.IsDir() {
		return result, fmt.Errorf("ls workspace path: %w; select a directory path", workspace.ErrNotDirectory)
	}
	relative := t.root.Rel(absolute)
	if relative != "." && !safeRelative(relative) {
		return result, errors.New("ls workspace path: could not form a safe relative result path; use a path inside the working directory")
	}

	entries := make([]ListEntry, 0)
	walkRoot, err := workspace.NewRoot(absolute)
	if err != nil {
		return result, fmt.Errorf("ls workspace path: %w; verify the directory is readable and retry", err)
	}
	err = walk.Walk(ctx, &walk.Filter{
		Root:       walkRoot,
		MaxDepth:   args.MaxDepth,
		MaxResults: -1,
		SkipHidden: !args.ShowHidden,
	}, func(entry walk.Entry) error {
		if err := listContextError(ctx); err != nil {
			return err
		}
		path := entry.Path
		if relative != "." {
			path = relative + "/" + path
		}
		entries = append(entries, ListEntry{
			Path:    path,
			Type:    listEntryType(entry),
			Size:    entry.Size,
			ModTime: entry.ModTime.UTC().Format("2006-01-02T15:04:05Z07:00"),
		})
		return nil
	})
	truncated := errors.Is(err, walk.ErrWalkTruncated)
	if err != nil && !truncated {
		return result, err
	}
	if err := listContextError(ctx); err != nil {
		return result, err
	}
	sort.Slice(entries, func(left, right int) bool {
		leftDirectory := entries[left].Type == "dir"
		rightDirectory := entries[right].Type == "dir"
		if leftDirectory != rightDirectory {
			return leftDirectory
		}
		return entries[left].Path < entries[right].Path
	})
	if len(entries) > args.MaxResults {
		entries = entries[:args.MaxResults]
		truncated = true
	}
	encoded, err := boundedListResult(ListResult{Entries: entries, Truncated: truncated}, reservation.Grant, t.output.MaxBytes)
	if err != nil {
		return result, fmt.Errorf("encode ls result: %w; retry the listing", err)
	}
	if err := listContextError(ctx); err != nil {
		return result, err
	}
	marshaled, _ := json.Marshal(encoded)
	emitted = len(marshaled)
	t.touch.Publish(ctx, workspace.Touch{SessionID: sessionID, InvocationID: invocationID(ctx), Path: relative, Kind: workspace.TouchList})
	return encoded, nil
}

func boundedListResult(value ListResult, grant, configured int) (map[string]any, error) {
	limit := resultLimit(grant, configured)
	for {
		object, err := resultObject(value)
		if err != nil {
			return nil, err
		}
		encoded, _ := json.Marshal(object)
		if limit > 0 && len(encoded) <= limit || len(value.Entries) == 0 {
			return object, nil
		}
		value.Entries = value.Entries[:len(value.Entries)-1]
		value.Truncated = true
	}
}

func decodeListArgs(raw map[string]any) (ListArgs, error) {
	object, err := decodeArgumentObject(raw)
	if err != nil {
		return ListArgs{}, fmt.Errorf("ls arguments: %w; provide a JSON object matching the ls schema", err)
	}
	for key := range object {
		switch key {
		case "path", "max_depth", "show_hidden", "max_results":
		default:
			return ListArgs{}, fmt.Errorf("ls arguments: unknown argument %q; remove unsupported arguments and retry", key)
		}
	}
	path := "."
	if value, exists := object["path"]; exists {
		var ok bool
		path, ok = value.(string)
		if !ok {
			return ListArgs{}, errors.New("ls arguments: path must be a string; provide a workspace-relative directory path")
		}
		if path == "" {
			return ListArgs{}, errors.New("ls arguments: path must not be empty; provide a workspace-relative directory path")
		}
	}
	maxDepth, err := integerArgument(object, "max_depth", defaultListDepth)
	if err != nil {
		return ListArgs{}, fmt.Errorf("ls arguments: %w; provide max_depth as a positive JSON integer", err)
	}
	if maxDepth < 1 {
		return ListArgs{}, errors.New("ls arguments: max_depth must be at least 1; provide a positive traversal depth")
	}
	maxResults, err := integerArgument(object, "max_results", defaultListResults)
	if err != nil {
		return ListArgs{}, fmt.Errorf("ls arguments: %w; provide max_results as a positive JSON integer", err)
	}
	if maxResults < 1 {
		return ListArgs{}, errors.New("ls arguments: max_results must be at least 1; provide a positive result limit")
	}
	showHidden := false
	if value, exists := object["show_hidden"]; exists {
		var ok bool
		showHidden, ok = value.(bool)
		if !ok {
			return ListArgs{}, errors.New("ls arguments: show_hidden must be a boolean; provide true or false")
		}
	}
	return ListArgs{Path: path, MaxDepth: maxDepth, ShowHidden: showHidden, MaxResults: maxResults}, nil
}

func listEntryType(entry walk.Entry) string {
	if entry.IsSymlink {
		return "symlink"
	}
	if entry.IsDir {
		return "dir"
	}
	return "file"
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

func listFilesystemError(operation string, err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s ls workspace path: %w; verify the path and retry", operation, ErrFileNotFound)
	}
	if pathError := new(os.PathError); errors.As(err, &pathError) {
		return fmt.Errorf("%s ls workspace path: %w; verify the directory is readable and retry", operation, pathError.Err)
	}
	return fmt.Errorf("%s ls workspace path: %w; verify the directory is readable and retry", operation, err)
}
