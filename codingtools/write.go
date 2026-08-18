package codingtools

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/plasmid-dev/plasmid/codingtools/internal/textmatch"
	adktool "google.golang.org/adk/v2/tool"

	"github.com/plasmid-dev/plasmid/outputlimit"
	"github.com/plasmid-dev/plasmid/workspace"
)

const defaultMaxWriteBytes int64 = 5 << 20

// writeHandler atomically creates or replaces complete workspace files.
type writeHandler struct {
	root          *workspace.Root
	queue         *workspace.MutationQueue
	ledger        *workspace.Ledger
	touch         *workspace.TouchBus
	output        outputlimit.Policy
	budget        *outputlimit.Budget
	maxWriteBytes int64
}

// NewWriteTool validates the shared mutation dependencies and constructs a write tool.
func NewWriteTool(cfg Config) (adktool.Tool, error) {
	handler, err := newWriteHandler(cfg)
	if err != nil {
		return nil, err
	}
	return newNativeTool("write", WriteDescription, WriteInputSchema(), handler.call)
}

func newWriteHandler(cfg Config) (*writeHandler, error) {
	if cfg.Root == nil {
		return nil, errors.New("construct write tool: workspace root is required; provide the harness workspace root")
	}
	if cfg.Queue == nil {
		return nil, errors.New("construct write tool: mutation queue is required; provide the shared workspace mutation queue")
	}
	if cfg.Ledger == nil {
		return nil, errors.New("construct write tool: workspace ledger is required; provide the shared workspace ledger")
	}
	if cfg.Touch == nil {
		return nil, errors.New("construct write tool: touch bus is required; provide the shared workspace touch bus")
	}
	if cfg.Budget == nil {
		return nil, errors.New("construct write tool: output budget is required; provide the shared session budget")
	}
	if cfg.MaxWriteBytes <= 0 {
		cfg.MaxWriteBytes = defaultMaxWriteBytes
	}
	if cfg.Output == (outputlimit.Policy{}) {
		cfg.Output = outputlimit.Defaults()
	}
	if _, err := outputlimit.NewWriter(cfg.Output); err != nil {
		return nil, fmt.Errorf("construct write tool: invalid output policy: %w; provide non-negative output limits", err)
	}
	if cfg.Output.MaxLines <= 0 {
		return nil, errors.New("construct write tool: output max lines must be positive; provide a positive output limit")
	}
	return &writeHandler{root: cfg.Root, queue: cfg.Queue, ledger: cfg.Ledger, touch: cfg.Touch, output: cfg.Output, budget: cfg.Budget, maxWriteBytes: cfg.MaxWriteBytes}, nil
}

// call serializes verification and replacement so a successful check cannot be
// invalidated by another mutation before the rename.
func (t *writeHandler) call(ctx context.Context, sessionID string, args WriteArgs) (result map[string]any, err error) {
	reservation := t.budget.Reserve(sessionID, t.output.MaxBytes)
	emitted := 0
	defer func() { t.budget.Consume(sessionID, reservation.ID, emitted) }()
	if err := writeContextError(ctx); err != nil {
		return result, err
	}
	if args.Path == "" {
		return result, errors.New("write arguments: path must not be empty; provide a workspace-relative destination path")
	}
	if int64(len(args.Content)) > t.maxWriteBytes {
		return result, fmt.Errorf("write arguments: %w (content %d bytes, limit %d bytes); split the content into smaller writes", ErrFileTooLarge, len(args.Content), t.maxWriteBytes)
	}

	var relative string
	var diff string
	data := []byte(args.Content)
	err = t.queue.Do(ctx, func() error {
		if err := writeContextError(ctx); err != nil {
			return err
		}
		absolute, err := t.root.ResolveForWrite(args.Path)
		if err != nil {
			return writeResolveError(err)
		}
		relative = t.root.Rel(absolute)

		secureRoot, err := os.OpenRoot(t.root.Dir())
		if err != nil {
			return fmt.Errorf("open write workspace root: %w; verify the workspace is accessible and retry", err)
		}
		defer func() { _ = secureRoot.Close() }()
		parentPath := filepath.Dir(filepath.FromSlash(relative))
		if err := secureRoot.MkdirAll(parentPath, 0o755); err != nil {
			return fmt.Errorf("write workspace parent: %w; verify the destination directory is writable and retry", err)
		}
		parent, err := secureRoot.OpenRoot(parentPath)
		if err != nil {
			return fmt.Errorf("open write workspace parent: %w; verify the destination directory is writable and retry", err)
		}
		defer func() { _ = parent.Close() }()
		targetName := filepath.Base(relative)

		old, mode, exists, err := inspectWriteTarget(ctx, parent, targetName)
		if err != nil {
			return err
		}
		if exists {
			hash := sha256.Sum256(old)
			if err := t.ledger.Verify(sessionID, relative, int64(len(old)), hash); err != nil {
				return writeLedgerError(err)
			}
		}
		diff = writeUnifiedDiff(old, data, relative)
		if err := atomicReplaceFile(ctx, parent, targetName, data, mode, exists); err != nil {
			return err
		}
		t.ledger.RecordWrite(sessionID, relative, int64(len(data)), sha256.Sum256(data))
		return nil
	})
	if err != nil {
		return result, err
	}
	content, report := applyWriteOutput(diff, t.output, reservation.Grant)
	encoded := resultObject(WriteResult{Path: relative, BytesWritten: len(data), Diff: content, Truncated: report.Truncated, Report: report})
	emitted = len(content)
	// The bus is intentionally outside the queue. Observers never receive mutable caller storage.
	t.touch.Publish(ctx, workspace.Touch{SessionID: sessionID, InvocationID: invocationID(ctx), Path: relative, Kind: workspace.TouchWrite, Content: append([]byte(nil), data...)})
	return encoded, nil
}

// writeUnifiedDiff must first compare the bytes that will be replaced. The
// text diff intentionally normalizes BOM and line endings for edit matching,
// which would otherwise hide a successful byte-level write.
func writeUnifiedDiff(old, new []byte, path string) string {
	if bytes.Equal(old, new) {
		return ""
	}
	return textmatch.UnifiedDiffExact(string(old), string(new), path, 3)
}

func inspectWriteTarget(ctx context.Context, parent *os.Root, name string) ([]byte, os.FileMode, bool, error) {
	info, err := parent.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0o644, false, nil
	}
	if err != nil {
		return nil, 0, false, fmt.Errorf("inspect write target: %w; verify the path is accessible and retry", err)
	}
	if info.IsDir() {
		return nil, 0, false, fmt.Errorf("write workspace path: %w; choose a file path instead", ErrIsDirectory)
	}
	if !info.Mode().IsRegular() {
		return nil, 0, false, fmt.Errorf("write workspace path: %w; select a regular file", workspace.ErrNotRegularFile)
	}
	if err := writeContextError(ctx); err != nil {
		return nil, 0, false, err
	}
	file, err := parent.Open(name)
	if err != nil {
		return nil, 0, false, fmt.Errorf("open write target: %w; verify the file is readable and retry", err)
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, 0, false, fmt.Errorf("read write target: %w; verify the file is readable and retry", err)
	}
	if err := writeContextError(ctx); err != nil {
		return nil, 0, false, err
	}
	return data, info.Mode().Perm(), true, nil
}

// atomicReplaceFile is shared mutation machinery for write and edit. Its temp
// file stays beside the target, so a completed rename never exposes partial data.
func atomicReplaceFile(ctx context.Context, parent *os.Root, name string, data []byte, mode os.FileMode, exists bool) (err error) {
	return atomicReplaceFileWith(ctx, parent, name, data, mode, exists, atomicReplaceOptions{})
}

type atomicReplaceOptions struct {
	rename       func(string, string) error
	beforeRename func(string)
}

func atomicReplaceFileWith(ctx context.Context, parent *os.Root, name string, data []byte, mode os.FileMode, exists bool, options atomicReplaceOptions) (err error) {
	temp, tempName, err := createRootTemp(parent)
	if err != nil {
		return fmt.Errorf("create write temporary file: %w; verify the destination directory is writable and retry", err)
	}
	defer func() {
		if err != nil {
			_ = temp.Close()
			_ = parent.Remove(tempName)
		}
	}()
	_, writeErr := temp.Write(data)
	if writeErr != nil {
		err = writeErr
		return fmt.Errorf("write temporary file: %w; retry the write", err)
	}
	if err = temp.Sync(); err != nil {
		return fmt.Errorf("sync temporary file: %w; retry the write", err)
	}
	if !exists {
		mode = 0o644
	}
	if err = temp.Chmod(mode.Perm()); err != nil {
		return fmt.Errorf("set write file permissions: %w; retry the write", err)
	}
	if err = temp.Close(); err != nil {
		return fmt.Errorf("close temporary file: %w; retry the write", err)
	}
	if options.beforeRename != nil {
		options.beforeRename(tempName)
	}
	if err = writeContextError(ctx); err != nil {
		return err
	}
	if options.rename != nil {
		err = options.rename(tempName, name)
	} else {
		err = parent.Rename(tempName, name)
	}
	if err != nil {
		return fmt.Errorf("replace workspace file atomically: %w; retry after closing conflicting file handles", err)
	}
	return nil
}

func createRootTemp(parent *os.Root) (*os.File, string, error) {
	var random [16]byte
	for range 100 {
		if _, err := rand.Read(random[:]); err != nil {
			return nil, "", err
		}
		name := ".plasmid-write-" + hex.EncodeToString(random[:])
		file, err := parent.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if err == nil {
			return file, name, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, "", err
		}
	}
	return nil, "", errors.New("could not allocate a unique temporary file")
}

func writeContextError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("write cancelled: %w; retry with an active context", err)
	}
	return nil
}

func writeResolveError(err error) error {
	if errors.Is(err, workspace.ErrOutsideRoot) {
		return fmt.Errorf("write workspace path: %w; use a workspace-relative path inside the working directory", ErrPathOutsideRoot)
	}
	return fmt.Errorf("write workspace path: %w; verify the destination path and retry", err)
}

func writeLedgerError(err error) error {
	if errors.Is(err, workspace.ErrNeverRead) {
		return fmt.Errorf("write workspace file: %w; read the file again before replacing it", ErrNeverRead)
	}
	return fmt.Errorf("write workspace file: %w; read the file again because it changed on disk", err)
}

func applyWriteOutput(diff string, configured outputlimit.Policy, grant int) (string, outputlimit.Report) {
	content, report := applyReadOutput(diff, configured, grant)
	return content, report
}
