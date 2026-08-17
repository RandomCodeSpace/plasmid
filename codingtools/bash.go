package codingtools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/plasmid-dev/plasmid/loop"
	"github.com/plasmid-dev/plasmid/outputlimit"
	"github.com/plasmid-dev/plasmid/shellexec"
	"github.com/plasmid-dev/plasmid/workspace"
)

const defaultBashTimeout = 120 * time.Second

// BashTool runs fresh non-interactive shell commands through the shared
// executor. It deliberately does not publish workspace touches: bash has no
// reliable file-level mutation information to publish.
type BashTool struct {
	shell          *shellexec.Executor
	output         outputlimit.Policy
	budget         *outputlimit.Budget
	defaultTimeout time.Duration
}

var _ loop.Tool = (*BashTool)(nil)

// NewBashTool validates the shared executor dependencies and constructs a
// bash tool.
func NewBashTool(cfg Config) (loop.Tool, error) {
	if cfg.Root == nil {
		return nil, errors.New("construct bash tool: workspace root is required; provide the harness workspace root")
	}
	if cfg.Shell == nil {
		return nil, errors.New("construct bash tool: shell executor is required; provide the shared shell executor")
	}
	if cfg.Budget == nil {
		return nil, errors.New("construct bash tool: output budget is required; provide the shared session budget")
	}
	if cfg.Output == (outputlimit.Policy{}) {
		cfg.Output = outputlimit.Defaults()
	}
	if _, err := outputlimit.NewWriter(cfg.Output); err != nil {
		return nil, fmt.Errorf("construct bash tool: invalid output policy: %w; provide non-negative output limits", err)
	}
	if cfg.DefaultBashTimeout <= 0 {
		cfg.DefaultBashTimeout = defaultBashTimeout
	}
	return &BashTool{
		shell:          cfg.Shell,
		output:         cfg.Output,
		budget:         cfg.Budget,
		defaultTimeout: cfg.DefaultBashTimeout,
	}, nil
}

// Name returns the stable wire name.
func (*BashTool) Name() string { return "bash" }

// Description returns the frozen model-facing description.
func (*BashTool) Description() string { return BashDescription }

// InputSchema returns an independent copy of the frozen bash schema.
func (*BashTool) InputSchema() json.RawMessage { return BashInputSchema() }

// Call invokes the configured shell from a workspace-contained initial
// directory. Non-zero process exits are represented in BashResult, not as a
// tool error.
func (t *BashTool) Call(ctx context.Context, call loop.ToolCall) (result loop.ToolResult, err error) {
	result.CallID = call.ID
	reservation := t.budget.Reserve(call.SessionID, t.output.MaxBytes)
	emitted := 0
	defer func() { t.budget.Consume(call.SessionID, reservation.ID, emitted) }()

	if err := bashContextError(ctx); err != nil {
		return result, err
	}
	args, err := decodeBashArgs(call.Args, t.defaultTimeout)
	if err != nil {
		return result, err
	}
	if err := bashContextError(ctx); err != nil {
		return result, err
	}

	shellResult, err := t.shell.Run(ctx, shellexec.Request{
		Command: args.Command,
		Dir:     args.Dir,
		Timeout: bashTimeout(args.TimeoutMS),
	})
	if err != nil {
		return result, bashExecutorError(err)
	}

	bashResult := BashResult{
		Stdout:       shellResult.Stdout,
		Stderr:       shellResult.Stderr,
		ExitCode:     shellResult.ExitCode,
		Signal:       shellResult.Signal,
		TimedOut:     shellResult.TimedOut,
		Killed:       shellResult.Killed,
		Truncated:    shellResult.StdoutReport.Truncated || shellResult.StderrReport.Truncated,
		StdoutReport: shellResult.StdoutReport,
		StderrReport: shellResult.StderrReport,
	}
	if bashResult.TimedOut {
		bashResult.Stderr = fmt.Sprintf("[plasmid: command timed out after timeout_ms=%d]\n%s", args.TimeoutMS, bashResult.Stderr)
	}
	limitBashResult(&bashResult, reservation.Grant, t.output)
	encoded, err := resultObject(bashResult)
	if err != nil {
		return result, fmt.Errorf("encode bash result: %w; retry the command", err)
	}
	result.Content = encoded
	emitted = len(bashResult.Stdout) + len(bashResult.Stderr)
	return result, nil
}

func limitBashResult(result *BashResult, grant int, configured outputlimit.Policy) {
	if result == nil || len(result.Stdout)+len(result.Stderr) <= grant && grant > 0 {
		return
	}
	if grant <= 0 {
		result.Stdout, result.StdoutReport = limitBashText(result.Stdout, result.StdoutReport, 0, configured)
		result.Stderr, result.StderrReport = limitBashText(result.Stderr, result.StderrReport, 0, configured)
		result.Truncated = result.StdoutReport.Truncated || result.StderrReport.Truncated
		return
	}
	total := len(result.Stdout) + len(result.Stderr)
	stdoutGrant := 0
	if total > 0 {
		stdoutGrant = grant * len(result.Stdout) / total
	}
	stderrGrant := grant - stdoutGrant
	if result.Stdout != "" && stdoutGrant == 0 && stderrGrant > 0 {
		stdoutGrant++
		stderrGrant--
	}
	if result.Stderr != "" && stderrGrant == 0 && stdoutGrant > 0 {
		stderrGrant++
		stdoutGrant--
	}
	result.Stdout, result.StdoutReport = limitBashText(result.Stdout, result.StdoutReport, stdoutGrant, configured)
	result.Stderr, result.StderrReport = limitBashText(result.Stderr, result.StderrReport, stderrGrant, configured)
	result.Truncated = result.StdoutReport.Truncated || result.StderrReport.Truncated
}

func limitBashText(value string, prior outputlimit.Report, grant int, configured outputlimit.Policy) (string, outputlimit.Report) {
	if value == "" {
		return "", prior
	}
	if grant >= len(value) {
		return value, prior
	}
	original := prior
	if original.OriginalBytes == 0 {
		_, original = (outputlimit.Policy{}).Apply(value)
	}
	budgetReport := outputlimit.Report{
		Truncated:     true,
		Reason:        outputlimit.ReasonBudget,
		OriginalBytes: original.OriginalBytes,
		OriginalLines: original.OriginalLines,
	}
	if grant <= 0 {
		return "", budgetReport
	}

	policy := configured
	for sourceLimit := grant; sourceLimit > 0; {
		policy.MaxBytes = sourceLimit
		content, report := policy.Apply(value)
		if report.Truncated && report.Reason == outputlimit.ReasonBytes {
			oldMarker := outputlimit.Marker(outputlimit.ReasonBytes, report.KeptBytes, report.OriginalBytes, report.KeptLines, report.OriginalLines)
			report.Reason = outputlimit.ReasonBudget
			content = strings.Replace(content, oldMarker, outputlimit.Marker(outputlimit.ReasonBudget, report.KeptBytes, report.OriginalBytes, report.KeptLines, report.OriginalLines), 1)
		}
		if len(content) <= grant {
			report.OriginalBytes = max(report.OriginalBytes, original.OriginalBytes)
			report.OriginalLines = max(report.OriginalLines, original.OriginalLines)
			return content, report
		}
		decrement := len(content) - grant
		if decrement < 1 {
			decrement = 1
		}
		sourceLimit -= decrement
	}
	return "", budgetReport
}

func bashTimeout(milliseconds int) time.Duration {
	const maxDurationMilliseconds = int64(^uint64(0)>>1) / int64(time.Millisecond)
	if int64(milliseconds) > maxDurationMilliseconds {
		return time.Duration(^uint64(0) >> 1)
	}
	return time.Duration(milliseconds) * time.Millisecond
}

func decodeBashArgs(raw map[string]any, defaultTimeout time.Duration) (BashArgs, error) {
	object, err := decodeArgumentObject(raw)
	if err != nil {
		return BashArgs{}, fmt.Errorf("bash arguments: %w; provide a JSON object matching the bash schema", err)
	}
	for key := range object {
		switch key {
		case "command", "dir", "timeout_ms":
		default:
			return BashArgs{}, fmt.Errorf("bash arguments: unknown argument %q; remove unsupported arguments and retry", key)
		}
	}
	commandValue, exists := object["command"]
	if !exists {
		return BashArgs{}, errors.New("bash arguments: command is required; provide a non-empty shell command")
	}
	command, ok := commandValue.(string)
	if !ok {
		return BashArgs{}, errors.New("bash arguments: command must be a string; provide a non-empty shell command")
	}
	if strings.TrimSpace(command) == "" {
		return BashArgs{}, errors.New("bash arguments: command must not be empty; provide a shell command")
	}
	dir := "."
	if value, exists := object["dir"]; exists {
		var ok bool
		dir, ok = value.(string)
		if !ok {
			return BashArgs{}, errors.New("bash arguments: dir must be a string; provide a workspace-relative directory")
		}
		if dir == "" {
			dir = "."
		}
	}
	defaultMilliseconds := int(defaultTimeout / time.Millisecond)
	timeout, err := integerArgument(object, "timeout_ms", defaultMilliseconds)
	if err != nil {
		return BashArgs{}, fmt.Errorf("bash arguments: %w; provide timeout_ms as a positive JSON integer", err)
	}
	if timeout <= 0 {
		return BashArgs{}, errors.New("bash arguments: timeout_ms must be positive; provide a positive timeout in milliseconds")
	}
	return BashArgs{Command: command, Dir: dir, TimeoutMS: timeout}, nil
}

func bashContextError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("bash cancelled: %w; retry with an active context", err)
	}
	return nil
}

func bashExecutorError(err error) error {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("bash cancelled: %w; retry with an active context", err)
	case errors.Is(err, workspace.ErrOutsideRoot):
		return fmt.Errorf("bash directory: %w; use a workspace-relative directory inside the working directory", ErrPathOutsideRoot)
	case errors.Is(err, workspace.ErrNotFound):
		return fmt.Errorf("bash directory: %w; verify the directory or use ls to inspect the working directory", ErrFileNotFound)
	case errors.Is(err, workspace.ErrNotDirectory):
		return fmt.Errorf("bash directory: %w; select a workspace directory", workspace.ErrNotDirectory)
	default:
		return fmt.Errorf("run bash command: %w", err)
	}
}
