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

// Callers escape each path segment with url.PathEscape. The client then
// escaped the whole path again, so a name with a "/" in it reached the hive as
// "team%2Fscanner" (a literal percent sign) instead of "team/scanner".
func TestClientSendsEscapedPathSegmentsOnce(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"team/scanner", "kiro-api-key/claude-opus-5:high", "owner/repo#12", "with space", "100%", "naïve", "plain"} {
		var gotName, gotEscaped string
		mux := http.NewServeMux()
		mux.HandleFunc("GET /api/agents/{name}/log", func(w http.ResponseWriter, r *http.Request) {
			gotName, gotEscaped = r.PathValue("name"), r.URL.EscapedPath()
			w.Write([]byte(`{}`))
		})
		server := httptest.NewServer(mux)
		client, err := NewClient(server.URL, "secret", time.Second)
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.Do(context.Background(), http.MethodGet, "/api/agents/"+url.PathEscape(name)+"/log", nil, nil)
		server.Close()
		if err != nil {
			t.Errorf("%q: %v", name, err)
			continue
		}
		if gotName != name {
			t.Errorf("hive read name %q, want %q (request path %s)", gotName, name, gotEscaped)
		}
		if want := "/api/agents/" + url.PathEscape(name) + "/log"; gotEscaped != want {
			t.Errorf("request path = %s, want %s", gotEscaped, want)
		}
	}
}

// A server URL with a path prefix keeps it, escaped or not.
func TestClientKeepsServerPathPrefixWithEscapedSegments(t *testing.T) {
	t.Parallel()
	var gotEscaped string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEscaped = r.URL.EscapedPath()
		w.Write([]byte(`{}`))
	}))
	defer server.Close()
	client, err := NewClient(server.URL+"/hives/my%20hive/", "secret", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Do(context.Background(), http.MethodGet, "/api/runs/"+url.PathEscape("org/repo#7")+"/detail", nil, nil); err != nil {
		t.Fatal(err)
	}
	if want := "/hives/my%20hive/api/runs/org%2Frepo%237/detail"; gotEscaped != want {
		t.Fatalf("request path = %s, want %s", gotEscaped, want)
	}
}

// A path that is not percent-encoded is still sent escaped, as before: an
// unescaped space, a "?" left in the path (the query goes in separately,
// #9160) and a bare "%".
func TestClientStillEscapesUnescapedPaths(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"/api/x/a b":       "/api/x/a%20b",
		"/api/x?limit=5":   "/api/x%3Flimit=5",
		"/api/x/50%":       "/api/x/50%25",
		"/api/x/50%zz/end": "/api/x/50%25zz/end",
	} {
		var gotEscaped, gotQuery string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotEscaped, gotQuery = r.URL.EscapedPath(), r.URL.RawQuery
			w.Write([]byte(`{}`))
		}))
		client, err := NewClient(server.URL, "secret", time.Second)
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.Do(context.Background(), http.MethodGet, in, nil, nil)
		server.Close()
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if gotEscaped != want || gotQuery != "" {
			t.Errorf("%q went out as path %s query %q, want path %s and no query", in, gotEscaped, gotQuery, want)
		}
	}
}
