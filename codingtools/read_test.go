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
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/plasmid-dev/plasmid/internal/fixture"
	"github.com/plasmid-dev/plasmid/loop"
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
	tool     loop.Tool
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
	tool, err := NewReadTool(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return readHarness{tool: tool, root: root, ledger: ledger, budget: cfg.Budget, observer: observer}
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
			if _, err := NewReadTool(cfg); err == nil {
				t.Fatal("constructor accepted a missing dependency")
			}
		})
	}
	tool, err := NewReadTool(valid)
	if err != nil {
		t.Fatal(err)
	}
	if tool.Name() != "read" || tool.Description() != ReadDescription || !bytes.Equal(tool.InputSchema(), ReadInputSchema()) {
		t.Fatalf("tool metadata drifted: %q, %q, %s", tool.Name(), tool.Description(), tool.InputSchema())
	}
	first := tool.InputSchema()
	first[0] ^= 0xff
	if bytes.Equal(first, tool.InputSchema()) {
		t.Fatal("input schema aliases tool state")
	}
	if _, err := NewReadTool(Config{
		Root: root, Ledger: workspace.NewLedger(), Touch: workspace.NewTouchBus(),
		Budget: outputlimit.NewBudget(10000), Output: outputlimit.Policy{MaxBytes: -1, MaxLines: 1},
	}); !errors.Is(err, outputlimit.ErrInvalidLimit) {
		t.Fatalf("negative output bytes error = %v", err)
	}
}

func TestDecodeReadArgsStrict(t *testing.T) {
	maximum := int64(math.MaxInt)
	tests := []struct {
		name string
		args map[string]any
		want ReadArgs
		err  string
	}{
		{name: "defaults", args: map[string]any{"path": "file.txt"}, want: ReadArgs{Path: "file.txt", Offset: 1, Limit: 17}},
		{name: "numbers", args: map[string]any{"limit": json.Number("3"), "offset": json.Number("2"), "path": "file.txt"}, want: ReadArgs{Path: "file.txt", Offset: 2, Limit: 3}},
		{name: "nil object", err: "object"},
		{name: "missing path", args: map[string]any{}, err: "required"},
		{name: "empty path", args: map[string]any{"path": ""}, err: "must not be empty"},
		{name: "path number", args: map[string]any{"path": json.Number("1")}, err: "string"},
		{name: "unknown", args: map[string]any{"extra": true, "path": "file.txt"}, err: "unknown"},
		{name: "decoded JSON integer", args: map[string]any{"offset": float64(2), "path": "file.txt"}, want: ReadArgs{Path: "file.txt", Offset: 2, Limit: 17}},
		{name: "decoded JSON fraction", args: map[string]any{"offset": float64(1.5), "path": "file.txt"}, err: "integer"},
		{name: "nan", args: map[string]any{"offset": math.NaN(), "path": "file.txt"}, err: "valid JSON"},
		{name: "infinity", args: map[string]any{"offset": math.Inf(1), "path": "file.txt"}, err: "valid JSON"},
		{name: "fraction", args: map[string]any{"offset": json.Number("1.5"), "path": "file.txt"}, err: "integer"},
		{name: "exponent", args: map[string]any{"offset": json.Number("1e2"), "path": "file.txt"}, err: "integer"},
		{name: "string integer", args: map[string]any{"limit": "2", "path": "file.txt"}, err: "integer"},
		{name: "boolean integer", args: map[string]any{"offset": true, "path": "file.txt"}, err: "integer"},
		{name: "zero", args: map[string]any{"limit": 0, "path": "file.txt"}, err: "at least 1"},
		{name: "negative", args: map[string]any{"offset": -1, "path": "file.txt"}, err: "at least 1"},
		{name: "over platform int", args: map[string]any{"limit": json.Number(strconv.FormatUint(uint64(math.MaxInt)+1, 10)), "path": "file.txt"}, err: "int range"},
		{name: "over int", args: map[string]any{"limit": json.Number("9223372036854775808"), "path": "file.txt"}, err: "int range"},
	}
	if maximum == math.MaxInt64 {
		tests = append(tests, struct {
			name string
			args map[string]any
			want ReadArgs
			err  string
		}{name: "platform maximum", args: map[string]any{"limit": json.Number("9223372036854775807"), "path": "file.txt"}, want: ReadArgs{Path: "file.txt", Offset: 1, Limit: math.MaxInt}})
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := decodeReadArgs(test.args, 17)
			if test.err != "" {
				if err == nil || !strings.Contains(err.Error(), test.err) {
					t.Fatalf("error = %v, want substring %q", err, test.err)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("decodeReadArgs() = %#v, %v; want %#v, nil", got, err, test.want)
			}
		})
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
	result, err := harness.tool.Call(context.Background(), loop.ToolCall{SessionID: "session", Args: map[string]any{"path": "file.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeReadResult(t, result.Content).Content; got != "     1\tfour" {
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
	tool, err := NewReadTool(Config{Root: root, Ledger: ledger, Touch: touch, Budget: outputlimit.NewBudget(10000)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Call(context.Background(), loop.ToolCall{SessionID: "session", Args: map[string]any{"path": "file.txt"}}); err != nil {
		t.Fatal(err)
	}
	if !observer.seen || observer.err != nil {
		t.Fatalf("observer saw ledger state = %t, %v", observer.seen, observer.err)
	}
}

func TestReadLineSemantics(t *testing.T) {
	tests := []struct {
		name      string
		data      string
		args      map[string]any
		content   string
		start     int
		end       int
		total     int
		truncated bool
	}{
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
			rootDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(rootDir, "file.txt"), []byte(test.data), 0o644); err != nil {
				t.Fatal(err)
			}
			harness := newReadHarness(t, rootDir, nil)
			args := map[string]any{"path": "file.txt"}
			for key, value := range test.args {
				args[key] = value
			}
			got, err := harness.tool.Call(context.Background(), loop.ToolCall{ID: "call", SessionID: "session", Args: args})
			if err != nil {
				t.Fatal(err)
			}
			decoded := decodeReadResult(t, got.Content)
			if got.CallID != "call" || got.IsError || decoded.Content != test.content || decoded.StartLine != test.start || decoded.EndLine != test.end || decoded.TotalLines != test.total || decoded.Truncated != test.truncated {
				t.Fatalf("result = %#v, envelope = %#v", decoded, got)
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
		})
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
			result, err := harness.tool.Call(context.Background(), loop.ToolCall{ID: "failed", SessionID: test.path, Args: map[string]any{"path": test.path}})
			if !errors.Is(err, test.want) || result.CallID != "failed" || result.Content != nil {
				t.Fatalf("Call() = %#v, %v; want %v", result, err, test.want)
			}
			if filepath.IsAbs(err.Error()) || strings.Contains(err.Error(), base) {
				t.Fatalf("error exposed an absolute path: %v", err)
			}
			if err := harness.ledger.Verify(test.path, test.path, 0, [sha256.Size]byte{}); !errors.Is(err, workspace.ErrNeverRead) {
				t.Fatalf("failure changed ledger: %v", err)
			}
		})
	}
	if touches := harness.observer.snapshot(); len(touches) != 0 {
		t.Fatalf("failed reads published touches: %#v", touches)
	}
}

func TestReadOutputPolicyAndBudget(t *testing.T) {
	rootDir := t.TempDir()
	data := "alpha\nbeta\ngamma"
	if err := os.WriteFile(filepath.Join(rootDir, "file.txt"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Run("configured output", func(t *testing.T) {
		harness := newReadHarness(t, rootDir, func(cfg *Config) {
			cfg.Output = outputlimit.Policy{MaxBytes: 12, MaxLines: 10, MaxLineBytes: 100, HeadFraction: 0.5}
		})
		result, err := harness.tool.Call(context.Background(), loop.ToolCall{SessionID: "output", Args: map[string]any{"path": "file.txt"}})
		if err != nil {
			t.Fatal(err)
		}
		decoded := decodeReadResult(t, result.Content)
		if !decoded.Truncated || !decoded.Report.Truncated || decoded.Report.Reason != outputlimit.ReasonBytes || !strings.Contains(decoded.Content, "reason=bytes") {
			t.Fatalf("configured output result = %#v", decoded)
		}
	})
	t.Run("budget", func(t *testing.T) {
		harness := newReadHarness(t, rootDir, func(cfg *Config) {
			cfg.Output = outputlimit.Policy{MaxBytes: 100, MaxLines: 10, MaxLineBytes: 100, HeadFraction: 0.5}
			cfg.Budget = outputlimit.NewBudget(20)
		})
		result, err := harness.tool.Call(context.Background(), loop.ToolCall{SessionID: "budget", Args: map[string]any{"path": "file.txt"}})
		if err != nil {
			t.Fatal(err)
		}
		decoded := decodeReadResult(t, result.Content)
		if !decoded.Truncated || decoded.Report.Reason != outputlimit.ReasonBudget || !strings.Contains(decoded.Content, "reason=budget") {
			t.Fatalf("budget result = %#v", decoded)
		}
		if used, limit := harness.budget.Report("budget"); used != 20 || limit != 20 {
			t.Fatalf("budget report = %d/%d, want 20/20", used, limit)
		}
	})
	t.Run("zero grant", func(t *testing.T) {
		budget := outputlimit.NewBudget(1)
		first := budget.Reserve("zero", 1)
		budget.Consume("zero", first.ID, first.Grant)
		harness := newReadHarness(t, rootDir, func(cfg *Config) {
			cfg.Output = outputlimit.Policy{MaxBytes: 100, MaxLines: 10, MaxLineBytes: 100, HeadFraction: 0.5}
			cfg.Budget = budget
		})
		result, err := harness.tool.Call(context.Background(), loop.ToolCall{SessionID: "zero", Args: map[string]any{"path": "file.txt"}})
		if err != nil {
			t.Fatal(err)
		}
		decoded := decodeReadResult(t, result.Content)
		if decoded.Report.Reason != outputlimit.ReasonBudget || decoded.Report.KeptBytes != 0 || strings.Contains(decoded.Content, "alpha") {
			t.Fatalf("zero-grant result = %#v", decoded)
		}
	})
}

func TestReadSettlesFailuresAndHonorsCancellation(t *testing.T) {
	rootDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootDir, "file.txt"), []byte("text"), 0o644); err != nil {
		t.Fatal(err)
	}
	harness := newReadHarness(t, rootDir, func(cfg *Config) { cfg.Budget = outputlimit.NewBudget(10000) })
	result, err := harness.tool.Call(context.Background(), loop.ToolCall{ID: "bad", SessionID: "session", Args: map[string]any{}})
	if err == nil || result.CallID != "bad" || result.Content != nil {
		t.Fatalf("argument failure = %#v, %v", result, err)
	}
	if grant := harness.budget.Reserve("session", 10000); grant.Grant != 5000 {
		t.Fatalf("failed call leaked its reservation; next grant = %#v", grant)
	} else {
		harness.budget.Consume("session", grant.ID, 0)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	result, err = harness.tool.Call(cancelled, loop.ToolCall{ID: "cancelled", SessionID: "cancelled", Args: map[string]any{"path": "file.txt"}})
	if !errors.Is(err, context.Canceled) || result.CallID != "cancelled" || result.Content != nil {
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
	call := loop.ToolCall{SessionID: "session", Args: map[string]any{"path": "file.txt"}}
	first, err := harness.tool.Call(context.Background(), call)
	if err != nil {
		t.Fatal(err)
	}
	first.Content["content"] = "mutated"
	first.Content["report"].(map[string]any)["reason"] = "mutated"
	second, err := harness.tool.Call(context.Background(), call)
	if err != nil {
		t.Fatal(err)
	}
	decoded := decodeReadResult(t, second.Content)
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
			result, err := harness.tool.Call(context.Background(), loop.ToolCall{SessionID: "session", ID: string(rune(index)), Args: map[string]any{"path": "file.txt"}})
			if err == nil && result.Content == nil {
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
	fixture.WalkKinds(t, "tools", "codingtools/read", []string{"read"}, func(t *testing.T, testCase fixture.Case) {
		var metadata schemaFixtureMetadata
		var input readFixtureInput
		testCase.Decode(t, "case.json", &metadata)
		testCase.Decode(t, "input.json", &input)
		if metadata.Area != "tools" || metadata.ID != testCase.ID || metadata.Kind != "read" {
			t.Fatalf("invalid metadata: %#v", metadata)
		}

		base := t.TempDir()
		rootDir := filepath.Join(base, "root")
		if err := os.Mkdir(rootDir, 0o755); err != nil {
			t.Fatal(err)
		}
		switch input.Setup {
		case "file":
			if err := os.WriteFile(filepath.Join(rootDir, input.Path), []byte(input.File), 0o644); err != nil {
				t.Fatal(err)
			}
		case "directory":
			if err := os.Mkdir(filepath.Join(rootDir, input.Path), 0o755); err != nil {
				t.Fatal(err)
			}
		case "missing":
		case "escape":
			if err := os.WriteFile(filepath.Join(base, "outside.txt"), []byte(input.File), 0o644); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatalf("unknown setup %q", input.Setup)
		}
		budgetLimit := input.Budget
		if budgetLimit == 0 {
			budgetLimit = outputlimit.DefaultPerSession
		}
		harness := newReadHarness(t, rootDir, func(cfg *Config) {
			cfg.Budget = outputlimit.NewBudget(budgetLimit)
			cfg.MaxReadBytes = input.MaxReadBytes
			cfg.Output = input.Output
		})
		args := decodeFixtureArguments(t, input.Args)
		ctx := context.Background()
		if input.Cancel {
			cancelled, cancel := context.WithCancel(ctx)
			cancel()
			ctx = cancelled
		}
		result, callErr := harness.tool.Call(ctx, loop.ToolCall{ID: "fixture-call", SessionID: "fixture-session", Args: args})
		actual := readFixtureExpected{Error: readFixtureError(t, callErr)}
		if result.Content == nil {
			if result.CallID != "fixture-call" || result.Content != nil {
				t.Fatalf("failure result = %#v", result)
			}
		} else {
			got := decodeReadResult(t, result.Content)
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
		testCase.CompareJSON(t, "expected.json", actual, fixture.Paths{}, fixture.GoldenReadOnly)
	})
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
	if strings.Contains(err.Error(), "read arguments") {
		return "arguments"
	}
	t.Fatalf("unknown read fixture error: %v", err)
	return ""
}
