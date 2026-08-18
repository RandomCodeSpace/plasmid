package contextresolver

import (
	"os"
	"testing"
	"time"

	"github.com/plasmid-dev/plasmid/config"
	"github.com/plasmid-dev/plasmid/internal/fixture"
	"github.com/plasmid-dev/plasmid/outputlimit"
	"github.com/plasmid-dev/plasmid/shellexec"
	"github.com/plasmid-dev/plasmid/warning"
	"github.com/plasmid-dev/plasmid/workspace"
)

func init() {
	fixture.RegisterRunner("context", "contextresolver/all", "budget", "command", "discovery", "imports", "lazy", "records", "scope")
}

func TestMain(m *testing.M) { os.Exit(fixture.Run(m)) }

type contextFixtureInput struct {
	CommandBytes  int                  `json:"command_bytes"`
	DocumentBytes int                  `json:"document_bytes"`
	Files         []contextFixtureFile `json:"files"`
	Matrix        []contextFixtureExec `json:"matrix"`
	MaxBytes      int                  `json:"max_bytes"`
	Source        string               `json:"source"`
	Touches       []string             `json:"touches"`
}

type contextFixtureFile struct {
	Content string `json:"content"`
	Path    string `json:"path"`
}

type contextFixtureExec struct {
	Mode  config.PromptCommandMode `json:"mode"`
	Trust string                   `json:"trust"`
}

func TestContextFixtures(t *testing.T) {
	fixture.Walk(t, "context", "contextresolver/all", func(t *testing.T, testCase fixture.Case) {
		metadata := testCase.Metadata(t)
		var input contextFixtureInput
		testCase.Decode(t, "input.json", &input)
		rootDir := t.TempDir()
		for _, file := range input.Files {
			writeFile(t, rootDir, file.Path, file.Content)
		}
		root, err := workspace.NewRoot(rootDir)
		if err != nil {
			t.Fatal(err)
		}
		sink := &warning.SliceSink{}
		actual := runContextFixture(t, metadata.Kind, input, root, sink)
		testCase.CompareJSON(t, "expected.json", actual, fixture.Paths{WorkDir: rootDir}, fixture.GoldenReadOnly)
	})
	fixture.AssertCoverage(t, "context")
}

func runContextFixture(t *testing.T, kind string, input contextFixtureInput, root *workspace.Root, sink *warning.SliceSink) any {
	t.Helper()
	switch kind {
	case "discovery", "imports", "lazy", "budget":
		return runDiscoveryFixture(t, input, root, sink)
	case "command":
		return runCommandFixture(t, input, root)
	case "scope":
		return runScopeFixture(t, root, sink)
	case "records":
		return runRecordsFixture(t, root, sink)
	default:
		t.Fatalf("unknown context fixture kind %q", kind)
		return nil
	}
}

func runDiscoveryFixture(t *testing.T, input contextFixtureInput, root *workspace.Root, sink *warning.SliceSink) any {
	t.Helper()
	maximum := input.MaxBytes
	if maximum == 0 {
		maximum = 256 << 10
	}
	resolver, err := New(Options{Root: root, MaxFileBytes: 16 << 10, MaxBytes: maximum, MaxImportDepth: 4, WarningSink: sink})
	if err != nil {
		t.Fatal(err)
	}
	defer resolver.Close()
	if err := resolver.StartSession(t.Context(), "fixture"); err != nil {
		t.Fatal(err)
	}
	before, err := resolver.Instructions(t.Context(), "fixture", "before")
	if err != nil {
		t.Fatal(err)
	}
	resolver.ReleaseRun("fixture")
	for _, path := range input.Touches {
		resolver.ObserveTouch(t.Context(), workspace.Touch{SessionID: "fixture", Path: path, Kind: workspace.TouchRead})
	}
	after, err := resolver.Instructions(t.Context(), "fixture", "after")
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{"after": after, "before": before, "warnings": fixture.StableWarnings(sink.Warnings())}
}

func runCommandFixture(t *testing.T, input contextFixtureInput, root *workspace.Root) any {
	t.Helper()
	executor, err := shellexec.New(shellexec.Config{Root: root, DefaultTimeout: time.Second, MaxTimeout: time.Second, OutputLimit: outputlimit.Policy{MaxBytes: 4096, MaxLines: 100, MaxLineBytes: 4096}})
	if err != nil {
		t.Fatal(err)
	}
	results := make([]map[string]any, 0, len(input.Matrix))
	for _, entry := range input.Matrix {
		entrySink := &warning.SliceSink{}
		trust := fixtureTrust(entry.Trust)
		output := expandCommands(t.Context(), input.Source, "fixture.md", trust, commandOptions{
			Mode: entry.Mode, CommandTimeout: time.Second, DocumentTimeout: time.Second,
			CommandOutputBytes: input.CommandBytes, DocumentOutputBytes: input.DocumentBytes,
		}, executor, entrySink)
		results = append(results, map[string]any{"mode": entry.Mode, "output": output, "trust": entry.Trust, "warnings": fixture.StableWarnings(entrySink.Warnings())})
	}
	return results
}

func runScopeFixture(t *testing.T, root *workspace.Root, sink *warning.SliceSink) any {
	t.Helper()
	resolver, err := New(Options{Root: root, WarningSink: sink})
	if err != nil {
		t.Fatal(err)
	}
	defer resolver.Close()
	if err := resolver.StartSession(t.Context(), "fixture"); err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.Instructions(t.Context(), "fixture", "turn"); err != nil {
		t.Fatal(err)
	}
	readAllowed := resolver.Allows("fixture", "turn", "read", map[string]any{"path": "README.md"})
	writeAllowed := resolver.Allows("fixture", "turn", "write", map[string]any{"path": "README.md"})
	resolver.ReleaseRun("fixture")
	return map[string]any{
		"active_after_release": resolver.ActiveScopes(), "read_allowed": readAllowed,
		"write_after_release": resolver.Allows("fixture", "turn", "write", map[string]any{"path": "README.md"}),
		"write_allowed":       writeAllowed,
	}
}

func runRecordsFixture(t *testing.T, root *workspace.Root, sink *warning.SliceSink) any {
	t.Helper()
	resolver, err := New(Options{Root: root, WarningSink: sink})
	if err != nil {
		t.Fatal(err)
	}
	defer resolver.Close()
	if err := resolver.StartSession(t.Context(), "fixture"); err != nil {
		t.Fatal(err)
	}
	return resolver.InstructionRecords("fixture")
}

func fixtureTrust(value string) TrustLevel {
	switch value {
	case userScope:
		return TrustUser
	case "repository":
		return TrustRepository
	default:
		return TrustUntrusted
	}
}
