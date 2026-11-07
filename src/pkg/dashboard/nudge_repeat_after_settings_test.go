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
