package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// `/jam who is online?` used to be forwarded as free text and bounced off the
// responder. Presence questions are now answered locally from the same
// roster /api/presence serves, for both the jam scope and plain wording.
func TestHandleChat_JamPresenceAnsweredLocally(t *testing.T) {
	s, deps := apiServer(t)
	deps.ChatResponder = func(_ context.Context, _ string, _ []any) (string, error) {
		t.Fatalf("presence question must not reach the responder")
		return "", nil
	}
	s.markUserEngaged("alice", time.Now())

	for _, query := range []string{"jam: who is online?", "who is here", "presence"} {
		var body bytes.Buffer
		_ = json.NewEncoder(&body).Encode(map[string]string{"query": query})
		req := httptest.NewRequest(http.MethodPost, "/api/chat", &body)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Hive-User", "alice")
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%q: status = %d, want 200", query, rec.Code)
		}
		result := decodeJSON(t, rec)
		answer, _ := result["answer"].(string)
		if result["status"] != "ok" || !strings.Contains(answer, "🟢 **alice** (you) — active") {
			t.Fatalf("%q: result = %#v, want local presence answer", query, result)
		}
	}
}

func TestHandleChat_JamPresenceUnauthenticatedIsLocalOnly(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPost(s, "/api/chat", map[string]interface{}{"query": "jam: who is online?"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	answer, _ := decodeJSON(t, rec)["answer"].(string)
	if !strings.Contains(answer, "No authenticated users are visible") {
		t.Fatalf("answer = %q, want unauthenticated presence hint", answer)
	}
}

func TestChatPresenceIntent(t *testing.T) {
	for query, want := range map[string]bool{
		"jam: who is online?":   true,
		"who is around":         true,
		"online":                true,
		"who completed last?":   false,
		"agents stuck":          false,
		"prs waiting on review": false,
	} {
		if got := chatPresenceIntent(chatIntentTokens(query)); got != want {
			t.Errorf("chatPresenceIntent(%q) = %v, want %v", query, got, want)
		}
	}
}

func TestChatJamCommandStaticWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		"async function chatCommandJam(args)",
		"run: chatCommandJam }",
		"CHAT_JAM_PRESENCE_RE = /\\b(online|who|free|here|around|present|presence|active|idle)\\b/i",
		"return await chatCommandWho();",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
}
