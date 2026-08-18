package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/plasmid-dev/plasmid/warning"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	workingDir := t.TempDir()
	result, err := Load(context.Background(), Options{WorkingDir: workingDir})
	if err != nil {
		t.Fatal(err)
	}
	if result.SourcePath != "" || len(result.Warnings) != 0 {
		t.Fatalf("unexpected source or warnings: %#v", result)
	}
	configuration := result.Config
	if configuration.Version != CurrentVersion || configuration.AppName != "plasmid" || configuration.UserID != "default" {
		t.Fatalf("identity defaults = %#v", configuration)
	}
	if configuration.WorkingDir != workingDir || configuration.SessionDir != filepath.Join(workingDir, ".plasmid", "sessions") {
		t.Fatalf("path defaults = %#v", configuration)
	}
	if configuration.LSP.Mode != LSPAuto || len(configuration.LSP.Servers) != 1 || configuration.LSP.Servers[0].ID != "gopls" {
		t.Fatalf("LSP defaults = %#v", configuration.LSP)
	}
	if !configuration.Foreign.Enabled || configuration.MCP.InheritForeign || configuration.Syntax.PromptCommands != PromptCommandsTrusted {
		t.Fatalf("security defaults = foreign %#v, MCP %#v, syntax %#v", configuration.Foreign, configuration.MCP, configuration.Syntax)
	}
	if configuration.Tools.CallOutputBytes != 30_000 || configuration.Tools.SessionOutputBytes != 400_000 || configuration.Tools.BashTimeout != 120*time.Second {
		t.Fatalf("tool defaults = %#v", configuration.Tools)
	}
	if configuration.Compaction.ContextTokens != 0 || configuration.Compaction.TriggerFraction != 0.85 || configuration.Compaction.TargetFraction != 0.60 {
		t.Fatalf("compaction defaults = %#v", configuration.Compaction)
	}
}

func TestLoadCanonicalizesMissingSessionDirBelowSymlink(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	workingDir := filepath.Join(root, "work")
	stateDir := filepath.Join(root, "state")
	for _, path := range []string{workingDir, stateDir} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	aliases := []string{filepath.Join(root, "state-one"), filepath.Join(root, "state-two")}
	for _, alias := range aliases {
		if err := os.Symlink(stateDir, alias); err != nil {
			t.Fatal(err)
		}
	}

	canonicalStateDir, err := filepath.EvalSymlinks(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(canonicalStateDir, "future", "sessions")
	for _, alias := range aliases {
		result, loadErr := Load(context.Background(), Options{
			WorkingDir: workingDir,
			SessionDir: filepath.Join(alias, "future", "sessions"),
		})
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		if result.Config.SessionDir != want {
			t.Fatalf("session directory through %q = %q, want %q", alias, result.Config.SessionDir, want)
		}
	}
}

func TestLoadNormalizesDefaultSessionDirBelowSymlink(t *testing.T) {
	tests := []struct {
		name      string
		dangling  bool
		wantError error
	}{
		{name: "existing alias"},
		{name: "dangling alias", dangling: true, wantError: os.ErrNotExist},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("HOME", t.TempDir())
			root := t.TempDir()
			workingDir := filepath.Join(root, "work")
			stateDir := filepath.Join(root, "state")
			if err := os.Mkdir(workingDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if !test.dangling {
				if err := os.Mkdir(stateDir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(stateDir, filepath.Join(workingDir, ".plasmid")); err != nil {
				t.Fatal(err)
			}

			result, err := Load(context.Background(), Options{WorkingDir: workingDir})
			if test.wantError != nil {
				if !errors.Is(err, test.wantError) {
					t.Fatalf("error = %v, want %v", err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			canonicalStateDir, evalErr := filepath.EvalSymlinks(stateDir)
			if evalErr != nil {
				t.Fatal(evalErr)
			}
			if want := filepath.Join(canonicalStateDir, "sessions"); result.Config.SessionDir != want {
				t.Fatalf("default session directory = %q, want %q", result.Config.SessionDir, want)
			}
		})
	}
}

func TestLoadHardFailures(t *testing.T) {
	workingDir := t.TempDir()
	tests := []struct {
		name    string
		content string
		want    error
	}{
		{name: "invalid JSON", content: `{`, want: ErrInvalidConfig},
		{name: "future version", content: `{"version":2}`, want: ErrUnsupportedVersion},
		{name: "negative version", content: `{"version":-1}`, want: ErrInvalidConfig},
		{name: "null version", content: `{"version":null}`, want: ErrInvalidConfig},
		{name: "non-object", content: `[]`, want: ErrInvalidConfig},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(test.content), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(context.Background(), Options{ConfigPath: path, WorkingDir: workingDir})
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
	_, err := Load(context.Background(), Options{ConfigPath: filepath.Join(workingDir, "missing.json"), WorkingDir: workingDir})
	if !errors.Is(err, ErrConfigNotFound) || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing explicit config error = %v, want ErrConfigNotFound preserving os.ErrNotExist", err)
	}
}

func TestLoadClassifiesDanglingExplicitConfigAsNotFound(t *testing.T) {
	workingDir := t.TempDir()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.Symlink(filepath.Join(t.TempDir(), "missing.json"), path); err != nil {
		t.Fatal(err)
	}

	_, err := Load(context.Background(), Options{ConfigPath: path, WorkingDir: workingDir})
	if !errors.Is(err, ErrConfigNotFound) || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("error = %v, want ErrConfigNotFound preserving os.ErrNotExist", err)
	}
}

func TestDiscoveryFirstHitAndOptionPrecedence(t *testing.T) {
	root := t.TempDir()
	workingDir := filepath.Join(root, "work")
	xdg := filepath.Join(root, "xdg")
	home := filepath.Join(root, "home")
	for _, dir := range []string{workingDir, filepath.Join(xdg, "plasmid"), filepath.Join(home, ".config", "plasmid")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for path, appName := range map[string]string{
		filepath.Join(workingDir, ".plasmid.json"):               "work",
		filepath.Join(xdg, "plasmid", "config.json"):             "xdg",
		filepath.Join(home, ".config", "plasmid", "config.json"): "home",
	} {
		if err := os.WriteFile(path, []byte(`{"appName":"`+appName+`","version":1}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", home)
	override := "option"
	result, err := Load(context.Background(), Options{WorkingDir: workingDir, AppName: &override})
	if err != nil {
		t.Fatal(err)
	}
	if result.Config.AppName != "option" || result.SourcePath != filepath.Join(workingDir, ".plasmid.json") {
		t.Fatalf("result = %#v", result)
	}
	for _, step := range []struct {
		remove   string
		wantApp  string
		wantPath string
	}{
		{remove: filepath.Join(workingDir, ".plasmid.json"), wantApp: "xdg", wantPath: filepath.Join(xdg, "plasmid", "config.json")},
		{remove: filepath.Join(xdg, "plasmid", "config.json"), wantApp: "home", wantPath: filepath.Join(home, ".config", "plasmid", "config.json")},
	} {
		if err := os.Remove(step.remove); err != nil {
			t.Fatal(err)
		}
		result, err := Load(context.Background(), Options{WorkingDir: workingDir})
		if err != nil {
			t.Fatal(err)
		}
		if result.Config.AppName != step.wantApp || result.SourcePath != step.wantPath {
			t.Fatalf("fallback result = %#v", result)
		}
	}
}

func TestLoadReturnsDefensiveCollections(t *testing.T) {
	result, err := Load(context.Background(), Options{WorkingDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	copyResult := result
	copyResult.Config.LSP.Servers[0].Extensions[0] = ".changed"
	copyResult.Warnings = append(copyResult.Warnings, warning.Warning{Code: "changed"})
	second, err := Load(context.Background(), Options{WorkingDir: result.Config.WorkingDir})
	if err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(copyResult.Config.LSP, second.Config.LSP) || len(second.Warnings) != 0 {
		t.Fatalf("load result aliases mutable state")
	}
}

func TestFunctionalOptionValidation(t *testing.T) {
	workingDir := t.TempDir()
	badMode := LSPMode("sometimes")
	if _, err := Load(context.Background(), Options{WorkingDir: workingDir, LSPMode: &badMode}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("invalid LSP option error = %v", err)
	}
	empty := ""
	if _, err := Load(context.Background(), Options{WorkingDir: workingDir, AppName: &empty}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("empty app-name option error = %v", err)
	}
	foreign := Foreign{Enabled: true, Codex: true, TrustedRoots: []string{"missing"}}
	if _, err := Load(context.Background(), Options{WorkingDir: workingDir, Foreign: &foreign}); !errors.Is(err, ErrConfigNotFound) {
		t.Fatalf("missing trusted-root option error = %v", err)
	}
}

func TestLoadHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := Load(ctx, Options{WorkingDir: t.TempDir()})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestLoadRepairsNullBooleansAndOverflowDurations(t *testing.T) {
	workingDir := t.TempDir()
	path := filepath.Join(t.TempDir(), "config.json")
	content := `{
		"version": 1,
		"foreign": {"enabled": null},
		"compaction": {"calibration": null},
		"tools": {"bashTimeoutMs": 9223372036855}
	}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := Load(context.Background(), Options{ConfigPath: path, WorkingDir: workingDir})
	if err != nil {
		t.Fatal(err)
	}
	want := defaults(workingDir)
	if result.Config.Foreign.Enabled != want.Foreign.Enabled || result.Config.Compaction.Calibration != want.Compaction.Calibration || result.Config.Tools.BashTimeout != want.Tools.BashTimeout {
		t.Fatalf("repairs = foreign %t, calibration %t, timeout %s", result.Config.Foreign.Enabled, result.Config.Compaction.Calibration, result.Config.Tools.BashTimeout)
	}
	if len(result.Warnings) != 3 {
		t.Fatalf("warnings = %#v, want three local repairs", result.Warnings)
	}
	for _, item := range result.Warnings {
		if item.Code != warning.WarnConfigInvalidValue {
			t.Fatalf("warning = %#v, want %s", item, warning.WarnConfigInvalidValue)
		}
	}
}

func TestLoadEmitsOneWarningForMalformedLSPEntry(t *testing.T) {
	workingDir := t.TempDir()
	path := filepath.Join(t.TempDir(), "config.json")
	content := `{"version":1,"lsp":{"servers":[{"id":"broken","command":"missing/server","extensions":[".go"]}]}}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := Load(context.Background(), Options{ConfigPath: path, WorkingDir: workingDir})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Warnings) != 1 || result.Warnings[0].Code != warning.WarnLSPConfigInvalidServer {
		t.Fatalf("warnings = %#v, want one %s", result.Warnings, warning.WarnLSPConfigInvalidServer)
	}
}

func TestLoadEmitsOneWarningPerMalformedMCPEntry(t *testing.T) {
	workingDir := t.TempDir()
	path := filepath.Join(t.TempDir(), "config.json")
	content := `{
		"version": 1,
		"mcp": {"servers": [
			{"id":"bad-transport","transport":1,"command":"server"},
			{"id":"stdio-headers","transport":"stdio","command":"server","headers":{"X-Test":"fixture"}},
			{"id":"stdio-url","transport":"stdio","command":"server","url":"https://example.invalid"},
			{"id":"http-args","transport":"http","url":"https://example.invalid","args":["wrong"]},
			{"id":"http-env","transport":"http","url":"https://example.invalid","env":{"MODE":"fixture"}},
			{"id":"http-command","transport":"http","url":"https://example.invalid","command":"wrong"},
			{"id":"bad-url","transport":"http","url":1},
			{"id":"missing-command","transport":"stdio"},
			{"id":"missing-path","transport":"stdio","command":"missing/server"}
		]}
	}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := Load(context.Background(), Options{ConfigPath: path, WorkingDir: workingDir})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Config.MCP.Servers) != 0 || len(result.Warnings) != 9 {
		t.Fatalf("MCP servers = %#v, warnings = %#v", result.Config.MCP.Servers, result.Warnings)
	}
	for _, item := range result.Warnings {
		if item.Code != warning.WarnConfigMCPIncomplete {
			t.Fatalf("warning = %#v, want %s", item, warning.WarnConfigMCPIncomplete)
		}
	}
}

func TestLoadWarnsAndKeepsFirstDuplicateLSPEntry(t *testing.T) {
	workingDir := t.TempDir()
	path := filepath.Join(t.TempDir(), "config.json")
	content := `{"version":1,"lsp":{"servers":[
		{"id":"duplicate","command":"first","extensions":[".one"]},
		{"id":"duplicate","command":"second","extensions":[".two"]}
	]}}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := Load(context.Background(), Options{ConfigPath: path, WorkingDir: workingDir})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Config.LSP.Servers) != 2 || result.Config.LSP.Servers[1].Command != "first" {
		t.Fatalf("LSP servers = %#v", result.Config.LSP.Servers)
	}
	if len(result.Warnings) != 1 || result.Warnings[0].Code != warning.WarnLSPConfigDuplicateServer {
		t.Fatalf("warnings = %#v, want one %s", result.Warnings, warning.WarnLSPConfigDuplicateServer)
	}
}

func TestLoadRepairsNullNonnegativeIntegersAndCollections(t *testing.T) {
	workingDir := t.TempDir()
	path := filepath.Join(t.TempDir(), "config.json")
	content := `{
		"version": 1,
		"compaction": {"contextTokens": null, "preserveToolNames": null},
		"context": {"importRoots": null, "maxImportDepth": null},
		"lsp": {"servers": null},
		"mcp": {"allowForeign": null, "servers": null},
		"skills": {"roots": null}
	}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := Load(context.Background(), Options{ConfigPath: path, WorkingDir: workingDir})
	if err != nil {
		t.Fatal(err)
	}
	want := defaults(workingDir)
	if result.Config.Context.MaxImportDepth != want.Context.MaxImportDepth || !reflect.DeepEqual(result.Config.LSP.Servers, want.LSP.Servers) {
		t.Fatalf("repaired config = %#v", result.Config)
	}
	if len(result.Warnings) != 8 {
		t.Fatalf("warnings = %#v, want eight null repairs", result.Warnings)
	}
	for _, item := range result.Warnings {
		if item.Code != warning.WarnConfigInvalidValue {
			t.Fatalf("warning = %#v, want %s", item, warning.WarnConfigInvalidValue)
		}
	}
}

func TestLoadHonorsCancellationDuringPathRepair(t *testing.T) {
	workingDir := t.TempDir()
	path := filepath.Join(t.TempDir(), "config.json")
	roots := make([]string, 500)
	for index := range roots {
		roots[index] = fmt.Sprintf("missing-%d", index)
	}
	content, err := json.Marshal(map[string]any{
		"version": 1,
		"skills":  map[string]any{"roots": roots},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}

	ctx := &cancelAfterChecksContext{done: make(chan struct{})}
	ctx.remaining.Store(1000)
	_, err = Load(ctx, Options{ConfigPath: path, WorkingDir: workingDir})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled during path repair", err)
	}
}

func TestLoadDropsOnlyMalformedListElements(t *testing.T) {
	configDir := t.TempDir()
	workingDir := t.TempDir()
	directories := []string{"skills-one", "skills-two", "trusted-one", "trusted-two", "imports-one", "imports-two"}
	for _, name := range directories {
		if err := os.Mkdir(filepath.Join(configDir, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(configDir, "config.json")
	content := `{
		"version": 1,
		"compaction": {"preserveToolNames": ["read", 42, "write"]},
		"context": {"importRoots": ["imports-one", 42, "imports-two"]},
		"foreign": {"trustedRoots": ["trusted-one", 42, "trusted-two"]},
		"mcp": {"allowForeign": ["alpha", 42, "beta"]},
		"skills": {"roots": ["skills-one", 42, "skills-two"]}
	}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := Load(context.Background(), Options{ConfigPath: path, WorkingDir: workingDir})
	if err != nil {
		t.Fatal(err)
	}
	wantPaths := func(first, second string) []string {
		return []string{filepath.Join(configDir, first), filepath.Join(configDir, second)}
	}
	if !reflect.DeepEqual(result.Config.Compaction.PreserveToolNames, []string{"read", "write"}) ||
		!reflect.DeepEqual(result.Config.MCP.AllowForeign, []string{"alpha", "beta"}) ||
		!reflect.DeepEqual(result.Config.Skills.Roots, wantPaths("skills-one", "skills-two")) ||
		!reflect.DeepEqual(result.Config.Foreign.TrustedRoots, wantPaths("trusted-one", "trusted-two")) ||
		!reflect.DeepEqual(result.Config.Context.ImportRoots, wantPaths("imports-one", "imports-two")) {
		t.Fatalf("repaired lists = %#v", result.Config)
	}
	if len(result.Warnings) != 5 {
		t.Fatalf("warnings = %#v, want five element repairs", result.Warnings)
	}
	for _, item := range result.Warnings {
		if item.Code != warning.WarnConfigInvalidValue {
			t.Fatalf("warning = %#v, want %s", item, warning.WarnConfigInvalidValue)
		}
	}
}

func TestLoadRoutesEntryWarningsAfterValidation(t *testing.T) {
	workingDir := t.TempDir()
	path := filepath.Join(t.TempDir(), "config.json")
	content := `{
		"version": 1,
		"lsp": {"servers": [
			{"id":"bad-lsp","command":"  ","extensions":[".bad"],"future":true},
			{"id":"good-lsp","command":" server ","extensions":[".good"],"future":true}
		]},
		"mcp": {"servers": [
			{"id":"bad-mcp","transport":"stdio","command":"  ","future":true},
			{"id":"good-mcp","transport":"http","url":"https://example.invalid","future":true}
		]}
	}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := Load(context.Background(), Options{ConfigPath: path, WorkingDir: workingDir})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Config.LSP.Servers) != 2 || result.Config.LSP.Servers[1].Command != "server" || len(result.Config.MCP.Servers) != 1 {
		t.Fatalf("accepted entries = LSP %#v, MCP %#v", result.Config.LSP.Servers, result.Config.MCP.Servers)
	}
	wantCodes := []string{
		warning.WarnLSPConfigInvalidServer,
		warning.WarnConfigUnknownKey,
		warning.WarnConfigMCPIncomplete,
		warning.WarnConfigUnknownKey,
	}
	if len(result.Warnings) != len(wantCodes) {
		t.Fatalf("warnings = %#v", result.Warnings)
	}
	for index, code := range wantCodes {
		if result.Warnings[index].Code != code {
			t.Fatalf("warning[%d] = %#v, want %s", index, result.Warnings[index], code)
		}
	}
}

func TestConfigCloneOwnsNestedCollections(t *testing.T) {
	original := Config{
		LSP:        LSP{Servers: []LSPServer{{Args: []string{"arg"}, Extensions: []string{".go"}, RootMarkers: []string{"go.mod"}}}},
		MCP:        MCP{AllowForeign: []string{"foreign"}, Servers: []MCPServer{{Args: []string{"arg"}, Env: map[string]string{"TOKEN": "secret"}, Headers: map[string]string{"X-Test": "value"}}}},
		Skills:     Skills{Roots: []string{"skills"}},
		Foreign:    Foreign{TrustedRoots: []string{"trusted"}},
		Context:    Context{ImportRoots: []string{"imports"}},
		Compaction: Compaction{PreserveToolNames: []string{"read"}},
	}
	cloned := original.Clone()
	cloned.LSP.Servers[0].Args[0] = "changed"
	cloned.LSP.Servers[0].Extensions[0] = ".changed"
	cloned.LSP.Servers[0].RootMarkers[0] = "changed.mod"
	cloned.MCP.AllowForeign[0] = "changed"
	cloned.MCP.Servers[0].Args[0] = "changed"
	cloned.MCP.Servers[0].Env["TOKEN"] = "changed"
	cloned.MCP.Servers[0].Headers["X-Test"] = "changed"
	cloned.Skills.Roots[0] = "changed"
	cloned.Foreign.TrustedRoots[0] = "changed"
	cloned.Context.ImportRoots[0] = "changed"
	cloned.Compaction.PreserveToolNames[0] = "changed"

	if original.LSP.Servers[0].Args[0] != "arg" || original.LSP.Servers[0].Extensions[0] != ".go" || original.LSP.Servers[0].RootMarkers[0] != "go.mod" ||
		original.MCP.AllowForeign[0] != "foreign" || original.MCP.Servers[0].Args[0] != "arg" || original.MCP.Servers[0].Env["TOKEN"] != "secret" || original.MCP.Servers[0].Headers["X-Test"] != "value" ||
		original.Skills.Roots[0] != "skills" || original.Foreign.TrustedRoots[0] != "trusted" || original.Context.ImportRoots[0] != "imports" || original.Compaction.PreserveToolNames[0] != "read" {
		t.Fatalf("Clone shares nested state with original: %#v", original)
	}
}

type cancelAfterChecksContext struct {
	remaining atomic.Int64
	done      chan struct{}
	once      sync.Once
}

func (c *cancelAfterChecksContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *cancelAfterChecksContext) Done() <-chan struct{}       { return c.done }
func (c *cancelAfterChecksContext) Value(any) any               { return nil }

func (c *cancelAfterChecksContext) Err() error {
	if c.remaining.Add(-1) < 0 {
		c.once.Do(func() { close(c.done) })
		return context.Canceled
	}
	return nil
}
