package hub

import (
	"strings"
	"testing"
)

// TestSpokeIdentityFromPayload covers both the nil-payload guard and the
// populated mapping of the pure heartbeat→identity translation.
func TestSpokeIdentityFromPayload(t *testing.T) {
	if got := SpokeIdentityFromPayload(nil); got != (IdentitySet{}) {
		t.Errorf("nil payload = %+v, want zero IdentitySet", got)
	}
	p := &HeartbeatPayload{
		GitHubAppID:          5686,
		GitHubAppSlug:        "kubestellar-hive-ghe",
		GitHubInstallationID: 43015,
		GitHubAPIURL:         "https://github.ibm.com/api/v3",
		GitHubBaseURL:        "https://github.ibm.com",
	}
	got := SpokeIdentityFromPayload(p)
	if got.AppID != 5686 || got.AppSlug != "kubestellar-hive-ghe" || got.InstallationID != 43015 ||
		got.APIURL != "https://github.ibm.com/api/v3" || got.BaseURL != "https://github.ibm.com" {
		t.Errorf("mapped identity = %+v, want the payload's GitHub fields verbatim", got)
	}
}

// TestIdentitySetIssuesAndValidate covers the coherence checks: a token-only
// hive is silent, a coherent GHE set is clean, and each inconsistency (GHE slug
// on a public api_url; base/api forge mismatch both directions) is named.
// ValidateIdentityPush wraps the same checks into a refusal string.
func TestIdentitySetIssuesAndValidate(t *testing.T) {
	// No App configured → no issues, and a safe (empty) push verdict.
	if got := IdentitySetIssues(IdentitySet{}); got != nil {
		t.Errorf("token-only hive should have no identity issues, got %v", got)
	}
	if got := ValidateIdentityPush(IdentitySet{}); got != "" {
		t.Errorf("token-only push should be safe, got %q", got)
	}

	// A coherent GHE identity is clean.
	ghe := IdentitySet{AppID: 5686, AppSlug: "kubestellar-hive-ghe", APIURL: "https://github.ibm.com/api/v3", BaseURL: "https://github.ibm.com"}
	if !ghe.IsGHE() {
		t.Error("GHE api_url must read as GHE")
	}
	if got := IdentitySetIssues(ghe); got != nil {
		t.Errorf("coherent GHE identity should be clean, got %v", got)
	}

	// GHE slug presented to public api.github.com (empty api_url) — the live
	// regression. ValidateIdentityPush must refuse.
	badSlug := IdentitySet{AppID: 5686, AppSlug: "kubestellar-hive-ghe", APIURL: ""}
	issues := IdentitySetIssues(badSlug)
	if len(issues) == 0 || !strings.Contains(issues[0], "404") {
		t.Errorf("GHE slug on empty api_url must be flagged (404), got %v", issues)
	}
	if v := ValidateIdentityPush(badSlug); !strings.Contains(v, "refusing") {
		t.Errorf("inconsistent identity must be refused, got %q", v)
	}

	// base_url is GHE but api_url is public — a stale-base mismatch.
	baseGHE := IdentitySet{AppID: 3568013, APIURL: "https://api.github.com", BaseURL: "https://github.ibm.com"}
	if got := IdentitySetIssues(baseGHE); len(got) == 0 {
		t.Error("base GHE / api public mismatch must be flagged")
	}
	// api_url is GHE but base_url is public — the reverse mismatch.
	apiGHE := IdentitySet{AppID: 5686, APIURL: "https://github.ibm.com/api/v3", BaseURL: "https://github.com"}
	if got := IdentitySetIssues(apiGHE); len(got) == 0 {
		t.Error("api GHE / base public mismatch must be flagged")
	}
}
