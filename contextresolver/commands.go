package contextresolver

import (
	"context"
	"strings"
	"time"

	"github.com/RandomCodeSpace/plasmid/config"
	"github.com/RandomCodeSpace/plasmid/internal/syntax"
	"github.com/RandomCodeSpace/plasmid/outputlimit"
	"github.com/RandomCodeSpace/plasmid/shellexec"
	"github.com/RandomCodeSpace/plasmid/warning"
)

func expandCommands(ctx context.Context, source, path string, trust TrustLevel, options commandOptions, executor *shellexec.Executor, sink warning.Warner) string {
	return expandCommandsWithBudget(ctx, commandExpansion{
		source: source, path: path, trust: trust, options: options, executor: executor, sink: sink,
		budget: newCommandDocumentBudget(options),
	})
}

type commandDocumentBudget struct {
	remaining    time.Duration
	timeoutBound bool
	outputBytes  int
}

func newCommandDocumentBudget(options commandOptions) *commandDocumentBudget {
	return &commandDocumentBudget{remaining: options.DocumentTimeout, timeoutBound: options.DocumentTimeout > 0}
}

type commandExpansion struct {
	source   string
	path     string
	trust    TrustLevel
	options  commandOptions
	executor *shellexec.Executor
	sink     warning.Warner
	budget   *commandDocumentBudget
}

func expandCommandsWithBudget(ctx context.Context, input commandExpansion) string {
	if input.sink == nil {
		input.sink = warning.DiscardSink{}
	}
	if input.budget == nil {
		input.budget = newCommandDocumentBudget(input.options)
	}
	directives := syntax.ScanCommandDirectives(input.source)
	if len(directives) == 0 {
		return input.source
	}
	allowed := input.options.Mode == config.PromptCommandsOn || input.options.Mode == config.PromptCommandsTrusted && input.trust != TrustUntrusted
	var output strings.Builder
	cursor := 0
	for _, directive := range directives {
		output.WriteString(input.source[cursor:directive.Start])
		cursor = directive.End
		if !allowed || input.executor == nil {
			input.warn(directive, warning.WarnSyntaxExecDisabled, "prompt command execution is disabled for this source")
			continue
		}
		if input.options.DocumentOutputBytes > 0 && input.budget.outputBytes >= input.options.DocumentOutputBytes {
			input.warn(directive, warning.WarnSyntaxExecBudgetExhausted, "prompt command document output budget is exhausted")
			continue
		}
		if input.budget.timeoutBound && input.budget.remaining <= 0 {
			input.warn(directive, warning.WarnSyntaxExecTimeout, "prompt command document timeout is exhausted")
			continue
		}
		output.WriteString(input.run(ctx, directive))
	}
	output.WriteString(input.source[cursor:])
	return output.String()
}

func (input commandExpansion) run(ctx context.Context, directive syntax.CommandDirective) string {
	runContext := ctx
	var cancel context.CancelFunc
	if input.budget.timeoutBound {
		runContext, cancel = context.WithTimeout(runContext, input.budget.remaining)
	}
	started := time.Now()
	result, err := input.executor.RunMerged(runContext, shellexec.Request{Command: directive.Command, Timeout: input.options.CommandTimeout})
	if input.budget.timeoutBound {
		input.budget.remaining -= time.Since(started)
	}
	runContextErr := runContext.Err()
	if cancel != nil {
		cancel()
	}
	if err != nil {
		code := warning.WarnSyntaxExecFailed
		if runContextErr != nil {
			code = warning.WarnSyntaxExecTimeout
		}
		input.warn(directive, code, "prompt command execution failed")
		return ""
	}
	if result.TimedOut {
		input.warn(directive, warning.WarnSyntaxExecTimeout, "prompt command timed out")
	}
	if result.ExitCode != 0 {
		input.warn(directive, warning.WarnSyntaxExecFailed, "prompt command exited unsuccessfully")
	}
	limit := input.options.CommandOutputBytes
	if input.options.DocumentOutputBytes > 0 && (limit <= 0 || input.options.DocumentOutputBytes-input.budget.outputBytes < limit) {
		limit = input.options.DocumentOutputBytes - input.budget.outputBytes
	}
	rendered, report := (outputlimit.Policy{MaxBytes: limit, MaxLines: 2000, MaxLineBytes: 2000, HeadFraction: 0.6}).Apply(result.Stdout)
	if report.Truncated {
		input.warn(directive, warning.WarnSyntaxExecBudgetExhausted, "prompt command output was truncated")
	}
	input.budget.outputBytes += len(rendered)
	return rendered
}

func (input commandExpansion) warn(directive syntax.CommandDirective, code, message string) {
	input.sink.Warn(warning.Warning{Code: code, Source: "syntax", Path: input.path, Line: directive.Line, Message: message})
}
