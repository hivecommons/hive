package hub

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ============================================================
// saas.go — hive management handlers (auth/validation/kubectl-error paths)
//
// These handlers gate on getAuthUser (cookie -> user on disk) then either fail
// validation or reach a kubectl call that errors under the fail-fast fake
// kubectl, exercising the request-handling body without a real cluster.
// ============================================================

// testHubSecret is the shared hub secret every test signs its session cookie
// with. Tests that build a HubServer relying on cookie auth set
// hubSecret: testHubSecret (or arrange for NewHubServer to load it via
// HIVE_HUB_SECRET, e.g. through helperSetupTempDirs) so the signature the
// server recomputes matches the one baked into the cookie.
const testHubSecret = "test-hub-secret-f4"

// testAuthCookie mints a signed hive_hub_user cookie for username using the
// derived SESSION sub-key of testHubSecret (C2 domain separation) — the same key
// the hub now verifies session cookies with. A cookie signed with the raw master
// no longer verifies (that is the whole point of the fix), so tests must sign
// with the session sub-key to match getAuthUser / handleAuthUser.
// testAuthCookie mints the cookie production mints: V2 / Ed25519.
//
// AUDIT F1: this helper used to mint the LEGACY symmetric format keyed on the
// derived session key. That lane is deleted (any spoke holds the same key, so
// accepting it means any spoke can forge hub admin), and a test helper that
// keeps minting it would silently exercise a path production no longer has.
func testAuthCookie(username string) *http.Cookie {
	seed := deriveDomainKey(testHubSecret, infoSessionEd25519Seed)
	return &http.Cookie{Name: "hive_hub_user", Value: mintHubUserCookieValueV2(seed, username)}
}

// heartbeatBearer returns the PER-HIVE heartbeat bearer for a given master
// secret and hive ID. The hub verifies /api/heartbeat and /api/task-status
// against HMAC(master, infoHeartbeatKey || 0x00 || hiveID) and nothing else.
//
// This used to mint the fleet-wide sub-key, deriveDomainKey(master,
// infoHeartbeatKey) — the F2 lane, one value shared by every spoke, which the
// hub no longer accepts. hiveID MUST match the hive_id in the request body, or
// the heartbeat is (correctly) refused as an impersonation attempt.
func heartbeatBearer(master, hiveID string) string {
	return "Bearer " + derivePerHiveKey(master, infoHeartbeatKey, hiveID)
}

// mkUser writes a SaaS user to the temp dir so getAuthUser resolves the cookie.
func mkUser(t *testing.T, username string) {
	t.Helper()
	if err := saveSaaSUser(&SaaSUser{GitHubUsername: username, Hives: map[string]string{}}); err != nil {
		t.Fatalf("saveSaaSUser: %v", err)
	}
}

func reqWithUser(method, target, body, username string) *http.Request {
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	r.AddCookie(testAuthCookie(username))
	return r
}

// setPathValue attaches a path variable the same way net/http's mux would.
func setPathValue(r *http.Request, key, val string) *http.Request {
	r.SetPathValue(key, val)
	return r
}

func newHandlerHub() *HubServer {
	return &HubServer{
		logger:    slog.Default(),
		hubSecret: testHubSecret,
		// Mirror NewHubServer: a real hub ALWAYS has a generation set holding
		// its master (legacyGenerationSet). Without this the impersonation
		// verifier — the first adopter of hub_generations.go — sees a nil set
		// and fails closed, which is correct behavior but not the behavior
		// these handler tests are exercising.
		keyGenerations:     legacyGenerationSet(testHubSecret),
		hubBanners:         make(map[string]*HubBannerEntry),
		heartbeatSwitchTag: make(map[string]string),
		heartbeatUpgrade:   make(map[string]string),
		// Domain is load-bearing, not decoration: the placeholder URL a hive
		// with no reported dashboard URL falls back to is now derived from ITS
		// OWN cluster's domain (placeholderHostURL) instead of a hardcoded
		// hive.kubestellar.io. A cluster with no domain therefore yields no
		// URL at all — which is the correct fail-closed answer, and is why this
		// fixture must name the domain it is standing in for.
		clusters: map[string]ClusterConfig{"hive-oke": {ID: "hive-oke", Domain: "hive.kubestellar.io"}},
	}
}

func TestHandleAdminDeleteUser(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHandlerHub()
	mkUser(t, hubAdminUsername)

	// Cannot delete admin.
	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser(http.MethodDelete, "/x", "", hubAdminUsername), "username", hubAdminUsername)
	s.handleAdminDeleteUser(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("delete admin status = %d, want 403", rec.Code)
	}

	// Invalid username.
	rec = httptest.NewRecorder()
	req = setPathValue(reqWithUser(http.MethodDelete, "/x", "", hubAdminUsername), "username", "../evil")
	s.handleAdminDeleteUser(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("invalid username status = %d, want 400", rec.Code)
	}

	// Not found.
	rec = httptest.NewRecorder()
	req = setPathValue(reqWithUser(http.MethodDelete, "/x", "", hubAdminUsername), "username", "ghost")
	s.handleAdminDeleteUser(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("missing user status = %d, want 404", rec.Code)
	}

	// Owns a hive -> conflict.
	saveSaaSUser(&SaaSUser{GitHubUsername: "owner1", Hives: map[string]string{"h1": "owner"}})
	rec = httptest.NewRecorder()
	req = setPathValue(reqWithUser(http.MethodDelete, "/x", "", hubAdminUsername), "username", "owner1")
	s.handleAdminDeleteUser(rec, req)
	if rec.Code != http.StatusConflict {
		t.Errorf("owns-hive status = %d, want 409", rec.Code)
	}

	// Clean delete.
	saveSaaSUser(&SaaSUser{GitHubUsername: "plain", Hives: map[string]string{}})
	rec = httptest.NewRecorder()
	req = setPathValue(reqWithUser(http.MethodDelete, "/x", "", hubAdminUsername), "username", "plain")
	s.handleAdminDeleteUser(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("clean delete status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestHandleOpenHive(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHandlerHub()
	// newHandlerHub already sets hubSecret to testHubSecret, which is what
	// reqWithUser signs its session cookie with — keep them in sync so the
	// authenticated open paths below verify.

	// Invalid id.
	rec := httptest.NewRecorder()
	req := setPathValue(httptest.NewRequest(http.MethodGet, "/open", nil), "id", "../x")
	s.handleOpenHive(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("invalid id status = %d, want 400", rec.Code)
	}

	// Not logged in -> redirect to login.
	rec = httptest.NewRecorder()
	req = setPathValue(httptest.NewRequest(http.MethodGet, "/open", nil), "id", "hosted-x")
	s.handleOpenHive(rec, req)
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "/login") {
		t.Errorf("anon open: status=%d loc=%q", rec.Code, rec.Header().Get("Location"))
	}

	// Admin opening a hosted hive -> SSO redirect.
	mkUser(t, hubAdminUsername)
	rec = httptest.NewRecorder()
	req = setPathValue(reqWithUser(http.MethodGet, "/open", "", hubAdminUsername), "id", "hosted-abc")
	s.handleOpenHive(rec, req)
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "/sso?token=") {
		t.Errorf("admin open: status=%d loc=%q", rec.Code, rec.Header().Get("Location"))
	}
	// INVERTED (not relaxed): this used to assert only that SOME /sso?token=
	// redirect happened, which passed just as happily when the host was
	// unreachable — the 503 bug. Assert the HOST too: it must be the one this
	// hive's own cluster serves.
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "https://hosted-abc.hive.kubestellar.io/sso?token=") {
		t.Errorf("admin open must hand off to the host the hive's cluster serves, got %q", loc)
	}

	// Non-owner, non-authorized -> access denied.
	mkUser(t, "stranger")
	saveSaaSHive(&SaaSHive{ID: "hosted-abc", Owner: "someone-else"})
	rec = httptest.NewRecorder()
	req = setPathValue(reqWithUser(http.MethodGet, "/open", "", "stranger"), "id", "hosted-abc")
	s.handleOpenHive(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("stranger open status = %d, want 403", rec.Code)
	}

	// No reachable URL (non-hosted id, not in registry) -> conflict.
	mkUser(t, hubAdminUsername)
	rec = httptest.NewRecorder()
	req = setPathValue(reqWithUser(http.MethodGet, "/open", "", hubAdminUsername), "id", "plainid")
	s.handleOpenHive(rec, req)
	if rec.Code != http.StatusConflict {
		t.Errorf("no-url open status = %d, want 409", rec.Code)
	}
}

func TestHandleToggleVisibility(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHandlerHub()
	mkUser(t, "alice")

	// OPTIONS preflight.
	rec := httptest.NewRecorder()
	req := reqWithUser(http.MethodOptions, "/vis", "", "alice")
	req.Header.Set("Origin", "https://hive.kubestellar.io")
	s.handleToggleVisibility(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Errorf("OPTIONS status = %d, want 204", rec.Code)
	}

	// Hive not found.
	rec = httptest.NewRecorder()
	req = setPathValue(reqWithUser(http.MethodPut, "/vis", `{"is_public":true}`, "alice"), "id", "missing")
	s.handleToggleVisibility(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("missing hive status = %d, want 404", rec.Code)
	}

	// Not owner.
	saveSaaSHive(&SaaSHive{ID: "h1", Owner: "bob"})
	rec = httptest.NewRecorder()
	req = setPathValue(reqWithUser(http.MethodPut, "/vis", `{"is_public":true}`, "alice"), "id", "h1")
	s.handleToggleVisibility(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("non-owner status = %d, want 403", rec.Code)
	}

	// Owner toggles (push goroutine best-effort fails; response is still ok).
	saveSaaSHive(&SaaSHive{ID: "h2", Owner: "alice"})
	rec = httptest.NewRecorder()
	req = setPathValue(reqWithUser(http.MethodPut, "/vis", `{"is_public":true}`, "alice"), "id", "h2")
	s.handleToggleVisibility(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("owner toggle status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	// Bad JSON body from owner.
	saveSaaSHive(&SaaSHive{ID: "h3", Owner: "alice"})
	rec = httptest.NewRecorder()
	req = setPathValue(reqWithUser(http.MethodPut, "/vis", `{bad`, "alice"), "id", "h3")
	s.handleToggleVisibility(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad body status = %d, want 400", rec.Code)
	}
}

func TestPushVisibilityToSpoke(t *testing.T) {
	s := newHandlerHub()
	// The in-cluster service URL is unresolvable in tests -> exercises the
	// request-failed warn path without panicking.
	s.pushVisibilityToSpoke("some-hive", true)
}

func TestHandleUpgradeHiveKubectlFails(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHandlerHub()
	mkUser(t, "alice")

	// Not found.
	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser(http.MethodPost, "/up", "", "alice"), "id", "missing")
	s.handleUpgradeHive(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("missing hive status = %d, want 404", rec.Code)
	}

	// Owner, cluster resolves, kubectl fails (fake), and no build target is
	// known for the branch — the one remaining hard-failure case. A kubectl
	// failure alone no longer fails the request: with a known target it arms
	// the heartbeat fallback instead (TestHandleUpgradeHiveUnreachableCluster*).
	saveSaaSHive(&SaaSHive{ID: "h1", Owner: "alice", ClusterID: "hive-oke"})
	// The hive must be heartbeating, otherwise the collectibility gate refuses
	// with 409 BEFORE the no-build-target check this case is about. The branch
	// deliberately has no known SHA, which is what produces the 502.
	s.registry.Hives = []RegistryEntry{{
		ID: "h1", GitBranch: "branch-with-no-build",
		LastHeartbeat: time.Now().UTC().Format(time.RFC3339),
	}}
	rec = httptest.NewRecorder()
	req = setPathValue(reqWithUser(http.MethodPost, "/up", "", "alice"), "id", "h1")
	s.handleUpgradeHive(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("kubectl-fail upgrade with no target status = %d, want 502", rec.Code)
	}
}

func TestHandleSwitchBranchValidation(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHandlerHub()
	mkUser(t, "alice")

	// Not found.
	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser(http.MethodPost, "/sw", `{}`, "alice"), "id", "missing")
	s.handleSwitchBranch(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("missing hive status = %d, want 404", rec.Code)
	}

	// Owner, empty branch -> 400.
	saveSaaSHive(&SaaSHive{ID: "h1", Owner: "alice", ClusterID: "hive-oke"})
	rec = httptest.NewRecorder()
	req = setPathValue(reqWithUser(http.MethodPost, "/sw", `{"branch":""}`, "alice"), "id", "h1")
	s.handleSwitchBranch(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty branch status = %d, want 400", rec.Code)
	}

	// Owner, a tracked branch that exists -> kubectl fails, handler falls back
	// to the heartbeat delivery path and returns 200 ("switching via heartbeat").
	// Seed the SHA cache so trackedBranchList()/getLatestSHAForBranch validate
	// the branch without a network fetch.
	resetSHACaches(t)
	latestSHAMu.Lock()
	latestSHAByBranch["v2"] = branchSHAInfo{SHA: "abc1234"}
	latestSHAMu.Unlock()
	// Point the hive's registry entry at the tracked branch so it's in the list.
	s.registry.Hives = []RegistryEntry{{ID: "h1", GitBranch: "v2"}}
	rec = httptest.NewRecorder()
	req = setPathValue(reqWithUser(http.MethodPost, "/sw", `{"branch":"v2"}`, "alice"), "id", "h1")
	s.handleSwitchBranch(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "heartbeat") {
		t.Errorf("switch fallback status = %d body=%s, want 200 heartbeat", rec.Code, rec.Body.String())
	}
}

func TestBranchToTag(t *testing.T) {
	if got := branchToTag("feat/x"); got != "feat-x" {
		t.Errorf("branchToTag(feat/x) = %q, want feat-x", got)
	}
	if got := branchToTag("v2"); got != "v2" {
		t.Errorf("branchToTag(v2) = %q, want v2", got)
	}
}
