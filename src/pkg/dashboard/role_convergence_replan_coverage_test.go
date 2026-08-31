package dashboard

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kubestellar/hive/pkg/config"
	"github.com/kubestellar/hive/pkg/notify"
)

// ── requestRoleAllowsOwner (server.go) ──────────────────────────────────────
//
// SECURITY (F9, CWE-862): a missing X-Hive-Role must default to least
// privilege on any spoke with an auth boundary. These tests pin every branch.

func roleReq(role string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	if role != "" {
		r.Header.Set("X-Hive-Role", role)
	}
	return r
}

func TestRequestRoleAllowsOwner_ExplicitOwnerRole(t *testing.T) {
	s := newTestServer()
	if !s.requestRoleAllowsOwner(roleReq("owner")) {
		t.Fatal("explicit owner role must be allowed")
	}
}

func TestRequestRoleAllowsOwner_NonOwnerRoleDenied(t *testing.T) {
	s := newTestServer()
	for _, role := range []string{"viewer", "merger", "contributor", "Owner"} {
		if s.requestRoleAllowsOwner(roleReq(role)) {
			t.Fatalf("role %q must not be treated as owner", role)
		}
	}
}

func TestRequestRoleAllowsOwner_EmptyRoleOpenSpoke(t *testing.T) {
	// No auth token and no direct-route allowlist: a genuinely open spoke may
	// treat a missing role as owner (no security boundary exists at all).
	s := newTestServer()
	s.authToken = ""
	if !s.requestRoleAllowsOwner(roleReq("")) {
		t.Fatal("open spoke (no token, no allowlist) must allow empty role")
	}
}

func TestRequestRoleAllowsOwner_EmptyRoleDeniedWithAuthToken(t *testing.T) {
	// A shared token establishes an auth boundary: an empty role means no
	// identity was established and must NOT be promoted to owner.
	s := newTestServer()
	s.authToken = "secret"
	if s.requestRoleAllowsOwner(roleReq("")) {
		t.Fatal("empty role must be denied when an auth token is configured")
	}
}

func TestRequestRoleAllowsOwner_EmptyRoleDeniedWithDirectRouteAuthz(t *testing.T) {
	// A standalone spoke with an authorized-users allowlist (not hub-proxied)
	// enforces per-user authz itself; an empty role must fail closed even
	// without a shared token.
	s := newTestServer()
	s.authToken = ""
	s.deps = &Dependencies{Config: &config.Config{}}
	s.deps.Config.Dashboard.HubProxied = false
	s.deps.Config.Dashboard.AuthorizedUsers = []string{"alice:owner"}
	if !s.directRouteAuthzEnabled() {
		t.Fatal("test setup: direct-route authz should be enabled")
	}
	if s.requestRoleAllowsOwner(roleReq("")) {
		t.Fatal("empty role must be denied when direct-route authz is enabled")
	}
}

// ── configuredDossierCacheMaxEntries (dossier.go) ───────────────────────────

func TestConfiguredDossierCacheMaxEntries(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want int
	}{
		{"unset", "", dossierCacheMaxEntriesDefault},
		{"whitespace-only", "   ", dossierCacheMaxEntriesDefault},
		{"valid", "64", 64},
		{"whitespace-padded valid", "  128  ", 128},
		{"non-numeric", "lots", dossierCacheMaxEntriesDefault},
		{"zero", "0", dossierCacheMaxEntriesDefault},
		{"negative", "-3", dossierCacheMaxEntriesDefault},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(dossierCacheMaxEntriesEnv, tc.env)
			if got := configuredDossierCacheMaxEntries(); got != tc.want {
				t.Fatalf("env %q: got %d, want %d", tc.env, got, tc.want)
			}
		})
	}
}

// ── handleConvergenceConfigGet (api_config_convergence.go) ──────────────────

func convergenceGetReq(owner bool) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/config/convergence", nil)
	if owner {
		r.Header.Set("X-Hive-Role", "owner")
		r.Header.Set(ownerRoleVerifiedHeader, "true")
	}
	return r
}

func TestHandleConvergenceConfigGet_NonOwnerForbidden(t *testing.T) {
	s := newTestServer()
	w := httptest.NewRecorder()
	s.handleConvergenceConfigGet(w, convergenceGetReq(false))
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for non-owner, got %d", w.Code)
	}
}

func TestHandleConvergenceConfigGet_UnverifiedOwnerHeaderForbidden(t *testing.T) {
	// A client-supplied X-Hive-Role: owner without the server-only verified
	// marker must fail closed (requireOwnerRole contract).
	s := newTestServer()
	r := httptest.NewRequest(http.MethodGet, "/api/config/convergence", nil)
	r.Header.Set("X-Hive-Role", "owner")
	w := httptest.NewRecorder()
	s.handleConvergenceConfigGet(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for unverified owner header, got %d", w.Code)
	}
}

func TestHandleConvergenceConfigGet_NoConfigUnavailable(t *testing.T) {
	s := newTestServer()
	s.deps = &Dependencies{} // Config nil
	w := httptest.NewRecorder()
	s.handleConvergenceConfigGet(w, convergenceGetReq(true))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 with nil config, got %d", w.Code)
	}
}

func TestHandleConvergenceConfigGet_Success(t *testing.T) {
	s := newTestServer()
	cfg := &config.Config{}
	cfg.Convergence.Mode = config.ConvergenceModeOff
	s.deps = &Dependencies{Config: cfg}
	w := httptest.NewRecorder()
	s.handleConvergenceConfigGet(w, convergenceGetReq(true))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, key := range []string{`"mode"`, `"effective_mode"`, `"modes"`, `"generation"`} {
		if !contains2(body, key) {
			t.Fatalf("response missing %s: %s", key, body)
		}
	}
}

// contains2 avoids importing strings just for one assertion helper name that
// could collide; simple substring check.
func contains2(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// ── ReplanSink.Notify (replan_sink.go) ──────────────────────────────────────

func TestReplanSinkNotify_NilNotifierSkips(t *testing.T) {
	sink := NewReplanSink(newTestServer(), nil)
	// Must not panic: notifications are skipped when no notifier is wired.
	sink.Notify("stall", "replan issued")
}

func TestReplanSinkNotify_WithNotifierSends(t *testing.T) {
	// A notifier with no channels configured accepts Send as a no-op; this
	// exercises the non-nil branch without any network traffic.
	n := notify.New(config.NotificationsConfig{}, testLogger())
	sink := NewReplanSink(newTestServer(), n)
	sink.Notify("stall", "replan issued")
}
