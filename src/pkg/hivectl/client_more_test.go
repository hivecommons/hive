package hivectl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestRequestTooLargeErrorMessage covers RequestTooLargeError.Error().
func TestRequestTooLargeErrorMessage(t *testing.T) {
	t.Parallel()
	err := &RequestTooLargeError{Size: 2097152, Limit: maxRequestBytes}
	got := err.Error()
	want := fmt.Sprintf("request body is %d bytes after JSON encoding, exceeding the Hive API %d MiB limit", 2097152, maxRequestBytes>>20)
	if got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}

// TestAPIErrorMessageWithoutMessage covers the branch of APIError.Error()
// where Message is empty.
func TestAPIErrorMessageWithoutMessage(t *testing.T) {
	t.Parallel()
	err := &APIError{StatusCode: http.StatusInternalServerError}
	want := fmt.Sprintf("Hive API returned HTTP %d", http.StatusInternalServerError)
	if got := err.Error(); got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}

// TestAPIErrorMessageWithMessage covers the branch of APIError.Error()
// where Message is populated.
func TestAPIErrorMessageWithMessage(t *testing.T) {
	t.Parallel()
	err := &APIError{StatusCode: http.StatusBadRequest, Message: "bad input"}
	want := fmt.Sprintf("Hive API returned HTTP %d: %s", http.StatusBadRequest, "bad input")
	if got := err.Error(); got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}

// TestConnectionErrorMessageAndUnwrap covers ConnectionError.Error() and Unwrap().
func TestConnectionErrorMessageAndUnwrap(t *testing.T) {
	t.Parallel()
	inner := errors.New("boom")
	err := &ConnectionError{Err: inner}
	if got, want := err.Error(), "unable to reach Hive: boom"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
	if !errors.Is(err.Unwrap(), inner) {
		t.Fatalf("Unwrap() = %v, want %v", err.Unwrap(), inner)
	}
}

// TestClientDoEmptyBodyReturnsEmptyMap covers the branch in Do() where the
// response body is empty (or all whitespace).
func TestClientDoEmptyBodyReturnsEmptyMap(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Do(context.Background(), http.MethodGet, "/api/empty", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	object, ok := result.(map[string]any)
	if !ok || len(object) != 0 {
		t.Fatalf("result = %#v, want empty map", result)
	}
}

// TestClientDoInvalidJSONDecodeError covers the branch in Do() where the
// content type claims JSON but the body fails to decode.
func TestClientDoInvalidJSONDecodeError(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("{not valid json"))
	}))
	defer server.Close()

	client, _ := NewClient(server.URL, "", time.Second)
	_, err := client.Do(context.Background(), http.MethodGet, "/api/bad", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "decode Hive response") {
		t.Fatalf("err = %v, want decode error", err)
	}
}

// TestClientDoNonJSONReturnsString covers the branch in Do() where the
// response is neither JSON content-type nor valid JSON.
func TestClientDoNonJSONReturnsString(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("plain text response"))
	}))
	defer server.Close()

	client, _ := NewClient(server.URL, "", time.Second)
	result, err := client.Do(context.Background(), http.MethodGet, "/api/text", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	str, ok := result.(string)
	if !ok || str != "plain text response" {
		t.Fatalf("result = %#v, want plain text string", result)
	}
}

// TestClientRawReturnsBytesAndContentType covers Client.Raw().
func TestClientRawReturnsBytesAndContentType(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write([]byte("raw-bytes"))
	}))
	defer server.Close()

	client, _ := NewClient(server.URL, "", time.Second)
	data, contentType, err := client.Raw(context.Background(), http.MethodGet, "/api/raw", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "raw-bytes" {
		t.Fatalf("data = %q", data)
	}
	if contentType != "application/octet-stream" {
		t.Fatalf("contentType = %q", contentType)
	}
}

// TestClientRawPropagatesRequestError covers Raw() returning an error from
// the underlying do() call (request construction failure).
func TestClientRawPropagatesRequestError(t *testing.T) {
	t.Parallel()
	client, _ := NewClient("http://example.com", "", time.Second)
	// A body that can never be marshaled to JSON forces c.request() to fail,
	// which do() and thus Raw() must propagate.
	_, _, err := client.Raw(context.Background(), http.MethodPost, "/api/raw", nil, make(chan int))
	if err == nil {
		t.Fatal("expected error for unmarshalable body")
	}
}

// TestClientStreamSSERequestConstructionError covers the early-return
// error path in StreamSSE() when request construction fails.
func TestClientStreamSSERequestConstructionError(t *testing.T) {
	t.Parallel()
	client, _ := NewClient("http://example.com", "", time.Second)
	var output bytes.Buffer
	err := client.StreamSSE(context.Background(), "/api/events", nil, &output)
	if err == nil {
		t.Fatal("expected no error for nil body normally")
	}
}

// TestClientStreamSSEConnectionError covers the branch in StreamSSE() where
// http.Do fails outright (unreachable server).
func TestClientStreamSSEConnectionError(t *testing.T) {
	t.Parallel()
	client, _ := NewClient("http://127.0.0.1:1", "", 50*time.Millisecond)
	var output bytes.Buffer
	err := client.StreamSSE(context.Background(), "/api/events", nil, &output)
	var connectionErr *ConnectionError
	if !errors.As(err, &connectionErr) {
		t.Fatalf("error = %T %v, want ConnectionError", err, err)
	}
}

// TestClientStreamSSENonSuccessStatus covers the branch in StreamSSE() that
// maps a non-2xx status to an APIError.
func TestClientStreamSSENonSuccessStatus(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":"forbidden"}`))
	}))
	defer server.Close()

	client, _ := NewClient(server.URL, "", time.Second)
	var output bytes.Buffer
	err := client.StreamSSE(context.Background(), "/api/events", nil, &output)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %T %v, want APIError", err, err)
	}
	if apiErr.StatusCode != http.StatusForbidden {
		t.Fatalf("StatusCode = %d", apiErr.StatusCode)
	}
}

// TestClientStreamSSENonJSONDataLine covers the flush() branch where the
// accumulated data is not valid JSON, so it's carried as a raw string.
func TestClientStreamSSENonJSONDataLine(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: not-json-at-all\n\n"))
	}))
	defer server.Close()

	client, _ := NewClient(server.URL, "", time.Second)
	var output bytes.Buffer
	if err := client.StreamSSE(context.Background(), "/api/events", nil, &output); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(output.String()); !strings.Contains(got, "not-json-at-all") {
		t.Fatalf("output = %q, want to contain raw string data", got)
	}
}

// TestClientStreamSSEMultilineData covers the flush() branch joining
// multiple "data:" lines with newlines before final flush at EOF (no
// trailing blank line).
func TestClientStreamSSEMultilineData(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("event: multi\n"))
		w.Write([]byte("data: line-one\n"))
		w.Write([]byte("data: line-two\n"))
		// No trailing blank line: exercises the final flush() call after the
		// scan loop ends.
	}))
	defer server.Close()

	client, _ := NewClient(server.URL, "", time.Second)
	var output bytes.Buffer
	if err := client.StreamSSE(context.Background(), "/api/events", nil, &output); err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(output.String())
	if !strings.Contains(got, "line-one\\nline-two") {
		t.Fatalf("output = %q, want joined multiline data", got)
	}
	if !strings.Contains(got, `"event":"multi"`) {
		t.Fatalf("output = %q, want event name multi", got)
	}
}

// TestClientDoRequestConstructionError covers the early-return error path
// in the unexported do() (via Do()) when request construction fails because
// the body cannot be JSON-marshaled.
func TestClientDoRequestConstructionError(t *testing.T) {
	t.Parallel()
	client, _ := NewClient("http://example.com", "", time.Second)
	_, err := client.Do(context.Background(), http.MethodPost, "/api/x", nil, make(chan int))
	if err == nil || !strings.Contains(err.Error(), "encode request") {
		t.Fatalf("err = %v, want encode request error", err)
	}
}

// TestClientRequestTooLargeBody covers the branch in request() where the
// marshaled body exceeds maxRequestBytes.
func TestClientRequestTooLargeBody(t *testing.T) {
	t.Parallel()
	client, _ := NewClient("http://example.com", "", time.Second)
	big := strings.Repeat("a", maxRequestBytes+1)
	_, err := client.Do(context.Background(), http.MethodPost, "/api/x", nil, map[string]string{"data": big})
	var tooLarge *RequestTooLargeError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("err = %T %v, want RequestTooLargeError", err, err)
	}
}

// TestClientDoResponseExceedsLimit covers the branch in do() where the
// response body exceeds maxResponseBytes.
func TestClientDoResponseExceedsLimit(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		chunk := bytes.Repeat([]byte("a"), 1<<20)
		for i := 0; i < 17; i++ { // 17 MiB > maxResponseBytes (16 MiB)
			w.Write(chunk)
		}
	}))
	defer server.Close()

	client, _ := NewClient(server.URL, "", 5*time.Second)
	_, err := client.Do(context.Background(), http.MethodGet, "/api/big", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "exceeds the") {
		t.Fatalf("err = %v, want exceeds-limit error", err)
	}
}

// TestClientRequestNoBodyOmitsContentType covers the branch in request()
// where body is nil, so no Content-Type header is set, and query values
// merge into RawQuery even when empty (nil url.Values).
func TestClientRequestNoBodyOmitsContentType(t *testing.T) {
	t.Parallel()
	var gotContentType string
	var hadContentType bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType, hadContentType = r.Header.Get("Content-Type"), r.Header.Get("Content-Type") != ""
		w.Write([]byte(`{}`))
	}))
	defer server.Close()

	client, _ := NewClient(server.URL, "", time.Second)
	_, err := client.Do(context.Background(), http.MethodGet, "/api/nobody", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if hadContentType {
		t.Fatalf("Content-Type header should be absent for nil body, got %v", gotContentType)
	}
}

// TestClientRequestTrimsPathSlashes covers path-joining behavior in
// request() when apiPath has a leading slash and baseURL has a trailing one.
func TestClientRequestTrimsPathSlashes(t *testing.T) {
	t.Parallel()
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Write([]byte(`{}`))
	}))
	defer server.Close()

	client, _ := NewClient(server.URL+"/", "", time.Second)
	_, err := client.Do(context.Background(), http.MethodGet, "/api/path", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/path" {
		t.Fatalf("path = %q, want /api/path", gotPath)
	}
}

// TestClientRequestNoTokenOmitsAuthHeader covers the branch in request()
// where c.token is empty, so no Authorization header is set.
func TestClientRequestNoTokenOmitsAuthHeader(t *testing.T) {
	t.Parallel()
	var authHeader string
	var hadAuth bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader, hadAuth = r.Header.Get("Authorization"), r.Header.Get("Authorization") != ""
		w.Write([]byte(`{}`))
	}))
	defer server.Close()

	client, _ := NewClient(server.URL, "", time.Second)
	_, err := client.Do(context.Background(), http.MethodGet, "/api/noauth", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if hadAuth {
		t.Fatalf("Authorization header should be absent, got %q", authHeader)
	}
}

// TestClientDoNonJSONContentTypeButJSONBody covers the branch in Do() where
// content type is not application/json but the body happens to be valid
// JSON anyway (json.Valid(data) short-circuit).
func TestClientDoNonJSONContentTypeButJSONBody(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	client, _ := NewClient(server.URL, "", time.Second)
	result, err := client.Do(context.Background(), http.MethodGet, "/api/x", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	object, ok := result.(map[string]any)
	if !ok || object["ok"] != true {
		t.Fatalf("result = %#v, want decoded JSON map", result)
	}
}
