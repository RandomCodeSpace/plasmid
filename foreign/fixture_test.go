package foreign

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/RandomCodeSpace/plasmid/internal/fixture"
)

const foreignFixtureRunner = "foreign/scan"

func init() {
	fixture.RegisterRunner("foreign", "foreign/scan", "scan")
}

func TestMain(m *testing.M) {
	os.Exit(fixture.Run(m))
}

type foreignFixtureCase struct {
	Host           Host `json:"host"`
	ProjectTrusted bool `json:"project_trusted"`
	Preview        bool `json:"preview"`
}

type foreignFixtureOutput struct {
	Host       Host                     `json:"host"`
	Skills     []foreignFixtureSkill    `json:"skills"`
	Templates  []foreignFixtureTemplate `json:"templates"`
	MCPServers []foreignFixtureMCP      `json:"mcp_servers"`
	Warnings   []string                 `json:"warnings"`
}

type foreignFixtureSkill struct {
	Name          string                     `json:"name"`
	QualifiedName string                     `json:"qualified_name"`
	Description   string                     `json:"description"`
	Provenance    []foreignFixtureProvenance `json:"provenance"`
}

type foreignFixtureTemplate struct {
	Name           string         `json:"name"`
	Classification Classification `json:"classification"`
}

type foreignFixtureMCP struct {
	Enabled   bool   `json:"enabled"`
	Name      string `json:"name"`
	Transport string `json:"transport"`
	Inert     bool   `json:"inert"`
}

type foreignFixtureProvenance struct {
	Scope          Scope          `json:"scope"`
	PluginID       string         `json:"plugin_id"`
	PluginVersion  string         `json:"plugin_version"`
	Enabled        bool           `json:"enabled"`
	Trust          Trust          `json:"trust"`
	Classification Classification `json:"classification"`
}

func TestForeignFixtures(t *testing.T) {
	fixture.WalkKinds(t, "foreign", foreignFixtureRunner, []string{"scan"}, func(t *testing.T, testCase fixture.Case) {
		var spec foreignFixtureCase
		testCase.Decode(t, "case.json", &spec)
		input := filepath.Join(t.TempDir(), "input")
		copyForeignFixtureInput(t, filepath.Join(testCase.Dir, "input"), input)
		home := filepath.Join(input, "home")
		work := filepath.Join(input, "work")
		rewriteForeignFixturePaths(t, input, map[string]string{"$HOME": filepath.ToSlash(home), "$ROOT": filepath.ToSlash(work)})
		options := Options{
			HomeDir: home, WorkingDir: work, RepositoryRoot: work,
			CodexHome: filepath.Join(home, ".codex"), AdminSkillsDir: filepath.Join(input, "admin"),
			ProjectTrusted: spec.ProjectTrusted, EnableCopilotPreview: spec.Preview,
		}
		var catalog HostCatalog
		var err error
		switch spec.Host {
		case Host("all"):
			combined, scanErr := Scan(context.Background(), options)
			if scanErr != nil {
				t.Fatal(scanErr)
			}
			testCase.CompareJSON(t, "expected.json", projectCombinedFixture(combined), fixture.Paths{}, fixture.GoldenReadOnly)
			return
		case HostClaude:
			catalog, err = ScanClaude(context.Background(), options)
		case HostCodex:
			catalog, err = ScanCodex(context.Background(), options)
		case HostCopilot:
			catalog, err = ScanCopilot(context.Background(), options)
		default:
			t.Fatalf("unknown host %q", spec.Host)
		}
		if err != nil {
			t.Fatal(err)
		}
		testCase.CompareJSON(t, "expected.json", projectForeignFixture(catalog), fixture.Paths{}, fixture.GoldenReadOnly)
	})
	fixture.AssertCoverage(t, "foreign")
}

type foreignCombinedOutput struct {
	Hosts    []foreignCombinedHost  `json:"hosts"`
	Skills   []foreignCombinedSkill `json:"skills"`
	Warnings []string               `json:"warnings"`
}

type foreignCombinedHost struct {
	Host   Host     `json:"host"`
	Skills []string `json:"skills"`
}

type foreignCombinedSkill struct {
	Hosts []Host `json:"hosts"`
	Name  string `json:"name"`
}

func projectCombinedFixture(catalog Catalog) foreignCombinedOutput {
	output := foreignCombinedOutput{Hosts: []foreignCombinedHost{}, Skills: []foreignCombinedSkill{}, Warnings: []string{}}
	for _, host := range catalog.Hosts {
		item := foreignCombinedHost{Host: host.Host, Skills: []string{}}
		for _, skill := range host.Skills {
			item.Skills = append(item.Skills, skill.Name)
		}
		output.Hosts = append(output.Hosts, item)
	}
	for _, skill := range catalog.Skills {
		hosts := []Host{}
		seen := make(map[Host]bool)
		for _, provenance := range skill.Provenance {
			if !seen[provenance.Host] {
				hosts = append(hosts, provenance.Host)
				seen[provenance.Host] = true
			}
		}
		output.Skills = append(output.Skills, foreignCombinedSkill{Hosts: hosts, Name: skill.Name})
	}
	for _, item := range catalog.Warnings {
		output.Warnings = append(output.Warnings, item.Code)
	}
	return output
}

func copyForeignFixtureInput(t *testing.T, source, destination string) {
	t.Helper()
	if err := filepath.WalkDir(source, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600)
	}); err != nil {
		t.Fatal(err)
	}
}

func rewriteForeignFixturePaths(t *testing.T, root string, replacements map[string]string) {
	t.Helper()
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for old, replacement := range replacements {
			data = bytes.ReplaceAll(data, []byte(old), []byte(replacement))
		}
		return os.WriteFile(path, data, 0o600)
	}); err != nil {
		t.Fatal(err)
	}
}

func projectForeignFixture(catalog HostCatalog) foreignFixtureOutput {
	output := foreignFixtureOutput{Host: catalog.Host, Skills: []foreignFixtureSkill{}, Templates: []foreignFixtureTemplate{}, MCPServers: []foreignFixtureMCP{}, Warnings: []string{}}
	for _, skill := range catalog.Skills {
		item := foreignFixtureSkill{Name: skill.Name, QualifiedName: skill.QualifiedName, Description: skill.Description, Provenance: []foreignFixtureProvenance{}}
		for _, provenance := range skill.Provenance {
			item.Provenance = append(item.Provenance, foreignFixtureProvenance{
				Scope: provenance.Scope, PluginID: provenance.PluginID, PluginVersion: provenance.PluginVersion,
				Enabled: provenance.Enabled, Trust: provenance.Trust, Classification: provenance.Classification,
			})
		}
		output.Skills = append(output.Skills, item)
	}
	for _, template := range catalog.Templates {
		output.Templates = append(output.Templates, foreignFixtureTemplate{Name: template.Name, Classification: template.Provenance[0].Classification})
	}
	for _, server := range catalog.MCPServers {
		output.MCPServers = append(output.MCPServers, foreignFixtureMCP{Enabled: server.Provenance[0].Enabled, Name: server.Name, Transport: server.Transport, Inert: server.Inert})
	}
	for _, item := range catalog.Warnings {
		output.Warnings = append(output.Warnings, item.Code)
	}
	sort.Strings(output.Warnings)
	return output
}
