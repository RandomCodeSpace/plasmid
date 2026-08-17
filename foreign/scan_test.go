package foreign

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plasmid-dev/plasmid/warning"
)

func TestScanClaudeProjectSkillProvenance(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	skillDir := filepath.Join(root, ".claude", "skills", "review")
	if err := os.MkdirAll(skillDir, 0o700); err != nil {
		t.Fatal(err)
	}
	source := "---\nname: review\ndescription: Review changes\nallowed-tools: Bash(git *)\n---\nBody\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}

	catalog, err := ScanClaude(context.Background(), Options{
		HomeDir:        home,
		WorkingDir:     root,
		RepositoryRoot: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if catalog.Host != HostClaude || len(catalog.Skills) != 1 {
		t.Fatalf("catalog = %#v", catalog)
	}
	skill := catalog.Skills[0]
	if skill.Name != "review" || skill.Description != "Review changes" || skill.Body != "Body\n" {
		t.Fatalf("skill = %#v", skill)
	}
	if len(skill.Permissions.Allowed) != 1 || skill.Permissions.Allowed[0] != (ToolPattern{Tool: "Bash", Argument: "git *"}) || len(skill.Permissions.Denied) != 0 {
		t.Fatalf("permissions = %#v", skill.Permissions)
	}
	if len(skill.Provenance) != 1 {
		t.Fatalf("provenance = %#v", skill.Provenance)
	}
	provenance := skill.Provenance[0]
	if provenance.Host != HostClaude || provenance.Scope != ScopeProject || provenance.Classification != ClassificationDocumented || provenance.Trust != TrustUntrusted || !provenance.Enabled {
		t.Fatalf("provenance = %#v", provenance)
	}
	if len(catalog.Warnings) != 1 || catalog.Warnings[0].Code != warning.WarnForeignPermissionInert {
		t.Fatalf("warnings = %#v", catalog.Warnings)
	}
}

func TestScanClaudePluginCompatibilityAndInertMCP(t *testing.T) {
	home := t.TempDir()
	workingDir := t.TempDir()
	pluginsDir := filepath.Join(home, ".claude", "plugins")
	current := filepath.Join(pluginsDir, "cache", "market", "demo", "2.0.0")
	cached := filepath.Join(pluginsDir, "cache", "market", "cached", "1.0.0")
	escape := filepath.Join(pluginsDir, "cache", "market", "escape", "1.0.0")
	for _, item := range []struct {
		root, name, body string
	}{
		{root: current, name: "current", body: "Current"},
		{root: cached, name: "cached", body: "Cached"},
	} {
		directory := filepath.Join(item.root, "skills", item.name)
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		source := "---\nname: " + item.name + "\ndescription: " + item.body + "\n---\n" + item.body + "\n"
		if err := os.WriteFile(filepath.Join(directory, "SKILL.md"), []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(escape, ".claude-plugin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(escape, ".claude-plugin", "plugin.json"), []byte(`{"name":"escape","skills":"../outside"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(current, ".mcp.json"), []byte(`{"mcpServers":{"secret":{"command":"server","env":{"TOKEN":"TOPSECRET"}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(pluginsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	index := map[string]any{
		"version": 2,
		"plugins": map[string]any{
			"cached@market": []map[string]any{{"scope": "user", "installPath": cached, "version": "1.0.0", "lastUpdated": "2026-01-01"}},
			"demo@market":   []map[string]any{{"scope": "user", "installPath": current, "version": "2.0.0", "lastUpdated": "2026-02-01"}},
			"escape@market": []map[string]any{{"scope": "user", "installPath": escape, "version": "1.0.0", "lastUpdated": "2026-01-01"}},
		},
	}
	writeJSON(t, filepath.Join(pluginsDir, "installed_plugins.json"), index)
	writeJSON(t, filepath.Join(home, ".claude", "settings.json"), map[string]any{"enabledPlugins": map[string]bool{"demo@market": true, "escape@market": true}})

	catalog, err := ScanClaude(context.Background(), Options{HomeDir: home, WorkingDir: workingDir, RepositoryRoot: workingDir})
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Skills) != 2 || catalog.Skills[0].Name != "cached" || catalog.Skills[0].Provenance[0].Enabled || catalog.Skills[1].Name != "current" || !catalog.Skills[1].Provenance[0].Enabled {
		t.Fatalf("skills = %#v", catalog.Skills)
	}
	if len(catalog.MCPServers) != 1 || catalog.MCPServers[0].Name != "secret" || !catalog.MCPServers[0].Inert {
		t.Fatalf("MCP servers = %#v", catalog.MCPServers)
	}
	encoded, err := json.Marshal(catalog)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) == "" || strings.Contains(string(encoded), "TOPSECRET") {
		t.Fatalf("catalog leaked secret: %s", encoded)
	}
	if !hasWarning(catalog.Warnings, warning.WarnForeignPathEscape) || !hasWarning(catalog.Warnings, warning.WarnForeignMCPInert) {
		t.Fatalf("warnings = %#v", catalog.Warnings)
	}
}

func TestScanCodexUsesIndependentRootsPluginsAndMCP(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	codexHome := filepath.Join(home, ".codex")
	writeSkill(t, filepath.Join(root, ".agents", "skills", "shared"), "shared", "Project")
	writeSkill(t, filepath.Join(home, ".agents", "skills", "shared"), "shared", "User")
	writeSkill(t, filepath.Join(home, ".agents", "skills", "user"), "user", "User only")
	writeSkill(t, filepath.Join(codexHome, "skills", "compat"), "compat", "Compatibility")
	if err := os.MkdirAll(filepath.Join(codexHome, "prompts"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexHome, "prompts", "legacy.md"), []byte("Legacy $ARGUMENTS"), 0o600); err != nil {
		t.Fatal(err)
	}

	pluginRoot := filepath.Join(root, "marketplace", "plugins", "demo")
	writeSkill(t, filepath.Join(pluginRoot, "skills", "plug"), "plug", "Plugin")
	writeJSON(t, filepath.Join(pluginRoot, ".codex-plugin", "plugin.json"), map[string]any{
		"name": "demo", "version": "1.2.3", "skills": "./skills", "mcpServers": "./mcp.json",
	})
	writeJSON(t, filepath.Join(pluginRoot, "mcp.json"), map[string]any{"mcp_servers": map[string]any{"plugin-server": map[string]any{"command": "server"}}})
	writeJSON(t, filepath.Join(root, ".agents", "plugins", "marketplace.json"), map[string]any{
		"name": "market", "plugins": []any{map[string]any{"name": "demo", "source": map[string]any{"source": "local", "path": "./marketplace/plugins/demo"}}},
	})
	configText := "[mcp_servers.user-server]\ncommand = \"server\"\n[plugins.\"demo@market\"]\nenabled = true\n"
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".codex", "config.toml"), []byte("[mcp_servers.project-server]\nurl = \"https://example.invalid\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	catalog, err := ScanCodex(context.Background(), Options{HomeDir: home, CodexHome: codexHome, WorkingDir: root, RepositoryRoot: root, ProjectTrusted: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Skills) != 4 || skillDescription(catalog.Skills, "shared") != "Project" || skillClassification(catalog.Skills, "compat") != ClassificationCompatibility || skillEnabled(catalog.Skills, "demo@market:plug") != true {
		t.Fatalf("skills = %#v", catalog.Skills)
	}
	if len(catalog.Templates) != 1 || catalog.Templates[0].Name != "legacy" || catalog.Templates[0].Provenance[0].Classification != ClassificationCompatibility {
		t.Fatalf("templates = %#v", catalog.Templates)
	}
	if len(catalog.MCPServers) != 3 || !hasMCP(catalog.MCPServers, "plugin-server") || !hasMCP(catalog.MCPServers, "project-server") || !hasMCP(catalog.MCPServers, "user-server") {
		t.Fatalf("MCP servers = %#v", catalog.MCPServers)
	}
}

func TestScanCopilotUsesDocumentedPrecedencePluginsAndMCP(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	nested := filepath.Join(root, "nested")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, filepath.Join(root, ".github", "skills", "shared"), "shared", "GitHub project")
	writeSkill(t, filepath.Join(root, ".agents", "skills", "shared"), "shared", "Agents project")
	writeSkill(t, filepath.Join(home, ".copilot", "skills", "personal"), "personal", "Personal")
	if err := os.MkdirAll(filepath.Join(root, ".claude", "commands"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".claude", "commands", "legacy.md"), []byte("Legacy"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".github", "prompts"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".github", "prompts", "preview.prompt.md"), []byte("Preview"), 0o600); err != nil {
		t.Fatal(err)
	}

	marketPlugin := filepath.Join(home, ".copilot", "installed-plugins", "market", "demo")
	directPlugin := filepath.Join(home, ".copilot", "installed-plugins", "_direct", "source-id")
	writeSkill(t, filepath.Join(marketPlugin, "skills", "market-skill"), "market-skill", "Market")
	writeSkill(t, filepath.Join(directPlugin, "custom-skills", "direct-skill"), "direct-skill", "Direct")
	writeJSON(t, filepath.Join(marketPlugin, ".plugin", "plugin.json"), map[string]any{"name": "first", "version": "2", "skills": "skills", "mcpServers": map[string]any{"market-server": map[string]any{"command": "server", "env": map[string]string{"TOKEN": "SECRET"}}}})
	writeJSON(t, filepath.Join(marketPlugin, "plugin.json"), map[string]any{"name": "wrong", "skills": "missing"})
	writeJSON(t, filepath.Join(directPlugin, "plugin.json"), map[string]any{"name": "direct", "skills": "custom-skills"})
	writeJSON(t, filepath.Join(home, ".copilot", "mcp-config.json"), map[string]any{"mcpServers": map[string]any{"user": map[string]any{"command": "server"}}})
	writeJSON(t, filepath.Join(root, ".github", "mcp.json"), map[string]any{"github": map[string]any{"command": "server"}})
	writeJSON(t, filepath.Join(root, ".mcp.json"), map[string]any{"project": map[string]any{"url": "https://example.invalid"}})

	catalog, err := ScanCopilot(context.Background(), Options{HomeDir: home, WorkingDir: nested, RepositoryRoot: root, ProjectTrusted: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Skills) != 4 || skillDescription(catalog.Skills, "shared") != "GitHub project" || skillDescription(catalog.Skills, "first:market-skill") != "Market" || skillDescription(catalog.Skills, "direct:direct-skill") != "Direct" {
		t.Fatalf("skills = %#v", catalog.Skills)
	}
	if len(catalog.Templates) != 1 || catalog.Templates[0].Name != "legacy" {
		t.Fatalf("templates = %#v", catalog.Templates)
	}
	if !hasWarning(catalog.Warnings, warning.WarnForeignEcosystemDisabled) {
		t.Fatalf("missing disabled preview warning: %#v", catalog.Warnings)
	}
	if len(catalog.MCPServers) != 4 || !hasMCP(catalog.MCPServers, "user") || !hasMCP(catalog.MCPServers, "github") || !hasMCP(catalog.MCPServers, "project") || !hasMCP(catalog.MCPServers, "market-server") {
		t.Fatalf("MCP servers = %#v", catalog.MCPServers)
	}
	encoded, err := json.Marshal(catalog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "SECRET") {
		t.Fatalf("catalog leaked secret: %s", encoded)
	}
}

func TestScanKeepsHostCatalogsIndependentAndWarnsOnAmbiguity(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	writeSkill(t, filepath.Join(root, ".claude", "skills", "review"), "review", "Claude")
	writeSkill(t, filepath.Join(root, ".agents", "skills", "review"), "review", "Codex")
	writeSkill(t, filepath.Join(root, ".github", "skills", "review"), "review", "Copilot")

	catalog, err := Scan(context.Background(), Options{HomeDir: home, WorkingDir: root, RepositoryRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Hosts) != 3 || catalog.Hosts[0].Host != HostClaude || catalog.Hosts[1].Host != HostCodex || catalog.Hosts[2].Host != HostCopilot {
		t.Fatalf("hosts = %#v", catalog.Hosts)
	}
	for _, host := range catalog.Hosts {
		if len(host.Skills) != 1 || host.Skills[0].Name != "review" {
			t.Fatalf("%s skills = %#v", host.Host, host.Skills)
		}
	}
	if len(catalog.Warnings) != 1 || catalog.Warnings[0].Code != warning.WarnForeignAmbiguousName || catalog.Warnings[0].Path != "review" {
		t.Fatalf("warnings = %#v", catalog.Warnings)
	}
}

func TestScanCancellationAndEntryBound(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	writeSkill(t, filepath.Join(root, ".claude", "skills", "a"), "a", "A")
	writeSkill(t, filepath.Join(root, ".claude", "skills", "b"), "b", "B")

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ScanClaude(cancelled, Options{HomeDir: home, WorkingDir: root, RepositoryRoot: root}); err != context.Canceled {
		t.Fatalf("cancelled error = %v", err)
	}
	catalog, err := ScanClaude(context.Background(), Options{HomeDir: home, WorkingDir: root, RepositoryRoot: root, MaxEntries: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Skills) != 1 || !hasWarning(catalog.Warnings, warning.WarnForeignScanTruncated) {
		t.Fatalf("bounded catalog = %#v", catalog)
	}
}

func TestSkillDedupPreservesSourceProvenance(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, filepath.Join(root, ".claude", "skills", "shared"), "shared", "Same source")
	catalog, err := ScanClaude(context.Background(), Options{HomeDir: root, WorkingDir: root, RepositoryRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Skills) != 1 || len(catalog.Skills[0].Provenance) != 2 {
		t.Fatalf("skills = %#v", catalog.Skills)
	}
	if catalog.Skills[0].Provenance[0].Scope != ScopeProject || catalog.Skills[0].Provenance[1].Scope != ScopeUser {
		t.Fatalf("provenance = %#v", catalog.Skills[0].Provenance)
	}
}

func TestMalformedMCPEntryWarnsOnceAndDoesNotLeakIntoCatalog(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	writeJSON(t, filepath.Join(root, ".mcp.json"), map[string]any{"mcpServers": map[string]any{"broken": map[string]any{"env": map[string]string{"TOKEN": "SECRET"}}}})
	catalog, err := ScanClaude(context.Background(), Options{HomeDir: home, WorkingDir: root, RepositoryRoot: root, ProjectTrusted: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.MCPServers) != 0 || len(catalog.Warnings) != 1 || catalog.Warnings[0].Code != warning.WarnForeignEntryShapeUnknown {
		t.Fatalf("catalog = %#v", catalog)
	}
}

func TestCopilotPluginMCPUsesDocumentedLastWinsOrder(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	for _, plugin := range []string{"a", "b"} {
		pluginRoot := filepath.Join(home, ".copilot", "installed-plugins", "market", plugin)
		writeJSON(t, filepath.Join(pluginRoot, "plugin.json"), map[string]any{
			"name": plugin, "mcpServers": map[string]any{"shared": map[string]any{"command": "server"}},
		})
	}
	catalog, err := ScanCopilot(context.Background(), Options{HomeDir: home, WorkingDir: root, RepositoryRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.MCPServers) != 1 || catalog.MCPServers[0].QualifiedName != "b:shared" {
		t.Fatalf("MCP servers = %#v", catalog.MCPServers)
	}
	if !hasWarning(catalog.Warnings, warning.WarnForeignDuplicateMCPServer) {
		t.Fatalf("warnings = %#v", catalog.Warnings)
	}
}

func TestClaudeMCPPrecedenceIsLocalProjectUser(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	writeJSON(t, filepath.Join(home, ".claude.json"), map[string]any{
		"mcpServers": map[string]any{"shared": map[string]any{"command": "user"}},
		"projects":   map[string]any{root: map[string]any{"mcpServers": map[string]any{"shared": map[string]any{"command": "local"}}}},
	})
	writeJSON(t, filepath.Join(root, ".mcp.json"), map[string]any{"mcpServers": map[string]any{"shared": map[string]any{"command": "project"}}})
	catalog, err := ScanClaude(context.Background(), Options{HomeDir: home, WorkingDir: root, RepositoryRoot: root, ProjectTrusted: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.MCPServers) != 1 || catalog.MCPServers[0].Provenance[0].Scope != ScopeLocal {
		t.Fatalf("MCP servers = %#v", catalog.MCPServers)
	}
	duplicates := 0
	for _, item := range catalog.Warnings {
		if item.Code == warning.WarnForeignDuplicateMCPServer {
			duplicates++
		}
	}
	if duplicates != 2 {
		t.Fatalf("warnings = %#v", catalog.Warnings)
	}
}

func TestScanRejectsSymlinkedSourceRootEscape(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	outside := t.TempDir()
	writeSkill(t, filepath.Join(outside, "skills", "escape"), "escape", "Outside")
	if err := os.Symlink(outside, filepath.Join(root, ".claude")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	catalog, err := ScanClaude(context.Background(), Options{HomeDir: home, WorkingDir: root, RepositoryRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Skills) != 0 || !hasWarning(catalog.Warnings, warning.WarnForeignPathEscape) {
		t.Fatalf("catalog = %#v", catalog)
	}
}

func TestIdenticalPluginSkillsKeepDistinctQualifiedIdentities(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	pluginsRoot := filepath.Join(home, ".claude", "plugins")
	installs := map[string][]map[string]any{}
	for _, identifier := range []string{"a@market", "b@market"} {
		pluginRoot := filepath.Join(pluginsRoot, "cache", strings.TrimSuffix(identifier, "@market"))
		writeSkill(t, filepath.Join(pluginRoot, "skills", "same"), "same", "Identical")
		installs[identifier] = []map[string]any{{"scope": "user", "installPath": pluginRoot, "version": "1"}}
	}
	writeJSON(t, filepath.Join(pluginsRoot, "installed_plugins.json"), map[string]any{"version": 2, "plugins": installs})
	catalog, err := ScanClaude(context.Background(), Options{HomeDir: home, WorkingDir: root, RepositoryRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Skills) != 2 || skillDescription(catalog.Skills, "a@market:same") != "Identical" || skillDescription(catalog.Skills, "b@market:same") != "Identical" {
		t.Fatalf("skills = %#v", catalog.Skills)
	}
}

func TestClaudePluginFreshnessBreaksTiesWithinScope(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	pluginsRoot := filepath.Join(home, ".claude", "plugins")
	stale := filepath.Join(pluginsRoot, "cache", "a-stale")
	current := filepath.Join(pluginsRoot, "cache", "z-current")
	writeSkill(t, filepath.Join(stale, "skills", "demo"), "demo", "Stale")
	writeSkill(t, filepath.Join(current, "skills", "demo"), "demo", "Current")
	writeJSON(t, filepath.Join(pluginsRoot, "installed_plugins.json"), map[string]any{
		"version": 2,
		"plugins": map[string]any{
			"demo@market": []map[string]any{
				{"scope": "user", "installPath": stale, "version": "1.0.0", "lastUpdated": "2026-01-01T00:00:00Z"},
				{"scope": "user", "installPath": current, "version": "2.0.0", "lastUpdated": "2026-02-01T00:00:00Z"},
			},
		},
	})
	catalog, err := ScanClaude(context.Background(), Options{HomeDir: home, WorkingDir: root, RepositoryRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if got := skillDescription(catalog.Skills, "demo@market:demo"); got != "Current" {
		t.Fatalf("description = %q, want Current", got)
	}
}

func TestClaudePluginVersionBreaksSameScopeFreshnessTies(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	pluginsRoot := filepath.Join(home, ".claude", "plugins")
	stale := filepath.Join(pluginsRoot, "cache", "a-stale")
	current := filepath.Join(pluginsRoot, "cache", "z-current")
	writeSkill(t, filepath.Join(stale, "skills", "demo"), "demo", "Stale")
	writeSkill(t, filepath.Join(current, "skills", "demo"), "demo", "Current")
	writeJSON(t, filepath.Join(pluginsRoot, "installed_plugins.json"), map[string]any{
		"version": 2,
		"plugins": map[string]any{
			"demo@market": []map[string]any{
				{"scope": "user", "installPath": stale, "version": "1.9"},
				{"scope": "user", "installPath": current, "version": "1.10"},
			},
		},
	})
	catalog, err := ScanClaude(context.Background(), Options{HomeDir: home, WorkingDir: root, RepositoryRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if got := skillDescription(catalog.Skills, "demo@market:demo"); got != "Current" {
		t.Fatalf("description = %q, want Current", got)
	}
}

func TestClaudePluginVersionOrderingIsTotalAcrossInvalidCohorts(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		versions []string
		want     string
	}{
		{name: "one invalid two", versions: []string{"1", "bad", "2"}, want: "2"},
		{name: "two invalid ten", versions: []string{"2", "bad", "10"}, want: "10"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			home := t.TempDir()
			root := t.TempDir()
			pluginsRoot := filepath.Join(home, ".claude", "plugins")
			paths := []string{
				filepath.Join(pluginsRoot, "cache", "b-first-valid"),
				filepath.Join(pluginsRoot, "cache", "a-invalid"),
				filepath.Join(pluginsRoot, "cache", "c-current"),
			}
			entries := make([]map[string]any, 0, len(paths))
			for index, path := range paths {
				writeSkill(t, filepath.Join(path, "skills", "demo"), "demo", testCase.versions[index])
				entries = append(entries, map[string]any{"scope": "user", "installPath": path, "version": testCase.versions[index]})
			}
			writeJSON(t, filepath.Join(pluginsRoot, "installed_plugins.json"), map[string]any{
				"version": 2,
				"plugins": map[string]any{"demo@market": entries},
			})
			catalog, err := ScanClaude(context.Background(), Options{HomeDir: home, WorkingDir: root, RepositoryRoot: root})
			if err != nil {
				t.Fatal(err)
			}
			if got := skillDescription(catalog.Skills, "demo@market:demo"); got != testCase.want {
				t.Fatalf("description = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestExplicitStdioMCPDoesNotInferHTTPFromURL(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	writeJSON(t, filepath.Join(root, ".mcp.json"), map[string]any{"mcpServers": map[string]any{"broken": map[string]any{"type": "stdio", "url": "https://example.invalid"}}})
	catalog, err := ScanClaude(context.Background(), Options{HomeDir: home, WorkingDir: root, RepositoryRoot: root, ProjectTrusted: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.MCPServers) != 0 || !hasWarning(catalog.Warnings, warning.WarnForeignEntryShapeUnknown) {
		t.Fatalf("catalog = %#v", catalog)
	}
}

func TestClaudeUntrustedProjectMCPIsSkipped(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	writeJSON(t, filepath.Join(root, ".mcp.json"), map[string]any{"mcpServers": map[string]any{"project": map[string]any{"command": "server"}}})
	catalog, err := ScanClaude(context.Background(), Options{HomeDir: home, WorkingDir: root, RepositoryRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.MCPServers) != 0 || !hasWarning(catalog.Warnings, warning.WarnForeignProjectUntrusted) {
		t.Fatalf("catalog = %#v", catalog)
	}
}

func TestCombinedCatalogMergesSharedPortableSkillProvenance(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	writeSkill(t, filepath.Join(root, ".agents", "skills", "shared"), "shared", "Shared")
	catalog, err := Scan(context.Background(), Options{HomeDir: home, WorkingDir: root, RepositoryRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Skills) != 1 || len(catalog.Skills[0].Provenance) != 2 {
		t.Fatalf("skills = %#v", catalog.Skills)
	}
	if catalog.Skills[0].Provenance[0].Host != HostCodex || catalog.Skills[0].Provenance[1].Host != HostCopilot || hasWarning(catalog.Warnings, warning.WarnForeignAmbiguousName) {
		t.Fatalf("catalog = %#v", catalog)
	}
}

func TestCodexPreservesDisabledMCPAndPluginState(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	codexHome := filepath.Join(home, ".codex")
	writeJSON(t, filepath.Join(root, ".agents", "plugins", "marketplace.json"), map[string]any{
		"name": "local", "plugins": []any{map[string]any{"name": "demo", "source": map[string]any{"source": "local", "path": "./plugin"}}},
	})
	writeJSON(t, filepath.Join(root, "plugin", ".codex-plugin", "plugin.json"), map[string]any{"name": "demo", "skills": "./skills"})
	writeSkill(t, filepath.Join(root, "plugin", "skills", "demo"), "demo", "Demo")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	config := "[plugins.\"demo@local\"]\nenabled = false\n[mcp_servers.disabled]\nenabled = false\ncommand = \"server\"\n"
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog, err := ScanCodex(context.Background(), Options{HomeDir: home, CodexHome: codexHome, WorkingDir: root, RepositoryRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if skillEnabled(catalog.Skills, "demo@local:demo") || len(catalog.MCPServers) != 1 || catalog.MCPServers[0].Provenance[0].Enabled {
		t.Fatalf("catalog = %#v", catalog)
	}
}

func TestCodexRejectsNonBooleanEnabledState(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	codexHome := filepath.Join(home, ".codex")
	writeJSON(t, filepath.Join(root, ".agents", "plugins", "marketplace.json"), map[string]any{
		"name": "local", "plugins": []any{map[string]any{"name": "demo", "source": map[string]any{"source": "local", "path": "./plugin"}}},
	})
	writeJSON(t, filepath.Join(root, "plugin", ".codex-plugin", "plugin.json"), map[string]any{"name": "demo", "skills": "./skills"})
	writeSkill(t, filepath.Join(root, "plugin", "skills", "demo"), "demo", "Demo")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	config := "[plugins.\"demo@local\"]\nenabled = \"false\"\n[mcp_servers.invalid]\nenabled = 0\ncommand = \"server\"\n"
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog, err := ScanCodex(context.Background(), Options{HomeDir: home, CodexHome: codexHome, WorkingDir: root, RepositoryRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if skillEnabled(catalog.Skills, "demo@local:demo") || len(catalog.MCPServers) != 0 {
		t.Fatalf("catalog = %#v", catalog)
	}
	if got := warningCount(catalog.Warnings, warning.WarnForeignTOMLUnsupported); got != 2 {
		t.Fatalf("TOML warnings = %d, want 2: %#v", got, catalog.Warnings)
	}
}

func writeSkill(t *testing.T, directory, name, description string) {
	t.Helper()
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	source := "---\nname: " + name + "\ndescription: " + description + "\n---\n" + description + "\n"
	if err := os.WriteFile(filepath.Join(directory, "SKILL.md"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
}

func skillDescription(skills []Skill, qualifiedName string) string {
	for _, skill := range skills {
		if skill.QualifiedName == qualifiedName {
			return skill.Description
		}
	}
	return ""
}

func skillClassification(skills []Skill, qualifiedName string) Classification {
	for _, skill := range skills {
		if skill.QualifiedName == qualifiedName && len(skill.Provenance) != 0 {
			return skill.Provenance[0].Classification
		}
	}
	return ""
}

func skillEnabled(skills []Skill, qualifiedName string) bool {
	for _, skill := range skills {
		if skill.QualifiedName == qualifiedName && len(skill.Provenance) != 0 {
			return skill.Provenance[0].Enabled
		}
	}
	return false
}

func hasMCP(servers []MCPServer, name string) bool {
	for _, server := range servers {
		if server.Name == name {
			return true
		}
	}
	return false
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func hasWarning(values []warning.Warning, code string) bool {
	for _, item := range values {
		if item.Code == code {
			return true
		}
	}
	return false
}

func warningCount(values []warning.Warning, code string) int {
	count := 0
	for _, value := range values {
		if value.Code == code {
			count++
		}
	}
	return count
}
