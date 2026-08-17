package codingtools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
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
	fixture.Register("tools")
}

func TestSchemaFixtures(t *testing.T) {
	fixture.WalkKinds(t, "tools", []string{"schema"}, func(t *testing.T, testCase fixture.Case) {
		var metadata schemaFixtureMetadata
		var input schemaFixtureInput
		var expected json.RawMessage
		var warnings []any
		testCase.Decode(t, "case.json", &metadata)
		testCase.Decode(t, "input.json", &input)
		testCase.Decode(t, "expected.json", &expected)
		testCase.Decode(t, "warnings.json", &warnings)
		if metadata.Area != "tools" || metadata.ID != testCase.ID || metadata.Kind != "schema" {
			t.Fatalf("invalid metadata: %#v", metadata)
		}
		if len(warnings) != 0 {
			t.Fatalf("schema fixture warnings = %#v, want none", warnings)
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
		if !bytes.Equal(got, expected) {
			t.Fatalf("schema fixture mismatch\ngot:  %s\nwant: %s", got, expected)
		}
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
	fixture.WalkKinds(t, "tools", []string{"write"}, func(t *testing.T, testCase fixture.Case) {
		var metadata schemaFixtureMetadata
		var input writeFixtureInput
		var expected writeFixtureExpected
		var warnings []any
		testCase.Decode(t, "case.json", &metadata)
		testCase.Decode(t, "input.json", &input)
		testCase.Decode(t, "expected.json", &expected)
		testCase.Decode(t, "warnings.json", &warnings)
		if metadata.Area != "tools" || metadata.ID != testCase.ID || metadata.Kind != "write" {
			t.Fatalf("invalid metadata: %#v", metadata)
		}
		if len(warnings) != 0 {
			t.Fatalf("write fixture warnings = %#v, want none", warnings)
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
		if expected.OK && err != nil {
			t.Fatal(err)
		}
		if !expected.OK && (err == nil || expected.Error == "" || !bytes.Contains([]byte(err.Error()), []byte(expected.Error))) {
			t.Fatalf("error = %v, want substring %q", err, expected.Error)
		}
		if expected.OK {
			decoded := decodeWriteResult(t, result.Content)
			if expected.Result == nil || !reflect.DeepEqual(decoded, *expected.Result) {
				t.Fatalf("result = %#v, want %#v", decoded, expected.Result)
			}
		} else if result.Content != nil || expected.Result != nil {
			t.Fatalf("failure result = %#v, want nil", result.Content)
		}
		path, _ := input.Args["path"].(string)
		full := filepath.Join(rootDir, filepath.FromSlash(path))
		got, readErr := os.ReadFile(full)
		if expected.FileExists {
			if readErr != nil || string(got) != expected.File {
				t.Fatalf("file = %q, %v; want %q", got, readErr, expected.File)
			}
		} else if !errors.Is(readErr, os.ErrNotExist) {
			t.Fatalf("file unexpectedly exists or cannot be checked: %v", readErr)
		}
		if touches := len(observer.snapshot()); touches != expected.Touches {
			t.Fatalf("touches = %d, want %d", touches, expected.Touches)
		}
		ledgerPath := path
		if expected.Result != nil {
			ledgerPath = expected.Result.Path
		}
		hash := sha256.Sum256([]byte(expected.File))
		ledgerErr := ledger.Verify(sessionID, ledgerPath, int64(len(expected.File)), hash)
		if expected.Ledger && ledgerErr != nil {
			t.Fatalf("ledger = %v", ledgerErr)
		}
		if !expected.Ledger && !errors.Is(ledgerErr, workspace.ErrNeverRead) {
			t.Fatalf("failed write changed ledger: %v", ledgerErr)
		}
		temps := 0
		walkErr := filepath.WalkDir(rootDir, func(_ string, entry os.DirEntry, walkErr error) error {
			if walkErr == nil && strings.HasPrefix(entry.Name(), ".plasmid-write-") {
				temps++
			}
			return walkErr
		})
		if walkErr != nil || temps != expected.TempFiles {
			t.Fatalf("temporary files = %d, %v; want %d", temps, walkErr, expected.TempFiles)
		}
	})
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
	fixture.WalkKinds(t, "tools", []string{"edit"}, func(t *testing.T, testCase fixture.Case) {
		var metadata schemaFixtureMetadata
		var input editHandlerFixtureInput
		var expected editHandlerFixtureExpected
		testCase.Decode(t, "case.json", &metadata)
		testCase.Decode(t, "input.json", &input)
		testCase.Decode(t, "expected.json", &expected)
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
		if got := editHandlerErrorCode(callErr); got != expected.ErrorCode {
			t.Fatalf("error classification = %q (%v), want %q", got, callErr, expected.ErrorCode)
		}
		if result.CallID != "fixture-call" {
			t.Fatalf("call id = %q", result.CallID)
		}
		if expected.OK {
			decoded := decodeEditResult(t, result.Content)
			if decoded.Path != "file.txt" || decoded.Replacements != expected.Replacements || decoded.MatchTier != expected.Tier || decoded.Diff != expected.Diff {
				t.Fatalf("result = %#v, fixture = %#v", decoded, expected)
			}
		} else if result.Content != nil {
			t.Fatalf("failure result = %#v, want nil", result.Content)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != expected.ResultContent && expected.OK {
			t.Fatalf("content = %q, want %q", got, expected.ResultContent)
		}
		if !expected.OK && string(got) != input.Content {
			t.Fatalf("failed edit changed content: %q", got)
		}
		if touches := len(observer.snapshot()); touches != boolToCount(expected.OK) {
			t.Fatalf("touches = %d, want %d", touches, boolToCount(expected.OK))
		}
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
	fixture.WalkKinds(t, "tools", []string{"bash", "grep", "find", "ls", "specifier"}, func(t *testing.T, testCase fixture.Case) {
		var metadata schemaFixtureMetadata
		var input behaviorFixtureInput
		testCase.Decode(t, "case.json", &metadata)
		testCase.Decode(t, "input.json", &input)
		if metadata.Area != "tools" || metadata.ID != testCase.ID || metadata.Kind != input.Tool {
			t.Fatalf("invalid metadata/input: %#v, %#v", metadata, input)
		}
		if metadata.Kind == "specifier" {
			var expected struct {
				Value string `json:"value"`
			}
			testCase.Decode(t, "expected.json", &expected)
			if got := ToolSpecifier(input.ToolName(), input.Args); got != expected.Value {
				t.Fatalf("specifier = %q, want %q", got, expected.Value)
			}
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
		var expected struct {
			ExitCode  int    `json:"exit_code"`
			Killed    bool   `json:"killed"`
			Signal    string `json:"signal"`
			Stderr    string `json:"stderr"`
			Stdout    string `json:"stdout"`
			TimedOut  bool   `json:"timed_out"`
			Truncated bool   `json:"truncated"`
		}
		testCase.Decode(t, "expected.json", &expected)
		got := decodeBashResult(t, content)
		if got.ExitCode != expected.ExitCode || got.Killed != expected.Killed || got.Signal != expected.Signal || got.Stderr != expected.Stderr || got.Stdout != expected.Stdout || got.TimedOut != expected.TimedOut || got.Truncated != expected.Truncated {
			t.Fatalf("bash result = %#v, want %#v", got, expected)
		}
	case "grep":
		var expected struct {
			Result  GrepResult `json:"result"`
			Touches []string   `json:"touches"`
		}
		testCase.Decode(t, "expected.json", &expected)
		var got GrepResult
		decodeFixtureObject(t, content, &got)
		if !reflect.DeepEqual(got, expected.Result) || !reflect.DeepEqual(touchPaths(touches), expected.Touches) {
			t.Fatalf("grep result/touches = %#v, %#v; want %#v", got, touchPaths(touches), expected)
		}
	case "find":
		var expected struct {
			Result  FindResult `json:"result"`
			Touches []string   `json:"touches"`
		}
		testCase.Decode(t, "expected.json", &expected)
		var got FindResult
		decodeFixtureObject(t, content, &got)
		if !reflect.DeepEqual(got, expected.Result) || !reflect.DeepEqual(touchPaths(touches), expected.Touches) {
			t.Fatalf("find result/touches = %#v, %#v; want %#v", got, touchPaths(touches), expected)
		}
	case "ls":
		var expected struct {
			Entries []struct {
				Path string `json:"path"`
				Type string `json:"type"`
			} `json:"entries"`
			Touches   []string `json:"touches"`
			Truncated bool     `json:"truncated"`
		}
		testCase.Decode(t, "expected.json", &expected)
		var got ListResult
		decodeFixtureObject(t, content, &got)
		entries := make([]struct {
			Path string `json:"path"`
			Type string `json:"type"`
		}, len(got.Entries))
		for index, entry := range got.Entries {
			entries[index].Path, entries[index].Type = entry.Path, entry.Type
		}
		if !reflect.DeepEqual(entries, expected.Entries) || got.Truncated != expected.Truncated || !reflect.DeepEqual(touchPaths(touches), expected.Touches) {
			t.Fatalf("ls result/touches = %#v, %#v; want %#v", got, touchPaths(touches), expected)
		}
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
