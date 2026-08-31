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
		// Heartbeating: the manual upgrade path refuses a hive that cannot
		// COLLECT the instruction (pull-only delivery, see pullonly_upgrade.go).
		LastHeartbeat: time.Now().UTC().Format(time.RFC3339),
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
	s.registry.Hives = []RegistryEntry{{ID: hiveID, Owner: user, ClusterID: "hive-oke", GitBranch: branch,
		LastHeartbeat: time.Now().UTC().Format(time.RFC3339)}}
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

// TestSpokeUpgradeProofWithoutUserIdentityAttributesToOwner is the regression
// test for "Upgrade failed: not authenticated" on gateway-fronted hosted
// spokes: the Node auth proxy authenticates the operator with the shared
// dashboard token and strips per-user identity headers, so the spoke's relayed
// upgrade request arrives with a VALID proof but NO X-Hive-User. The proof is
// the hive's own per-hive secret — owner-equivalent on the spoke by definition
// — so the hub must accept the request and attribute it to the hive's
// registered owner instead of rejecting the logged-in owner as unauthenticated.
func TestSpokeUpgradeProofWithoutUserIdentityAttributesToOwner(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	const (
		hiveID = "hosted-spoke-upgrade-no-identity"
		owner  = "clubanderson"
		token  = "spoke-dashboard-token"
		branch = "v4"
	)

	s := newHandlerHub()
	s.hubGitBranch = branch
	s.registry.Hives = []RegistryEntry{{ID: hiveID, Owner: owner, ClusterID: "hive-oke", GitBranch: branch,
		LastHeartbeat: time.Now().UTC().Format(time.RFC3339)}}
	s.spokeProxyAuthCache = map[string]spokeProxyAuthEntry{
		hiveID: {token: token, expires: time.Now().Add(time.Hour)},
	}
	if err := saveSaaSHive(&SaaSHive{ID: hiveID, Owner: owner, ClusterID: "hive-oke", Org: "org", PrimaryRepo: "repo"}); err != nil {
		t.Fatalf("save hive: %v", err)
	}
	if err := saveSaaSUser(&SaaSUser{GitHubUsername: owner, Hives: map[string]string{hiveID: "owner"}}); err != nil {
		t.Fatalf("save user: %v", err)
	}

	latestSHAMu.Lock()
	prev, hadPrev := latestSHAByBranch[branch]
	latestSHAByBranch[branch] = branchSHAInfo{SHA: "d4b21af6"}
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

	// Exactly what the spoke relays on the gateway topology: role + proof, NO user.
	req := setPathValue(httptest.NewRequest(http.MethodPost, "/api/saas/hives/"+hiveID+"/upgrade", nil), "id", hiveID)
	req.Header.Set("Origin", "https://hive.kubestellar.io")
	req.Header.Set("X-Hive-Role", "owner")
	req.Header.Set(proxyAuthHeader, token)

	rec := httptest.NewRecorder()
	s.requireAuthOrSpokeUpgrade(s.handleUpgradeHive)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if got := s.heartbeatUpgrade[hiveID]; got != "d4b21af6" {
		t.Fatalf("heartbeatUpgrade[%q] = %q, want target SHA", hiveID, got)
	}
}

// TestSpokeUpgradeHonestErrors pins the #4446 honest-error standard on every
// spoke-lane rejection: the spoke relays the hub's body verbatim into the
// operator's toast, so each failure must name WHICH credential failed and what
// to do — never a bare "not authenticated".
func TestSpokeUpgradeHonestErrors(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	const (
		hiveID = "hosted-spoke-upgrade-honest"
		owner  = "alice"
		token  = "spoke-dashboard-token"
	)

	s := newHandlerHub()
	s.registry.Hives = []RegistryEntry{{ID: hiveID, Owner: owner, ClusterID: "hive-oke"}}
	s.spokeProxyAuthCache = map[string]spokeProxyAuthEntry{
		hiveID: {token: token, expires: time.Now().Add(time.Hour)},
	}
	if err := saveSaaSUser(&SaaSUser{GitHubUsername: owner, Hives: map[string]string{hiveID: "owner"}}); err != nil {
		t.Fatalf("save user: %v", err)
	}

	cases := []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{
			// Pre-proof spoke builds relay the click with no credentials at all;
			// the error must point at the way out (upgrade from the hub side).
			name:    "credential-less relay from an old spoke build",
			headers: map[string]string{},
			want:    "too old to relay",
		},
		{
			name:    "proof missing",
			headers: map[string]string{"X-Hive-User": owner, "X-Hive-Role": "owner"},
			want:    "DASHBOARD_AUTH_TOKEN",
		},
		{
			name:    "proof mismatch",
			headers: map[string]string{"X-Hive-User": owner, "X-Hive-Role": "owner", proxyAuthHeader: "wrong"},
			want:    "does not match",
		},
		{
			name:    "wrong role",
			headers: map[string]string{"X-Hive-User": owner, "X-Hive-Role": "read", proxyAuthHeader: token},
			want:    "owner role",
		},
		{
			name:    "unknown user",
			headers: map[string]string{"X-Hive-User": "nobody-here", "X-Hive-Role": "owner", proxyAuthHeader: token},
			want:    "no record of user",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := setPathValue(httptest.NewRequest(http.MethodPost, "/api/saas/hives/"+hiveID+"/upgrade", nil), "id", hiveID)
			req.Header.Set("Origin", "https://hive.kubestellar.io")
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			s.requireAuthOrSpokeUpgrade(s.handleUpgradeHive)(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (body=%s)", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("body %q must contain %q", rec.Body.String(), tc.want)
			}
		})
	}

	// Proof unverifiable: the hive is not in the registry and nothing is cached,
	// so the hub cannot resolve its dashboard-token secret at all.
	req := setPathValue(httptest.NewRequest(http.MethodPost, "/api/saas/hives/unknown-hive/upgrade", nil), "id", "unknown-hive")
	req.Header.Set("Origin", "https://hive.kubestellar.io")
	req.Header.Set("X-Hive-User", owner)
	req.Header.Set("X-Hive-Role", "owner")
	req.Header.Set(proxyAuthHeader, token)
	rec := httptest.NewRecorder()
	s.requireAuthOrSpokeUpgrade(s.handleUpgradeHive)(rec, req)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "could not read") {
		t.Fatalf("unverifiable proof: status/body = %d %q, want 401 naming the unreadable secret", rec.Code, rec.Body.String())
	}
}
