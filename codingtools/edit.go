package codingtools

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"unicode/utf8"

	"github.com/plasmid-dev/plasmid/codingtools/internal/textmatch"
	"github.com/plasmid-dev/plasmid/loop"
	"github.com/plasmid-dev/plasmid/outputlimit"
	"github.com/plasmid-dev/plasmid/workspace"
)

// EditTool applies one deterministic replacement to a previously read file.
type EditTool struct {
	root          *workspace.Root
	queue         *workspace.MutationQueue
	ledger        *workspace.Ledger
	touch         *workspace.TouchBus
	output        outputlimit.Policy
	budget        *outputlimit.Budget
	maxWriteBytes int64
}

var _ loop.Tool = (*EditTool)(nil)

// NewEditTool validates the shared mutation dependencies and constructs an edit tool.
func NewEditTool(cfg Config) (loop.Tool, error) {
	if cfg.Root == nil {
		return nil, errors.New("construct edit tool: workspace root is required; provide the harness workspace root")
	}
	if cfg.Queue == nil {
		return nil, errors.New("construct edit tool: mutation queue is required; provide the shared workspace mutation queue")
	}
	if cfg.Ledger == nil {
		return nil, errors.New("construct edit tool: workspace ledger is required; provide the shared workspace ledger")
	}
	if cfg.Touch == nil {
		return nil, errors.New("construct edit tool: touch bus is required; provide the shared workspace touch bus")
	}
	if cfg.Budget == nil {
		return nil, errors.New("construct edit tool: output budget is required; provide the shared session budget")
	}
	if cfg.MaxWriteBytes <= 0 {
		cfg.MaxWriteBytes = defaultMaxWriteBytes
	}
	if cfg.Output == (outputlimit.Policy{}) {
		cfg.Output = outputlimit.Defaults()
	}
	if _, err := outputlimit.NewWriter(cfg.Output); err != nil {
		return nil, fmt.Errorf("construct edit tool: invalid output policy: %w; provide non-negative output limits", err)
	}
	if cfg.Output.MaxLines <= 0 {
		return nil, errors.New("construct edit tool: output max lines must be positive; provide a positive output limit")
	}
	return &EditTool{root: cfg.Root, queue: cfg.Queue, ledger: cfg.Ledger, touch: cfg.Touch, output: cfg.Output, budget: cfg.Budget, maxWriteBytes: cfg.MaxWriteBytes}, nil
}

func (*EditTool) Name() string                 { return "edit" }
func (*EditTool) Description() string          { return EditDescription }
func (*EditTool) InputSchema() json.RawMessage { return EditInputSchema() }

// Call serializes the entire read-modify-write transition against all other mutations.
func (t *EditTool) Call(ctx context.Context, call loop.ToolCall) (result loop.ToolResult, err error) {
	result.CallID = call.ID
	reservation := t.budget.Reserve(call.SessionID, t.output.MaxBytes)
	emitted := 0
	defer func() { t.budget.Consume(call.SessionID, reservation.ID, emitted) }()
	if err := editContextError(ctx); err != nil {
		return result, err
	}
	args, err := decodeEditArgs(call.Args)
	if err != nil {
		return result, err
	}

	var relative, diff string
	var replacement textmatch.Result
	err = t.queue.Do(ctx, func() error {
		if err := editContextError(ctx); err != nil {
			return err
		}
		absolute, err := t.root.ResolveExisting(args.Path)
		if err != nil {
			return editResolveError(err)
		}
		relative = t.root.Rel(absolute)
		if !safeRelative(relative) {
			return errors.New("edit workspace path: could not form a safe relative result path; use a path inside the working directory")
		}
		secureRoot, err := os.OpenRoot(t.root.Dir())
		if err != nil {
			return fmt.Errorf("open edit workspace root: %w; verify the workspace is accessible and retry", err)
		}
		defer secureRoot.Close()
		parent, err := secureRoot.OpenRoot(filepath.Dir(filepath.FromSlash(relative)))
		if err != nil {
			return fmt.Errorf("open edit workspace parent: %w; verify the file is accessible and retry", err)
		}
		defer parent.Close()
		targetName := filepath.Base(relative)
		info, err := parent.Lstat(targetName)
		if err != nil {
			return editFilesystemError("stat", err)
		}
		if info.IsDir() {
			return fmt.Errorf("edit workspace path: %w; select a file instead", ErrIsDirectory)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("edit workspace path: %w; select a regular file", workspace.ErrNotRegularFile)
		}
		old, err := editReadCompleteFile(ctx, parent, targetName, t.maxWriteBytes)
		if err != nil {
			return err
		}
		hash := sha256.Sum256(old)
		if err := t.ledger.Verify(call.SessionID, relative, int64(len(old)), hash); err != nil {
			return editLedgerError(err)
		}
		replacement, err = textmatch.Apply(textmatch.Request{Content: string(old), Old: args.OldText, New: args.NewText, ReplaceAll: args.ReplaceAll})
		if err != nil {
			return editMatchError(err, args.OldText)
		}
		data := []byte(replacement.Content)
		if int64(len(data)) > t.maxWriteBytes {
			return editTooLargeError(len(data), t.maxWriteBytes)
		}
		diff = textmatch.UnifiedDiff(string(old), replacement.Content, relative, 3)
		if err := atomicReplaceFile(ctx, parent, targetName, data, info.Mode().Perm(), true); err != nil {
			return err
		}
		t.ledger.RecordWrite(call.SessionID, relative, int64(len(data)), sha256.Sum256(data))
		return nil
	})
	if err != nil {
		return result, err
	}
	content, report := applyWriteOutput(diff, t.output, reservation.Grant)
	encoded, err := resultObject(EditResult{Path: relative, Replacements: replacement.Replacements, MatchTier: replacement.Tier.String(), Diff: content, Truncated: report.Truncated, Report: report})
	if err != nil {
		return result, fmt.Errorf("encode edit result: %w; retry the edit", err)
	}
	result.Content = encoded
	emitted = len(content)
	t.touch.Publish(ctx, workspace.Touch{SessionID: call.SessionID, Path: relative, Kind: workspace.TouchEdit, Content: append([]byte(nil), []byte(replacement.Content)...)})
	return result, nil
}

func decodeEditArgs(raw map[string]any) (EditArgs, error) {
	object, err := decodeArgumentObject(raw)
	if err != nil {
		return EditArgs{}, fmt.Errorf("edit arguments: %w; provide a JSON object matching the edit schema", err)
	}
	for key := range object {
		switch key {
		case "path", "old_text", "new_text", "replace_all":
		default:
			return EditArgs{}, fmt.Errorf("edit arguments: unknown argument %q; remove unsupported arguments and retry", key)
		}
	}
	path, ok := object["path"].(string)
	if !ok {
		return EditArgs{}, errors.New("edit arguments: path is required and must be a string; provide a workspace-relative file path")
	}
	if path == "" {
		return EditArgs{}, errors.New("edit arguments: path must not be empty; provide a workspace-relative file path")
	}
	oldText, ok := object["old_text"].(string)
	if !ok {
		return EditArgs{}, errors.New("edit arguments: old_text is required and must be a string; provide the text previously read")
	}
	if oldText == "" {
		return EditArgs{}, fmt.Errorf("edit arguments: %w; old_text must be non-empty", textmatch.ErrEmptyOld)
	}
	newText, ok := object["new_text"].(string)
	if !ok {
		return EditArgs{}, errors.New("edit arguments: new_text is required and must be a string; use an empty string to delete the match")
	}
	replaceAll := false
	if value, exists := object["replace_all"]; exists {
		var valid bool
		replaceAll, valid = value.(bool)
		if !valid {
			return EditArgs{}, errors.New("edit arguments: replace_all must be a boolean; omit it or provide true or false")
		}
	}
	return EditArgs{Path: path, OldText: oldText, NewText: newText, ReplaceAll: replaceAll}, nil
}

func editContextError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("edit cancelled: %w; retry with an active context", err)
	}
	return nil
}

func editResolveError(err error) error {
	switch {
	case errors.Is(err, workspace.ErrOutsideRoot):
		return fmt.Errorf("edit workspace path: %w; use a workspace-relative path inside the working directory", ErrPathOutsideRoot)
	case errors.Is(err, workspace.ErrNotFound):
		return fmt.Errorf("edit workspace path: %w; provide an existing regular file", ErrFileNotFound)
	default:
		return fmt.Errorf("edit workspace path: %w; provide an existing regular file", err)
	}
}

func editFilesystemError(operation string, err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s edit workspace file: %w; provide an existing regular file", operation, ErrFileNotFound)
	}
	return fmt.Errorf("%s edit workspace file: %w; verify the file is readable and retry", operation, err)
}

func editLedgerError(err error) error {
	if errors.Is(err, workspace.ErrNeverRead) {
		return fmt.Errorf("edit workspace file: %w; read the file again before editing it", ErrNeverRead)
	}
	if errors.Is(err, workspace.ErrStaleRead) {
		return fmt.Errorf("edit workspace file: %w; the file changed, so read it again before editing", ErrStaleRead)
	}
	return fmt.Errorf("verify edit workspace file: %w; read the file again and retry", err)
}

func editMatchError(err error, oldText string) error {
	var ambiguity *textmatch.AmbiguityError
	switch {
	case errors.As(err, &ambiguity):
		return fmt.Errorf("edit workspace file: %w; matching lines %v; add context or set replace_all", ambiguity, ambiguity.Lines)
	case errors.Is(err, textmatch.ErrNoMatch):
		return fmt.Errorf("edit workspace file: %w for %q; re-read the file and retry", ErrNoMatch, editErrorQuote(oldText))
	case errors.Is(err, textmatch.ErrEmptyOld):
		return fmt.Errorf("edit arguments: %w; old_text must be non-empty", textmatch.ErrEmptyOld)
	case errors.Is(err, textmatch.ErrNoOpEdit):
		return fmt.Errorf("edit arguments: %w; old_text and new_text must differ", textmatch.ErrNoOpEdit)
	default:
		return fmt.Errorf("apply edit: %w; re-read the file and retry", err)
	}
}

func editErrorQuote(value string) string {
	if len(value) <= 200 {
		return value
	}
	end := 200
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end]
}

func editTooLargeError(size int, limit int64) error {
	return fmt.Errorf("edit workspace file: %w (size %d bytes, limit %d bytes); make a smaller edit", ErrFileTooLarge, size, limit)
}

func editReadCompleteFile(ctx context.Context, parent *os.Root, name string, maxBytes int64) ([]byte, error) {
	file, err := parent.Open(name)
	if err != nil {
		return nil, editFilesystemError("open", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, editFilesystemError("read", err)
	}
	if err := editContextError(ctx); err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, editTooLargeError(len(data), maxBytes)
	}
	return data, nil
}
