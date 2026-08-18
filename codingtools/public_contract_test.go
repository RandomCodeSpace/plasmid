package codingtools_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	adktool "google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"

	"github.com/plasmid-dev/plasmid/codingtools"
	"github.com/plasmid-dev/plasmid/outputlimit"
	"github.com/plasmid-dev/plasmid/shellexec"
	"github.com/plasmid-dev/plasmid/warning"
	"github.com/plasmid-dev/plasmid/workspace"
)

func TestPublicConstructorsRejectInvalidConfiguration(t *testing.T) {
	t.Parallel()
	root, err := workspace.NewRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	shell, err := shellexec.New(shellexec.Config{Root: root, Shell: "sh"})
	if err != nil {
		t.Skipf("shell unavailable: %v", err)
	}
	valid := codingtools.Config{
		Root: root, Queue: workspace.NewMutationQueue(), Ledger: workspace.NewLedger(),
		Touch: workspace.NewTouchBus(), Shell: shell, Budget: outputlimit.NewBudget(1 << 20),
	}
	tests := []struct {
		name      string
		construct func(codingtools.Config) (adktool.Tool, error)
		mutate    func(*codingtools.Config)
	}{
		{name: "read root", construct: codingtools.NewReadTool, mutate: func(cfg *codingtools.Config) { cfg.Root = nil }},
		{name: "read ledger", construct: codingtools.NewReadTool, mutate: func(cfg *codingtools.Config) { cfg.Ledger = nil }},
		{name: "read touch", construct: codingtools.NewReadTool, mutate: func(cfg *codingtools.Config) { cfg.Touch = nil }},
		{name: "read budget", construct: codingtools.NewReadTool, mutate: func(cfg *codingtools.Config) { cfg.Budget = nil }},
		{name: "write root", construct: codingtools.NewWriteTool, mutate: func(cfg *codingtools.Config) { cfg.Root = nil }},
		{name: "write queue", construct: codingtools.NewWriteTool, mutate: func(cfg *codingtools.Config) { cfg.Queue = nil }},
		{name: "write ledger", construct: codingtools.NewWriteTool, mutate: func(cfg *codingtools.Config) { cfg.Ledger = nil }},
		{name: "write touch", construct: codingtools.NewWriteTool, mutate: func(cfg *codingtools.Config) { cfg.Touch = nil }},
		{name: "write budget", construct: codingtools.NewWriteTool, mutate: func(cfg *codingtools.Config) { cfg.Budget = nil }},
		{name: "edit root", construct: codingtools.NewEditTool, mutate: func(cfg *codingtools.Config) { cfg.Root = nil }},
		{name: "edit queue", construct: codingtools.NewEditTool, mutate: func(cfg *codingtools.Config) { cfg.Queue = nil }},
		{name: "edit ledger", construct: codingtools.NewEditTool, mutate: func(cfg *codingtools.Config) { cfg.Ledger = nil }},
		{name: "edit touch", construct: codingtools.NewEditTool, mutate: func(cfg *codingtools.Config) { cfg.Touch = nil }},
		{name: "edit budget", construct: codingtools.NewEditTool, mutate: func(cfg *codingtools.Config) { cfg.Budget = nil }},
		{name: "bash root", construct: codingtools.NewBashTool, mutate: func(cfg *codingtools.Config) { cfg.Root = nil }},
		{name: "bash shell", construct: codingtools.NewBashTool, mutate: func(cfg *codingtools.Config) { cfg.Shell = nil }},
		{name: "bash budget", construct: codingtools.NewBashTool, mutate: func(cfg *codingtools.Config) { cfg.Budget = nil }},
		{name: "grep root", construct: codingtools.NewGrepTool, mutate: func(cfg *codingtools.Config) { cfg.Root = nil }},
		{name: "grep touch", construct: codingtools.NewGrepTool, mutate: func(cfg *codingtools.Config) { cfg.Touch = nil }},
		{name: "grep budget", construct: codingtools.NewGrepTool, mutate: func(cfg *codingtools.Config) { cfg.Budget = nil }},
		{name: "find root", construct: codingtools.NewFindTool, mutate: func(cfg *codingtools.Config) { cfg.Root = nil }},
		{name: "find touch", construct: codingtools.NewFindTool, mutate: func(cfg *codingtools.Config) { cfg.Touch = nil }},
		{name: "find budget", construct: codingtools.NewFindTool, mutate: func(cfg *codingtools.Config) { cfg.Budget = nil }},
		{name: "list root", construct: codingtools.NewListTool, mutate: func(cfg *codingtools.Config) { cfg.Root = nil }},
		{name: "list touch", construct: codingtools.NewListTool, mutate: func(cfg *codingtools.Config) { cfg.Touch = nil }},
		{name: "list budget", construct: codingtools.NewListTool, mutate: func(cfg *codingtools.Config) { cfg.Budget = nil }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			cfg := valid
			test.mutate(&cfg)
			if _, err := test.construct(cfg); err == nil {
				t.Fatal("constructor accepted invalid configuration")
			}
		})
	}

	invalidOutput := valid
	invalidOutput.Output = outputlimit.Policy{MaxBytes: -1}
	if _, err := codingtools.New(invalidOutput); !errors.Is(err, outputlimit.ErrInvalidLimit) {
		t.Fatalf("set invalid output policy error = %v", err)
	}
	for _, construct := range []func(codingtools.Config) (adktool.Tool, error){
		codingtools.NewReadTool, codingtools.NewWriteTool, codingtools.NewEditTool,
		codingtools.NewBashTool, codingtools.NewGrepTool, codingtools.NewFindTool, codingtools.NewListTool,
	} {
		if _, err := construct(invalidOutput); !errors.Is(err, outputlimit.ErrInvalidLimit) {
			t.Fatalf("invalid output policy error = %v", err)
		}
	}
	noLines := valid
	noLines.Output = outputlimit.Policy{MaxBytes: 10, MaxLineBytes: 10, HeadFraction: 0.5}
	for _, construct := range []func(codingtools.Config) (adktool.Tool, error){
		codingtools.NewReadTool, codingtools.NewWriteTool, codingtools.NewEditTool,
		codingtools.NewGrepTool,
	} {
		if _, err := construct(noLines); err == nil || !strings.Contains(err.Error(), "max lines") {
			t.Fatalf("zero line limit error = %v", err)
		}
	}
}

func TestNilSetAccessorsAreEmpty(t *testing.T) {
	t.Parallel()
	var set *codingtools.Set
	if set.Tools() != nil || set.Names() != nil {
		t.Fatal("nil set exposed tools")
	}
	if tool, ok := set.Tool("read"); ok || tool != nil {
		t.Fatalf("nil set lookup = %#v, %t", tool, ok)
	}
}

func TestPublicNativeToolsHonorCancelledADKContext(t *testing.T) {
	directory := t.TempDir()
	writeFile(t, directory, "file.txt", "value\n")
	root, err := workspace.NewRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	shell, err := shellexec.New(shellexec.Config{Root: root, Shell: "sh"})
	if err != nil {
		t.Skipf("shell unavailable: %v", err)
	}
	set, err := codingtools.New(codingtools.Config{
		Root: root, Queue: workspace.NewMutationQueue(), Ledger: workspace.NewLedger(),
		Touch: workspace.NewTouchBus(), Shell: shell, Budget: outputlimit.NewBudget(1 << 20), WarningSink: warning.DiscardSink{},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	agentContext := &publicAgentContext{StrictContextMock: agent.NewStrictContextMock(ctx)}
	arguments := map[string]map[string]any{
		"read":  {"path": "file.txt"},
		"write": {"path": "new.txt", "content": "new"},
		"edit":  {"path": "file.txt", "old_text": "value", "new_text": "changed"},
		"bash":  {"command": "true"},
		"grep":  {"pattern": "value"},
		"find":  {"glob": "*"},
		"ls":    {},
	}
	for name, args := range arguments {
		tool, ok := set.Tool(name)
		if !ok {
			t.Fatalf("tool %q missing", name)
		}
		runnable, ok := tool.(interface {
			Run(agent.Context, any) (map[string]any, error)
		})
		if !ok {
			t.Fatalf("tool %q does not expose native Run", name)
		}
		if _, err := runnable.Run(agentContext, args); !errors.Is(err, context.Canceled) {
			t.Errorf("%s cancellation error = %v", name, err)
		}
	}
}

func TestPublicReadAndSearchToolsHonorCancellationBetweenOperationStages(t *testing.T) {
	directory := t.TempDir()
	writeFile(t, directory, "file.txt", strings.Repeat("needle\n", 20))
	root, err := workspace.NewRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	set, err := codingtools.New(codingtools.Config{
		Root: root, Queue: workspace.NewMutationQueue(), Ledger: workspace.NewLedger(),
		Touch: workspace.NewTouchBus(), Budget: outputlimit.NewBudget(1 << 20), WarningSink: warning.DiscardSink{},
	})
	if err != nil {
		t.Fatal(err)
	}
	operations := []struct {
		name string
		args map[string]any
	}{
		{name: "read", args: map[string]any{"path": "file.txt"}},
		{name: "grep", args: map[string]any{"path": ".", "pattern": "needle"}},
		{name: "find", args: map[string]any{"path": ".", "glob": "*.txt"}},
		{name: "ls", args: map[string]any{"path": "."}},
	}
	for _, operation := range operations {
		tool, ok := set.Tool(operation.name)
		if !ok {
			t.Fatalf("tool %q missing", operation.name)
		}
		runnable := tool.(interface {
			Run(agent.Context, any) (map[string]any, error)
		})
		for cancelAfter := 2; cancelAfter <= 100; cancelAfter++ {
			ctx := cancellationAfterChecks(cancelAfter)
			agentContext := &publicAgentContext{StrictContextMock: agent.NewStrictContextMock(ctx)}
			_, runErr := runnable.Run(agentContext, operation.args)
			if runErr != nil && !errors.Is(runErr, context.Canceled) {
				t.Fatalf("%s cancellation after check %d = %v", operation.name, cancelAfter, runErr)
			}
		}
	}
}

func TestPublicMutationAndBashToolsHonorCancellationBetweenOperationStages(t *testing.T) {
	directory := t.TempDir()
	root, err := workspace.NewRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	shell, err := shellexec.New(shellexec.Config{Root: root, Shell: "sh"})
	if err != nil {
		t.Skipf("shell unavailable: %v", err)
	}
	set, err := codingtools.New(codingtools.Config{
		Root: root, Queue: workspace.NewMutationQueue(), Ledger: workspace.NewLedger(),
		Touch: workspace.NewTouchBus(), Shell: shell, Budget: outputlimit.NewBudget(1 << 20), WarningSink: warning.DiscardSink{},
	})
	if err != nil {
		t.Fatal(err)
	}
	for cancelAfter := 2; cancelAfter <= 20; cancelAfter++ {
		path := filepath.ToSlash(filepath.Join("write", fmt.Sprintf("%d.txt", cancelAfter)))
		runPublicToolWithCancellation(t, set, "write", map[string]any{"path": path, "content": "new\n"}, cancelAfter)

		editPath := fmt.Sprintf("edit-%d.txt", cancelAfter)
		writeFile(t, directory, editPath, "old\n")
		invokeTool(t, set, "read", map[string]any{"path": editPath})
		runPublicToolWithCancellation(t, set, "edit", map[string]any{"path": editPath, "old_text": "old", "new_text": "new"}, cancelAfter)

		runPublicToolWithCancellation(t, set, "bash", map[string]any{"command": "true"}, cancelAfter)
	}
}

type pathChangeCase struct {
	name      string
	maxBytes  int64
	after     int
	prepare   func(*testing.T, string)
	mutate    func(*testing.T, string)
	wantError string
}

func TestPublicReadDetectsPathChangesAfterOpening(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink and open-file replacement semantics differ on Windows")
	}
	tests := []pathChangeCase{
		{
			name: "symlink retarget", prepare: func(t *testing.T, directory string) { writeFile(t, directory, "other.txt", "other\n") },
			mutate: func(t *testing.T, directory string) {
				if err := os.Remove(filepath.Join(directory, "file.txt")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("other.txt", filepath.Join(directory, "file.txt")); err != nil {
					t.Fatal(err)
				}
			},
			wantError: "path changed",
		},
		{
			name: "inode replacement", prepare: func(t *testing.T, directory string) { writeFile(t, directory, "other.txt", "other\n") },
			mutate: func(t *testing.T, directory string) {
				if err := os.Rename(filepath.Join(directory, "other.txt"), filepath.Join(directory, "file.txt")); err != nil {
					t.Fatal(err)
				}
			},
			wantError: "path changed",
		},
		{
			name: "late inode replacement", after: 7, prepare: func(t *testing.T, directory string) { writeFile(t, directory, "other.txt", "other\n") },
			mutate: func(t *testing.T, directory string) {
				if err := os.Rename(filepath.Join(directory, "other.txt"), filepath.Join(directory, "file.txt")); err != nil {
					t.Fatal(err)
				}
			},
			wantError: "path changed",
		},
		{
			name: "removal",
			mutate: func(t *testing.T, directory string) {
				if err := os.Remove(filepath.Join(directory, "file.txt")); err != nil {
					t.Fatal(err)
				}
			},
			wantError: "path not found",
		},
		{
			name: "growth", maxBytes: 8,
			mutate:    func(t *testing.T, directory string) { writeFile(t, directory, "file.txt", strings.Repeat("x", 100)) },
			wantError: "file is too large",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertPathChangeCase(t, test)
		})
	}
}

func assertPathChangeCase(t *testing.T, test pathChangeCase) {
	t.Helper()
	directory := t.TempDir()
	writeFile(t, directory, "file.txt", "tiny")
	if test.prepare != nil {
		test.prepare(t, directory)
	}
	root, err := workspace.NewRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	set, err := codingtools.New(codingtools.Config{
		Root: root, Queue: workspace.NewMutationQueue(), Ledger: workspace.NewLedger(), Touch: workspace.NewTouchBus(),
		Budget: outputlimit.NewBudget(1 << 20), WarningSink: warning.DiscardSink{}, MaxReadBytes: test.maxBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	after := test.after
	if after == 0 {
		after = 2
	}
	ctx := effectAfterChecks(after, func() { test.mutate(t, directory) })
	_, runErr := runPublicTool(t, set, "read", map[string]any{"path": "file.txt"}, ctx)
	if runErr == nil || !strings.Contains(runErr.Error(), test.wantError) {
		t.Fatalf("read error = %v, want %q", runErr, test.wantError)
	}
}

func TestPublicToolsContainSymlinkResolutionFailures(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation commonly requires elevated privileges")
	}
	directory := t.TempDir()
	if err := os.Symlink("loop", filepath.Join(directory, "loop")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	root, err := workspace.NewRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	set, err := codingtools.New(codingtools.Config{
		Root: root, Queue: workspace.NewMutationQueue(), Ledger: workspace.NewLedger(),
		Touch: workspace.NewTouchBus(), Budget: outputlimit.NewBudget(1 << 20), WarningSink: warning.DiscardSink{},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		tool string
		args map[string]any
	}{
		{tool: "read", args: map[string]any{"path": "loop"}},
		{tool: "write", args: map[string]any{"path": "loop/file", "content": "new"}},
		{tool: "edit", args: map[string]any{"path": "loop", "old_text": "old", "new_text": "new"}},
		{tool: "grep", args: map[string]any{"path": "loop", "pattern": "old"}},
		{tool: "find", args: map[string]any{"path": "loop", "glob": "*"}},
		{tool: "ls", args: map[string]any{"path": "loop"}},
	} {
		response := invokeTool(t, set, test.tool, test.args)
		if message, _ := response["error"].(string); message == "" {
			t.Fatalf("%s accepted a symlink loop: %#v", test.tool, response)
		}
	}
}

func TestPublicNativeToolsPerformWorkspaceWorkflow(t *testing.T) {
	directory := t.TempDir()
	writeFile(t, directory, "edit.txt", "old value\n")
	writeFile(t, directory, "search/a.txt", "before\nneedle\nafter\n")
	writeFile(t, directory, "search/b.go", "needle two\n")
	root, err := workspace.NewRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	touch := workspace.NewTouchBus()
	set, err := codingtools.New(codingtools.Config{
		Root: root, Queue: workspace.NewMutationQueue(), Ledger: workspace.NewLedger(),
		Touch: touch, Budget: outputlimit.NewBudget(1 << 20), WarningSink: warning.DiscardSink{},
	})
	if err != nil {
		t.Fatal(err)
	}

	read := invokeTool(t, set, "read", map[string]any{"path": "edit.txt"})
	if read["content"] != "     1\told value\n" {
		t.Fatalf("read response = %#v", read)
	}
	edit := invokeTool(t, set, "edit", map[string]any{"path": "edit.txt", "old_text": "old", "new_text": "new"})
	if edit["replacements"] != float64(1) || edit["match_tier"] != "exact" {
		t.Fatalf("edit response = %#v", edit)
	}
	created := invokeTool(t, set, "write", map[string]any{"path": "new/file.txt", "content": "created\n"})
	if created["bytes_written"] != float64(8) {
		t.Fatalf("write response = %#v", created)
	}
	grep := invokeTool(t, set, "grep", map[string]any{"pattern": "needle", "path": "search", "glob": "*.txt", "context_lines": 1})
	if grep["match_count"] != float64(1) || grep["files"] != float64(1) {
		t.Fatalf("grep response = %#v", grep)
	}
	find := invokeTool(t, set, "find", map[string]any{"path": ".", "glob": "**/*.txt", "type": "file"})
	if paths, ok := find["paths"].([]any); !ok || len(paths) != 3 {
		t.Fatalf("find response = %#v", find)
	}
	list := invokeTool(t, set, "ls", map[string]any{"path": ".", "max_depth": 2})
	if entries, ok := list["entries"].([]any); !ok || len(entries) < 6 {
		t.Fatalf("ls response = %#v", list)
	}

	content, err := os.ReadFile(filepath.Join(directory, "edit.txt"))
	if err != nil || string(content) != "new value\n" {
		t.Fatalf("edited content = %q, %v", content, err)
	}
}

func TestPublicToolsReturnStableBoundaryErrors(t *testing.T) {
	directory := t.TempDir()
	writeFile(t, directory, "file.txt", "value\n")
	if err := os.Mkdir(filepath.Join(directory, "dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := workspace.NewRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	set, err := codingtools.New(codingtools.Config{
		Root: root, Queue: workspace.NewMutationQueue(), Ledger: workspace.NewLedger(),
		Touch: workspace.NewTouchBus(), Budget: outputlimit.NewBudget(1 << 20), WarningSink: warning.DiscardSink{},
		MaxReadBytes: 4, MaxWriteBytes: 4, MaxGrepFileBytes: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		tool string
		args map[string]any
		want string
	}{
		{name: "read unknown argument", tool: "read", args: map[string]any{"path": "file.txt", "extra": true}, want: "unexpected additional properties"},
		{name: "read empty path", tool: "read", args: map[string]any{"path": ""}, want: "must not be empty"},
		{name: "read outside", tool: "read", args: map[string]any{"path": "../outside"}, want: "outside root"},
		{name: "read directory", tool: "read", args: map[string]any{"path": "dir"}, want: "is a directory"},
		{name: "read too large", tool: "read", args: map[string]any{"path": "file.txt"}, want: "file is too large"},
		{name: "write too large", tool: "write", args: map[string]any{"path": "new", "content": "12345"}, want: "file is too large"},
		{name: "write outside", tool: "write", args: map[string]any{"path": "../outside", "content": "x"}, want: "outside root"},
		{name: "write empty path", tool: "write", args: map[string]any{"path": "", "content": "x"}, want: "must not be empty"},
		{name: "edit missing", tool: "edit", args: map[string]any{"path": "missing", "old_text": "x", "new_text": "y"}, want: "path not found"},
		{name: "edit outside", tool: "edit", args: map[string]any{"path": "../outside", "old_text": "x", "new_text": "y"}, want: "outside root"},
		{name: "edit empty path", tool: "edit", args: map[string]any{"path": "", "old_text": "x", "new_text": "y"}, want: "must not be empty"},
		{name: "edit empty old text", tool: "edit", args: map[string]any{"path": "file.txt", "old_text": "", "new_text": "y"}, want: "non-empty"},
		{name: "edit directory", tool: "edit", args: map[string]any{"path": "dir", "old_text": "x", "new_text": "y"}, want: "is a directory"},
		{name: "grep invalid pattern", tool: "grep", args: map[string]any{"pattern": "(?=x)", "path": "."}, want: "unsupported pattern"},
		{name: "grep invalid file glob", tool: "grep", args: map[string]any{"pattern": "x", "path": "file.txt", "glob": "["}, want: "unsupported pattern"},
		{name: "find invalid glob", tool: "find", args: map[string]any{"glob": "[", "path": "."}, want: "unsupported pattern"},
		{name: "find empty glob", tool: "find", args: map[string]any{"glob": "", "path": "."}, want: "non-empty"},
		{name: "list file", tool: "ls", args: map[string]any{"path": "file.txt"}, want: "not a directory"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := invokeTool(t, set, test.tool, test.args)
			if got, _ := response["error"].(string); !strings.Contains(got, test.want) {
				t.Fatalf("%s response = %#v, want error containing %q", test.tool, response, test.want)
			}
		})
	}
}

func TestPublicBashToolSharesSessionOutputBudget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell command is Unix-specific")
	}
	root, err := workspace.NewRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	policy := outputlimit.Policy{MaxBytes: 256, MaxLines: 10, MaxLineBytes: 256, HeadFraction: 0.5}
	shell, err := shellexec.New(shellexec.Config{Root: root, Shell: "sh", OutputLimit: policy})
	if err != nil {
		t.Skipf("shell unavailable: %v", err)
	}
	set, err := codingtools.New(codingtools.Config{
		Root: root, Queue: workspace.NewMutationQueue(), Ledger: workspace.NewLedger(),
		Touch: workspace.NewTouchBus(), Shell: shell, Output: policy,
		Budget: outputlimit.NewBudget(128), WarningSink: warning.DiscardSink{},
		DefaultBashTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	response := invokeTool(t, set, "bash", map[string]any{"command": "printf '%0200d' 0; printf '%0100d' 0 >&2"})
	if response["truncated"] != true {
		t.Fatalf("bash response = %#v", response)
	}
	if stdout, _ := response["stdout"].(string); len(stdout) == 0 || len(stdout) >= 200 {
		t.Fatalf("bounded stdout = %q", stdout)
	}
}

func TestPublicBashBalancesAsymmetricStreamsAndCapsTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell command is Unix-specific")
	}
	for _, command := range []string{
		"printf x; printf '%0200d' 0 >&2",
		"printf '%0200d' 0; printf x >&2",
	} {
		root, err := workspace.NewRoot(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		policy := outputlimit.Policy{MaxBytes: 256, MaxLines: 10, MaxLineBytes: 256, HeadFraction: 0.5}
		shell, err := shellexec.New(shellexec.Config{Root: root, Shell: "sh", OutputLimit: policy})
		if err != nil {
			t.Skipf("shell unavailable: %v", err)
		}
		set, err := codingtools.New(codingtools.Config{
			Root: root, Queue: workspace.NewMutationQueue(), Ledger: workspace.NewLedger(), Touch: workspace.NewTouchBus(),
			Shell: shell, Output: policy, Budget: outputlimit.NewBudget(64), WarningSink: warning.DiscardSink{},
		})
		if err != nil {
			t.Fatal(err)
		}
		response := invokeTool(t, set, "bash", map[string]any{"command": command})
		if response["truncated"] != true {
			t.Fatalf("asymmetric bash response = %#v", response)
		}
	}

	root, err := workspace.NewRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	shell, err := shellexec.New(shellexec.Config{Root: root, Shell: "sh"})
	if err != nil {
		t.Skipf("shell unavailable: %v", err)
	}
	set, err := codingtools.New(codingtools.Config{
		Root: root, Queue: workspace.NewMutationQueue(), Ledger: workspace.NewLedger(), Touch: workspace.NewTouchBus(), Shell: shell,
		Budget: outputlimit.NewBudget(1 << 20), WarningSink: warning.DiscardSink{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response := invokeTool(t, set, "bash", map[string]any{"command": "true", "timeout_ms": int64(10_000_000_000_000)}); response["exit_code"] != float64(0) {
		t.Fatalf("capped timeout response = %#v", response)
	}
}

func TestPublicBashToolClassifiesWorkspaceDirectoryErrors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell command is Unix-specific")
	}
	directory := t.TempDir()
	writeFile(t, directory, "file", "content")
	root, err := workspace.NewRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	shell, err := shellexec.New(shellexec.Config{Root: root, Shell: "sh"})
	if err != nil {
		t.Skipf("shell unavailable: %v", err)
	}
	set, err := codingtools.New(codingtools.Config{
		Root: root, Queue: workspace.NewMutationQueue(), Ledger: workspace.NewLedger(), Touch: workspace.NewTouchBus(),
		Shell: shell, Budget: outputlimit.NewBudget(1 << 20), WarningSink: warning.DiscardSink{},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		dir  string
		want string
	}{
		{dir: "../outside", want: "outside root"},
		{dir: "missing", want: "path not found"},
		{dir: "file", want: "not a directory"},
	} {
		response := invokeTool(t, set, "bash", map[string]any{"command": "true", "dir": test.dir})
		if got, _ := response["error"].(string); !strings.Contains(got, test.want) {
			t.Fatalf("bash dir %q response = %#v", test.dir, response)
		}
	}
	empty := invokeTool(t, set, "bash", map[string]any{"command": " \t"})
	if got, _ := empty["error"].(string); !strings.Contains(got, "must not be empty") {
		t.Fatalf("empty bash response = %#v", empty)
	}
}

func TestPublicBashToolContainsShellStartupFailures(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executable script setup is Unix-specific")
	}
	directory := t.TempDir()
	root, err := workspace.NewRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	shellPath := filepath.Join(directory, "temporary-shell")
	if err := os.WriteFile(shellPath, []byte("#!/bin/sh\nexec /bin/sh \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	shell, err := shellexec.New(shellexec.Config{Root: root, Shell: shellPath})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(shellPath); err != nil {
		t.Fatal(err)
	}
	set, err := codingtools.New(codingtools.Config{
		Root: root, Queue: workspace.NewMutationQueue(), Ledger: workspace.NewLedger(), Touch: workspace.NewTouchBus(),
		Shell: shell, Budget: outputlimit.NewBudget(1 << 20), WarningSink: warning.DiscardSink{},
	})
	if err != nil {
		t.Fatal(err)
	}
	response := invokeTool(t, set, "bash", map[string]any{"command": "true"})
	if message, _ := response["error"].(string); !strings.Contains(message, "run bash command") {
		t.Fatalf("bash startup response = %#v", response)
	}
}

func TestPublicReadHandlesBinaryWindowsAndExhaustedBudget(t *testing.T) {
	directory := t.TempDir()
	writeFile(t, directory, "binary", string([]byte{'a', 0, 'b'}))
	writeFile(t, directory, "lines.txt", "one\ntwo\nthree\n")
	writeFile(t, directory, "large.txt", strings.Repeat("x", 100<<10))
	root, err := workspace.NewRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	budget := exhaustedBudget("public-session")
	set, err := codingtools.New(codingtools.Config{
		Root: root, Queue: workspace.NewMutationQueue(), Ledger: workspace.NewLedger(),
		Touch: workspace.NewTouchBus(), Budget: budget, WarningSink: warning.DiscardSink{},
	})
	if err != nil {
		t.Fatal(err)
	}
	binary := invokeTool(t, set, "read", map[string]any{"path": "binary"})
	if got, _ := binary["error"].(string); !strings.Contains(got, "file is binary") {
		t.Fatalf("binary read response = %#v", binary)
	}
	window := invokeTool(t, set, "read", map[string]any{"path": "lines.txt", "offset": 2, "limit": 1})
	if window["start_line"] != float64(2) || window["end_line"] != float64(2) || window["truncated"] != true {
		t.Fatalf("window read response = %#v", window)
	}
	content, _ := window["content"].(string)
	if !strings.Contains(content, "reason=budget") || !strings.HasSuffix(content, "\n") {
		t.Fatalf("exhausted read content = %q", content)
	}
	pastEOF := invokeTool(t, set, "read", map[string]any{"path": "lines.txt", "offset": 99})
	if pastEOF["content"] != "" || pastEOF["start_line"] != float64(0) {
		t.Fatalf("past-EOF read response = %#v", pastEOF)
	}
	if large := invokeTool(t, set, "read", map[string]any{"path": "large.txt"}); large["total_lines"] != float64(1) {
		t.Fatalf("large read response = %#v", large)
	}
}

func TestPublicWriteAndEditEnforceReadFreshness(t *testing.T) {
	directory := t.TempDir()
	writeFile(t, directory, "write.txt", "old\n")
	writeFile(t, directory, "edit.txt", "same\nsame\n")
	root, err := workspace.NewRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	set, err := codingtools.New(codingtools.Config{
		Root: root, Queue: workspace.NewMutationQueue(), Ledger: workspace.NewLedger(),
		Touch: workspace.NewTouchBus(), Budget: outputlimit.NewBudget(1 << 20), WarningSink: warning.DiscardSink{},
		MaxWriteBytes: 16,
	})
	if err != nil {
		t.Fatal(err)
	}
	neverRead := invokeTool(t, set, "write", map[string]any{"path": "write.txt", "content": "new\n"})
	if got, _ := neverRead["error"].(string); !strings.Contains(got, "never read") {
		t.Fatalf("write without read response = %#v", neverRead)
	}
	invokeTool(t, set, "read", map[string]any{"path": "write.txt"})
	writeFile(t, directory, "write.txt", "disk\n")
	stale := invokeTool(t, set, "write", map[string]any{"path": "write.txt", "content": "new\n"})
	if got, _ := stale["error"].(string); !strings.Contains(got, "changed since it was read") {
		t.Fatalf("stale write response = %#v", stale)
	}

	editNeverRead := invokeTool(t, set, "edit", map[string]any{"path": "edit.txt", "old_text": "same", "new_text": "changed"})
	if got, _ := editNeverRead["error"].(string); !strings.Contains(got, "never read") {
		t.Fatalf("edit without read response = %#v", editNeverRead)
	}
	invokeTool(t, set, "read", map[string]any{"path": "edit.txt"})
	ambiguous := invokeTool(t, set, "edit", map[string]any{"path": "edit.txt", "old_text": "same", "new_text": "changed"})
	if got, _ := ambiguous["error"].(string); !strings.Contains(got, "matched 2 locations") {
		t.Fatalf("ambiguous edit response = %#v", ambiguous)
	}
	noMatch := invokeTool(t, set, "edit", map[string]any{"path": "edit.txt", "old_text": strings.Repeat("x", 199) + "é", "new_text": "changed"})
	if got, _ := noMatch["error"].(string); !strings.Contains(got, "did not match") {
		t.Fatalf("no-match edit response = %#v", noMatch)
	}
	tooLarge := invokeTool(t, set, "edit", map[string]any{"path": "edit.txt", "old_text": "same", "new_text": strings.Repeat("x", 20), "replace_all": true})
	if got, _ := tooLarge["error"].(string); !strings.Contains(got, "file is too large") {
		t.Fatalf("oversized edit response = %#v", tooLarge)
	}
	writeFile(t, directory, "edit.txt", "changed on disk\n")
	staleEdit := invokeTool(t, set, "edit", map[string]any{"path": "edit.txt", "old_text": "same", "new_text": "changed", "replace_all": true})
	if got, _ := staleEdit["error"].(string); !strings.Contains(got, "changed") {
		t.Fatalf("stale edit response = %#v", staleEdit)
	}
}

func TestPublicWriteHandlesExistingTargetsAndExhaustedBudget(t *testing.T) {
	directory := t.TempDir()
	writeFile(t, directory, "same.txt", "same\n")
	if err := os.Mkdir(filepath.Join(directory, "directory"), 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := workspace.NewRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	budget := exhaustedBudget("public-session")
	set, err := codingtools.New(codingtools.Config{
		Root: root, Queue: workspace.NewMutationQueue(), Ledger: workspace.NewLedger(), Touch: workspace.NewTouchBus(),
		Budget: budget, WarningSink: warning.DiscardSink{},
	})
	if err != nil {
		t.Fatal(err)
	}
	invokeTool(t, set, "read", map[string]any{"path": "same.txt"})
	unchanged := invokeTool(t, set, "write", map[string]any{"path": "same.txt", "content": "same\n"})
	if unchanged["diff"] != "" || unchanged["truncated"] != false {
		t.Fatalf("unchanged write response = %#v", unchanged)
	}
	invokeTool(t, set, "read", map[string]any{"path": "same.txt"})
	changed := invokeTool(t, set, "write", map[string]any{"path": "same.txt", "content": "changed\n"})
	if diff, _ := changed["diff"].(string); !strings.Contains(diff, "reason=budget") {
		t.Fatalf("budgeted write response = %#v", changed)
	}
	directoryResponse := invokeTool(t, set, "write", map[string]any{"path": "directory", "content": "x"})
	if got, _ := directoryResponse["error"].(string); !strings.Contains(got, "is a directory") {
		t.Fatalf("directory write response = %#v", directoryResponse)
	}
	rootResponse := invokeTool(t, set, "write", map[string]any{"path": ".", "content": "x"})
	if got, _ := rootResponse["error"].(string); !strings.Contains(got, "outside root") {
		t.Fatalf("root write response = %#v", rootResponse)
	}
}

func TestPublicSearchToolsReportSkippedAndBoundedResults(t *testing.T) {
	directory := t.TempDir()
	writeFile(t, directory, "search/a.txt", "before\nneedle\nafter\n")
	writeFile(t, directory, "search/binary.txt", string([]byte{'n', 0, 'x'}))
	writeFile(t, directory, "search/large.txt", strings.Repeat("z", (2<<20)+1))
	writeFile(t, directory, "search/other.go", "Needle\n")
	writeFile(t, directory, "search/long.txt", strings.Repeat("x", (1<<20)+1)+"\nneedle\n")
	if err := os.Symlink("a.txt", filepath.Join(directory, "search", "link.txt")); err != nil {
		t.Logf("symlink unavailable: %v", err)
	}
	root, err := workspace.NewRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	sink := &warning.SliceSink{}
	bus := workspace.NewTouchBus()
	set, err := codingtools.New(codingtools.Config{
		Root: root, Queue: workspace.NewMutationQueue(), Ledger: workspace.NewLedger(), Touch: bus,
		Budget: outputlimit.NewBudget(1 << 20), WarningSink: sink, MaxGrepFileBytes: 2 << 20, MaxTouchEvents: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := invokeTool(t, set, "grep", map[string]any{
		"pattern": "needle", "path": "search", "case_insensitive": true, "context_lines": 1, "max_results": 1,
	})
	if result["match_count"] != float64(1) || result["truncated"] != true || result["skipped_binary"] != float64(1) || result["skipped_too_large"] != float64(1) || result["skipped_long_lines"] != float64(1) {
		t.Fatalf("grep result = %#v", result)
	}
	if warnings := sink.Warnings(); len(warnings) != 2 || warnings[1].Code != warning.WarnContextTouchOverflow {
		t.Fatalf("grep warnings = %#v", warnings)
	}
	filtered := invokeTool(t, set, "grep", map[string]any{"pattern": "needle", "path": "search/a.txt", "glob": "*.go"})
	if filtered["match_count"] != float64(0) {
		t.Fatalf("file glob response = %#v", filtered)
	}
	for _, test := range []struct {
		pattern string
		kind    string
	}{
		{pattern: `(a)\1`, kind: "backreference"},
		{pattern: "(?=a)", kind: "lookahead"},
		{pattern: "(?!a)", kind: "lookahead"},
		{pattern: "(?<=a)", kind: "lookbehind"},
		{pattern: "(?<!a)", kind: "lookbehind"},
	} {
		response := invokeTool(t, set, "grep", map[string]any{"pattern": test.pattern, "path": "."})
		if got, _ := response["error"].(string); !strings.Contains(got, test.kind) {
			t.Fatalf("pattern %q response = %#v, want %q classification", test.pattern, response, test.kind)
		}
	}
	named := invokeTool(t, set, "grep", map[string]any{"pattern": "(?<name>after)", "path": "search/a.txt"})
	if named["match_count"] != float64(1) {
		t.Fatalf("named capture response = %#v", named)
	}
	literal := invokeTool(t, set, "grep", map[string]any{"pattern": "Needle", "path": "search/other.go", "literal": true})
	if literal["match_count"] != float64(1) {
		t.Fatalf("literal grep response = %#v", literal)
	}
	escaped := invokeTool(t, set, "grep", map[string]any{"pattern": `Needle\.`, "path": "search/other.go"})
	if escaped["match_count"] != float64(0) {
		t.Fatalf("escaped grep response = %#v", escaped)
	}
	invalid := invokeTool(t, set, "grep", map[string]any{"pattern": "[", "path": "search/other.go"})
	if got, _ := invalid["error"].(string); !strings.Contains(got, "unsupported pattern") {
		t.Fatalf("invalid regexp response = %#v", invalid)
	}
}

func TestPublicGrepDropsMatchesToFitOutputLimit(t *testing.T) {
	directory := t.TempDir()
	writeFile(t, directory, "many.txt", strings.Repeat("needle with enough trailing text to make the result large\n", 20))
	root, err := workspace.NewRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	policy := outputlimit.Policy{MaxBytes: 300, MaxLines: 100, MaxLineBytes: 300, HeadFraction: 0.5}
	set, err := codingtools.New(codingtools.Config{
		Root: root, Queue: workspace.NewMutationQueue(), Ledger: workspace.NewLedger(), Touch: workspace.NewTouchBus(),
		Output: policy, Budget: outputlimit.NewBudget(100), WarningSink: warning.DiscardSink{},
	})
	if err != nil {
		t.Fatal(err)
	}
	result := invokeTool(t, set, "grep", map[string]any{"pattern": "needle", "path": "many.txt"})
	if result["truncated"] != true || result["match_count"].(float64) >= 20 {
		t.Fatalf("bounded grep response = %#v", result)
	}
}

func TestPublicSearchAndListingClassifyWorkspacePaths(t *testing.T) {
	directory := t.TempDir()
	writeFile(t, directory, "file", "text")
	if err := os.Mkdir(filepath.Join(directory, "dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := workspace.NewRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	set, err := codingtools.New(codingtools.Config{
		Root: root, Queue: workspace.NewMutationQueue(), Ledger: workspace.NewLedger(), Touch: workspace.NewTouchBus(),
		Budget: outputlimit.NewBudget(1 << 20), WarningSink: warning.DiscardSink{},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		tool string
		args map[string]any
		want string
	}{
		{tool: "grep", args: map[string]any{"pattern": "x", "path": "missing"}, want: "path not found"},
		{tool: "grep", args: map[string]any{"pattern": "x", "path": "../outside"}, want: "outside root"},
		{tool: "find", args: map[string]any{"glob": "*", "path": "missing"}, want: "path not found"},
		{tool: "find", args: map[string]any{"glob": "*", "path": "../outside"}, want: "outside root"},
		{tool: "find", args: map[string]any{"glob": "*", "path": "file"}, want: "not a directory"},
		{tool: "ls", args: map[string]any{"path": "missing"}, want: "path not found"},
		{tool: "ls", args: map[string]any{"path": "../outside"}, want: "outside root"},
	} {
		response := invokeTool(t, set, test.tool, test.args)
		if got, _ := response["error"].(string); !strings.Contains(got, test.want) {
			t.Fatalf("%s response = %#v, want %q", test.tool, response, test.want)
		}
	}
}

func TestPublicFindAndListApplyTypesOrderingAndBudgets(t *testing.T) {
	directory := t.TempDir()
	writeFile(t, directory, ".hidden", "hidden")
	writeFile(t, directory, "a/old.txt", "old")
	writeFile(t, directory, "b/new.txt", "new")
	oldTime := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(directory, "a", "old.txt"), oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	symlinkAvailable := os.Symlink("a/old.txt", filepath.Join(directory, "link.txt")) == nil
	root, err := workspace.NewRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	set, err := codingtools.New(codingtools.Config{
		Root: root, Queue: workspace.NewMutationQueue(), Ledger: workspace.NewLedger(),
		Touch: workspace.NewTouchBus(), Budget: outputlimit.NewBudget(1 << 20), WarningSink: warning.DiscardSink{},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertFindAndListResults(t, set, symlinkAvailable)
	assertExhaustedSearchBudgets(t, root)
}

func assertFindAndListResults(t *testing.T, set *codingtools.Set, symlinkAvailable bool) {
	t.Helper()
	modified := invokeTool(t, set, "find", map[string]any{"glob": "**/*.txt", "sort_by": "modified", "type": "file", "max_results": 1})
	paths, _ := modified["paths"].([]any)
	if len(paths) != 1 || paths[0] != "b/new.txt" || modified["truncated"] != true {
		t.Fatalf("modified find response = %#v", modified)
	}
	directories := invokeTool(t, set, "find", map[string]any{"glob": "*", "type": "dir"})
	if paths, _ := directories["paths"].([]any); len(paths) != 2 {
		t.Fatalf("directory find response = %#v", directories)
	}
	if symlinkAvailable {
		links := invokeTool(t, set, "find", map[string]any{"glob": "*.txt", "type": "symlink"})
		if paths, _ := links["paths"].([]any); len(paths) != 1 || paths[0] != "link.txt" {
			t.Fatalf("symlink find response = %#v", links)
		}
	}
	listing := invokeTool(t, set, "ls", map[string]any{"path": ".", "max_depth": 2, "show_hidden": true, "max_results": 2})
	entries, _ := listing["entries"].([]any)
	if len(entries) != 2 || listing["truncated"] != true {
		t.Fatalf("bounded list response = %#v", listing)
	}
	nested := invokeTool(t, set, "ls", map[string]any{"path": "a"})
	if entries, _ := nested["entries"].([]any); len(entries) != 1 {
		t.Fatalf("nested list response = %#v", nested)
	}
}

func assertExhaustedSearchBudgets(t *testing.T, root *workspace.Root) {
	t.Helper()
	budget := exhaustedBudget("public-session")
	boundedSet, err := codingtools.New(codingtools.Config{
		Root: root, Queue: workspace.NewMutationQueue(), Ledger: workspace.NewLedger(),
		Touch: workspace.NewTouchBus(), Budget: budget, WarningSink: warning.DiscardSink{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result := invokeTool(t, boundedSet, "grep", map[string]any{"pattern": "new", "path": "."}); result["match_count"] != float64(0) || result["truncated"] != true {
		t.Fatalf("exhausted grep response = %#v", result)
	}
	if result := invokeTool(t, boundedSet, "find", map[string]any{"glob": "*"}); result["truncated"] != true {
		t.Fatalf("exhausted find response = %#v", result)
	}
	if result := invokeTool(t, boundedSet, "ls", map[string]any{}); result["truncated"] != true {
		t.Fatalf("exhausted list response = %#v", result)
	}
}

func exhaustedBudget(sessionID string) *outputlimit.Budget {
	budget := outputlimit.NewBudget(1)
	reservation := budget.Reserve(sessionID, 1)
	budget.Consume(sessionID, reservation.ID, reservation.Grant)
	return budget
}

func invokeTool(t *testing.T, set *codingtools.Set, name string, args map[string]any) map[string]any {
	t.Helper()
	tool, ok := set.Tool(name)
	if !ok {
		t.Fatalf("tool %q not found", name)
	}
	llm := &singleToolModel{name: name, args: args}
	agentValue, err := llmagent.New(llmagent.Config{Name: "public_contract_agent", Model: llm, Tools: []adktool.Tool{tool}})
	if err != nil {
		t.Fatal(err)
	}
	runnerValue, err := runner.NewInMemory("public-contract", agentValue)
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	for event, runErr := range runnerValue.Run(t.Context(), "user", "public-session", genai.NewContentFromText("invoke", genai.RoleUser), agent.RunConfig{}) {
		if runErr != nil {
			t.Fatal(runErr)
		}
		if found := publicToolResponse(t, event, name); found != nil {
			response = found
		}
	}
	if response == nil {
		t.Fatalf("tool %q emitted no function response", name)
	}
	return response
}

func publicToolResponse(t *testing.T, event *session.Event, name string) map[string]any {
	t.Helper()
	if event == nil || event.Content == nil {
		return nil
	}
	for _, part := range event.Content.Parts {
		if part == nil || part.FunctionResponse == nil || part.FunctionResponse.Name != name {
			continue
		}
		encoded, err := json.Marshal(part.FunctionResponse.Response)
		if err != nil {
			t.Fatal(err)
		}
		var response map[string]any
		if err := json.Unmarshal(encoded, &response); err != nil {
			t.Fatal(err)
		}
		return response
	}
	return nil
}

func runPublicToolWithCancellation(t *testing.T, set *codingtools.Set, name string, args map[string]any, cancelAfter int) {
	t.Helper()
	ctx := cancellationAfterChecks(cancelAfter)
	_, runErr := runPublicTool(t, set, name, args, ctx)
	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		t.Fatalf("%s cancellation after check %d = %v", name, cancelAfter, runErr)
	}
}

func runPublicTool(t *testing.T, set *codingtools.Set, name string, args map[string]any, ctx context.Context) (map[string]any, error) {
	t.Helper()
	tool, ok := set.Tool(name)
	if !ok {
		t.Fatalf("tool %q missing", name)
	}
	runnable := tool.(interface {
		Run(agent.Context, any) (map[string]any, error)
	})
	agentContext := &publicAgentContext{StrictContextMock: agent.NewStrictContextMock(ctx)}
	return runnable.Run(agentContext, args)
}

type singleToolModel struct {
	name  string
	args  map[string]any
	calls int
}

type publicAgentContext struct {
	agent.StrictContextMock
}

type stagedErrorContext struct {
	mu     sync.Mutex
	calls  int
	onCall func(int) error
}

func cancellationAfterChecks(after int) context.Context {
	return &stagedErrorContext{onCall: func(call int) error {
		if call >= after {
			return context.Canceled
		}
		return nil
	}}
}

func effectAfterChecks(after int, effect func()) context.Context {
	return &stagedErrorContext{onCall: func(call int) error {
		if call == after {
			effect()
		}
		return nil
	}}
}

func (c *stagedErrorContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *stagedErrorContext) Done() <-chan struct{}       { return nil }
func (c *stagedErrorContext) Value(any) any               { return nil }

func (c *stagedErrorContext) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return c.onCall(c.calls)
}

func (*publicAgentContext) SessionID() string      { return "public-session" }
func (*publicAgentContext) FunctionCallID() string { return "public-call" }
func (*publicAgentContext) ToolConfirmation() *toolconfirmation.ToolConfirmation {
	return nil
}

func (*singleToolModel) Name() string { return "public-contract-model" }

func (modelValue *singleToolModel) GenerateContent(_ context.Context, request *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if modelValue.calls == 0 {
			if request.Tools[modelValue.name] == nil {
				yield(nil, errors.New("requested tool is absent"))
				return
			}
			modelValue.calls++
			yield(&model.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "call", Name: modelValue.name, Args: modelValue.args}}}}}, nil)
			return
		}
		modelValue.calls++
		yield(&model.LLMResponse{Content: genai.NewContentFromText("done", genai.RoleModel)}, nil)
	}
}

func writeFile(t *testing.T, root, path, content string) {
	t.Helper()
	absolute := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(absolute, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

var _ model.LLM = (*singleToolModel)(nil)
