package openai

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	openaioption "github.com/openai/openai-go/v3/option"
)

// ResponseTooLargeError reports a decompressed response that crossed its configured limit.
type ResponseTooLargeError struct {
	Limit int64
}

func (err *ResponseTooLargeError) Error() string {
	return "openai: response exceeds configured byte limit"
}

// Is allows errors.Is to classify all oversized OpenAI responses.
func (err *ResponseTooLargeError) Is(target error) bool {
	_, ok := target.(*ResponseTooLargeError)
	return ok
}

func explicitWireMiddleware(wireURL *url.URL, apiKey string, limit int64) openaioption.Middleware {
	return func(request *http.Request, next openaioption.MiddlewareNext) (*http.Response, error) {
		sanitized := request.Clone(request.Context())
		sanitized.URL = cloneURL(wireURL)
		sanitized.Host = ""
		sanitized.Header = make(http.Header)
		sanitized.Header.Set("Accept", "application/json")
		if sanitized.Body != nil {
			sanitized.Header.Set("Content-Type", "application/json")
		}
		if apiKey != "" {
			sanitized.Header.Set("Authorization", "Bearer "+apiKey)
		}

		response, err := next(sanitized)
		if err != nil || response == nil || response.Body == nil {
			return response, err
		}
		if err := decompressResponse(response); err != nil {
			return response, err
		}
		response.Body = newBoundedBody(response.Body, limit)
		if response.StatusCode >= http.StatusBadRequest {
			contents, readErr := io.ReadAll(response.Body)
			closeErr := response.Body.Close()
			if readErr != nil {
				var oversized *ResponseTooLargeError
				if errors.As(readErr, &oversized) {
					if response.Header == nil {
						response.Header = make(http.Header)
					}
					response.Header.Set("X-Should-Retry", "false")
				}
				return response, readErr
			}
			if closeErr != nil {
				return response, closeErr
			}
			response.Body = io.NopCloser(bytes.NewReader(contents))
		}
		return response, nil
	}
}

func cloneURL(value *url.URL) *url.URL {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func decompressResponse(response *http.Response) error {
	if !strings.EqualFold(strings.TrimSpace(response.Header.Get("Content-Encoding")), "gzip") {
		return nil
	}
	source := response.Body
	reader, err := gzip.NewReader(source)
	if err != nil {
		_ = source.Close()
		return err
	}
	response.Body = &combinedReadCloser{Reader: reader, closers: []io.Closer{reader, source}}
	response.Header.Del("Content-Encoding")
	response.Header.Del("Content-Length")
	response.ContentLength = -1
	response.Uncompressed = true
	return nil
}

type combinedReadCloser struct {
	io.Reader
	closers []io.Closer
	once    sync.Once
	err     error
}

func (body *combinedReadCloser) Close() error {
	body.once.Do(func() {
		for _, closer := range body.closers {
			body.err = errors.Join(body.err, closer.Close())
		}
	})
	return body.err
}

type boundedBody struct {
	source    io.ReadCloser
	limit     int64
	remaining int64
	once      sync.Once
	closeErr  error
}

func newBoundedBody(source io.ReadCloser, limit int64) *boundedBody {
	return &boundedBody{source: source, limit: limit, remaining: limit}
}

func (body *boundedBody) Read(buffer []byte) (int, error) {
	if len(buffer) == 0 {
		return 0, nil
	}
	maximum := len(buffer)
	if body.remaining < int64(maximum) {
		maximum = int(body.remaining) + 1
	}
	count, err := body.source.Read(buffer[:maximum])
	if int64(count) <= body.remaining {
		body.remaining -= int64(count)
		return count, err
	}
	allowed := int(body.remaining)
	body.remaining = 0
	_ = body.Close()
	return allowed, &ResponseTooLargeError{Limit: body.limit}
}

func (body *boundedBody) Close() error {
	body.once.Do(func() { body.closeErr = body.source.Close() })
	return body.closeErr
}
