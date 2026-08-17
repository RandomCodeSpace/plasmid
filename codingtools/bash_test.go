package codingtools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/plasmid-dev/plasmid/loop"
	"github.com/plasmid-dev/plasmid/outputlimit"
	"github.com/plasmid-dev/plasmid/shellexec"
	"github.com/plasmid-dev/plasmid/workspace"
)

func newBashToolForTest(t *testing.T, configure func(*Config)) (loop.Tool, *outputlimit.Budget) {
	t.Helper()
	root, err := workspace.NewRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	output := outputlimit.Defaults()
	executor, err := shellexec.New(shellexec.Config{Root: root, Shell: "sh", OutputLimit: output})
	if err != nil {
		t.Skipf("shell unavailable: %v", err)
	}
	budget := outputlimit.NewBudget(10000)
	cfg := Config{Root: root, Shell: executor, Budget: budget, Output: output}
	if configure != nil {
		configure(&cfg)
	}
	tool, err := NewBashTool(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return tool, budget
}

func decodeBashResult(t *testing.T, value map[string]any) BashResult {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var result BashResult
	if err := json.Unmarshal(encoded, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestNewBashToolContract(t *testing.T) {
	root, err := workspace.NewRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	executor, err := shellexec.New(shellexec.Config{Root: root, Shell: "sh"})
	if err != nil {
		t.Skipf("shell unavailable: %v", err)
	}
	valid := Config{Root: root, Shell: executor, Budget: outputlimit.NewBudget(10000)}
	for _, mutate := range []func(*Config){
		func(cfg *Config) { cfg.Root = nil },
		func(cfg *Config) { cfg.Shell = nil },
		func(cfg *Config) { cfg.Budget = nil },
	} {
		cfg := valid
		mutate(&cfg)
		if _, err := NewBashTool(cfg); err == nil {
			t.Fatal("constructor accepted a missing dependency")
		}
	}
	tool, err := NewBashTool(valid)
	if err != nil {
		t.Fatal(err)
	}
	if tool.Name() != "bash" || tool.Description() != BashDescription || !bytes.Equal(tool.InputSchema(), BashInputSchema()) {
		t.Fatal("bash metadata drifted")
	}
	first := tool.InputSchema()
	first[0] ^= 0xff
	if bytes.Equal(first, tool.InputSchema()) {
		t.Fatal("input schema aliases tool state")
	}
	if _, err := NewBashTool(Config{Root: root, Shell: executor, Budget: outputlimit.NewBudget(1), Output: outputlimit.Policy{MaxBytes: -1}}); !errors.Is(err, outputlimit.ErrInvalidLimit) {
		t.Fatalf("invalid output policy error = %v", err)
	}
}

func TestDecodeBashArgsStrict(t *testing.T) {
	for _, test := range []struct {
		name string
		args map[string]any
		want BashArgs
		err  string
	}{
		{"defaults", map[string]any{"command": "true"}, BashArgs{Command: "true", Dir: ".", TimeoutMS: 120000}, ""},
		{"values", map[string]any{"command": "echo ok", "dir": "sub", "timeout_ms": json.Number("2")}, BashArgs{Command: "echo ok", Dir: "sub", TimeoutMS: 2}, ""},
		{"missing command", map[string]any{}, BashArgs{}, "required"},
		{"empty command", map[string]any{"command": " \t"}, BashArgs{}, "empty"},
		{"command type", map[string]any{"command": 1}, BashArgs{}, "string"},
		{"dir type", map[string]any{"command": "true", "dir": 1}, BashArgs{}, "dir must be a string"},
		{"timeout fraction", map[string]any{"command": "true", "timeout_ms": 1.5}, BashArgs{}, "integer"},
		{"timeout zero", map[string]any{"command": "true", "timeout_ms": 0}, BashArgs{}, "positive"},
		{"unknown", map[string]any{"command": "true", "extra": true}, BashArgs{}, "unknown"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := decodeBashArgs(test.args, defaultBashTimeout)
			if test.err != "" {
				if err == nil || !strings.Contains(err.Error(), test.err) {
					t.Fatalf("error = %v, want %q", err, test.err)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("decodeBashArgs() = %#v, %v; want %#v, nil", got, err, test.want)
			}
		})
	}
}

func TestBashToolRunsContainedCommandsAndPreservesResults(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell command assertions are Unix-specific")
	}
	tool, budget := newBashToolForTest(t, nil)
	result, err := tool.Call(context.Background(), loop.ToolCall{ID: "call", SessionID: "session", Args: map[string]any{"command": "printf out; printf err >&2; exit 7"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.CallID != "call" {
		t.Fatalf("call ID = %q", result.CallID)
	}
	decoded := decodeBashResult(t, result.Content)
	if decoded.Stdout != "out" || decoded.Stderr != "err" || decoded.ExitCode != 7 || decoded.TimedOut || decoded.Killed {
		t.Fatalf("bash result = %#v", decoded)
	}
	if used, _ := budget.Report("session"); used != len("out")+len("err") {
		t.Fatalf("budget used = %d", used)
	}
	_, err = tool.Call(context.Background(), loop.ToolCall{SessionID: "session", Args: map[string]any{"command": "true", "dir": "../outside"}})
	if !errors.Is(err, ErrPathOutsideRoot) {
		t.Fatalf("outside directory error = %v", err)
	}
}

func TestBashToolTimeoutCancellationAndNoTouch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell command assertions are Unix-specific")
	}
	tool, budget := newBashToolForTest(t, nil)
	result, err := tool.Call(context.Background(), loop.ToolCall{SessionID: "timeout", Args: map[string]any{"command": "sleep 1", "timeout_ms": 10}})
	if err != nil {
		t.Fatal(err)
	}
	decoded := decodeBashResult(t, result.Content)
	if !decoded.TimedOut || !strings.Contains(decoded.Stderr, "timeout_ms=10") {
		t.Fatalf("timeout result = %#v", decoded)
	}
	if used, _ := budget.Report("timeout"); used != len(decoded.Stdout)+len(decoded.Stderr) {
		t.Fatalf("timeout budget = %d", used)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err = tool.Call(ctx, loop.ToolCall{ID: "cancel", SessionID: "cancel", Args: map[string]any{"command": "true"}})
	if !errors.Is(err, context.Canceled) || result.CallID != "cancel" || result.Content != nil {
		t.Fatalf("cancel result = %#v, %v", result, err)
	}
	if used, _ := budget.Report("cancel"); used != 0 {
		t.Fatalf("cancel budget = %d", used)
	}
}

func TestBashToolUsesConfiguredDefaultTimeout(t *testing.T) {
	args, err := decodeBashArgs(map[string]any{"command": "true"}, 37*time.Millisecond)
	if err != nil || args.TimeoutMS != 37 {
		t.Fatalf("default timeout args = %#v, %v", args, err)
	}
}

func TestBashToolSuppressesOutputAfterBudgetExhaustion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell command assertions are Unix-specific")
	}
	budget := outputlimit.NewBudget(1)
	reservation := budget.Reserve("session", 1)
	budget.Consume("session", reservation.ID, reservation.Grant)
	tool, _ := newBashToolForTest(t, func(cfg *Config) { cfg.Budget = budget })

	result, err := tool.Call(context.Background(), loop.ToolCall{SessionID: "session", Args: map[string]any{"command": "printf stdout; printf stderr >&2"}})
	if err != nil {
		t.Fatal(err)
	}
	decoded := decodeBashResult(t, result.Content)
	if decoded.Stdout != "" || decoded.Stderr != "" || !decoded.Truncated {
		t.Fatalf("exhausted result = %#v", decoded)
	}
	if decoded.StdoutReport.Reason != outputlimit.ReasonBudget || decoded.StderrReport.Reason != outputlimit.ReasonBudget {
		t.Fatalf("exhausted reports = %#v, %#v", decoded.StdoutReport, decoded.StderrReport)
	}
}
