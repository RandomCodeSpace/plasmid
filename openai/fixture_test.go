package openai_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/RandomCodeSpace/plasmid/internal/fixture"
	"github.com/RandomCodeSpace/plasmid/openai"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

const openAIFixtureRunner = "openai/wire"

func init() {
	fixture.RegisterRunner("openai", openAIFixtureRunner, "wire")
}

func TestMain(m *testing.M) {
	os.Exit(fixture.Run(m))
}

func TestOpenAIFixtureCoverage(t *testing.T) {
	fixture.AssertCoverage(t, "openai")
}

func TestOpenAIWireFixtures(t *testing.T) {
	fixture.WalkKinds(t, "openai", openAIFixtureRunner, []string{"wire"}, func(t *testing.T, testCase fixture.Case) {
		var specification struct {
			Area string `json:"area"`
			ID   string `json:"id"`
			Kind string `json:"kind"`
		}
		var input struct {
			Environment map[string]string `json:"environment"`
			APIKey      string            `json:"api_key"`
		}
		testCase.Decode(t, "case.json", &specification)
		testCase.Decode(t, "input.json", &input)
		if specification.Area != "openai" || specification.ID != testCase.ID || specification.Kind != "wire" {
			t.Fatalf("invalid fixture identity: %#v", specification)
		}
		for name, value := range input.Environment {
			t.Setenv(name, value)
		}

		type wireProjection struct {
			Method   string              `json:"method"`
			Path     string              `json:"path"`
			RawQuery string              `json:"raw_query"`
			Headers  map[string][]string `json:"headers"`
			Model    string              `json:"model"`
			Text     string              `json:"text"`
		}
		received := make(chan wireProjection, 1)
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Errorf("read request body: %v", err)
			}
			var payload struct {
				Model string `json:"model"`
			}
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Errorf("decode request body: %v", err)
			}
			received <- wireProjection{
				Method: request.Method, Path: request.URL.Path, RawQuery: request.URL.RawQuery,
				Headers: request.Header.Clone(), Model: payload.Model,
			}
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, responseJSON("fixture-model", "fixture response"))
		}))
		defer server.Close()

		llm, err := openai.New(t.Context(), openai.Config{
			Protocol: openai.ProtocolResponses, Model: "fixture-model",
			BaseURL: server.URL + "/v1?typed_query=typed-secret#typed-fragment",
			APIKey:  input.APIKey, HTTPClient: server.Client(), MaxResponseBytes: 4096,
		})
		if err != nil {
			t.Fatal(err)
		}
		text, err := collectModel(llm, t.Context())
		if err != nil {
			t.Fatal(err)
		}
		projection := <-received
		projection.Text = text
		testCase.CompareJSON(t, "expected.json", projection, fixture.Paths{}, fixture.GoldenReadOnly)
	})
}

func collectModel(llm model.LLM, ctx context.Context) (string, error) {
	var text string
	for response, err := range llm.GenerateContent(ctx, testRequest(), false) {
		if err != nil {
			return "", err
		}
		if response == nil || response.Content == nil {
			continue
		}
		for _, part := range response.Content.Parts {
			text += part.Text
		}
	}
	return text, nil
}

func responseJSON(modelName, text string) string {
	payload := map[string]any{
		"id": "response-id", "model": modelName,
		"output": []any{map[string]any{
			"type": "message", "content": []any{map[string]any{"type": "output_text", "text": text}},
		}},
		"usage": map[string]any{
			"input_tokens": 1, "input_tokens_details": map[string]any{"cached_tokens": 0},
			"output_tokens": 1, "output_tokens_details": map[string]any{"reasoning_tokens": 0}, "total_tokens": 2,
		},
	}
	encoded, _ := json.Marshal(payload)
	return string(encoded)
}

func testRequest() *model.LLMRequest {
	return &model.LLMRequest{Contents: []*genai.Content{genai.NewContentFromText("hello", genai.RoleUser)}}
}
