// Package openai constructs native ADK models for OpenAI-compatible endpoints.
package openai

import (
	"context"
	"errors"
	"iter"
	"net/http"
	"net/url"
	"strings"

	openaisdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/model/openaimodel"
)

// Protocol selects the fixed OpenAI wire protocol used by a model.
type Protocol string

const (
	// ProtocolResponses selects the OpenAI Responses API.
	ProtocolResponses Protocol = "responses"
	// ProtocolChatCompletions selects the OpenAI Chat Completions API.
	ProtocolChatCompletions Protocol = "chat_completions"
)

// ChatTokenLimitDialect selects the Chat Completions output-token field.
type ChatTokenLimitDialect string

const (
	// ChatTokenLimitMaxTokens sends ADK MaxOutputTokens as max_tokens.
	ChatTokenLimitMaxTokens ChatTokenLimitDialect = "max_tokens"
	// ChatTokenLimitMaxCompletionTokens sends ADK MaxOutputTokens as max_completion_tokens.
	ChatTokenLimitMaxCompletionTokens ChatTokenLimitDialect = "max_completion_tokens"
)

// Config contains the complete OpenAI model construction contract.
type Config struct {
	Protocol         Protocol
	Model            string
	BaseURL          string
	APIKey           string
	HTTPClient       *http.Client
	MaxResponseBytes int64
	MaxRetries       int
	ChatTokenLimit   ChatTokenLimitDialect
}

// ValidationError reports one invalid construction field without including its value.
type ValidationError struct {
	Field string
}

func (err *ValidationError) Error() string {
	return "openai: invalid " + err.Field
}

// ProtocolUnavailableError reports a recognized protocol whose implementation is not present.
type ProtocolUnavailableError struct {
	Protocol Protocol
}

func (err *ProtocolUnavailableError) Error() string {
	return "openai: protocol is unavailable"
}

// RequestFailure classifies a redacted model request error.
type RequestFailure string

const (
	// RequestFailureTransport identifies a caller HTTP policy or transport failure.
	RequestFailureTransport RequestFailure = "transport"
	// RequestFailureProvider identifies an HTTP error response from the provider.
	RequestFailureProvider RequestFailure = "provider"
	// RequestFailureResponse identifies an invalid or unreadable provider response.
	RequestFailureResponse RequestFailure = "response"
)

// RequestError reports a request failure without exposing provider, endpoint, or body text.
type RequestError struct {
	Failure    RequestFailure
	StatusCode int
	cause      error
}

func (err *RequestError) Error() string {
	return "openai: request failed"
}

// Is preserves matching for caller-owned transport and redirect-policy sentinels.
func (err *RequestError) Is(target error) bool {
	return err != nil && err.cause != nil && errors.Is(err.cause, target)
}

// New constructs a native ADK model from explicit OpenAI configuration.
func New(ctx context.Context, cfg Config) (model.LLM, error) {
	baseURL, err := validate(ctx, cfg)
	if err != nil {
		return nil, err
	}
	wirePath := "responses"
	if cfg.Protocol == ProtocolChatCompletions {
		wirePath = "chat/completions"
	}
	wireURL := baseURL.ResolveReference(&url.URL{Path: wirePath})
	options := explicitOpenAIOptions(cfg, wireURL)
	if cfg.Protocol == ProtocolChatCompletions {
		client := openaisdk.NewClient(options...)
		return &redactedModel{inner: &chatModel{
			client: &client, name: cfg.Model, tokenLimit: cfg.ChatTokenLimit,
		}}, nil
	}

	inner, err := openaimodel.NewModel(ctx, cfg.Model, &openaimodel.ClientConfig{
		APIKey: cfg.APIKey, BaseURL: cfg.BaseURL, HTTPClient: cfg.HTTPClient, Options: options,
	})
	if err != nil {
		return nil, redactRequestError(err)
	}
	return &redactedModel{inner: inner}, nil
}

func explicitOpenAIOptions(cfg Config, wireURL *url.URL) []option.RequestOption {
	return []option.RequestOption{
		openAIEnvironmentDefaultsDisabled(),
		option.WithBaseURL(cfg.BaseURL),
		option.WithHTTPClient(cfg.HTTPClient),
		option.WithMaxRetries(cfg.MaxRetries),
		option.WithAdminAPIKey(""),
		option.WithOrganization(""),
		option.WithProject(""),
		option.WithWebhookSecret(""),
		option.WithAPIKey(cfg.APIKey),
		option.WithMiddleware(explicitWireMiddleware(wireURL, cfg.APIKey, cfg.MaxResponseBytes)),
	}
}

func validate(ctx context.Context, cfg Config) (*url.URL, error) {
	if ctx == nil {
		return nil, &ValidationError{Field: "context"}
	}
	if cfg.Protocol != ProtocolResponses && cfg.Protocol != ProtocolChatCompletions {
		return nil, &ValidationError{Field: "protocol"}
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, &ValidationError{Field: "model"}
	}
	baseURL, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return nil, &ValidationError{Field: "base_url"}
	}
	scheme := strings.ToLower(baseURL.Scheme)
	if (scheme != "http" && scheme != "https") || baseURL.Hostname() == "" || baseURL.User != nil {
		return nil, &ValidationError{Field: "base_url"}
	}
	baseURL.Scheme = scheme
	if cfg.HTTPClient == nil {
		return nil, &ValidationError{Field: "http_client"}
	}
	if cfg.MaxResponseBytes <= 0 {
		return nil, &ValidationError{Field: "max_response_bytes"}
	}
	if cfg.MaxRetries < 0 {
		return nil, &ValidationError{Field: "max_retries"}
	}
	if cfg.Protocol == ProtocolChatCompletions {
		if cfg.ChatTokenLimit != ChatTokenLimitMaxTokens && cfg.ChatTokenLimit != ChatTokenLimitMaxCompletionTokens {
			return nil, &ValidationError{Field: "chat_token_limit"}
		}
	} else if cfg.ChatTokenLimit != "" {
		return nil, &ValidationError{Field: "chat_token_limit"}
	}
	if baseURL.Path != "" && !strings.HasSuffix(baseURL.Path, "/") {
		baseURL.Path += "/"
	}
	return baseURL, nil
}

type redactedModel struct {
	inner model.LLM
}

func (modelValue *redactedModel) Name() string {
	return modelValue.inner.Name()
}

func (modelValue *redactedModel) GenerateContent(ctx context.Context, request *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		for response, err := range modelValue.inner.GenerateContent(ctx, request, stream) {
			if err != nil {
				yield(nil, redactRequestError(err))
				return
			}
			if !yield(response, nil) {
				return
			}
		}
	}
}

func redactRequestError(err error) error {
	if err == nil {
		return nil
	}
	var oversized *ResponseTooLargeError
	if errors.As(err, &oversized) {
		return oversized
	}
	var compatibility *ChatError
	if errors.As(err, &compatibility) {
		return compatibility
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	var provider *openaisdk.Error
	if errors.As(err, &provider) {
		return &RequestError{Failure: RequestFailureProvider, StatusCode: provider.StatusCode}
	}
	var endpoint *url.Error
	if errors.As(err, &endpoint) {
		return &RequestError{Failure: RequestFailureTransport, cause: err}
	}
	return &RequestError{Failure: RequestFailureResponse, cause: err}
}

var _ model.LLM = (*redactedModel)(nil)
