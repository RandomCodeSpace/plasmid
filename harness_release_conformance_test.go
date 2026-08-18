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

	"github.com/plasmid-dev/plasmid"
	"github.com/plasmid-dev/plasmid/codingtools"
	"github.com/plasmid-dev/plasmid/compaction"
	"github.com/plasmid-dev/plasmid/lsp"
	"github.com/plasmid-dev/plasmid/warning"
)

var releaseLSPMarker = flag.String("plasmid-release-lsp-marker", "", "release conformance LSP marker directory")

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
			return protocol.InitializeResult{Capabilities: protocol.ServerCapabilities{PositionEncoding: protocol.PositionEncodingKindUTF16}}, nil
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
	defer transport.Close()
	<-transport.Done()
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

func TestReleaseConformanceCombinesNativeV1Runtime(t *testing.T) {
	workingDir := t.TempDir()
	sessionDir := filepath.Join(t.TempDir(), "sessions")
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	writeReleaseFile(t, workingDir, "go.mod", "module release.test\n")
	writeReleaseFile(t, workingDir, "AGENTS.md", "release root instruction\n")
	writeReleaseFile(t, workingDir, "src/AGENTS.md", "release nested instruction\n")
	writeReleaseFile(t, workingDir, "src/seed.txt", "seed\n")
	var large []string
	for index := range 70 {
		large = append(large, fmt.Sprintf("release-needle-%03d payload", index))
	}
	writeReleaseFile(t, workingDir, "large.txt", strings.Join(large, "\n")+"\n")

	skillRoot := filepath.Join(t.TempDir(), "skills")
	writeReleaseFile(t, skillRoot, "review/SKILL.md", "---\nname: review\ndescription: Review changed Go\narguments: [focus]\nglobs: [src/**]\nallowed-tools: [write, edit, grep]\n---\nReview ${focus}, $1, and $ARGUMENTS in ${PROJECT_DIR}.\n")
	writeReleaseFile(t, homeDir, ".codex/prompts/release-check.md", "---\nallowed-tools: [read]\n---\nTemplate $ARGUMENTS in ${PROJECT_DIR}.\n")

	lspMarkers := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var mcpRequests atomic.Int32
	mcpServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		mcpRequests.Add(1)
		http.Error(response, "unavailable", http.StatusServiceUnavailable)
	}))
	defer mcpServer.Close()
	configPath := writeHarnessConfig(t, map[string]any{
		"version": 1,
		"foreign": map[string]any{
			"enabled": true, "claude": false, "codex": true, "copilot": false,
		},
		"skills": map[string]any{"roots": []string{skillRoot}},
		"mcp": map[string]any{"servers": []map[string]any{{
			"id": "release-failure", "transport": "http", "url": mcpServer.URL,
		}}},
		"lsp": map[string]any{
			"settleTimeoutMs": 1000,
			"servers": []map[string]any{{
				"id": "gopls", "command": executable,
				"args":       []string{"-test.run=^TestReleaseConformanceLSPHelper$", "-plasmid-release-lsp-marker=" + lspMarkers},
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

	var closeMu sync.Mutex
	var closeOrder []string
	var lspPID int
	var pluginClosesBeforeLSPExit atomic.Int32
	recordClose := func(name string) {
		closeMu.Lock()
		closeOrder = append(closeOrder, name)
		closeMu.Unlock()
		if lspPID > 0 && releaseProcessExists(lspPID) {
			pluginClosesBeforeLSPExit.Add(1)
		}
	}
	plugins := []plasmid.Plugin{
		&releasePlugin{name: "first", close: func() { recordClose("compiled-first") }},
		&releasePlugin{name: "second", close: func() { recordClose("compiled-second") }},
	}
	nativeFirst, err := adkplugin.New(adkplugin.Config{Name: "release-native-first", CloseFunc: func() error {
		recordClose("native-first")
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	nativeSecond, err := adkplugin.New(adkplugin.Config{Name: "release-native-second", CloseFunc: func() error {
		recordClose("native-second")
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	firstModel := &releaseConformanceModel{workingDir: workingDir}
	first, err := plasmid.New(t.Context(),
		plasmid.WithModel(firstModel), plasmid.WithWorkingDir(workingDir),
		plasmid.WithSessionDir(sessionDir), plasmid.WithConfig(configPath), plasmid.WithPlugins(plugins...),
		plasmid.WithADKPlugins(nativeFirst, nativeSecond),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	if policy := first.Config().Compaction; policy.ContextTokens != 1 || policy.KeepRecentContents != 0 || policy.MinimumElisionTokens != 1 || !reflect.DeepEqual(policy.PreserveToolNames, []string{"read", "load_skill", "write", "edit"}) {
		t.Fatalf("resolved compaction policy = %#v", policy)
	}
	sessionID, err := first.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	templates, err := first.ListTemplates(t.Context(), sessionID)
	if err != nil || len(templates) != 1 || templates[0].Name != "release-check" {
		t.Fatalf("ListTemplates = %#v, %v", templates, err)
	}
	prompt, err := first.GetTemplate(t.Context(), sessionID, "release-check", "security")
	wantPrompt := "Template security in " + workingDir + ".\n"
	if err != nil || prompt != wantPrompt {
		t.Fatalf("GetTemplate = %q, %v; want %q", prompt, err, wantPrompt)
	}
	if answer, err := first.AskTemplate(t.Context(), sessionID, "release-check", "security"); err != nil || answer != "template complete" {
		t.Fatalf("AskTemplate = %q, %v", answer, err)
	}
	if answer, err := first.Ask(t.Context(), sessionID, "run combined release turn"); err != nil || answer != "combined complete" {
		t.Fatalf("combined Ask = %q, %v", answer, err)
	}
	if firstModel.calls != 7 || !firstModel.sawNested || !firstModel.sawSkill || !firstModel.sawSkillPolicy || !firstModel.sawWriteDiagnostics || !firstModel.sawEditDiagnostics || !firstModel.sawElision {
		t.Fatalf("model evidence = %#v, warnings = %#v", firstModel, first.Warnings())
	}
	if mcpRequests.Load() == 0 {
		t.Fatal("authorized MCP server was never attempted lazily")
	}
	assertReleaseWarning(t, first.Warnings(), warning.WarnMCPConnectFailed)
	encodedWarnings, err := json.Marshal(first.Warnings())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encodedWarnings), mcpServer.URL) || strings.Contains(string(encodedWarnings), "unavailable") {
		t.Fatalf("runtime warning leaked MCP transport detail: %s", encodedWarnings)
	}
	started, err := os.ReadFile(filepath.Join(lspMarkers, "started"))
	if err != nil {
		t.Fatalf("read lazy LSP process marker: %v", err)
	}
	lspPID, err = strconv.Atoi(string(started))
	if err != nil {
		t.Fatalf("parse lazy LSP process marker: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	closeMu.Lock()
	gotCloseOrder := append([]string(nil), closeOrder...)
	closeMu.Unlock()
	if want := []string{"native-second", "native-first", "compiled-second", "compiled-first"}; !reflect.DeepEqual(gotCloseOrder, want) {
		t.Fatalf("plugin close order = %v, want %v", gotCloseOrder, want)
	}
	if got := pluginClosesBeforeLSPExit.Load(); got != 4 {
		t.Fatalf("plugin closes while LSP process alive = %d, want 4", got)
	}
	assertReleaseProcessExited(t, lspPID)
	if err := first.ResumeSession(t.Context(), sessionID); !errors.Is(err, plasmid.ErrClosed) {
		t.Fatalf("closed Harness ResumeSession error = %v", err)
	}
	writeReleaseFile(t, workingDir, "AGENTS.md", "release reopened instruction\n")
	writeReleaseFile(t, skillRoot, "review/SKILL.md", "---\nname: review\ndescription: Review changed Go\narguments: [focus]\nglobs: [src/**]\nallowed-tools: [read]\n---\nReopened ${focus} in ${PROJECT_DIR}.\n")

	resumedModel := &releaseResumeModel{workingDir: workingDir}
	second, err := plasmid.New(t.Context(),
		plasmid.WithModel(resumedModel), plasmid.WithWorkingDir(workingDir),
		plasmid.WithSessionDir(sessionDir), plasmid.WithConfig(configPath),
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
		response := &model.LLMResponse{UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 100}}
		switch m.calls {
		case 0:
			if latestUserText(request) != "Template security in "+m.workingDir+".\n" || request.Tools["read"] == nil || request.Tools["write"] != nil {
				yield(nil, errors.New("root template API did not use the normal scoped native run path"))
				return
			}
			response.Content = genai.NewContentFromText("template complete", genai.RoleModel)
		case 1:
			instruction := releaseInstruction(request)
			if !strings.Contains(instruction, "release root instruction") || strings.Contains(instruction, "release nested instruction") || request.Tools["load_skill"] != nil {
				yield(nil, errors.New("session snapshot exposed nested context or path-scoped skill before touch"))
				return
			}
			response.Content = releaseToolCall("release-read", "read", map[string]any{"path": "src/seed.txt"})
		case 2:
			if strings.Contains(releaseInstruction(request), "release nested instruction") && request.Tools["load_skill"] != nil {
				m.sawNested = true
			}
			if !m.sawNested {
				yield(nil, errors.New("native read touch did not activate nested context and path-scoped skill"))
				return
			}
			response.Content = releaseToolCall("release-skill", "load_skill", map[string]any{"name": "review", "arguments": "auth focus=security"})
		case 3:
			want := "Review security, auth, and auth focus=security in " + m.workingDir + ".\n"
			m.sawSkill = releaseFunctionResponseContains(request, "load_skill", "content", want)
			m.sawSkillPolicy = request.Tools["write"] != nil && request.Tools["edit"] != nil && request.Tools["grep"] != nil && request.Tools["read"] == nil && request.Tools["load_skill"] == nil
			if !m.sawSkill || !m.sawSkillPolicy {
				encoded, _ := json.Marshal(request.Contents)
				yield(nil, fmt.Errorf("skill arguments or nested tool-policy intersection did not reach the native request (body=%t policy=%t): %s", m.sawSkill, m.sawSkillPolicy, encoded))
				return
			}
			response.Content = releaseToolCall("release-write", "write", map[string]any{"path": "src/main.go", "content": "package bad\n"})
		case 4:
			m.sawWriteDiagnostics = releaseDiagnosticsPresent(request, "release-write")
			if !m.sawWriteDiagnostics {
				yield(nil, errors.New("successful native write result lacks current LSP diagnostics"))
				return
			}
			response.Content = releaseToolCall("release-edit", "edit", map[string]any{"path": "src/main.go", "old_text": "bad", "new_text": "good"})
		case 5:
			m.sawEditDiagnostics = releaseDiagnosticsPresent(request, "release-edit")
			if !m.sawEditDiagnostics {
				yield(nil, errors.New("successful native edit result lacks current LSP diagnostics"))
				return
			}
			response.Content = releaseToolCall("release-grep", "grep", map[string]any{"pattern": "release-needle", "path": "large.txt", "max_results": 70})
		case 6:
			m.sawElision = releaseFunctionResponseContains(request, "grep", "output", compaction.ElisionMarker)
			if !m.sawElision {
				yield(nil, errors.New("combined native history was not compacted deterministically"))
				return
			}
			response.Content = genai.NewContentFromText("combined complete", genai.RoleModel)
		default:
			yield(nil, fmt.Errorf("unexpected release model call %d", m.calls))
			return
		}
		m.calls++
		yield(response, nil)
	}
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
