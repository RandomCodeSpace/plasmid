package codingtools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	adktool "google.golang.org/adk/v2/tool"

	"github.com/RandomCodeSpace/plasmid/outputlimit"
	"github.com/RandomCodeSpace/plasmid/shellexec"
	"github.com/RandomCodeSpace/plasmid/workspace"
)

const defaultBashTimeout = 120 * time.Second

// bashHandler runs fresh non-interactive shell commands through the shared
// executor. It deliberately does not publish workspace touches: bash has no
// reliable file-level mutation information to publish.
type bashHandler struct {
	shell          *shellexec.Executor
	output         outputlimit.Policy
	budget         *outputlimit.Budget
	defaultTimeout time.Duration
}

// NewBashTool validates the shared executor dependencies and constructs a
// bash tool.
func NewBashTool(cfg Config) (adktool.Tool, error) {
	handler, err := newBashHandler(cfg)
	if err != nil {
		return nil, err
	}
	return newNativeTool("bash", BashDescription, BashInputSchema(), handler.call)
}

func newBashHandler(cfg Config) (*bashHandler, error) {
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
	return &bashHandler{
		shell:          cfg.Shell,
		output:         cfg.Output,
		budget:         cfg.Budget,
		defaultTimeout: cfg.DefaultBashTimeout,
	}, nil
}

// call invokes the configured shell from a workspace-contained initial
// directory. Non-zero process exits are represented in BashResult, not as a
// tool error.
func (t *bashHandler) call(ctx context.Context, sessionID string, args BashArgs) (result map[string]any, err error) {
	reservation := t.budget.Reserve(sessionID, t.output.MaxBytes)
	emitted := 0
	defer func() { t.budget.Consume(sessionID, reservation.ID, emitted) }()

	if err := bashContextError(ctx); err != nil {
		return result, err
	}
	if strings.TrimSpace(args.Command) == "" {
		return result, errors.New("bash arguments: command must not be empty; provide a shell command")
	}
	if args.Dir == "" {
		args.Dir = "."
	}
	if args.TimeoutMS == 0 {
		args.TimeoutMS = int(t.defaultTimeout / time.Millisecond)
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
	encoded := resultObject(bashResult)
	emitted = len(bashResult.Stdout) + len(bashResult.Stderr)
	return encoded, nil
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
	budgetReport := outputlimit.Report{
		Truncated:     true,
		Reason:        outputlimit.ReasonBudget,
		OriginalBytes: prior.OriginalBytes,
		OriginalLines: prior.OriginalLines,
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
			report.OriginalBytes = max(report.OriginalBytes, prior.OriginalBytes)
			report.OriginalLines = max(report.OriginalLines, prior.OriginalLines)
			return content, report
		}
		decrement := len(content) - grant
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
