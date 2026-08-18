package plasmid

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"go.lsp.dev/protocol"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	adkplugin "google.golang.org/adk/v2/plugin"
	adktool "google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"github.com/plasmid-dev/plasmid/codingtools"
	"github.com/plasmid-dev/plasmid/lsp"
)

var harnessLSPHelperMarker = flag.String("plasmid-lsp-helper-marker", "", "fake LSP start marker")

type harnessLSPServerConnection struct{}

func (harnessLSPServerConnection) Read(value []byte) (int, error)  { return os.Stdin.Read(value) }
func (harnessLSPServerConnection) Write(value []byte) (int, error) { return os.Stdout.Write(value) }
func (harnessLSPServerConnection) Close() error                    { return nil }

func TestHarnessLSPServerHelper(t *testing.T) {
	if *harnessLSPHelperMarker == "" {
		t.Skip("helper process")
	}
	if err := os.WriteFile(*harnessLSPHelperMarker, []byte("started"), 0o644); err != nil {
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
			if opened.TextDocument.LanguageID != "go" {
				return nil, nil
			}
			err := transport.Notify(ctx, "textDocument/publishDiagnostics", protocol.PublishDiagnosticsParams{
				URI: opened.TextDocument.URI, Version: protocol.NewOptional(opened.TextDocument.Version),
				Diagnostics: []protocol.Diagnostic{{
					Range:    protocol.Range{Start: protocol.Position{Line: 0, Character: 8}, End: protocol.Position{Line: 0, Character: 12}},
					Severity: protocol.DiagnosticSeverityError, Code: protocol.String("E1"),
					Source: protocol.NewOptional("fake-gopls"), Message: protocol.String("invalid package"),
				}},
			})
			return nil, err
		case "shutdown":
			return nil, nil
		}
		return nil, nil
	}
	var err error
	transport, err = lsp.NewRPCTransport(context.Background(), harnessLSPServerConnection{}, lsp.DefaultMaxMessageBytes, handler)
	if err != nil {
		t.Fatal(err)
	}
	defer transport.Close()
	<-transport.Done()
}

type harnessLSPModel struct {
	calls        int
	statuses     []string
	toolResponse map[string]any
}

type harnessLSPEditModel struct {
	calls        int
	toolResponse map[string]any
}

func (*harnessLSPEditModel) Name() string { return "harness-lsp-edit" }

func (modelValue *harnessLSPEditModel) GenerateContent(_ context.Context, request *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		switch modelValue.calls {
		case 0:
			modelValue.calls++
			yield(&model.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
				ID: "read-1", Name: "read", Args: map[string]any{"path": "main.go"},
			}}}}}, nil)
		case 1:
			modelValue.calls++
			yield(&model.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
				ID: "edit-1", Name: "edit", Args: map[string]any{"path": "main.go", "old_text": "package old", "new_text": "package bad"},
			}}}}}, nil)
		default:
			for _, content := range request.Contents {
				for _, part := range content.Parts {
					if part != nil && part.FunctionResponse != nil && part.FunctionResponse.Name == "edit" {
						modelValue.toolResponse = part.FunctionResponse.Response
					}
				}
			}
			modelValue.calls++
			yield(&model.LLMResponse{Content: genai.NewContentFromText("done", genai.RoleModel)}, nil)
		}
	}
}

func harnessLSPConfig(t *testing.T) (string, string) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	startMarker := filepath.Join(t.TempDir(), "started")
	configuration := map[string]any{
		"version": 1,
		"lsp": map[string]any{
			"settleTimeoutMs": 1000,
			"servers": []map[string]any{{
				"id": "gopls", "command": executable,
				"args":       []string{"-test.run=^TestHarnessLSPServerHelper$", "-plasmid-lsp-helper-marker=" + startMarker},
				"extensions": []string{".go"}, "rootMarkers": []string{"go.mod"},
			}},
		},
	}
	encodedConfiguration, err := json.Marshal(configuration)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, encodedConfiguration, 0o644); err != nil {
		t.Fatal(err)
	}
	return configPath, startMarker
}

func (*harnessLSPModel) Name() string { return "harness-lsp" }

func (modelValue *harnessLSPModel) GenerateContent(_ context.Context, request *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		status := ""
		if request.Config != nil && request.Config.SystemInstruction != nil {
			for _, part := range request.Config.SystemInstruction.Parts {
				if part != nil {
					status += part.Text
				}
			}
		}
		modelValue.statuses = append(modelValue.statuses, status)
		if modelValue.calls == 0 {
			modelValue.calls++
			yield(&model.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
				ID: "write-1", Name: "write", Args: map[string]any{"path": "main.go", "content": "package bad\n"},
			}}}}}, nil)
			return
		}
		for _, content := range request.Contents {
			for _, part := range content.Parts {
				if part != nil && part.FunctionResponse != nil && part.FunctionResponse.Name == "write" {
					modelValue.toolResponse = part.FunctionResponse.Response
				}
			}
		}
		modelValue.calls++
		yield(&model.LLMResponse{Content: genai.NewContentFromText("done", genai.RoleModel)}, nil)
	}
}

func TestHarnessNativeTurnInjectsCurrentLSPDiagnosticsAndPromptStatus(t *testing.T) {
	workingDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workingDir, "go.mod"), []byte("module example.test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	configPath, startMarker := harnessLSPConfig(t)
	var pluginSawDiagnostics atomic.Bool
	nativePlugin, err := adkplugin.New(adkplugin.Config{
		Name: "observe-lsp",
		AfterToolCallback: func(_ agent.Context, current adktool.Tool, _ map[string]any, result map[string]any, err error) (map[string]any, error) {
			if err == nil && current.Name() == "write" && result[codingtools.DiagnosticsResultKey] != nil {
				pluginSawDiagnostics.Store(true)
			}
			return nil, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	modelValue := &harnessLSPModel{}
	harness, err := New(t.Context(),
		WithModel(modelValue), WithWorkingDir(workingDir), WithSessionDir(filepath.Join(t.TempDir(), "sessions")),
		WithConfig(configPath), WithADKPlugins(nativePlugin),
	)
	if err != nil {
		t.Fatal(err)
	}
	sessionID, err := harness.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	answer, err := harness.Ask(t.Context(), sessionID, "write invalid Go")
	if err != nil || answer != "done" {
		t.Fatalf("Ask = %q, %v", answer, err)
	}
	encoded, _ := json.Marshal(modelValue.toolResponse[codingtools.DiagnosticsResultKey])
	if !strings.Contains(string(encoded), `"message":"invalid package"`) || modelValue.toolResponse[codingtools.DiagnosticsTextResultKey] != "main.go:1:9: error E1 (fake-gopls): invalid package" {
		t.Fatalf("tool response = %#v", modelValue.toolResponse)
	}
	if len(modelValue.statuses) != 2 || !strings.Contains(modelValue.statuses[0], "LSP: none detected") || !strings.Contains(modelValue.statuses[1], "LSP: gopls") {
		t.Fatalf("prompt statuses = %#v", modelValue.statuses)
	}
	if !pluginSawDiagnostics.Load() {
		t.Fatal("plugin after-tool callback did not observe built-in LSP decoration")
	}
	if _, err := os.Stat(startMarker); err != nil {
		t.Fatal("fake language server did not start lazily")
	}
	manager := harness.lspManager
	if err := harness.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Start(t.Context(), "gopls", workingDir); !errors.Is(err, lsp.ErrManagerClosed) {
		t.Fatalf("LSP manager remained open after Harness.Close: %v", err)
	}
}

func TestHarnessNativeEditTurnInjectsCurrentLSPDiagnostics(t *testing.T) {
	workingDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workingDir, "go.mod"), []byte("module example.test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workingDir, "main.go"), []byte("package old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	configPath, startMarker := harnessLSPConfig(t)
	modelValue := &harnessLSPEditModel{}
	harness, err := New(t.Context(),
		WithModel(modelValue), WithWorkingDir(workingDir), WithSessionDir(filepath.Join(t.TempDir(), "sessions")), WithConfig(configPath),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = harness.Close() })
	sessionID, err := harness.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	answer, err := harness.Ask(t.Context(), sessionID, "read then edit invalid Go")
	if err != nil || answer != "done" {
		t.Fatalf("Ask = %q, %v", answer, err)
	}
	encoded, _ := json.Marshal(modelValue.toolResponse[codingtools.DiagnosticsResultKey])
	if !strings.Contains(string(encoded), `"message":"invalid package"`) || modelValue.toolResponse[codingtools.DiagnosticsTextResultKey] == nil {
		t.Fatalf("edit tool response = %#v", modelValue.toolResponse)
	}
	if _, err := os.Stat(startMarker); err != nil {
		t.Fatal("fake language server did not start lazily for edit")
	}
}

func TestHarnessLSPOffOmitsStatusAndRuntime(t *testing.T) {
	modelValue := &harnessLSPModel{calls: 1}
	harness, err := New(t.Context(),
		WithModel(modelValue), WithWorkingDir(t.TempDir()), WithSessionDir(filepath.Join(t.TempDir(), "sessions")), WithLSP(LSPOff),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer harness.Close()
	if harness.lspManager != nil || harness.lspEnforcer != nil {
		t.Fatalf("off runtime = manager %#v, enforcer %#v", harness.lspManager, harness.lspEnforcer)
	}
	sessionID, err := harness.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := harness.Ask(t.Context(), sessionID, "hello"); err != nil {
		t.Fatal(err)
	}
	if len(modelValue.statuses) != 1 || strings.Contains(modelValue.statuses[0], "LSP:") {
		t.Fatalf("off prompt statuses = %#v", modelValue.statuses)
	}
}

var _ model.LLM = (*harnessLSPModel)(nil)
var _ model.LLM = (*harnessLSPEditModel)(nil)
var _ io.ReadWriteCloser = harnessLSPServerConnection{}
