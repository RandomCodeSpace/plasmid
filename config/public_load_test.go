package config_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RandomCodeSpace/plasmid/config"
)

type cancelAfterChecks struct {
	limit int

	mu     sync.Mutex
	checks int
	done   chan struct{}
	err    error
}

func newCancelAfterChecks(limit int) *cancelAfterChecks {
	return &cancelAfterChecks{limit: limit, done: make(chan struct{})}
}

func (*cancelAfterChecks) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *cancelAfterChecks) Done() <-chan struct{}     { return c.done }
func (*cancelAfterChecks) Value(any) any               { return nil }

func (c *cancelAfterChecks) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	c.checks++
	if c.checks < c.limit {
		return nil
	}
	c.err = context.Canceled
	close(c.done)
	return c.err
}

func TestLoadStopsAtEveryCancellationBoundary(t *testing.T) {
	root := t.TempDir()
	workingDir := filepath.Join(root, "work")
	configDir := filepath.Join(root, "config")
	for _, directory := range []string{
		workingDir,
		configDir,
		filepath.Join(configDir, "skills"),
		filepath.Join(configDir, "trusted"),
		filepath.Join(configDir, "imports"),
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(configDir, "config.json")
	content := `{
		"version":1,
		"lsp":{"future":true,"servers":[{"id":"gopls","command":"gopls","args":["serve"],"extensions":[".go"],"rootMarkers":["go.mod"],"future":true}]},
		"mcp":{"allowForeign":["fixture"],"servers":[{"id":"stdio","command":"server","args":["serve"],"env":{"MODE":"test"},"future":true}]},
		"skills":{"roots":["skills"]},
		"foreign":{"trustedRoots":["trusted"]},
		"context":{"importRoots":["imports"]},
		"compaction":{"preserveToolNames":["read"]}
	}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	const maximumChecks = 256
	for limit := 1; limit <= maximumChecks; limit++ {
		t.Run(strconv.Itoa(limit), func(t *testing.T) {
			ctx := newCancelAfterChecks(limit)
			_, err := config.Load(ctx, config.Options{ConfigPath: path, WorkingDir: workingDir})
			if err == nil {
				return
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Load error = %v, want context cancellation", err)
			}
		})
	}
}

func TestLoadAcceptsCompleteValidatedConfiguration(t *testing.T) {
	configDir := t.TempDir()
	workingDir := t.TempDir()
	for _, name := range []string{"skills", "trusted", "imports"} {
		if err := os.Mkdir(filepath.Join(configDir, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(configDir, "config.json")
	content := `{
		"version":1,"appName":"complete","future":"ignored",
		"lsp":{"mode":"off","settleTimeoutMs":1,"initializeTimeoutMs":2,"requestTimeoutMs":3,"failureThreshold":4,"maxDiagnosticsPerFile":5,
			"servers":[{"id":"gopls","command":"gopls","args":["serve"],"extensions":[".go"],"rootMarkers":["go.mod"],"disabled":true,"future":true}]},
		"mcp":{"inheritForeign":true,"allowForeign":["alpha","alpha","bad*"],"servers":[
			{"id":"stdio","transport":"stdio","command":"server","args":["serve"],"env":{"MODE":"test"},"future":true},
			{"id":"http","transport":"http","url":"https://example.invalid/mcp","headers":{"X-Test":"value"}}]},
		"skills":{"roots":["skills","skills"],"future":true},
		"foreign":{"enabled":false,"claude":false,"codex":false,"copilot":false,"trustedRoots":["trusted"],"future":true},
		"syntax":{"promptCommands":"off","commandTimeoutMs":10,"documentTimeoutMs":20,"commandOutputBytes":30,"documentOutputBytes":40,"future":true},
		"context":{"maxFileBytes":100,"maxBytes":200,"maxImportDepth":0,"touchesPerToolCall":3,"importRoots":["imports"],"future":true},
		"tools":{"callOutputBytes":100,"sessionOutputBytes":200,"bashTimeoutMs":300,"bashMaxTimeoutMs":400,"confirmation":true,"future":true},
		"compaction":{"contextTokens":1000,"triggerFraction":0.8,"targetFraction":0.5,"keepRecentContents":2,"minimumElisionTokens":3,"preserveToolNames":["read","read"],"calibration":false,"future":true}
	}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := config.Load(t.Context(), config.Options{ConfigPath: path, WorkingDir: workingDir, UserID: "user"})
	if err != nil {
		t.Fatal(err)
	}
	got := result.Config
	if got.AppName != "complete" || got.UserID != "user" || got.LSP.Mode != config.LSPOff || len(got.LSP.Servers) != 1 || !got.LSP.Servers[0].Disabled {
		t.Fatalf("identity or LSP config = %#v", got)
	}
	if !got.MCP.InheritForeign || len(got.MCP.AllowForeign) != 1 || len(got.MCP.Servers) != 2 {
		t.Fatalf("MCP config = %#v", got.MCP)
	}
	if len(got.Skills.Roots) != 1 || len(got.Foreign.TrustedRoots) != 1 || len(got.Context.ImportRoots) != 1 {
		t.Fatalf("normalized roots = skills %#v, foreign %#v, context %#v", got.Skills, got.Foreign, got.Context)
	}
	if got.Syntax.CommandTimeout != 10*time.Millisecond || got.Tools.BashMaxTimeout != 400*time.Millisecond || got.Compaction.TargetFraction != 0.5 {
		t.Fatalf("limits = syntax %#v, tools %#v, compaction %#v", got.Syntax, got.Tools, got.Compaction)
	}
	if len(result.Warnings) != 10 {
		t.Fatalf("warnings = %#v, want ten unknown/allowlist warnings", result.Warnings)
	}
}

func TestLoadRepairsMalformedPublicConfigurationFields(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{name: "non-object blocks", content: `{"version":1,"lsp":[],"mcp":[],"skills":[],"foreign":[],"syntax":[],"context":[],"tools":[],"compaction":[]}`},
		{name: "null blocks", content: `{"version":1,"lsp":null,"mcp":null,"skills":null,"foreign":null,"syntax":null,"context":null,"tools":null,"compaction":null}`},
		{name: "bad scalar fields", content: `{"version":1,"appName":42,"lsp":{"mode":42,"settleTimeoutMs":0,"initializeTimeoutMs":"bad","requestTimeoutMs":null,"failureThreshold":-1,"maxDiagnosticsPerFile":0},"mcp":{"inheritForeign":"bad"},"foreign":{"enabled":"bad","claude":null,"codex":1,"copilot":[]},"syntax":{"promptCommands":"bad","commandTimeoutMs":0,"documentTimeoutMs":-1,"commandOutputBytes":null,"documentOutputBytes":"bad"},"context":{"maxFileBytes":0,"maxBytes":-1,"maxImportDepth":-1,"touchesPerToolCall":null},"tools":{"callOutputBytes":0,"sessionOutputBytes":-1,"bashTimeoutMs":0,"bashMaxTimeoutMs":-1,"confirmation":"bad"},"compaction":{"contextTokens":-1,"triggerFraction":0,"targetFraction":2,"keepRecentContents":null,"minimumElisionTokens":"bad","calibration":"bad"}}`},
		{name: "bad list shapes", content: `{"version":1,"lsp":{"servers":42},"mcp":{"allowForeign":42,"servers":42},"skills":{"roots":42},"foreign":{"trustedRoots":42},"context":{"importRoots":42},"compaction":{"preserveToolNames":42}}`},
		{name: "bad lsp entries", content: `{"version":1,"lsp":{"servers":[null,42,{}, {"id":"x","command":"x","args":null,"extensions":[".x"]},{"id":"y","command":"y","args":[""],"extensions":[".y"]},{"id":"z","command":"z","extensions":["bad/path"],"rootMarkers":null},{"id":"b","command":"b","extensions":[".b"],"disabled":null}]}}`},
		{name: "bad mcp entries", content: `{"version":1,"mcp":{"servers":[null,42,{}, {"id":"bad-kind","transport":"socket","command":"x"},{"id":"args","command":"x","args":null},{"id":"env","command":"x","env":null},{"id":"http","transport":"http","url":"ftp://example.invalid"},{"id":"headers","transport":"http","url":"https://example.invalid","headers":null}]}}`},
		{name: "invalid and duplicate names", content: `{"version":1,"mcp":{"allowForeign":["ok","bad?","bad[","ok"],"servers":[{"id":"same","command":"one"},{"id":"same","command":"two"}]},"compaction":{"preserveToolNames":["read","read"]}}`},
		{name: "reversed bounds", content: `{"version":1,"tools":{"bashTimeoutMs":500,"bashMaxTimeoutMs":100},"compaction":{"triggerFraction":0.4,"targetFraction":0.8}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := loadText(t, test.content)
			if len(result.Warnings) == 0 {
				t.Fatal("malformed config emitted no repair warning")
			}
		})
	}
}

func TestLoadRejectsInvalidPublicInputs(t *testing.T) {
	var nilContext context.Context
	if _, err := config.Load(nilContext, config.Options{}); err == nil || !strings.Contains(err.Error(), "nil context") {
		t.Fatalf("nil context error = %v", err)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	if _, err := config.Load(t.Context(), config.Options{WorkingDir: missing}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing working directory error = %v", err)
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(t.Context(), config.Options{WorkingDir: file}); !errors.Is(err, config.ErrInvalidConfig) {
		t.Fatalf("file working directory error = %v", err)
	}
	if _, err := config.Load(t.Context(), config.Options{WorkingDir: t.TempDir(), ConfigPath: t.TempDir()}); !errors.Is(err, config.ErrInvalidConfig) {
		t.Fatalf("directory config error = %v", err)
	}

	workingDir := t.TempDir()
	oversized := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(oversized, []byte(strings.Repeat(" ", (1<<20)+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(t.Context(), config.Options{WorkingDir: workingDir, ConfigPath: oversized}); !errors.Is(err, config.ErrInvalidConfig) {
		t.Fatalf("oversized config error = %v", err)
	}

	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(workingDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldDir) })
	if result, err := config.Load(context.Background(), config.Options{}); err != nil || result.Config.WorkingDir != workingDir {
		t.Fatalf("implicit working directory result = %#v, err = %v", result, err)
	}
}

func TestLoadNormalizesPublicPathAndCommandForms(t *testing.T) {
	root := t.TempDir()
	workingDir := filepath.Join(root, "work")
	homeDir := filepath.Join(root, "home")
	configDir := filepath.Join(homeDir, "configs")
	for _, directory := range []string{workingDir, homeDir, configDir, filepath.Join(homeDir, "skills"), filepath.Join(homeDir, "imports"), filepath.Join(homeDir, "trusted")} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	server := filepath.Join(homeDir, "server")
	if err := os.WriteFile(server, []byte("server"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(configDir, "config.json")
	content := `{
		"version":1,
		"lsp":{"servers":[
			{"id":"home","command":"~/server","extensions":[".home"]},
			{"id":"directory","command":"~/skills","extensions":[".bad"]}
		]},
		"mcp":{"servers":[{"id":"home","command":"~/server","env":{"MODE":"test"}}]},
		"skills":{"roots":["~/skills","~/missing"]},
		"foreign":{"trustedRoots":["~/trusted"]},
		"context":{"importRoots":["~/imports"]}
	}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", homeDir)
	result, err := config.Load(t.Context(), config.Options{
		ConfigPath: "~/configs/config.json", WorkingDir: workingDir, SessionDir: "~/sessions",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.SourcePath != path || result.Config.SessionDir != filepath.Join(homeDir, "sessions") {
		t.Fatalf("source/session paths = %q / %q", result.SourcePath, result.Config.SessionDir)
	}
	if len(result.Config.LSP.Servers) != 2 || result.Config.LSP.Servers[1].Command != server {
		t.Fatalf("LSP servers = %#v", result.Config.LSP.Servers)
	}
	if len(result.Config.MCP.Servers) != 1 || result.Config.MCP.Servers[0].Command != server {
		t.Fatalf("MCP servers = %#v", result.Config.MCP.Servers)
	}
	if len(result.Config.Skills.Roots) != 1 || result.Config.Skills.Roots[0] != filepath.Join(homeDir, "skills") {
		t.Fatalf("skill roots = %#v", result.Config.Skills.Roots)
	}
	if len(result.Warnings) < 2 {
		t.Fatalf("path repair warnings = %#v", result.Warnings)
	}
}

func TestLoadDropsInvalidStringMapsAndNonRegularCommands(t *testing.T) {
	result := loadText(t, `{"version":1,"lsp":{"servers":[{"id":"directory","command":".","extensions":[".bad"]}]},"mcp":{"servers":[{"id":"empty-key","command":"server","env":{"":"value"}},{"id":"bad-value","command":"server","env":{"MODE":3}},{"id":"bad-header-key","transport":"http","url":"https://example.invalid","headers":{"": "value"}},{"id":"bad-header-value","transport":"http","url":"https://example.invalid","headers":{"X-Test":3}}]}}`)
	if len(result.Config.MCP.Servers) != 0 {
		t.Fatalf("invalid MCP maps survived: %#v", result.Config.MCP.Servers)
	}
	if len(result.Warnings) != 4 {
		t.Fatalf("warnings = %#v, want four dropped servers", result.Warnings)
	}
}

func TestLoadAppliesAndValidatesFunctionalOptions(t *testing.T) {
	workingDir := t.TempDir()
	for _, name := range []string{"skills", "trusted", "imports"} {
		if err := os.Mkdir(filepath.Join(workingDir, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	appName := "host"
	mode := config.LSPOff
	confirmation := true
	foreignOptions := config.Foreign{
		Enabled: true, Claude: true,
		TrustedRoots: []string{filepath.Join(workingDir, "trusted"), filepath.Join(workingDir, "trusted")},
	}
	result, err := config.Load(t.Context(), config.Options{
		WorkingDir: workingDir, SessionDir: filepath.Join(workingDir, "sessions"), UserID: "host-user",
		AppName: &appName, LSPMode: &mode, Foreign: &foreignOptions, ToolConfirmation: &confirmation,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Config.AppName != appName || result.Config.UserID != "host-user" || result.Config.LSP.Mode != mode || !result.Config.Tools.Confirmation {
		t.Fatalf("functional options = %#v", result.Config)
	}
	if len(result.Config.Foreign.TrustedRoots) != 1 {
		t.Fatalf("trusted roots = %#v", result.Config.Foreign.TrustedRoots)
	}
	foreignOptions.TrustedRoots[0] = "mutated"
	if result.Config.Foreign.TrustedRoots[0] == "mutated" {
		t.Fatal("foreign option aliases returned configuration")
	}

	empty := " "
	if _, err := config.Load(t.Context(), config.Options{WorkingDir: workingDir, AppName: &empty}); !errors.Is(err, config.ErrInvalidConfig) {
		t.Fatalf("empty app name error = %v", err)
	}
	invalidMode := config.LSPMode("invalid")
	if _, err := config.Load(t.Context(), config.Options{WorkingDir: workingDir, LSPMode: &invalidMode}); !errors.Is(err, config.ErrInvalidConfig) {
		t.Fatalf("invalid LSP mode error = %v", err)
	}
	missingRoot := config.Foreign{TrustedRoots: []string{filepath.Join(workingDir, "missing")}}
	if _, err := config.Load(t.Context(), config.Options{WorkingDir: workingDir, Foreign: &missingRoot}); !errors.Is(err, config.ErrConfigNotFound) {
		t.Fatalf("missing trusted root error = %v", err)
	}
}

func loadText(t *testing.T, content string) config.Result {
	t.Helper()
	directory := t.TempDir()
	path := filepath.Join(directory, "config.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := config.Load(t.Context(), config.Options{ConfigPath: path, WorkingDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
