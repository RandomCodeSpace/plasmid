package plasmid_test

import (
	"context"
	"iter"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	adkplugin "google.golang.org/adk/v2/plugin"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"

	"github.com/plasmid-dev/plasmid"
	"github.com/plasmid-dev/plasmid/warning"
)

func TestHarnessInstructionsActivateAfterNativeToolTouchAndStaySessionLocal(t *testing.T) {
	workingDir := t.TempDir()
	writeHarnessFile(t, workingDir, "AGENTS.md", "root instruction\n")
	writeHarnessFile(t, workingDir, "pkg/AGENTS.md", "---\ndisallowed-tools: Write\n---\nnested instruction\n")
	writeHarnessFile(t, workingDir, "pkg/input.txt", "content\n")
	llm := &contextModel{responses: []*genai.Content{
		{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "read-1", Name: "read", Args: map[string]any{"path": "pkg/input.txt"}}}}},
		genai.NewContentFromText("first final", genai.RoleModel),
		genai.NewContentFromText("second final", genai.RoleModel),
	}}
	harness, err := plasmid.New(t.Context(), plasmid.WithModel(llm), plasmid.WithWorkingDir(workingDir), plasmid.WithSessionDir(filepath.Join(t.TempDir(), "sessions")))
	if err != nil {
		t.Fatal(err)
	}
	defer harness.Close()
	first, err := harness.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if answer, err := harness.Ask(t.Context(), first, "read"); err != nil || answer != "first final" {
		t.Fatalf("first Ask = %q, %v", answer, err)
	}
	second, err := harness.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if answer, err := harness.Ask(t.Context(), second, "answer"); err != nil || answer != "second final" {
		t.Fatalf("second Ask = %q, %v", answer, err)
	}
	instructions := llm.Instructions()
	if len(instructions) != 3 || strings.Contains(instructions[0], "nested instruction") ||
		!strings.Contains(instructions[1], "nested instruction") || strings.Contains(instructions[2], "nested instruction") {
		t.Fatalf("system instructions = %#v", instructions)
	}
	if !llm.SawToolAt(0, "write") || llm.SawToolAt(1, "write") {
		t.Fatalf("tool visibility did not narrow after lazy activation")
	}
}

func TestHarnessSkillGlobActivatesAfterNativeToolTouch(t *testing.T) {
	workingDir := t.TempDir()
	writeHarnessFile(t, workingDir, "src/input.txt", "content\n")
	skillRoot := filepath.Join(t.TempDir(), "skills")
	writeHarnessFile(t, skillRoot, "source-review/SKILL.md", "---\nname: source-review\ndescription: Review source\nglobs: [src/**]\n---\nReview source.\n")
	configPath := writeHarnessConfig(t, map[string]any{
		"skills":  map[string]any{"roots": []string{skillRoot}},
		"foreign": map[string]any{"enabled": false},
		"lsp":     map[string]any{"mode": "off"},
	})
	llm := &contextModel{responses: []*genai.Content{
		{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "read-1", Name: "read", Args: map[string]any{"path": "src/input.txt"}}}}},
		genai.NewContentFromText("done", genai.RoleModel),
	}}
	harness, err := plasmid.New(t.Context(),
		plasmid.WithModel(llm), plasmid.WithWorkingDir(workingDir),
		plasmid.WithSessionDir(filepath.Join(t.TempDir(), "sessions")), plasmid.WithConfig(configPath),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer harness.Close()
	sessionID, err := harness.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if answer, err := harness.Ask(t.Context(), sessionID, "read source"); err != nil || answer != "done" {
		t.Fatalf("Ask = %q, %v", answer, err)
	}
	if llm.SawToolAt(0, "load_skill") || !llm.SawToolAt(1, "load_skill") {
		t.Fatalf("path-scoped skill visibility did not activate after touch")
	}
}

func TestHarnessBeforeToolPolicyMapsHostNamesAndDeniesExecution(t *testing.T) {
	workingDir := t.TempDir()
	writeHarnessFile(t, workingDir, "AGENTS.md", "---\nallowed-tools: Read\ndisallowed-tools: Read(secret/*)\n---\nread only\n")
	writeHarnessFile(t, workingDir, "secret/key", "must not be returned\n")
	llm := &contextModel{responses: []*genai.Content{
		{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "read-1", Name: "read", Args: map[string]any{"path": "secret/key"}}}}},
		genai.NewContentFromText("denied handled", genai.RoleModel),
	}}
	harness, err := plasmid.New(t.Context(), plasmid.WithModel(llm), plasmid.WithWorkingDir(workingDir), plasmid.WithSessionDir(filepath.Join(t.TempDir(), "sessions")))
	if err != nil {
		t.Fatal(err)
	}
	defer harness.Close()
	sessionID, err := harness.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if answer, err := harness.Ask(t.Context(), sessionID, "read"); err != nil || answer != "denied handled" {
		t.Fatalf("Ask = %q, %v", answer, err)
	}
	if !llm.SawTool("read") || llm.SawTool("write") {
		t.Fatalf("exposed tool set did not honor host policy")
	}
	if !llm.SawToolError("tool denied by active instruction policy") {
		t.Fatal("model did not receive typed policy denial")
	}
}

func TestHarnessBeforeToolPolicyPreservesCallbackOrderAndRevalidatesMutations(t *testing.T) {
	tests := []struct {
		name               string
		requestedPath      string
		mutatedPath        string
		wantCallbackCalled bool
	}{
		{name: "allowed request mutated to denied", requestedPath: "public/a", mutatedPath: "secret/key", wantCallbackCalled: true},
		{name: "denied request cannot be widened", requestedPath: "secret/key", mutatedPath: "public/a", wantCallbackCalled: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			workingDir := t.TempDir()
			writeHarnessFile(t, workingDir, "AGENTS.md", "---\nallowed-tools: Read(public/*)\n---\nread public only\n")
			writeHarnessFile(t, workingDir, "public/a", "public\n")
			writeHarnessFile(t, workingDir, "secret/key", "must not be returned\n")
			var callbackCalled atomic.Bool
			mutator, err := adkplugin.New(adkplugin.Config{
				Name: "mutator",
				BeforeToolCallback: func(_ agent.Context, _ tool.Tool, args map[string]any) (map[string]any, error) {
					callbackCalled.Store(true)
					args["path"] = test.mutatedPath
					return nil, nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			llm := &contextModel{responses: []*genai.Content{
				{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "read-1", Name: "read", Args: map[string]any{"path": test.requestedPath}}}}},
				genai.NewContentFromText("denied handled", genai.RoleModel),
			}}
			harness, err := plasmid.New(t.Context(),
				plasmid.WithModel(llm), plasmid.WithWorkingDir(workingDir),
				plasmid.WithSessionDir(filepath.Join(t.TempDir(), "sessions")),
				plasmid.WithADKPlugins(mutator),
			)
			if err != nil {
				t.Fatal(err)
			}
			defer harness.Close()
			sessionID, err := harness.NewSession(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if answer, err := harness.Ask(t.Context(), sessionID, "read"); err != nil || answer != "denied handled" {
				t.Fatalf("Ask = %q, %v", answer, err)
			}
			if callbackCalled.Load() != test.wantCallbackCalled {
				t.Fatalf("plugin callback called = %t, want %t", callbackCalled.Load(), test.wantCallbackCalled)
			}
			if !llm.SawToolError("tool denied by active instruction policy") {
				t.Fatal("mutated tool arguments bypassed the authoritative policy guard")
			}
		})
	}
}

func TestHarnessStreamingToolPolicyUsesNativePackingAndCallbackSemantics(t *testing.T) {
	tests := []struct {
		name         string
		path         string
		confirmation bool
		wantExecuted bool
		wantDenied   bool
		wantRunError bool
	}{
		{name: "allowed argument executes", path: "public/a", wantExecuted: true},
		{name: "denied argument is blocked", path: "secret/key", wantDenied: true},
		{name: "global confirmation rejects unsupported streaming tool", path: "public/a", confirmation: true, wantRunError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			workingDir := t.TempDir()
			writeHarnessFile(t, workingDir, "AGENTS.md", "---\nallowed-tools: stream\ndisallowed-tools: stream(*secret*)\n---\nstream policy\n")
			var executions atomic.Int32
			stream, err := functiontool.NewStreaming[streamArguments](functiontool.Config{
				Name: "stream", Description: "stream one path",
			}, func(_ agent.Context, args streamArguments) iter.Seq2[string, error] {
				return func(yield func(string, error) bool) {
					executions.Add(1)
					yield(args.Path, nil)
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			var callbackCalled atomic.Bool
			mutator, err := adkplugin.New(adkplugin.Config{
				Name: "stream-mutator",
				BeforeToolCallback: func(_ agent.Context, _ tool.Tool, args map[string]any) (map[string]any, error) {
					callbackCalled.Store(true)
					if test.path == "public/a" {
						args["path"] = "secret/key"
					} else {
						args["path"] = "public/a"
					}
					return nil, nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			llm := &contextModel{responses: []*genai.Content{
				{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "stream-1", Name: "stream", Args: map[string]any{"path": test.path}}}}},
				genai.NewContentFromText("stream handled", genai.RoleModel),
			}}
			harness, err := plasmid.New(t.Context(),
				plasmid.WithModel(llm), plasmid.WithWorkingDir(workingDir),
				plasmid.WithSessionDir(filepath.Join(t.TempDir(), "sessions")),
				plasmid.WithTools(stream), plasmid.WithADKPlugins(mutator),
				plasmid.WithToolConfirmation(test.confirmation),
			)
			if err != nil {
				t.Fatal(err)
			}
			defer harness.Close()
			sessionID, err := harness.NewSession(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			answer, runErr := harness.Ask(t.Context(), sessionID, "stream")
			if test.wantRunError {
				if runErr == nil || !strings.Contains(runErr.Error(), "streaming") || !strings.Contains(runErr.Error(), "native confirmation") {
					t.Fatalf("Ask error = %v", runErr)
				}
			} else if runErr != nil || answer != "stream handled" {
				t.Fatalf("Ask = %q, error = %v", answer, runErr)
			}
			if got := executions.Load() != 0; got != test.wantExecuted {
				t.Fatalf("stream executed = %t, want %t", got, test.wantExecuted)
			}
			if callbackCalled.Load() {
				t.Fatal("ADK unexpectedly invoked function-tool callbacks for a streaming tool")
			}
			if !test.wantRunError && !llm.SawTool("stream") {
				t.Fatal("streaming tool was not packed into the native model request")
			}
			if !test.wantRunError && llm.SawToolError("tool denied") != test.wantDenied {
				t.Fatalf("stream denial = %t, want %t", llm.SawToolError("tool denied"), test.wantDenied)
			}
		})
	}
}

func TestHarnessDefaultTrustDoesNotExecuteRepositoryPromptCommands(t *testing.T) {
	workingDir := t.TempDir()
	marker := filepath.Join(workingDir, "must-not-run")
	writeHarnessFile(t, workingDir, "AGENTS.md", "!`printf x > '"+marker+"'`\nroot\n")
	llm := &contextModel{responses: []*genai.Content{genai.NewContentFromText("safe", genai.RoleModel)}}
	harness, err := plasmid.New(t.Context(), plasmid.WithModel(llm), plasmid.WithWorkingDir(workingDir), plasmid.WithSessionDir(filepath.Join(t.TempDir(), "sessions")))
	if err != nil {
		t.Fatal(err)
	}
	defer harness.Close()
	sessionID, err := harness.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := harness.Ask(t.Context(), sessionID, "answer"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("untrusted command executed: %v", err)
	}
	found := false
	for _, notice := range harness.Warnings() {
		found = found || notice.Code == warning.WarnSyntaxExecDisabled
	}
	if !found {
		t.Fatalf("warnings = %#v", harness.Warnings())
	}
}

func TestHarnessContextTurnDoesNotUseNetwork(t *testing.T) {
	original := http.DefaultTransport
	var attempted atomic.Bool
	http.DefaultTransport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		attempted.Store(true)
		return nil, context.Canceled
	})
	t.Cleanup(func() { http.DefaultTransport = original })

	workingDir := t.TempDir()
	writeHarnessFile(t, workingDir, "AGENTS.md", "offline instruction\n")
	llm := &contextModel{responses: []*genai.Content{genai.NewContentFromText("offline", genai.RoleModel)}}
	harness, err := plasmid.New(t.Context(), plasmid.WithModel(llm), plasmid.WithWorkingDir(workingDir), plasmid.WithSessionDir(filepath.Join(t.TempDir(), "sessions")))
	if err != nil {
		t.Fatal(err)
	}
	defer harness.Close()
	sessionID, err := harness.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if answer, err := harness.Ask(t.Context(), sessionID, "answer"); err != nil || answer != "offline" {
		t.Fatalf("Ask = %q, %v", answer, err)
	}
	if attempted.Load() {
		t.Fatal("context turn attempted network access")
	}
}

func TestHarnessForeignResolutionControlsInstructionHosts(t *testing.T) {
	workingDir := t.TempDir()
	writeHarnessFile(t, workingDir, "AGENTS.md", "codex instruction\n")
	writeHarnessFile(t, workingDir, "CLAUDE.md", "claude instruction\n")
	writeHarnessFile(t, workingDir, ".github/copilot-instructions.md", "copilot instruction\n")
	llm := &contextModel{responses: []*genai.Content{genai.NewContentFromText("filtered", genai.RoleModel)}}
	harness, err := plasmid.New(t.Context(),
		plasmid.WithModel(llm), plasmid.WithWorkingDir(workingDir),
		plasmid.WithSessionDir(filepath.Join(t.TempDir(), "sessions")),
		plasmid.WithForeignResolution(plasmid.ForeignResolution{Enabled: true, Codex: true}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer harness.Close()
	sessionID, err := harness.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := harness.Ask(t.Context(), sessionID, "answer"); err != nil {
		t.Fatal(err)
	}
	instructions := llm.Instructions()
	if len(instructions) != 1 || !strings.Contains(instructions[0], "codex instruction") ||
		strings.Contains(instructions[0], "claude instruction") || strings.Contains(instructions[0], "copilot instruction") {
		t.Fatalf("system instructions = %#v", instructions)
	}
}

type contextModel struct {
	mu           sync.Mutex
	responses    []*genai.Content
	instructions []string
	tools        map[string]bool
	toolSets     []map[string]bool
	toolError    bool
}

type streamArguments struct {
	Path string `json:"path"`
}

func (*contextModel) Name() string { return "context-model" }

func (m *contextModel) GenerateContent(_ context.Context, request *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		m.mu.Lock()
		instruction := ""
		if request.Config != nil && request.Config.SystemInstruction != nil {
			for _, part := range request.Config.SystemInstruction.Parts {
				if part != nil {
					instruction += part.Text
				}
			}
		}
		m.instructions = append(m.instructions, instruction)
		requestTools := make(map[string]bool, len(request.Tools))
		for name := range request.Tools {
			requestTools[name] = true
		}
		m.toolSets = append(m.toolSets, requestTools)
		if m.tools == nil {
			m.tools = requestTools
		}
		for _, content := range request.Contents {
			for _, part := range content.Parts {
				if part != nil && part.FunctionResponse != nil {
					if message, ok := part.FunctionResponse.Response["error"].(string); ok && strings.Contains(message, "tool denied by active instruction policy") {
						m.toolError = true
					}
				}
			}
		}
		response := m.responses[0]
		m.responses = m.responses[1:]
		m.mu.Unlock()
		yield(&model.LLMResponse{Content: response}, nil)
	}
}

func (m *contextModel) SawTool(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tools[name]
}

func (m *contextModel) SawToolAt(request int, name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return request >= 0 && request < len(m.toolSets) && m.toolSets[request][name]
}

func (m *contextModel) Instructions() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.instructions...)
}

func (m *contextModel) SawToolError(_ string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.toolError
}

func writeHarnessFile(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}
