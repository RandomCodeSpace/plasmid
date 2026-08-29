package lsp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/RandomCodeSpace/plasmid/internal/fixture"
	"github.com/RandomCodeSpace/plasmid/outputlimit"
	"go.lsp.dev/protocol"
)

func init() {
	fixture.RegisterRunner("lsp", "lsp/normalization", "diagnostic-normalization")
	fixture.RegisterRunner(
		"lsp", "lsp/enforcement",
		"diagnostics-rendering",
		"enforcement-result-status",
		"enforcement-failure",
		"enforcement-versioning",
		"enforcement-settle",
	)
}

func TestDiagnosticEnforcementFixtures(t *testing.T) {
	fixture.WalkKinds(t, "lsp", "lsp/enforcement", []string{"diagnostics-rendering"}, func(t *testing.T, testCase fixture.Case) {
		var input enforcementFixtureInput
		testCase.Decode(t, "input.json", &input)
		diagnostics := combineDiagnostics(input.Diagnostics, input.Maximum)
		actual := enforcementFixtureOutput{
			LanguageID:  languageID(input.Path),
			Diagnostics: diagnostics,
			Text:        renderDiagnostics(diagnostics, input.Output),
		}
		testCase.CompareJSON(t, "expected.json", actual, fixture.Paths{}, fixture.GoldenReadOnly)
	})
	walkEnforcementBehaviorFixtures(t)
}

func TestMain(main *testing.M) {
	os.Exit(fixture.Run(main))
}

func TestDiagnosticNormalizationFixtures(t *testing.T) {
	fixture.WalkKinds(t, "lsp", "lsp/normalization", []string{"diagnostic-normalization"}, func(t *testing.T, testCase fixture.Case) {
		var input diagnosticFixtureInput
		testCase.Decode(t, "input.json", &input)
		root := t.TempDir()
		path := filepath.Join(root, filepath.FromSlash(input.Path))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("package fixture\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		uri, err := PathToFileURI(path)
		if err != nil {
			t.Fatal(err)
		}
		values := make([]protocol.Diagnostic, 0, len(input.Diagnostics))
		for _, value := range input.Diagnostics {
			values = append(values, protocol.Diagnostic{
				Range: protocol.Range{Start: value.Start, End: value.End}, Severity: value.Severity,
				Code: protocol.String(value.Code), Source: protocol.NewOptional(value.Source), Message: protocol.String(value.Message),
			})
		}
		actual, err := NormalizeDiagnostics(root, uri, values, input.Maximum)
		if err != nil {
			t.Fatal(err)
		}
		testCase.CompareJSON(t, "expected.json", actual, fixture.Paths{}, fixture.GoldenReadOnly)
	})
	fixture.AssertCoverage(t, "lsp")
}

type diagnosticFixtureInput struct {
	Diagnostics []diagnosticFixtureValue `json:"diagnostics"`
	Maximum     int                      `json:"maximum"`
	Path        string                   `json:"path"`
}

type diagnosticFixtureValue struct {
	Code     string                      `json:"code"`
	End      protocol.Position           `json:"end"`
	Message  string                      `json:"message"`
	Severity protocol.DiagnosticSeverity `json:"severity"`
	Source   string                      `json:"source"`
	Start    protocol.Position           `json:"start"`
}

type enforcementFixtureInput struct {
	Diagnostics []Diagnostic       `json:"diagnostics"`
	Maximum     int                `json:"maximum"`
	Output      outputlimit.Policy `json:"output"`
	Path        string             `json:"path"`
}

type enforcementFixtureOutput struct {
	Diagnostics []Diagnostic `json:"diagnostics"`
	LanguageID  string       `json:"languageId"`
	Text        string       `json:"diagnosticsText"`
}
