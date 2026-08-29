package plasmid_test

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.lsp.dev/protocol"
	"google.golang.org/adk/v2/model"
	adkplugin "google.golang.org/adk/v2/plugin"
	"google.golang.org/genai"

	"github.com/RandomCodeSpace/plasmid"
	"github.com/RandomCodeSpace/plasmid/codingtools"
	"github.com/RandomCodeSpace/plasmid/compaction"
	"github.com/RandomCodeSpace/plasmid/lsp"
	"github.com/RandomCodeSpace/plasmid/warning"
)

var releaseLSPMarker = flag.String("plasmid-release-lsp-marker", "", "release conformance LSP marker directory")
var releaseLSPBlockInitializeMarker = flag.String("plasmid-release-lsp-block-initialize-marker", "", "release conformance blocked initialize marker")

type releaseLSPConnection struct{}

func (releaseLSPConnection) Read(value []byte) (int, error)  { return os.Stdin.Read(value) }
func (releaseLSPConnection) Write(value []byte) (int, error) { return os.Stdout.Write(value) }
func (releaseLSPConnection) Close() error                    { return nil }

func TestReleaseConformanceLSPHelper(t *testing.T) {
	if *releaseLSPMarker == "" {
		t.Skip("helper process")
	}
	if err := os.WriteFile(filepath.Join(*releaseLSPMarker, "started"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	var transport *lsp.RPCTransport
	handler := func(ctx context.Context, method string, raw json.RawMessage) (any, error) {
		switch method {
		case "initialize":
			return releaseInitialize(transport)
		case "textDocument/didOpen":
			var opened protocol.DidOpenTextDocumentParams
			if err := protocol.Unmarshal(raw, &opened); err != nil {
				return nil, err
			}
			return nil, transport.Notify(ctx, "textDocument/publishDiagnostics", releaseDiagnostics(protocol.PublishDiagnosticsParams{
				URI: opened.TextDocument.URI, Version: protocol.NewOptional(opened.TextDocument.Version),
			}))
		case "textDocument/didChange":
			var changed protocol.DidChangeTextDocumentParams
			if err := protocol.Unmarshal(raw, &changed); err != nil {
				return nil, err
			}
			return nil, transport.Notify(ctx, "textDocument/publishDiagnostics", releaseDiagnostics(protocol.PublishDiagnosticsParams{
				URI: changed.TextDocument.URI, Version: protocol.NewOptional(changed.TextDocument.Version),
			}))
		}
		return nil, nil
	}
	var err error
	transport, err = lsp.NewRPCTransport(context.Background(), releaseLSPConnection{}, lsp.DefaultMaxMessageBytes, handler)
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestResource(t, transport)
	<-transport.Done()
}

func releaseInitialize(transport *lsp.RPCTransport) (any, error) {
	if *releaseLSPBlockInitializeMarker == "" {
		return protocol.InitializeResult{Capabilities: protocol.ServerCapabilities{PositionEncoding: protocol.PositionEncodingKindUTF16}}, nil
	}
	if err := os.WriteFile(*releaseLSPBlockInitializeMarker, []byte("blocked"), 0o600); err != nil {
		return nil, err
	}
	<-transport.Done()
	return nil, context.Canceled
}

func releaseDiagnostics(params protocol.PublishDiagnosticsParams) protocol.PublishDiagnosticsParams {
	params.Diagnostics = []protocol.Diagnostic{{
		Range: protocol.Range{
			Start: protocol.Position{Line: 0, Character: 8},
			End:   protocol.Position{Line: 0, Character: 11},
		},
		Severity: protocol.DiagnosticSeverityError,
		Code:     protocol.String("E-RELEASE"),
		Source:   protocol.NewOptional("release-lsp"),
		Message:  protocol.String("invalid package name"),
	}}
	return params
}

type releaseEnvironment struct {
	workingDir                string
	sessionDir                string
	skillRoot                 string
	lspMarkers                string
	configPath                string
	mcpServer                 *httptest.Server
	mcpRequests               atomic.Int32
	closeMu                   sync.Mutex
	closeOrder                []string
	lspPID                    int
	pluginClosesBeforeLSPExit atomic.Int32
}

func TestReleaseConformanceCombinesNativeV1Runtime(t *testing.T) {
	environment := newReleaseEnvironment(t)
	firstModel := &releaseConformanceModel{workingDir: environment.workingDir}
	first := newFirstReleaseHarness(t, environment, firstModel)
	sessionID := exerciseFirstReleaseHarness(t, environment, first, firstModel)
	closeFirstReleaseHarness(t, environment, first, sessionID)
	reopenReleaseInputs(t, environment)
	exerciseResumedReleaseHarness(t, environment, sessionID)
}

func TestHarnessCloseKeepsLSPAliveThroughPluginTeardown(t *testing.T) {
	environment := newReleaseCloseEnvironment(t)
	var pluginClosed, lspExitedDuringPlugin atomic.Bool
	plugin := &releasePlugin{name: "lsp-lifecycle-observer", close: func() {
		pluginClosed.Store(true)
		deadline := time.Now().Add(250 * time.Millisecond)
		for time.Now().Before(deadline) {
			if !releaseProcessExists(environment.lspPID) {
				lspExitedDuringPlugin.Store(true)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}}
	harness := newReleaseCloseHarness(t, environment, &releaseCloseModel{requireDiagnostics: true}, plugin)
	sessionID, err := harness.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if answer, askErr := harness.Ask(t.Context(), sessionID, "write invalid Go"); askErr != nil || answer != testDoneResponse {
		t.Fatalf("Ask = %q, %v", answer, askErr)
	}
	environment.lspPID = readReleaseLSPPID(t, environment.lspMarkers)
	if !releaseProcessExists(environment.lspPID) {
		t.Fatal("owned LSP process exited before Harness.Close")
	}
	if err := harness.Close(); err != nil {
		t.Fatal(err)
	}
	if !pluginClosed.Load() {
		t.Fatal("compiled plugin was not closed")
	}
	if lspExitedDuringPlugin.Load() {
		t.Fatal("owned LSP process exited during plugin teardown")
	}
	assertReleaseProcessExited(t, environment.lspPID)
}

func TestHarnessCloseCancelsActiveLSPOperationPromptly(t *testing.T) {
	blockedMarker := filepath.Join(t.TempDir(), "initialize-blocked")
	environment := newReleaseCloseEnvironment(t, "-plasmid-release-lsp-block-initialize-marker="+blockedMarker)
	harness := newReleaseCloseHarness(t, environment, &releaseCloseModel{})
	sessionID, err := harness.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	askDone := make(chan error, 1)
	go func() {
		_, askErr := harness.Ask(t.Context(), sessionID, "write invalid Go")
		askDone <- askErr
	}()
	waitForReleaseMarker(t, blockedMarker)
	environment.lspPID = readReleaseLSPPID(t, environment.lspMarkers)
	started := time.Now()
	closeErr := harness.Close()
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("Harness.Close took %s with active LSP operation: %v", elapsed, closeErr)
	}
	if closeErr != nil {
		t.Fatalf("Harness.Close with active LSP operation: %v", closeErr)
	}
	select {
	case <-askDone:
	case <-time.After(2 * time.Second):
		t.Fatal("active LSP operation survived Harness.Close")
	}
	assertReleaseProcessExited(t, environment.lspPID)
}

func waitForReleaseMarker(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) || time.Now().After(deadline) {
			t.Fatalf("wait for release marker %q: %v", filepath.Base(path), err)
		}
		time.Sleep(time.Millisecond)
	}
}

func newReleaseCloseEnvironment(t *testing.T, helperArgs ...string) *releaseEnvironment {
	t.Helper()
	environment := &releaseEnvironment{
		workingDir: t.TempDir(), sessionDir: filepath.Join(t.TempDir(), "sessions"), lspMarkers: t.TempDir(),
	}
	writeReleaseFile(t, environment.workingDir, "go.mod", "module release.close\n")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	arguments := []string{"-test.run=^TestReleaseConformanceLSPHelper$", "-plasmid-release-lsp-marker=" + environment.lspMarkers}
	arguments = append(arguments, helperArgs...)
	environment.configPath = writeHarnessConfig(t, map[string]any{
		"version": 1,
		"lsp": map[string]any{
			"settleTimeoutMs": 1000,
			"servers": []map[string]any{{
				"id": "gopls", "command": executable,
				"args":       arguments,
				"extensions": []string{".go"}, "rootMarkers": []string{"go.mod"},
			}},
		},
	})
	return environment
}

func newReleaseCloseHarness(t *testing.T, environment *releaseEnvironment, llm model.LLM, plugins ...plasmid.Plugin) *plasmid.Harness {
	t.Helper()
	harness, err := plasmid.New(t.Context(),
		plasmid.WithModel(llm), plasmid.WithWorkingDir(environment.workingDir),
		plasmid.WithSessionDir(environment.sessionDir), plasmid.WithConfig(environment.configPath),
		plasmid.WithPlugins(plugins...),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = harness.Close() })
	return harness
}

type releaseCloseModel struct {
	calls              int
	requireDiagnostics bool
}

func (*releaseCloseModel) Name() string { return "release-close" }

func (modelValue *releaseCloseModel) GenerateContent(_ context.Context, request *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if modelValue.calls == 0 {
			modelValue.calls++
			yield(&model.LLMResponse{Content: releaseToolCall("release-close-write", "write", map[string]any{"path": "main.go", "content": "package bad\n"})}, nil)
			return
		}
		modelValue.calls++
		if modelValue.requireDiagnostics && !releaseDiagnosticsPresent(request, "release-close-write") {
			yield(nil, errors.New("successful native write result lacks current LSP diagnostics"))
			return
		}
		yield(&model.LLMResponse{Content: genai.NewContentFromText(testDoneResponse, genai.RoleModel)}, nil)
	}
}

func newReleaseEnvironment(t *testing.T) *releaseEnvironment {
	t.Helper()
	environment := &releaseEnvironment{
		workingDir: t.TempDir(), sessionDir: filepath.Join(t.TempDir(), "sessions"),
		skillRoot: filepath.Join(t.TempDir(), "skills"), lspMarkers: t.TempDir(),
	}
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	writeReleaseFile(t, environment.workingDir, "go.mod", "module release.test\n")
	writeReleaseFile(t, environment.workingDir, "AGENTS.md", "release root instruction\n")
	writeReleaseFile(t, environment.workingDir, "src/AGENTS.md", "release nested instruction\n")
	writeReleaseFile(t, environment.workingDir, "src/seed.txt", "seed\n")
	var large []string
	for index := range 70 {
		large = append(large, fmt.Sprintf("release-needle-%03d payload", index))
	}
	writeReleaseFile(t, environment.workingDir, "large.txt", strings.Join(large, "\n")+"\n")
	writeReleaseFile(t, environment.skillRoot, "review/SKILL.md", "---\nname: review\ndescription: Review changed Go\narguments: [focus]\nglobs: [src/**]\nallowed-tools: [write, edit, grep]\n---\nReview ${focus}, $1, and $ARGUMENTS in ${PROJECT_DIR}.\n")
	writeReleaseFile(t, homeDir, ".codex/prompts/release-check.md", "---\nallowed-tools: [read]\n---\nTemplate $ARGUMENTS in ${PROJECT_DIR}.\n")
	environment.mcpServer = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		environment.mcpRequests.Add(1)
		http.Error(response, "unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(environment.mcpServer.Close)
	environment.configPath = releaseConfig(t, environment)
	return environment
}

func releaseConfig(t *testing.T, environment *releaseEnvironment) string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return writeHarnessConfig(t, map[string]any{
		"version": 1,
		"foreign": map[string]any{"enabled": true, "claude": false, "codex": true, "copilot": false},
		"skills":  map[string]any{"roots": []string{environment.skillRoot}},
		"mcp": map[string]any{"servers": []map[string]any{{
			"id": "release-failure", "transport": "http", "url": environment.mcpServer.URL,
		}}},
		"lsp": map[string]any{
			"settleTimeoutMs": 5000,
			"servers": []map[string]any{{
				"id": "gopls", "command": executable,
				"args":       []string{"-test.run=^TestReleaseConformanceLSPHelper$", "-plasmid-release-lsp-marker=" + environment.lspMarkers},
				"extensions": []string{".go"}, "rootMarkers": []string{"go.mod"},
			}},
		},
		"compaction": map[string]any{
			"calibration": true, "contextTokens": 1, "keepRecentContents": 0,
			"minimumElisionTokens": 1, "preserveToolNames": []string{"read", "load_skill", "write", "edit"},
			"targetFraction": 0.1, "triggerFraction": 0.5,
		},
		"tools": map[string]any{"callOutputBytes": 5000, "sessionOutputBytes": 3000},
	})
}

func (environment *releaseEnvironment) recordClose(name string) {
	environment.closeMu.Lock()
	environment.closeOrder = append(environment.closeOrder, name)
	environment.closeMu.Unlock()
	if environment.lspPID > 0 && releaseProcessExists(environment.lspPID) {
		environment.pluginClosesBeforeLSPExit.Add(1)
	}
}

func newFirstReleaseHarness(t *testing.T, environment *releaseEnvironment, firstModel *releaseConformanceModel) *plasmid.Harness {
	t.Helper()
	plugins := []plasmid.Plugin{
		&releasePlugin{name: "first", close: func() { environment.recordClose("compiled-first") }},
		&releasePlugin{name: "second", close: func() { environment.recordClose("compiled-second") }},
	}
	nativeFirst := newReleaseNativePlugin(t, "release-native-first", func() { environment.recordClose("native-first") })
	nativeSecond := newReleaseNativePlugin(t, "release-native-second", func() { environment.recordClose("native-second") })
	first, err := plasmid.New(t.Context(),
		plasmid.WithModel(firstModel), plasmid.WithWorkingDir(environment.workingDir),
		plasmid.WithSessionDir(environment.sessionDir), plasmid.WithConfig(environment.configPath), plasmid.WithPlugins(plugins...),
		plasmid.WithADKPlugins(nativeFirst, nativeSecond),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	return first
}

func newReleaseNativePlugin(t *testing.T, name string, close func()) *adkplugin.Plugin {
	t.Helper()
	result, err := adkplugin.New(adkplugin.Config{Name: name, CloseFunc: func() error {
		close()
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func exerciseFirstReleaseHarness(t *testing.T, environment *releaseEnvironment, first *plasmid.Harness, firstModel *releaseConformanceModel) string {
	t.Helper()
	assertReleasePolicy(t, first)
	sessionID, err := first.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	assertReleaseTemplates(t, environment, first, sessionID)
	assertReleaseCombinedTurn(t, environment, first, firstModel, sessionID)
	environment.lspPID = readReleaseLSPPID(t, environment.lspMarkers)
	return sessionID
}

func assertReleasePolicy(t *testing.T, harness *plasmid.Harness) {
	t.Helper()
	policy := harness.Config().Compaction
	if policy.ContextTokens != 1 || policy.KeepRecentContents != 0 || policy.MinimumElisionTokens != 1 || !reflect.DeepEqual(policy.PreserveToolNames, []string{"read", "load_skill", "write", "edit"}) {
		t.Fatalf("resolved compaction policy = %#v", policy)
	}
}

func assertReleaseTemplates(t *testing.T, environment *releaseEnvironment, harness *plasmid.Harness, sessionID string) {
	t.Helper()
	templates, err := harness.ListTemplates(t.Context(), sessionID)
	if err != nil || len(templates) != 1 || templates[0].Name != "release-check" {
		t.Fatalf("ListTemplates = %#v, %v", templates, err)
	}
	prompt, err := harness.GetTemplate(t.Context(), sessionID, "release-check", "security")
	wantPrompt := "Template security in " + environment.workingDir + ".\n"
	if err != nil || prompt != wantPrompt {
		t.Fatalf("GetTemplate = %q, %v; want %q", prompt, err, wantPrompt)
	}
	if answer, err := harness.AskTemplate(t.Context(), sessionID, "release-check", "security"); err != nil || answer != "template complete" {
		t.Fatalf("AskTemplate = %q, %v", answer, err)
	}
}

func assertReleaseCombinedTurn(t *testing.T, environment *releaseEnvironment, harness *plasmid.Harness, firstModel *releaseConformanceModel, sessionID string) {
	t.Helper()
	if answer, err := harness.Ask(t.Context(), sessionID, "run combined release turn"); err != nil || answer != "combined complete" {
		t.Fatalf("combined Ask = %q, %v", answer, err)
	}
	if firstModel.calls != 7 || !firstModel.sawNested || !firstModel.sawSkill || !firstModel.sawSkillPolicy || !firstModel.sawWriteDiagnostics || !firstModel.sawEditDiagnostics || !firstModel.sawElision {
		t.Fatalf("model evidence = %#v, warnings = %#v", firstModel, harness.Warnings())
	}
	if environment.mcpRequests.Load() == 0 {
		t.Fatal("authorized MCP server was never attempted lazily")
	}
	assertReleaseWarning(t, harness.Warnings(), warning.WarnMCPConnectFailed)
	encodedWarnings, err := json.Marshal(harness.Warnings())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encodedWarnings), environment.mcpServer.URL) || strings.Contains(string(encodedWarnings), "unavailable") {
		t.Fatalf("runtime warning leaked MCP transport detail: %s", encodedWarnings)
	}
}

func readReleaseLSPPID(t *testing.T, markerDir string) int {
	t.Helper()
	started, err := os.ReadFile(filepath.Join(markerDir, "started"))
	if err != nil {
		t.Fatalf("read lazy LSP process marker: %v", err)
	}
	pid, err := strconv.Atoi(string(started))
	if err != nil {
		t.Fatalf("parse lazy LSP process marker: %v", err)
	}
	return pid
}

func closeFirstReleaseHarness(t *testing.T, environment *releaseEnvironment, first *plasmid.Harness, sessionID string) {
	t.Helper()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	environment.closeMu.Lock()
	gotCloseOrder := append([]string(nil), environment.closeOrder...)
	environment.closeMu.Unlock()
	if want := []string{"native-second", "native-first", "compiled-second", "compiled-first"}; !reflect.DeepEqual(gotCloseOrder, want) {
		t.Fatalf("plugin close order = %v, want %v", gotCloseOrder, want)
	}
	if got := environment.pluginClosesBeforeLSPExit.Load(); got != 4 {
		t.Fatalf("plugin closes while LSP process alive = %d, want 4", got)
	}
	assertReleaseProcessExited(t, environment.lspPID)
	if err := first.ResumeSession(t.Context(), sessionID); !errors.Is(err, plasmid.ErrClosed) {
		t.Fatalf("closed Harness ResumeSession error = %v", err)
	}
}

func reopenReleaseInputs(t *testing.T, environment *releaseEnvironment) {
	t.Helper()
	writeReleaseFile(t, environment.workingDir, "AGENTS.md", "release reopened instruction\n")
	writeReleaseFile(t, environment.skillRoot, "review/SKILL.md", "---\nname: review\ndescription: Review changed Go\narguments: [focus]\nglobs: [src/**]\nallowed-tools: [read]\n---\nReopened ${focus} in ${PROJECT_DIR}.\n")
}

func exerciseResumedReleaseHarness(t *testing.T, environment *releaseEnvironment, sessionID string) {
	t.Helper()
	resumedModel := &releaseResumeModel{workingDir: environment.workingDir}
	second, err := plasmid.New(t.Context(),
		plasmid.WithModel(resumedModel), plasmid.WithWorkingDir(environment.workingDir),
		plasmid.WithSessionDir(environment.sessionDir), plasmid.WithConfig(environment.configPath),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	if err := second.ResumeSession(t.Context(), sessionID); err != nil {
		t.Fatal(err)
	}
	if answer, err := second.Ask(t.Context(), sessionID, "resume release session"); err != nil || answer != "resume complete" {
		t.Fatalf("resumed Ask = %q, %v", answer, err)
	}
	if resumedModel.calls != 3 || !resumedModel.sawDurableElision || !resumedModel.sawFreshContext || !resumedModel.sawFreshSkill {
		t.Fatalf("resumed runtime evidence = %#v", resumedModel)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("close resumed Harness: %v", err)
	}
}

func assertReleaseProcessExited(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for releaseProcessExists(pid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if releaseProcessExists(pid) {
		t.Fatalf("owned LSP process %d survived Harness close", pid)
	}
}

type releasePlugin struct {
	name  string
	close func()
}

func (p *releasePlugin) Name() string              { return p.name }
func (*releasePlugin) Init(*plasmid.Harness) error { return nil }
func (p *releasePlugin) Close() error {
	if p.close != nil {
		p.close()
	}
	return nil
}

type releaseConformanceModel struct {
	workingDir          string
	calls               int
	sawNested           bool
	sawSkill            bool
	sawSkillPolicy      bool
	sawWriteDiagnostics bool
	sawEditDiagnostics  bool
	sawElision          bool
}

func (*releaseConformanceModel) Name() string { return "release-conformance" }

func (m *releaseConformanceModel) GenerateContent(_ context.Context, request *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		content, err := m.responseContent(request)
		if err != nil {
			yield(nil, err)
			return
		}
		m.calls++
		yield(&model.LLMResponse{
			Content:       content,
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 100},
		}, nil)
	}
}

func (m *releaseConformanceModel) responseContent(request *model.LLMRequest) (*genai.Content, error) {
	switch m.calls {
	case 0:
		return m.templateContent(request)
	case 1:
		return m.initialContent(request)
	case 2:
		return m.nestedContent(request)
	case 3:
		return m.skillContent(request)
	case 4:
		return m.writeContent(request)
	case 5:
		return m.editContent(request)
	case 6:
		return m.elisionContent(request)
	default:
		return nil, fmt.Errorf("unexpected release model call %d", m.calls)
	}
}

func (m *releaseConformanceModel) templateContent(request *model.LLMRequest) (*genai.Content, error) {
	if latestUserText(request) != "Template security in "+m.workingDir+".\n" || request.Tools["read"] == nil || request.Tools["write"] != nil {
		return nil, errors.New("root template API did not use the normal scoped native run path")
	}
	return genai.NewContentFromText("template complete", genai.RoleModel), nil
}

func (m *releaseConformanceModel) initialContent(request *model.LLMRequest) (*genai.Content, error) {
	instruction := releaseInstruction(request)
	if !strings.Contains(instruction, "release root instruction") || strings.Contains(instruction, "release nested instruction") || request.Tools["load_skill"] != nil {
		return nil, errors.New("session snapshot exposed nested context or path-scoped skill before touch")
	}
	return releaseToolCall("release-read", "read", map[string]any{"path": "src/seed.txt"}), nil
}

func (m *releaseConformanceModel) nestedContent(request *model.LLMRequest) (*genai.Content, error) {
	if strings.Contains(releaseInstruction(request), "release nested instruction") && request.Tools["load_skill"] != nil {
		m.sawNested = true
	}
	if !m.sawNested {
		return nil, errors.New("native read touch did not activate nested context and path-scoped skill")
	}
	return releaseToolCall("release-skill", "load_skill", map[string]any{"name": "review", "arguments": "auth focus=security"}), nil
}

func (m *releaseConformanceModel) skillContent(request *model.LLMRequest) (*genai.Content, error) {
	want := "Review security, auth, and auth focus=security in " + m.workingDir + ".\n"
	m.sawSkill = releaseFunctionResponseContains(request, "load_skill", "content", want)
	m.sawSkillPolicy = request.Tools["write"] != nil && request.Tools["edit"] != nil && request.Tools["grep"] != nil && request.Tools["read"] == nil && request.Tools["load_skill"] == nil
	if !m.sawSkill || !m.sawSkillPolicy {
		encoded, _ := json.Marshal(request.Contents)
		return nil, fmt.Errorf("skill arguments or nested tool-policy intersection did not reach the native request (body=%t policy=%t): %s", m.sawSkill, m.sawSkillPolicy, encoded)
	}
	return releaseToolCall("release-write", "write", map[string]any{"path": "src/main.go", "content": "package bad\n"}), nil
}

func (m *releaseConformanceModel) writeContent(request *model.LLMRequest) (*genai.Content, error) {
	m.sawWriteDiagnostics = releaseDiagnosticsPresent(request, "release-write")
	if !m.sawWriteDiagnostics {
		return nil, errors.New("successful native write result lacks current LSP diagnostics")
	}
	return releaseToolCall("release-edit", "edit", map[string]any{"path": "src/main.go", "old_text": "bad", "new_text": "good"}), nil
}

func (m *releaseConformanceModel) editContent(request *model.LLMRequest) (*genai.Content, error) {
	m.sawEditDiagnostics = releaseDiagnosticsPresent(request, "release-edit")
	if !m.sawEditDiagnostics {
		return nil, errors.New("successful native edit result lacks current LSP diagnostics")
	}
	return releaseToolCall("release-grep", "grep", map[string]any{"pattern": "release-needle", "path": "large.txt", "max_results": 70}), nil
}

func (m *releaseConformanceModel) elisionContent(request *model.LLMRequest) (*genai.Content, error) {
	m.sawElision = releaseFunctionResponseContains(request, "grep", "output", compaction.ElisionMarker)
	if !m.sawElision {
		return nil, errors.New("combined native history was not compacted deterministically")
	}
	return genai.NewContentFromText("combined complete", genai.RoleModel), nil
}

type releaseResumeModel struct {
	workingDir        string
	calls             int
	sawDurableElision bool
	sawFreshContext   bool
	sawFreshSkill     bool
}

func (*releaseResumeModel) Name() string { return "release-resume" }
func (m *releaseResumeModel) GenerateContent(_ context.Context, request *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		m.sawDurableElision = m.sawDurableElision || releaseFunctionResponseContains(request, "grep", "output", compaction.ElisionMarker)
		response := &model.LLMResponse{UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 100}}
		switch m.calls {
		case 0:
			instruction := releaseInstruction(request)
			m.sawFreshContext = strings.Contains(instruction, "release reopened instruction") && !strings.Contains(instruction, "release root instruction")
			if !m.sawFreshContext || request.Tools["load_skill"] != nil {
				yield(nil, errors.New("resumed Harness did not rebuild its root context and scoped extension snapshot"))
				return
			}
			response.Content = releaseToolCall("release-resume-read", "read", map[string]any{"path": "src/seed.txt"})
		case 1:
			if request.Tools["load_skill"] == nil {
				yield(nil, errors.New("resumed native touch did not reactivate the rebuilt path-scoped skill"))
				return
			}
			response.Content = releaseToolCall("release-resume-skill", "load_skill", map[string]any{"name": "review", "arguments": "focus=resume"})
		case 2:
			want := "Reopened resume in " + m.workingDir + ".\n"
			m.sawFreshSkill = releaseFunctionResponseContains(request, "load_skill", "content", want)
			if !m.sawFreshSkill {
				yield(nil, errors.New("resumed Harness retained the closed skill snapshot"))
				return
			}
			response.Content = genai.NewContentFromText("resume complete", genai.RoleModel)
		default:
			yield(nil, fmt.Errorf("unexpected resumed model call %d", m.calls))
			return
		}
		m.calls++
		yield(response, nil)
	}
}

func releaseToolCall(id, name string, arguments map[string]any) *genai.Content {
	return &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: id, Name: name, Args: arguments}}}}
}

func releaseInstruction(request *model.LLMRequest) string {
	if request.Config == nil || request.Config.SystemInstruction == nil {
		return ""
	}
	var result strings.Builder
	for _, part := range request.Config.SystemInstruction.Parts {
		if part != nil {
			result.WriteString(part.Text)
		}
	}
	return result.String()
}

func latestUserText(request *model.LLMRequest) string {
	for index := len(request.Contents) - 1; index >= 0; index-- {
		content := request.Contents[index]
		if content == nil || content.Role != genai.RoleUser {
			continue
		}
		for _, part := range content.Parts {
			if part != nil && part.Text != "" {
				return part.Text
			}
		}
	}
	return ""
}

func releaseFunctionResponseContains(request *model.LLMRequest, name, key, want string) bool {
	for _, content := range request.Contents {
		if content == nil {
			continue
		}
		for _, part := range content.Parts {
			if part == nil || part.FunctionResponse == nil || part.FunctionResponse.Name != name {
				continue
			}
			if value, ok := part.FunctionResponse.Response[key].(string); ok && value == want {
				return true
			}
		}
	}
	return false
}

func releaseDiagnosticsPresent(request *model.LLMRequest, id string) bool {
	for _, content := range request.Contents {
		if content == nil {
			continue
		}
		for _, part := range content.Parts {
			if part == nil || part.FunctionResponse == nil || part.FunctionResponse.ID != id {
				continue
			}
			text, _ := part.FunctionResponse.Response[codingtools.DiagnosticsTextResultKey].(string)
			return strings.Contains(text, "E-RELEASE") && strings.Contains(text, "invalid package name") && part.FunctionResponse.Response[codingtools.DiagnosticsResultKey] != nil
		}
	}
	return false
}

func assertReleaseWarning(t *testing.T, warnings []warning.Warning, code string) {
	t.Helper()
	for _, notice := range warnings {
		if notice.Code == code {
			return
		}
	}
	t.Fatalf("warning %q not found in %#v", code, warnings)
}

func writeReleaseFile(t *testing.T, root, relative, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

var _ io.ReadWriteCloser = releaseLSPConnection{}
var _ model.LLM = (*releaseConformanceModel)(nil)
var _ model.LLM = (*releaseResumeModel)(nil)
var _ plasmid.Plugin = (*releasePlugin)(nil)
