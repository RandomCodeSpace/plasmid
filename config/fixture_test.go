package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/plasmid-dev/plasmid/internal/fixture"
)

const configFixtureRunner = "config/load"

func init() {
	fixture.RegisterRunner("config", "config/load", "load")
}

func TestMain(m *testing.M) {
	os.Exit(fixture.Run(m))
}

type fixtureCase struct {
	Area    string `json:"area"`
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Mode    string `json:"mode"`
	Options struct {
		AppName          string `json:"appName"`
		ConfigFile       string `json:"configFile"`
		LSPMode          string `json:"lspMode"`
		SessionDir       string `json:"sessionDir"`
		ToolConfirmation *bool  `json:"toolConfirmation"`
		UserID           string `json:"userID"`
	} `json:"options"`
}

type fixtureOutput struct {
	Config     Config                  `json:"config"`
	Error      string                  `json:"error"`
	SourcePath string                  `json:"sourcePath"`
	Warnings   []fixture.WarningFields `json:"warnings"`
}

func TestConfigFixtures(t *testing.T) {
	fixture.WalkKinds(t, "config", configFixtureRunner, []string{"load"}, func(t *testing.T, testCase fixture.Case) {
		var spec fixtureCase
		testCase.Decode(t, "case.json", &spec)
		inputRoot := filepath.Join(testCase.Dir, "input")
		paths := fixture.Paths{
			Home:      filepath.Join(inputRoot, "home"),
			WorkDir:   filepath.Join(inputRoot, "work"),
			ConfigDir: filepath.Join(inputRoot, "config"),
		}
		t.Setenv("HOME", paths.Home)
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(inputRoot, "xdg"))
		options := Options{WorkingDir: paths.WorkDir, SessionDir: spec.Options.SessionDir, UserID: spec.Options.UserID}
		if spec.Options.AppName != "" {
			options.AppName = &spec.Options.AppName
		}
		if spec.Options.LSPMode != "" {
			mode := LSPMode(spec.Options.LSPMode)
			options.LSPMode = &mode
		}
		options.ToolConfirmation = spec.Options.ToolConfirmation
		configFile := spec.Options.ConfigFile
		if configFile == "" {
			configFile = "config.json"
		}
		switch spec.Mode {
		case "explicit":
			options.ConfigPath = filepath.Join(inputRoot, "config", configFile)
		case "discovery":
		case "explicit-wins":
			options.ConfigPath = filepath.Join(inputRoot, "config", configFile)
		default:
			t.Fatalf("unknown fixture mode %q", spec.Mode)
		}
		result, err := Load(context.Background(), options)
		output := fixtureOutput{Error: fixtureError(err), Warnings: []fixture.WarningFields{}}
		if err == nil {
			output.Config = result.Config
			output.SourcePath = result.SourcePath
			output.Warnings = fixture.StableWarnings(result.Warnings)
		}
		testCase.CompareJSON(t, "expected.json", output, paths, fixture.GoldenReadOnly)
	})
	fixture.AssertCoverage(t, "config")
}

func fixtureError(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrUnsupportedVersion):
		return "unsupported-version"
	case errors.Is(err, ErrConfigNotFound):
		return "config-not-found"
	case errors.Is(err, ErrInvalidConfig):
		return "invalid-config"
	default:
		return "other"
	}
}
