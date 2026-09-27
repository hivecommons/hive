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

func TestChatJamCommandAnswersPresenceLocallyAndForwardsTheRest(t *testing.T) {
	html := indexHTML(t)
	commands := indexSliceBetween(t, html, "async function chatCommandWho()", "\n\n    async function chatRunCommand(q)")
	jamEntry := indexSliceBetween(t, html, "{ name:'jam',", "},\n") + "}"
	runNodeScript(t, `
const assert = require('node:assert/strict');
let asked = [];
async function chatFetchJson() { return { users: [{ username: 'alice', active: true, you: true }] }; }
function _inceptionAuthHeaders() { return {}; }
async function chatAskBackend(q, suppressUserEcho) { asked.push([q, suppressUserEcho]); return 'forwarded'; }
`+commands+`
const jam = (`+jamEntry+`);
(async () => {
  assert.equal(jam.run, chatCommandJam);
  assert(jam.aliases.includes('swarm'), JSON.stringify(jam.aliases));
  for (const q of ['who is online?', '', 'anyone around']) {
    const answer = await chatCommandJam(q);
    assert(answer.includes('🟢 **alice** (you) — active'), q + ' -> ' + answer);
  }
  assert.deepEqual(asked, [], 'presence questions must not reach the backend');
  assert.equal(await chatCommandJam('start a session'), 'forwarded');
  assert.deepEqual(asked, [['jam: start a session', true]]);
})().catch(err => { console.error(err); process.exit(1); });
`)
}
