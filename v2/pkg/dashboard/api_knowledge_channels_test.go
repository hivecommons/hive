package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestKnowledgeChannelsListRoute asserts GET /api/knowledge/channels is
// actually registered on the mux (not a 404) and returns a JSON array — the
// v2-parity gap where the dashboard SPA called this route but v4 never wired
// it up.
func TestKnowledgeChannelsListRoute(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()

	rec := doGet(s, "/api/knowledge/channels")

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/knowledge/channels status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	var out []interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not a JSON array: %v (body: %s)", err, rec.Body.String())
	}
}

// TestKnowledgeChannelsListNoKnowledge asserts the handler degrades to an
// empty list rather than erroring when knowledge is disabled, mirroring the
// other knowledge list handlers.
func TestKnowledgeChannelsListNoKnowledge(t *testing.T) {
	s, _ := apiServer(t)

	rec := doGet(s, "/api/knowledge/channels")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "[]\n" && got != "[]" {
		t.Fatalf("body = %q, want empty JSON array", got)
	}
}

// TestKnowledgeChannelCreateRoute asserts POST /api/knowledge/channels is
// registered and validates its input, mirroring v2's handleKnowledgeChannelCreate.
func TestKnowledgeChannelCreateRoute(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()

	rec := doOwnerPost(s, "/api/knowledge/channels", map[string]interface{}{"name": ""})

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty channel name status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
}

// TestKnowledgeChannelCreateNoKnowledge asserts the handler reports knowledge
// as unavailable rather than 404ing when knowledge is disabled.
func TestKnowledgeChannelCreateNoKnowledge(t *testing.T) {
	s, _ := apiServer(t)

	rec := doOwnerPost(s, "/api/knowledge/channels", map[string]interface{}{"name": "test-channel"})

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body: %s)", rec.Code, rec.Body.String())
	}
}

// TestKnowledgeChannelCreateBadJSON asserts malformed bodies are rejected
// with 400 rather than panicking or falling through to a 404 (route absent).
func TestKnowledgeChannelCreateBadJSON(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/knowledge/channels", nil)
	req.Header.Set("Content-Type", "application/json")
	markOwnerRequest(req)
	req.Body = http.NoBody
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty body status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
}
