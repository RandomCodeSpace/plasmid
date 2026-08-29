package openai_test

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/RandomCodeSpace/plasmid/openai"
)

func TestConfigPublicFieldInventory(t *testing.T) {
	type field struct {
		name     string
		typeName string
	}
	want := []field{
		{name: "Protocol", typeName: "openai.Protocol"},
		{name: "Model", typeName: "string"},
		{name: "BaseURL", typeName: "string"},
		{name: "APIKey", typeName: "string"},
		{name: "HTTPClient", typeName: "*http.Client"},
		{name: "MaxResponseBytes", typeName: "int64"},
		{name: "MaxRetries", typeName: "int"},
		{name: "ChatTokenLimit", typeName: "openai.ChatTokenLimitDialect"},
	}

	typ := reflect.TypeFor[openai.Config]()
	if typ.NumField() != len(want) {
		t.Fatalf("Config field count = %d, want %d", typ.NumField(), len(want))
	}
	for index, expected := range want {
		actual := typ.Field(index)
		if actual.Name != expected.name || actual.Type.String() != expected.typeName {
			t.Errorf("Config field %d = %s %s, want %s %s", index, actual.Name, actual.Type, expected.name, expected.typeName)
		}
	}
}

func TestNewRejectsInvalidConfigurationBeforeNetworkWork(t *testing.T) {
	var calls atomic.Int64
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("unexpected network call")
	})}
	valid := openai.Config{
		Protocol:         openai.ProtocolResponses,
		Model:            "test-model",
		BaseURL:          "https://example.test/v1",
		APIKey:           "",
		HTTPClient:       client,
		MaxResponseBytes: 1024,
		MaxRetries:       0,
	}

	tests := []struct {
		name  string
		ctx   context.Context
		alter func(*openai.Config)
		field string
	}{
		{name: "nil context", ctx: nil, field: "context"},
		{name: "empty protocol", ctx: t.Context(), alter: func(cfg *openai.Config) { cfg.Protocol = "" }, field: "protocol"},
		{name: "unknown protocol", ctx: t.Context(), alter: func(cfg *openai.Config) { cfg.Protocol = "other" }, field: "protocol"},
		{name: "empty model", ctx: t.Context(), alter: func(cfg *openai.Config) { cfg.Model = "" }, field: "model"},
		{name: "blank model", ctx: t.Context(), alter: func(cfg *openai.Config) { cfg.Model = " \t" }, field: "model"},
		{name: "relative base URL", ctx: t.Context(), alter: func(cfg *openai.Config) { cfg.BaseURL = "/v1" }, field: "base_url"},
		{name: "missing base URL scheme", ctx: t.Context(), alter: func(cfg *openai.Config) { cfg.BaseURL = "example.test/v1" }, field: "base_url"},
		{name: "unsupported base URL scheme", ctx: t.Context(), alter: func(cfg *openai.Config) { cfg.BaseURL = "ftp://example.test/v1" }, field: "base_url"},
		{name: "malformed base URL", ctx: t.Context(), alter: func(cfg *openai.Config) { cfg.BaseURL = "https://example.test/%zz" }, field: "base_url"},
		{name: "missing base URL host", ctx: t.Context(), alter: func(cfg *openai.Config) { cfg.BaseURL = "https:///v1" }, field: "base_url"},
		{name: "empty base URL hostname", ctx: t.Context(), alter: func(cfg *openai.Config) { cfg.BaseURL = "https://:443/v1" }, field: "base_url"},
		{name: "base URL userinfo", ctx: t.Context(), alter: func(cfg *openai.Config) { cfg.BaseURL = "https://user:secret@example.test/v1" }, field: "base_url"},
		{name: "nil HTTP client", ctx: t.Context(), alter: func(cfg *openai.Config) { cfg.HTTPClient = nil }, field: "http_client"},
		{name: "zero response cap", ctx: t.Context(), alter: func(cfg *openai.Config) { cfg.MaxResponseBytes = 0 }, field: "max_response_bytes"},
		{name: "negative response cap", ctx: t.Context(), alter: func(cfg *openai.Config) { cfg.MaxResponseBytes = -1 }, field: "max_response_bytes"},
		{name: "negative retries", ctx: t.Context(), alter: func(cfg *openai.Config) { cfg.MaxRetries = -1 }, field: "max_retries"},
		{name: "Chat token limit on Responses", ctx: t.Context(), alter: func(cfg *openai.Config) { cfg.ChatTokenLimit = openai.ChatTokenLimitMaxTokens }, field: "chat_token_limit"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := valid
			if test.alter != nil {
				test.alter(&cfg)
			}
			_, err := openai.New(test.ctx, cfg)
			var validation *openai.ValidationError
			if !errors.As(err, &validation) {
				t.Fatalf("New() error = %T %v, want *ValidationError", err, err)
			}
			if validation.Field != test.field {
				t.Errorf("ValidationError.Field = %q, want %q", validation.Field, test.field)
			}
		})
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("network calls = %d, want 0", got)
	}
}

func TestNewChatCompletionsRunsCommonValidationBeforeConstruction(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("Chat construction performed network work")
		return nil, nil
	})}
	cfg := openai.Config{
		Protocol: openai.ProtocolChatCompletions, Model: "test-model",
		BaseURL: "https://example.test/v1", HTTPClient: client, MaxResponseBytes: 1024,
		ChatTokenLimit: openai.ChatTokenLimitMaxTokens,
	}

	invalid := cfg
	invalid.Model = ""
	if _, err := openai.New(t.Context(), invalid); err == nil {
		t.Fatal("New() with invalid Chat configuration returned nil error")
	} else {
		var validation *openai.ValidationError
		if !errors.As(err, &validation) {
			t.Fatalf("New() invalid Chat error = %T, want *ValidationError", err)
		}
	}

	llm, err := openai.New(t.Context(), cfg)
	if err != nil {
		t.Fatalf("New() valid Chat error = %v", err)
	}
	if llm.Name() != "test-model" {
		t.Errorf("Name() = %q, want test-model", llm.Name())
	}
}

func TestNewChatCompletionsRequiresExplicitTokenLimitDialect(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("Chat construction performed network work")
		return nil, nil
	})}
	valid := openai.Config{
		Protocol: openai.ProtocolChatCompletions, Model: "test-model",
		BaseURL: "https://example.test/v1", HTTPClient: client, MaxResponseBytes: 1024,
		ChatTokenLimit: openai.ChatTokenLimitMaxTokens,
	}
	for _, dialect := range []openai.ChatTokenLimitDialect{"", "inferred"} {
		cfg := valid
		cfg.ChatTokenLimit = dialect
		_, err := openai.New(t.Context(), cfg)
		var validation *openai.ValidationError
		if !errors.As(err, &validation) || validation.Field != "chat_token_limit" {
			t.Fatalf("dialect %q error = %T %v", dialect, err, err)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}
