package codingtools

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"reflect"
	"testing"

	adktool "google.golang.org/adk/v2/tool"

	"github.com/RandomCodeSpace/plasmid/outputlimit"
	"github.com/RandomCodeSpace/plasmid/shellexec"
	"github.com/RandomCodeSpace/plasmid/warning"
	"github.com/RandomCodeSpace/plasmid/workspace"
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
	root, err := workspace.NewRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig(Config{Root: root, Queue: workspace.NewMutationQueue(), Ledger: workspace.NewLedger(), Touch: workspace.NewTouchBus()})
	if cfg.MaxReadBytes != defaultRegistryFileBytes || cfg.MaxWriteBytes != defaultRegistryFileBytes || cfg.MaxGrepFileBytes != defaultRegistryGrepBytes || cfg.MaxTouchEvents != MaxTouchEvents || cfg.Output != outputlimit.Defaults() {
		t.Fatalf("registry defaults = %#v", cfg)
	}
	if used, limit := cfg.Budget.Report("session"); used != 0 || limit != outputlimit.DefaultPerSession {
		t.Fatalf("budget defaults = (%d, %d)", used, limit)
	}
	for _, name := range want {
		tool, ok := set.Tool(name)
		if !ok || tool.Name() != name || tool.IsLongRunning() {
			t.Fatalf("native tool %q = %#v", name, tool)
		}
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
	var warnings warning.SliceSink
	set := newRegistrySet(t, shell, &warnings)
	want := []string{"read", "write", "edit", "bash", "grep", "find", "ls"}
	if got := set.Names(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
	if got := warnings.Warnings(); len(got) != 0 {
		t.Fatalf("shell configuration warnings = %#v", got)
	}

	_ = newRegistrySet(t, nil, &warnings)
	got := warnings.Warnings()
	if len(got) != 1 || got[0] != (warning.Warning{
		Code:    warning.WarnCodingtoolsBashOmitted,
		Source:  "codingtools",
		Message: "bash tool omitted because no shell executor is configured",
	}) {
		t.Fatalf("warnings = %#v", got)
	}
}

func TestNewDefaultWarningSinkLogsStructuredOmission(t *testing.T) {
	root, err := workspace.NewRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	_, err = New(Config{
		Root:   root,
		Queue:  workspace.NewMutationQueue(),
		Ledger: workspace.NewLedger(),
		Touch:  workspace.NewTouchBus(),
		Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["code"] != warning.WarnCodingtoolsBashOmitted || got["source"] != "codingtools" || got["path"] != "" || got["line"] != float64(0) || got["message"] != "bash tool omitted because no shell executor is configured" {
		t.Fatalf("warning log = %#v", got)
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
	if _, err := newSet([]adktool.Tool{tool, tool}); err == nil {
		t.Fatal("newSet() error = nil")
	}
}

func newRegistrySet(t *testing.T, shell *shellexec.Executor, warnings warning.Warner) *Set {
	t.Helper()
	root, err := workspace.NewRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	set, err := New(Config{Root: root, Queue: workspace.NewMutationQueue(), Ledger: workspace.NewLedger(), Touch: workspace.NewTouchBus(), Shell: shell, WarningSink: warnings})
	if err != nil {
		t.Fatal(err)
	}
	return set
}

type registryStubTool struct{ name string }

func (t registryStubTool) Name() string      { return t.name }
func (registryStubTool) Description() string { return "" }
func (registryStubTool) IsLongRunning() bool { return false }
