package contextresolver

import (
	"context"
	"errors"
	"fmt"

	"github.com/RandomCodeSpace/plasmid/internal/syntax"
)

// Expansion supplies the supported deterministic extension substitutions.
type Expansion struct {
	Source     string
	Path       string
	Trust      TrustLevel
	Arguments  string
	Declared   []string
	SessionID  string
	SkillDir   string
	ProjectDir string
	PluginRoot string
	PluginData string
	Effort     string
}

// Expand applies argument and Harness substitutions once, then runs prompt
// commands through the resolver's single bounded executor and trust gate.
func (r *Resolver) Expand(ctx context.Context, value Expansion) (string, error) {
	if r == nil || r.Closed() {
		return "", errors.New("expand extension: resolver is closed")
	}
	arguments, err := syntax.ParseArguments(value.Arguments, value.Declared)
	if err != nil {
		return "", err
	}
	projectDir := value.ProjectDir
	if projectDir == "" {
		projectDir = r.options.Root.Dir()
	}
	output, notices, err := syntax.SubstituteBounded(value.Source, value.Path, syntax.Substitutions{
		Arguments: arguments,
		Variables: syntax.Variables{
			SessionID: value.SessionID, SkillDir: value.SkillDir, ProjectDir: projectDir,
			PluginRoot: value.PluginRoot, PluginData: value.PluginData, Effort: value.Effort,
		},
	}, r.options.DocumentOutputBytes)
	if err != nil {
		return "", fmt.Errorf("expand extension: %w", err)
	}
	for _, notice := range notices {
		r.options.WarningSink.Warn(notice)
	}
	options := commandOptions{
		Mode: r.options.PromptCommands, CommandTimeout: r.options.CommandTimeout,
		DocumentTimeout: r.options.DocumentTimeout, CommandOutputBytes: r.options.CommandOutputBytes,
		DocumentOutputBytes: r.options.DocumentOutputBytes,
	}
	output = expandCommandsWithBudget(ctx, commandExpansion{
		source: output, path: value.Path, trust: value.Trust, options: options,
		executor: r.options.Executor, sink: r.options.WarningSink, budget: newCommandDocumentBudget(options),
	})
	if options.DocumentOutputBytes > 0 && len(output) > options.DocumentOutputBytes {
		return "", fmt.Errorf("expand extension: %w", syntax.ErrSubstitutionLimit)
	}
	return output, ctx.Err()
}
