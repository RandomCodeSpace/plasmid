package plasmid

import (
	"context"
	"sync"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	adktool "google.golang.org/adk/v2/tool"

	"github.com/plasmid-dev/plasmid/codingtools"
	"github.com/plasmid-dev/plasmid/lsp"
	"github.com/plasmid-dev/plasmid/warning"
)

type lspDecorator interface {
	Await(context.Context, string, string) (lsp.Decoration, bool)
	Drop(string, string)
}

func lspAfterToolCallback(decorator lspDecorator, warnings warning.Sink) llmagent.AfterToolCallback {
	var panicWarning sync.Once
	return func(ctx agent.Context, currentTool adktool.Tool, _ map[string]any, result map[string]any, toolErr error) (replacement map[string]any, callbackErr error) {
		defer recoverLSPAfterTool(warnings, &panicWarning, &replacement, &callbackErr)
		return decorateLSPAfterTool(decorator, ctx, currentTool, result, toolErr)
	}
}

func recoverLSPAfterTool(warnings warning.Sink, once *sync.Once, replacement *map[string]any, callbackErr *error) {
	if recover() == nil {
		return
	}
	once.Do(func() {
		if warnings != nil {
			warnings.Warn(warning.Warning{
				Code: warning.WarnLSPRequestFailed, Source: "lsp.callback",
				Message: "LSP after-tool decoration degraded to no-op after panic",
			})
		}
	})
	*replacement = nil
	*callbackErr = nil
}

func decorateLSPAfterTool(decorator lspDecorator, ctx agent.Context, currentTool adktool.Tool, result map[string]any, toolErr error) (map[string]any, error) {
	if decorator == nil || ctx == nil || currentTool == nil {
		return nil, nil
	}
	name := currentTool.Name()
	if name != "write" && name != "edit" {
		return nil, nil
	}
	sessionID, invocationID := ctx.SessionID(), ctx.FunctionCallID()
	if toolErr != nil || result == nil {
		decorator.Drop(sessionID, invocationID)
		return nil, nil
	}
	decoration, ok := decorator.Await(ctx, sessionID, invocationID)
	if !ok {
		return nil, nil
	}
	result[codingtools.DiagnosticsResultKey] = decoration.Diagnostics
	result[codingtools.DiagnosticsTextResultKey] = decoration.Text
	return nil, nil
}
