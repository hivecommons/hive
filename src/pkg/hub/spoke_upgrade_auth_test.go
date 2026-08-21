package hub

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSpokeUpgradeProofAllowsOwnerWithoutHubCookie(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	const (
		hiveID = "hosted-spoke-upgrade"
		user   = "alice"
		token  = "spoke-dashboard-token"
		branch = "v4"
	)

	s := newHandlerHub()
	s.hubGitBranch = branch
	s.registry.Hives = []RegistryEntry{{
		ID:        hiveID,
		Owner:     user,
		ClusterID: "hive-oke",
		GitBranch: branch,
	}}
	s.spokeProxyAuthCache = map[string]spokeProxyAuthEntry{
		hiveID: {token: token, expires: time.Now().Add(time.Hour)},
	}
	if err := saveSaaSHive(&SaaSHive{
		ID:          hiveID,
		Owner:       user,
		ClusterID:   "hive-oke",
		Org:         "org",
		PrimaryRepo: "repo",
	}); err != nil {
		t.Fatalf("save hive: %v", err)
	}
	if err := saveSaaSUser(&SaaSUser{
		GitHubUsername: user,
		Hives:          map[string]string{hiveID: "owner"},
	}); err != nil {
		t.Fatalf("save user: %v", err)
	}

	latestSHAMu.Lock()
	prev, hadPrev := latestSHAByBranch[branch]
	latestSHAByBranch[branch] = branchSHAInfo{SHA: "012e54f8"}
	latestSHAMu.Unlock()
	defer func() {
		latestSHAMu.Lock()
		if hadPrev {
			latestSHAByBranch[branch] = prev
		} else {
			delete(latestSHAByBranch, branch)
		}
		latestSHAMu.Unlock()
	}()

	req := setPathValue(httptest.NewRequest(http.MethodPost, "/api/saas/hives/"+hiveID+"/upgrade", nil), "id", hiveID)
	req.Header.Set("Origin", "https://hive.kubestellar.io")
	req.Header.Set("X-Hive-User", user)
	req.Header.Set("X-Hive-Role", "owner")
	req.Header.Set(proxyAuthHeader, token)

	rec := httptest.NewRecorder()
	s.requireAuthOrSpokeUpgrade(s.handleUpgradeHive)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if got := s.heartbeatUpgrade[hiveID]; got != "012e54f8" {
		t.Fatalf("heartbeatUpgrade[%q] = %q, want target SHA", hiveID, got)
	}
}

func TestSpokeUpgradeProofRejectsNonOwner(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	const (
		hiveID = "hosted-spoke-upgrade-deny"
		user   = "reader"
		token  = "spoke-dashboard-token"
	)

	s := newHandlerHub()
	s.registry.Hives = []RegistryEntry{{ID: hiveID, Owner: "alice", ClusterID: "hive-oke"}}
	s.spokeProxyAuthCache = map[string]spokeProxyAuthEntry{
		hiveID: {token: token, expires: time.Now().Add(time.Hour)},
	}
	if err := saveSaaSHive(&SaaSHive{ID: hiveID, Owner: "alice", ClusterID: "hive-oke"}); err != nil {
		t.Fatalf("save hive: %v", err)
	}
	if err := saveSaaSUser(&SaaSUser{GitHubUsername: user, Hives: map[string]string{hiveID: "read"}}); err != nil {
		t.Fatalf("save user: %v", err)
	}

	req := setPathValue(httptest.NewRequest(http.MethodPost, "/api/saas/hives/"+hiveID+"/upgrade", nil), "id", hiveID)
	req.Header.Set("Origin", "https://hive.kubestellar.io")
	req.Header.Set("X-Hive-User", user)
	req.Header.Set("X-Hive-Role", "owner")
	req.Header.Set(proxyAuthHeader, token)

	rec := httptest.NewRecorder()
	s.requireAuthOrSpokeUpgrade(s.handleUpgradeHive)(rec, req)

	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "only the owner can upgrade") {
		t.Fatalf("status/body = %d %q, want owner denial", rec.Code, rec.Body.String())
	}
}

func TestSpokeUpgradeMiddlewareStillAcceptsHubSession(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	const (
		hiveID = "hosted-hub-session-upgrade"
		user   = "alice"
		branch = "v4"
	)

	s := newHandlerHub()
	s.hubGitBranch = branch
	s.registry.Hives = []RegistryEntry{{ID: hiveID, Owner: user, ClusterID: "hive-oke", GitBranch: branch}}
	if err := saveSaaSHive(&SaaSHive{ID: hiveID, Owner: user, ClusterID: "hive-oke"}); err != nil {
		t.Fatalf("save hive: %v", err)
	}
	if err := saveSaaSUser(&SaaSUser{GitHubUsername: user, Hives: map[string]string{}}); err != nil {
		t.Fatalf("save user: %v", err)
	}

	latestSHAMu.Lock()
	prev, hadPrev := latestSHAByBranch[branch]
	latestSHAByBranch[branch] = branchSHAInfo{SHA: "feed1234"}
	latestSHAMu.Unlock()
	defer func() {
		latestSHAMu.Lock()
		if hadPrev {
			latestSHAByBranch[branch] = prev
		} else {
			delete(latestSHAByBranch, branch)
		}
		latestSHAMu.Unlock()
	}()

	req := setPathValue(reqWithUser(http.MethodPost, "/api/saas/hives/"+hiveID+"/upgrade", "", user), "id", hiveID)
	req.Header.Set("Origin", "https://hive.kubestellar.io")
	rec := httptest.NewRecorder()
	s.requireAuthOrSpokeUpgrade(s.handleUpgradeHive)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestSpokeUpgradeMiddlewareRejectsMissingAndInvalidProof(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	const (
		hiveID = "hosted-spoke-upgrade-bad-proof"
		user   = "alice"
		token  = "spoke-dashboard-token"
	)

	s := newHandlerHub()
	s.registry.Hives = []RegistryEntry{{ID: hiveID, Owner: user, ClusterID: "hive-oke"}}
	s.spokeProxyAuthCache = map[string]spokeProxyAuthEntry{
		hiveID: {token: token, expires: time.Now().Add(time.Hour)},
	}
	if err := saveSaaSUser(&SaaSUser{GitHubUsername: user, Hives: map[string]string{hiveID: "owner"}}); err != nil {
		t.Fatalf("save user: %v", err)
	}

	for _, proof := range []string{"", "wrong"} {
		req := setPathValue(httptest.NewRequest(http.MethodPost, "/api/saas/hives/"+hiveID+"/upgrade", nil), "id", hiveID)
		req.Header.Set("Origin", "https://hive.kubestellar.io")
		req.Header.Set("X-Hive-User", user)
		req.Header.Set("X-Hive-Role", "owner")
		req.Header.Set(proxyAuthHeader, proof)

		rec := httptest.NewRecorder()
		s.requireAuthOrSpokeUpgrade(s.handleUpgradeHive)(rec, req)

		if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "not authenticated") {
			t.Fatalf("proof %q: status/body = %d %q, want unauthenticated", proof, rec.Code, rec.Body.String())
		}
	}
}

func TestSpokeUpgradeMiddlewareRejectsBlockedSpokeUser(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	const (
		hiveID = "hosted-spoke-upgrade-blocked"
		user   = "alice"
		token  = "spoke-dashboard-token"
	)

	s := newHandlerHub()
	s.registry.Hives = []RegistryEntry{{ID: hiveID, Owner: user, ClusterID: "hive-oke"}}
	s.spokeProxyAuthCache = map[string]spokeProxyAuthEntry{
		hiveID: {token: token, expires: time.Now().Add(time.Hour)},
	}
	if err := saveSaaSUser(&SaaSUser{GitHubUsername: user, Hives: map[string]string{hiveID: "owner"}, Blocked: true}); err != nil {
		t.Fatalf("save user: %v", err)
	}

	req := setPathValue(httptest.NewRequest(http.MethodPost, "/api/saas/hives/"+hiveID+"/upgrade", nil), "id", hiveID)
	req.Header.Set("Origin", "https://hive.kubestellar.io")
	req.Header.Set("X-Hive-User", user)
	req.Header.Set("X-Hive-Role", "owner")
	req.Header.Set(proxyAuthHeader, token)

	rec := httptest.NewRecorder()
	s.requireAuthOrSpokeUpgrade(s.handleUpgradeHive)(rec, req)

	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "not authenticated") {
		t.Fatalf("status/body = %d %q, want unauthenticated", rec.Code, rec.Body.String())
	}
}

func TestSpokeUpgradeMiddlewareKeepsCSRFGate(t *testing.T) {
	s := newHandlerHub()
	req := httptest.NewRequest(http.MethodPost, "/api/saas/hives/h1/upgrade", nil)
	req.Header.Set("X-Hive-User", "alice")
	req.Header.Set("X-Hive-Role", "owner")
	req.Header.Set(proxyAuthHeader, "token")

	rec := httptest.NewRecorder()
	s.requireAuthOrSpokeUpgrade(s.handleUpgradeHive)(rec, req)

	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "CSRF") {
		t.Fatalf("status/body = %d %q, want CSRF denial", rec.Code, rec.Body.String())
	}
}
