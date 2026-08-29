package openai_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/RandomCodeSpace/plasmid/internal/fixture"
	"github.com/RandomCodeSpace/plasmid/openai"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

const openAIFixtureRunner = "openai/wire"
const openAIChatFixtureRunner = "openai/chat-wire"

type wireFixtureInput struct {
	APIKey      string            `json:"api_key"`
	Environment map[string]string `json:"environment"`
	MaxRetries  int               `json:"max_retries"`
	Scenario    string            `json:"scenario"`
}

func init() {
	fixture.RegisterRunner("openai", openAIFixtureRunner, "wire")
	fixture.RegisterRunner("openai", openAIChatFixtureRunner, "chat-wire")
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
		var input wireFixtureInput
		testCase.Decode(t, "case.json", &specification)
		testCase.Decode(t, "input.json", &input)
		if specification.Area != "openai" || specification.ID != testCase.ID || specification.Kind != "wire" {
			t.Fatalf("invalid fixture identity: %#v", specification)
		}
		for name, value := range input.Environment {
			t.Setenv(name, value)
		}
		result := runWireFixture(t, input)
		testCase.CompareJSON(t, "expected.json", result, fixture.Paths{}, fixture.GoldenReadOnly)
	})
}

func runWireFixture(t *testing.T, input wireFixtureInput) any {
	t.Helper()
	switch input.Scenario {
	case "hostile-environment":
		return runHostileEnvironmentFixture(t, input)
	case "authorization":
		return runAuthorizationFixture(t, input)
	case "retries":
		return runRetryFixture(t, input)
	case "compressed-oversized-success":
		return runCompressedOverflowFixture(t, http.StatusOK, input.MaxRetries)
	case "compressed-oversized-error":
		return runCompressedOverflowFixture(t, http.StatusInternalServerError, input.MaxRetries)
	case "redirect":
		return runRedirectFixture(t)
	case "cancellation":
		return runCancellationFixture(t)
	case "endpoint-query-redaction":
		return runEndpointRedactionFixture(t)
	default:
		t.Fatalf("unknown OpenAI wire fixture scenario %q", input.Scenario)
		return nil
	}
}

func runHostileEnvironmentFixture(t *testing.T, input wireFixtureInput) any {
	t.Helper()
	type wireProjection struct {
		ForbiddenHeadersPresent []string            `json:"forbidden_headers_present"`
		Headers                 map[string][]string `json:"headers"`
		Method                  string              `json:"method"`
		Model                   string              `json:"model"`
		Path                    string              `json:"path"`
		RawQuery                string              `json:"raw_query"`
		Text                    string              `json:"text"`
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
			ForbiddenHeadersPresent: ambientHeadersPresent(request.Header),
			Headers:                 controlledHeaders(request.Header), Method: request.Method, Model: payload.Model,
			Path: request.URL.Path, RawQuery: request.URL.RawQuery,
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
	return projection
}

func controlledHeaders(headers http.Header) map[string][]string {
	result := make(map[string][]string)
	for _, name := range []string{"Accept", "Accept-Encoding", "Authorization", "Content-Type"} {
		result[name] = append([]string(nil), headers.Values(name)...)
	}
	return result
}

func ambientHeadersPresent(headers http.Header) []string {
	checks := []struct {
		name  string
		value string
	}{
		{name: "Accept", value: "text/plain"},
		{name: "Accept-Encoding", value: "br"},
		{name: "Authorization", value: "Bearer ambient-authorization"},
		{name: "Content-Type", value: "text/plain"},
		{name: "User-Agent", value: "ambient-agent"},
		{name: "X-Ambient"},
		{name: "OpenAI-Organization"},
		{name: "OpenAI-Project"},
	}
	var present []string
	for _, check := range checks {
		values := headers.Values(check.name)
		if check.value == "" && len(values) != 0 {
			present = append(present, check.name)
			continue
		}
		for _, value := range values {
			if value == check.value {
				present = append(present, check.name)
				break
			}
		}
	}
	if present == nil {
		return []string{}
	}
	return present
}

func runAuthorizationFixture(t *testing.T, input wireFixtureInput) any {
	t.Helper()
	authorization := make(chan []string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		values := append([]string(nil), request.Header.Values("Authorization")...)
		if values == nil {
			values = []string{}
		}
		authorization <- values
		writeResponse(writer, http.StatusOK, responseJSON("fixture-model", "authorized"))
	}))
	defer server.Close()
	llm := newModel(t, server.URL+"/v1", server.Client(), input.APIKey, 4096, 0)
	text, err := collectModel(llm, t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{"authorization": <-authorization, "text": text}
}

func runRetryFixture(t *testing.T, input wireFixtureInput) any {
	t.Helper()
	var attempts atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		attempts.Add(1)
		writer.Header().Set("Retry-After", "0")
		writeResponse(writer, http.StatusInternalServerError, `{"error":{"message":"retry"}}`)
	}))
	defer server.Close()
	llm := newModel(t, server.URL+"/v1", server.Client(), "", 4096, input.MaxRetries)
	_, err := collectModel(llm, t.Context())
	return map[string]any{"attempts": attempts.Load(), "error": fixtureErrorKind(err)}
}

func runCompressedOverflowFixture(t *testing.T, status, retries int) any {
	t.Helper()
	contents := []byte(responseJSON("fixture-model", "compressed response"))
	if status >= http.StatusBadRequest {
		contents = []byte(`{"error":{"message":"compressed-body-secret"}}`)
	}
	compressed := gzipBytes(t, contents)
	var attempts atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		attempts.Add(1)
		writer.Header().Set("Content-Encoding", "gzip")
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(status)
		_, _ = writer.Write(compressed)
	}))
	defer server.Close()
	llm := newModel(t, server.URL+"/v1", server.Client(), "", int64(len(contents)-1), retries)
	_, err := collectModel(llm, t.Context())
	return map[string]any{"attempts": attempts.Load(), "error": fixtureErrorKind(err)}
}

func runRedirectFixture(t *testing.T) any {
	t.Helper()
	paths := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		paths <- request.URL.Path
		if request.URL.Path != "/final" {
			http.Redirect(writer, request, "/final", http.StatusTemporaryRedirect)
			return
		}
		writeResponse(writer, http.StatusOK, responseJSON("fixture-model", "redirected"))
	}))
	defer server.Close()
	llm := newModel(t, server.URL+"/v1", server.Client(), "", 4096, 0)
	text, err := collectModel(llm, t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{"paths": []string{<-paths, <-paths}, "text": text}
}

func runCancellationFixture(t *testing.T) any {
	t.Helper()
	started := make(chan struct{})
	release := make(chan struct{})
	var attempts atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		attempts.Add(1)
		close(started)
		<-release
	}))
	defer server.Close()
	llm := newModel(t, server.URL+"/v1", server.Client(), "", 4096, 0)
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() {
		_, err := collectModel(llm, ctx)
		result <- err
	}()
	<-started
	cancel()
	err := <-result
	close(release)
	return map[string]any{"attempts": attempts.Load(), "error": fixtureErrorKind(err)}
}

func runEndpointRedactionFixture(t *testing.T) any {
	t.Helper()
	type endpoint struct {
		Path     string `json:"path"`
		RawQuery string `json:"raw_query"`
	}
	received := make(chan endpoint, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		received <- endpoint{Path: request.URL.Path, RawQuery: request.URL.RawQuery}
		writeResponse(writer, http.StatusUnauthorized, `{"error":{"message":"provider-body-secret"}}`)
	}))
	defer server.Close()
	llm := newModel(t, server.URL+"/v1?query-secret=value#fragment-secret", server.Client(), "api-key-secret", 4096, 0)
	_, err := collectModel(llm, t.Context())
	requestEndpoint := <-received
	redacted := true
	for _, secret := range []string{"query-secret", "fragment-secret", "provider-body-secret", "api-key-secret", server.URL} {
		redacted = redacted && !strings.Contains(err.Error(), secret)
	}
	return map[string]any{
		"error": err.Error(), "failure": fixtureErrorKind(err), "path": requestEndpoint.Path,
		"raw_query": requestEndpoint.RawQuery, "redacted": redacted,
	}
}

func fixtureErrorKind(err error) string {
	if errors.Is(err, context.Canceled) {
		return "context_canceled"
	}
	var oversized *openai.ResponseTooLargeError
	if errors.As(err, &oversized) {
		return "response_too_large"
	}
	var requestError *openai.RequestError
	if errors.As(err, &requestError) {
		return string(requestError.Failure)
	}
	if err == nil {
		return ""
	}
	return "unknown"
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
