package foreign_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/plasmid-dev/plasmid/foreign"
	"github.com/plasmid-dev/plasmid/internal/foreignactivation"
	"github.com/plasmid-dev/plasmid/warning"
)

type hostScanner func(context.Context, foreign.Options) (foreign.HostCatalog, error)

func TestHostScannersRejectInvalidPublicOptions(t *testing.T) {
	t.Parallel()

	scanners := map[string]hostScanner{
		"claude":  foreign.ScanClaude,
		"codex":   foreign.ScanCodex,
		"copilot": foreign.ScanCopilot,
	}
	for name, scan := range scanners {
		t.Run(name+" nil context", func(t *testing.T) { assertNilContextRejected(t, scan) })
		t.Run(name+" cancelled context", func(t *testing.T) { assertCancelledContextRejected(t, scan) })
		t.Run(name+" missing working directory", func(t *testing.T) { assertMissingWorkingDirectoryRejected(t, scan) })
		t.Run(name+" working file", func(t *testing.T) { assertWorkingFileRejected(t, scan) })
		t.Run(name+" outside repository", func(t *testing.T) { assertOutsideRepositoryRejected(t, scan) })
	}
}

func assertNilContextRejected(t *testing.T, scan hostScanner) {
	if _, err := scan(nil, foreign.Options{}); err == nil {
		t.Fatal("nil context was accepted")
	}
}

func assertCancelledContextRejected(t *testing.T, scan hostScanner) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := scan(ctx, foreign.Options{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
}

func assertMissingWorkingDirectoryRejected(t *testing.T, scan hostScanner) {
	missing := filepath.Join(t.TempDir(), "missing")
	if _, err := scan(context.Background(), foreign.Options{WorkingDir: missing, RepositoryRoot: missing}); err == nil {
		t.Fatal("missing working directory was accepted")
	}
}

func assertWorkingFileRejected(t *testing.T, scan hostScanner) {
	root := t.TempDir()
	path := filepath.Join(root, "file")
	writeText(t, path, "not a directory")
	if _, err := scan(context.Background(), foreign.Options{WorkingDir: path, RepositoryRoot: root}); err == nil {
		t.Fatal("working file was accepted")
	}
}

func assertOutsideRepositoryRejected(t *testing.T, scan hostScanner) {
	if _, err := scan(context.Background(), foreign.Options{WorkingDir: t.TempDir(), RepositoryRoot: t.TempDir()}); err == nil {
		t.Fatal("working directory outside repository was accepted")
	}
}

func TestScanDiscoversDigestsAndActivationThroughPublicAPI(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	home := t.TempDir()
	writeSkill(t, filepath.Join(root, ".claude", "skills", "demo"), "demo", "Demo")
	writeText(t, filepath.Join(root, ".claude", "commands", "prompt.md"), "plain prompt")
	writeJSON(t, filepath.Join(root, ".mcp.json"), map[string]any{
		"mcpServers": map[string]any{
			"server": map[string]any{"command": "serve", "args": []string{"--stdio"}, "env": map[string]string{"TOKEN": "secret"}},
		},
	})
	var vault foreignactivation.Vault
	catalog, err := foreign.ScanClaudeWithActivations(context.Background(), foreign.Options{
		HomeDir: home, WorkingDir: root, RepositoryRoot: root, ProjectTrusted: true,
	}, &vault)
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Skills) != 1 || catalog.Skills[0].SourceDigest() == "" || len(catalog.Templates) != 1 || catalog.Templates[0].SourceDigest() == "" {
		t.Fatalf("catalog = %#v", catalog)
	}
	descriptor, ok := foreign.TransferMCPActivation(catalog.MCPServers[0], &vault)
	if !ok || descriptor.Command != "serve" || len(descriptor.Args) != 1 || descriptor.Env["TOKEN"] != "secret" {
		t.Fatalf("descriptor = %#v, found = %v", descriptor, ok)
	}
	if _, ok := foreign.TransferMCPActivation(catalog.MCPServers[0], &vault); ok {
		t.Fatal("activation descriptor was reusable")
	}
	if _, ok := foreign.TransferMCPActivation(catalog.MCPServers[0], nil); ok {
		t.Fatal("nil vault returned an activation")
	}
}

func TestClaudeScannerHandlesMalformedPublicInputs(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	home := t.TempDir()
	nested := filepath.Join(root, "nested")
	mustMkdir(t, nested)

	writeText(t, filepath.Join(root, ".claude", "skills", "regular-file"), "ignored")
	mustMkdir(t, filepath.Join(root, ".claude", "skills", "missing-markdown"))
	writeText(t, filepath.Join(root, ".claude", "skills", "invalid", "SKILL.md"), "---\nname: invalid\n---\n")
	writeText(t, filepath.Join(root, ".claude", "commands", "ignored.txt"), "ignored")
	mustMkdir(t, filepath.Join(root, ".claude", "commands", "directory.md"))
	writeText(t, filepath.Join(root, ".claude", "commands", "broken.md"), "---\nmissing close")

	writeJSON(t, filepath.Join(home, ".claude.json"), map[string]any{
		"projects": map[string]any{
			nested: "invalid",
			root:   map[string]any{"mcpServers": "invalid"},
		},
		"mcpServers": "invalid",
	})
	writeJSON(t, filepath.Join(root, ".mcp.json"), map[string]any{"wrong": map[string]any{}})

	pluginsRoot := filepath.Join(home, ".claude", "plugins")
	outside := t.TempDir()
	missing := filepath.Join(pluginsRoot, "missing")
	valid := filepath.Join(pluginsRoot, "valid")
	writeJSON(t, filepath.Join(valid, ".claude-plugin", "plugin.json"), map[string]any{
		"name": "valid", "version": "2", "skills": 42, "commands": []string{"../escape", "commands"},
		"mcpServers": map[string]any{"bad": map[string]any{"type": "unknown"}},
	})
	writeJSON(t, filepath.Join(pluginsRoot, "installed_plugins.json"), map[string]any{
		"version": 2,
		"plugins": map[string]any{
			"relative": []map[string]any{{"scope": "local", "installPath": "relative"}},
			"outside":  []map[string]any{{"scope": "project", "installPath": outside}},
			"missing":  []map[string]any{{"scope": "user", "installPath": missing}},
			"valid":    []map[string]any{{"scope": "unknown", "installPath": valid}},
		},
	})
	writeText(t, filepath.Join(home, ".claude", "settings.json"), "not json")

	catalog, err := foreign.ScanClaude(context.Background(), foreign.Options{
		HomeDir: home, WorkingDir: nested, RepositoryRoot: root, ProjectTrusted: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, code := range []string{
		warning.WarnForeignSkillMissingMarkdown,
		warning.WarnForeignInstallPathRelative,
		warning.WarnForeignPathEscape,
		warning.WarnForeignInstallPathMissing,
		warning.WarnForeignManifestInvalid,
		warning.WarnForeignMCPShapeUnknown,
		warning.WarnForeignEntryShapeUnknown,
	} {
		if !hasWarning(catalog.Warnings, code) {
			t.Errorf("missing warning %q: %#v", code, catalog.Warnings)
		}
	}
}

func TestCodexScannerHandlesTOMLAndMarketplaceVariants(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	home := t.TempDir()
	codexHome := filepath.Join(home, ".codex")
	config := `outside = "ignored"
[broken
ignored = true
[mcp_servers."http.server"]
url = "https://example.invalid/mcp" # comment
http_headers = { Authorization = "secret", 'X-Key' = 'value' }
[mcp_servers.stdio]
command = 'serve'
args = ["one", 'two']
env = { TOKEN = "secret" }
[mcp_servers.missing]
enabled = true
[mcp_servers.bad_command]
command = ["wrong"]
[mcp_servers.bad_enabled]
command = "serve"
enabled = ["wrong"]
[plugins."demo@market"]
enabled = true
[plugins."demo@market"]
enabled = false
`
	writeText(t, filepath.Join(codexHome, "config.toml"), config)
	writeText(t, filepath.Join(root, ".codex", "config.toml"), "[mcp_servers.project]\ncommand = \"project\"\n")

	pluginRoot := filepath.Join(root, "plugin")
	writeJSON(t, filepath.Join(pluginRoot, ".codex-plugin", "plugin.json"), map[string]any{
		"name": "demo", "version": "1", "skills": []string{"./skills", "../escape"}, "commands": "commands", "mcpServers": "./mcp.json",
	})
	writeSkill(t, filepath.Join(pluginRoot, "skills", "demo"), "demo", "Demo")
	writeJSON(t, filepath.Join(pluginRoot, "mcp.json"), map[string]any{"mcp_servers": map[string]any{"plugin": map[string]any{"command": "serve"}}})
	writeJSON(t, filepath.Join(root, ".agents", "plugins", "marketplace.json"), map[string]any{
		"name": "market",
		"plugins": []any{
			map[string]any{"name": "remote", "source": map[string]any{"source": "github", "path": "./remote"}},
			map[string]any{"name": "escape", "source": map[string]any{"source": "local", "path": "./../escape"}},
			map[string]any{"name": "missing", "source": map[string]any{"source": "local", "path": "./missing"}},
			map[string]any{"name": "demo", "version": "2", "source": map[string]any{"source": "local", "path": "./plugin"}},
		},
	})
	writeText(t, filepath.Join(home, ".agents", "plugins", "marketplace.json"), "not json")
	writeJSON(t, filepath.Join(root, ".claude-plugin", "marketplace.json"), map[string]any{"name": "compat", "plugins": []any{}})

	var vault foreignactivation.Vault
	catalog, err := foreign.ScanCodexWithActivations(context.Background(), foreign.Options{
		HomeDir: home, CodexHome: codexHome, AdminSkillsDir: filepath.Join(home, "admin"),
		WorkingDir: root, RepositoryRoot: root, ProjectTrusted: true,
	}, &vault)
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.MCPServers) < 4 || len(catalog.Skills) != 1 {
		t.Fatalf("catalog = %#v", catalog)
	}
	if descriptor, ok := takeActivationByName(catalog, "http.server", &vault); !ok || descriptor.URL == "" || descriptor.Headers["Authorization"] != "secret" {
		t.Fatalf("HTTP descriptor = %#v, found = %v", descriptor, ok)
	}
	for _, code := range []string{warning.WarnForeignTOMLUnsupported, warning.WarnForeignEntryShapeUnknown, warning.WarnForeignPathEscape, warning.WarnForeignInstallPathMissing, warning.WarnForeignManifestInvalid} {
		if !hasWarning(catalog.Warnings, code) {
			t.Errorf("missing warning %q: %#v", code, catalog.Warnings)
		}
	}
}

func TestCopilotScannerHandlesPreviewPluginAndMCPVariants(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	home := t.TempDir()
	nested := filepath.Join(root, "nested")
	mustMkdir(t, nested)
	writeText(t, filepath.Join(root, ".github", "prompts", "preview.prompt.md"), "preview")
	writeText(t, filepath.Join(root, ".github", "prompts", "ignored.md"), "ignored")
	writeJSON(t, filepath.Join(root, ".mcp.json"), map[string]any{"mcpServers": map[string]any{"project": map[string]any{"command": "serve"}}})
	writeText(t, filepath.Join(root, ".github", "mcp.json"), "not json")
	writeJSON(t, filepath.Join(home, ".copilot", "mcp-config.json"), map[string]any{"bare": map[string]any{"command": "serve"}})

	plugins := filepath.Join(home, ".copilot", "installed-plugins")
	writeText(t, filepath.Join(plugins, "regular"), "ignored")
	mustMkdir(t, filepath.Join(plugins, "empty-group"))
	missingManifest := filepath.Join(plugins, "market", "missing")
	mustMkdir(t, missingManifest)
	invalidManifest := filepath.Join(plugins, "market", "invalid")
	writeText(t, filepath.Join(invalidManifest, ".plugin", "plugin.json"), "not json")
	valid := filepath.Join(plugins, "market", "valid")
	writeJSON(t, filepath.Join(valid, ".plugin", "plugin.json"), map[string]any{
		"name": "valid", "version": "1", "skills": "skills", "commands": "commands", "mcpServers": "mcp.json",
	})
	writeSkill(t, filepath.Join(valid, "skills", "demo"), "demo", "Demo")
	writeText(t, filepath.Join(valid, "commands", "prompt.md"), "prompt")
	writeJSON(t, filepath.Join(valid, "mcp.json"), map[string]any{"plugin": map[string]any{"url": "https://example.invalid"}})

	catalog, err := foreign.ScanCopilot(context.Background(), foreign.Options{
		HomeDir: home, WorkingDir: nested, RepositoryRoot: root, ProjectTrusted: true, EnableCopilotPreview: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Skills) != 1 || len(catalog.Templates) != 2 || len(catalog.MCPServers) != 2 {
		t.Fatalf("catalog = %#v", catalog)
	}
	for _, code := range []string{warning.WarnForeignManifestMissing, warning.WarnForeignManifestInvalid, warning.WarnForeignMCPShapeUnknown} {
		if !hasWarning(catalog.Warnings, code) {
			t.Errorf("missing warning %q: %#v", code, catalog.Warnings)
		}
	}
}

func TestPublicScannersApplyDefaultsAndBounds(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	nested := filepath.Join(root, "nested")
	mustMkdir(t, filepath.Join(root, ".git"))
	mustMkdir(t, nested)
	writeSkill(t, filepath.Join(root, ".agents", "skills", "large"), "large", strings.Repeat("x", 128))

	catalog, err := foreign.ScanCodex(context.Background(), foreign.Options{
		HomeDir: home, CodexHome: filepath.Join(home, ".codex"), AdminSkillsDir: filepath.Join(home, "admin"),
		WorkingDir: nested, MaxFileBytes: 32,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Skills) != 0 || !hasWarning(catalog.Warnings, warning.WarnForeignIndexUnreadable) {
		t.Fatalf("catalog = %#v", catalog)
	}
}

func TestRepositoryRootDefaultsToWorkingDirectoryWithoutGit(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	home := t.TempDir()
	writeSkill(t, filepath.Join(root, ".claude", "skills", "demo"), "demo", "Demo")
	catalog, err := foreign.ScanClaude(context.Background(), foreign.Options{HomeDir: home, WorkingDir: root})
	if err != nil || len(catalog.Skills) != 1 {
		t.Fatalf("catalog = %#v, error = %v", catalog, err)
	}
}

func TestCombinedCatalogMergesEqualPortableSources(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	home := t.TempDir()
	writeSkill(t, filepath.Join(root, ".claude", "skills", "shared"), "shared", "Same")
	writeSkill(t, filepath.Join(root, ".agents", "skills", "shared"), "shared", "Same")
	catalog, err := foreign.Scan(context.Background(), foreign.Options{HomeDir: home, WorkingDir: root, RepositoryRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Skills) != 1 || len(catalog.Skills[0].Provenance) < 2 {
		t.Fatalf("catalog = %#v", catalog)
	}
}

func TestCodexTOMLPublicEdgeCases(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	home := t.TempDir()
	codexHome := filepath.Join(home, ".codex")
	config := `
[mcp_servers."escaped.name"]
command = "serve\\\"quoted" # outside comment
args = ["one,two", 'three']
env = { "A.B" = "one,two", plain = 'three' }
[mcp_servers.'single.name']
url = 'https://example.invalid/#fragment'
headers = { Key = "value" }
[mcp_servers.bad_array]
command = "serve"
args = ["one",]
[mcp_servers.bad_map]
command = "serve"
env = { missing }
[mcp_servers.bad_key]
command = true
[mcp_servers.bad_url]
url = false
[mcp_servers.bad_quote]
command = "unterminated
[mcp_servers..empty]
command = "ignored"
[mcp_servers."unterminated]
command = "ignored"
`
	writeText(t, filepath.Join(codexHome, "config.toml"), config)
	catalog, err := foreign.ScanCodex(context.Background(), foreign.Options{
		HomeDir: home, CodexHome: codexHome, AdminSkillsDir: filepath.Join(home, "admin"), WorkingDir: root, RepositoryRoot: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.MCPServers) != 4 || !hasWarning(catalog.Warnings, warning.WarnForeignTOMLUnsupported) {
		t.Fatalf("catalog = %#v", catalog)
	}
}

func TestClaudePluginIndexPublicVariants(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name  string
		index any
		code  string
	}{
		{name: "invalid shape", index: []string{"invalid"}, code: warning.WarnForeignEntryShapeUnknown},
		{name: "missing plugins", index: map[string]any{"version": 2}, code: warning.WarnForeignEntryShapeUnknown},
		{name: "unsupported version", index: map[string]any{"version": 1, "plugins": map[string]any{}}, code: warning.WarnForeignIndexUnsupportedVersion},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			home := t.TempDir()
			writeJSON(t, filepath.Join(home, ".claude", "plugins", "installed_plugins.json"), test.index)
			catalog, err := foreign.ScanClaude(context.Background(), foreign.Options{HomeDir: home, WorkingDir: root, RepositoryRoot: root})
			if err != nil || !hasWarning(catalog.Warnings, test.code) {
				t.Fatalf("catalog = %#v, error = %v", catalog, err)
			}
		})
	}
}

func TestClaudePluginComponentPublicVariants(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	home := t.TempDir()
	pluginsRoot := filepath.Join(home, ".claude", "plugins")
	defaults := filepath.Join(pluginsRoot, "defaults")
	writeSkill(t, filepath.Join(defaults, "skills", "default"), "default", "Default")
	writeText(t, filepath.Join(defaults, "commands", "default.md"), "prompt")
	writeJSON(t, filepath.Join(defaults, ".mcp.json"), map[string]any{"mcpServers": map[string]any{"default": map[string]any{"command": "serve"}}})

	fileMCP := filepath.Join(pluginsRoot, "file-mcp")
	writeJSON(t, filepath.Join(fileMCP, ".claude-plugin", "plugin.json"), map[string]any{
		"name": "file-mcp", "version": "1", "mcpServers": "mcp.json",
	})
	writeJSON(t, filepath.Join(fileMCP, "mcp.json"), map[string]any{"mcpServers": map[string]any{"file": map[string]any{"command": "serve"}}})

	inline := filepath.Join(pluginsRoot, "inline")
	writeJSON(t, filepath.Join(inline, ".claude-plugin", "plugin.json"), map[string]any{
		"name": "inline", "mcpServers": map[string]any{"inline": map[string]any{"url": "https://example.invalid"}},
	})
	missingName := filepath.Join(pluginsRoot, "missing-name")
	writeJSON(t, filepath.Join(missingName, ".claude-plugin", "plugin.json"), map[string]any{"version": "1"})

	writeJSON(t, filepath.Join(pluginsRoot, "installed_plugins.json"), map[string]any{
		"version": 2,
		"plugins": map[string]any{
			"defaults":     []map[string]any{{"scope": "local", "installPath": defaults}},
			"file-mcp":     []map[string]any{{"scope": "project", "installPath": fileMCP}},
			"inline":       []map[string]any{{"scope": "user", "installPath": inline}},
			"missing-name": []map[string]any{{"scope": "user", "installPath": missingName}},
		},
	})
	writeJSON(t, filepath.Join(home, ".claude", "settings.json"), map[string]any{"enabledPlugins": map[string]bool{"defaults": true}})
	writeJSON(t, filepath.Join(root, ".claude", "settings.json"), map[string]any{"enabledPlugins": map[string]bool{"defaults": false}})
	writeJSON(t, filepath.Join(root, ".claude", "settings.local.json"), map[string]any{"enabledPlugins": map[string]bool{"defaults": true}})

	catalog, err := foreign.ScanClaude(context.Background(), foreign.Options{HomeDir: home, WorkingDir: root, RepositoryRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Skills) != 1 || len(catalog.Templates) != 0 || len(catalog.MCPServers) != 3 || !hasWarning(catalog.Warnings, warning.WarnForeignManifestInvalid) {
		t.Fatalf("catalog = %#v", catalog)
	}
}

func TestClaudeMCPPublicVariants(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name       string
		rootConfig any
		project    any
		code       string
	}{
		{name: "invalid root", rootConfig: []string{"invalid"}, code: warning.WarnForeignMCPShapeUnknown},
		{name: "invalid projects", rootConfig: map[string]any{"projects": "invalid"}, code: warning.WarnForeignMCPShapeUnknown},
		{name: "valid user", rootConfig: map[string]any{"mcpServers": map[string]any{"user": map[string]any{"command": "serve"}}}, project: map[string]any{"mcpServers": "invalid"}, code: warning.WarnForeignMCPShapeUnknown},
		{name: "invalid project json", rootConfig: map[string]any{}, project: []string{"invalid"}, code: warning.WarnForeignMCPShapeUnknown},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			home := t.TempDir()
			writeJSON(t, filepath.Join(home, ".claude.json"), test.rootConfig)
			if test.project != nil {
				writeJSON(t, filepath.Join(root, ".mcp.json"), test.project)
			}
			catalog, err := foreign.ScanClaude(context.Background(), foreign.Options{HomeDir: home, WorkingDir: root, RepositoryRoot: root, ProjectTrusted: true})
			if err != nil || !hasWarning(catalog.Warnings, test.code) {
				t.Fatalf("catalog = %#v, error = %v", catalog, err)
			}
		})
	}
}

func TestCodexPluginMCPPublicVariants(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		mcp  any
		code string
	}{
		{name: "invalid document", mcp: []string{"invalid"}, code: warning.WarnForeignMCPShapeUnknown},
		{name: "invalid wrapper", mcp: map[string]any{"mcp_servers": "invalid"}, code: warning.WarnForeignMCPShapeUnknown},
		{name: "bare", mcp: map[string]any{"server": map[string]any{"command": "serve"}}, code: warning.WarnForeignMCPInert},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			home := t.TempDir()
			plugin := filepath.Join(root, "plugin")
			writeJSON(t, filepath.Join(plugin, ".codex-plugin", "plugin.json"), map[string]any{"name": "demo", "mcpServers": "./mcp.json"})
			writeJSON(t, filepath.Join(plugin, "mcp.json"), test.mcp)
			writeJSON(t, filepath.Join(root, ".agents", "plugins", "marketplace.json"), map[string]any{
				"name": "market", "plugins": []any{map[string]any{"name": "demo", "source": map[string]any{"source": "local", "path": "./plugin"}}},
			})
			catalog, err := foreign.ScanCodex(context.Background(), foreign.Options{
				HomeDir: home, CodexHome: filepath.Join(home, ".codex"), AdminSkillsDir: filepath.Join(home, "admin"), WorkingDir: root, RepositoryRoot: root,
			})
			if err != nil || !hasWarning(catalog.Warnings, test.code) {
				t.Fatalf("catalog = %#v, error = %v", catalog, err)
			}
		})
	}
}

func TestCopilotMCPPublicVariants(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name   string
		config any
		code   string
		count  int
	}{
		{name: "invalid document", config: []string{"invalid"}, code: warning.WarnForeignMCPShapeUnknown},
		{name: "invalid wrapper", config: map[string]any{"mcpServers": "invalid"}, code: warning.WarnForeignMCPShapeUnknown},
		{name: "valid wrapper", config: map[string]any{"mcpServers": map[string]any{"user": map[string]any{"command": "serve"}}}, code: warning.WarnForeignMCPInert, count: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			home := t.TempDir()
			writeJSON(t, filepath.Join(home, ".copilot", "mcp-config.json"), test.config)
			catalog, err := foreign.ScanCopilot(context.Background(), foreign.Options{HomeDir: home, WorkingDir: root, RepositoryRoot: root})
			if err != nil || len(catalog.MCPServers) != test.count || !hasWarning(catalog.Warnings, test.code) {
				t.Fatalf("catalog = %#v, error = %v", catalog, err)
			}
		})
	}
}

func TestTemplatePrecedencePublicBehavior(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	home := t.TempDir()
	writeSkill(t, filepath.Join(root, ".claude", "skills", "same"), "same", "Skill")
	writeText(t, filepath.Join(root, ".claude", "commands", "same.md"), "shadowed")
	writeText(t, filepath.Join(root, ".claude", "commands", "duplicate.md"), "project")
	writeText(t, filepath.Join(home, ".claude", "commands", "duplicate.md"), "user")
	catalog, err := foreign.ScanClaude(context.Background(), foreign.Options{HomeDir: home, WorkingDir: root, RepositoryRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Templates) != 1 || !hasWarning(catalog.Warnings, warning.WarnForeignDuplicateTemplate) {
		t.Fatalf("catalog = %#v", catalog)
	}
}

func TestTemplateScannerPublicVariants(t *testing.T) {
	t.Parallel()

	t.Run("rich projection", testRichTemplateProjection)
	t.Run("root is file", testTemplateRootFile)
	t.Run("oversized template", testOversizedTemplate)
	t.Run("symlink ignored", testTemplateSymlinkIgnored)
}

func testRichTemplateProjection(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	writeText(t, filepath.Join(root, ".claude", "commands", "rich.md"), `---
arguments: [target]
allowed-tools: [Read]
disallowed-tools: Write
disable-model-invocation: true
user-invocable: false
---
body`)
	catalog, err := foreign.ScanClaude(context.Background(), foreign.Options{HomeDir: home, WorkingDir: root, RepositoryRoot: root})
	if err != nil || len(catalog.Templates) != 1 {
		t.Fatalf("catalog = %#v, error = %v", catalog, err)
	}
	template := catalog.Templates[0]
	if len(template.Arguments) != 1 || len(template.Permissions.Allowed) != 1 || len(template.Permissions.Denied) != 1 || template.UserInvocable || template.ModelInvocable || !template.RestrictsTools {
		t.Fatalf("template = %#v", template)
	}
}

func testTemplateRootFile(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	writeText(t, filepath.Join(root, ".claude", "commands"), "not a directory")
	catalog, err := foreign.ScanClaude(context.Background(), foreign.Options{HomeDir: home, WorkingDir: root, RepositoryRoot: root})
	if err != nil || !hasWarning(catalog.Warnings, warning.WarnForeignIndexUnreadable) {
		t.Fatalf("catalog = %#v, error = %v", catalog, err)
	}
}

func testOversizedTemplate(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	writeText(t, filepath.Join(root, ".claude", "commands", "large.md"), strings.Repeat("x", 128))
	catalog, err := foreign.ScanClaude(context.Background(), foreign.Options{HomeDir: home, WorkingDir: root, RepositoryRoot: root, MaxFileBytes: 16})
	if err != nil || len(catalog.Templates) != 0 || !hasWarning(catalog.Warnings, warning.WarnForeignIndexUnreadable) {
		t.Fatalf("catalog = %#v, error = %v", catalog, err)
	}
}

func testTemplateSymlinkIgnored(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	commands := filepath.Join(root, ".claude", "commands")
	mustMkdir(t, commands)
	outside := filepath.Join(t.TempDir(), "outside.md")
	writeText(t, outside, "outside")
	if err := os.Symlink(outside, filepath.Join(commands, "link.md")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	catalog, err := foreign.ScanClaude(context.Background(), foreign.Options{HomeDir: home, WorkingDir: root, RepositoryRoot: root})
	if err != nil || len(catalog.Templates) != 0 {
		t.Fatalf("catalog = %#v, error = %v", catalog, err)
	}
}

func TestCodexPluginPublicVariants(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	home := t.TempDir()
	valid := filepath.Join(root, "valid")
	writeJSON(t, filepath.Join(valid, ".codex-plugin", "plugin.json"), map[string]any{
		"name": "valid", "commands": "./commands", "skills": "./skills", "mcpServers": "./mcp.json",
	})
	writeText(t, filepath.Join(valid, "commands", "prompt.md"), "prompt")
	writeSkill(t, filepath.Join(valid, "skills", "skill"), "skill", "Skill")
	writeJSON(t, filepath.Join(valid, "mcp.json"), map[string]any{"server": map[string]any{"command": "serve"}})
	missingManifest := filepath.Join(root, "missing-manifest")
	mustMkdir(t, missingManifest)
	invalidShape := filepath.Join(root, "invalid-shape")
	writeJSON(t, filepath.Join(invalidShape, ".codex-plugin", "plugin.json"), map[string]any{"name": "invalid", "commands": 42})
	writeJSON(t, filepath.Join(root, ".agents", "plugins", "marketplace.json"), map[string]any{
		"name": "market",
		"plugins": []any{
			map[string]any{"name": "valid", "source": map[string]any{"source": "local", "path": "./valid"}},
			map[string]any{"name": "missing", "source": map[string]any{"source": "local", "path": "./missing-manifest"}},
			map[string]any{"name": "invalid", "source": map[string]any{"source": "local", "path": "./invalid-shape"}},
		},
	})
	catalog, err := foreign.ScanCodex(context.Background(), foreign.Options{
		HomeDir: home, CodexHome: filepath.Join(home, ".codex"), AdminSkillsDir: filepath.Join(home, "admin"), WorkingDir: root, RepositoryRoot: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Skills) != 1 || len(catalog.Templates) != 1 || len(catalog.MCPServers) != 1 || !hasWarning(catalog.Warnings, warning.WarnForeignManifestMissing) || !hasWarning(catalog.Warnings, warning.WarnForeignManifestInvalid) {
		t.Fatalf("catalog = %#v", catalog)
	}
}

func TestCopilotPluginDirectoryPublicVariants(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	home := t.TempDir()
	pluginsRoot := filepath.Join(home, ".copilot", "installed-plugins")
	group := filepath.Join(pluginsRoot, "market")
	writeText(t, filepath.Join(group, "regular"), "ignored")
	valid := filepath.Join(group, "valid")
	writeJSON(t, filepath.Join(valid, ".github", "plugin", "plugin.json"), map[string]any{
		"name": "valid", "skills": "skills", "commands": "commands", "mcpServers": map[string]any{"inline": map[string]any{"command": "serve"}},
	})
	writeSkill(t, filepath.Join(valid, "skills", "skill"), "skill", "Skill")
	writeText(t, filepath.Join(valid, "commands", "prompt.md"), "prompt")
	if err := os.Symlink(valid, filepath.Join(group, "symlink")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	catalog, err := foreign.ScanCopilot(context.Background(), foreign.Options{HomeDir: home, WorkingDir: root, RepositoryRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Skills) != 1 || len(catalog.Templates) != 1 || len(catalog.MCPServers) != 1 {
		t.Fatalf("catalog = %#v", catalog)
	}
}

func TestPublicScannersHonorCancellationAtDiscoveryBoundaries(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	codexHome := filepath.Join(home, ".codex")
	writeSkill(t, filepath.Join(root, ".claude", "skills", "claude"), "claude", "Claude")
	writeSkill(t, filepath.Join(root, ".agents", "skills", "codex"), "codex", "Codex")
	writeSkill(t, filepath.Join(root, ".github", "skills", "copilot"), "copilot", "Copilot")
	writeText(t, filepath.Join(root, ".claude", "commands", "prompt.md"), "prompt")
	writeText(t, filepath.Join(root, ".github", "prompts", "preview.prompt.md"), "preview")
	writeJSON(t, filepath.Join(root, ".mcp.json"), map[string]any{"mcpServers": map[string]any{"server": map[string]any{"command": "serve"}}})
	writeText(t, filepath.Join(codexHome, "config.toml"), "[mcp_servers.server]\ncommand = \"serve\"\n")
	claudePlugin := filepath.Join(home, ".claude", "plugins", "demo")
	writeJSON(t, filepath.Join(claudePlugin, ".claude-plugin", "plugin.json"), map[string]any{"name": "demo", "skills": "skills", "commands": "commands", "mcpServers": map[string]any{"plugin": map[string]any{"command": "serve"}}})
	writeSkill(t, filepath.Join(claudePlugin, "skills", "demo"), "demo", "Demo")
	writeText(t, filepath.Join(claudePlugin, "commands", "demo.md"), "demo")
	writeJSON(t, filepath.Join(home, ".claude", "plugins", "installed_plugins.json"), map[string]any{"version": 2, "plugins": map[string]any{"demo": []any{map[string]any{"scope": "user", "installPath": claudePlugin}}}})
	codexPlugin := filepath.Join(root, "codex-plugin")
	writeJSON(t, filepath.Join(codexPlugin, ".codex-plugin", "plugin.json"), map[string]any{"name": "codex", "skills": "./skills", "commands": "./commands", "mcpServers": "./mcp.json"})
	writeSkill(t, filepath.Join(codexPlugin, "skills", "demo"), "demo", "Demo")
	writeText(t, filepath.Join(codexPlugin, "commands", "demo.md"), "demo")
	writeJSON(t, filepath.Join(codexPlugin, "mcp.json"), map[string]any{"plugin": map[string]any{"command": "serve"}})
	writeJSON(t, filepath.Join(root, ".agents", "plugins", "marketplace.json"), map[string]any{"name": "local", "plugins": []any{map[string]any{"name": "codex", "source": map[string]any{"source": "local", "path": "./codex-plugin"}}}})
	copilotPlugin := filepath.Join(home, ".copilot", "installed-plugins", "market", "demo")
	writeJSON(t, filepath.Join(copilotPlugin, "plugin.json"), map[string]any{"name": "copilot", "skills": "skills", "commands": "commands", "mcpServers": map[string]any{"plugin": map[string]any{"command": "serve"}}})
	writeSkill(t, filepath.Join(copilotPlugin, "skills", "demo"), "demo", "Demo")
	writeText(t, filepath.Join(copilotPlugin, "commands", "demo.md"), "demo")
	options := foreign.Options{
		HomeDir: home, CodexHome: codexHome, AdminSkillsDir: filepath.Join(home, "admin"),
		WorkingDir: root, RepositoryRoot: root, ProjectTrusted: true, EnableCopilotPreview: true,
	}
	scanners := []func(context.Context, foreign.Options) error{
		func(ctx context.Context, options foreign.Options) error {
			_, err := foreign.ScanClaude(ctx, options)
			return err
		},
		func(ctx context.Context, options foreign.Options) error {
			_, err := foreign.ScanCodex(ctx, options)
			return err
		},
		func(ctx context.Context, options foreign.Options) error {
			_, err := foreign.ScanCopilot(ctx, options)
			return err
		},
		func(ctx context.Context, options foreign.Options) error {
			_, err := foreign.Scan(ctx, options)
			return err
		},
	}
	for _, scan := range scanners {
		if err := scan(&countdownContext{threshold: 2}, options); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation after scanner construction: error = %v", err)
		}
		for threshold := int64(1); threshold <= 300; threshold++ {
			err := scan(&countdownContext{threshold: threshold}, options)
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Fatalf("threshold %d: error = %v", threshold, err)
			}
		}
	}
}

type countdownContext struct {
	checks    atomic.Int64
	threshold int64
}

func (c *countdownContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *countdownContext) Done() <-chan struct{}       { return nil }
func (c *countdownContext) Value(any) any               { return nil }

func (c *countdownContext) Err() error {
	if c.checks.Add(1) >= c.threshold {
		return context.Canceled
	}
	return nil
}

func TestPublicScannersWarnOnUnreadableSources(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	skill := filepath.Join(root, ".claude", "skills", "unreadable", "SKILL.md")
	writeSkill(t, filepath.Dir(skill), "unreadable", "Unreadable")
	if err := os.Chmod(skill, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(skill, 0o600) })
	catalog, err := foreign.ScanClaude(context.Background(), foreign.Options{HomeDir: home, WorkingDir: root, RepositoryRoot: root})
	if err != nil || !hasWarning(catalog.Warnings, warning.WarnForeignIndexUnreadable) {
		t.Fatalf("catalog = %#v, error = %v", catalog, err)
	}
}

func TestSkillScannerProjectsAllPublicMetadata(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	home := t.TempDir()
	writeText(t, filepath.Join(root, ".claude", "skills", "rich", "SKILL.md"), `---
name: rich
description: Rich
license: MIT
compatibility: go
metadata:
  owner: team
arguments: [target]
globs: ["**/*.go"]
allowed-tools: [Read]
disallowed-tools: Write
---
body`)
	catalog, err := foreign.ScanClaude(context.Background(), foreign.Options{HomeDir: home, WorkingDir: root, RepositoryRoot: root})
	if err != nil || len(catalog.Skills) != 1 {
		t.Fatalf("catalog = %#v, error = %v", catalog, err)
	}
	skill := catalog.Skills[0]
	if skill.License != "MIT" || skill.Compatibility != "go" || len(skill.Metadata) != 1 || len(skill.Arguments) != 1 || len(skill.Globs) != 1 || len(skill.Permissions.Allowed) != 1 || len(skill.Permissions.Denied) != 1 || !skill.RestrictsTools {
		t.Fatalf("skill = %#v", skill)
	}
	if !hasWarning(catalog.Warnings, warning.WarnForeignPermissionInert) {
		t.Fatalf("warnings = %#v", catalog.Warnings)
	}
}

func TestSkillMarkdownSymlinkEscapeIsRejected(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	home := t.TempDir()
	directory := filepath.Join(root, ".claude", "skills", "escape")
	mustMkdir(t, directory)
	outside := filepath.Join(t.TempDir(), "SKILL.md")
	writeText(t, outside, "---\nname: escape\ndescription: Escape\n---\n")
	if err := os.Symlink(outside, filepath.Join(directory, "SKILL.md")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	catalog, err := foreign.ScanClaude(context.Background(), foreign.Options{HomeDir: home, WorkingDir: root, RepositoryRoot: root})
	if err != nil || len(catalog.Skills) != 0 || !hasWarning(catalog.Warnings, warning.WarnForeignPathEscape) {
		t.Fatalf("catalog = %#v, error = %v", catalog, err)
	}
}

func TestScannerAllowsEmptyHomeOption(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, filepath.Join(root, ".claude", "skills", "demo"), "demo", "Demo")
	catalog, err := foreign.ScanClaude(context.Background(), foreign.Options{WorkingDir: root, RepositoryRoot: root, MaxEntries: 1})
	if err != nil || len(catalog.Skills) != 1 {
		t.Fatalf("catalog = %#v, error = %v", catalog, err)
	}
}

func TestCodexTOMLInvalidEntriesRemainWarnings(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	home := t.TempDir()
	codexHome := filepath.Join(home, ".codex")
	config := `
[mcp_servers.valid]
missing equals
= "missing-key"
command = "serve"
args = []
env = {}
[mcp_servers.invalid_array_type]
command = "serve"
args = [true]
[mcp_servers.invalid_map_key]
command = "serve"
env = { "unterminated = "value" }
[mcp_servers.invalid_map_value]
command = "serve"
env = { key = true }
[mcp_servers.empty_array_item]
command = "serve"
args = [, "value"]
[mcp_servers.unterminated_array_quote]
command = "serve"
args = ["value]
`
	writeText(t, filepath.Join(codexHome, "config.toml"), config)
	catalog, err := foreign.ScanCodex(context.Background(), foreign.Options{
		HomeDir: home, CodexHome: codexHome, AdminSkillsDir: filepath.Join(home, "admin"), WorkingDir: root, RepositoryRoot: root,
	})
	if err != nil || len(catalog.MCPServers) != 6 || !hasWarning(catalog.Warnings, warning.WarnForeignTOMLUnsupported) {
		t.Fatalf("catalog = %#v, error = %v", catalog, err)
	}
}

func takeActivationByName(catalog foreign.HostCatalog, name string, vault *foreignactivation.Vault) (foreignactivation.Descriptor, bool) {
	for _, server := range catalog.MCPServers {
		if server.Name == name {
			return foreign.TransferMCPActivation(server, vault)
		}
	}
	return foreignactivation.Descriptor{}, false
}

func hasWarning(values []warning.Warning, code string) bool {
	for _, item := range values {
		if item.Code == code {
			return true
		}
	}
	return false
}

func writeSkill(t *testing.T, directory, name, description string) {
	t.Helper()
	writeText(t, filepath.Join(directory, "SKILL.md"), "---\nname: "+name+"\ndescription: "+description+"\n---\nbody\n")
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	writeText(t, path, string(data))
}

func writeText(t *testing.T, path, value string) {
	t.Helper()
	mustMkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
}
