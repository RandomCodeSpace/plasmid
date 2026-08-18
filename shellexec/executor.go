// Package shellexec runs bounded shell commands with workspace-validated
// initial working directories and process-tree cleanup.
package shellexec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/plasmid-dev/plasmid/outputlimit"
	"github.com/plasmid-dev/plasmid/workspace"
)

const (
	defaultTimeout   = 120 * time.Second
	maximumTimeout   = 600 * time.Second
	defaultKillGrace = 2 * time.Second
	hardWaitBackstop = 5 * time.Second
)

var (
	ErrNoShell      = errors.New("shellexec: no shell found")
	errNilRoot      = errors.New("shellexec: nil workspace root")
	errWaitBackstop = errors.New("shellexec: process did not stop before the wait backstop")
)

// Config defines process, environment, timeout, and output-capture policy.
type Config struct {
	Root           *workspace.Root
	Shell          string
	DefaultTimeout time.Duration
	MaxTimeout     time.Duration
	KillGrace      time.Duration
	Env            []string
	ExtraEnv       map[string]string
	OutputLimit    outputlimit.Policy
}

// Request describes one fresh, non-interactive shell invocation.
type Request struct {
	Command string
	Dir     string
	Stdin   string
	Timeout time.Duration
}

// Result describes command output and process termination. A non-zero command
// exit is represented here and is not a Run error.
type Result struct {
	Stdout       string
	Stderr       string
	ExitCode     int
	Signal       string
	TimedOut     bool
	Killed       bool
	Duration     time.Duration
	Dir          string
	StdoutReport outputlimit.Report
	StderrReport outputlimit.Report
}

// Executor holds immutable policy shared by independent command invocations.
type Executor struct {
	root           *workspace.Root
	shell          string
	defaultTimeout time.Duration
	maxTimeout     time.Duration
	killGrace      time.Duration
	env            []string
	extraEnv       map[string]string
	outputLimit    outputlimit.Policy
}

// New validates and resolves an executor configuration.
func New(cfg Config) (*Executor, error) {
	if cfg.Root == nil {
		return nil, errNilRoot
	}
	shell, err := resolveShell(cfg.Shell)
	if err != nil {
		return nil, err
	}
	if cfg.DefaultTimeout <= 0 {
		cfg.DefaultTimeout = defaultTimeout
	}
	if cfg.MaxTimeout <= 0 {
		cfg.MaxTimeout = maximumTimeout
	}
	if cfg.DefaultTimeout > cfg.MaxTimeout {
		cfg.DefaultTimeout = cfg.MaxTimeout
	}
	if cfg.KillGrace <= 0 {
		cfg.KillGrace = defaultKillGrace
	}
	if _, err := outputlimit.NewWriter(cfg.OutputLimit); err != nil {
		return nil, fmt.Errorf("shellexec output limit: %w", err)
	}

	extraEnv := make(map[string]string, len(cfg.ExtraEnv))
	for key, value := range cfg.ExtraEnv {
		extraEnv[key] = value
	}
	var configuredEnv []string
	if cfg.Env != nil {
		configuredEnv = make([]string, len(cfg.Env))
		copy(configuredEnv, cfg.Env)
	}
	return &Executor{
		root:           cfg.Root,
		shell:          shell,
		defaultTimeout: cfg.DefaultTimeout,
		maxTimeout:     cfg.MaxTimeout,
		killGrace:      cfg.KillGrace,
		env:            configuredEnv,
		extraEnv:       extraEnv,
		outputLimit:    cfg.OutputLimit,
	}, nil
}

// Shell returns the resolved shell executable.
func (e *Executor) Shell() string { return e.shell }

// Run executes a command with stdout and stderr captured independently.
func (e *Executor) Run(ctx context.Context, req Request) (*Result, error) {
	return e.run(ctx, req, false)
}

// RunMerged executes a command with stdout and stderr connected to one stream,
// preserving the order in which the process writes to the shared descriptor.
func (e *Executor) RunMerged(ctx context.Context, req Request) (Result, error) {
	result, err := e.run(ctx, req, true)
	if result == nil {
		return Result{}, err
	}
	return *result, err
}

func (e *Executor) run(ctx context.Context, req Request, merged bool) (*Result, error) {
	dir, err := e.resolveDir(req.Dir)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	stdout, stderr, err := e.outputWriters(merged)
	if err != nil {
		return nil, err
	}

	cmd := exec.Command(e.shell, "-c", req.Command)
	cmd.Dir = dir
	cmd.Env = buildEnv(e.env, e.extraEnv, dir)
	cmd.Stdin = strings.NewReader(req.Stdin)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// If the shell exits while a background descendant still owns its output
	// pipes, Wait returns ErrWaitDelay. Keep that interval short, then clean up
	// the remaining process group below.
	cmd.WaitDelay = e.killGrace
	configureProcess(cmd)

	started := time.Now()
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start shell command: %w", err)
	}

	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	timeout := time.NewTimer(e.requestTimeout(req.Timeout))
	defer stopTimer(timeout)

	waitErr, timedOut, killed, stopErr := e.waitForCommand(ctx, cmd, waited, timeout)
	result := e.captureResult(dir, started, stdout, stderr, merged, timedOut, killed)
	classifyExit(result, cmd.ProcessState)
	return finishCommand(result, cmd, waitErr, stopErr)
}

func (e *Executor) outputWriters(merged bool) (*outputlimit.Writer, *outputlimit.Writer, error) {
	stdout, err := outputlimit.NewWriter(e.outputLimit)
	if err != nil {
		return nil, nil, fmt.Errorf("create stdout capture: %w", err)
	}
	if merged {
		return stdout, stdout, nil
	}
	stderr, err := outputlimit.NewWriter(e.outputLimit)
	if err != nil {
		return nil, nil, fmt.Errorf("create stderr capture: %w", err)
	}
	return stdout, stderr, nil
}

func (e *Executor) waitForCommand(
	ctx context.Context,
	cmd *exec.Cmd,
	waited <-chan error,
	timeout *time.Timer,
) (waitErr error, timedOut bool, killed bool, stopErr error) {
	select {
	case waitErr = <-waited:
		return waitErr, false, false, nil
	case <-ctx.Done():
		if completed, ok := pollWait(waited); ok {
			return completed, false, false, nil
		}
		killed, waitErr, stopErr = e.stopProcess(cmd, waited)
		return waitErr, false, killed, stopErr
	case <-timeout.C:
		if completed, ok := pollWait(waited); ok {
			return completed, false, false, nil
		}
		killed, waitErr, stopErr = e.stopProcess(cmd, waited)
		return waitErr, true, killed, stopErr
	}
}

func (e *Executor) captureResult(
	dir string,
	started time.Time,
	stdout *outputlimit.Writer,
	stderr *outputlimit.Writer,
	merged bool,
	timedOut bool,
	killed bool,
) *Result {
	result := &Result{
		ExitCode: -1,
		TimedOut: timedOut,
		Killed:   killed,
		Duration: time.Since(started),
		Dir:      e.root.Rel(dir),
	}
	result.Stdout, result.StdoutReport = stdout.String()
	if !merged {
		result.Stderr, result.StderrReport = stderr.String()
	}
	return result
}

func finishCommand(result *Result, cmd *exec.Cmd, waitErr error, stopErr error) (*Result, error) {
	if stopErr != nil {
		return result, stopErr
	}
	if waitErr != nil && !isCommandExit(waitErr) && !errors.Is(waitErr, exec.ErrWaitDelay) {
		return result, fmt.Errorf("wait for shell command: %w", waitErr)
	}
	// A shell can exit successfully while descendants continue with their
	// standard descriptors redirected. WaitDelay cannot detect those, so every
	// completed invocation closes its process group explicitly.
	if cleanupErr := signalProcessGroup(cmd.Process, true); cleanupErr != nil && !errors.Is(cleanupErr, os.ErrProcessDone) {
		return result, fmt.Errorf("clean up shell process group: %w", cleanupErr)
	}
	return result, nil
}

func (e *Executor) resolveDir(requested string) (string, error) {
	if requested == "" {
		requested = e.root.Dir()
	}
	dir, err := e.root.ResolveExisting(requested)
	if err != nil {
		return "", fmt.Errorf("resolve shell directory: %w", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return "", fmt.Errorf("stat shell directory: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("shell directory %q: %w", requested, workspace.ErrNotDirectory)
	}
	return dir, nil
}

func (e *Executor) requestTimeout(requested time.Duration) time.Duration {
	if requested <= 0 {
		return e.defaultTimeout
	}
	if requested > e.maxTimeout {
		return e.maxTimeout
	}
	return requested
}

func (e *Executor) stopProcess(cmd *exec.Cmd, waited <-chan error) (bool, error, error) {
	killed := signalProcessGroup(cmd.Process, false) == nil
	grace := time.NewTimer(e.killGrace)
	defer stopTimer(grace)
	select {
	case waitErr := <-waited:
		return killed, waitErr, nil
	case <-grace.C:
	}

	if signalProcessGroup(cmd.Process, true) == nil {
		killed = true
	}
	backstop := time.NewTimer(hardWaitBackstop)
	defer stopTimer(backstop)
	select {
	case waitErr := <-waited:
		return killed, waitErr, nil
	case <-backstop.C:
		return killed, nil, errWaitBackstop
	}
}

func resolveShell(configured string) (string, error) {
	if configured != "" {
		path, err := exec.LookPath(configured)
		if err != nil {
			return "", fmt.Errorf("resolve shell %q: %w", configured, ErrNoShell)
		}
		return absoluteExecutable(path)
	}
	for _, candidate := range []string{"bash", "sh"} {
		if path, err := exec.LookPath(candidate); err == nil {
			return absoluteExecutable(path)
		}
	}
	return "", ErrNoShell
}

func absoluteExecutable(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve shell path %q: %w", path, err)
	}
	return abs, nil
}

func pollWait(waited <-chan error) (error, bool) {
	select {
	case err := <-waited:
		return err, true
	default:
		return nil, false
	}
}

func stopTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

func classifyExit(result *Result, state *os.ProcessState) {
	if state == nil {
		return
	}
	if code := state.ExitCode(); code >= 0 {
		result.ExitCode = code
		return
	}
	result.Signal = processSignal(state)
}

func isCommandExit(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr)
}
