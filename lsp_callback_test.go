package plasmid

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"go.lsp.dev/protocol"
	"google.golang.org/adk/v2/agent"
	adktool "google.golang.org/adk/v2/tool"

	"github.com/plasmid-dev/plasmid/codingtools"
	"github.com/plasmid-dev/plasmid/lsp"
	"github.com/plasmid-dev/plasmid/warning"
)

type lspCallbackContext struct {
	agent.StrictContextMock
	sessionID    string
	invocationID string
}

func (ctx *lspCallbackContext) SessionID() string      { return ctx.sessionID }
func (ctx *lspCallbackContext) FunctionCallID() string { return ctx.invocationID }

type lspCallbackTool string

func (tool lspCallbackTool) Name() string   { return string(tool) }
func (lspCallbackTool) Description() string { return "test" }
func (lspCallbackTool) IsLongRunning() bool { return false }

type fakeLSPDecorator struct {
	decoration lsp.Decoration
	ok         bool
	panicAwait bool
	awaits     int
	drops      [][2]string
}

func (decorator *fakeLSPDecorator) Await(context.Context, string, string) (lsp.Decoration, bool) {
	decorator.awaits++
	if decorator.panicAwait {
		panic("server callback panic")
	}
	return decorator.decoration, decorator.ok
}

func (decorator *fakeLSPDecorator) Drop(sessionID, invocationID string) {
	decorator.drops = append(decorator.drops, [2]string{sessionID, invocationID})
}

func TestLSPAfterToolCallbackInjectsOnlyReservedKeysAndContinues(t *testing.T) {
	diagnostics := []lsp.Diagnostic{{
		Path: "main.go", Start: protocol.Position{Line: 2, Character: 1}, End: protocol.Position{Line: 2, Character: 3},
		Severity: protocol.DiagnosticSeverityError, Message: "broken",
	}}
	decorator := &fakeLSPDecorator{decoration: lsp.Decoration{Diagnostics: diagnostics, Text: "main.go:3:2: error: broken"}, ok: true}
	callback := lspAfterToolCallback(decorator, warning.DiscardSink{})
	ctx := &lspCallbackContext{StrictContextMock: agent.NewStrictContextMock(t.Context()), sessionID: "session", invocationID: "write-1"}
	result := map[string]any{"path": "main.go", "bytes_written": 13}

	replacement, err := callback(ctx, lspCallbackTool("write"), nil, result, nil)
	if err != nil || replacement != nil {
		t.Fatalf("callback = %#v, %v", replacement, err)
	}
	if !reflect.DeepEqual(result[codingtools.DiagnosticsResultKey], diagnostics) || result[codingtools.DiagnosticsTextResultKey] != decorator.decoration.Text {
		t.Fatalf("decorated result = %#v", result)
	}
	if len(result) != 4 || decorator.awaits != 1 || len(decorator.drops) != 0 {
		t.Fatalf("result size = %d, awaits = %d, drops = %#v", len(result), decorator.awaits, decorator.drops)
	}
}

func TestLSPAfterToolCallbackFiltersFailuresAndContainsPanics(t *testing.T) {
	tests := []struct {
		name      string
		tool      string
		result    map[string]any
		toolErr   error
		decorator *fakeLSPDecorator
		wantAwait int
		wantDrop  bool
	}{
		{name: "non mutation", tool: "read", result: map[string]any{"path": "main.go"}, decorator: &fakeLSPDecorator{ok: true}},
		{name: "tool failure", tool: "write", toolErr: errors.New("failed"), decorator: &fakeLSPDecorator{ok: true}, wantDrop: true},
		{name: "nil success", tool: "edit", decorator: &fakeLSPDecorator{ok: true}, wantDrop: true},
		{name: "no current diagnostics", tool: "write", result: map[string]any{"path": "main.go"}, decorator: &fakeLSPDecorator{}, wantAwait: 1},
		{name: "panic", tool: "edit", result: map[string]any{"path": "main.go"}, decorator: &fakeLSPDecorator{panicAwait: true}, wantAwait: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			callback := lspAfterToolCallback(test.decorator, warning.DiscardSink{})
			ctx := &lspCallbackContext{StrictContextMock: agent.NewStrictContextMock(t.Context()), sessionID: "session", invocationID: "call"}
			replacement, err := callback(ctx, lspCallbackTool(test.tool), nil, test.result, test.toolErr)
			if replacement != nil || err != nil {
				t.Fatalf("callback = %#v, %v", replacement, err)
			}
			if test.decorator.awaits != test.wantAwait {
				t.Fatalf("awaits = %d, want %d", test.decorator.awaits, test.wantAwait)
			}
			if (len(test.decorator.drops) != 0) != test.wantDrop {
				t.Fatalf("drops = %#v, want drop %t", test.decorator.drops, test.wantDrop)
			}
			if test.result != nil {
				if _, exists := test.result[codingtools.DiagnosticsResultKey]; exists {
					t.Fatalf("unexpected decoration = %#v", test.result)
				}
			}
		})
	}
}

func TestLSPAfterToolCallbackWarnsOnceAfterPanic(t *testing.T) {
	decorator := &fakeLSPDecorator{panicAwait: true}
	var warnings warning.SliceSink
	callback := lspAfterToolCallback(decorator, &warnings)
	ctx := &lspCallbackContext{StrictContextMock: agent.NewStrictContextMock(t.Context()), sessionID: "session", invocationID: "call"}
	for range 2 {
		replacement, err := callback(ctx, lspCallbackTool("write"), nil, map[string]any{"path": "main.go"}, nil)
		if replacement != nil || err != nil {
			t.Fatalf("callback = %#v, %v", replacement, err)
		}
	}
	got := warnings.Warnings()
	if len(got) != 1 || got[0].Code != warning.WarnLSPRequestFailed || got[0].Source != "lsp.callback" {
		t.Fatalf("warnings = %#v", got)
	}
}

var _ agent.Context = (*lspCallbackContext)(nil)
var _ adktool.Tool = lspCallbackTool("")
