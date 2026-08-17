package textmatch

import (
	"errors"
	"reflect"
	"testing"

	"github.com/plasmid-dev/plasmid/internal/fixture"
)

type fixtureMetadata struct {
	Area string `json:"area"`
	ID   string `json:"id"`
	Kind string `json:"kind"`
}

type editFixtureInput struct {
	Content    string `json:"content"`
	NewText    string `json:"new_text"`
	OldText    string `json:"old_text"`
	ReplaceAll bool   `json:"replace_all"`
}

type editFixtureExpected struct {
	AmbiguityLines []int  `json:"ambiguity_lines"`
	Diff           string `json:"diff"`
	ErrorCode      string `json:"error_code"`
	OK             bool   `json:"ok"`
	Replacements   int    `json:"replacements"`
	ResultContent  string `json:"result_content"`
	Tier           string `json:"tier"`
}

type diffFixtureInput struct {
	Context int    `json:"context"`
	NewText string `json:"new_text"`
	OldText string `json:"old_text"`
	Path    string `json:"path"`
}

type diffFixtureExpected struct {
	Diff string `json:"diff"`
}

func init() {
	fixture.Register("tools")
}

func TestFixtureCoverage(t *testing.T) {
	fixture.AssertCoverage(t, "tools")
}

func TestToolsFixtures(t *testing.T) {
	fixture.WalkKinds(t, "tools", []string{"diff", "edit"}, func(t *testing.T, testCase fixture.Case) {
		metadata := validateFixtureMetadata(t, testCase)
		switch metadata.Kind {
		case "edit":
			runEditFixture(t, testCase)
		case "diff":
			runDiffFixture(t, testCase)
		}
	})
}

func runEditFixture(t *testing.T, testCase fixture.Case) {
	t.Helper()
	var input editFixtureInput
	var want editFixtureExpected
	testCase.Decode(t, "input.json", &input)
	testCase.Decode(t, "expected.json", &want)

	result, err := Apply(Request{
		Content: input.Content, Old: input.OldText, New: input.NewText, ReplaceAll: input.ReplaceAll,
	})
	got := editFixtureExpected{AmbiguityLines: []int{}, ErrorCode: editErrorCode(err), OK: err == nil}
	if err == nil {
		got.Diff = UnifiedDiff(input.Content, result.Content, "file.txt", 3)
		if again := UnifiedDiff(input.Content, result.Content, "file.txt", 3); again != got.Diff {
			t.Fatal("edit fixture diff was nondeterministic")
		}
		got.Replacements = result.Replacements
		got.ResultContent = result.Content
		got.Tier = result.Tier.String()
	} else {
		var ambiguity *AmbiguityError
		if errors.As(err, &ambiguity) {
			got.AmbiguityLines = ambiguity.Lines
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fixture result mismatch\ngot:  %#v\nwant: %#v", got, want)
	}
}

func runDiffFixture(t *testing.T, testCase fixture.Case) {
	t.Helper()
	var input diffFixtureInput
	var want diffFixtureExpected
	testCase.Decode(t, "input.json", &input)
	testCase.Decode(t, "expected.json", &want)
	got := UnifiedDiff(input.OldText, input.NewText, input.Path, input.Context)
	if again := UnifiedDiff(input.OldText, input.NewText, input.Path, input.Context); again != got {
		t.Fatal("diff fixture was nondeterministic")
	}
	if got != want.Diff {
		t.Fatalf("diff mismatch\ngot:\n%q\nwant:\n%q", got, want.Diff)
	}
}

func validateFixtureMetadata(t *testing.T, testCase fixture.Case) fixtureMetadata {
	t.Helper()
	var metadata fixtureMetadata
	testCase.Decode(t, "case.json", &metadata)
	if metadata.Area != "tools" || metadata.ID != testCase.ID {
		t.Fatalf("metadata = %#v, want area %q id %q", metadata, "tools", testCase.ID)
	}
	return metadata
}

func editErrorCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrEmptyOld):
		return "empty_old_text"
	case errors.Is(err, ErrNoOpEdit):
		return "no_op_edit"
	case errors.Is(err, ErrNoMatch):
		return "no_match"
	case errors.Is(err, ErrAmbiguousMatch):
		return "ambiguous_match"
	default:
		return "unknown"
	}
}
