package hub

// Regression tests for #4541 follow-up: on pull-only clusters (e.g. fmaas) the
// hub cannot kubectl into the spoke's cluster, so verifying a spoke-relayed
// upgrade proof by live-reading hive-secrets/dashboard-token was impossible by
// design and every owner click died with "the hub could not read this hive's
// dashboard-token secret". Verification now checks the hub's OWN stored record
// first (SaaSHive.DashboardTokenHash — written at provisioning, refreshed from
// the authenticated heartbeat and from successful live reads), with the live
// secret read demoted to fallback.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// seedUpgradeTarget pins a build target for the branch so handleUpgradeHive can
// arm the heartbeat, restoring the previous global state afterwards.
func seedUpgradeTarget(t *testing.T, branch, sha string) {
	t.Helper()
	latestSHAMu.Lock()
	prev, hadPrev := latestSHAByBranch[branch]
	latestSHAByBranch[branch] = branchSHAInfo{SHA: sha}
	latestSHAMu.Unlock()
	t.Cleanup(func() {
		latestSHAMu.Lock()
		if hadPrev {
			latestSHAByBranch[branch] = prev
		} else {
			delete(latestSHAByBranch, branch)
		}
		latestSHAMu.Unlock()
	})
}

func spokeUpgradeRequest(hiveID, user, proof string) *http.Request {
	req := setPathValue(httptest.NewRequest(http.MethodPost, "/api/saas/hives/"+hiveID+"/upgrade", nil), "id", hiveID)
	req.Header.Set("Origin", "https://hive.kubestellar.io")
	if user != "" {
		req.Header.Set("X-Hive-User", user)
	}
	req.Header.Set("X-Hive-Role", "owner")
	if proof != "" {
		req.Header.Set(proxyAuthHeader, proof)
	}
	return req
}

// TestSpokeUpgradeProofStoredRecordNoClusterAccess is the pull-only regression:
// the hive's cluster is NOT in s.clusters and nothing is cached, so a live
// secret read is impossible — exactly the fmaas topology. The stored record
// alone must verify the proof.
func TestSpokeUpgradeProofStoredRecordNoClusterAccess(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	const (
		hiveID = "hosted-pullonly-stored-record"
		owner  = "alice"
		token  = "spoke-dashboard-token"
		branch = "v4"
	)

	s := newHandlerHub()
	s.hubGitBranch = branch
	// ClusterID the hub has no config (and so no kubeconfig) for: every
	// kubectl lane is structurally unavailable.
	s.registry.Hives = []RegistryEntry{{ID: hiveID, Owner: owner, ClusterID: "fmaas-pull-only", GitBranch: branch, LastHeartbeat: time.Now().UTC().Format(time.RFC3339)}}
	if err := saveSaaSHive(&SaaSHive{
		ID: hiveID, Owner: owner, ClusterID: "fmaas-pull-only",
		Org: "org", PrimaryRepo: "repo",
		DashboardTokenHash: HashDashboardToken(token),
	}); err != nil {
		t.Fatalf("save hive: %v", err)
	}
	if err := saveSaaSUser(&SaaSUser{GitHubUsername: owner, Hives: map[string]string{hiveID: "owner"}}); err != nil {
		t.Fatalf("save user: %v", err)
	}
	seedUpgradeTarget(t, branch, "0123abcd")

	rec := httptest.NewRecorder()
	s.requireAuthOrSpokeUpgrade(s.handleUpgradeHive)(rec, spokeUpgradeRequest(hiveID, owner, token))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if got := s.heartbeatUpgrade[hiveID]; got != "0123abcd" {
		t.Fatalf("heartbeatUpgrade[%q] = %q, want target SHA", hiveID, got)
	}
}

// TestSpokeUpgradeProofClusterReadFallbackBackfillsRecord: a hive predating the
// stored record on a REACHABLE cluster still verifies via the live secret read
// — and that success must backfill the stored record, so the next verification
// no longer depends on cluster access.
func TestSpokeUpgradeProofClusterReadFallbackBackfillsRecord(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	const (
		hiveID = "hosted-fallback-backfill"
		owner  = "alice"
		token  = "spoke-dashboard-token"
		branch = "v4"
	)

	s := newHandlerHub()
	s.hubGitBranch = branch
	s.registry.Hives = []RegistryEntry{{ID: hiveID, Owner: owner, ClusterID: "hive-oke", GitBranch: branch, LastHeartbeat: time.Now().UTC().Format(time.RFC3339)}}
	s.spokeProxyAuthCache = map[string]spokeProxyAuthEntry{
		hiveID: {token: token, expires: time.Now().Add(time.Hour)},
	}
	if err := saveSaaSHive(&SaaSHive{ID: hiveID, Owner: owner, ClusterID: "hive-oke", Org: "org", PrimaryRepo: "repo"}); err != nil {
		t.Fatalf("save hive: %v", err)
	}
	if err := saveSaaSUser(&SaaSUser{GitHubUsername: owner, Hives: map[string]string{hiveID: "owner"}}); err != nil {
		t.Fatalf("save user: %v", err)
	}
	seedUpgradeTarget(t, branch, "4567beef")

	rec := httptest.NewRecorder()
	s.requireAuthOrSpokeUpgrade(s.handleUpgradeHive)(rec, spokeUpgradeRequest(hiveID, owner, token))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	h := loadSaaSHive(hiveID)
	if h == nil || h.DashboardTokenHash != HashDashboardToken(token) {
		t.Fatalf("stored record not backfilled after successful live read: %+v", h)
	}
}

// TestSpokeUpgradeProofBothLanesUnavailableIsHonest: no stored record AND no
// readable secret — the only remaining unverifiable case — must say both
// halves and point at the heartbeat-report way out.
func TestSpokeUpgradeProofBothLanesUnavailableIsHonest(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	const (
		hiveID = "hosted-both-lanes-dark"
		owner  = "alice"
		token  = "spoke-dashboard-token"
	)

	s := newHandlerHub()
	s.registry.Hives = []RegistryEntry{{ID: hiveID, Owner: owner, ClusterID: "fmaas-pull-only", LastHeartbeat: time.Now().UTC().Format(time.RFC3339)}}
	if err := saveSaaSHive(&SaaSHive{ID: hiveID, Owner: owner, ClusterID: "fmaas-pull-only"}); err != nil {
		t.Fatalf("save hive: %v", err)
	}
	if err := saveSaaSUser(&SaaSUser{GitHubUsername: owner, Hives: map[string]string{hiveID: "owner"}}); err != nil {
		t.Fatalf("save user: %v", err)
	}

	rec := httptest.NewRecorder()
	s.requireAuthOrSpokeUpgrade(s.handleUpgradeHive)(rec, spokeUpgradeRequest(hiveID, owner, token))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body=%s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"no stored dashboard-token record", "could not read", "heartbeat"} {
		if !strings.Contains(body, want) {
			t.Errorf("body %q must contain %q", body, want)
		}
	}
}

// TestSpokeUpgradeProofMismatchAgainstStoredRecord: a stored record makes the
// proof VERIFIABLE, so a wrong token is a mismatch ("does not match"), never
// the unverifiable message.
func TestSpokeUpgradeProofMismatchAgainstStoredRecord(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	const (
		hiveID = "hosted-stored-record-mismatch"
		owner  = "alice"
	)

	s := newHandlerHub()
	s.registry.Hives = []RegistryEntry{{ID: hiveID, Owner: owner, ClusterID: "fmaas-pull-only", LastHeartbeat: time.Now().UTC().Format(time.RFC3339)}}
	if err := saveSaaSHive(&SaaSHive{
		ID: hiveID, Owner: owner, ClusterID: "fmaas-pull-only",
		DashboardTokenHash: HashDashboardToken("the-real-token"),
	}); err != nil {
		t.Fatalf("save hive: %v", err)
	}
	if err := saveSaaSUser(&SaaSUser{GitHubUsername: owner, Hives: map[string]string{hiveID: "owner"}}); err != nil {
		t.Fatalf("save user: %v", err)
	}

	rec := httptest.NewRecorder()
	s.requireAuthOrSpokeUpgrade(s.handleUpgradeHive)(rec, spokeUpgradeRequest(hiveID, owner, "wrong-token"))

	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "does not match") {
		t.Fatalf("status/body = %d %q, want 401 naming the mismatch", rec.Code, rec.Body.String())
	}
}

// TestHeartbeatAdoptsDashboardTokenHash: an authenticated beat carrying
// dashboard_token_hash must land in the hive's stored record — this is how a
// pull-only hive the hub never provisioned a record for becomes verifiable.
// A malformed value, and a beat that reports nothing, must change nothing.
func TestHeartbeatAdoptsDashboardTokenHash(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	const hiveID = "hosted-heartbeat-token-hash"
	s := newHeartbeatHub()
	if err := saveSaaSHive(&SaaSHive{ID: hiveID, Owner: "alice", ClusterID: "fmaas-pull-only"}); err != nil {
		t.Fatalf("save hive: %v", err)
	}

	hash := HashDashboardToken("spoke-dashboard-token")
	rec := postHeartbeat(t, s, fmt.Sprintf(`{"hive_id":%q,"dashboard_token_hash":%q}`, hiveID, hash))
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat status = %d (body=%s)", rec.Code, rec.Body.String())
	}
	if h := loadSaaSHive(hiveID); h == nil || h.DashboardTokenHash != hash {
		t.Fatalf("stored record after beat = %+v, want hash %s", h, hash)
	}

	// Malformed hash: rejected, record unchanged.
	rec = postHeartbeat(t, s, fmt.Sprintf(`{"hive_id":%q,"dashboard_token_hash":"not-a-digest"}`, hiveID))
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat status = %d (body=%s)", rec.Code, rec.Body.String())
	}
	if h := loadSaaSHive(hiveID); h == nil || h.DashboardTokenHash != hash {
		t.Fatalf("malformed hash must not disturb the record; got %+v", h)
	}

	// Old spoke (no field): record kept.
	rec = postHeartbeat(t, s, fmt.Sprintf(`{"hive_id":%q}`, hiveID))
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat status = %d (body=%s)", rec.Code, rec.Body.String())
	}
	if h := loadSaaSHive(hiveID); h == nil || h.DashboardTokenHash != hash {
		t.Fatalf("absent hash must not disturb the record; got %+v", h)
	}
}

// TestHeartbeatAdoptedHashVerifiesUpgradeProof closes the loop end-to-end for
// the stuck fmaas hive: beat reports the hash, then the spoke's relayed
// upgrade request verifies against it with zero cluster access.
func TestHeartbeatAdoptedHashVerifiesUpgradeProof(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	const (
		hiveID = "hosted-beat-then-upgrade"
		owner  = "alice"
		token  = "spoke-dashboard-token"
		branch = "v4"
	)

	hb := newHeartbeatHub()
	hb.registry.Hives = []RegistryEntry{{ID: hiveID, Owner: owner, ClusterID: "fmaas-pull-only", GitBranch: branch, LastHeartbeat: time.Now().UTC().Format(time.RFC3339)}}
	if err := saveSaaSHive(&SaaSHive{ID: hiveID, Owner: owner, ClusterID: "fmaas-pull-only", Org: "org", PrimaryRepo: "repo"}); err != nil {
		t.Fatalf("save hive: %v", err)
	}
	if err := saveSaaSUser(&SaaSUser{GitHubUsername: owner, Hives: map[string]string{hiveID: "owner"}}); err != nil {
		t.Fatalf("save user: %v", err)
	}
	rec := postHeartbeat(t, hb, fmt.Sprintf(`{"hive_id":%q,"dashboard_token_hash":%q}`, hiveID, HashDashboardToken(token)))
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat status = %d (body=%s)", rec.Code, rec.Body.String())
	}

	s := newHandlerHub()
	s.hubGitBranch = branch
	s.registry.Hives = []RegistryEntry{{ID: hiveID, Owner: owner, ClusterID: "fmaas-pull-only", GitBranch: branch, LastHeartbeat: time.Now().UTC().Format(time.RFC3339)}}
	seedUpgradeTarget(t, branch, "89abcdef")

	rec = httptest.NewRecorder()
	s.requireAuthOrSpokeUpgrade(s.handleUpgradeHive)(rec, spokeUpgradeRequest(hiveID, owner, token))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if got := s.heartbeatUpgrade[hiveID]; got != "89abcdef" {
		t.Fatalf("heartbeatUpgrade[%q] = %q, want target SHA", hiveID, got)
	}
}
