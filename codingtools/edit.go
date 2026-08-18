package codingtools

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"unicode/utf8"

	"github.com/plasmid-dev/plasmid/codingtools/internal/textmatch"
	adktool "google.golang.org/adk/v2/tool"

	"github.com/plasmid-dev/plasmid/outputlimit"
	"github.com/plasmid-dev/plasmid/workspace"
)

// editHandler applies one deterministic replacement to a previously read file.
type editHandler struct {
	root          *workspace.Root
	queue         *workspace.MutationQueue
	ledger        *workspace.Ledger
	touch         *workspace.TouchBus
	output        outputlimit.Policy
	budget        *outputlimit.Budget
	maxWriteBytes int64
}

// NewEditTool validates the shared mutation dependencies and constructs an edit tool.
func NewEditTool(cfg Config) (adktool.Tool, error) {
	handler, err := newEditHandler(cfg)
	if err != nil {
		return nil, err
	}
	return newNativeTool("edit", EditDescription, EditInputSchema(), handler.call)
}

func newEditHandler(cfg Config) (*editHandler, error) {
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
	return &editHandler{root: cfg.Root, queue: cfg.Queue, ledger: cfg.Ledger, touch: cfg.Touch, output: cfg.Output, budget: cfg.Budget, maxWriteBytes: cfg.MaxWriteBytes}, nil
}

// call serializes the entire read-modify-write transition against all other mutations.
func (t *editHandler) call(ctx context.Context, sessionID string, args EditArgs) (result map[string]any, err error) {
	reservation := t.budget.Reserve(sessionID, t.output.MaxBytes)
	emitted := 0
	defer func() { t.budget.Consume(sessionID, reservation.ID, emitted) }()
	if err := editContextError(ctx); err != nil {
		return result, err
	}
	if args.Path == "" {
		return result, errors.New("edit arguments: path must not be empty; provide a workspace-relative file path")
	}
	if args.OldText == "" {
		return result, fmt.Errorf("edit arguments: %w; old_text must be non-empty", textmatch.ErrEmptyOld)
	}

	operation, err := t.performEdit(ctx, sessionID, args)
	if err != nil {
		return result, err
	}
	content, report := applyWriteOutput(operation.diff, t.output, reservation.Grant)
	encoded := resultObject(EditResult{Path: operation.relative, Replacements: operation.replacement.Replacements, MatchTier: operation.replacement.Tier.String(), Diff: content, Truncated: report.Truncated, Report: report})
	emitted = len(content)
	t.touch.Publish(ctx, workspace.Touch{SessionID: sessionID, InvocationID: invocationID(ctx), Path: operation.relative, Kind: workspace.TouchEdit, Content: []byte(operation.replacement.Content)})
	return encoded, nil
}

type editOperation struct {
	relative    string
	diff        string
	replacement textmatch.Result
}

func (t *editHandler) performEdit(ctx context.Context, sessionID string, args EditArgs) (editOperation, error) {
	var operation editOperation
	err := t.queue.Do(ctx, func() error {
		var err error
		operation, err = t.replaceFile(ctx, sessionID, args)
		return err
	})
	return operation, err
}

func (t *editHandler) replaceFile(ctx context.Context, sessionID string, args EditArgs) (editOperation, error) {
	if err := editContextError(ctx); err != nil {
		return editOperation{}, err
	}
	absolute, err := t.root.ResolveExisting(args.Path)
	if err != nil {
		return editOperation{}, editResolveError(err)
	}
	relative := t.root.Rel(absolute)
	secureRoot, err := os.OpenRoot(t.root.Dir())
	if err != nil {
		return editOperation{}, fmt.Errorf("open edit workspace root: %w; verify the workspace is accessible and retry", err)
	}
	defer func() { _ = secureRoot.Close() }()
	parent, err := secureRoot.OpenRoot(filepath.Dir(filepath.FromSlash(relative)))
	if err != nil {
		return editOperation{}, fmt.Errorf("open edit workspace parent: %w; verify the file is accessible and retry", err)
	}
	defer func() { _ = parent.Close() }()
	return t.replaceOpenedFile(ctx, sessionID, args, relative, parent)
}

func (t *editHandler) replaceOpenedFile(ctx context.Context, sessionID string, args EditArgs, relative string, parent *os.Root) (editOperation, error) {
	targetName := filepath.Base(relative)
	info, err := parent.Lstat(targetName)
	if err != nil {
		return editOperation{}, editFilesystemError("stat", err)
	}
	if info.IsDir() {
		return editOperation{}, fmt.Errorf("edit workspace path: %w; select a file instead", ErrIsDirectory)
	}
	if !info.Mode().IsRegular() {
		return editOperation{}, fmt.Errorf("edit workspace path: %w; select a regular file", workspace.ErrNotRegularFile)
	}
	old, err := editReadCompleteFile(ctx, parent, targetName, t.maxWriteBytes)
	if err != nil {
		return editOperation{}, err
	}
	if err := t.ledger.Verify(sessionID, relative, int64(len(old)), sha256.Sum256(old)); err != nil {
		return editOperation{}, editLedgerError(err)
	}
	replacement, err := textmatch.Apply(textmatch.Request{Content: string(old), Old: args.OldText, New: args.NewText, ReplaceAll: args.ReplaceAll})
	if err != nil {
		return editOperation{}, editMatchError(err, args.OldText)
	}
	data := []byte(replacement.Content)
	if int64(len(data)) > t.maxWriteBytes {
		return editOperation{}, editTooLargeError(len(data), t.maxWriteBytes)
	}
	if err := atomicReplaceFile(ctx, parent, targetName, data, info.Mode().Perm(), true); err != nil {
		return editOperation{}, err
	}
	t.ledger.RecordWrite(sessionID, relative, int64(len(data)), sha256.Sum256(data))
	return editOperation{relative: relative, diff: textmatch.UnifiedDiff(string(old), replacement.Content, relative, 3), replacement: replacement}, nil
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
	return fmt.Errorf("edit workspace file: %w; the file changed, so read it again before editing", err)
}

func editMatchError(err error, oldText string) error {
	var ambiguity *textmatch.AmbiguityError
	switch {
	case errors.As(err, &ambiguity):
		return fmt.Errorf("edit workspace file: %w; matching lines %v; add context or set replace_all", ambiguity, ambiguity.Lines)
	case errors.Is(err, textmatch.ErrNoMatch):
		return fmt.Errorf("edit workspace file: %w for %q; re-read the file and retry", ErrNoMatch, editErrorQuote(oldText))
	default:
		return fmt.Errorf("edit arguments: %w; old_text and new_text must differ", err)
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
	defer func() { _ = file.Close() }()
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
