package compaction

import (
	"bytes"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

func TestEstimateRequestCountsCompleteNativeShape(t *testing.T) {
	request := &model.LLMRequest{
		Model: "fixture-model",
		Contents: []*genai.Content{{Role: genai.RoleUser, Parts: []*genai.Part{
			{Text: "<&>"},
			{FunctionCall: &genai.FunctionCall{ID: "call-1", Name: "read", Args: map[string]any{"z": 2, "a": 1}}},
			{InlineData: &genai.Blob{MIMEType: "application/octet-stream", Data: []byte{0, 1, 2}}},
		}}},
		Config: &genai.GenerateContentConfig{
			SystemInstruction: genai.NewContentFromText("system", genai.RoleUser),
			Tools:             []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{{Name: "read"}}}},
		},
	}
	estimate, err := EstimateRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if estimate.Contents != 2 || estimate.Parts != 4 || estimate.Functions != 1 || estimate.Binaries != 1 || estimate.ToolDeclarations != 1 {
		t.Fatalf("estimate counts = %#v", estimate)
	}
	wantOverhead := 2*ContentOverheadTokens + 4*PartOverheadTokens + FunctionOverheadTokens + BinaryOverheadTokens + ToolDeclarationOverheadTokens
	if estimate.OverheadTokens != wantOverhead || estimate.Tokens != estimate.RawTokens+wantOverhead {
		t.Fatalf("estimate = %#v, want overhead %d", estimate, wantOverhead)
	}
	canonical, err := canonicalJSON(requestWire{Model: request.Model, Contents: request.Contents, Config: request.Config})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(canonical, []byte(`\u003c`)) || bytes.Contains(canonical, []byte(`\u0026`)) {
		t.Fatalf("canonical JSON HTML-escaped text: %s", canonical)
	}
	if strings.Index(string(canonical), `"a":1`) > strings.Index(string(canonical), `"z":2`) {
		t.Fatalf("canonical JSON did not sort map keys: %s", canonical)
	}
	if strings.Index(string(canonical), `"config"`) > strings.Index(string(canonical), `"contents"`) ||
		strings.Index(string(canonical), `"args"`) > strings.Index(string(canonical), `"id"`) {
		t.Fatalf("canonical JSON did not sort struct keys: %s", canonical)
	}
}

func TestEstimateRequestRejectsNil(t *testing.T) {
	if _, err := EstimateRequest(nil); err == nil {
		t.Fatal("EstimateRequest(nil) succeeded")
	}
}
