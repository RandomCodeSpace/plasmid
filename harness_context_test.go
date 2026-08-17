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

	"google.golang.org/adk/v2/model"
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
