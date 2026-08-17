package codingtools

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"reflect"
	"testing"

	"github.com/plasmid-dev/plasmid/loop"
	"github.com/plasmid-dev/plasmid/outputlimit"
	"github.com/plasmid-dev/plasmid/shellexec"
	"github.com/plasmid-dev/plasmid/workspace"
)

func TestNewRequiresSharedWorkspaceDependencies(t *testing.T) {
	root, err := workspace.NewRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base := Config{Root: root, Queue: workspace.NewMutationQueue(), Ledger: workspace.NewLedger(), Touch: workspace.NewTouchBus()}
	for _, test := range []struct {
		name string
		edit func(*Config)
	}{
		{"root", func(cfg *Config) { cfg.Root = nil }},
		{"queue", func(cfg *Config) { cfg.Queue = nil }},
		{"ledger", func(cfg *Config) { cfg.Ledger = nil }},
		{"touch", func(cfg *Config) { cfg.Touch = nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := base
			test.edit(&cfg)
			if _, err := New(cfg); err == nil {
				t.Fatal("New() error = nil")
			}
		})
	}
}

func TestNewDefaultsAndOrderWithoutShell(t *testing.T) {
	set := newRegistrySet(t, nil, nil)
	want := []string{"read", "write", "edit", "grep", "find", "ls"}
	if got := set.Names(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
	read, ok := set.Tool("read")
	if !ok {
		t.Fatal("read tool missing")
	}
	readTool := read.(*ReadTool)
	if readTool.maxReadBytes != defaultRegistryFileBytes || readTool.output != outputlimit.Defaults() {
		t.Fatalf("read defaults = bytes %d output %#v", readTool.maxReadBytes, readTool.output)
	}
	if used, limit := readTool.budget.Report("session"); used != 0 || limit != outputlimit.DefaultPerSession {
		t.Fatalf("budget defaults = (%d, %d)", used, limit)
	}
	grep, ok := set.Tool("grep")
	if !ok || grep.(*GrepTool).maxGrepFileBytes != defaultRegistryGrepBytes {
		t.Fatalf("grep defaults = %#v", grep)
	}
	write, ok := set.Tool("write")
	if !ok || write.(*WriteTool).maxWriteBytes != defaultRegistryFileBytes {
		t.Fatalf("write defaults = %#v", write)
	}
}

func TestNewOrderWithShellAndWarning(t *testing.T) {
	root, err := workspace.NewRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	shell, err := shellexec.New(shellexec.Config{Root: root, Shell: "sh", OutputLimit: outputlimit.Defaults()})
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	set := newRegistrySet(t, shell, logger)
	want := []string{"read", "write", "edit", "bash", "grep", "find", "ls"}
	if got := set.Names(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
	if logs.Len() != 0 {
		t.Fatalf("shell configuration logged %q", logs.String())
	}

	logs.Reset()
	_ = newRegistrySet(t, nil, logger)
	if got := bytes.Count(logs.Bytes(), []byte("\"level\":\"WARN\"")); got != 1 {
		t.Fatalf("warnings = %d, logs = %q", got, logs.String())
	}
}

func TestSetLookupAndDefensiveSlices(t *testing.T) {
	set := newRegistrySet(t, nil, nil)
	if _, ok := set.Tool("READ"); ok {
		t.Fatal("case-insensitive lookup succeeded")
	}
	if _, ok := set.Tool("missing"); ok {
		t.Fatal("unknown lookup succeeded")
	}
	tools := set.Tools()
	names := set.Names()
	tools[0] = nil
	names[0] = "changed"
	if got, ok := set.Tool("read"); !ok || got == nil {
		t.Fatal("caller mutation changed registry")
	}
	if got := set.Names()[0]; got != "read" {
		t.Fatalf("Names()[0] = %q", got)
	}
}

func TestNewSetRejectsDuplicates(t *testing.T) {
	tool := registryStubTool{name: "read"}
	if _, err := newSet([]loop.Tool{tool, tool}); err == nil {
		t.Fatal("newSet() error = nil")
	}
}

func newRegistrySet(t *testing.T, shell *shellexec.Executor, logger *slog.Logger) *Set {
	t.Helper()
	root, err := workspace.NewRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	set, err := New(Config{Root: root, Queue: workspace.NewMutationQueue(), Ledger: workspace.NewLedger(), Touch: workspace.NewTouchBus(), Shell: shell, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	return set
}

type registryStubTool struct{ name string }

func (t registryStubTool) Name() string               { return t.name }
func (registryStubTool) Description() string          { return "" }
func (registryStubTool) InputSchema() json.RawMessage { return nil }
func (registryStubTool) Call(context.Context, loop.ToolCall) (loop.ToolResult, error) {
	return loop.ToolResult{}, nil
}
