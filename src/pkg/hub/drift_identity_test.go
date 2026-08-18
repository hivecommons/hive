package hub

import (
	"strings"
	"testing"
)

// hasSignal reports whether the report carries a signal of the given kind.
func hasSignal(r DriftReport, kind string) (DriftSignal, bool) {
	for _, s := range r.Signals {
		if s.Kind == kind {
			return s, true
		}
	}
	return DriftSignal{}, false
}

// TestDriftFlagsHalfAppliedIdentity is the live regression, seen from the hub.
//
// A GHE app_id/slug with an empty api_url authenticates as nothing and returns
// "404 Integration not found" on every token request. Until the spoke reported
// its whole identity back, this was invisible to the hub: with only app_id
// visible, a half-applied identity looked exactly like a healthy delivery, and
// the first symptom was a user reporting that their agents had stopped.
func TestDriftFlagsHalfAppliedIdentity(t *testing.T) {
	norm := computeFleetNorm(normFleet())
	latest := map[string]string{"v2": "abc1234"}

	h := healthyEntry("kalantar-msb")
	h.GitHubAppID = 5686
	h.GitHubAppSlug = "kubestellar-hive-ghe"
	h.GitHubInstallationID = 146551814
	h.GitHubAPIURL = "" // the field that was never pushed

	got := computeDrift(h, norm, latest, driftNow)

	sig, ok := hasSignal(got, DriftKindIdentitySplit)
	if !ok {
		t.Fatal("a GHE App with an empty api_url raised no identity-split signal — " +
			"this hive 404s on every token request and nothing would say so")
	}
	if sig.Severity != DriftCritical {
		t.Errorf("severity = %q, want critical: the hive cannot authenticate at all", sig.Severity)
	}
	if !strings.Contains(sig.Reason, "operator") {
		t.Errorf("reason %q should say only an operator can fix it — the owner cannot", sig.Reason)
	}
}

// TestDriftSilentOnHealthyPublicIdentity guards the other direction. Public
// GitHub legitimately runs with an EMPTY api_url, so firing here would raise a
// critical signal on most of the fleet and the signal would be ignored.
func TestDriftSilentOnHealthyPublicIdentity(t *testing.T) {
	norm := computeFleetNorm(normFleet())
	latest := map[string]string{"v2": "abc1234"}

	h := healthyEntry("public-hive")
	h.GitHubAppID = 3568013
	h.GitHubAppSlug = "kubestellar-hive"
	h.GitHubAPIURL = ""

	if sig, ok := hasSignal(computeDrift(h, norm, latest, driftNow), DriftKindIdentitySplit); ok {
		t.Errorf("a healthy public-GitHub hive was flagged: %s", sig.Reason)
	}
}

// TestDriftSilentOnHealthyGHEIdentity: the fully-delivered GHE case.
func TestDriftSilentOnHealthyGHEIdentity(t *testing.T) {
	norm := computeFleetNorm(normFleet())
	latest := map[string]string{"v2": "abc1234"}

	h := healthyEntry("ghe-hive")
	h.GitHubAppID = 5686
	h.GitHubAppSlug = "kubestellar-hive-ghe"
	h.GitHubAPIURL = "https://github.ibm.com/api/v3"
	h.GitHubBaseURL = "https://github.ibm.com"

	if sig, ok := hasSignal(computeDrift(h, norm, latest, driftNow), DriftKindIdentitySplit); ok {
		t.Errorf("a correctly-delivered GHE hive was flagged: %s", sig.Reason)
	}
}

// TestDriftSilentOnUnderReportingSpoke: a spoke too old to report app_slug or
// base_url must read as UNKNOWN. Inferring breakage from silence would flag
// every spoke that has not yet upgraded past this change.
func TestDriftSilentOnUnderReportingSpoke(t *testing.T) {
	norm := computeFleetNorm(normFleet())
	latest := map[string]string{"v2": "abc1234"}

	h := healthyEntry("old-spoke")
	h.GitHubAppID = 5686 // reports app_id only, as every spoke did before this change

	if sig, ok := hasSignal(computeDrift(h, norm, latest, driftNow), DriftKindIdentitySplit); ok {
		t.Errorf("a spoke too old to report the full identity was flagged: %s", sig.Reason)
	}
}
