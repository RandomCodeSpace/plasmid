package openai_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RandomCodeSpace/plasmid/openai"
	openaisdk "github.com/openai/openai-go/v3"
	"google.golang.org/adk/v2/model"
)

func TestExplicitAPIKeyControlsAuthorization(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "ambient-api-key")
	t.Setenv("OPENAI_ADMIN_KEY", "ambient-admin-key")
	t.Setenv("OPENAI_CUSTOM_HEADERS", "Authorization: Bearer ambient-custom-key")

	tests := []struct {
		name     string
		apiKey   string
		wantAuth string
	}{
		{name: "empty", apiKey: "", wantAuth: ""},
		{name: "nonempty", apiKey: "typed-key", wantAuth: "Bearer typed-key"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			authorization := make(chan []string, 1)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				authorization <- append([]string(nil), request.Header.Values("Authorization")...)
				writeResponse(writer, http.StatusOK, responseJSON("auth-model", "ok"))
			}))
			defer server.Close()

			llm := newModel(t, server.URL+"/v1", server.Client(), test.apiKey, 4096, 0)
			if _, err := collectModel(llm, t.Context()); err != nil {
				t.Fatal(err)
			}
			got := <-authorization
			if test.wantAuth == "" && len(got) != 0 {
				t.Fatalf("Authorization headers = %q, want none", got)
			}
			if test.wantAuth != "" && (len(got) != 1 || got[0] != test.wantAuth) {
				t.Fatalf("Authorization headers = %q, want [%q]", got, test.wantAuth)
			}
		})
	}
}

func TestTypedRetryCountIsHonoredExactly(t *testing.T) {
	for _, maximum := range []int{0, 2} {
		t.Run(strconv.Itoa(maximum), func(t *testing.T) {
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				calls.Add(1)
				writer.Header().Set("Retry-After", "0")
				writeResponse(writer, http.StatusInternalServerError, `{"error":{"message":"retry"}}`)
			}))
			defer server.Close()

			llm := newModel(t, server.URL+"/v1", server.Client(), "", 4096, maximum)
			_, err := collectModel(llm, t.Context())
			var requestError *openai.RequestError
			if !errors.As(err, &requestError) || requestError.Failure != openai.RequestFailureProvider {
				t.Fatalf("GenerateContent() error = %T %v, want provider RequestError", err, err)
			}
			if got, want := calls.Load(), int64(maximum+1); got != want {
				t.Fatalf("transport attempts = %d, want %d", got, want)
			}
		})
	}
}

func TestDecompressedResponseLimitAndBodyClose(t *testing.T) {
	success := []byte(responseJSON("limit-model", "bounded response"))
	errorBody := []byte(`{"error":{"message":"compressed-body-secret"}}`)
	tests := []struct {
		name       string
		status     int
		body       []byte
		limit      int64
		maxRetries int
		wantText   string
		wantLarge  bool
	}{
		{name: "exact success", status: http.StatusOK, body: success, limit: int64(len(success)), wantText: "bounded response"},
		{name: "oversized success", status: http.StatusOK, body: success, limit: int64(len(success) - 1), wantLarge: true},
		{name: "oversized error is not retried", status: http.StatusInternalServerError, body: errorBody, limit: int64(len(errorBody) - 1), maxRetries: 3, wantLarge: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			compressed := gzipBytes(t, test.body)
			var calls atomic.Int64
			var closed atomic.Bool
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				calls.Add(1)
				body := &trackedBody{ReadCloser: io.NopCloser(bytes.NewReader(compressed)), closed: &closed}
				return &http.Response{
					StatusCode: test.status, Header: http.Header{"Content-Encoding": {"gzip"}, "Content-Type": {"application/json"}},
					Body: body, Request: request,
				}, nil
			})}
			llm := newModel(t, "https://provider.test/v1", client, "", test.limit, test.maxRetries)
			text, err := collectModel(llm, t.Context())
			if test.wantLarge {
				var oversized *openai.ResponseTooLargeError
				if !errors.As(err, &oversized) || !errors.Is(err, &openai.ResponseTooLargeError{}) {
					t.Fatalf("GenerateContent() error = %T %v, want oversized response", err, err)
				}
				if oversized.Limit != test.limit {
					t.Errorf("ResponseTooLargeError.Limit = %d, want %d", oversized.Limit, test.limit)
				}
			} else if err != nil || text != test.wantText {
				t.Fatalf("GenerateContent() = %q, %v, want %q, nil", text, err, test.wantText)
			}
			if !closed.Load() {
				t.Fatal("response body was not closed")
			}
			if test.status >= http.StatusBadRequest && calls.Load() != 1 {
				t.Fatalf("oversized error attempts = %d, want 1", calls.Load())
			}
		})
	}
}

func TestCallerHTTPClientTimeoutRemainsAuthoritative(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		time.Sleep(100 * time.Millisecond)
		writeResponse(writer, http.StatusOK, responseJSON("timeout-model", "late"))
	}))
	defer server.Close()
	client := server.Client()
	client.Timeout = 20 * time.Millisecond

	llm := newModel(t, server.URL+"/v1", client, "", 4096, 0)
	_, err := collectModel(llm, t.Context())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("GenerateContent() error = %T %v, want deadline exceeded", err, err)
	}
}

func TestCallerRedirectPolicyRemainsAuthoritative(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/final" {
			writeResponse(writer, http.StatusOK, responseJSON("redirect-model", "redirected"))
			return
		}
		http.Redirect(writer, request, "/final", http.StatusTemporaryRedirect)
	}))
	defer server.Close()

	policyErr := errors.New("redirect-policy-secret")
	denyClientValue := *server.Client()
	denyClient := &denyClientValue
	denyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return policyErr }
	denyModel := newModel(t, server.URL+"/v1", denyClient, "", 4096, 0)
	_, err := collectModel(denyModel, t.Context())
	if !errors.Is(err, policyErr) {
		t.Fatalf("denied redirect error = %T %v, want caller sentinel match", err, err)
	}
	if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), server.URL) {
		t.Fatalf("denied redirect error leaked policy or URL: %q", err)
	}

	allowModel := newModel(t, server.URL+"/v1", server.Client(), "", 4096, 0)
	text, err := collectModel(allowModel, t.Context())
	if err != nil || text != "redirected" {
		var requestError *openai.RequestError
		if errors.As(err, &requestError) {
			t.Fatalf("allowed redirect = %q, failure %q, want redirected, nil", text, requestError.Failure)
		}
		t.Fatalf("allowed redirect = %q, %v, want redirected, nil", text, err)
	}
}

func TestCallerCookieJarRemainsAuthoritative(t *testing.T) {
	cookies := make(chan []*http.Cookie, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		cookies <- request.Cookies()
		writeResponse(writer, http.StatusOK, responseJSON("cookie-model", "cookie"))
	}))
	defer server.Close()
	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	jar.SetCookies(serverURL, []*http.Cookie{{Name: "session", Value: "caller-cookie"}})
	client := server.Client()
	client.Jar = jar

	llm := newModel(t, server.URL+"/v1", client, "", 4096, 0)
	if _, err := collectModel(llm, t.Context()); err != nil {
		t.Fatal(err)
	}
	got := <-cookies
	if len(got) != 1 || got[0].Name != "session" || got[0].Value != "caller-cookie" {
		t.Fatalf("received cookies = %#v", got)
	}
}

func TestCallerProxyAndTLSConfigurationRemainAuthoritative(t *testing.T) {
	t.Run("proxy", func(t *testing.T) {
		received := make(chan *url.URL, 1)
		proxy := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			copy := *request.URL
			received <- &copy
			writeResponse(writer, http.StatusOK, responseJSON("proxy-model", "proxied"))
		}))
		defer proxy.Close()
		proxyURL, err := url.Parse(proxy.URL)
		if err != nil {
			t.Fatal(err)
		}
		client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
		llm := newModel(t, "http://provider.invalid/v1", client, "", 4096, 0)
		text, err := collectModel(llm, t.Context())
		if err != nil || text != "proxied" {
			t.Fatalf("proxied GenerateContent() = %q, %v", text, err)
		}
		wireURL := <-received
		if wireURL.Host != "provider.invalid" || wireURL.Path != "/v1/responses" {
			t.Fatalf("proxy received URL = %q", wireURL)
		}
	})

	t.Run("TLS", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			writeResponse(writer, http.StatusOK, responseJSON("tls-model", "tls"))
		}))
		defer server.Close()
		llm := newModel(t, server.URL+"/v1", server.Client(), "", 4096, 0)
		text, err := collectModel(llm, t.Context())
		if err != nil || text != "tls" {
			t.Fatalf("TLS GenerateContent() = %q, %v", text, err)
		}
	})
}

func TestCallerCancellationRemainsAuthoritative(t *testing.T) {
	started := make(chan struct{})
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		close(started)
		<-request.Context().Done()
		return nil, request.Context().Err()
	})}
	llm := newModel(t, "https://provider.test/v1", client, "", 4096, 0)
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() {
		_, err := collectModel(llm, ctx)
		result <- err
	}()
	<-started
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("GenerateContent() error = %T %v, want context canceled", err, err)
	}
}

func TestRequestErrorsAreTypedAndRedacted(t *testing.T) {
	t.Run("transport and caller sentinel", func(t *testing.T) {
		policyErr := errors.New("caller-policy-secret")
		client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, policyErr
		})}
		llm := newModel(t, "https://provider.test/v1?query-secret=value#fragment-secret", client, "body-key-secret", 4096, 0)
		_, err := collectModel(llm, t.Context())
		var requestError *openai.RequestError
		if !errors.As(err, &requestError) || requestError.Failure != openai.RequestFailureTransport {
			t.Fatalf("GenerateContent() error = %T %v, want transport RequestError", err, err)
		}
		if !errors.Is(err, policyErr) {
			t.Fatal("RequestError does not preserve caller sentinel matching")
		}
		assertRedacted(t, err, "caller-policy-secret", "query-secret", "fragment-secret", "body-key-secret", "provider.test")
		var endpoint *url.Error
		if errors.As(err, &endpoint) {
			t.Fatal("RequestError unwraps to url.Error")
		}
	})

	t.Run("provider body", func(t *testing.T) {
		client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusUnauthorized, Header: http.Header{"Content-Type": {"application/json"}},
				Body: io.NopCloser(strings.NewReader(`{"error":{"message":"provider-body-secret","type":"auth"}}`)), Request: request,
			}, nil
		})}
		llm := newModel(t, "https://provider.test/v1?query-secret=value#fragment-secret", client, "api-key-secret", 4096, 0)
		_, err := collectModel(llm, t.Context())
		var requestError *openai.RequestError
		if !errors.As(err, &requestError) || requestError.Failure != openai.RequestFailureProvider || requestError.StatusCode != http.StatusUnauthorized {
			t.Fatalf("GenerateContent() error = %#v, want 401 provider RequestError", err)
		}
		assertRedacted(t, err, "provider-body-secret", "query-secret", "fragment-secret", "api-key-secret", "provider.test")
		var provider *openaisdk.Error
		if errors.As(err, &provider) {
			t.Fatal("RequestError unwraps to openai-go Error")
		}
	})

	t.Run("malformed success body", func(t *testing.T) {
		client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
				Body: io.NopCloser(strings.NewReader("malformed-response-secret")), Request: request,
			}, nil
		})}
		llm := newModel(t, "https://provider.test/v1", client, "", 4096, 0)
		_, err := collectModel(llm, t.Context())
		var requestError *openai.RequestError
		if !errors.As(err, &requestError) || requestError.Failure != openai.RequestFailureResponse {
			t.Fatalf("GenerateContent() error = %T %v, want response RequestError", err, err)
		}
		assertRedacted(t, err, "malformed-response-secret", "provider.test")
	})
}

func TestResponsesProtocolNeverFallsBack(t *testing.T) {
	paths := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		paths <- request.URL.Path
		writeResponse(writer, http.StatusNotFound, `{"error":{"message":"missing"}}`)
	}))
	defer server.Close()
	llm := newModel(t, server.URL+"/v1", server.Client(), "", 4096, 0)
	if _, err := collectModel(llm, t.Context()); err == nil {
		t.Fatal("GenerateContent() returned nil error")
	}
	close(paths)
	var got []string
	for path := range paths {
		got = append(got, path)
	}
	if len(got) != 1 || got[0] != "/v1/responses" {
		t.Fatalf("request paths = %q, want only Responses", got)
	}
}

func newModel(t *testing.T, baseURL string, client *http.Client, apiKey string, responseLimit int64, retries int) model.LLM {
	t.Helper()
	llm, err := openai.New(t.Context(), openai.Config{
		Protocol: openai.ProtocolResponses, Model: "test-model", BaseURL: baseURL, APIKey: apiKey,
		HTTPClient: client, MaxResponseBytes: responseLimit, MaxRetries: retries,
	})
	if err != nil {
		t.Fatal(err)
	}
	if llm.Name() != "test-model" {
		t.Fatalf("model Name() = %q, want test-model", llm.Name())
	}
	return llm
}

func writeResponse(writer http.ResponseWriter, status int, body string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_, _ = io.WriteString(writer, body)
}

func gzipBytes(t *testing.T, contents []byte) []byte {
	t.Helper()
	var result bytes.Buffer
	writer := gzip.NewWriter(&result)
	if _, err := writer.Write(contents); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return result.Bytes()
}

type trackedBody struct {
	io.ReadCloser
	closed *atomic.Bool
}

func (body *trackedBody) Close() error {
	body.closed.Store(true)
	return body.ReadCloser.Close()
}

func assertRedacted(t *testing.T, err error, forbidden ...string) {
	t.Helper()
	message := err.Error()
	for _, value := range forbidden {
		if strings.Contains(message, value) {
			t.Errorf("error %q contains forbidden value %q", message, value)
		}
	}
}

var _ func(context.Context, openai.Config) (model.LLM, error) = openai.New
