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

	"github.com/RandomCodeSpace/plasmid/codingtools/internal/textmatch"
	"github.com/RandomCodeSpace/plasmid/internal/fixture"
	"github.com/RandomCodeSpace/plasmid/outputlimit"
	"github.com/RandomCodeSpace/plasmid/shellexec"
	"github.com/RandomCodeSpace/plasmid/workspace"
)

const (
	toolsFixtureArea  = "tools"
	fixtureTargetFile = "file.txt"
	fixtureBashKind   = "bash"
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
	fixture.RegisterRunner(toolsFixtureArea, "codingtools/schema", "schema")
	fixture.RegisterRunner(toolsFixtureArea, "codingtools/write", "write")
	fixture.RegisterRunner(toolsFixtureArea, "codingtools/edit", "edit")
	fixture.RegisterRunner(toolsFixtureArea, "codingtools/read", "read")
	fixture.RegisterRunner(toolsFixtureArea, "codingtools/e02-behavior", fixtureBashKind, "grep", "find", "ls", "specifier")
}

func TestMain(m *testing.M) {
	os.Exit(fixture.Run(m))
}

func TestToolsFixtureCoverage(t *testing.T) {
	fixture.AssertCoverage(t, toolsFixtureArea)
}

func TestSchemaFixtures(t *testing.T) {
	fixture.WalkKinds(t, toolsFixtureArea, "codingtools/schema", []string{"schema"}, func(t *testing.T, testCase fixture.Case) {
		var metadata schemaFixtureMetadata
		var input schemaFixtureInput
		testCase.Decode(t, "case.json", &metadata)
		testCase.Decode(t, "input.json", &input)
		if metadata.Area != toolsFixtureArea || metadata.ID != testCase.ID || metadata.Kind != "schema" {
			t.Fatalf("invalid metadata: %#v", metadata)
		}
		accessors := map[string]schemaAccessor{
			"read": ReadInputSchema, "write": WriteInputSchema, "edit": EditInputSchema,
			fixtureBashKind: BashInputSchema, "grep": GrepInputSchema, "find": FindInputSchema,
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
	fixture.WalkKinds(t, toolsFixtureArea, "codingtools/write", []string{"write"}, func(t *testing.T, testCase fixture.Case) {
		runWriteFixture(t, testCase)
	})
}

type writeFixtureEnvironment struct {
	rootDir   string
	sessionID string
	ledger    *workspace.Ledger
	observer  *writeObserver
	handler   *writeHandler
}

func runWriteFixture(t *testing.T, testCase fixture.Case) {
	t.Helper()
	var metadata schemaFixtureMetadata
	var input writeFixtureInput
	testCase.Decode(t, "case.json", &metadata)
	testCase.Decode(t, "input.json", &input)
	assertFixtureMetadata(t, testCase, metadata, "write")
	environment := newWriteFixtureEnvironment(t, input)
	result, err := adaptTestHandler(t, environment.handler.call)(context.Background(), environment.sessionID, input.Args)
	actual := collectWriteFixtureResult(t, environment, input.Args, result, err)
	testCase.CompareJSON(t, "expected.json", actual, fixture.Paths{}, fixture.GoldenReadOnly)
	testCase.CompareJSON(t, "warnings.json", fixture.StableWarnings(nil), fixture.Paths{}, fixture.GoldenReadOnly)
}

func assertFixtureMetadata(t *testing.T, testCase fixture.Case, metadata schemaFixtureMetadata, kind string) {
	t.Helper()
	if metadata.Area != toolsFixtureArea || metadata.ID != testCase.ID || metadata.Kind != kind {
		t.Fatalf("invalid metadata: %#v", metadata)
	}
}

func newWriteFixtureEnvironment(t *testing.T, input writeFixtureInput) writeFixtureEnvironment {
	t.Helper()
	rootDir := t.TempDir()
	seedBehaviorFixture(t, rootDir, input.Files)
	root, err := workspace.NewRoot(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	ledger, bus := workspace.NewLedger(), workspace.NewTouchBus()
	observer := &writeObserver{}
	bus.Subscribe(observer)
	handler, err := newWriteHandler(Config{Root: root, Queue: workspace.NewMutationQueue(), Ledger: ledger, Touch: bus, Budget: outputlimit.NewBudget(10000)})
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
	return writeFixtureEnvironment{rootDir: rootDir, sessionID: sessionID, ledger: ledger, observer: observer, handler: handler}
}

func collectWriteFixtureResult(t *testing.T, environment writeFixtureEnvironment, args map[string]any, result map[string]any, callErr error) writeFixtureExpected {
	t.Helper()
	actual := writeFixtureExpected{Error: writeFixtureError(callErr), OK: callErr == nil}
	if callErr == nil {
		decoded := decodeWriteResult(t, result)
		actual.Result = &decoded
	} else if result != nil {
		t.Fatalf("failure result = %#v, want nil", result)
	}
	path, _ := args["path"].(string)
	actual.File, actual.FileExists = readOptionalFixtureFile(t, environment.rootDir, path)
	actual.Touches = len(environment.observer.snapshot())
	assertWriteFixtureLedger(t, environment, path, &actual)
	actual.TempFiles = countWriteFixtureTemps(t, environment.rootDir)
	return actual
}

func readOptionalFixtureFile(t *testing.T, rootDir, path string) (string, bool) {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(rootDir, filepath.FromSlash(path)))
	if errors.Is(err, os.ErrNotExist) {
		return "", false
	}
	if err != nil {
		t.Fatalf("check written file: %v", err)
	}
	return string(content), true
}

func assertWriteFixtureLedger(t *testing.T, environment writeFixtureEnvironment, path string, actual *writeFixtureExpected) {
	t.Helper()
	ledgerPath := path
	if actual.Result != nil {
		ledgerPath = actual.Result.Path
	}
	hash := sha256.Sum256([]byte(actual.File))
	err := environment.ledger.Verify(environment.sessionID, ledgerPath, int64(len(actual.File)), hash)
	actual.Ledger = err == nil
	if err != nil && !errors.Is(err, workspace.ErrNeverRead) {
		t.Fatalf("failed write changed ledger: %v", err)
	}
}

func countWriteFixtureTemps(t *testing.T, rootDir string) int {
	t.Helper()
	count := 0
	err := filepath.WalkDir(rootDir, func(_ string, entry os.DirEntry, walkErr error) error {
		if walkErr == nil && strings.HasPrefix(entry.Name(), ".plasmid-write-") {
			count++
		}
		return walkErr
	})
	if err != nil {
		t.Fatal(err)
	}
	return count
}

func writeFixtureError(err error) string {
	if err == nil {
		return ""
	}
	if strings.Contains(err.Error(), "content is required") || strings.Contains(err.Error(), "required") && strings.Contains(err.Error(), "content") {
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
	fixture.WalkKinds(t, toolsFixtureArea, "codingtools/edit", []string{"edit"}, func(t *testing.T, testCase fixture.Case) {
		runEditFixture(t, testCase)
	})
}

func runEditFixture(t *testing.T, testCase fixture.Case) {
	t.Helper()
	var metadata schemaFixtureMetadata
	var input editHandlerFixtureInput
	testCase.Decode(t, "case.json", &metadata)
	testCase.Decode(t, "input.json", &input)
	assertFixtureMetadata(t, testCase, metadata, "edit")
	rootDir, handler, observer := newEditFixtureEnvironment(t, input.Content)
	result, callErr := adaptTestHandler(t, handler.call)(context.Background(), "fixture", map[string]any{
		"path": fixtureTargetFile, "old_text": input.OldText, "new_text": input.NewText, "replace_all": input.ReplaceAll,
	})
	actual := collectEditFixtureResult(t, rootDir, input, result, callErr)
	if touches := len(observer.snapshot()); touches != boolToCount(callErr == nil) {
		t.Fatalf("touches = %d, want %d", touches, boolToCount(callErr == nil))
	}
	testCase.CompareJSON(t, "expected.json", actual, fixture.Paths{}, fixture.GoldenReadOnly)
}

func newEditFixtureEnvironment(t *testing.T, content string) (string, *editHandler, *writeObserver) {
	t.Helper()
	rootDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootDir, fixtureTargetFile), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := workspace.NewRoot(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	ledger, bus := workspace.NewLedger(), workspace.NewTouchBus()
	observer := &writeObserver{}
	bus.Subscribe(observer)
	handler, err := newEditHandler(Config{Root: root, Queue: workspace.NewMutationQueue(), Ledger: ledger, Touch: bus, Budget: outputlimit.NewBudget(10000)})
	if err != nil {
		t.Fatal(err)
	}
	ledger.RecordRead("fixture", fixtureTargetFile, int64(len(content)), sha256.Sum256([]byte(content)))
	return rootDir, handler, observer
}

func collectEditFixtureResult(t *testing.T, rootDir string, input editHandlerFixtureInput, result map[string]any, callErr error) editHandlerFixtureExpected {
	t.Helper()
	actual := editHandlerFixtureExpected{AmbiguityLines: []int{}, ErrorCode: editHandlerErrorCode(callErr), OK: callErr == nil}
	if callErr == nil {
		decoded := decodeEditResult(t, result)
		if decoded.Path != fixtureTargetFile {
			t.Fatalf("result path = %q, want file.txt", decoded.Path)
		}
		actual.Diff, actual.Replacements, actual.Tier = decoded.Diff, decoded.Replacements, decoded.MatchTier
	} else if result != nil {
		t.Fatalf("failure result = %#v, want nil", result)
	} else {
		var ambiguity *textmatch.AmbiguityError
		if errors.As(callErr, &ambiguity) {
			actual.AmbiguityLines = ambiguity.Lines
		}
	}
	content, err := os.ReadFile(filepath.Join(rootDir, fixtureTargetFile))
	if err != nil {
		t.Fatal(err)
	}
	if callErr == nil {
		actual.ResultContent = string(content)
	} else if string(content) != input.Content {
		t.Fatalf("failed edit changed content: %q", content)
	}
	return actual
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
	kinds := []string{fixtureBashKind, "grep", "find", "ls", "specifier"}
	fixture.WalkKinds(t, toolsFixtureArea, "codingtools/e02-behavior", kinds, func(t *testing.T, testCase fixture.Case) {
		runBehaviorFixture(t, testCase)
	})
}

func runBehaviorFixture(t *testing.T, testCase fixture.Case) {
	t.Helper()
	var metadata schemaFixtureMetadata
	var input behaviorFixtureInput
	testCase.Decode(t, "case.json", &metadata)
	testCase.Decode(t, "input.json", &input)
	if metadata.Area != toolsFixtureArea || metadata.ID != testCase.ID || metadata.Kind != input.Tool {
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
	invoke := newBehaviorFixtureHandler(t, metadata.Kind, Config{Root: root, Touch: bus, Budget: outputlimit.NewBudget(100000), Output: outputlimit.Defaults()})
	result, err := invoke(context.Background(), input.Session, input.Args)
	if err != nil {
		t.Fatal(err)
	}
	assertBehaviorFixture(t, testCase, metadata.Kind, result, observer.snapshot())
}

func newBehaviorFixtureHandler(t *testing.T, kind string, cfg Config) testNativeHandler {
	t.Helper()
	var invoke testNativeHandler
	var err error
	switch kind {
	case fixtureBashKind:
		shell, shellErr := shellexec.New(shellexec.Config{Root: cfg.Root, Shell: "sh", OutputLimit: cfg.Output})
		if shellErr != nil {
			t.Skipf("fixture shell unavailable: %v", shellErr)
		}
		cfg.Shell = shell
		var handler *bashHandler
		handler, err = newBashHandler(cfg)
		if err == nil {
			invoke = adaptTestHandler(t, handler.call)
		}
	case "grep":
		var handler *grepHandler
		handler, err = newGrepHandler(cfg)
		if err == nil {
			invoke = adaptTestHandler(t, handler.call)
		}
	case "find":
		var handler *findHandler
		handler, err = newFindHandler(cfg)
		if err == nil {
			invoke = adaptTestHandler(t, handler.call)
		}
	case "ls":
		var handler *listHandler
		handler, err = newListHandler(cfg)
		if err == nil {
			invoke = adaptTestHandler(t, handler.call)
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	return invoke
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
	case fixtureBashKind:
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
