package lsp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/RandomCodeSpace/plasmid/internal/fixture"
	"github.com/RandomCodeSpace/plasmid/outputlimit"
	"github.com/RandomCodeSpace/plasmid/warning"
	"github.com/RandomCodeSpace/plasmid/workspace"
)

const (
	fixtureDidOpenMethod   = "textDocument/didOpen"
	fixtureDidChangeMethod = "textDocument/didChange"
)

var enforcementBehaviorKinds = []string{
	"enforcement-result-status",
	"enforcement-failure",
	"enforcement-versioning",
	"enforcement-settle",
}

type enforcementBehaviorInput struct {
	Content      string `json:"content"`
	FailureLimit int    `json:"failure_limit"`
	Message      string `json:"message"`
	Path         string `json:"path"`
	SettleMillis int    `json:"settle_millis"`
}

type enforcementResultStatusOutput struct {
	Result          map[string]any `json:"result"`
	StatusBefore    string         `json:"status_before"`
	StatusAfter     string         `json:"status_after"`
	LanguageID      string         `json:"language_id"`
	DocumentVersion int32          `json:"document_version"`
}

type enforcementFailureOutput struct {
	FailuresBeforeReset    int      `json:"failures_before_reset"`
	SuccessfulReset        bool     `json:"successful_reset"`
	ActiveAfterReset       bool     `json:"active_after_reset"`
	DisabledAfterThreshold bool     `json:"disabled_after_threshold"`
	WarningCodes           []string `json:"warning_codes"`
}

type enforcementVersioningOutput struct {
	DocumentVersions []int32 `json:"document_versions"`
	FirstMessage     string  `json:"first_message"`
	ClearCount       int     `json:"clear_count"`
	ClearText        string  `json:"clear_text"`
	StaleRejected    bool    `json:"stale_rejected"`
}

type enforcementSettleOutput struct {
	Decorated               bool     `json:"decorated"`
	DeadlinePresent         bool     `json:"deadline_present"`
	DeadlineExceeded        bool     `json:"deadline_exceeded"`
	WarningCodes            []string `json:"warning_codes"`
	WarningCountAfterRepeat int      `json:"warning_count_after_repeat"`
	WarningCountAfterCancel int      `json:"warning_count_after_cancel"`
}

type settleFixtureManager struct {
	deadlinePresent  bool
	deadlineExceeded bool
}

func (manager *settleFixtureManager) synchronize(_ context.Context, _, _, path, _ string, _ []byte) (enforcementTicket, bool, error) {
	return enforcementTicket{diagnostic: diagnosticTicket{path: path}}, true, nil
}

func (manager *settleFixtureManager) waitForDiagnostics(ctx context.Context, _ enforcementTicket) ([]Diagnostic, bool) {
	_, manager.deadlinePresent = ctx.Deadline()
	<-ctx.Done()
	manager.deadlineExceeded = errors.Is(ctx.Err(), context.DeadlineExceeded)
	return nil, false
}

func (*settleFixtureManager) ActiveServers() []string { return nil }

func walkEnforcementBehaviorFixtures(t *testing.T) {
	t.Helper()
	fixture.WalkKinds(t, "lsp", "lsp/enforcement", enforcementBehaviorKinds, func(t *testing.T, testCase fixture.Case) {
		var input enforcementBehaviorInput
		testCase.Decode(t, "input.json", &input)
		var actual any
		switch testCase.Metadata(t).Kind {
		case "enforcement-result-status":
			actual = runEnforcementResultStatusFixture(t, input)
		case "enforcement-failure":
			actual = runEnforcementFailureFixture(t, input)
		case "enforcement-versioning":
			actual = runEnforcementVersioningFixture(t, input)
		case "enforcement-settle":
			actual = runEnforcementSettleFixture(t, input)
		default:
			t.Fatalf("unsupported enforcement fixture kind %q", testCase.Metadata(t).Kind)
		}
		testCase.CompareJSON(t, "expected.json", actual, fixture.Paths{}, fixture.GoldenReadOnly)
	})
}

func runEnforcementResultStatusFixture(t *testing.T, input enforcementBehaviorInput) enforcementResultStatusOutput {
	t.Helper()
	root, path := prepareEnforcementFixtureWorkspace(t, input)
	var opened textDocumentItem
	_, enforcer, bus := newEnforcementFixtureRuntime(t, root, warning.DiscardSink{}, 100*time.Millisecond, func(transport *enforcerTransport) {
		transport.publish = func(ctx context.Context, transport *enforcerTransport, method string, params any) {
			if method != fixtureDidOpenMethod {
				return
			}
			opened = params.(didOpenParams).TextDocument
			transport.publishDiagnostics(ctx, opened.URI, opened.Version, diagnosticValues(input.Message))
		}
	})
	statusBefore := enforcer.Status()
	bus.Publish(t.Context(), workspace.Touch{SessionID: "fixture", InvocationID: "write", Path: path, Kind: workspace.TouchWrite, Content: []byte(input.Content)})
	decoration, ok := enforcer.Await(t.Context(), "fixture", "write")
	if !ok {
		t.Fatal("fixture write did not produce a decoration")
	}
	return enforcementResultStatusOutput{
		Result: map[string]any{
			"diagnostics":      decoration.Diagnostics,
			"diagnostics_text": decoration.Text,
		},
		StatusBefore: statusBefore, StatusAfter: enforcer.Status(),
		LanguageID: opened.LanguageID, DocumentVersion: opened.Version,
	}
}

func runEnforcementFailureFixture(t *testing.T, input enforcementBehaviorInput) enforcementFailureOutput {
	t.Helper()
	root := t.TempDir()
	sink := &warning.SliceSink{}
	transport := newFakeTransport()
	manager, err := NewManager(t.Context(), DefaultRegistry(), ManagerOptions{
		Warnings: sink, FailureLimit: input.FailureLimit,
		LookPath: func(string) (string, error) { return "/fixture/gopls", nil },
		Start: func(context.Context, string, []string, string, int64, MessageHandler) (Transport, error) {
			return transport, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	client, err := manager.Start(t.Context(), "gopls", root)
	if err != nil || client == nil {
		t.Fatalf("Start = %#v, %v", client, err)
	}
	setCall := func(call func(context.Context, string, any, any) error) {
		transport.mu.Lock()
		transport.call = call
		transport.mu.Unlock()
	}
	setCall(func(context.Context, string, any, any) error { return errors.New("fixture request failed") })
	failuresBeforeReset := input.FailureLimit - 1
	for range failuresBeforeReset {
		client.Call(t.Context(), "fixture/failure", nil, nil)
	}
	setCall(func(context.Context, string, any, any) error { return nil })
	successfulReset := client.Call(t.Context(), "fixture/success", nil, nil)
	setCall(func(context.Context, string, any, any) error { return errors.New("fixture request failed") })
	for range failuresBeforeReset {
		client.Call(t.Context(), "fixture/failure", nil, nil)
	}
	active, activeErr := manager.Start(t.Context(), "gopls", root)
	if activeErr != nil {
		t.Fatal(activeErr)
	}
	client.Call(t.Context(), "fixture/threshold", nil, nil)
	disabled, disabledErr := manager.Start(t.Context(), "gopls", root)
	if disabledErr != nil {
		t.Fatal(disabledErr)
	}
	return enforcementFailureOutput{
		FailuresBeforeReset: failuresBeforeReset, SuccessfulReset: successfulReset,
		ActiveAfterReset: active == client, DisabledAfterThreshold: disabled == nil,
		WarningCodes: warningCodes(sink.Warnings()),
	}
}

func runEnforcementVersioningFixture(t *testing.T, input enforcementBehaviorInput) enforcementVersioningOutput {
	t.Helper()
	root, path := prepareEnforcementFixtureWorkspace(t, input)
	var versions []int32
	_, enforcer, bus := newEnforcementFixtureRuntime(t, root, warning.DiscardSink{}, 100*time.Millisecond, func(transport *enforcerTransport) {
		transport.publish = func(ctx context.Context, transport *enforcerTransport, method string, params any) {
			switch method {
			case fixtureDidOpenMethod:
				opened := params.(didOpenParams).TextDocument
				versions = append(versions, opened.Version)
				transport.publishDiagnostics(ctx, opened.URI, opened.Version, diagnosticValues(input.Message))
			case fixtureDidChangeMethod:
				changed := params.(didChangeParams).TextDocument
				versions = append(versions, changed.Version)
				transport.publishDiagnostics(ctx, changed.URI, changed.Version-1, diagnosticValues("versioned stale"))
				transport.publishUnversionedDiagnostics(ctx, changed.URI, diagnosticValues("unversioned stale"))
				transport.publishDiagnostics(ctx, changed.URI, changed.Version, nil)
			}
		}
	})
	bus.Publish(t.Context(), workspace.Touch{SessionID: "fixture", InvocationID: "write", Path: path, Kind: workspace.TouchWrite, Content: []byte(input.Content)})
	first, firstOK := enforcer.Await(t.Context(), "fixture", "write")
	bus.Publish(t.Context(), workspace.Touch{SessionID: "fixture", InvocationID: "edit", Path: path, Kind: workspace.TouchEdit, Content: []byte(input.Content + "// edited\n")})
	cleared, clearOK := enforcer.Await(t.Context(), "fixture", "edit")
	firstMessage := ""
	if firstOK && len(first.Diagnostics) == 1 {
		firstMessage = first.Diagnostics[0].Message
	}
	return enforcementVersioningOutput{
		DocumentVersions: versions, FirstMessage: firstMessage,
		ClearCount: len(cleared.Diagnostics), ClearText: cleared.Text,
		StaleRejected: clearOK && len(cleared.Diagnostics) == 0,
	}
}

func runEnforcementSettleFixture(t *testing.T, input enforcementBehaviorInput) enforcementSettleOutput {
	t.Helper()
	root, path := prepareEnforcementFixtureWorkspace(t, input)
	sink := &warning.SliceSink{}
	manager := &settleFixtureManager{}
	bus := workspace.NewTouchBus()
	enforcer, err := NewEnforcer(EnforcerOptions{
		WorkspaceDir: root, Touches: bus, Registry: DefaultRegistry(), Manager: manager,
		SettleTimeout: time.Duration(input.SettleMillis) * time.Millisecond,
		Output:        outputlimit.Defaults(), Warnings: sink, Maximum: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = enforcer.Close() })
	bus.Publish(t.Context(), workspace.Touch{SessionID: "fixture", InvocationID: "timeout", Path: path, Kind: workspace.TouchWrite, Content: []byte(input.Content)})
	_, decorated := enforcer.Await(t.Context(), "fixture", "timeout")
	deadlinePresent := manager.deadlinePresent
	deadlineExceeded := manager.deadlineExceeded
	bus.Publish(t.Context(), workspace.Touch{SessionID: "fixture", InvocationID: "repeat", Path: path, Kind: workspace.TouchEdit, Content: []byte(input.Content)})
	_, _ = enforcer.Await(t.Context(), "fixture", "repeat")
	afterRepeat := len(sink.Warnings())
	bus.Publish(t.Context(), workspace.Touch{SessionID: "fixture", InvocationID: "cancel", Path: path, Kind: workspace.TouchEdit, Content: []byte(input.Content)})
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	_, _ = enforcer.Await(canceled, "fixture", "cancel")
	return enforcementSettleOutput{
		Decorated: decorated, DeadlinePresent: deadlinePresent, DeadlineExceeded: deadlineExceeded,
		WarningCodes:            warningCodes(sink.Warnings()),
		WarningCountAfterRepeat: afterRepeat, WarningCountAfterCancel: len(sink.Warnings()),
	}
}

func prepareEnforcementFixtureWorkspace(t *testing.T, input enforcementBehaviorInput) (string, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module fixture.test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.FromSlash(input.Path)
	absolute := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(absolute), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(absolute, []byte(input.Content), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, path
}

func newEnforcementFixtureRuntime(t *testing.T, root string, sink warning.Warner, settle time.Duration, configure func(*enforcerTransport)) (*Manager, *Enforcer, *workspace.TouchBus) {
	t.Helper()
	manager, err := NewManager(t.Context(), DefaultRegistry(), ManagerOptions{
		Warnings: sink,
		LookPath: func(string) (string, error) { return "/fixture/gopls", nil },
		Start: func(_ context.Context, _ string, _ []string, _ string, _ int64, handler MessageHandler) (Transport, error) {
			transport := newEnforcerTransport(handler)
			if configure != nil {
				configure(transport)
			}
			return transport, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	bus := workspace.NewTouchBus()
	enforcer, err := NewEnforcer(EnforcerOptions{
		WorkspaceDir: root, Touches: bus, Registry: DefaultRegistry(), Manager: manager,
		SettleTimeout: settle, Output: outputlimit.Defaults(), Warnings: sink, Maximum: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = enforcer.Close() })
	return manager, enforcer, bus
}

func warningCodes(values []warning.Warning) []string {
	codes := make([]string, 0, len(values))
	for _, value := range values {
		codes = append(codes, value.Code)
	}
	return codes
}
