package dashboard

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// deadLoopbackURL returns an http URL on 127.0.0.1 pointing at a port that was
// just listened on and closed, so any connection attempt fails immediately
// with connection-refused instead of hanging. This lets a test drive
// ConnectGitSource through URL validation into a fast, deterministic clone
// failure without any network access.
func deadLoopbackURL(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve loopback port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("release loopback port: %v", err)
	}
	return "http://" + addr + "/repo.git"
}

// TestGitSourcesConnect_OwnerGate pins that POST and DELETE on
// /api/knowledge/git-sources are refused outright without a verified owner
// role: these endpoints mutate persisted config and trigger outbound git
// clones, so the gate must fire before any body parsing or validation.
func TestGitSourcesConnect_OwnerGate(t *testing.T) {
	s := covApiServer(t)

	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		req := httptest.NewRequest(method, "/api/knowledge/git-sources",
			strings.NewReader(`{"name":"docs","url":"https://example.com/r.git"}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s without owner role: expected 403, got %d", method, rec.Code)
		}
	}
}

// TestGitSourcesConnect_DefaultLayerCloneFailure drives handleGitSourcesConnect
// past URL validation (HIVE_ALLOW_PRIVATE_GIT_SOURCE permits the loopback
// literal) into ConnectGitSource against a dead port. It pins the arms that sit
// between validation and success:
//   - an omitted layer defaults to "project" (the request must not be rejected
//     for a missing layer),
//   - a ConnectGitSource failure surfaces as HTTP 500 with the error text, and
//   - a failed connect must NOT append the source to persisted config —
//     otherwise a bad URL would be retried by every future startup.
func TestGitSourcesConnect_DefaultLayerCloneFailure(t *testing.T) {
	t.Setenv("HIVE_ALLOW_PRIVATE_GIT_SOURCE", "true")

	s, deps := apiServer(t)
	url := deadLoopbackURL(t)

	rec := doPost(s, "/api/knowledge/git-sources", map[string]any{
		"name": "dead-docs",
		"url":  url,
	})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("connect to dead port: expected 500, got %d (body %q)", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "dead-docs") {
		t.Errorf("expected error body to identify the failing source, got %q", body)
	}
	if n := len(deps.Config.Knowledge.GitSources); n != 0 {
		t.Errorf("failed connect must not persist config entry, got %d entries", n)
	}
}

// TestGitSourcesConnect_ExplicitLayerCloneFailure repeats the dead-port drive
// with an explicit layer so the non-defaulted arm is pinned too: the supplied
// layer must be accepted verbatim (no rejection, no silent rewrite to
// "project") all the way into the connect attempt.
func TestGitSourcesConnect_ExplicitLayerCloneFailure(t *testing.T) {
	t.Setenv("HIVE_ALLOW_PRIVATE_GIT_SOURCE", "true")

	s, deps := apiServer(t)
	url := deadLoopbackURL(t)

	rec := doPost(s, "/api/knowledge/git-sources", map[string]any{
		"name":  "dead-org-docs",
		"url":   url,
		"layer": "org",
	})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("connect to dead port: expected 500, got %d (body %q)", rec.Code, rec.Body.String())
	}
	if n := len(deps.Config.Knowledge.GitSources); n != 0 {
		t.Errorf("failed connect must not persist config entry, got %d entries", n)
	}
}
