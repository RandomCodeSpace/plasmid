package walk

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/plasmid-dev/plasmid/internal/fixture"
	"github.com/plasmid-dev/plasmid/loop"
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

type walkFixtureWarnings struct {
	Codes []string `json:"codes"`
}

func init() {
	fixture.Register("tools")
}

func TestWalkFixtureCoverage(t *testing.T) {
	fixture.AssertCoverage(t, "tools")
}

func TestWalkFixtures(t *testing.T) {
	fixture.WalkKinds(t, "tools", []string{"walk"}, func(t *testing.T, testCase fixture.Case) {
		var metadata walkFixtureMetadata
		var want walkFixtureExpected
		testCase.Decode(t, "case.json", &metadata)
		testCase.Decode(t, "expected.json", &want)
		if metadata.Area != "tools" || metadata.ID != testCase.ID || metadata.Kind != "walk" {
			t.Fatalf("metadata = %#v", metadata)
		}

		var warningGolden walkFixtureWarnings
		warningsPath := filepath.Join(testCase.Dir, "warnings.json")
		if _, err := os.Stat(warningsPath); err == nil {
			testCase.Decode(t, "warnings.json", &warningGolden)
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}

		got, warningCodes := runWalkFixture(t, testCase, metadata.Filter)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("fixture result mismatch\ngot:  %#v\nwant: %#v", got, want)
		}
		if !reflect.DeepEqual(warningCodes, warningGolden.Codes) {
			t.Fatalf("warning codes = %#v, want %#v", warningCodes, warningGolden.Codes)
		}
		again, againWarnings := runWalkFixture(t, testCase, metadata.Filter)
		if !reflect.DeepEqual(got, again) || !reflect.DeepEqual(warningCodes, againWarnings) {
			t.Fatal("walk fixture was nondeterministic")
		}
	})
}

func runWalkFixture(t *testing.T, testCase fixture.Case, input walkFixtureFilter) (walkFixtureExpected, []string) {
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
	var warnings loop.SliceSink
	err = walk(context.Background(), filter, func(entry Entry) error {
		got.OrderedPaths = append(got.OrderedPaths, entry.Path)
		return nil
	}, &warnings)
	if errors.Is(err, ErrWalkTruncated) {
		got.ErrorCode = "walk_truncated"
	} else if err != nil {
		got.ErrorCode = "unknown"
	}
	collected := warnings.Warnings()
	var warningCodes []string
	for _, warning := range collected {
		warningCodes = append(warningCodes, warning.Code)
	}
	return got, warningCodes
}
