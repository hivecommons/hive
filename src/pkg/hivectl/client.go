package hivectl

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// maxResponseBytes caps the buffered response body so a runaway endpoint cannot
// exhaust client memory. Exceeding it is reported as an error rather than
// silently truncated, which would corrupt JSON decoding.
const maxResponseBytes = 16 << 20

// maxRequestBytes mirrors the dashboard's decodeBody limit (pkg/dashboard/api.go
// maxDecodeBodyBytes). The server measures the JSON-encoded request body, not the
// raw file/stdin bytes, so JSON escaping (e.g. newlines, quotes) can push a small
// input past the limit. Checking the encoded size here turns that into a clear
// client-side error instead of an opaque server-side HTTP 400.
const maxRequestBytes = 1 << 20

// RequestTooLargeError is returned before sending when the JSON-encoded request
// body would exceed the Hive API request-size limit.
type RequestTooLargeError struct {
	Size  int
	Limit int
}

func (e *RequestTooLargeError) Error() string {
	return fmt.Sprintf("request body is %d bytes after JSON encoding, exceeding the Hive API %d MiB limit", e.Size, e.Limit>>20)
}

// APIError is returned when Hive responds with a non-2xx status.
type APIError struct {
	StatusCode int
	Message    string
	Body       []byte
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("Hive API returned HTTP %d: %s", e.StatusCode, e.Message)
	}
	return fmt.Sprintf("Hive API returned HTTP %d", e.StatusCode)
}

// ConnectionError distinguishes network failures from API failures.
type ConnectionError struct {
	Err error
}

func (e *ConnectionError) Error() string { return "unable to reach Hive: " + e.Err.Error() }
func (e *ConnectionError) Unwrap() error { return e.Err }

// Client is a small client for the Hive dashboard API.
type Client struct {
	baseURL *url.URL
	token   string
	http    *http.Client
}

func NewClient(server, token string, timeout time.Duration) (*Client, error) {
	if timeout <= 0 {
		return nil, fmt.Errorf("timeout must be greater than zero")
	}
	baseURL, err := url.Parse(strings.TrimSpace(server))
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" {
		return nil, fmt.Errorf("invalid Hive server URL %q", server)
	}
	if baseURL.Scheme != "http" && baseURL.Scheme != "https" {
		return nil, fmt.Errorf("hive server URL must use http or https")
	}
	baseURL.Path = strings.TrimRight(baseURL.Path, "/")
	// Give the client its OWN transport rather than sharing the process-wide
	// http.DefaultTransport (which a nil Transport silently uses).
	//
	// Sharing bit in CI: every parallel client test tears down its
	// httptest.Server, and Server.Close() calls CloseIdleConnections on
	// http.DefaultTransport — racing whichever OTHER parallel test has a
	// request in flight on a pooled connection. Under a loaded runner that
	// surfaced as a flaky "net/http: HTTP/1.x transport connection broken:
	// http: CloseIdleConnections called" from an unrelated test. A per-client
	// pool also means a hivectl embedder can never have its connections reaped
	// by stray CloseIdleConnections calls elsewhere in the process. Clone()
	// keeps all of DefaultTransport's dial/TLS/proxy defaults; the two-value
	// assertion falls back to the shared default only if DefaultTransport is
	// not a *http.Transport (it always is in practice).
	var transport http.RoundTripper
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = t.Clone()
	}
	return &Client{
		baseURL: baseURL,
		token:   strings.TrimSpace(token),
		http:    &http.Client{Timeout: timeout, Transport: transport},
	}, nil
}

func (c *Client) Do(ctx context.Context, method, apiPath string, query url.Values, body any) (any, error) {
	data, contentType, err := c.do(ctx, method, apiPath, query, body, nil)
	if err != nil {
		return nil, err
	}
	return decodeResponse(data, contentType)
}

func (c *Client) DoWithHeaders(ctx context.Context, method, apiPath string, query url.Values, body any, headers http.Header) (any, error) {
	data, contentType, err := c.do(ctx, method, apiPath, query, body, headers)
	if err != nil {
		return nil, err
	}
	return decodeResponse(data, contentType)
}

func decodeResponse(data []byte, contentType string) (any, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return map[string]any{}, nil
	}
	if strings.Contains(contentType, "application/json") || json.Valid(data) {
		var result any
		if err := json.Unmarshal(data, &result); err != nil {
			return nil, fmt.Errorf("decode Hive response: %w", err)
		}
		return result, nil
	}
	return string(data), nil
}

func (c *Client) Raw(ctx context.Context, method, apiPath string, query url.Values, body any) ([]byte, string, error) {
	return c.do(ctx, method, apiPath, query, body, nil)
}

func (c *Client) StreamSSE(ctx context.Context, apiPath string, query url.Values, output io.Writer) error {
	req, err := c.request(ctx, http.MethodGet, apiPath, query, nil, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return connectionError(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return apiError(resp.StatusCode, data)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 2<<20)
	eventName := "message"
	dataLines := []string{}
	flush := func() error {
		if len(dataLines) == 0 {
			eventName = "message"
			return nil
		}
		data := strings.Join(dataLines, "\n")
		event := map[string]any{"event": eventName}
		if json.Valid([]byte(data)) {
			event["data"] = json.RawMessage(data)
		} else {
			event["data"] = data
		}
		encoded, err := json.Marshal(event)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintln(output, string(encoded)); err != nil {
			return err
		}
		eventName = "message"
		dataLines = dataLines[:0]
		return nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, "event:") {
			if name := strings.TrimSpace(strings.TrimPrefix(line, "event:")); name != "" {
				eventName = name
			}
		} else if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := flush(); err != nil {
		return err
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, context.Canceled) {
		return connectionError(err)
	}
	return ctx.Err()
}

func (c *Client) do(ctx context.Context, method, apiPath string, query url.Values, body any, headers http.Header) ([]byte, string, error) {
	req, err := c.request(ctx, method, apiPath, query, body, headers)
	if err != nil {
		return nil, "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", connectionError(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, "", connectionError(err)
	}
	if int64(len(data)) > maxResponseBytes {
		return nil, "", fmt.Errorf("hive response exceeds the %d MiB client limit", maxResponseBytes>>20)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", apiError(resp.StatusCode, data)
	}
	return data, resp.Header.Get("Content-Type"), nil
}

func (c *Client) request(ctx context.Context, method, apiPath string, query url.Values, body any, headers http.Header) (*http.Request, error) {
	target := *c.baseURL
	target.Path = strings.TrimRight(c.baseURL.Path, "/") + "/" + strings.TrimLeft(apiPath, "/")
	target.RawQuery = query.Encode()

	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode request: %w", err)
		}
		if len(data) > maxRequestBytes {
			return nil, &RequestTooLargeError{Size: len(data), Limit: maxRequestBytes}
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, target.String(), reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("User-Agent", "hivectl/0.1")
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	return req, nil
}

func apiError(status int, body []byte) error {
	message := strings.TrimSpace(string(body))
	var payload struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &payload) == nil && payload.Error != "" {
		message = payload.Error
	}
	return &APIError{StatusCode: status, Message: message, Body: body}
}

func connectionError(err error) error {
	return &ConnectionError{Err: err}
}
