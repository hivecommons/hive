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

func TestFacadeSettersDelegateToChatService(t *testing.T) {
	b := NewBot(Config{Token: "t", ChannelID: "c"}, discardLogger())

	SetAgentIdentities(map[string]AgentIdentity{"scanner": {Emoji: "🔎", Color: 0x123456}})
	SetAgentAliases(map[string]string{"scan": "scanner"})
	b.SetAgentNames([]string{"scanner"})
}

func TestSetTopic_DelegatesThroughFacadeAndBackend(t *testing.T) {
	var patchCount int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("method = %q, want PATCH", r.Method)
		}
		patchCount++
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	b := newTestBot(ts, "ch")
	if err := b.SetTopic("facade topic"); err != nil {
		t.Fatalf("Bot.SetTopic error: %v", err)
	}
	if err := b.discordBackend.SetTopic("backend topic"); err != nil {
		t.Fatalf("discordBackend.SetTopic error: %v", err)
	}
	if patchCount != 2 {
		t.Fatalf("topic PATCH count = %d, want 2", patchCount)
	}
}

func TestSetChannelTopic_NewRequestError(t *testing.T) {
	b := NewBot(Config{Token: "tok", ChannelID: "\x00"}, discardLogger())
	if err := b.setChannelTopic("topic"); err == nil {
		t.Fatal("expected error for invalid channel ID")
	}
}

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
