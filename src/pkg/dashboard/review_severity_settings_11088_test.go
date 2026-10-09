package dashboard

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// TestReviewConfigPut_SeverityBacklog covers the 'Severity & backlog' write
// path (#11088): owner-only, validate-before-mutate with errors that name the
// field, absent keys untouched, and destinations gated on the active work
// source.
func TestReviewConfigPut_SeverityBacklog(t *testing.T) {
	s := covApiServer(t)
	blockAt := func() string { return s.deps.Config.Review.Severity.BlockAt }

	if rec := doPutNoRole(s, "/api/config/review", `{"severity":{"block_at":"P2"}}`); rec.Code != http.StatusForbidden {
		t.Fatalf("un-gated PUT severity: expected 403, got %d", rec.Code)
	}
	if blockAt() != "" {
		t.Fatal("refused write still set block_at")
	}

	bad := []struct {
		body  map[string]any
		field string
	}{
		{map[string]any{"require_approval": true, "severity": map[string]any{"block_at": "P0"}}, "review.severity.block_at"},
		{map[string]any{"backlog": map[string]any{"destination": "trello"}}, "review.backlog.destination"},
		{map[string]any{"backlog": map[string]any{"destination": "linear"}}, "review.backlog.destination"},
		{map[string]any{"backlog": map[string]any{"max_per_pr_per_day": -1}}, "review.backlog.max_per_pr_per_day"},
	}
	for _, tc := range bad {
		rec := doPut(s, "/api/config/review", tc.body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("PUT %v: expected 400, got %d", tc.body, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), tc.field) {
			t.Errorf("PUT %v: error %q does not name %s", tc.body, rec.Body.String(), tc.field)
		}
	}
	if s.deps.Config.Review.RequireApproval || blockAt() != "" || s.deps.Config.Review.Backlog.Destination != "" {
		t.Fatal("rejected requests must not mutate anything")
	}

	if rec := doPut(s, "/api/config/review", map[string]any{"severity": map[string]any{"block_at": " p1 ", "comment_below": false}}); rec.Code != http.StatusOK {
		t.Fatalf("PUT severity: %d: %s", rec.Code, rec.Body.String())
	}
	sev := s.deps.Config.Review.Severity
	if sev.BlockAt != "P1" || sev.CommentBelowEnabled() || !sev.BacklogBelowEnabled() {
		t.Fatalf("severity not stored/normalised: %+v", sev)
	}
	if s.deps.Config.Classification.ReviewBots.MinPriority != "" {
		t.Fatal("block_at must not be copied into classification.review_bots.min_priority")
	}

	s.deps.Config.Governor.WorkSource.Type = "linear"
	if rec := doPut(s, "/api/config/review", map[string]any{"backlog": map[string]any{"destination": "linear", "linear_state": " Triage ", "labels": []string{"from-review", " ", "nit"}, "max_per_pr_per_day": 4}}); rec.Code != http.StatusOK {
		t.Fatalf("PUT backlog: %d: %s", rec.Code, rec.Body.String())
	}
	bl := s.deps.Config.Review.Backlog
	if bl.Destination != "linear" || bl.LinearState != "Triage" || !reflect.DeepEqual(bl.Labels, []string{"from-review", "nit"}) || bl.MaxPerPRPerDay != 4 {
		t.Fatalf("backlog not stored: %+v", bl)
	}

	if rec := doPut(s, "/api/config/review", map[string]any{"all_authors": true, "severity": map[string]any{}}); rec.Code != http.StatusOK {
		t.Fatalf("PUT without severity keys: %d", rec.Code)
	}
	if s.deps.Config.Review.Severity.BlockAt != "P1" || s.deps.Config.Review.Backlog.Destination != "linear" {
		t.Fatal("absent keys changed stored values")
	}

	s.deps.Config.Governor.WorkSource.Type = "github_projects"
	rec := doPut(s, "/api/config/review", map[string]any{"backlog": map[string]any{"destination": "github_project"}})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "review.backlog.project_column_id") {
		t.Fatalf("github_project without a column: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doPut(s, "/api/config/review", map[string]any{"backlog": map[string]any{"destination": "github_project", "project_column_id": "f75ad846"}}); rec.Code != http.StatusOK {
		t.Fatalf("PUT github_project: %d: %s", rec.Code, rec.Body.String())
	}
}

// The settings UI is JS inside index.html; pin its wiring.
func TestReviewSeverityBacklogSettingsStaticWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		"${renderReviewSeverityBacklog(rv)}",
		"function renderReviewSeverityBacklog(rv)",
		"'Strict (P0–P2 block)'",
		"'Ship fast (P0–P1 block)'",
		"'Everything blocks'",
		`data-action="applyReviewSeverityPreset"`,
		`data-action="toggleReviewSeverityFlag"`,
		`data-change-action="setReviewBacklogDestination"`,
		"markDirty('review', 'severity',",
		"markDirty('review', 'backlog',",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html is missing severity/backlog wiring %q", want)
		}
	}

	render := jsFunc(t, html, "renderReviewSeverityBacklog")
	for _, want := range []string{
		"dashboardRoleAtLeast(window._hiveRole || 'read', 'owner')",
		"reviewBacklogDestinationsFor(ws)",
		"dest === 'github_project'",
		"dest === 'linear'",
		"dest === 'jira'",
		"project_column_id",
		"linear_state",
		"jira_status",
		"max_per_pr_per_day",
	} {
		if !strings.Contains(render, want) {
			t.Errorf("renderReviewSeverityBacklog missing %q", want)
		}
	}
	filter := jsFunc(t, html, "reviewBacklogDestinationsFor")
	if !strings.Contains(filter, "!o.workSource || o.workSource === ws") {
		t.Error("destination selector must offer only the active work source's destination plus GitHub issue")
	}
	for _, fn := range []string{"renderReviewSeverityBacklog", "applyReviewSeverityPreset", "toggleReviewSeverityFlag", "markDirtyReviewBacklog", "setReviewBacklogDestination"} {
		body := jsFunc(t, html, fn)
		for _, banned := range []string{"window.alert", "window.confirm", "window.prompt", "alert(", "confirm(", "prompt("} {
			if strings.Contains(body, banned) {
				t.Errorf("%s uses banned native dialog %q", fn, banned)
			}
		}
		if strings.Contains(body, "style=") {
			t.Errorf("%s adds an inline style", fn)
		}
	}
}
