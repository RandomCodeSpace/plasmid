package codingtools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	adktool "google.golang.org/adk/v2/tool"

	"github.com/plasmid-dev/plasmid/outputlimit"
	"github.com/plasmid-dev/plasmid/shellexec"
	"github.com/plasmid-dev/plasmid/warning"
	"github.com/plasmid-dev/plasmid/workspace"
)

const defaultMaxReadBytes int64 = 5 << 20

// Config contains the shared dependencies and limits used by coding tools.
type Config struct {
	Root               *workspace.Root
	Queue              *workspace.MutationQueue
	Ledger             *workspace.Ledger
	Touch              *workspace.TouchBus
	Shell              *shellexec.Executor
	Output             outputlimit.Policy
	Budget             *outputlimit.Budget
	Logger             *slog.Logger
	WarningSink        warning.Sink
	MaxReadBytes       int64
	MaxWriteBytes      int64
	MaxGrepFileBytes   int64
	MaxTouchEvents     int
	DefaultBashTimeout time.Duration
}

// readHandler reads bounded, numbered text windows behind the native ADK tool.
type readHandler struct {
	root         *workspace.Root
	ledger       *workspace.Ledger
	touch        *workspace.TouchBus
	output       outputlimit.Policy
	budget       *outputlimit.Budget
	maxReadBytes int64
}

// NewReadTool validates the read dependencies and constructs a read tool.
func NewReadTool(cfg Config) (adktool.Tool, error) {
	handler, err := newReadHandler(cfg)
	if err != nil {
		return nil, err
	}
	return newNativeTool("read", ReadDescription, ReadInputSchema(), handler.call)
}

func newReadHandler(cfg Config) (*readHandler, error) {
	if cfg.Root == nil {
		return nil, errors.New("construct read tool: workspace root is required; provide the harness workspace root")
	}
	if cfg.Ledger == nil {
		return nil, errors.New("construct read tool: workspace ledger is required; provide the shared workspace ledger")
	}
	if cfg.Touch == nil {
		return nil, errors.New("construct read tool: touch bus is required; provide the shared workspace touch bus")
	}
	if cfg.Budget == nil {
		return nil, errors.New("construct read tool: output budget is required; provide the shared session budget")
	}
	if cfg.MaxReadBytes <= 0 {
		cfg.MaxReadBytes = defaultMaxReadBytes
	}
	if cfg.Output == (outputlimit.Policy{}) {
		cfg.Output = outputlimit.Defaults()
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if _, err := outputlimit.NewWriter(cfg.Output); err != nil {
		return nil, fmt.Errorf("construct read tool: invalid output policy: %w; provide non-negative output limits", err)
	}
	if cfg.Output.MaxLines <= 0 {
		return nil, errors.New("construct read tool: output max lines must be positive; provide a positive default read limit")
	}
	return &readHandler{
		root:         cfg.Root,
		ledger:       cfg.Ledger,
		touch:        cfg.Touch,
		output:       cfg.Output,
		budget:       cfg.Budget,
		maxReadBytes: cfg.MaxReadBytes,
	}, nil
}

// call reads and renders one workspace file window.
func (t *readHandler) call(ctx context.Context, sessionID string, args ReadArgs) (result map[string]any, err error) {
	reservation := t.budget.Reserve(sessionID, t.output.MaxBytes)
	emitted := 0
	defer func() { t.budget.Consume(sessionID, reservation.ID, emitted) }()

	if err := contextError(ctx); err != nil {
		return result, err
	}
	if args.Path == "" {
		return result, errors.New("read arguments: path must not be empty; provide a workspace-relative text file path")
	}
	if args.Offset == 0 {
		args.Offset = 1
	}
	if args.Limit == 0 {
		args.Limit = t.output.MaxLines
	}
	snapshot, err := t.loadReadFile(ctx, args.Path)
	if err != nil {
		return result, err
	}
	readResult, err := t.renderReadResult(ctx, snapshot, args, reservation.Grant)
	if err != nil {
		return result, err
	}
	contentObject := resultObject(readResult)
	if err := contextError(ctx); err != nil {
		return result, err
	}
	if err := verifyOpenedPath(t.root, snapshot.absolute, snapshot.info); err != nil {
		return result, err
	}
	if err := contextError(ctx); err != nil {
		return result, err
	}

	t.ledger.RecordRead(sessionID, snapshot.relative, int64(len(snapshot.data)), sha256.Sum256(snapshot.data))
	t.touch.Publish(ctx, workspace.Touch{SessionID: sessionID, InvocationID: invocationID(ctx), Path: snapshot.relative, Kind: workspace.TouchRead})
	emitted = len(readResult.Content)
	return contentObject, nil
}

type readFileSnapshot struct {
	absolute string
	relative string
	data     []byte
	info     os.FileInfo
}

func (t *readHandler) loadReadFile(ctx context.Context, path string) (readFileSnapshot, error) {
	absolute, err := t.root.ResolveExisting(path)
	if err != nil {
		return readFileSnapshot{}, readResolveError(err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return readFileSnapshot{}, readFilesystemError("stat", err)
	}
	if info.IsDir() {
		return readFileSnapshot{}, fmt.Errorf("read workspace path: %w; use ls to inspect the directory", ErrIsDirectory)
	}
	if !info.Mode().IsRegular() {
		return readFileSnapshot{}, fmt.Errorf("read workspace path: %w; select a regular text file", workspace.ErrNotRegularFile)
	}
	if info.Size() > t.maxReadBytes {
		return readFileSnapshot{}, fmt.Errorf("read workspace path: %w (size %d bytes, limit %d bytes); select a smaller file or split it", ErrFileTooLarge, info.Size(), t.maxReadBytes)
	}
	data, openedInfo, err := readCompleteFile(ctx, absolute, t.maxReadBytes)
	if err != nil {
		return readFileSnapshot{}, err
	}
	if err := verifyOpenedPath(t.root, absolute, openedInfo); err != nil {
		return readFileSnapshot{}, err
	}
	if err := contextError(ctx); err != nil {
		return readFileSnapshot{}, err
	}
	if isBinaryText(data) {
		return readFileSnapshot{}, fmt.Errorf("read workspace path: %w; the read tool accepts UTF-8 text only", ErrBinaryFile)
	}
	if err := contextError(ctx); err != nil {
		return readFileSnapshot{}, err
	}
	return readFileSnapshot{absolute: absolute, relative: t.root.Rel(absolute), data: data, info: openedInfo}, nil
}

func (t *readHandler) renderReadResult(ctx context.Context, snapshot readFileSnapshot, args ReadArgs, grant int) (ReadResult, error) {
	lines := splitReadLines(snapshot.data)
	window, startLine, endLine := selectReadWindow(lines, args.Offset, args.Limit)
	rendered, err := renderReadWindow(ctx, window, startLine)
	if err != nil {
		return ReadResult{}, err
	}
	content, report := applyReadOutput(rendered, t.output, grant)
	return ReadResult{
		Path: snapshot.relative, Content: content, StartLine: startLine, EndLine: endLine, TotalLines: len(lines),
		Truncated: endLine > 0 && endLine < len(lines) || report.Truncated, Report: report,
	}, nil
}

func resultObject(value any) map[string]any {
	encoded, _ := json.Marshal(value)
	var object map[string]any
	_ = json.Unmarshal(encoded, &object)
	return object
}

func contextError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("read cancelled: %w; retry with an active context", err)
	}
	return nil
}

func readResolveError(err error) error {
	switch {
	case errors.Is(err, workspace.ErrOutsideRoot):
		return fmt.Errorf("read workspace path: %w; use a workspace-relative path inside the working directory", ErrPathOutsideRoot)
	case errors.Is(err, workspace.ErrNotFound):
		return fmt.Errorf("read workspace path: %w; verify the path or use ls to inspect the working directory", ErrFileNotFound)
	default:
		return fmt.Errorf("read workspace path: %w; verify the path is readable and retry", err)
	}
}

func readFilesystemError(operation string, err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s workspace file: %w; verify the path or use ls to inspect the working directory", operation, ErrFileNotFound)
	}
	if pathError := new(os.PathError); errors.As(err, &pathError) {
		err = pathError.Err
	}
	return fmt.Errorf("%s workspace file: %w; verify the file is readable and retry", operation, err)
}

func readCompleteFile(ctx context.Context, path string, maxBytes int64) ([]byte, os.FileInfo, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, readFilesystemError("open", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, nil, readFilesystemError("stat opened", err)
	}
	if err := validateOpenedReadFile(info, maxBytes); err != nil {
		return nil, nil, err
	}
	data, err := readOpenedFile(ctx, file, info.Size(), maxBytes)
	if err != nil {
		return nil, nil, err
	}
	return data, info, nil
}

func validateOpenedReadFile(info os.FileInfo, maxBytes int64) error {
	if info.IsDir() {
		return fmt.Errorf("read opened workspace path: %w; use ls to inspect the directory", ErrIsDirectory)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("read opened workspace path: %w; select a regular text file", workspace.ErrNotRegularFile)
	}
	if info.Size() > maxBytes {
		return fmt.Errorf("read opened workspace path: %w (size %d bytes, limit %d bytes); select a smaller file or split it", ErrFileTooLarge, info.Size(), maxBytes)
	}
	return nil
}

func readOpenedFile(ctx context.Context, file *os.File, size, maxBytes int64) ([]byte, error) {
	capacity := size
	if capacity > 64<<10 {
		capacity = 64 << 10
	}
	data := make([]byte, 0, int(capacity))
	buffer := make([]byte, 32<<10)
	var total int64
	for {
		if err := contextError(ctx); err != nil {
			return nil, err
		}
		count, readErr := file.Read(buffer)
		if count > 0 {
			if int64(count) > maxBytes-total {
				return nil, fmt.Errorf("read opened workspace path: %w (limit %d bytes); select a smaller file or split it", ErrFileTooLarge, maxBytes)
			}
			data = append(data, buffer[:count]...)
			total += int64(count)
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return nil, readFilesystemError("read", readErr)
		}
	}
	return data, nil
}

func verifyOpenedPath(root *workspace.Root, path string, opened os.FileInfo) error {
	current, err := root.ResolveExisting(path)
	if err != nil {
		return readResolveError(err)
	}
	if current != path {
		return errors.New("read workspace file: path changed during the read; retry after filesystem changes settle")
	}
	currentInfo, err := os.Stat(current)
	if err != nil {
		return readFilesystemError("verify", err)
	}
	if !os.SameFile(opened, currentInfo) {
		return errors.New("read workspace file: path changed during the read; retry after filesystem changes settle")
	}
	return nil
}

func isBinaryText(data []byte) bool {
	prefix := len(data)
	if prefix > 8000 {
		prefix = 8000
	}
	if bytes.IndexByte(data[:prefix], 0) >= 0 || !utf8.Valid(data) {
		return true
	}
	total := 0
	nonPrintable := 0
	for len(data) > 0 {
		r, size := utf8.DecodeRune(data)
		data = data[size:]
		total++
		if r != '\t' && r != '\n' && r != '\r' && !unicode.IsPrint(r) {
			nonPrintable++
		}
	}
	return total > 0 && nonPrintable*10 > total*3
}

type readLine struct {
	body       string
	terminated bool
}

func splitReadLines(data []byte) []readLine {
	if len(data) == 0 {
		return nil
	}
	lines := make([]readLine, 0, bytes.Count(data, []byte{'\n'})+1)
	start := 0
	for index, value := range data {
		if value != '\n' {
			continue
		}
		end := index
		if end > start && data[end-1] == '\r' {
			end--
		}
		lines = append(lines, readLine{body: string(data[start:end]), terminated: true})
		start = index + 1
	}
	if start < len(data) {
		lines = append(lines, readLine{body: string(data[start:])})
	}
	return lines
}

func selectReadWindow(lines []readLine, offset, limit int) ([]readLine, int, int) {
	if len(lines) == 0 || offset > len(lines) {
		return nil, 0, 0
	}
	startIndex := offset - 1
	count := len(lines) - startIndex
	if limit < count {
		count = limit
	}
	endIndex := startIndex + count
	return lines[startIndex:endIndex], offset, endIndex
}

func renderReadWindow(ctx context.Context, lines []readLine, firstLine int) (string, error) {
	if len(lines) == 0 {
		return "", nil
	}
	var output strings.Builder
	for index, line := range lines {
		if err := contextError(ctx); err != nil {
			return "", err
		}
		_, _ = fmt.Fprintf(&output, "%6d\t%s", firstLine+index, line.body)
		if index+1 < len(lines) || line.terminated {
			output.WriteByte('\n')
		}
	}
	return output.String(), nil
}

func applyReadOutput(rendered string, configured outputlimit.Policy, grant int) (string, outputlimit.Report) {
	if rendered == "" {
		return "", outputlimit.Report{}
	}
	if grant == 0 {
		_, original := (outputlimit.Policy{}).Apply(rendered)
		report := outputlimit.Report{
			Truncated:     true,
			Reason:        outputlimit.ReasonBudget,
			OriginalBytes: original.OriginalBytes,
			OriginalLines: original.OriginalLines,
		}
		content := outputlimit.Marker(report.Reason, 0, report.OriginalBytes, 0, report.OriginalLines)
		if strings.HasSuffix(rendered, "\n") {
			content += "\n"
		}
		return content, report
	}
	policy := configured
	budgetLimited := policy.MaxBytes <= 0 || grant < policy.MaxBytes
	if budgetLimited {
		policy.MaxBytes = grant
	}
	content, report := policy.Apply(rendered)
	if budgetLimited && report.Truncated && report.Reason == outputlimit.ReasonBytes {
		oldMarker := outputlimit.Marker(outputlimit.ReasonBytes, report.KeptBytes, report.OriginalBytes, report.KeptLines, report.OriginalLines)
		report.Reason = outputlimit.ReasonBudget
		newMarker := outputlimit.Marker(outputlimit.ReasonBudget, report.KeptBytes, report.OriginalBytes, report.KeptLines, report.OriginalLines)
		content = strings.Replace(content, oldMarker, newMarker, 1)
	}
	return content, report
}
