package plasmid_test

import (
	"context"
	"errors"
	"iter"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/adk/v2/model"

	"github.com/RandomCodeSpace/plasmid"
	"github.com/RandomCodeSpace/plasmid/internal/fixture"
)

const harnessFixtureRunner = "plasmid/harness"

func init() {
	fixture.RegisterRunner("harness", "plasmid/harness", "error-code")
}

func TestMain(m *testing.M) {
	os.Exit(fixture.Run(m))
}

func TestHarnessFixtureCoverage(t *testing.T) {
	fixture.AssertCoverage(t, "harness")
}

func TestHarnessFixtures(t *testing.T) {
	fixture.WalkKinds(t, "harness", harnessFixtureRunner, []string{"error-code"}, func(t *testing.T, testCase fixture.Case) {
		var specification struct {
			Area     string `json:"area"`
			ID       string `json:"id"`
			Kind     string `json:"kind"`
			Scenario string `json:"scenario"`
		}
		testCase.Decode(t, "case.json", &specification)
		if specification.Area != "harness" || specification.ID != testCase.ID {
			t.Fatalf("fixture identity = %#v", specification)
		}
		err := runHarnessErrorScenario(t, specification.Scenario)
		output := struct {
			Code         plasmid.ErrorCode `json:"code"`
			Construction bool              `json:"construction"`
			Invalid      bool              `json:"invalid"`
			Unknown      bool              `json:"unknown"`
			Closed       bool              `json:"closed"`
		}{
			Code:         plasmid.CodeOf(err),
			Construction: errors.Is(err, plasmid.ErrConstructionFailed),
			Invalid:      errors.Is(err, plasmid.ErrInvalidArgument),
			Unknown:      errors.Is(err, plasmid.ErrUnknownSession),
			Closed:       errors.Is(err, plasmid.ErrClosed),
		}
		testCase.CompareJSON(t, "expected.json", output, fixture.Paths{}, fixture.GoldenReadOnly)
	})
}

func runHarnessErrorScenario(t *testing.T, scenario string) error {
	t.Helper()
	workingDir := t.TempDir()
	sessionDir := filepath.Join(t.TempDir(), "sessions")
	if scenario == "missing-model" {
		_, err := plasmid.New(t.Context(), plasmid.WithWorkingDir(workingDir), plasmid.WithSessionDir(sessionDir))
		return err
	}
	harness, err := plasmid.New(t.Context(),
		plasmid.WithModel(emptyModel{}),
		plasmid.WithWorkingDir(workingDir),
		plasmid.WithSessionDir(sessionDir),
	)
	if err != nil {
		t.Fatal(err)
	}
	switch scenario {
	case "unknown-session":
		defer closeTestResource(t, harness)
		for _, runErr := range harness.Run(t.Context(), "missing", "prompt") {
			if runErr != nil {
				return runErr
			}
		}
		return nil
	case "closed":
		if err := harness.Close(); err != nil {
			t.Fatal(err)
		}
		_, err := harness.NewSession(t.Context())
		return err
	default:
		t.Fatalf("unknown harness fixture scenario %q", scenario)
		return nil
	}
}

type emptyModel struct{}

func (emptyModel) Name() string { return "empty" }
func (emptyModel) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(func(*model.LLMResponse, error) bool) {}
}

var _ model.LLM = emptyModel{}
