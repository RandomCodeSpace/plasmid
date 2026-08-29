package plasmid_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	adkplugin "google.golang.org/adk/v2/plugin"
	"google.golang.org/genai"

	"github.com/RandomCodeSpace/plasmid"
	"github.com/RandomCodeSpace/plasmid/warning"
)

func TestNativeSkillFullTurnExpandsArgumentsAndNarrowsTools(t *testing.T) {
	workingDir := t.TempDir()
	skillRoot := filepath.Join(t.TempDir(), "skills")
	skillDir := filepath.Join(skillRoot, "review")
	if err := os.MkdirAll(skillDir, 0o700); err != nil {
		t.Fatal(err)
	}
	source := "---\nname: review\ndescription: Review code\narguments: [focus]\nallowed-tools: [read]\n---\nReview ${focus}, $1, and $ARGUMENTS in ${PROJECT_DIR}.\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := writeHarnessConfig(t, map[string]any{
		"skills":  map[string]any{"roots": []string{skillRoot}},
		"foreign": map[string]any{"enabled": false},
		"lsp":     map[string]any{"mode": "off"},
	})
	llm := &skillTurnModel{workingDir: workingDir}
	harness, err := plasmid.New(t.Context(), plasmid.WithModel(llm), plasmid.WithWorkingDir(workingDir), plasmid.WithSessionDir(filepath.Join(t.TempDir(), "sessions")), plasmid.WithConfig(configPath))
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestResource(t, harness)
	sessionID, err := harness.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	answer, err := harness.Ask(t.Context(), sessionID, "load review")
	if err != nil || answer != testDoneResponse || llm.calls != 2 {
		t.Fatalf("Ask = %q, calls = %d, err = %v", answer, llm.calls, err)
	}
}

func TestTemplateAPIsUseFilenameIdentityExpansionAndNormalRun(t *testing.T) {
	workingDir := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	prompts := filepath.Join(home, ".codex", "prompts")
	if err := os.MkdirAll(prompts, 0o700); err != nil {
		t.Fatal(err)
	}
	source := "---\nname: ignored\nunknown-field: ignored\nallowed-tools: [read]\n---\nTemplate $ARGUMENTS in ${PROJECT_DIR}.\n"
	if err := os.WriteFile(filepath.Join(prompts, "review.md"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := writeHarnessConfig(t, map[string]any{
		"foreign": map[string]any{"enabled": true, "claude": false, "codex": true, "copilot": false},
		"lsp":     map[string]any{"mode": "off"},
	})
	llm := &templateTurnModel{wantPrompt: "Template security in " + workingDir + ".\n"}
	harness, err := plasmid.New(t.Context(), plasmid.WithModel(llm), plasmid.WithWorkingDir(workingDir), plasmid.WithSessionDir(filepath.Join(t.TempDir(), "sessions")), plasmid.WithConfig(configPath))
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestResource(t, harness)
	sessionID, err := harness.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	templates, err := harness.ListTemplates(t.Context(), sessionID)
	if err != nil || len(templates) != 1 || templates[0].Name != "review" {
		t.Fatalf("templates = %#v, err = %v", templates, err)
	}
	prompt, err := harness.GetTemplate(t.Context(), sessionID, "review", "security")
	if err != nil || prompt != llm.wantPrompt {
		t.Fatalf("GetTemplate = %q, err = %v", prompt, err)
	}
	answer, err := harness.AskTemplate(t.Context(), sessionID, "review", "security")
	if err != nil || answer != "template done" {
		t.Fatalf("AskTemplate = %q, err = %v", answer, err)
	}
	if notices := harness.Warnings(); len(notices) == 0 {
		t.Fatal("template warning was not reported")
	}
}

func TestResumeSessionRefreshesExtensionSnapshot(t *testing.T) {
	workingDir := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	prompts := filepath.Join(home, ".codex", "prompts")
	if err := os.MkdirAll(prompts, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prompts, "first.md"), []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := writeHarnessConfig(t, map[string]any{
		"foreign": map[string]any{"enabled": true, "claude": false, "codex": true, "copilot": false},
		"lsp":     map[string]any{"mode": "off"},
	})
	harness, err := plasmid.New(t.Context(), plasmid.WithModel(&instructionModel{}), plasmid.WithWorkingDir(workingDir), plasmid.WithSessionDir(filepath.Join(t.TempDir(), "sessions")), plasmid.WithConfig(configPath))
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestResource(t, harness)
	sessionID, err := harness.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if templates, err := harness.ListTemplates(t.Context(), sessionID); err != nil || len(templates) != 1 {
		t.Fatalf("initial templates = %#v, err = %v", templates, err)
	}
	if err := os.WriteFile(filepath.Join(prompts, "second.md"), []byte("second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := harness.ResumeSession(t.Context(), sessionID); err != nil {
		t.Fatal(err)
	}
	templates, err := harness.ListTemplates(t.Context(), sessionID)
	if err != nil || len(templates) != 2 || templates[0].Name != "first" || templates[1].Name != "second" {
		t.Fatalf("refreshed templates = %#v, err = %v", templates, err)
	}
}

func TestCompiledPluginIntegration(t *testing.T) {
	tests := []struct {
		name string
		run  func(*testing.T)
	}{
		{name: "fragments warnings and callback panics", run: testCompiledPluginFragmentsWarningsAndCallbackPanicsAreIsolated},
		{name: "prompt fragment collision", run: testCompiledPluginPromptFragmentCollisionIsRejected},
	}
	for _, test := range tests {
		t.Run(test.name, test.run)
	}
}

func testCompiledPluginFragmentsWarningsAndCallbackPanicsAreIsolated(t *testing.T) {
	workingDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workingDir, "AGENTS.md"), []byte("built-in instruction\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	panickingCallback := newPanickingInstructionCallback(t)
	compiled := &compiledPlugin{name: "compiled", init: func(h *plasmid.Harness) error {
		return registerCompiledPluginMetadata(h, panickingCallback)
	}}
	llm := &instructionModel{}
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	harness, err := plasmid.New(t.Context(), plasmid.WithModel(llm), plasmid.WithWorkingDir(workingDir), plasmid.WithSessionDir(filepath.Join(t.TempDir(), "sessions")), plasmid.WithPlugins(compiled), plasmid.WithLogger(logger), plasmid.WithLSP(plasmid.LSPOff))
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestResource(t, harness)
	sessionID, err := harness.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_, callbackErr := harness.Ask(t.Context(), sessionID, "hello")
	assertPluginPanicOutputs(t, callbackErr, harness.Warnings(), logs.String())
}

func newPanickingInstructionCallback(t *testing.T) *adkplugin.Plugin {
	t.Helper()
	panickingCallback, err := adkplugin.New(adkplugin.Config{
		Name: "panic-callback",
		BeforeModelCallback: func(_ agent.Context, request *model.LLMRequest) (*model.LLMResponse, error) {
			instruction := instructionText(request)
			builtIn := strings.Index(instruction, "built-in instruction")
			plugin := strings.Index(instruction, "plugin instruction")
			if builtIn < 0 || plugin <= builtIn {
				return nil, errors.New("plugin prompt fragment did not follow built-in instructions")
			}
			panic("TOPSECRET")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return panickingCallback
}

func instructionText(request *model.LLMRequest) string {
	if request.Config == nil || request.Config.SystemInstruction == nil {
		return ""
	}
	var result strings.Builder
	for _, part := range request.Config.SystemInstruction.Parts {
		result.WriteString(part.Text)
	}
	return result.String()
}

func registerCompiledPluginMetadata(h *plasmid.Harness, callback *adkplugin.Plugin) error {
	if err := h.RegisterPromptFragments(plasmid.PromptFragment{Name: "rules", Content: "plugin instruction"}); err != nil {
		return err
	}
	if err := h.RegisterWarnings(warning.Warning{Code: "plugin.notice", Message: "notice"}); err != nil {
		return err
	}
	return h.RegisterADKPlugins(callback)
}

func assertPluginPanicOutputs(t *testing.T, callbackErr error, notices []warning.Warning, logs string) {
	t.Helper()
	if callbackErr == nil || strings.Contains(callbackErr.Error(), "TOPSECRET") {
		t.Fatalf("callback panic error = %v", callbackErr)
	}
	encoded, err := json.Marshal(notices)
	if err != nil {
		t.Fatal(err)
	}
	warningJSON := string(encoded)
	if !strings.Contains(warningJSON, "plugin.notice") || !strings.Contains(warningJSON, warning.WarnPluginCallbackPanic) || strings.Contains(warningJSON, "TOPSECRET") {
		t.Fatalf("warnings = %s", warningJSON)
	}
	if !strings.Contains(logs, warning.WarnPluginCallbackPanic) || strings.Contains(logs, "TOPSECRET") {
		t.Fatalf("warning log = %q", logs)
	}
}

func testCompiledPluginPromptFragmentCollisionIsRejected(t *testing.T) {
	plugin := func(name string) *compiledPlugin {
		return &compiledPlugin{name: name, init: func(h *plasmid.Harness) error {
			return h.RegisterPromptFragments(plasmid.PromptFragment{Name: "duplicate", Content: name})
		}}
	}
	_, err := plasmid.New(t.Context(),
		plasmid.WithModel(&instructionModel{}), plasmid.WithWorkingDir(t.TempDir()),
		plasmid.WithSessionDir(filepath.Join(t.TempDir(), "sessions")), plasmid.WithLSP(plasmid.LSPOff),
		plasmid.WithPlugins(plugin("first"), plugin("second")),
	)
	if plasmid.CodeOf(err) != plasmid.CodeDuplicate || !errors.Is(err, plasmid.ErrDuplicate) {
		t.Fatalf("New error = %v, code = %q", err, plasmid.CodeOf(err))
	}
}

func TestHarnessFormattingRedactsResolvedExtensionSecrets(t *testing.T) {
	configPath := writeHarnessConfig(t, map[string]any{
		"foreign": map[string]any{"enabled": false},
		"lsp":     map[string]any{"mode": "off"},
		"mcp": map[string]any{"servers": []map[string]any{{
			"id": "secret", "transport": "http", "url": "https://example.invalid/mcp?token=TOPSECRET",
			"headers": map[string]string{"Authorization": "Bearer TOPSECRET"},
		}}},
	})
	harness, err := plasmid.New(t.Context(), plasmid.WithModel(&instructionModel{}), plasmid.WithWorkingDir(t.TempDir()), plasmid.WithSessionDir(filepath.Join(t.TempDir(), "sessions")), plasmid.WithConfig(configPath))
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestResource(t, harness)
	var logged bytes.Buffer
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("harness", "value", harness)
	for name, value := range map[string]string{
		"v": fmt.Sprintf("%v", harness), "plus": fmt.Sprintf("%+v", harness),
		"sharp": fmt.Sprintf("%#v", harness), "slog": logged.String(),
	} {
		if strings.Contains(value, "TOPSECRET") || strings.Contains(value, "Bearer") {
			t.Fatalf("%s formatting leaked extension secret: %s", name, value)
		}
	}
}

func TestHarnessCloseCancelsAndWaitsForGetTemplateCommand(t *testing.T) {
	workingDir := t.TempDir()
	commandDir := filepath.Join(workingDir, ".claude", "commands")
	if err := os.MkdirAll(commandDir, 0o700); err != nil {
		t.Fatal(err)
	}
	started := filepath.Join(t.TempDir(), "started")
	finished := filepath.Join(t.TempDir(), "finished")
	source := fmt.Sprintf("!`printf started > %q; sleep 30; printf finished > %q`\n", started, finished)
	if err := os.WriteFile(filepath.Join(commandDir, "wait.md"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := writeHarnessConfig(t, map[string]any{
		"foreign": map[string]any{"enabled": true, "claude": true, "codex": false, "copilot": false},
		"syntax":  map[string]any{"promptCommands": "on", "commandTimeoutMs": 60000, "documentTimeoutMs": 60000},
		"lsp":     map[string]any{"mode": "off"},
	})
	harness, err := plasmid.New(t.Context(), plasmid.WithModel(&instructionModel{}), plasmid.WithWorkingDir(workingDir), plasmid.WithSessionDir(filepath.Join(t.TempDir(), "sessions")), plasmid.WithConfig(configPath))
	if err != nil {
		t.Fatal(err)
	}
	sessionID, err := harness.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	getDone := make(chan error, 1)
	go func() {
		_, err := harness.GetTemplate(context.Background(), sessionID, "wait", "")
		getDone <- err
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) || time.Now().After(deadline) {
			t.Fatalf("template command did not start: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := harness.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-getDone:
		if err == nil {
			t.Fatal("GetTemplate returned nil after cancellation")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Harness.Close returned before GetTemplate stopped")
	}
	if _, err := os.Stat(finished); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("template command survived Harness.Close: %v", err)
	}
}

func TestNativeMCPFullTurnStaysLazyUntilRun(t *testing.T) {
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "full-turn", Version: "1"}, nil)
	sdkmcp.AddTool(server, &sdkmcp.Tool{Name: "echo", Description: "echo input"}, func(_ context.Context, _ *sdkmcp.CallToolRequest, input struct {
		Value string `json:"value"`
	}) (*sdkmcp.CallToolResult, struct {
		Value string `json:"value"`
	}, error) {
		return nil, struct {
			Value string `json:"value"`
		}{Value: input.Value}, nil
	})
	stream := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return server }, nil)
	var requests atomic.Int64
	httpServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		stream.ServeHTTP(response, request)
	}))
	defer httpServer.Close()
	workingDir := t.TempDir()
	configPath := writeHarnessConfig(t, map[string]any{
		"foreign": map[string]any{"enabled": false},
		"lsp":     map[string]any{"mode": "off"},
		"mcp": map[string]any{"servers": []map[string]any{{
			"id": "remote", "transport": "http", "url": httpServer.URL,
		}}},
	})
	llm := &mcpTurnModel{}
	harness, err := plasmid.New(t.Context(), plasmid.WithModel(llm), plasmid.WithWorkingDir(workingDir), plasmid.WithSessionDir(filepath.Join(t.TempDir(), "sessions")), plasmid.WithConfig(configPath))
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestResource(t, harness)
	if requests.Load() != 0 {
		t.Fatalf("construction contacted MCP server: requests = %d", requests.Load())
	}
	sessionID, err := harness.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 0 {
		t.Fatalf("session discovery contacted MCP server: requests = %d", requests.Load())
	}
	answer, err := harness.Ask(t.Context(), sessionID, "use remote echo")
	if err != nil || answer != "mcp done" || llm.calls != 2 || requests.Load() == 0 {
		t.Fatalf("Ask = %q, calls = %d, requests = %d, err = %v", answer, llm.calls, requests.Load(), err)
	}
}

type skillTurnModel struct {
	workingDir string
	calls      int
}

func (*skillTurnModel) Name() string { return "skill-turn" }
func (m *skillTurnModel) GenerateContent(_ context.Context, request *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if m.calls == 0 {
			if request.Tools["load_skill"] == nil || request.Tools["read"] == nil {
				yield(nil, errors.New("skill or read tool missing from first model request"))
				return
			}
			m.calls++
			yield(&model.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "load-1", Name: "load_skill", Args: map[string]any{"name": "review", "arguments": "auth focus=security"}}}}}}, nil)
			return
		}
		if request.Tools["read"] == nil || request.Tools["load_skill"] != nil {
			yield(nil, errors.New("skill policy did not narrow the second model request"))
			return
		}
		want := "Review security, auth, and auth focus=security in " + m.workingDir + ".\n"
		if !functionResponseContains(request.Contents, "load_skill", want) {
			encoded, _ := json.Marshal(request.Contents)
			yield(nil, fmt.Errorf("expanded skill body missing from function response: %s", encoded))
			return
		}
		m.calls++
		yield(&model.LLMResponse{Content: genai.NewContentFromText(testDoneResponse, genai.RoleModel)}, nil)
	}
}

type templateTurnModel struct{ wantPrompt string }

func (*templateTurnModel) Name() string { return "template-turn" }
func (m *templateTurnModel) GenerateContent(_ context.Context, request *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if len(request.Contents) == 0 || request.Contents[len(request.Contents)-1].Parts[0].Text != m.wantPrompt {
			yield(nil, errors.New("template prompt did not use normal model request path"))
			return
		}
		if request.Tools["read"] == nil || request.Tools["write"] != nil {
			yield(nil, errors.New("template tool policy was not applied"))
			return
		}
		yield(&model.LLMResponse{Content: genai.NewContentFromText("template ", genai.RoleModel), Partial: true}, nil)
		yield(&model.LLMResponse{Content: genai.NewContentFromText("template done", genai.RoleModel)}, nil)
	}
}

type instructionModel struct{}

func (*instructionModel) Name() string { return "instruction" }
func (*instructionModel) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{Content: genai.NewContentFromText("unused", genai.RoleModel)}, nil)
	}
}

type mcpTurnModel struct{ calls int }

func (*mcpTurnModel) Name() string { return "mcp-turn" }
func (m *mcpTurnModel) GenerateContent(_ context.Context, request *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if m.calls == 0 {
			name := ""
			for candidate := range request.Tools {
				if strings.HasPrefix(candidate, "mcp__") && strings.HasSuffix(candidate, "__echo") {
					name = candidate
				}
			}
			if name == "" {
				yield(nil, errors.New("native MCP tool missing from model request"))
				return
			}
			m.calls++
			yield(&model.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "mcp-1", Name: name, Args: map[string]any{"value": "hello"}}}}}}, nil)
			return
		}
		if !functionResponseContainsValue(request.Contents, "structured_content", "value", "hello") {
			yield(nil, errors.New("MCP structured response missing from native turn"))
			return
		}
		m.calls++
		yield(&model.LLMResponse{Content: genai.NewContentFromText("mcp done", genai.RoleModel)}, nil)
	}
}

func functionResponseContains(contents []*genai.Content, name, want string) bool {
	for _, content := range contents {
		for _, part := range content.Parts {
			if part == nil || part.FunctionResponse == nil || part.FunctionResponse.Name != name {
				continue
			}
			if value, ok := part.FunctionResponse.Response["content"].(string); ok && value == want {
				return true
			}
		}
	}
	return false
}

func functionResponseContainsValue(contents []*genai.Content, objectKey, key, want string) bool {
	for _, content := range contents {
		for _, part := range content.Parts {
			if part == nil || part.FunctionResponse == nil {
				continue
			}
			object, ok := part.FunctionResponse.Response[objectKey].(map[string]any)
			if ok && object[key] == want {
				return true
			}
		}
	}
	return false
}

func writeHarnessConfig(t *testing.T, values map[string]any) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	data, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
