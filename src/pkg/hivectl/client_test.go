package hivectl

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestClientDoSendsAuthQueryAndJSON(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/api/test" || r.URL.Query().Get("mode") != "safe" {
			t.Errorf("unexpected target %s", r.URL.String())
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("authorization = %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("content type = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true,"id":"abc"}`))
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "secret", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Do(context.Background(), http.MethodPost, "/api/test", url.Values{"mode": {"safe"}}, map[string]any{"name": "agent"})
	if err != nil {
		t.Fatal(err)
	}
	object := result.(map[string]any)
	if object["id"] != "abc" {
		t.Fatalf("result = %#v", result)
	}
}

func TestClientMapsAPIErrors(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"error":"agent already exists"}`))
	}))
	defer server.Close()

	client, _ := NewClient(server.URL, "", time.Second)
	_, err := client.Do(context.Background(), http.MethodGet, "/api/agents", nil, nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %T %v, want APIError", err, err)
	}
	if apiErr.StatusCode != http.StatusConflict || apiErr.Message != "agent already exists" {
		t.Fatalf("APIError = %#v", apiErr)
	}
}

func TestClientMapsConnectionErrors(t *testing.T) {
	t.Parallel()
	client, _ := NewClient("http://127.0.0.1:1", "", 50*time.Millisecond)
	_, err := client.Do(context.Background(), http.MethodGet, "/api/status", nil, nil)
	var connectionErr *ConnectionError
	if !errors.As(err, &connectionErr) {
		t.Fatalf("error = %T %v, want ConnectionError", err, err)
	}
}

func TestClientEnforcesTimeout(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(150 * time.Millisecond)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	client, _ := NewClient(server.URL, "", 25*time.Millisecond)
	_, err := client.Do(context.Background(), http.MethodGet, "/api/status", nil, nil)
	var connectionErr *ConnectionError
	if !errors.As(err, &connectionErr) {
		t.Fatalf("error = %T %v, want ConnectionError", err, err)
	}
}

func TestClientStreamsSSEAsJSONL(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("event: status\n"))
		w.Write([]byte("data: {\"state\":\"running\"}\n\n"))
		w.Write([]byte(": keepalive\n\n"))
		w.Write([]byte("data: {\"state\":\"idle\"}\n\n"))
	}))
	defer server.Close()

	client, _ := NewClient(server.URL, "", time.Second)
	var output bytes.Buffer
	if err := client.StreamSSE(context.Background(), "/api/events", nil, &output); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(output.String()); got != "{\"data\":{\"state\":\"running\"},\"event\":\"status\"}\n{\"data\":{\"state\":\"idle\"},\"event\":\"message\"}" {
		t.Fatalf("output = %q", got)
	}
}

func TestNewClientRejectsInvalidConfiguration(t *testing.T) {
	t.Parallel()
	if _, err := NewClient("localhost:3001", "", time.Second); err == nil {
		t.Fatal("expected invalid URL error")
	}
	if _, err := NewClient("ftp://example.com", "", time.Second); err == nil {
		t.Fatal("expected invalid scheme error")
	}
	if _, err := NewClient("http://example.com", "", 0); err == nil {
		t.Fatal("expected invalid timeout error")
	}
}
