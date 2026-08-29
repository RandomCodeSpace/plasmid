package plasmid_test

import (
	"context"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/RandomCodeSpace/plasmid"
	"github.com/RandomCodeSpace/plasmid/compaction"
)

func TestHarnessCompactionPersistsAcrossThreeTurnsAndResetsToolBudget(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	workingDir := t.TempDir()
	sessionDir := filepath.Join(t.TempDir(), "sessions")
	var lines []string
	for index := range 70 {
		lines = append(lines, fmt.Sprintf("needle-%03d payload", index))
	}
	if err := os.WriteFile(filepath.Join(workingDir, "large.txt"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "config.json")
	configuration := `{
  "compaction": {
    "calibration": true,
    "contextTokens": 1,
    "keepRecentContents": 0,
    "minimumElisionTokens": 1,
    "preserveToolNames": ["read"],
    "targetFraction": 0.1,
    "triggerFraction": 0.5
  },
  "tools": {
    "callOutputBytes": 5000,
    "sessionOutputBytes": 3000
  },
  "version": 1
}`
	if err := os.WriteFile(configPath, []byte(configuration), 0o600); err != nil {
		t.Fatal(err)
	}

	sessionID := exerciseInitialCompactionTurns(t, workingDir, sessionDir, configPath)
	exerciseResumedCompactionTurn(t, workingDir, sessionDir, configPath, sessionID)
}

func exerciseInitialCompactionTurns(t *testing.T, workingDir, sessionDir, configPath string) string {
	t.Helper()
	firstModel := &compactionHarnessModel{phase: "first"}
	first := newCompactionHarness(t, workingDir, sessionDir, configPath, firstModel)
	sessionID, err := first.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	answer, err := first.Ask(t.Context(), sessionID, "first turn")
	if err != nil || answer != "first complete" {
		t.Fatalf("first Ask = %q, %v", answer, err)
	}
	answer, err = first.Ask(t.Context(), sessionID, "second turn")
	if err != nil || answer != "second complete" {
		t.Fatalf("second Ask = %q, %v", answer, err)
	}
	if firstModel.calls != 4 || !firstModel.sawElision || !firstModel.sawFullRead {
		t.Fatalf("first model calls=%d elision=%v fullRead=%v", firstModel.calls, firstModel.sawElision, firstModel.sawFullRead)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	return sessionID
}

func exerciseResumedCompactionTurn(t *testing.T, workingDir, sessionDir, configPath, sessionID string) {
	t.Helper()
	resumedModel := &compactionHarnessModel{phase: "resumed"}
	second := newCompactionHarness(t, workingDir, sessionDir, configPath, resumedModel)
	defer closeTestResource(t, second)
	if err := second.ResumeSession(t.Context(), sessionID); err != nil {
		t.Fatal(err)
	}
	answer, err := second.Ask(t.Context(), sessionID, "third turn")
	if err != nil || answer != "third complete" {
		t.Fatalf("third Ask = %q, %v", answer, err)
	}
	if resumedModel.calls != 1 || !resumedModel.sawElision {
		t.Fatalf("resumed model calls=%d elision=%v", resumedModel.calls, resumedModel.sawElision)
	}
}

func newCompactionHarness(t *testing.T, workingDir, sessionDir, configPath string, model model.LLM) *plasmid.Harness {
	t.Helper()
	harness, err := plasmid.New(t.Context(),
		plasmid.WithModel(model),
		plasmid.WithWorkingDir(workingDir),
		plasmid.WithSessionDir(sessionDir),
		plasmid.WithConfig(configPath),
	)
	if err != nil {
		t.Fatal(err)
	}
	return harness
}

type compactionHarnessModel struct {
	phase       string
	calls       int
	sawElision  bool
	sawFullRead bool
}

func (*compactionHarnessModel) Name() string { return "compaction-harness-test" }

func (m *compactionHarnessModel) GenerateContent(_ context.Context, request *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		response := &model.LLMResponse{UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 100}}
		if m.phase == "resumed" {
			m.sawElision = requestHasResponse(request, "grep-1", compaction.ElisionMarker)
			m.sawFullRead = requestHasFullRead(request, "read-1")
			response.Content = genai.NewContentFromText("third complete", genai.RoleModel)
			m.calls++
			yield(response, nil)
			return
		}
		switch m.calls {
		case 0:
			response.Content = &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
				ID: "grep-1", Name: "grep", Args: map[string]any{"pattern": "needle", "path": "large.txt", "max_results": 70},
			}}}}
		case 1:
			m.sawElision = requestHasResponse(request, "grep-1", compaction.ElisionMarker)
			response.Content = genai.NewContentFromText("first complete", genai.RoleModel)
		case 2:
			response.Content = &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
				ID: "read-1", Name: "read", Args: map[string]any{"path": "large.txt"},
			}}}}
		case 3:
			m.sawFullRead = requestHasFullRead(request, "read-1")
			response.Content = genai.NewContentFromText("second complete", genai.RoleModel)
		default:
			yield(nil, fmt.Errorf("unexpected model call %d", m.calls))
			return
		}
		m.calls++
		yield(response, nil)
	}
}

func requestHasResponse(request *model.LLMRequest, id, body string) bool {
	for _, content := range request.Contents {
		if content == nil {
			continue
		}
		for _, part := range content.Parts {
			if part == nil || part.FunctionResponse == nil || part.FunctionResponse.ID != id {
				continue
			}
			return part.FunctionResponse.Response["output"] == body
		}
	}
	return false
}

func requestHasFullRead(request *model.LLMRequest, id string) bool {
	for _, content := range request.Contents {
		if content == nil {
			continue
		}
		for _, part := range content.Parts {
			if part == nil || part.FunctionResponse == nil || part.FunctionResponse.ID != id {
				continue
			}
			response := part.FunctionResponse.Response
			content, _ := response["content"].(string)
			truncated, _ := response["truncated"].(bool)
			return !truncated && strings.Contains(content, "needle-069 payload")
		}
	}
	return false
}

var _ model.LLM = (*compactionHarnessModel)(nil)
