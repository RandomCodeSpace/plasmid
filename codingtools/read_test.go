package codingtools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/plasmid-dev/plasmid/internal/fixture"
	"github.com/plasmid-dev/plasmid/outputlimit"
	"github.com/plasmid-dev/plasmid/workspace"
)

type readObserver struct {
	mu      sync.Mutex
	touches []workspace.Touch
}

func (o *readObserver) ObserveTouch(_ context.Context, touch workspace.Touch) {
	o.mu.Lock()
	o.touches = append(o.touches, touch)
	o.mu.Unlock()
}

func (o *readObserver) snapshot() []workspace.Touch {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]workspace.Touch(nil), o.touches...)
}

type readHarness struct {
	tool     testNativeHandler
	root     *workspace.Root
	ledger   *workspace.Ledger
	budget   *outputlimit.Budget
	observer *readObserver
}

func newReadHarness(t *testing.T, rootDir string, configure func(*Config)) readHarness {
	t.Helper()
	root, err := workspace.NewRoot(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	ledger := workspace.NewLedger()
	touch := workspace.NewTouchBus()
	observer := &readObserver{}
	touch.Subscribe(observer)
	budget := outputlimit.NewBudget(outputlimit.DefaultPerSession)
	cfg := Config{Root: root, Ledger: ledger, Touch: touch, Budget: budget}
	if configure != nil {
		configure(&cfg)
	}
	tool, err := newReadHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return readHarness{tool: adaptTestHandler(t, tool.call), root: root, ledger: ledger, budget: cfg.Budget, observer: observer}
}

func TestNewReadToolContractAndDependencies(t *testing.T) {
	rootDir := t.TempDir()
	root, err := workspace.NewRoot(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	valid := Config{
		Root: root, Ledger: workspace.NewLedger(), Touch: workspace.NewTouchBus(),
		Budget: outputlimit.NewBudget(10000),
	}
	for _, test := range []struct {
		name   string
		mutate func(*Config)
	}{
		{"nil root", func(cfg *Config) { cfg.Root = nil }},
		{"nil ledger", func(cfg *Config) { cfg.Ledger = nil }},
		{"nil touch", func(cfg *Config) { cfg.Touch = nil }},
		{"nil budget", func(cfg *Config) { cfg.Budget = nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := valid
			test.mutate(&cfg)
			if _, err := newReadHandler(cfg); err == nil {
				t.Fatal("constructor accepted a missing dependency")
			}
		})
	}
	tool, err := NewReadTool(valid)
	if err != nil {
		t.Fatal(err)
	}
	if tool.Name() != readToolName || tool.Description() != ReadDescription || tool.IsLongRunning() {
		t.Fatalf("tool metadata drifted: %q, %q, long-running=%t", tool.Name(), tool.Description(), tool.IsLongRunning())
	}
	first := ReadInputSchema()
	first[0] ^= 0xff
	if bytes.Equal(first, ReadInputSchema()) {
		t.Fatal("input schema aliases tool state")
	}
	if _, err := newReadHandler(Config{
		Root: root, Ledger: workspace.NewLedger(), Touch: workspace.NewTouchBus(),
		Budget: outputlimit.NewBudget(10000), Output: outputlimit.Policy{MaxBytes: -1, MaxLines: 1},
	}); !errors.Is(err, outputlimit.ErrInvalidLimit) {
		t.Fatalf("negative output bytes error = %v", err)
	}
}

func TestRenderReadWindowUsesExactPrefixes(t *testing.T) {
	got, err := renderReadWindow(context.Background(), []readLine{{body: ""}, {body: strings.Repeat("x", 4096)}}, 1000000)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("%6d\t\n%6d\t%s", 1000000, 1000001, strings.Repeat("x", 4096))
	if got != want {
		t.Fatalf("rendered prefix or long line drifted")
	}
}

func TestReadBinaryClassification(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want bool
	}{
		{name: "empty"},
		{name: "source controls", data: []byte("alpha\tbeta\r\ngamma")},
		{name: "unicode and combining", data: []byte("🙂 e\u0301 終")},
		{name: "exactly thirty percent", data: []byte{1, 2, 3, 'a', 'b', 'c', 'd', 'e', 'f', 'g'}},
		{name: "over thirty percent", data: []byte{1, 2, 3, 4, 'a', 'b', 'c', 'd', 'e', 'f'}, want: true},
		{name: "early nul", data: []byte{'a', 0, 'b'}, want: true},
		{name: "invalid utf8", data: []byte{0xff, 'a'}, want: true},
		{name: "nul after probe", data: append(append(bytes.Repeat([]byte{'a'}, 8000), 0), bytes.Repeat([]byte{'b'}, 10)...)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isBinaryText(test.data); got != test.want {
				t.Fatalf("isBinaryText() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestReadAcceptsExactMaximumSize(t *testing.T) {
	rootDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootDir, "file.txt"), []byte("four"), 0o644); err != nil {
		t.Fatal(err)
	}
	harness := newReadHarness(t, rootDir, func(cfg *Config) { cfg.MaxReadBytes = 4 })
	result, err := harness.tool(context.Background(), "session", map[string]any{"path": "file.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeReadResult(t, result).Content; got != "     1\tfour" {
		t.Fatalf("content = %q", got)
	}
}

type ledgerCheckingObserver struct {
	ledger *workspace.Ledger
	hash   [sha256.Size]byte
	size   int64
	seen   bool
	err    error
}

func (o *ledgerCheckingObserver) ObserveTouch(_ context.Context, touch workspace.Touch) {
	o.err = o.ledger.Verify(touch.SessionID, touch.Path, o.size, o.hash)
	o.seen = true
}

func TestReadRecordsLedgerBeforeTouch(t *testing.T) {
	rootDir := t.TempDir()
	data := []byte("text")
	if err := os.WriteFile(filepath.Join(rootDir, "file.txt"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := workspace.NewRoot(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	ledger := workspace.NewLedger()
	touch := workspace.NewTouchBus()
	observer := &ledgerCheckingObserver{ledger: ledger, hash: sha256.Sum256(data), size: int64(len(data))}
	touch.Subscribe(observer)
	tool, err := newReadHandler(Config{Root: root, Ledger: ledger, Touch: touch, Budget: outputlimit.NewBudget(10000)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adaptTestHandler(t, tool.call)(context.Background(), "session", map[string]any{"path": "file.txt"}); err != nil {
		t.Fatal(err)
	}
	if !observer.seen || observer.err != nil {
		t.Fatalf("observer saw ledger state = %t, %v", observer.seen, observer.err)
	}
}

type readLineCase struct {
	name      string
	data      string
	args      map[string]any
	content   string
	start     int
	end       int
	total     int
	truncated bool
}

func TestReadLineSemantics(t *testing.T) {
	tests := []readLineCase{
		{name: "empty"},
		{name: "no newline", data: "alpha", content: "     1\talpha", start: 1, end: 1, total: 1},
		{name: "trailing newline", data: "alpha\n", content: "     1\talpha\n", start: 1, end: 1, total: 1},
		{name: "two trailing newlines", data: "alpha\n\n", content: "     1\talpha\n     2\t\n", start: 1, end: 2, total: 2},
		{name: "crlf", data: "alpha\r\nbeta\r\n", content: "     1\talpha\n     2\tbeta\n", start: 1, end: 2, total: 2},
		{name: "unicode", data: "🙂\n終", content: "     1\t🙂\n     2\t終", start: 1, end: 2, total: 2},
		{name: "window", data: "a\nb\nc", args: map[string]any{"limit": 1, "offset": 2}, content: "     2\tb\n", start: 2, end: 2, total: 3, truncated: true},
		{name: "offset beyond eof", data: "a\nb", args: map[string]any{"offset": math.MaxInt}, total: 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertReadLineCase(t, test)
		})
	}
}

func assertReadLineCase(t *testing.T, test readLineCase) {
	t.Helper()
	rootDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootDir, "file.txt"), []byte(test.data), 0o644); err != nil {
		t.Fatal(err)
	}
	harness := newReadHarness(t, rootDir, nil)
	args := map[string]any{"path": "file.txt"}
	for key, value := range test.args {
		args[key] = value
	}
	got, err := harness.tool(context.Background(), "session", args)
	if err != nil {
		t.Fatal(err)
	}
	decoded := decodeReadResult(t, got)
	if decoded.Content != test.content || decoded.StartLine != test.start || decoded.EndLine != test.end || decoded.TotalLines != test.total || decoded.Truncated != test.truncated {
		t.Fatalf("result = %#v", decoded)
	}
	if decoded.Report.OriginalBytes != len(test.content) || decoded.Report.OriginalLines != len(splitReadLines([]byte(test.data))[maxZero(test.start-1):maxZero(test.end)]) {
		t.Fatalf("report = %#v", decoded.Report)
	}
	wantHash := sha256.Sum256([]byte(test.data))
	if err := harness.ledger.Verify("session", "file.txt", int64(len(test.data)), wantHash); err != nil {
		t.Fatalf("full-file ledger state: %v", err)
	}
	touches := harness.observer.snapshot()
	if len(touches) != 1 || touches[0].SessionID != "session" || touches[0].Path != "file.txt" || touches[0].Kind != workspace.TouchRead {
		t.Fatalf("touches = %#v", touches)
	}
}

func TestReadRejectsInvalidFilesWithoutSideEffects(t *testing.T) {
	base := t.TempDir()
	rootDir := filepath.Join(base, "root")
	if err := os.Mkdir(rootDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootDir, "binary-nul"), []byte{'a', 0, 'b'}, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootDir, "binary-ratio"), []byte{1, 2, 3, 'a'}, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootDir, "invalid-utf8"), []byte{0xff, 'a'}, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootDir, "large"), []byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(rootDir, "directory"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "outside")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	harness := newReadHarness(t, rootDir, func(cfg *Config) { cfg.MaxReadBytes = 4 })
	tests := []struct {
		path string
		want error
	}{
		{"binary-nul", ErrBinaryFile},
		{"binary-ratio", ErrBinaryFile},
		{"invalid-utf8", ErrBinaryFile},
		{"large", ErrFileTooLarge},
		{"directory", ErrIsDirectory},
		{"missing", ErrFileNotFound},
		{"../outside", ErrPathOutsideRoot},
	}
	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			assertInvalidRead(t, harness, base, test.path, test.want)
		})
	}
	if touches := harness.observer.snapshot(); len(touches) != 0 {
		t.Fatalf("failed reads published touches: %#v", touches)
	}
}

func assertInvalidRead(t *testing.T, harness readHarness, base, path string, want error) {
	t.Helper()
	result, err := harness.tool(context.Background(), path, map[string]any{"path": path})
	if !errors.Is(err, want) || result != nil {
		t.Fatalf("Call() = %#v, %v; want %v", result, err, want)
	}
	if filepath.IsAbs(err.Error()) || strings.Contains(err.Error(), base) {
		t.Fatalf("error exposed an absolute path: %v", err)
	}
	if err := harness.ledger.Verify(path, path, 0, [sha256.Size]byte{}); !errors.Is(err, workspace.ErrNeverRead) {
		t.Fatalf("failure changed ledger: %v", err)
	}
}

func TestReadOutputPolicyAndBudget(t *testing.T) {
	rootDir := t.TempDir()
	data := "alpha\nbeta\ngamma"
	if err := os.WriteFile(filepath.Join(rootDir, "file.txt"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Run("configured output", func(t *testing.T) {
		assertConfiguredReadOutput(t, rootDir)
	})
	t.Run("budget", func(t *testing.T) {
		assertReadBudget(t, rootDir)
	})
	t.Run("zero grant", func(t *testing.T) {
		assertZeroReadGrant(t, rootDir)
	})
}

func assertConfiguredReadOutput(t *testing.T, rootDir string) {
	t.Helper()
	harness := newReadHarness(t, rootDir, func(cfg *Config) {
		cfg.Output = outputlimit.Policy{MaxBytes: 12, MaxLines: 10, MaxLineBytes: 100, HeadFraction: 0.5}
	})
	result, err := harness.tool(context.Background(), "output", map[string]any{"path": "file.txt"})
	if err != nil {
		t.Fatal(err)
	}
	decoded := decodeReadResult(t, result)
	if !decoded.Truncated || !decoded.Report.Truncated || decoded.Report.Reason != outputlimit.ReasonBytes || !strings.Contains(decoded.Content, "reason=bytes") {
		t.Fatalf("configured output result = %#v", decoded)
	}
}

func assertReadBudget(t *testing.T, rootDir string) {
	t.Helper()
	harness := newReadHarness(t, rootDir, func(cfg *Config) {
		cfg.Output = outputlimit.Policy{MaxBytes: 100, MaxLines: 10, MaxLineBytes: 100, HeadFraction: 0.5}
		cfg.Budget = outputlimit.NewBudget(20)
	})
	result, err := harness.tool(context.Background(), "budget", map[string]any{"path": "file.txt"})
	if err != nil {
		t.Fatal(err)
	}
	decoded := decodeReadResult(t, result)
	if !decoded.Truncated || decoded.Report.Reason != outputlimit.ReasonBudget || !strings.Contains(decoded.Content, "reason=budget") {
		t.Fatalf("budget result = %#v", decoded)
	}
	if used, limit := harness.budget.Report("budget"); used != 20 || limit != 20 {
		t.Fatalf("budget report = %d/%d, want 20/20", used, limit)
	}
}

func assertZeroReadGrant(t *testing.T, rootDir string) {
	t.Helper()
	budget := outputlimit.NewBudget(1)
	first := budget.Reserve("zero", 1)
	budget.Consume("zero", first.ID, first.Grant)
	harness := newReadHarness(t, rootDir, func(cfg *Config) {
		cfg.Output = outputlimit.Policy{MaxBytes: 100, MaxLines: 10, MaxLineBytes: 100, HeadFraction: 0.5}
		cfg.Budget = budget
	})
	result, err := harness.tool(context.Background(), "zero", map[string]any{"path": "file.txt"})
	if err != nil {
		t.Fatal(err)
	}
	decoded := decodeReadResult(t, result)
	if decoded.Report.Reason != outputlimit.ReasonBudget || decoded.Report.KeptBytes != 0 || strings.Contains(decoded.Content, "alpha") {
		t.Fatalf("zero-grant result = %#v", decoded)
	}
}

func TestReadSettlesFailuresAndHonorsCancellation(t *testing.T) {
	rootDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootDir, "file.txt"), []byte("text"), 0o644); err != nil {
		t.Fatal(err)
	}
	harness := newReadHarness(t, rootDir, func(cfg *Config) { cfg.Budget = outputlimit.NewBudget(10000) })
	result, err := harness.tool(context.Background(), "session", map[string]any{})
	if err == nil || result != nil {
		t.Fatalf("argument failure = %#v, %v", result, err)
	}
	if grant := harness.budget.Reserve("session", 10000); grant.Grant != 5000 {
		t.Fatalf("failed call leaked its reservation; next grant = %#v", grant)
	} else {
		harness.budget.Consume("session", grant.ID, 0)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	result, err = harness.tool(cancelled, "cancelled", map[string]any{"path": "file.txt"})
	if !errors.Is(err, context.Canceled) || result != nil {
		t.Fatalf("cancelled call = %#v, %v", result, err)
	}
	if touches := harness.observer.snapshot(); len(touches) != 0 {
		t.Fatalf("failures published touches: %#v", touches)
	}
	if _, err := renderReadWindow(cancelled, []readLine{{body: "a"}}, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("render cancellation = %v", err)
	}
	if _, _, err := readCompleteFile(cancelled, filepath.Join(rootDir, "file.txt"), 100); !errors.Is(err, context.Canceled) {
		t.Fatalf("read cancellation = %v", err)
	}
}

func TestReadResultMapsAreFresh(t *testing.T) {
	rootDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootDir, "file.txt"), []byte("text"), 0o644); err != nil {
		t.Fatal(err)
	}
	harness := newReadHarness(t, rootDir, nil)
	args := map[string]any{"path": "file.txt"}
	first, err := harness.tool(context.Background(), "session", args)
	if err != nil {
		t.Fatal(err)
	}
	first["content"] = "mutated"
	first["report"].(map[string]any)["reason"] = "mutated"
	second, err := harness.tool(context.Background(), "session", args)
	if err != nil {
		t.Fatal(err)
	}
	decoded := decodeReadResult(t, second)
	if decoded.Content != "     1\ttext" || decoded.Report.Reason != "" {
		t.Fatalf("caller mutation leaked into later result: %#v", decoded)
	}
}

func TestReadConcurrentCalls(t *testing.T) {
	rootDir := t.TempDir()
	data := []byte("alpha\nbeta\n")
	if err := os.WriteFile(filepath.Join(rootDir, "file.txt"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	harness := newReadHarness(t, rootDir, nil)
	const calls = 64
	var group sync.WaitGroup
	errorsSeen := make(chan error, calls)
	for index := range calls {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			result, err := harness.tool(context.Background(), "session", map[string]any{"path": "file.txt"})
			if err == nil && result == nil {
				err = errors.New("nil result content")
			}
			errorsSeen <- err
		}(index)
	}
	group.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
	wantHash := sha256.Sum256(data)
	if err := harness.ledger.Verify("session", "file.txt", int64(len(data)), wantHash); err != nil {
		t.Fatal(err)
	}
	if touches := harness.observer.snapshot(); len(touches) != calls {
		t.Fatalf("touch count = %d, want %d", len(touches), calls)
	}
}

func decodeReadResult(t *testing.T, object map[string]any) ReadResult {
	t.Helper()
	encoded, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	var result ReadResult
	if err := json.Unmarshal(encoded, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func maxZero(value int) int {
	if value < 0 {
		return 0
	}
	return value
}

type readFixtureInput struct {
	Args         json.RawMessage    `json:"args"`
	Budget       int                `json:"budget"`
	Cancel       bool               `json:"cancel"`
	File         string             `json:"file"`
	MaxReadBytes int64              `json:"max_read_bytes"`
	Output       outputlimit.Policy `json:"output"`
	Path         string             `json:"path"`
	Setup        string             `json:"setup"`
}

type readFixtureExpected struct {
	Error   string      `json:"error"`
	Ledger  bool        `json:"ledger"`
	Result  *ReadResult `json:"result"`
	Touches int         `json:"touches"`
}

func TestReadFixtures(t *testing.T) {
	fixture.WalkKinds(t, "tools", "codingtools/read", []string{readToolName}, func(t *testing.T, testCase fixture.Case) {
		runReadFixture(t, testCase)
	})
}

func runReadFixture(t *testing.T, testCase fixture.Case) {
	t.Helper()
	var metadata schemaFixtureMetadata
	var input readFixtureInput
	testCase.Decode(t, "case.json", &metadata)
	testCase.Decode(t, "input.json", &input)
	assertFixtureMetadata(t, testCase, metadata, readToolName)
	rootDir := prepareReadFixture(t, input)
	budgetLimit := input.Budget
	if budgetLimit == 0 {
		budgetLimit = outputlimit.DefaultPerSession
	}
	harness := newReadHarness(t, rootDir, func(cfg *Config) {
		cfg.Budget = outputlimit.NewBudget(budgetLimit)
		cfg.MaxReadBytes = input.MaxReadBytes
		cfg.Output = input.Output
	})
	result, callErr := harness.tool(readFixtureContext(input.Cancel), "fixture-session", decodeFixtureArguments(t, input.Args))
	actual := collectReadFixtureResult(t, harness, input, result, callErr)
	testCase.CompareJSON(t, "expected.json", actual, fixture.Paths{}, fixture.GoldenReadOnly)
}

func prepareReadFixture(t *testing.T, input readFixtureInput) string {
	t.Helper()
	base := t.TempDir()
	rootDir := filepath.Join(base, "root")
	if err := os.Mkdir(rootDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var err error
	switch input.Setup {
	case "file":
		err = os.WriteFile(filepath.Join(rootDir, input.Path), []byte(input.File), 0o644)
	case "directory":
		err = os.Mkdir(filepath.Join(rootDir, input.Path), 0o755)
	case "missing":
	case "escape":
		err = os.WriteFile(filepath.Join(base, "outside.txt"), []byte(input.File), 0o644)
	default:
		t.Fatalf("unknown setup %q", input.Setup)
	}
	if err != nil {
		t.Fatal(err)
	}
	return rootDir
}

func readFixtureContext(cancelled bool) context.Context {
	ctx := context.Background()
	if !cancelled {
		return ctx
	}
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	return ctx
}

func collectReadFixtureResult(t *testing.T, harness readHarness, input readFixtureInput, result map[string]any, callErr error) readFixtureExpected {
	t.Helper()
	actual := readFixtureExpected{Error: readFixtureError(t, callErr)}
	if result == nil && callErr == nil {
		t.Fatal("successful fixture returned nil result")
	}
	if result != nil {
		got := decodeReadResult(t, result)
		actual.Result = &got
	}
	path := input.Path
	if input.Setup == "escape" {
		path = "../outside.txt"
	}
	hash := sha256.Sum256([]byte(input.File))
	ledgerErr := harness.ledger.Verify("fixture-session", path, int64(len(input.File)), hash)
	actual.Ledger = ledgerErr == nil
	if ledgerErr != nil && !errors.Is(ledgerErr, workspace.ErrNeverRead) {
		t.Fatalf("failure changed ledger: %v", ledgerErr)
	}
	actual.Touches = len(harness.observer.snapshot())
	return actual
}

func decodeFixtureArguments(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var args map[string]any
	if err := decoder.Decode(&args); err != nil {
		t.Fatal(err)
	}
	return args
}

func readFixtureError(t *testing.T, err error) string {
	t.Helper()
	for _, classification := range []struct {
		name string
		err  error
	}{
		{name: "binary", err: ErrBinaryFile},
		{name: "cancelled", err: context.Canceled},
		{name: "directory", err: ErrIsDirectory},
		{name: "missing", err: ErrFileNotFound},
		{name: "outside_root", err: ErrPathOutsideRoot},
		{name: "too_large", err: ErrFileTooLarge},
	} {
		if errors.Is(err, classification.err) {
			return classification.name
		}
	}
	if err == nil {
		return ""
	}
	if strings.Contains(err.Error(), "read arguments") || strings.Contains(err.Error(), "validating root") {
		return "arguments"
	}
	t.Fatalf("unknown read fixture error: %v", err)
	return ""
}
