package pathglob

import (
	"os"
	"testing"

	"github.com/plasmid-dev/plasmid/internal/fixture"
)

func init() {
	fixture.RegisterRunner("tools", "pathglob/behavior", "pathglob")
}

func TestMain(m *testing.M) {
	os.Exit(fixture.Run(m))
}

type pathglobFixtureInput struct {
	Patterns []string `json:"patterns"`
	Paths    []string `json:"paths"`
}

type pathglobFixtureExpected struct {
	Matches  map[string]bool `json:"matches"`
	Patterns []string        `json:"patterns"`
}

func TestBehaviorFixtures(t *testing.T) {
	fixture.WalkKinds(t, "tools", "pathglob/behavior", []string{"pathglob"}, func(t *testing.T, testCase fixture.Case) {
		var input pathglobFixtureInput
		testCase.Decode(t, "input.json", &input)
		matcher, problems := Compile(input.Patterns)
		if len(problems) != 0 {
			t.Fatalf("Compile() errors = %v", problems)
		}
		actual := pathglobFixtureExpected{Matches: make(map[string]bool, len(input.Paths)), Patterns: matcher.Patterns()}
		for _, path := range input.Paths {
			actual.Matches[path] = matcher.Match(path)
		}
		testCase.CompareJSON(t, "expected.json", actual, fixture.Paths{}, fixture.GoldenReadOnly)
	})
}
