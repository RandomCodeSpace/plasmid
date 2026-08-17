package walk

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/plasmid-dev/plasmid/internal/fixture"
	"github.com/plasmid-dev/plasmid/warning"
	"github.com/plasmid-dev/plasmid/workspace"
)

type walkFixtureMetadata struct {
	Area   string            `json:"area"`
	Filter walkFixtureFilter `json:"filter"`
	ID     string            `json:"id"`
	Kind   string            `json:"kind"`
}

type walkFixtureFilter struct {
	ExcludeGlobs     []string `json:"exclude_globs"`
	IncludeGlobs     []string `json:"include_globs"`
	MaxDepth         int      `json:"max_depth"`
	MaxResults       int      `json:"max_results"`
	MaxVisited       int      `json:"max_visited"`
	RespectGitignore bool     `json:"respect_gitignore"`
	SkipHidden       bool     `json:"skip_hidden"`
	SkipVCS          bool     `json:"skip_vcs"`
}

type walkFixtureExpected struct {
	ErrorCode    string   `json:"error_code"`
	OrderedPaths []string `json:"ordered_paths"`
}

type walkFixtureInput struct {
	Files map[string]string `json:"files"`
}

func init() {
	fixture.RegisterRunner("tools", "walk/all", "walk")
}

func TestMain(m *testing.M) {
	os.Exit(fixture.Run(m))
}

func TestWalkFixtureCoverage(t *testing.T) {
	fixture.AssertCoverage(t, "tools")
}

func TestWalkFixtures(t *testing.T) {
	fixture.WalkKinds(t, "tools", "walk/all", []string{"walk"}, func(t *testing.T, testCase fixture.Case) {
		var metadata walkFixtureMetadata
		testCase.Decode(t, "case.json", &metadata)
		if metadata.Area != "tools" || metadata.ID != testCase.ID || metadata.Kind != "walk" {
			t.Fatalf("metadata = %#v", metadata)
		}

		got, warnings := runWalkFixture(t, testCase, metadata.Filter)
		testCase.CompareJSON(t, "expected.json", got, fixture.Paths{}, fixture.GoldenReadOnly)
		warningsPath := filepath.Join(testCase.Dir, "warnings.json")
		if _, err := os.Stat(warningsPath); err == nil {
			testCase.CompareJSON(t, "warnings.json", fixture.StableWarnings(warnings), fixture.Paths{}, fixture.GoldenReadOnly)
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		} else if len(warnings) != 0 {
			t.Fatalf("fixture emitted warnings without warnings.json: %#v", fixture.StableWarnings(warnings))
		}
		again, againWarnings := runWalkFixture(t, testCase, metadata.Filter)
		if !reflect.DeepEqual(got, again) || !reflect.DeepEqual(warnings, againWarnings) {
			t.Fatal("walk fixture was nondeterministic")
		}
	})
}

func runWalkFixture(t *testing.T, testCase fixture.Case, input walkFixtureFilter) (walkFixtureExpected, []warning.Warning) {
	t.Helper()
	var tree walkFixtureInput
	testCase.Decode(t, "input.json", &tree)
	directory := t.TempDir()
	for path, content := range tree.Files {
		absolute := filepath.Join(directory, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(absolute, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	root, err := workspace.NewRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	filter := &Filter{
		Root:             root,
		IncludeGlobs:     input.IncludeGlobs,
		ExcludeGlobs:     input.ExcludeGlobs,
		SkipHidden:       input.SkipHidden,
		SkipVCS:          input.SkipVCS,
		RespectGitignore: input.RespectGitignore,
		MaxDepth:         input.MaxDepth,
		MaxVisited:       input.MaxVisited,
		MaxResults:       input.MaxResults,
	}
	got := walkFixtureExpected{OrderedPaths: []string{}}
	var warnings warning.SliceSink
	err = walk(context.Background(), filter, func(entry Entry) error {
		got.OrderedPaths = append(got.OrderedPaths, entry.Path)
		return nil
	}, &warnings)
	if errors.Is(err, ErrWalkTruncated) {
		got.ErrorCode = "walk_truncated"
	} else if err != nil {
		got.ErrorCode = "unknown"
	}
	return got, warnings.Warnings()
}
