package codingtools

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/plasmid-dev/plasmid/loop"
	"github.com/plasmid-dev/plasmid/outputlimit"
	"github.com/plasmid-dev/plasmid/workspace"
)

type writeObserver struct {
	mu      sync.Mutex
	touches []workspace.Touch
}

func (o *writeObserver) ObserveTouch(_ context.Context, touch workspace.Touch) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.touches = append(o.touches, touch)
}
func (o *writeObserver) snapshot() []workspace.Touch {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]workspace.Touch(nil), o.touches...)
}

type writeHarness struct {
	tool     loop.Tool
	ledger   *workspace.Ledger
	observer *writeObserver
	root     string
}

func newWriteHarness(t *testing.T, rootDir string, configure func(*Config)) writeHarness {
	t.Helper()
	root, err := workspace.NewRoot(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	ledger, bus, observer := workspace.NewLedger(), workspace.NewTouchBus(), &writeObserver{}
	bus.Subscribe(observer)
	cfg := Config{Root: root, Queue: workspace.NewMutationQueue(), Ledger: ledger, Touch: bus, Budget: outputlimit.NewBudget(10000)}
	if configure != nil {
		configure(&cfg)
	}
	tool, err := NewWriteTool(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return writeHarness{tool: tool, ledger: ledger, observer: observer, root: rootDir}
}

func TestNewWriteToolContract(t *testing.T) {
	root, err := workspace.NewRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	valid := Config{Root: root, Queue: workspace.NewMutationQueue(), Ledger: workspace.NewLedger(), Touch: workspace.NewTouchBus(), Budget: outputlimit.NewBudget(10000)}
	for _, mutate := range []func(*Config){func(c *Config) { c.Root = nil }, func(c *Config) { c.Queue = nil }, func(c *Config) { c.Ledger = nil }, func(c *Config) { c.Touch = nil }, func(c *Config) { c.Budget = nil }} {
		cfg := valid
		mutate(&cfg)
		if _, err := NewWriteTool(cfg); err == nil {
			t.Fatal("constructor accepted missing dependency")
		}
	}
	tool, err := NewWriteTool(valid)
	if err != nil {
		t.Fatal(err)
	}
	if tool.Name() != "write" || tool.Description() != WriteDescription || string(tool.InputSchema()) != string(WriteInputSchema()) {
		t.Fatal("write metadata drifted")
	}
}

func TestWriteCreateAndOverwrite(t *testing.T) {
	h := newWriteHarness(t, t.TempDir(), nil)
	result, err := h.tool.Call(context.Background(), loop.ToolCall{ID: "create", SessionID: "s", Args: map[string]any{"content": "one\r\n", "path": "nested/file.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(h.root, "nested", "file.txt")); string(got) != "one\r\n" {
		t.Fatalf("file = %q", got)
	}
	created := decodeWriteResult(t, result.Content)
	if created.Path != "nested/file.txt" || created.BytesWritten != 5 {
		t.Fatalf("create result = %#v", created)
	}
	hash := sha256.Sum256([]byte("one\r\n"))
	if err := h.ledger.Verify("s", "nested/file.txt", 5, hash); err != nil {
		t.Fatalf("ledger = %v", err)
	}
	if err := os.Chmod(filepath.Join(h.root, "nested", "file.txt"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The prior successful write is a full read for this session.
	result, err = h.tool.Call(context.Background(), loop.ToolCall{ID: "replace", SessionID: "s", Args: map[string]any{"content": "two", "path": "nested/file.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(h.root, "nested", "file.txt")); string(got) != "two" {
		t.Fatalf("file = %q", got)
	}
	if mode := mustStat(t, filepath.Join(h.root, "nested", "file.txt")).Mode().Perm(); mode != 0o600 {
		t.Fatalf("mode = %#o", mode)
	}
	if !strings.Contains(decodeWriteResult(t, result.Content).Diff, "-one") {
		t.Fatalf("diff = %q", decodeWriteResult(t, result.Content).Diff)
	}
	touches := h.observer.snapshot()
	if len(touches) != 2 || touches[0].Path != "nested/file.txt" {
		t.Fatalf("touches = %#v", touches)
	}
	touches[1].Content[0] = 'X'
	if got, _ := os.ReadFile(filepath.Join(h.root, "nested", "file.txt")); string(got) != "two" {
		t.Fatalf("touch aliases disk: %q", got)
	}
}

func TestWriteRejectsArgumentsAndUnreadTarget(t *testing.T) {
	h := newWriteHarness(t, t.TempDir(), func(c *Config) { c.MaxWriteBytes = 2 })
	path := filepath.Join(h.root, "old.txt")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range []map[string]any{nil, {"path": ""}, {"content": "x", "path": "x", "unknown": true}, {"content": 1, "path": "x"}, {"content": "xxx", "path": "x"}, {"content": "x", "path": []byte("x")}, {"content": []byte("x"), "path": "x"}} {
		_, err := h.tool.Call(context.Background(), loop.ToolCall{SessionID: "s", Args: args})
		if err == nil {
			t.Fatalf("args %#v accepted", args)
		}
	}
	_, err := h.tool.Call(context.Background(), loop.ToolCall{SessionID: "s", Args: map[string]any{"content": "x", "path": "old.txt"}})
	if !errors.Is(err, ErrNeverRead) || !strings.Contains(err.Error(), "read the file again") {
		t.Fatalf("unread overwrite error = %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "old" {
		t.Fatalf("unread write changed file: %q", got)
	}
}

func TestWriteDiffPreservesByteLevelChanges(t *testing.T) {
	h := newWriteHarness(t, t.TempDir(), nil)
	path := filepath.Join(h.root, "file.txt")
	old := []byte("\ufeffone\r\n")
	if err := os.WriteFile(path, old, 0o644); err != nil {
		t.Fatal(err)
	}
	h.ledger.RecordRead("s", "file.txt", int64(len(old)), sha256.Sum256(old))
	result, err := h.tool.Call(context.Background(), loop.ToolCall{SessionID: "s", Args: map[string]any{"path": "file.txt", "content": "one\n"}})
	if err != nil {
		t.Fatal(err)
	}
	want := "--- a/file.txt\n+++ b/file.txt\n@@ -1,1 +1,1 @@\n-\uFEFFone\r\n+one\n"
	if gotDiff := decodeWriteResult(t, result.Content).Diff; gotDiff != want {
		t.Fatalf("byte-level diff = %q, want %q", gotDiff, want)
	}
}

func TestWriteRejectsStaleAndEscapedTargets(t *testing.T) {
	h := newWriteHarness(t, t.TempDir(), nil)
	path := filepath.Join(h.root, "file.txt")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.ledger.RecordRead("s", "file.txt", 3, sha256.Sum256([]byte("old")))
	if err := os.WriteFile(path, []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := h.tool.Call(context.Background(), loop.ToolCall{SessionID: "s", Args: map[string]any{"path": "file.txt", "content": "next"}})
	if !errors.Is(err, ErrStaleRead) || !strings.Contains(err.Error(), "read the file again") {
		t.Fatalf("stale write error = %v", err)
	}
	_, err = h.tool.Call(context.Background(), loop.ToolCall{SessionID: "s", Args: map[string]any{"path": "../outside.txt", "content": "x"}})
	if !errors.Is(err, ErrPathOutsideRoot) {
		t.Fatalf("escape error = %v", err)
	}
}

func TestWriteAllowsEmptyContentAndAppliesBudget(t *testing.T) {
	h := newWriteHarness(t, t.TempDir(), nil)
	result, err := h.tool.Call(context.Background(), loop.ToolCall{SessionID: "s", Args: map[string]any{"path": "empty.txt", "content": ""}})
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeWriteResult(t, result.Content); got.BytesWritten != 0 {
		t.Fatalf("empty write result = %#v", got)
	}

	budgetRoot := t.TempDir()
	budget := outputlimit.NewBudget(2000)
	limited := newWriteHarness(t, budgetRoot, func(c *Config) {
		c.Budget = budget
		c.Output = outputlimit.Policy{MaxBytes: 4000, MaxLines: 10000, MaxLineBytes: 4000, HeadFraction: 0.6}
	})
	content := strings.Repeat("x\n", 3000)
	result, err = limited.tool.Call(context.Background(), loop.ToolCall{SessionID: "s", Args: map[string]any{"path": "large.txt", "content": content}})
	if err != nil {
		t.Fatal(err)
	}
	got := decodeWriteResult(t, result.Content)
	if !got.Truncated || got.Report.Reason != outputlimit.ReasonBudget {
		t.Fatalf("budget report = %#v", got.Report)
	}
}

func TestWriteCancellationAndDirectory(t *testing.T) {
	h := newWriteHarness(t, t.TempDir(), nil)
	if err := os.Mkdir(filepath.Join(h.root, "directory"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := h.tool.Call(context.Background(), loop.ToolCall{SessionID: "s", Args: map[string]any{"content": "x", "path": "directory"}})
	if !errors.Is(err, ErrIsDirectory) {
		t.Fatalf("directory error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = h.tool.Call(ctx, loop.ToolCall{SessionID: "s", Args: map[string]any{"content": "x", "path": "cancel.txt"}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.root, "cancel.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled file stat = %v", err)
	}
}

func TestAtomicReplaceCancellationAndFailureCleanTemps(t *testing.T) {
	for _, test := range []struct {
		name    string
		options func(context.CancelFunc) atomicReplaceOptions
	}{
		{
			name: "cancel before rename",
			options: func(cancel context.CancelFunc) atomicReplaceOptions {
				return atomicReplaceOptions{beforeRename: func(string) { cancel() }}
			},
		},
		{
			name: "rename failure",
			options: func(context.CancelFunc) atomicReplaceOptions {
				return atomicReplaceOptions{rename: func(string, string) error { return errors.New("injected rename failure") }}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "file.txt")
			if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			parent, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer parent.Close()
			err = atomicReplaceFileWith(ctx, parent, "file.txt", []byte("new"), 0o600, true, test.options(cancel))
			if err == nil {
				t.Fatal("atomic replacement unexpectedly succeeded")
			}
			if got, readErr := os.ReadFile(path); readErr != nil || string(got) != "old" {
				t.Fatalf("target after failure = %q, %v", got, readErr)
			}
			if temps, globErr := filepath.Glob(filepath.Join(dir, ".plasmid-write-*")); globErr != nil || len(temps) != 0 {
				t.Fatalf("temporary files after failure = %#v, %v", temps, globErr)
			}
		})
	}
}

func TestAtomicReplaceCannotEscapeReplacedParent(t *testing.T) {
	dir := t.TempDir()
	moved := dir + "-moved"
	t.Cleanup(func() { _ = os.RemoveAll(moved) })
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "file.txt"), []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	parent, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()

	err = atomicReplaceFileWith(context.Background(), parent, "file.txt", []byte("new"), 0o600, true, atomicReplaceOptions{
		beforeRename: func(string) {
			if renameErr := os.Rename(dir, moved); renameErr != nil {
				t.Fatal(renameErr)
			}
			if symlinkErr := os.Symlink(outside, dir); symlinkErr != nil {
				t.Fatal(symlinkErr)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, readErr := os.ReadFile(filepath.Join(outside, "file.txt")); readErr != nil || string(got) != "outside" {
		t.Fatalf("outside target = %q, %v", got, readErr)
	}
	if got, readErr := os.ReadFile(filepath.Join(moved, "file.txt")); readErr != nil || string(got) != "new" {
		t.Fatalf("anchored target = %q, %v", got, readErr)
	}
}

type writeOrderingObserver struct {
	ledger *workspace.Ledger
	queue  *workspace.MutationQueue
	err    chan error
}

func (o *writeOrderingObserver) ObserveTouch(_ context.Context, touch workspace.Touch) {
	hash := sha256.Sum256(touch.Content)
	if err := o.ledger.Verify(touch.SessionID, touch.Path, int64(len(touch.Content)), hash); err != nil {
		o.err <- err
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	o.err <- o.queue.Do(ctx, func() error { return nil })
}

func TestWriteRecordsLedgerBeforeReleasedQueueAndTouch(t *testing.T) {
	rootDir := t.TempDir()
	root, err := workspace.NewRoot(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	queue, ledger, bus := workspace.NewMutationQueue(), workspace.NewLedger(), workspace.NewTouchBus()
	observer := &writeOrderingObserver{ledger: ledger, queue: queue, err: make(chan error, 1)}
	bus.Subscribe(observer)
	tool, err := NewWriteTool(Config{Root: root, Queue: queue, Ledger: ledger, Touch: bus, Budget: outputlimit.NewBudget(10000)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Call(context.Background(), loop.ToolCall{SessionID: "s", Args: map[string]any{"path": "file.txt", "content": "content"}}); err != nil {
		t.Fatal(err)
	}
	if err := <-observer.err; err != nil {
		t.Fatalf("touch ordering: %v", err)
	}
}

func TestConcurrentWritesNeverExposePartialContent(t *testing.T) {
	h := newWriteHarness(t, t.TempDir(), nil)
	contents := []string{strings.Repeat("a", 1<<20), strings.Repeat("b", 1<<20), strings.Repeat("c", 1<<20)}
	if _, err := h.tool.Call(context.Background(), loop.ToolCall{SessionID: "s", Args: map[string]any{"path": "file.txt", "content": contents[0]}}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	readErrors := make(chan error, 1)
	go func() {
		defer close(readErrors)
		for {
			select {
			case <-done:
				return
			default:
			}
			data, err := os.ReadFile(filepath.Join(h.root, "file.txt"))
			if err != nil {
				readErrors <- err
				return
			}
			got := string(data)
			if got != contents[0] && got != contents[1] && got != contents[2] {
				readErrors <- errors.New("reader observed partial content")
				return
			}
		}
	}()
	start := make(chan struct{})
	writeErrors := make(chan error, 2)
	var writers sync.WaitGroup
	for _, content := range contents[1:] {
		content := content
		writers.Add(1)
		go func() {
			defer writers.Done()
			<-start
			_, err := h.tool.Call(context.Background(), loop.ToolCall{SessionID: "s", Args: map[string]any{"path": "file.txt", "content": content}})
			writeErrors <- err
		}()
	}
	close(start)
	writers.Wait()
	close(writeErrors)
	close(done)
	for err := range writeErrors {
		if err != nil {
			t.Fatal(err)
		}
	}
	for err := range readErrors {
		if err != nil {
			t.Fatal(err)
		}
	}
	final, err := os.ReadFile(filepath.Join(h.root, "file.txt"))
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(final)
	if err := h.ledger.Verify("s", "file.txt", int64(len(final)), hash); err != nil {
		t.Fatalf("final ledger does not match file: %v", err)
	}
}

func decodeWriteResult(t *testing.T, object map[string]any) WriteResult {
	t.Helper()
	encoded, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	var result WriteResult
	if err := json.Unmarshal(encoded, &result); err != nil {
		t.Fatal(err)
	}
	return result
}
func mustStat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}
