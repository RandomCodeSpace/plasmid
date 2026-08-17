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
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/plasmid-dev/plasmid/loop"
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
	DefaultBashTimeout time.Duration
}

// ReadTool reads bounded, numbered text windows without importing a provider.
type ReadTool struct {
	root         *workspace.Root
	ledger       *workspace.Ledger
	touch        *workspace.TouchBus
	output       outputlimit.Policy
	budget       *outputlimit.Budget
	maxReadBytes int64
}

var _ loop.Tool = (*ReadTool)(nil)

// NewReadTool validates the read dependencies and constructs a read tool.
func NewReadTool(cfg Config) (loop.Tool, error) {
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
	return &ReadTool{
		root:         cfg.Root,
		ledger:       cfg.Ledger,
		touch:        cfg.Touch,
		output:       cfg.Output,
		budget:       cfg.Budget,
		maxReadBytes: cfg.MaxReadBytes,
	}, nil
}

// Name returns the stable wire name.
func (*ReadTool) Name() string { return "read" }

// Description returns the frozen model-facing description.
func (*ReadTool) Description() string { return ReadDescription }

// InputSchema returns an independent copy of the frozen read schema.
func (*ReadTool) InputSchema() json.RawMessage { return ReadInputSchema() }

// Call reads and renders one workspace file window.
func (t *ReadTool) Call(ctx context.Context, call loop.ToolCall) (result loop.ToolResult, err error) {
	result.CallID = call.ID
	reservation := t.budget.Reserve(call.SessionID, t.output.MaxBytes)
	emitted := 0
	defer func() { t.budget.Consume(call.SessionID, reservation.ID, emitted) }()

	if err := contextError(ctx); err != nil {
		return result, err
	}
	args, err := decodeReadArgs(call.Args, t.output.MaxLines)
	if err != nil {
		return result, err
	}
	absolute, err := t.root.ResolveExisting(args.Path)
	if err != nil {
		return result, readResolveError(err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return result, readFilesystemError("stat", err)
	}
	if info.IsDir() {
		return result, fmt.Errorf("read workspace path: %w; use ls to inspect the directory", ErrIsDirectory)
	}
	if !info.Mode().IsRegular() {
		return result, fmt.Errorf("read workspace path: %w; select a regular text file", workspace.ErrNotRegularFile)
	}
	if info.Size() > t.maxReadBytes {
		return result, fmt.Errorf("read workspace path: %w (size %d bytes, limit %d bytes); select a smaller file or split it", ErrFileTooLarge, info.Size(), t.maxReadBytes)
	}
	if err := contextError(ctx); err != nil {
		return result, err
	}

	data, openedInfo, err := readCompleteFile(ctx, absolute, t.maxReadBytes)
	if err != nil {
		return result, err
	}
	if err := verifyOpenedPath(t.root, absolute, openedInfo); err != nil {
		return result, err
	}
	if err := contextError(ctx); err != nil {
		return result, err
	}
	if isBinaryText(data) {
		return result, fmt.Errorf("read workspace path: %w; the read tool accepts UTF-8 text only", ErrBinaryFile)
	}
	if err := contextError(ctx); err != nil {
		return result, err
	}
	hash := sha256.Sum256(data)
	lines := splitReadLines(data)
	window, startLine, endLine := selectReadWindow(lines, args.Offset, args.Limit)
	rendered, err := renderReadWindow(ctx, window, startLine)
	if err != nil {
		return result, err
	}
	content, report := applyReadOutput(rendered, t.output, reservation.Grant)
	windowTruncated := endLine > 0 && endLine < len(lines)
	relative := t.root.Rel(absolute)
	if filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, "../") {
		return result, errors.New("read workspace path: could not form a safe relative result path; select a path inside the working directory")
	}
	readResult := ReadResult{
		Path:       relative,
		Content:    content,
		StartLine:  startLine,
		EndLine:    endLine,
		TotalLines: len(lines),
		Truncated:  windowTruncated || report.Truncated,
		Report:     report,
	}
	contentObject, err := resultObject(readResult)
	if err != nil {
		return result, fmt.Errorf("encode read result: %w; retry the read", err)
	}
	if err := contextError(ctx); err != nil {
		return result, err
	}
	if err := verifyOpenedPath(t.root, absolute, openedInfo); err != nil {
		return result, err
	}
	if err := contextError(ctx); err != nil {
		return result, err
	}

	t.ledger.RecordRead(call.SessionID, relative, int64(len(data)), hash)
	t.touch.Publish(ctx, workspace.Touch{SessionID: call.SessionID, Path: relative, Kind: workspace.TouchRead})
	result.Content = contentObject
	emitted = len(content)
	return result, nil
}

func decodeReadArgs(raw map[string]any, defaultLimit int) (ReadArgs, error) {
	object, err := decodeArgumentObject(raw)
	if err != nil {
		return ReadArgs{}, fmt.Errorf("read arguments: %w; provide a JSON object matching the read schema", err)
	}
	for key := range object {
		switch key {
		case "path", "offset", "limit":
		default:
			return ReadArgs{}, fmt.Errorf("read arguments: unknown argument %q; remove unsupported arguments and retry", key)
		}
	}
	pathValue, exists := object["path"]
	if !exists {
		return ReadArgs{}, errors.New("read arguments: path is required; provide a workspace-relative text file path")
	}
	path, ok := pathValue.(string)
	if !ok {
		return ReadArgs{}, errors.New("read arguments: path must be a string; provide a workspace-relative text file path")
	}
	if path == "" {
		return ReadArgs{}, errors.New("read arguments: path must not be empty; provide a workspace-relative text file path")
	}
	offset, err := integerArgument(object, "offset", 1)
	if err != nil {
		return ReadArgs{}, fmt.Errorf("read arguments: %w; provide offset as a positive JSON integer", err)
	}
	limit, err := integerArgument(object, "limit", defaultLimit)
	if err != nil {
		return ReadArgs{}, fmt.Errorf("read arguments: %w; provide limit as a positive JSON integer", err)
	}
	if offset < 1 {
		return ReadArgs{}, errors.New("read arguments: offset must be at least 1; provide a one-based source line")
	}
	if limit < 1 {
		return ReadArgs{}, errors.New("read arguments: limit must be at least 1; provide a positive source-line limit")
	}
	return ReadArgs{Path: path, Offset: offset, Limit: limit}, nil
}

func decodeArgumentObject(raw map[string]any) (map[string]any, error) {
	if raw == nil {
		return nil, errors.New("arguments must be an object")
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("arguments are not valid JSON: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("decode arguments: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, err
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("arguments must be an object")
	}
	return object, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("arguments contain more than one JSON value")
		}
		return fmt.Errorf("decode trailing arguments: %w", err)
	}
	return nil
}

func integerArgument(object map[string]any, name string, fallback int) (int, error) {
	value, exists := object[name]
	if !exists {
		return fallback, nil
	}
	number, ok := value.(json.Number)
	if !ok {
		return 0, fmt.Errorf("%s must be a JSON integer", name)
	}
	parsed, err := strconv.ParseInt(number.String(), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be a JSON integer in the platform int range", name)
	}
	converted := int(parsed)
	if int64(converted) != parsed {
		return 0, fmt.Errorf("%s exceeds the platform int range", name)
	}
	return converted, nil
}

func resultObject(value any) (map[string]any, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var object map[string]any
	if err := json.Unmarshal(encoded, &object); err != nil {
		return nil, err
	}
	if object == nil {
		return nil, errors.New("result did not encode as a JSON object")
	}
	return object, nil
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
		return fmt.Errorf("%s workspace file: %w; verify the file is readable and retry", operation, pathError.Err)
	}
	return fmt.Errorf("%s workspace file: %w; verify the file is readable and retry", operation, err)
}

func readCompleteFile(ctx context.Context, path string, maxBytes int64) ([]byte, os.FileInfo, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, readFilesystemError("open", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, nil, readFilesystemError("stat opened", err)
	}
	if info.IsDir() {
		return nil, nil, fmt.Errorf("read opened workspace path: %w; use ls to inspect the directory", ErrIsDirectory)
	}
	if !info.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("read opened workspace path: %w; select a regular text file", workspace.ErrNotRegularFile)
	}
	if info.Size() > maxBytes {
		return nil, nil, fmt.Errorf("read opened workspace path: %w (size %d bytes, limit %d bytes); select a smaller file or split it", ErrFileTooLarge, info.Size(), maxBytes)
	}
	capacity := info.Size()
	if capacity > 64<<10 {
		capacity = 64 << 10
	}
	data := make([]byte, 0, int(capacity))
	buffer := make([]byte, 32<<10)
	var total int64
	for {
		if err := contextError(ctx); err != nil {
			return nil, nil, err
		}
		count, readErr := file.Read(buffer)
		if count > 0 {
			if int64(count) > maxBytes-total {
				return nil, nil, fmt.Errorf("read opened workspace path: %w (limit %d bytes); select a smaller file or split it", ErrFileTooLarge, maxBytes)
			}
			data = append(data, buffer[:count]...)
			total += int64(count)
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return nil, nil, readFilesystemError("read", readErr)
		}
		if count == 0 {
			return nil, nil, errors.New("read workspace file: reader made no progress; verify the file is a regular local file and retry")
		}
	}
	return data, info, nil
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
		output.WriteString(fmt.Sprintf("%6d\t%s", firstLine+index, line.body))
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
