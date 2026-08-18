package extensions

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plasmid-dev/plasmid/foreign"
	"github.com/plasmid-dev/plasmid/internal/fixture"
	"github.com/plasmid-dev/plasmid/workspace"
)

func init() {
	fixture.RegisterRunner("extensions", "extensions/catalog", "catalog", "template")
}

func TestMain(m *testing.M) { os.Exit(fixture.Run(m)) }

type extensionFixtureCase struct {
	Name       string   `json:"name"`
	SecondName string   `json:"second_name"`
	Body       string   `json:"body"`
	Globs      []string `json:"globs"`
	Touches    []string `json:"touches"`
	Resource   struct {
		Configured string `json:"configured"`
		Foreign    string `json:"foreign"`
	} `json:"resource"`
}

type extensionFixtureOutput struct {
	Skills                   []extensionFixtureSkill `json:"skills"`
	Templates                []string                `json:"templates"`
	Body                     string                  `json:"body"`
	Restricted               bool                    `json:"restricted"`
	UnqualifiedResourceError string                  `json:"unqualified_resource_error"`
	Resources                map[string]string       `json:"resources"`
	SkillStates              [][]string              `json:"skill_states,omitempty"`
}

type extensionFixtureSkill struct {
	Name            string   `json:"name"`
	QualifiedNames  []string `json:"qualified_names"`
	ProvenanceCount int      `json:"provenance_count"`
}

func TestExtensionFixtures(t *testing.T) {
	fixture.WalkKinds(t, "extensions", "extensions/catalog", []string{"catalog", "template"}, func(t *testing.T, testCase fixture.Case) {
		var spec extensionFixtureCase
		testCase.Decode(t, "case.json", &spec)
		testCase.Decode(t, "input.json", &spec)
		root := t.TempDir()
		home := t.TempDir()
		output := extensionFixtureOutput{Skills: []extensionFixtureSkill{}, Templates: []string{}, Resources: map[string]string{}}
		var options Options
		switch testCase.ID {
		case "portable-dedup-resources":
			configuredRoot := filepath.Join(root, "configured")
			foreignRoot := filepath.Join(root, ".agents", "skills")
			writeFixtureSkill(t, configuredRoot, spec.Name, spec.Body, spec.Resource.Configured)
			writeFixtureSkill(t, foreignRoot, spec.Name, spec.Body, spec.Resource.Foreign)
			options = Options{
				WorkingDir: root, HomeDir: home, SkillRoots: []string{configuredRoot}, Codex: true,
				Foreign: foreign.Options{HomeDir: home, WorkingDir: root, RepositoryRoot: root, ProjectTrusted: true},
			}
		case "template-filename-mode":
			codexHome := filepath.Join(home, ".codex")
			if err := os.MkdirAll(filepath.Join(codexHome, "prompts"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(codexHome, "prompts", spec.Name+".md"), []byte(spec.Body), 0o600); err != nil {
				t.Fatal(err)
			}
			options = Options{
				WorkingDir: root, HomeDir: home, Codex: true,
				Foreign: foreign.Options{HomeDir: home, CodexHome: codexHome, WorkingDir: root, RepositoryRoot: root},
			}
		case "skill-glob-activation":
			configuredRoot := filepath.Join(root, "configured")
			writeFixtureSkillWithGlobs(t, configuredRoot, spec.Name, spec.Body, spec.Globs)
			options = Options{WorkingDir: root, SkillRoots: []string{configuredRoot}, Foreign: foreign.Options{ProjectTrusted: true}}
		case "session-refresh":
			configuredRoot := filepath.Join(root, "configured")
			writeFixtureSkill(t, configuredRoot, spec.Name, spec.Body, "")
			options = Options{WorkingDir: root, SkillRoots: []string{configuredRoot}, Foreign: foreign.Options{ProjectTrusted: true}}
		default:
			t.Fatalf("unknown fixture %q", testCase.ID)
		}
		store, err := NewStore(options)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.StartSession(t.Context(), "session"); err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		catalog, _ := store.Snapshot("session")
		for _, skill := range catalog.AllSkills() {
			output.Skills = append(output.Skills, extensionFixtureSkill{Name: skill.Name, QualifiedNames: skill.QualifiedNames, ProvenanceCount: len(skill.Provenance)})
		}
		for _, template := range catalog.Templates() {
			output.Templates = append(output.Templates, template.Name)
		}
		if testCase.ID == "portable-dedup-resources" {
			if _, err := catalog.LoadSkillResource(t.Context(), spec.Name, "guide.md", true); errors.Is(err, ErrAmbiguous) {
				output.UnqualifiedResourceError = "ambiguous"
			}
			output.Resources["configured"], _ = catalog.LoadSkillResource(t.Context(), "plasmid:configured:"+spec.Name, "guide.md", true)
			output.Resources["foreign"], _ = catalog.LoadSkillResource(t.Context(), "codex:project:"+spec.Name, "guide.md", true)
		} else if testCase.ID == "template-filename-mode" {
			loaded, err := catalog.LoadTemplate(t.Context(), spec.Name, false)
			if err != nil {
				t.Fatal(err)
			}
			output.Body = loaded.Body
			output.Restricted = loaded.Restricted
		} else if testCase.ID == "skill-glob-activation" {
			output.SkillStates = append(output.SkillStates, fixtureSkillNames(catalog.Skills()))
			for _, path := range spec.Touches {
				store.ObserveTouch(t.Context(), workspace.Touch{SessionID: "session", Path: path, Kind: workspace.TouchRead})
				catalog, _ = store.Snapshot("session")
				output.SkillStates = append(output.SkillStates, fixtureSkillNames(catalog.Skills()))
			}
		} else if testCase.ID == "session-refresh" {
			output.SkillStates = append(output.SkillStates, fixtureSkillNames(catalog.Skills()))
			writeFixtureSkill(t, options.SkillRoots[0], spec.SecondName, spec.Body, "")
			store.DropSession("session")
			if err := store.StartSession(t.Context(), "session"); err != nil {
				t.Fatal(err)
			}
			catalog, _ = store.Snapshot("session")
			output.SkillStates = append(output.SkillStates, fixtureSkillNames(catalog.Skills()))
		}
		testCase.CompareJSON(t, "expected.json", output, fixture.Paths{}, fixture.GoldenReadOnly)
	})
	fixture.AssertCoverage(t, "extensions")
}

func writeFixtureSkillWithGlobs(t *testing.T, root, name, body string, globs []string) {
	t.Helper()
	directory := filepath.Join(root, name)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	source := "---\nname: " + name + "\ndescription: fixture\nglobs: [" + strings.Join(globs, ", ") + "]\n---\n" + body
	if err := os.WriteFile(filepath.Join(directory, "SKILL.md"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
}

func fixtureSkillNames(values []Skill) []string {
	result := make([]string, len(values))
	for index, value := range values {
		result[index] = value.Name
	}
	return result
}

func writeFixtureSkill(t *testing.T, root, name, body, resource string) {
	t.Helper()
	directory := filepath.Join(root, name)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	source := "---\nname: " + name + "\ndescription: fixture\n---\n" + body
	if err := os.WriteFile(filepath.Join(directory, "SKILL.md"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "guide.md"), []byte(resource), 0o600); err != nil {
		t.Fatal(err)
	}
}
