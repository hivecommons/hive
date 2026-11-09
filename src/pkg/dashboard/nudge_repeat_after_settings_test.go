package dashboard

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// auto_merge.nudge_repeat_after (hivecommons/hive#17067) is edited from the
// Labels tab alongside the legacy renudge_interval it supersedes.
func TestHandleGovernorLabels_NudgeRepeatAfterRoundTrip(t *testing.T) {
	srv := newFullServer(t)
	w := httptest.NewRecorder()
	srv.handleGovernorLabels(w, ownerPUT(t, "/api/config/governor/labels", `{"nudge_repeat_after":" 48h "}`))
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	if got := srv.deps.Config.AutoMerge.NudgeRepeatAfter; got != "48h" {
		t.Fatalf("nudge_repeat_after = %q, want trimmed 48h", got)
	}
	if got := decodeGovernorConfigGet(t, srv)["nudgeRepeatAfter"]; got != "48h" {
		t.Fatalf("GET nudgeRepeatAfter = %v, want 48h", got)
	}

	w = httptest.NewRecorder()
	srv.handleGovernorLabels(w, ownerPUT(t, "/api/config/governor/labels", `{"nudge_repeat_after":"soon"}`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid duration: code = %d, want 400; body = %s", w.Code, w.Body.String())
	}
	if got := srv.deps.Config.AutoMerge.NudgeRepeatAfter; got != "48h" {
		t.Fatalf("rejected save mutated nudge_repeat_after to %q", got)
	}

	w = httptest.NewRecorder()
	srv.handleGovernorLabels(w, ownerPUT(t, "/api/config/governor/labels", `{"nudge_repeat_after":""}`))
	if w.Code != http.StatusOK {
		t.Fatalf("clear: code = %d, body = %s", w.Code, w.Body.String())
	}
	if got := srv.deps.Config.AutoMerge.NudgeRepeatAfter; got != "" {
		t.Fatalf("empty must clear the override, got %q", got)
	}
}

// governor.labels.stage.contributor_nudge.ci_failure_cooldown
// (hivecommons/hive#17328) sits beside the nudge intervals on the Labels tab.
func TestHandleGovernorLabels_CIFailureCooldownRoundTrip(t *testing.T) {
	srv := newFullServer(t)
	w := httptest.NewRecorder()
	srv.handleGovernorLabels(w, ownerPUT(t, "/api/config/governor/labels", `{"stage_nudge_ci_failure_cooldown":" 2h "}`))
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	if got := srv.deps.Config.Governor.Labels.Stage.ContributorNudge.CIFailureCooldown; got != "2h" {
		t.Fatalf("ci_failure_cooldown = %q, want trimmed 2h", got)
	}
	if got := decodeGovernorConfigGet(t, srv)["stageNudgeCICooldown"]; got != "2h" {
		t.Fatalf("GET stageNudgeCICooldown = %v, want 2h", got)
	}

	w = httptest.NewRecorder()
	srv.handleGovernorLabels(w, ownerPUT(t, "/api/config/governor/labels", `{"stage_nudge_ci_failure_cooldown":"-1h"}`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("negative duration: code = %d, want 400; body = %s", w.Code, w.Body.String())
	}
	if got := srv.deps.Config.Governor.Labels.Stage.ContributorNudge.CIFailureCooldown; got != "2h" {
		t.Fatalf("rejected save mutated ci_failure_cooldown to %q", got)
	}

	w = httptest.NewRecorder()
	srv.handleGovernorLabels(w, ownerPUT(t, "/api/config/governor/labels", `{"stage_nudge_ci_failure_cooldown":""}`))
	if w.Code != http.StatusOK {
		t.Fatalf("clear: code = %d, body = %s", w.Code, w.Body.String())
	}
	if got := srv.deps.Config.Governor.Labels.Stage.ContributorNudge.CIFailureCooldown; got != "" {
		t.Fatalf("empty must restore the default, got %q", got)
	}
}
