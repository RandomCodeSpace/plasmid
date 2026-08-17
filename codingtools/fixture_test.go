package codingtools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plasmid-dev/plasmid/codingtools/internal/textmatch"
	"github.com/plasmid-dev/plasmid/internal/fixture"
	"github.com/plasmid-dev/plasmid/loop"
	"github.com/plasmid-dev/plasmid/outputlimit"
	"github.com/plasmid-dev/plasmid/shellexec"
	"github.com/plasmid-dev/plasmid/workspace"
)

type schemaFixtureMetadata struct {
	Area string `json:"area"`
	ID   string `json:"id"`
	Kind string `json:"kind"`
}

type schemaFixtureInput struct {
	Tool string `json:"tool"`
}

func init() {
	fixture.RegisterRunner("tools", "codingtools/schema", "schema")
	fixture.RegisterRunner("tools", "codingtools/write", "write")
	fixture.RegisterRunner("tools", "codingtools/edit", "edit")
	fixture.RegisterRunner("tools", "codingtools/read", "read")
	fixture.RegisterRunner("tools", "codingtools/e02-behavior")
}

func TestMain(m *testing.M) {
	os.Exit(fixture.Run(m))
}

func TestSchemaFixtures(t *testing.T) {
	fixture.WalkKinds(t, "tools", "codingtools/schema", []string{"schema"}, func(t *testing.T, testCase fixture.Case) {
		var metadata schemaFixtureMetadata
		var input schemaFixtureInput
		testCase.Decode(t, "case.json", &metadata)
		testCase.Decode(t, "input.json", &input)
		if metadata.Area != "tools" || metadata.ID != testCase.ID || metadata.Kind != "schema" {
			t.Fatalf("invalid metadata: %#v", metadata)
		}
		accessors := map[string]schemaAccessor{
			"read": ReadInputSchema, "write": WriteInputSchema, "edit": EditInputSchema,
			"bash": BashInputSchema, "grep": GrepInputSchema, "find": FindInputSchema,
			"ls": ListInputSchema,
		}
		accessor, ok := accessors[input.Tool]
		if !ok {
			t.Fatalf("unknown schema tool %q", input.Tool)
		}
		got := accessor()
		testCase.CompareJSON(t, "expected.json", got, fixture.Paths{}, fixture.GoldenReadOnly)
		testCase.CompareJSON(t, "warnings.json", fixture.StableWarnings(nil), fixture.Paths{}, fixture.GoldenReadOnly)
		var decoded any
		if err := json.Unmarshal(got, &decoded); err != nil {
			t.Fatal(err)
		}
		roundTrip, err := json.Marshal(decoded)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, roundTrip) {
			t.Fatalf("canonical round trip changed schema\ngot:  %s\nwant: %s", roundTrip, got)
		}
	})
}

type writeFixtureInput struct {
	Args    map[string]any    `json:"args"`
	Files   map[string]string `json:"files"`
	Read    []string          `json:"read"`
	Session string            `json:"session"`
}

type writeFixtureExpected struct {
	Error      string       `json:"error"`
	File       string       `json:"file"`
	FileExists bool         `json:"file_exists"`
	Ledger     bool         `json:"ledger"`
	OK         bool         `json:"ok"`
	Result     *WriteResult `json:"result"`
	TempFiles  int          `json:"temp_files"`
	Touches    int          `json:"touches"`
}

func TestWriteFixtures(t *testing.T) {
	fixture.WalkKinds(t, "tools", "codingtools/write", []string{"write"}, func(t *testing.T, testCase fixture.Case) {
		var metadata schemaFixtureMetadata
		var input writeFixtureInput
		testCase.Decode(t, "case.json", &metadata)
		testCase.Decode(t, "input.json", &input)
		if metadata.Area != "tools" || metadata.ID != testCase.ID || metadata.Kind != "write" {
			t.Fatalf("invalid metadata: %#v", metadata)
		}
		rootDir := t.TempDir()
		for path, content := range input.Files {
			full := filepath.Join(rootDir, filepath.FromSlash(path))
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		root, err := workspace.NewRoot(rootDir)
		if err != nil {
			t.Fatal(err)
		}
		ledger, bus := workspace.NewLedger(), workspace.NewTouchBus()
		observer := &writeObserver{}
		bus.Subscribe(observer)
		tool, err := NewWriteTool(Config{Root: root, Queue: workspace.NewMutationQueue(), Ledger: ledger, Touch: bus, Budget: outputlimit.NewBudget(10000)})
		if err != nil {
			t.Fatal(err)
		}
		sessionID := input.Session
		if sessionID == "" {
			sessionID = "fixture"
		}
		for _, path := range input.Read {
			contents, readErr := os.ReadFile(filepath.Join(rootDir, filepath.FromSlash(path)))
			if readErr != nil {
				t.Fatal(readErr)
			}
			ledger.RecordRead(sessionID, path, int64(len(contents)), sha256.Sum256(contents))
		}
		result, err := tool.Call(context.Background(), loop.ToolCall{SessionID: sessionID, Args: input.Args})
		actual := writeFixtureExpected{Error: writeFixtureError(err), OK: err == nil}
		if err == nil {
			decoded := decodeWriteResult(t, result.Content)
			actual.Result = &decoded
		} else if result.Content != nil {
			t.Fatalf("failure result = %#v, want nil", result.Content)
		}
		path, _ := input.Args["path"].(string)
		full := filepath.Join(rootDir, filepath.FromSlash(path))
		got, readErr := os.ReadFile(full)
		if readErr == nil {
			actual.FileExists = true
			actual.File = string(got)
		} else if !errors.Is(readErr, os.ErrNotExist) {
			t.Fatalf("check written file: %v", readErr)
		}
		actual.Touches = len(observer.snapshot())
		ledgerPath := path
		if actual.Result != nil {
			ledgerPath = actual.Result.Path
		}
		hash := sha256.Sum256([]byte(actual.File))
		ledgerErr := ledger.Verify(sessionID, ledgerPath, int64(len(actual.File)), hash)
		actual.Ledger = ledgerErr == nil
		if ledgerErr != nil && !errors.Is(ledgerErr, workspace.ErrNeverRead) {
			t.Fatalf("failed write changed ledger: %v", ledgerErr)
		}
		temps := 0
		walkErr := filepath.WalkDir(rootDir, func(_ string, entry os.DirEntry, walkErr error) error {
			if walkErr == nil && strings.HasPrefix(entry.Name(), ".plasmid-write-") {
				temps++
			}
			return walkErr
		})
		if walkErr != nil {
			t.Fatal(walkErr)
		}
		actual.TempFiles = temps
		testCase.CompareJSON(t, "expected.json", actual, fixture.Paths{}, fixture.GoldenReadOnly)
		testCase.CompareJSON(t, "warnings.json", fixture.StableWarnings(nil), fixture.Paths{}, fixture.GoldenReadOnly)
	})
}

func writeFixtureError(err error) string {
	if err == nil {
		return ""
	}
	if strings.Contains(err.Error(), "content is required") {
		return "content is required"
	}
	return err.Error()
}

type editHandlerFixtureInput struct {
	Content    string `json:"content"`
	NewText    string `json:"new_text"`
	OldText    string `json:"old_text"`
	ReplaceAll bool   `json:"replace_all"`
}

type editHandlerFixtureExpected struct {
	AmbiguityLines []int  `json:"ambiguity_lines"`
	Diff           string `json:"diff"`
	ErrorCode      string `json:"error_code"`
	OK             bool   `json:"ok"`
	Replacements   int    `json:"replacements"`
	ResultContent  string `json:"result_content"`
	Tier           string `json:"tier"`
}

func TestEditFixtures(t *testing.T) {
	fixture.WalkKinds(t, "tools", "codingtools/edit", []string{"edit"}, func(t *testing.T, testCase fixture.Case) {
		var metadata schemaFixtureMetadata
		var input editHandlerFixtureInput
		testCase.Decode(t, "case.json", &metadata)
		testCase.Decode(t, "input.json", &input)
		if metadata.Area != "tools" || metadata.ID != testCase.ID || metadata.Kind != "edit" {
			t.Fatalf("invalid metadata: %#v", metadata)
		}
		rootDir := t.TempDir()
		path := filepath.Join(rootDir, "file.txt")
		if err := os.WriteFile(path, []byte(input.Content), 0o644); err != nil {
			t.Fatal(err)
		}
		root, err := workspace.NewRoot(rootDir)
		if err != nil {
			t.Fatal(err)
		}
		ledger, bus := workspace.NewLedger(), workspace.NewTouchBus()
		observer := &writeObserver{}
		bus.Subscribe(observer)
		tool, err := NewEditTool(Config{Root: root, Queue: workspace.NewMutationQueue(), Ledger: ledger, Touch: bus, Budget: outputlimit.NewBudget(10000)})
		if err != nil {
			t.Fatal(err)
		}
		ledger.RecordRead("fixture", "file.txt", int64(len(input.Content)), sha256.Sum256([]byte(input.Content)))
		result, callErr := tool.Call(context.Background(), loop.ToolCall{ID: "fixture-call", SessionID: "fixture", Args: map[string]any{
			"path": "file.txt", "old_text": input.OldText, "new_text": input.NewText, "replace_all": input.ReplaceAll,
		}})
		actual := editHandlerFixtureExpected{AmbiguityLines: []int{}, ErrorCode: editHandlerErrorCode(callErr), OK: callErr == nil}
		if result.CallID != "fixture-call" {
			t.Fatalf("call id = %q", result.CallID)
		}
		if callErr == nil {
			decoded := decodeEditResult(t, result.Content)
			if decoded.Path != "file.txt" {
				t.Fatalf("result path = %q, want file.txt", decoded.Path)
			}
			actual.Diff = decoded.Diff
			actual.Replacements = decoded.Replacements
			actual.Tier = decoded.MatchTier
		} else if result.Content != nil {
			t.Fatalf("failure result = %#v, want nil", result.Content)
		} else {
			var ambiguity *textmatch.AmbiguityError
			if errors.As(callErr, &ambiguity) {
				actual.AmbiguityLines = ambiguity.Lines
			}
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if callErr == nil {
			actual.ResultContent = string(got)
		} else if string(got) != input.Content {
			t.Fatalf("failed edit changed content: %q", got)
		}
		if touches := len(observer.snapshot()); touches != boolToCount(callErr == nil) {
			t.Fatalf("touches = %d, want %d", touches, boolToCount(callErr == nil))
		}
		testCase.CompareJSON(t, "expected.json", actual, fixture.Paths{}, fixture.GoldenReadOnly)
	})
}

func editHandlerErrorCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, textmatch.ErrEmptyOld):
		return "empty_old_text"
	case errors.Is(err, textmatch.ErrNoOpEdit):
		return "no_op_edit"
	case errors.Is(err, ErrNoMatch):
		return "no_match"
	case errors.Is(err, ErrAmbiguousMatch):
		return "ambiguous_match"
	default:
		return "unknown"
	}
}

func boolToCount(value bool) int {
	if value {
		return 1
	}
	return 0
}

func decodeEditResult(t *testing.T, object map[string]any) EditResult {
	t.Helper()
	encoded, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	var result EditResult
	if err := json.Unmarshal(encoded, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

type behaviorFixtureInput struct {
	Args    map[string]any    `json:"args"`
	Files   map[string]string `json:"files"`
	Session string            `json:"session"`
	Tool    string            `json:"tool"`
}

func TestBehaviorFixtures(t *testing.T) {
	kinds := []string{}
	if len(kinds) == 0 {
		t.Skip("behavior fixture cases are owned by E02")
	}
	fixture.WalkKinds(t, "tools", "codingtools/e02-behavior", kinds, func(t *testing.T, testCase fixture.Case) {
		var metadata schemaFixtureMetadata
		var input behaviorFixtureInput
		testCase.Decode(t, "case.json", &metadata)
		testCase.Decode(t, "input.json", &input)
		if metadata.Area != "tools" || metadata.ID != testCase.ID || metadata.Kind != input.Tool {
			t.Fatalf("invalid metadata/input: %#v, %#v", metadata, input)
		}
		if metadata.Kind == "specifier" {
			actual := struct {
				Value string `json:"value"`
			}{Value: ToolSpecifier(input.ToolName(), input.Args)}
			testCase.CompareJSON(t, "expected.json", actual, fixture.Paths{}, fixture.GoldenReadOnly)
			return
		}

		rootDir := t.TempDir()
		seedBehaviorFixture(t, rootDir, input.Files)
		root, err := workspace.NewRoot(rootDir)
		if err != nil {
			t.Fatal(err)
		}
		bus := workspace.NewTouchBus()
		observer := &listObserver{}
		bus.Subscribe(observer)
		cfg := Config{Root: root, Touch: bus, Budget: outputlimit.NewBudget(100000), Output: outputlimit.Defaults()}
		var tool loop.Tool
		switch metadata.Kind {
		case "bash":
			shell, shellErr := shellexec.New(shellexec.Config{Root: root, Shell: "sh", OutputLimit: cfg.Output})
			if shellErr != nil {
				t.Skipf("fixture shell unavailable: %v", shellErr)
			}
			cfg.Shell = shell
			tool, err = NewBashTool(cfg)
		case "grep":
			tool, err = NewGrepTool(cfg)
		case "find":
			tool, err = NewFindTool(cfg)
		case "ls":
			tool, err = NewListTool(cfg)
		}
		if err != nil {
			t.Fatal(err)
		}
		result, err := tool.Call(context.Background(), loop.ToolCall{ID: "fixture-call", SessionID: input.Session, Args: input.Args})
		if err != nil {
			t.Fatal(err)
		}
		if result.CallID != "fixture-call" {
			t.Fatalf("call id = %q", result.CallID)
		}
		assertBehaviorFixture(t, testCase, metadata.Kind, result.Content, observer.snapshot())
	})
}

func (input behaviorFixtureInput) ToolName() string {
	if value, ok := input.Args["tool_name"].(string); ok {
		return value
	}
	return ""
}

func seedBehaviorFixture(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for path, content := range files {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func assertBehaviorFixture(t *testing.T, testCase fixture.Case, kind string, content map[string]any, touches []workspace.Touch) {
	t.Helper()
	switch kind {
	case "bash":
		actual := struct {
			ExitCode  int    `json:"exit_code"`
			Killed    bool   `json:"killed"`
			Signal    string `json:"signal"`
			Stderr    string `json:"stderr"`
			Stdout    string `json:"stdout"`
			TimedOut  bool   `json:"timed_out"`
			Truncated bool   `json:"truncated"`
		}{}
		got := decodeBashResult(t, content)
		actual.ExitCode, actual.Killed, actual.Signal = got.ExitCode, got.Killed, got.Signal
		actual.Stderr, actual.Stdout = got.Stderr, got.Stdout
		actual.TimedOut, actual.Truncated = got.TimedOut, got.Truncated
		testCase.CompareJSON(t, "expected.json", actual, fixture.Paths{}, fixture.GoldenReadOnly)
	case "grep":
		actual := struct {
			Result  GrepResult `json:"result"`
			Touches []string   `json:"touches"`
		}{Touches: touchPaths(touches)}
		decodeFixtureObject(t, content, &actual.Result)
		testCase.CompareJSON(t, "expected.json", actual, fixture.Paths{}, fixture.GoldenReadOnly)
	case "find":
		actual := struct {
			Result  FindResult `json:"result"`
			Touches []string   `json:"touches"`
		}{Touches: touchPaths(touches)}
		decodeFixtureObject(t, content, &actual.Result)
		testCase.CompareJSON(t, "expected.json", actual, fixture.Paths{}, fixture.GoldenReadOnly)
	case "ls":
		actual := struct {
			Entries []struct {
				Path string `json:"path"`
				Type string `json:"type"`
			} `json:"entries"`
			Touches   []string `json:"touches"`
			Truncated bool     `json:"truncated"`
		}{}
		var got ListResult
		decodeFixtureObject(t, content, &got)
		actual.Entries = make([]struct {
			Path string `json:"path"`
			Type string `json:"type"`
		}, len(got.Entries))
		for index, entry := range got.Entries {
			actual.Entries[index].Path, actual.Entries[index].Type = entry.Path, entry.Type
		}
		actual.Touches = touchPaths(touches)
		actual.Truncated = got.Truncated
		testCase.CompareJSON(t, "expected.json", actual, fixture.Paths{}, fixture.GoldenReadOnly)
	}
}

func decodeFixtureObject(t *testing.T, content map[string]any, target any) {
	t.Helper()
	encoded, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, target); err != nil {
		t.Fatal(err)
	}
}

func touchPaths(touches []workspace.Touch) []string {
	paths := make([]string, len(touches))
	for index, touch := range touches {
		paths[index] = touch.Path
	}
	return paths
}
