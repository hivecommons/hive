package linearagent

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// signBody produces Linear's bare-hex HMAC-SHA256 signature.
func signBody(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// eventRecorder collects handled events.
type eventRecorder struct {
	mu     sync.Mutex
	events []SessionEvent
	seen   chan struct{}
}

func newEventRecorder() *eventRecorder {
	return &eventRecorder{seen: make(chan struct{}, 16)}
}

func (r *eventRecorder) handle(ev SessionEvent) {
	r.mu.Lock()
	r.events = append(r.events, ev)
	r.mu.Unlock()
	r.seen <- struct{}{}
}

func (r *eventRecorder) waitOne(t *testing.T) SessionEvent {
	t.Helper()
	select {
	case <-r.seen:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never called")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.events[len(r.events)-1]
}

func (r *eventRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

// testWebhook builds a receiver with a fixed secret and clock.
func testWebhook(handle func(SessionEvent), secret string, now time.Time) *WebhookReceiver {
	w := NewWebhookReceiver(handle, quietLogger())
	w.SetSecretFunc(func() string { return secret })
	w.SetClock(func() time.Time { return now })
	return w
}

// sessionEventBody renders a signed-ready AgentSessionEvent payload.
func sessionEventBody(t *testing.T, action string, ts time.Time) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]interface{}{
		"type":             "AgentSessionEvent",
		"action":           action,
		"organizationId":   "org-1",
		"webhookTimestamp": ts.UnixMilli(),
		"agentSession": map[string]interface{}{
			"id": "sess-1",
			"issue": map[string]interface{}{
				"id": "iss-1", "identifier": "ENG-42", "title": "Fix the flux capacitor",
				"url": "https://linear.app/acme/issue/ENG-42",
			},
			"comment": map[string]interface{}{"id": "c-1", "body": "@hive please fix"},
		},
		"agentActivity": map[string]interface{}{"content": map[string]interface{}{"body": "try again with tests"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func postWebhook(w *WebhookReceiver, body []byte, sig string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/linear/webhook", strings.NewReader(string(body)))
	if sig != "" {
		req.Header.Set("Linear-Signature", sig)
	}
	w.ServeHTTP(rec, req)
	return rec
}

func TestWebhook_ValidSignature(t *testing.T) {
	now := time.Now()
	rec := newEventRecorder()
	w := testWebhook(rec.handle, "s3cr3t", now)
	body := sessionEventBody(t, "created", now)

	resp := postWebhook(w, body, signBody(body, "s3cr3t"))
	if resp.Code != http.StatusOK {
		t.Fatalf("code = %d — %s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), `"handled":true`) {
		t.Errorf("body = %s", resp.Body.String())
	}
	ev := rec.waitOne(t)
	if ev.Action != "created" || ev.AgentSession.ID != "sess-1" || ev.AgentSession.Issue.Identifier != "ENG-42" {
		t.Errorf("event = %+v", ev)
	}
}

func TestWebhook_InvalidSignature(t *testing.T) {
	now := time.Now()
	rec := newEventRecorder()
	w := testWebhook(rec.handle, "s3cr3t", now)
	body := sessionEventBody(t, "created", now)

	cases := map[string]string{
		"wrong secret":    signBody(body, "wrong"),
		"missing header":  "",
		"not hex":         "zzzz",
		"github-prefixed": "sha256=" + signBody(body, "s3cr3t"),
	}
	for name, sig := range cases {
		if resp := postWebhook(w, body, sig); resp.Code != http.StatusUnauthorized {
			t.Errorf("%s: code = %d, want 401", name, resp.Code)
		}
	}
	if rec.count() != 0 {
		t.Errorf("handler called %d times on unauthenticated posts", rec.count())
	}
}

func TestWebhook_MissingSecretFailsClosed(t *testing.T) {
	now := time.Now()
	rec := newEventRecorder()
	w := testWebhook(rec.handle, "", now)
	body := sessionEventBody(t, "created", now)
	// Even a self-consistent signature must be refused: with no configured
	// secret there is nothing to verify against.
	if resp := postWebhook(w, body, signBody(body, "")); resp.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", resp.Code)
	}
	if rec.count() != 0 {
		t.Error("handler called with no secret configured")
	}
}

func TestWebhook_ReplayGuard(t *testing.T) {
	now := time.Now()
	rec := newEventRecorder()
	w := testWebhook(rec.handle, "s3cr3t", now)

	cases := map[string]time.Time{
		"stale":  now.Add(-2 * time.Minute),
		"future": now.Add(2 * time.Minute),
		"zero":   time.UnixMilli(0),
	}
	for name, ts := range cases {
		body := sessionEventBody(t, "created", ts)
		if name == "zero" {
			// A payload with no timestamp at all.
			var m map[string]interface{}
			json.Unmarshal(body, &m)
			delete(m, "webhookTimestamp")
			body, _ = json.Marshal(m)
		}
		if resp := postWebhook(w, body, signBody(body, "s3cr3t")); resp.Code != http.StatusUnauthorized {
			t.Errorf("%s timestamp: code = %d, want 401", name, resp.Code)
		}
	}
	// Within the window on either side is accepted.
	body := sessionEventBody(t, "created", now.Add(-30*time.Second))
	if resp := postWebhook(w, body, signBody(body, "s3cr3t")); resp.Code != http.StatusOK {
		t.Errorf("fresh timestamp: code = %d", resp.Code)
	}
	if rec.count() == 0 {
		rec.waitOne(t)
	}
}

func TestWebhook_MethodAndBodyValidation(t *testing.T) {
	now := time.Now()
	rec := newEventRecorder()
	w := testWebhook(rec.handle, "s3cr3t", now)

	getRec := httptest.NewRecorder()
	w.ServeHTTP(getRec, httptest.NewRequest(http.MethodGet, "/api/linear/webhook", nil))
	if getRec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET code = %d", getRec.Code)
	}

	bad := []byte("{not json")
	if resp := postWebhook(w, bad, signBody(bad, "s3cr3t")); resp.Code != http.StatusBadRequest {
		t.Errorf("bad JSON code = %d", resp.Code)
	}

	// AgentSessionEvent with no session id.
	body, _ := json.Marshal(map[string]interface{}{
		"type": "AgentSessionEvent", "action": "created",
		"webhookTimestamp": now.UnixMilli(),
	})
	if resp := postWebhook(w, body, signBody(body, "s3cr3t")); resp.Code != http.StatusBadRequest {
		t.Errorf("missing session id code = %d", resp.Code)
	}
	if rec.count() != 0 {
		t.Errorf("handler called %d times", rec.count())
	}
}

func TestWebhook_IgnoresOtherEventsAndActions(t *testing.T) {
	now := time.Now()
	rec := newEventRecorder()
	w := testWebhook(rec.handle, "s3cr3t", now)

	issue, _ := json.Marshal(map[string]interface{}{
		"type": "Issue", "action": "create", "webhookTimestamp": now.UnixMilli(),
	})
	resp := postWebhook(w, issue, signBody(issue, "s3cr3t"))
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), `"handled":false`) {
		t.Errorf("Issue event: %d %s", resp.Code, resp.Body.String())
	}

	odd, _ := json.Marshal(map[string]interface{}{
		"type": "AgentSessionEvent", "action": "archived", "webhookTimestamp": now.UnixMilli(),
		"agentSession": map[string]interface{}{"id": "sess-1"},
	})
	resp = postWebhook(w, odd, signBody(odd, "s3cr3t"))
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), `"handled":false`) {
		t.Errorf("unknown action: %d %s", resp.Code, resp.Body.String())
	}
	if rec.count() != 0 {
		t.Errorf("handler called %d times", rec.count())
	}
}

func TestWebhook_ReadsSecretFromEnv(t *testing.T) {
	t.Setenv("LINEAR_WEBHOOK_SECRET", "env-secret")
	now := time.Now()
	rec := newEventRecorder()
	w := NewWebhookReceiver(rec.handle, quietLogger())
	w.SetClock(func() time.Time { return now })
	body := sessionEventBody(t, "created", now)
	if resp := postWebhook(w, body, signBody(body, "env-secret")); resp.Code != http.StatusOK {
		t.Fatalf("code = %d", resp.Code)
	}
	rec.waitOne(t)
}

func TestSessionEvent_PromptText(t *testing.T) {
	ev := SessionEvent{Action: "prompted"}
	ev.AgentSession.Issue.Title = "title"
	ev.AgentSession.Comment.Body = "comment"
	ev.PromptContext = "top ctx"
	ev.AgentSession.PromptContext = "nested ctx"
	ev.AgentActivity.Body = "flat body"
	ev.AgentActivity.Content.Body = "content body"

	if got := ev.PromptText(); got != "content body" {
		t.Errorf("prompted prefers content body: %q", got)
	}
	ev.AgentActivity.Content.Body = ""
	if got := ev.PromptText(); got != "flat body" {
		t.Errorf("prompted falls back to flat body: %q", got)
	}
	ev.Action = "created"
	if got := ev.PromptText(); got != "top ctx" {
		t.Errorf("created prefers promptContext: %q", got)
	}
	ev.PromptContext = ""
	if got := ev.PromptText(); got != "nested ctx" {
		t.Errorf("nested promptContext: %q", got)
	}
	ev.AgentSession.PromptContext = ""
	if got := ev.PromptText(); got != "comment" {
		t.Errorf("comment fallback: %q", got)
	}
	ev.AgentSession.Comment.Body = ""
	if got := ev.PromptText(); got != "title" {
		t.Errorf("title fallback: %q", got)
	}
}
