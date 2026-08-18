//go:build unix

package shellexec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/plasmid-dev/plasmid/outputlimit"
	"github.com/plasmid-dev/plasmid/workspace"
)

func TestNewValidationResolutionAndDefaults(t *testing.T) {
	root := testRoot(t)
	shell := testShell(t)

	if _, err := New(Config{Shell: shell}); !errors.Is(err, errNilRoot) {
		t.Fatalf("nil root error = %v", err)
	}
	if _, err := New(Config{Root: root, Shell: filepath.Join(t.TempDir(), "missing")}); !errors.Is(err, ErrNoShell) {
		t.Fatalf("missing explicit shell error = %v", err)
	}
	if _, err := New(Config{Root: root, Shell: shell, OutputLimit: outputlimit.Policy{MaxBytes: -1}}); !errors.Is(err, outputlimit.ErrInvalidLimit) {
		t.Fatalf("invalid output limit error = %v", err)
	}

	executor, err := New(Config{Root: root, Shell: shell})
	if err != nil {
		t.Fatal(err)
	}
	if executor.Shell() != shell {
		t.Fatalf("Shell() = %q, want %q", executor.Shell(), shell)
	}
	if executor.defaultTimeout != defaultTimeout || executor.maxTimeout != maximumTimeout || executor.killGrace != defaultKillGrace {
		t.Fatalf("defaults = %v, %v, %v", executor.defaultTimeout, executor.maxTimeout, executor.killGrace)
	}
	if executor.outputLimit != (outputlimit.Policy{}) {
		t.Fatalf("output limit = %#v, want unlimited zero policy", executor.outputLimit)
	}

	executor, err = New(Config{
		Root:           root,
		Shell:          shell,
		DefaultTimeout: -1,
		MaxTimeout:     25 * time.Millisecond,
		KillGrace:      -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if executor.defaultTimeout != 25*time.Millisecond || executor.killGrace != defaultKillGrace {
		t.Fatalf("normalized defaults = %v, %v", executor.defaultTimeout, executor.killGrace)
	}
}

func TestNewShellFallbackAndMissingShell(t *testing.T) {
	root := testRoot(t)
	actual := testShell(t)
	preferredBin := t.TempDir()
	preferredBash := filepath.Join(preferredBin, "bash")
	if err := os.Symlink(actual, preferredBash); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(actual, filepath.Join(preferredBin, "sh")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", preferredBin)
	executor, err := New(Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if executor.Shell() != preferredBash {
		t.Fatalf("preferred shell = %q, want %q", executor.Shell(), preferredBash)
	}

	bin := t.TempDir()
	fallback := filepath.Join(bin, "sh")
	if err := os.Symlink(actual, fallback); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	executor, err = New(Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if executor.Shell() != fallback {
		t.Fatalf("fallback shell = %q, want %q", executor.Shell(), fallback)
	}

	t.Setenv("PATH", t.TempDir())
	if _, err := New(Config{Root: root}); !errors.Is(err, ErrNoShell) {
		t.Fatalf("missing fallback error = %v", err)
	}
}

func TestRunExitClassificationAndStreams(t *testing.T) {
	executor := testExecutor(t, Config{})
	tests := []struct {
		name     string
		command  string
		exitCode int
		stdout   string
		stderr   string
	}{
		{"success", "printf 'out'; printf 'err' >&2", 0, "out", "err"},
		{"failure", "printf 'failed' >&2; exit 7", 7, "", "failed"},
		{"high exit", "printf 'high' >&2; exit 255", 255, "", "high"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := executor.Run(context.Background(), Request{Command: test.command})
			if err != nil {
				t.Fatal(err)
			}
			if result.ExitCode != test.exitCode || result.Stdout != test.stdout || result.Stderr != test.stderr {
				t.Fatalf("result = %#v", result)
			}
			if result.Signal != "" || result.TimedOut || result.Killed || result.Duration <= 0 {
				t.Fatalf("unexpected termination metadata: %#v", result)
			}
		})
	}

	result, err := executor.Run(context.Background(), Request{Command: "kill -TERM $$"})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != -1 || result.Signal != syscall.SIGTERM.String() || result.TimedOut {
		t.Fatalf("signal result = %#v", result)
	}
}

func TestRunMergedReturnsZeroValueWhenSetupFails(t *testing.T) {
	executor := testExecutor(t, Config{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := executor.RunMerged(ctx, Request{Command: "printf should-not-run"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunMerged error = %v, want cancellation", err)
	}
	if result != (Result{}) {
		t.Fatalf("RunMerged result = %#v, want zero value", result)
	}
}

func TestRunSuppliesStdinAndEOF(t *testing.T) {
	executor := testExecutor(t, Config{})
	result, err := executor.Run(context.Background(), Request{
		Command: "IFS= read -r first; printf '<%s>' \"$first\"; if IFS= read -r second; then printf '<%s>' \"$second\"; else printf '<eof>'; fi",
		Stdin:   "first line\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Stdout != "<first line><eof>" {
		t.Fatalf("stdout = %q", result.Stdout)
	}

	result, err = executor.Run(context.Background(), Request{Command: "if IFS= read -r value; then printf bad; else printf eof; fi"})
	if err != nil || result.Stdout != "eof" {
		t.Fatalf("empty stdin result = %#v, %v", result, err)
	}
}

func TestRunDirectoryResolution(t *testing.T) {
	root, rootDir, nested, outside := directoryFixture(t)
	executor := testExecutorWithRoot(t, root, Config{})

	tests := []struct {
		name    string
		dir     string
		wantDir string
		wantErr error
	}{
		{"empty uses root", "", ".", nil},
		{"relative nested", "nested", "nested", nil},
		{"absolute nested", nested, "nested", nil},
		{"inside symlink", "inside-link", "nested", nil},
		{"lexical escape", "../outside", "", workspace.ErrOutsideRoot},
		{"absolute escape", outside, "", workspace.ErrOutsideRoot},
		{"symlink escape", "outside-link", "", workspace.ErrOutsideRoot},
		{"missing", "missing", "", workspace.ErrNotFound},
		{"file", "file", "", workspace.ErrNotDirectory},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertRunDirectory(t, executor, rootDir, nested, test.dir, test.wantDir, test.wantErr)
		})
	}

	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(outside); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(original) })
	result, err := executor.Run(context.Background(), Request{Command: "pwd", Dir: "nested"})
	if err != nil || result.Dir != "nested" || strings.TrimSpace(result.Stdout) != nested {
		t.Fatalf("cwd-independent run = %#v, %v", result, err)
	}
}

func assertRunDirectory(
	t *testing.T,
	executor *Executor,
	rootDir string,
	nested string,
	dir string,
	wantDir string,
	wantErr error,
) {
	t.Helper()
	result, err := executor.Run(context.Background(), Request{Command: "pwd", Dir: dir})
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want %v", err, wantErr)
	}
	if wantErr != nil {
		return
	}
	wantPWD := rootDir
	if wantDir == "nested" {
		wantPWD = nested
	}
	if result.Dir != wantDir || strings.TrimSpace(result.Stdout) != wantPWD {
		t.Fatalf("result = %#v, want relative dir %q", result, wantDir)
	}
}

func directoryFixture(t *testing.T) (*workspace.Root, string, string, string) {
	t.Helper()
	base := t.TempDir()
	rootDir := filepath.Join(base, "root")
	nested := filepath.Join(rootDir, "nested")
	outside := filepath.Join(base, "outside")
	for _, dir := range []string{nested, outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(rootDir, "file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(nested, filepath.Join(rootDir, "inside-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(rootDir, "outside-link")); err != nil {
		t.Fatal(err)
	}
	root, err := workspace.NewRoot(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	return root, rootDir, nested, outside
}

func TestRunUnreadableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory execute permissions")
	}
	rootDir := t.TempDir()
	locked := filepath.Join(rootDir, "locked")
	if err := os.Mkdir(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	root, err := workspace.NewRoot(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	executor := testExecutorWithRoot(t, root, Config{})
	if _, err := executor.Run(context.Background(), Request{Command: ":", Dir: "locked"}); err == nil {
		t.Fatal("unreadable directory run succeeded")
	}
}

func TestRunSpawnFailure(t *testing.T) {
	rootDir := t.TempDir()
	root, err := workspace.NewRoot(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	shellLink := filepath.Join(rootDir, "temporary-shell")
	if err := os.Symlink(testShell(t), shellLink); err != nil {
		t.Fatal(err)
	}
	executor, err := New(Config{Root: root, Shell: shellLink})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(shellLink); err != nil {
		t.Fatal(err)
	}
	if _, err := executor.Run(context.Background(), Request{Command: ":"}); err == nil {
		t.Fatal("spawn with removed shell succeeded")
	}
}

func TestRunEnvironment(t *testing.T) {
	t.Setenv("PLASMID_INHERITED_AT_RUN", "inherited")
	inheriting := testExecutor(t, Config{})
	inherited, err := inheriting.Run(context.Background(), Request{Command: "printf '%s' \"$PLASMID_INHERITED_AT_RUN\""})
	if err != nil || inherited.Stdout != "inherited" {
		t.Fatalf("inherited environment result = %#v, %v", inherited, err)
	}

	executor := testExecutor(t, Config{
		Env:      []string{"BASE=first", "BASE=last"},
		ExtraEnv: map[string]string{"TERM": "extra", "CUSTOM": "set"},
	})
	command := "printf '%s|%s|%s|%s|%s|%s|%s|%s|%s' \"$BASE\" \"$TERM\" \"$PAGER\" \"$GIT_PAGER\" \"$GIT_TERMINAL_PROMPT\" \"$DEBIAN_FRONTEND\" \"$NO_COLOR\" \"$CUSTOM\" \"${CI-unset}\""
	result, err := executor.Run(context.Background(), Request{Command: command})
	if err != nil {
		t.Fatal(err)
	}
	want := "last|extra|cat|cat|0|noninteractive|1|set|unset"
	if result.Stdout != want {
		t.Fatalf("stdout = %q, want %q", result.Stdout, want)
	}

	t.Setenv("PLASMID_MUST_NOT_INHERIT", "inherited")
	replacement := testExecutor(t, Config{Env: []string{}})
	result, err = replacement.Run(context.Background(), Request{Command: "printf '%s' \"${PLASMID_MUST_NOT_INHERIT-unset}\""})
	if err != nil || result.Stdout != "unset" {
		t.Fatalf("empty replacement environment result = %#v, %v", result, err)
	}
}

func TestRunTimeoutCancellationAndClamp(t *testing.T) {
	tests := []struct {
		name    string
		request time.Duration
	}{
		{"explicit timeout", 35 * time.Millisecond},
		{"zero uses default", 0},
		{"negative uses default", -time.Second},
		{"over maximum clamps", time.Hour},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			executor := testExecutor(t, Config{
				DefaultTimeout: 35 * time.Millisecond,
				MaxTimeout:     45 * time.Millisecond,
				KillGrace:      20 * time.Millisecond,
			})
			started := time.Now()
			result, err := executor.Run(context.Background(), Request{
				Command: "printf started; trap '' TERM; while :; do :; done",
				Timeout: test.request,
			})
			if err != nil {
				t.Fatal(err)
			}
			if !result.TimedOut || !result.Killed || !strings.Contains(result.Stdout, "started") || result.ExitCode != -1 {
				t.Fatalf("timeout result = %#v", result)
			}
			if elapsed := time.Since(started); elapsed > 2*time.Second {
				t.Fatalf("timeout returned after %v", elapsed)
			}
		})
	}

	executor := testExecutor(t, Config{KillGrace: 20 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(40 * time.Millisecond)
		cancel()
	}()
	result, err := executor.Run(ctx, Request{Command: "printf started; trap '' TERM; while :; do :; done", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if result.TimedOut || !result.Killed || result.ExitCode != -1 || !strings.Contains(result.Stdout, "started") {
		t.Fatalf("cancellation result = %#v", result)
	}

	canceled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	if _, err := executor.Run(canceled, Request{Command: "printf should-not-run"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled context error = %v", err)
	}
}

func TestRunProcessCanExitDuringGrace(t *testing.T) {
	executor := testExecutor(t, Config{KillGrace: 500 * time.Millisecond})
	started := time.Now()
	result, err := executor.Run(context.Background(), Request{
		Command: "trap 'exit 0' TERM; printf ready; while :; do :; done",
		Timeout: 40 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.TimedOut || !result.Killed || result.ExitCode != 0 || result.Signal != "" {
		t.Fatalf("graceful timeout result = %#v", result)
	}
	if elapsed := time.Since(started); elapsed >= 500*time.Millisecond {
		t.Fatalf("executor waited through full grace after child exit: %v", elapsed)
	}
}

func TestRunMergedOrderingAndBoundedOutput(t *testing.T) {
	executor := testExecutor(t, Config{OutputLimit: outputlimit.Policy{MaxBytes: 128, HeadFraction: 0.5}})
	result, err := executor.RunMerged(context.Background(), Request{
		Command: "printf 'out-1\\n'; printf 'err-1\\n' >&2; printf 'out-2\\n'; printf 'err-2\\n' >&2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Stdout != "out-1\nerr-1\nout-2\nerr-2\n" || result.Stderr != "" || result.StderrReport != (outputlimit.Report{}) {
		t.Fatalf("merged result = %#v", result)
	}

	result, err = executor.RunMerged(context.Background(), Request{
		Command: "(i=0; while [ \"$i\" -lt 4096 ]; do printf 'oooooooooooooooo'; i=$((i+1)); done) & (i=0; while [ \"$i\" -lt 4096 ]; do printf 'eeeeeeeeeeeeeeee' >&2; i=$((i+1)); done) & wait",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Stderr != "" || !result.StdoutReport.Truncated || result.StdoutReport.OriginalBytes != 131072 || result.StdoutReport.KeptBytes > 128 {
		t.Fatalf("large merged result = %#v", result)
	}
	if len(result.Stdout) > 256 {
		t.Fatalf("bounded merged output rendered %d bytes", len(result.Stdout))
	}
}

func TestLargeOutputHelper(t *testing.T) {
	if os.Getenv("GO_WANT_LARGE_OUTPUT_HELPER") != "1" {
		return
	}
	block := make([]byte, 32<<10)
	for remaining := 100 << 20; remaining > 0; remaining -= len(block) {
		if _, err := os.Stdout.Write(block); err != nil {
			os.Exit(2)
		}
	}
	os.Exit(0)
}

func TestRunHundredMiBOutputRemainsBounded(t *testing.T) {
	executor := testExecutor(t, Config{OutputLimit: outputlimit.Policy{MaxBytes: 128, HeadFraction: 0.5}})
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	result, err := executor.Run(context.Background(), Request{
		Command: "GO_WANT_LARGE_OUTPUT_HELPER=1 " + shellQuote(os.Args[0]) + " -test.run=^TestLargeOutputHelper$",
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if !result.StdoutReport.Truncated || result.StdoutReport.OriginalBytes != 100<<20 || result.StdoutReport.KeptBytes > 128 {
		t.Fatalf("100 MiB output report = %#v", result.StdoutReport)
	}
	if growth := int64(after.HeapInuse) - int64(before.HeapInuse); growth > 16<<20 {
		t.Fatalf("parent heap grew by %d bytes while capturing bounded output", growth)
	}
}

func TestRunOutputLimitsAreIndependent(t *testing.T) {
	executor := testExecutor(t, Config{OutputLimit: outputlimit.Policy{MaxBytes: 64, HeadFraction: 0.5}})
	result, err := executor.Run(context.Background(), Request{
		Command: "(i=0; while [ \"$i\" -lt 4096 ]; do printf 'oooooooooooooooo'; i=$((i+1)); done) & (i=0; while [ \"$i\" -lt 4096 ]; do printf 'eeeeeeeeeeeeeeee' >&2; i=$((i+1)); done) & wait",
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, report := range map[string]outputlimit.Report{"stdout": result.StdoutReport, "stderr": result.StderrReport} {
		if !report.Truncated || report.OriginalBytes != 65536 || report.KeptBytes > 64 {
			t.Errorf("%s report = %#v", name, report)
		}
	}
	if strings.Contains(result.Stdout, "eeee") || strings.Contains(result.Stderr, "oooo") {
		t.Fatalf("streams contaminated: stdout %q stderr %q", result.Stdout, result.Stderr)
	}
}

func TestRunOutputLimitBoundary(t *testing.T) {
	tests := []struct {
		name      string
		command   string
		truncated bool
		original  int
	}{
		{"exact", "printf 12345678", false, 8},
		{"one over", "printf 123456789", true, 9},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			executor := testExecutor(t, Config{OutputLimit: outputlimit.Policy{MaxBytes: 8, HeadFraction: 0.5}})
			result, err := executor.Run(context.Background(), Request{Command: test.command})
			if err != nil {
				t.Fatal(err)
			}
			if result.StdoutReport.Truncated != test.truncated || result.StdoutReport.OriginalBytes != test.original {
				t.Fatalf("report = %#v", result.StdoutReport)
			}
		})
	}
}

func TestRunKillsProcessGroup(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not available")
	}
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	executor := testExecutor(t, Config{KillGrace: 30 * time.Millisecond})
	command := fmt.Sprintf("trap '' TERM; sleep 30 & child=$!; printf '%%s' \"$child\" > %s; wait", shellQuote(pidFile))
	result, err := executor.Run(context.Background(), Request{Command: command, Timeout: 80 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if !result.TimedOut || !result.Killed {
		t.Fatalf("process-group timeout result = %#v", result)
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for processExists(pid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if processExists(pid) {
		t.Fatalf("descendant process %d survived process-group kill", pid)
	}
}

func TestRunCleansProcessGroupAfterShellExit(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not available")
	}
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	executor := testExecutor(t, Config{KillGrace: 10 * time.Millisecond})
	result, err := executor.Run(context.Background(), Request{
		Command: fmt.Sprintf("sleep 30 >/dev/null 2>&1 & child=$!; printf '%%s' \"$child\" > %s", shellQuote(pidFile)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || result.TimedOut {
		t.Fatalf("background cleanup result = %#v", result)
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for processExists(pid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if processExists(pid) {
		t.Fatalf("descendant process %d survived shell exit cleanup", pid)
	}
}

func TestRunConcurrentExitCancellationRaces(t *testing.T) {
	executor := testExecutor(t, Config{KillGrace: 10 * time.Millisecond})
	for i := 0; i < 30; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		go cancel()
		result, err := executor.Run(ctx, Request{Command: ":", Timeout: time.Second})
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("iteration %d error = %v", i, err)
		}
		if result != nil && result.TimedOut {
			t.Fatalf("iteration %d falsely timed out: %#v", i, result)
		}
	}
}

func testExecutor(t *testing.T, cfg Config) *Executor {
	t.Helper()
	return testExecutorWithRoot(t, testRoot(t), cfg)
}

func testExecutorWithRoot(t *testing.T, root *workspace.Root, cfg Config) *Executor {
	t.Helper()
	cfg.Root = root
	if cfg.Shell == "" {
		cfg.Shell = testShell(t)
	}
	executor, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return executor
}

func testRoot(t *testing.T) *workspace.Root {
	t.Helper()
	root, err := workspace.NewRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func testShell(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not available")
	}
	path, err = filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func processExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
