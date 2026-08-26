package hivectl

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// DoWithHeaders is the only client entry point that lets a caller attach
// extra headers (e.g. idempotency keys); it had 0% coverage.
func TestClientDoWithHeadersForwardsExtraHeaders(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Hive-Idempotency-Key"); got != "abc-123" {
			t.Errorf("idempotency header = %q, want abc-123", got)
		}
		// Extra headers are additive: the client's own auth header stays.
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "secret", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	headers := http.Header{}
	headers.Set("X-Hive-Idempotency-Key", "abc-123")
	result, err := client.DoWithHeaders(context.Background(), http.MethodPost, "/api/test", url.Values{}, map[string]any{"name": "agent"}, headers)
	if err != nil {
		t.Fatal(err)
	}
	object, ok := result.(map[string]any)
	if !ok || object["ok"] != true {
		t.Fatalf("result = %#v", result)
	}
}

func TestClientDoWithHeadersPropagatesAPIError(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"nope"}`, http.StatusForbidden)
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "secret", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.DoWithHeaders(context.Background(), http.MethodGet, "/api/test", nil, nil, nil); err == nil {
		t.Fatal("expected API error to propagate through DoWithHeaders")
	}
}
