package dashboard

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

// #20073: the labels and features PUTs logged a saveConfig() error and still
// answered 200, so the change was live in memory but lost on restart.
func TestHandleGovernorLabels_FailedSaveIsNotSuccess(t *testing.T) {
	srv := newFullServer(t)
	unwritableConfig(t, srv.deps.Config)
	prevNudge := srv.deps.Config.AutoMerge.NudgeRepeatAfter
	prevTrusted := slices.Clone(srv.deps.Config.Project.IssueFilter.ReporterTrust.TrustedLogins)

	w := httptest.NewRecorder()
	srv.handleGovernorLabels(w, ownerPUT(t, "/api/config/governor/labels",
		`{"nudge_repeat_after":"48h","reporter_trust":{"trusted_logins":["octocat"]}}`))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d, want 500 for a failed save; body = %s", w.Code, w.Body.String())
	}
	if got := srv.deps.Config.AutoMerge.NudgeRepeatAfter; got != prevNudge {
		t.Fatalf("failed save left nudge_repeat_after = %q in memory, want %q", got, prevNudge)
	}
	if got := srv.deps.Config.Project.IssueFilter.ReporterTrust.TrustedLogins; !slices.Equal(got, prevTrusted) {
		t.Fatalf("failed save left trusted_logins = %v in memory, want %v", got, prevTrusted)
	}
}

func TestHandleGovernorFeatures_FailedSaveIsNotSuccess(t *testing.T) {
	srv := newFullServer(t)
	unwritableConfig(t, srv.deps.Config)

	w := httptest.NewRecorder()
	srv.handleGovernorFeatures(w, ownerPUT(t, "/api/config/governor/features", `{"retroEnabled":true}`))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d, want 500 for a failed save; body = %s", w.Code, w.Body.String())
	}
}
