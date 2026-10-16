package dashboard

import (
	"net/http"
	"strings"
	"testing"
)

// The newly editable review.* keys round-trip through PUT /api/config/review;
// invalid values are rejected without changing anything and an absent key
// leaves its value untouched.
func TestReviewConfigPutParityBatch5RoundTrip(t *testing.T) {
	s := covApiServer(t)
	before := s.deps.Config.Review

	for _, bad := range []map[string]any{
		{"max_redispatch": -1},
		{"max_out_of_scope_backlog_issues": -1},
		{"event_driven_mode": "sometimes"},
		{"event_debounce_s": -1},
		{"event_debounce_s": 3601},
		{"pending_ttl": "forty minutes"},
		{"post_comments": true, "pending_ttl": "soon"},
	} {
		if rec := doPut(s, "/api/config/review", bad); rec.Code != http.StatusBadRequest {
			t.Fatalf("%v: expected 400, got %d", bad, rec.Code)
		}
	}
	if s.deps.Config.Review.PostComments != before.PostComments || s.deps.Config.Review.PendingTTL != before.PendingTTL {
		t.Fatal("a rejected PUT changed the config")
	}

	if rec := doPut(s, "/api/config/review", map[string]any{
		"post_comments":                   true,
		"event_driven_mode":               "off",
		"event_debounce_s":                120,
		"pending_ttl":                     " 30m ",
		"max_redispatch":                  5,
		"out_of_scope_backlog_disabled":   true,
		"max_out_of_scope_backlog_issues": 2,
		"max_reviews_per_head":            -1,
	}); rec.Code != http.StatusOK {
		t.Fatalf("review PUT: %d — %s", rec.Code, rec.Body.String())
	}
	rc := s.deps.Config.Review
	if !rc.PostComments || rc.EventDriven == nil || *rc.EventDriven || rc.EventDebounceS != 120 || rc.PendingTTL != "30m" ||
		rc.MaxRedispatch != 5 || !rc.OutOfScopeBacklogDisabled || rc.MaxOutOfScopeBacklogIssues != 2 || rc.MaxReviewsPerHead != -1 {
		t.Fatalf("review settings not applied: %+v", rc)
	}

	// A partial PUT changes only what it carries; auto clears event_driven
	// back to unset.
	if rec := doPut(s, "/api/config/review", map[string]any{"event_driven_mode": "auto", "pending_ttl": ""}); rec.Code != http.StatusOK {
		t.Fatalf("partial PUT: %d — %s", rec.Code, rec.Body.String())
	}
	rc = s.deps.Config.Review
	if rc.EventDriven != nil || rc.PendingTTL != "" || rc.EventDebounceS != 120 || rc.MaxRedispatch != 5 || !rc.PostComments {
		t.Fatalf("partial PUT changed unrelated keys or did not reset: %+v", rc)
	}
}

// Every review.* key this batch exposes is rendered on the Features tab with
// its YAML-key hint and a control bound to the review dirty map.
func TestReviewSettingsRenderEveryKey(t *testing.T) {
	body := extractJSFunction(t, indexHTML(t), "renderGovFeatures")
	for _, c := range []struct{ yaml, control string }{
		{"review.fan_out", `data-section="review" data-key="fan_out"`},
		{"review.max_parallel_reviews", `data-change-action="gh73"`},
		{"review.max_perspectives_per_pr", `data-change-action="gh81"`},
		{"review.combined_perspectives", `data-section="review" data-key="combined_perspectives"`},
		{"review.all_authors", `data-section="review" data-key="all_authors"`},
		{"review.fix_human_prs", `data-section="review" data-key="fix_human_prs"`},
		{"review.post_comments", `data-section="review" data-key="post_comments"`},
		{"review.acknowledge_no_findings", `data-section="review" data-key="acknowledge_no_findings"`},
		{"review.confidence_score", `data-section="review" data-key="confidence_score"`},
		{"review.priority_labels", `data-section="review" data-key="priority_labels"`},
		{"review.fixer_agent", `data-arg0="review" data-arg1="fixer_agent" data-arg-types="s,s,w"`},
		{"review.human_decision_label", `data-arg0="review" data-arg1="human_decision_label" data-arg-types="s,s,w"`},
		{"review.max_reviews_per_head", `data-arg0="review" data-arg1="max_reviews_per_head" data-arg-types="s,s,N"`},
		{"review.revise_verdicts_before", `data-arg0="review" data-arg1="revise_verdicts_before" data-arg-types="s,s,w"`},
		{"review.event_driven", `data-arg0="review" data-arg1="event_driven_mode" data-arg-types="s,s,v"`},
		{"review.event_debounce_s", `data-arg0="review" data-arg1="event_debounce_s" data-arg-types="s,s,N"`},
		{"review.pending_ttl", `data-arg0="review" data-arg1="pending_ttl" data-arg-types="s,s,w"`},
		{"review.max_redispatch", `data-arg0="review" data-arg1="max_redispatch" data-arg-types="s,s,N"`},
		{"review.out_of_scope_backlog_disabled", `data-section="review" data-key="out_of_scope_backlog_disabled"`},
		{"review.max_out_of_scope_backlog_issues", `data-arg0="review" data-arg1="max_out_of_scope_backlog_issues" data-arg-types="s,s,N"`},
		{"review.recommendations.enabled", `id="rec-enabled-switch" data-action="gh82"`},
		{"review.recommendations.min_ready_to_open", `id="rec-minready-input"`},
	} {
		if !strings.Contains(body, "yamlHint('"+c.yaml+"')") {
			t.Errorf("%s has no YAML-key hint", c.yaml)
		}
		if !strings.Contains(body, c.control) {
			t.Errorf("%s has no Features control (%s)", c.yaml, c.control)
		}
	}
}
