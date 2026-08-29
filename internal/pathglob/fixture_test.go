package pathglob

import (
	"os"
	"testing"

	"github.com/RandomCodeSpace/plasmid/internal/fixture"
)

func init() {
	fixture.RegisterRunner("tools", "pathglob/behavior", "pathglob")
	fixture.RegisterRunner("pathglob", "pathglob/all", "invalid", "match", "split")
}

func TestMain(m *testing.M) {
	os.Exit(fixture.Run(m))
}

type portablePathglobFixtureInput struct {
	Patterns []string `json:"patterns"`
	Paths    []string `json:"paths"`
}

type portablePathglobFixtureExpected struct {
	Matches  map[string]bool `json:"matches"`
	Patterns []string        `json:"patterns"`
}

func TestBehaviorFixtures(t *testing.T) {
	fixture.WalkKinds(t, "tools", "pathglob/behavior", []string{"pathglob"}, func(t *testing.T, testCase fixture.Case) {
		var input portablePathglobFixtureInput
		testCase.Decode(t, "input.json", &input)
		matcher, problems := Compile(input.Patterns)
		if len(problems) != 0 {
			t.Fatalf("Compile() errors = %v", problems)
		}
		actual := portablePathglobFixtureExpected{Matches: make(map[string]bool, len(input.Paths)), Patterns: matcher.Patterns()}
		for _, path := range input.Paths {
			actual.Matches[path] = matcher.Match(path)
		}
		testCase.CompareJSON(t, "expected.json", actual, fixture.Paths{}, fixture.GoldenReadOnly)
	})
}

type pathglobFixtureInput struct {
	List     string   `json:"list"`
	Paths    []string `json:"paths"`
	Patterns []string `json:"patterns"`
}

func TestPathglobFixtures(t *testing.T) {
	fixture.Walk(t, "pathglob", "pathglob/all", func(t *testing.T, testCase fixture.Case) {
		metadata := testCase.Metadata(t)
		var input pathglobFixtureInput
		testCase.Decode(t, "input.json", &input)
		var actual any
		switch metadata.Kind {
		case "match", "invalid":
			matcher, compileErrors := Compile(input.Patterns)
			matches := make([]bool, 0, len(input.Paths))
			for _, path := range input.Paths {
				matches = append(matches, matcher.Match(path))
			}
			actual = map[string]any{"error_count": len(compileErrors), "matches": matches, "patterns": matcher.Patterns()}
		case "split":
			actual = SplitList(input.List)
		default:
			t.Fatalf("unknown pathglob fixture kind %q", metadata.Kind)
		}
		testCase.CompareJSON(t, "expected.json", actual, fixture.Paths{}, fixture.GoldenReadOnly)
	})
	fixture.AssertCoverage(t, "pathglob")
}
