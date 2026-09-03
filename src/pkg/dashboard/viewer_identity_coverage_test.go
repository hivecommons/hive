package dashboard

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/hivecommons/hive/pkg/beads"
)

// ---------- resolveViewerUsername (api_contribute.go) ----------
//
// resolveViewerUsername is the SERVER-SIDE identity used to gate contributor
// mutations (invite minting, register force-rotation). Its four resolution
// tiers — per-user session, hub-injected header, direct-route anonymity, and
// the persisted owner token — were only partially pinned (43.8%): the session
// tier and the whole owner-token tier (missing/empty/invalid/valid token file)
// had no tests. A regression there silently changes WHO the server believes is
// calling, which is a trust-gate bypass, not a cosmetic bug.

// A valid per-user session outranks every other identity source, including a
// conflicting hub header on the same request.
func TestResolveViewerUsername_SessionWins(t *testing.T) {
	s, _ := apiServer(t)

	id := s.createUserSession("session-user", "viewer")
	if id == "" {
		t.Fatal("createUserSession returned empty id")
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: id})
	req.Header.Set("X-Hive-User", "header-imposter")

	if got := s.resolveViewerUsername(req); got != "session-user" {
		t.Fatalf("expected session identity to win, got %q", got)
	}
}

// With no session, the hub-injected X-Hive-User header is honored.
func TestResolveViewerUsername_HubHeader(t *testing.T) {
	s, _ := apiServer(t)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Hive-User", "hub-user")

	if got := s.resolveViewerUsername(req); got != "hub-user" {
		t.Fatalf("expected hub header identity, got %q", got)
	}
}

// On a direct-route spoke (allowlist configured, not hub-proxied) the owner
// token must NOT be consulted: an anonymous request resolves to "" even when a
// valid token file exists on disk.
func TestResolveViewerUsername_DirectRouteStaysAnonymous(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Dashboard.AuthorizedUsers = []string{"alice"}
	deps.Config.Dashboard.HubProxied = false

	// A token file that WOULD resolve if the direct-route guard failed.
	if err := os.WriteFile(userTokenPath, []byte("gho_sometoken"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if got := s.resolveViewerUsername(req); got != "" {
		t.Fatalf("direct-route spoke must not fall through to owner token, got %q", got)
	}
}

// No session, no header, no token file on disk: anonymous.
func TestResolveViewerUsername_NoTokenFile(t *testing.T) {
	s, _ := apiServer(t)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if got := s.resolveViewerUsername(req); got != "" {
		t.Fatalf("expected anonymous with no token file, got %q", got)
	}
}

// A token file containing only whitespace is treated as absent.
func TestResolveViewerUsername_EmptyTokenFile(t *testing.T) {
	s, _ := apiServer(t)

	if err := os.WriteFile(userTokenPath, []byte("  \n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if got := s.resolveViewerUsername(req); got != "" {
		t.Fatalf("expected anonymous with blank token file, got %q", got)
	}
}

// A persisted token GitHub rejects resolves to anonymous, never an error page.
func TestResolveViewerUsername_InvalidOwnerToken(t *testing.T) {
	s, deps := apiServer(t)

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer mock.Close()
	deps.Config.GitHub.OAuthAPIURLOverride = mock.URL

	if err := os.WriteFile(userTokenPath, []byte("gho_revoked"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if got := s.resolveViewerUsername(req); got != "" {
		t.Fatalf("expected anonymous for rejected token, got %q", got)
	}
}

// The single-owner-spoke fallback: a valid persisted owner token resolves to
// the GitHub login it validates to.
func TestResolveViewerUsername_ValidOwnerToken(t *testing.T) {
	s, deps := apiServer(t)

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"login":"spoke-owner"}`))
	}))
	defer mock.Close()
	deps.Config.GitHub.OAuthAPIURLOverride = mock.URL

	if err := os.WriteFile(userTokenPath, []byte("gho_valid\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if got := s.resolveViewerUsername(req); got != "spoke-owner" {
		t.Fatalf("expected owner-token identity spoke-owner, got %q", got)
	}
}

// ---------- resolveContributeCaller (api_contribute.go) ----------
//
// resolveContributeCaller (61.5%) adds the Authorization-header tier on top of
// resolveViewerUsername. The Bearer/token prefix parsing and the empty-token
// fall-through were uncovered.

// "Bearer <tok>" is accepted and validated against the configured API.
func TestResolveContributeCaller_BearerToken(t *testing.T) {
	s, deps := apiServer(t)

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"login":"bearer-user"}`))
	}))
	defer mock.Close()
	deps.Config.GitHub.OAuthAPIURLOverride = mock.URL

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Authorization", "Bearer gho_abc")

	if got := s.resolveContributeCaller(req); got != "bearer-user" {
		t.Fatalf("expected bearer-user, got %q", got)
	}
}

// The legacy "token <tok>" scheme is accepted too.
func TestResolveContributeCaller_TokenScheme(t *testing.T) {
	s, deps := apiServer(t)

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"login":"legacy-user"}`))
	}))
	defer mock.Close()
	deps.Config.GitHub.OAuthAPIURLOverride = mock.URL

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Authorization", "token gho_def")

	if got := s.resolveContributeCaller(req); got != "legacy-user" {
		t.Fatalf("expected legacy-user, got %q", got)
	}
}

// An Authorization header in an unrecognized scheme yields anonymous — the
// token is never sent anywhere for validation.
func TestResolveContributeCaller_UnrecognizedScheme(t *testing.T) {
	s, _ := apiServer(t)

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")

	if got := s.resolveContributeCaller(req); got != "" {
		t.Fatalf("expected anonymous for Basic scheme, got %q", got)
	}
}

// No identity anywhere: anonymous.
func TestResolveContributeCaller_Anonymous(t *testing.T) {
	s, _ := apiServer(t)

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	if got := s.resolveContributeCaller(req); got != "" {
		t.Fatalf("expected anonymous, got %q", got)
	}
}

// The viewer tier (hub header) short-circuits before Authorization parsing.
func TestResolveContributeCaller_ViewerWins(t *testing.T) {
	s, _ := apiServer(t)

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("X-Hive-User", "viewer-user")
	req.Header.Set("Authorization", "Bearer gho_ignored")

	if got := s.resolveContributeCaller(req); got != "viewer-user" {
		t.Fatalf("expected viewer-user via header tier, got %q", got)
	}
}

// ---------- detectedProjectObservability (api_governor_project_observability.go) ----------
//
// detectedProjectObservability (45.5%) feeds the Governor tab's platform
// suggestions from the telemetry agent's beads. The nil guards and the
// bead-scanning path were uncovered.

func TestDetectedProjectObservability_NilGuards(t *testing.T) {
	// nil BeadStores map.
	s, _ := apiServer(t)
	if got := s.detectedProjectObservability(); got != nil {
		t.Fatalf("expected nil with nil BeadStores, got %v", got)
	}

	// BeadStores present but no telemetry store.
	s.deps.BeadStores = map[string]*beads.Store{}
	if got := s.detectedProjectObservability(); got != nil {
		t.Fatalf("expected nil without telemetry store, got %v", got)
	}
}

func TestDetectedProjectObservability_DetectsPlatformsFromBeads(t *testing.T) {
	s, deps := apiServer(t)

	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("beads.NewStore: %v", err)
	}
	if _, err := store.Create(
		"Cluster exports metrics to Prometheus and Grafana dashboards",
		beads.TypeAdvisory, beads.PriorityMedium, "telemetry", ""); err != nil {
		t.Fatalf("create bead: %v", err)
	}
	if _, err := store.Create(
		"Datadog agent daemonset detected in kube-system",
		beads.TypeAdvisory, beads.PriorityMedium, "telemetry", ""); err != nil {
		t.Fatalf("create bead: %v", err)
	}
	deps.BeadStores = map[string]*beads.Store{"telemetry": store}

	got := s.detectedProjectObservability()
	if got == nil {
		t.Fatal("expected detections, got nil")
	}

	want := map[string][]string{
		"open_source": {"grafana", "prometheus"},
		"commercial":  {"datadog"},
	}
	for family, platforms := range want {
		found := map[string]bool{}
		for _, p := range got[family] {
			found[p] = true
		}
		for _, p := range platforms {
			if !found[p] {
				t.Errorf("family %s: expected %q detected, got %v", family, p, got[family])
			}
		}
	}
	if len(got["kube_native"]) != 0 {
		t.Errorf("expected no kube_native detections, got %v", got["kube_native"])
	}
}

// ---------- configuredDossierCacheMaxEntries (dossier.go) ----------
//
// configuredDossierCacheMaxEntries (42.9%) bounds the public dossier caches
// against username-spray memory growth; only the unset branch was pinned.

func TestConfiguredDossierCacheMaxEntries(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want int
	}{
		{"unset uses default", "", dossierCacheMaxEntriesDefault},
		{"whitespace uses default", "   ", dossierCacheMaxEntriesDefault},
		{"non-numeric uses default", "lots", dossierCacheMaxEntriesDefault},
		{"zero uses default", "0", dossierCacheMaxEntriesDefault},
		{"negative uses default", "-3", dossierCacheMaxEntriesDefault},
		{"valid override honored", "64", 64},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(dossierCacheMaxEntriesEnv, tc.raw)
			if got := configuredDossierCacheMaxEntries(); got != tc.want {
				t.Fatalf("raw %q: got %d, want %d", tc.raw, got, tc.want)
			}
		})
	}
}
