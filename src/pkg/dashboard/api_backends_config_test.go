package dashboard

import (
	"net/http"
	"testing"
)

func TestBackendsPolicyRoundTrip(t *testing.T) {
	s := govServer(t)
	rec := doPut(s, "/api/config/backends", map[string]any{
		"allow": []string{"copilot", "claude"},
		"deny":  []string{"codex"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", rec.Code, rec.Body.String())
	}
	if got := s.deps.Config.Backends.Allow; len(got) != 2 || got[0] != "copilot" || got[1] != "claude" {
		t.Fatalf("allow = %#v", got)
	}
	if got := s.deps.Config.Backends.Deny; len(got) != 1 || got[0] != "codex" {
		t.Fatalf("deny = %#v", got)
	}
}

func TestBackendsPolicyRejectsUnknownBackend(t *testing.T) {
	s := govServer(t)
	s.deps.Config.Backends.Allow = []string{"copilot"}
	rec := doPut(s, "/api/config/backends", map[string]any{
		"allow": []string{"definitely-not-a-backend"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if got := s.deps.Config.Backends.Allow; len(got) != 1 || got[0] != "copilot" {
		t.Fatalf("invalid PUT mutated allow list: %#v", got)
	}
}
