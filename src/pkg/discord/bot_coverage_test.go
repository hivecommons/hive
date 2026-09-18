package discord

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// SetAgentNames
// ---------------------------------------------------------------------------

func TestSetChannelTopic_CorrectRequest(t *testing.T) {
	var gotMethod string
	var gotPath string
	var gotBody string
	var gotAuth string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	b := newTestBot(ts, "ch123")
	err := b.setChannelTopic("test topic")
	if err != nil {
		t.Fatalf("setChannelTopic error: %v", err)
	}

	if gotMethod != http.MethodPatch {
		t.Errorf("method = %q, want PATCH", gotMethod)
	}
	if !strings.Contains(gotPath, "ch123") {
		t.Errorf("path should contain channel ID, got: %s", gotPath)
	}
	if gotAuth != "Bot test-token" {
		t.Errorf("auth = %q, want 'Bot test-token'", gotAuth)
	}
	if !strings.Contains(gotBody, "test topic") {
		t.Errorf("body should contain topic, got: %s", gotBody)
	}
}

func TestSetChannelTopic_Error(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte("rate limited"))
	}))
	defer ts.Close()

	b := newTestBot(ts, "ch")
	err := b.setChannelTopic("topic")
	if err == nil {
		t.Fatal("expected error for rate-limited response")
	}
}

// ---------------------------------------------------------------------------
// registerBuiltinCommands
// ---------------------------------------------------------------------------
