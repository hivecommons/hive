package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func ownerPUT(t *testing.T, path, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest("PUT", path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hive-Role", "owner")
	req.Header.Set(ownerRoleVerifiedHeader, "true")
	return req
}

func TestHandleGovernorConfigGet_ReporterTrustDefaults(t *testing.T) {
	srv := newFullServer(t)
	result := decodeGovernorConfigGet(t, srv)

	rt, ok := result["reporterTrust"].(map[string]any)
	if !ok {
		t.Fatalf("reporterTrust section missing or wrong type: %v", result["reporterTrust"])
	}
	if rt["enabled"] != false {
		t.Errorf("enabled = %v, want false by default", rt["enabled"])
	}
	if got := toStringSlice(t, rt["trustedAssociations"]); !equalStringSlices(got, []string{"OWNER", "MEMBER", "COLLABORATOR"}) {
		t.Errorf("trustedAssociations = %v, want the effective default", got)
	}
	if got := toStringSlice(t, rt["untrustedRequireLabels"]); !equalStringSlices(got, []string{"triage/accepted"}) {
		t.Errorf("untrustedRequireLabels = %v, want the effective default", got)
	}
	if got := toStringSlice(t, rt["knownAssociations"]); len(got) != 7 {
		t.Errorf("knownAssociations = %v, want all seven GitHub values for the checkboxes", got)
	}
	if result["reporterTrustHold"] != false || result["reporterTrustHoldEnvLocked"] != false {
		t.Errorf("hold = %v envLocked = %v, want both false with the gate off", result["reporterTrustHold"], result["reporterTrustHoldEnvLocked"])
	}
	if _, present := result["repoReporterTrustHold"]; !present {
		t.Error("repoReporterTrustHold missing; the Repos tab renders per-repo rows from it")
	}
}

func TestHandleGovernorLabels_ReporterTrustRoundTrip(t *testing.T) {
	srv := newFullServer(t)
	body := `{"reporter_trust":{"enabled":true,"trusted_associations":["owner","MEMBER"],"trusted_logins":[" ext-maint ",""],"untrusted_require_labels":["ok-to-work"]}}`
	w := httptest.NewRecorder()
	srv.handleGovernorLabels(w, ownerPUT(t, "/api/config/governor/labels", body))
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	rt := srv.deps.Config.Project.IssueFilter.ReporterTrust
	if !rt.IsEnabled() {
		t.Error("enabled was not applied")
	}
	if !equalStringSlices(rt.TrustedAssociations, []string{"OWNER", "MEMBER"}) {
		t.Errorf("trusted_associations = %v, want upper-cased [OWNER MEMBER]", rt.TrustedAssociations)
	}
	if !equalStringSlices(rt.TrustedLogins, []string{"ext-maint"}) {
		t.Errorf("trusted_logins = %v, want trimmed, blanks dropped", rt.TrustedLogins)
	}
	if !equalStringSlices(rt.UntrustedRequireLabels, []string{"ok-to-work"}) {
		t.Errorf("untrusted_require_labels = %v", rt.UntrustedRequireLabels)
	}

	// A follow-up save that only flips enabled must leave the lists alone
	// (absent field = unchanged), and the GET must reflect the live value.
	w = httptest.NewRecorder()
	srv.handleGovernorLabels(w, ownerPUT(t, "/api/config/governor/labels", `{"reporter_trust":{"enabled":false}}`))
	if w.Code != http.StatusOK {
		t.Fatalf("second save code = %d, body = %s", w.Code, w.Body.String())
	}
	rt = srv.deps.Config.Project.IssueFilter.ReporterTrust
	if rt.IsEnabled() || !equalStringSlices(rt.TrustedLogins, []string{"ext-maint"}) {
		t.Errorf("partial save clobbered state: %+v", rt)
	}
	result := decodeGovernorConfigGet(t, srv)
	got := result["reporterTrust"].(map[string]any)
	if got["enabled"] != false || !equalStringSlices(toStringSlice(t, got["trustedLogins"]), []string{"ext-maint"}) {
		t.Errorf("GET does not reflect the saved block: %v", got)
	}
}

func TestHandleGovernorLabels_ReporterTrustRejectsUnknownAssociation(t *testing.T) {
	srv := newFullServer(t)
	w := httptest.NewRecorder()
	srv.handleGovernorLabels(w, ownerPUT(t, "/api/config/governor/labels", `{"reporter_trust":{"trusted_associations":["MAINTAINER"]}}`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400 for an association GitHub never reports; body = %s", w.Code, w.Body.String())
	}
	if srv.deps.Config.Project.IssueFilter.ReporterTrust.TrustedAssociations != nil {
		t.Error("a rejected save must not mutate config")
	}
}

func TestHandleGovernorRepos_ReporterTrustHoldOverrides(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Project.Org = "testorg"
	srv.deps.Config.Project.Repos = []string{"alpha", "beta"}
	srv.deps.Config.Project.PrimaryRepo = "alpha"

	w := httptest.NewRecorder()
	srv.handleGovernorRepos(w, ownerPUT(t, "/api/config/governor/repos", `{"reporterTrustHold":true,"repoReporterTrustHold":{"beta":false}}`))
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	cfg := srv.deps.Config
	if cfg.GitHub.ReporterTrustHold == nil || !*cfg.GitHub.ReporterTrustHold {
		t.Fatal("hive-wide reporterTrustHold was not applied")
	}
	if !cfg.ReporterTrustHoldEnabledForRepo("alpha") {
		t.Error("alpha should inherit the hive-wide true")
	}
	if cfg.ReporterTrustHoldEnabledForRepo("beta") {
		t.Error("beta's override to false was not applied")
	}

	result := decodeGovernorConfigGet(t, srv)
	if result["reporterTrustHold"] != true {
		t.Errorf("GET reporterTrustHold = %v, want true", result["reporterTrustHold"])
	}
	repoMap := result["repoReporterTrustHold"].(map[string]any)
	beta := repoMap["testorg/beta"].(map[string]any)
	if beta["value"] != false || beta["effective"] != false {
		t.Errorf("beta entry = %v, want value=false effective=false", beta)
	}
	alpha := repoMap["testorg/alpha"].(map[string]any)
	if _, hasValue := alpha["value"]; hasValue || alpha["effective"] != true {
		t.Errorf("alpha entry = %v, want no value (inherits) and effective=true", alpha)
	}

	// Clearing the override (null) restores inheritance.
	w = httptest.NewRecorder()
	srv.handleGovernorRepos(w, ownerPUT(t, "/api/config/governor/repos", `{"repoReporterTrustHold":{"beta":null}}`))
	if w.Code != http.StatusOK {
		t.Fatalf("clear code = %d, body = %s", w.Code, w.Body.String())
	}
	if !cfg.ReporterTrustHoldEnabledForRepo("beta") {
		t.Error("after clearing, beta must inherit the hive-wide true")
	}
}

func TestHandleGovernorLabels_HardSuppressLabelsRoundTrip(t *testing.T) {
	srv := newFullServer(t)
	body := `{"hard_suppress_labels":{"needs_direction":["direction-needed",""],"needs_decision":["decision-needed"],"needs_spec":["spec-needed"]}}`
	w := httptest.NewRecorder()
	srv.handleGovernorLabels(w, ownerPUT(t, "/api/config/governor/labels", body))
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	hs := srv.deps.Config.Project.IssueFilter.HardSuppressLabels
	if !equalStringSlices(hs.NeedsDirection, []string{"direction-needed"}) || !equalStringSlices(hs.NeedsDecision, []string{"decision-needed"}) || !equalStringSlices(hs.NeedsSpec, []string{"spec-needed"}) {
		t.Fatalf("hard suppress labels not trimmed/saved: %+v", hs)
	}
	result := decodeGovernorConfigGet(t, srv)
	got := result["hardSuppressLabels"].(map[string]any)
	if !equalStringSlices(toStringSlice(t, got["needsDirection"]), []string{"direction-needed"}) || !equalStringSlices(toStringSlice(t, got["needsHuman"]), []string{"needs-human"}) {
		t.Fatalf("GET hardSuppressLabels = %v", got)
	}
}
