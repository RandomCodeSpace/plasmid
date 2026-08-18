package codingtools

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"testing"

	"github.com/plasmid-dev/plasmid/outputlimit"
	"github.com/plasmid-dev/plasmid/shellexec"
	"github.com/plasmid-dev/plasmid/workspace"
)

func newBashToolForTest(t *testing.T, configure func(*Config)) (*bashHandler, *outputlimit.Budget) {
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
	tool, err := newBashHandler(cfg)
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
		if _, err := newBashHandler(cfg); err == nil {
			t.Fatal("constructor accepted a missing dependency")
		}
	}
	tool, err := NewBashTool(valid)
	if err != nil {
		t.Fatal(err)
	}
	if tool.Name() != "bash" || tool.Description() != BashDescription || tool.IsLongRunning() {
		t.Fatal("bash metadata drifted")
	}
	first := BashInputSchema()
	first[0] ^= 0xff
	if string(first) == string(BashInputSchema()) {
		t.Fatal("input schema aliases tool state")
	}
	if _, err := newBashHandler(Config{Root: root, Shell: executor, Budget: outputlimit.NewBudget(1), Output: outputlimit.Policy{MaxBytes: -1}}); !errors.Is(err, outputlimit.ErrInvalidLimit) {
		t.Fatalf("invalid output policy error = %v", err)
	}
}

func TestBashToolRunsContainedCommandsAndPreservesResults(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell command assertions are Unix-specific")
	}
	tool, budget := newBashToolForTest(t, nil)
	result, err := adaptTestHandler(t, tool.call)(context.Background(), "session", map[string]any{"command": "printf out; printf err >&2; exit 7"})
	if err != nil {
		t.Fatal(err)
	}
	decoded := decodeBashResult(t, result)
	if decoded.Stdout != "out" || decoded.Stderr != "err" || decoded.ExitCode != 7 || decoded.TimedOut || decoded.Killed {
		t.Fatalf("bash result = %#v", decoded)
	}
	if used, _ := budget.Report("session"); used != len("out")+len("err") {
		t.Fatalf("budget used = %d", used)
	}
	_, err = adaptTestHandler(t, tool.call)(context.Background(), "session", map[string]any{"command": "true", "dir": "../outside"})
	if !errors.Is(err, ErrPathOutsideRoot) {
		t.Fatalf("outside directory error = %v", err)
	}
}

func TestBashToolTimeoutCancellationAndNoTouch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell command assertions are Unix-specific")
	}
	tool, budget := newBashToolForTest(t, nil)
	result, err := adaptTestHandler(t, tool.call)(context.Background(), "timeout", map[string]any{"command": "sleep 1", "timeout_ms": 10})
	if err != nil {
		t.Fatal(err)
	}
	decoded := decodeBashResult(t, result)
	if !decoded.TimedOut || !strings.Contains(decoded.Stderr, "timeout_ms=10") {
		t.Fatalf("timeout result = %#v", decoded)
	}
	if used, _ := budget.Report("timeout"); used != len(decoded.Stdout)+len(decoded.Stderr) {
		t.Fatalf("timeout budget = %d", used)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err = adaptTestHandler(t, tool.call)(ctx, "cancel", map[string]any{"command": "true"})
	if !errors.Is(err, context.Canceled) || result != nil {
		t.Fatalf("cancel result = %#v, %v", result, err)
	}
	if used, _ := budget.Report("cancel"); used != 0 {
		t.Fatalf("cancel budget = %d", used)
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

	result, err := adaptTestHandler(t, tool.call)(context.Background(), "session", map[string]any{"command": "printf stdout; printf stderr >&2"})
	if err != nil {
		t.Fatal(err)
	}
	decoded := decodeBashResult(t, result)
	if decoded.Stdout != "" || decoded.Stderr != "" || !decoded.Truncated {
		t.Fatalf("exhausted result = %#v", decoded)
	}
	if decoded.StdoutReport.Reason != outputlimit.ReasonBudget || decoded.StderrReport.Reason != outputlimit.ReasonBudget {
		t.Fatalf("exhausted reports = %#v, %#v", decoded.StdoutReport, decoded.StderrReport)
	}
	result, err = adaptTestHandler(t, tool.call)(context.Background(), "session", map[string]any{"command": "printf stdout"})
	if err != nil {
		t.Fatal(err)
	}
	if decoded = decodeBashResult(t, result); decoded.Stdout != "" || decoded.Stderr != "" {
		t.Fatalf("single-stream exhausted result = %#v", decoded)
	}
}
