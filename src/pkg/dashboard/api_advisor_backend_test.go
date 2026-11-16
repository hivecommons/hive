package dashboard

import (
	"net/http"
	"strings"
	"testing"
)

// The fleet and per-agent advisor backend are edited like the other advisor
// fields: a valid value is saved (normalized) and shown per agent with its
// source, an unset one follows governor.reviewer_backend, and a value that
// names no path is refused before anything changes.
func TestAdvisorSettingsBackend(t *testing.T) {
	s := newFullServer(t)
	cfg := s.deps.Config

	resp := decodeAdvisorSettings(t, advisorSettingsRequest(t, s, http.MethodGet, "/api/config/advisor", "", true))
	if a := advisorSettingsAgent(t, resp, "scanner"); a.Effective.Backend != "auto" || a.Effective.Sources["backend"] != "default" {
		t.Fatalf("unset backend = %q (%s), want auto from governor.reviewer_backend", a.Effective.Backend, a.Effective.Sources["backend"])
	}

	resp = decodeAdvisorSettings(t, advisorSettingsRequest(t, s, http.MethodPut, "/api/config/advisor", `{"backend":" Claude-CLI "}`, true))
	if cfg.Advisor.Backend != "claude-cli" {
		t.Fatalf("fleet backend = %q, want claude-cli", cfg.Advisor.Backend)
	}
	if a := advisorSettingsAgent(t, resp, "scanner"); a.Effective.Backend != "claude-cli" || a.Effective.Sources["backend"] != "fleet" {
		t.Fatalf("scanner backend = %q (%s), want claude-cli from the fleet", a.Effective.Backend, a.Effective.Sources["backend"])
	}

	resp = decodeAdvisorSettings(t, advisorSettingsRequest(t, s, http.MethodPut, "/api/config/advisor/agent/scanner", `{"override":{"backend":"http"}}`, true))
	if a := advisorSettingsAgent(t, resp, "scanner"); a.Effective.Backend != "http" || a.Effective.Sources["backend"] != "agent" {
		t.Fatalf("scanner backend = %q (%s), want http from its override", a.Effective.Backend, a.Effective.Sources["backend"])
	}

	for _, tc := range []struct{ path, body string }{
		{"/api/config/advisor", `{"backend":"codex-cli"}`},
		{"/api/config/advisor/agent/scanner", `{"override":{"backend":"copilot"}}`},
	} {
		rec := advisorSettingsRequest(t, s, http.MethodPut, tc.path, tc.body, true)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "must be one of auto, http, claude-cli") {
			t.Errorf("%s %s: status = %d body=%s", tc.path, tc.body, rec.Code, rec.Body.String())
		}
	}
	if cfg.Advisor.Backend != "claude-cli" || cfg.Agents["scanner"].Advisor == nil || *cfg.Agents["scanner"].Advisor.Backend != "http" {
		t.Fatalf("refused edits changed the backend: fleet %q, scanner %+v", cfg.Advisor.Backend, cfg.Agents["scanner"].Advisor)
	}
}
