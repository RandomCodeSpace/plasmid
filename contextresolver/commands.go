package contextresolver

import (
	"context"
	"strings"
	"time"

	"github.com/plasmid-dev/plasmid/config"
	"github.com/plasmid-dev/plasmid/internal/syntax"
	"github.com/plasmid-dev/plasmid/outputlimit"
	"github.com/plasmid-dev/plasmid/shellexec"
	"github.com/plasmid-dev/plasmid/warning"
)

func expandCommands(ctx context.Context, source, path string, trust TrustLevel, options commandOptions, executor *shellexec.Executor, sink warning.Sink) string {
	return expandCommandsWithBudget(ctx, source, path, trust, options, executor, sink, newCommandDocumentBudget(options))
}

type commandDocumentBudget struct {
	remaining    time.Duration
	timeoutBound bool
	outputBytes  int
}

func newCommandDocumentBudget(options commandOptions) *commandDocumentBudget {
	return &commandDocumentBudget{remaining: options.DocumentTimeout, timeoutBound: options.DocumentTimeout > 0}
}

func expandCommandsWithBudget(ctx context.Context, source, path string, trust TrustLevel, options commandOptions, executor *shellexec.Executor, sink warning.Sink, budget *commandDocumentBudget) string {
	if sink == nil {
		sink = warning.DiscardSink{}
	}
	if budget == nil {
		budget = newCommandDocumentBudget(options)
	}
	directives := syntax.ScanCommandDirectives(source)
	if len(directives) == 0 {
		return source
	}
	allowed := options.Mode == config.PromptCommandsOn || options.Mode == config.PromptCommandsTrusted && trust != TrustUntrusted
	var output strings.Builder
	cursor := 0
	for _, directive := range directives {
		output.WriteString(source[cursor:directive.Start])
		cursor = directive.End
		if !allowed || executor == nil {
			sink.Warn(warning.Warning{Code: warning.WarnSyntaxExecDisabled, Source: "syntax", Path: path, Line: directive.Line, Message: "prompt command execution is disabled for this source"})
			continue
		}
		if options.DocumentOutputBytes > 0 && budget.outputBytes >= options.DocumentOutputBytes {
			sink.Warn(warning.Warning{Code: warning.WarnSyntaxExecBudgetExhausted, Source: "syntax", Path: path, Line: directive.Line, Message: "prompt command document output budget is exhausted"})
			continue
		}
		if budget.timeoutBound && budget.remaining <= 0 {
			sink.Warn(warning.Warning{Code: warning.WarnSyntaxExecTimeout, Source: "syntax", Path: path, Line: directive.Line, Message: "prompt command document timeout is exhausted"})
			continue
		}
		runContext := ctx
		cancel := func() {}
		if budget.timeoutBound {
			runContext, cancel = context.WithTimeout(ctx, budget.remaining)
		}
		started := time.Now()
		result, err := executor.RunMerged(runContext, shellexec.Request{Command: directive.Command, Timeout: options.CommandTimeout})
		if budget.timeoutBound {
			budget.remaining -= time.Since(started)
		}
		runContextErr := runContext.Err()
		cancel()
		if err != nil {
			code := warning.WarnSyntaxExecFailed
			if runContextErr != nil {
				code = warning.WarnSyntaxExecTimeout
			}
			sink.Warn(warning.Warning{Code: code, Source: "syntax", Path: path, Line: directive.Line, Message: "prompt command execution failed"})
			continue
		}
		if result.TimedOut {
			sink.Warn(warning.Warning{Code: warning.WarnSyntaxExecTimeout, Source: "syntax", Path: path, Line: directive.Line, Message: "prompt command timed out"})
		}
		if result.ExitCode != 0 {
			sink.Warn(warning.Warning{Code: warning.WarnSyntaxExecFailed, Source: "syntax", Path: path, Line: directive.Line, Message: "prompt command exited unsuccessfully"})
		}
		limit := options.CommandOutputBytes
		if options.DocumentOutputBytes > 0 && (limit <= 0 || options.DocumentOutputBytes-budget.outputBytes < limit) {
			limit = options.DocumentOutputBytes - budget.outputBytes
		}
		rendered, report := (outputlimit.Policy{MaxBytes: limit, MaxLines: 2000, MaxLineBytes: 2000, HeadFraction: 0.6}).Apply(result.Stdout)
		if report.Truncated {
			sink.Warn(warning.Warning{Code: warning.WarnSyntaxExecBudgetExhausted, Source: "syntax", Path: path, Line: directive.Line, Message: "prompt command output was truncated"})
		}
		budget.outputBytes += len(rendered)
		output.WriteString(rendered)
	}
	output.WriteString(source[cursor:])
	return output.String()
}
