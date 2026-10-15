package dashboard

import (
	"encoding/json"
	"net/http"
	"testing"
)

type advisoryAPIResponse struct {
	MaxFindings       int                         `json:"max_findings"`
	ShowAll           bool                        `json:"show_all"`
	StalenessDays     int                         `json:"staleness_days"`
	PRAutoClose       bool                        `json:"pr_autoclose"`
	UpdateIntervalS   int                         `json:"update_interval_s"`
	PostRetryBackoffS int                         `json:"post_retry_backoff_s"`
	QueueHealth       advisoryQueueHealthResponse `json:"queue_health"`
}

type advisoryQueueHealthResponse struct {
	BlockedPRPct       int `json:"blocked_pr_pct"`
	NeedsHumanPRPct    int `json:"needs_human_pr_pct"`
	NeedsHumanIssuePct int `json:"needs_human_issue_pct"`
	StaleBlockedPRs    int `json:"stale_blocked_prs"`
	LaneSharePct       int `json:"lane_share_pct"`
	CheckSharePct      int `json:"check_share_pct"`
}

func getAdvisorySettings(t *testing.T, s *Server) advisoryAPIResponse {
	t.Helper()
	rec := doOwnerGet(s, "/api/config/governor/advisory")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET advisory settings: %d — %s", rec.Code, rec.Body.String())
	}
	var resp advisoryAPIResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding advisory settings: %v", err)
	}
	return resp
}

// TestGovAdvisory_RequiresOwner pins the gate: these settings decide what the
// hive reports to repo owners, so a non-owner must not be able to read or
// shrink them.
func TestGovAdvisory_RequiresOwner(t *testing.T) {
	s := govServer(t)
	if rec := doGet(s, "/api/config/governor/advisory"); rec.Code != http.StatusForbidden {
		t.Errorf("unauthenticated GET = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

// TestGovAdvisory_GetPutRoundTrip exercises the settings surface end to end:
// the GET reports the resolved values, the PUT stores only the fields it was
// sent, and pr_autoclose's tri-state is reported as the boolean the hive acts
// on.
func TestGovAdvisory_GetPutRoundTrip(t *testing.T) {
	s := govServer(t)

	if rec := doPut(s, "/api/config/governor/advisory", map[string]any{
		"max_findings": 25, "show_all": true, "staleness_days": 3, "pr_autoclose": false,
		"post_retry_backoff_s": 900,
		"queue_health": map[string]any{
			"blocked_pr_pct": 51, "needs_human_pr_pct": 26, "needs_human_issue_pct": 31,
			"stale_blocked_prs": 11, "lane_share_pct": 52, "check_share_pct": 53,
		},
	}); rec.Code != http.StatusOK {
		t.Fatalf("put advisory settings: %d — %s", rec.Code, rec.Body.String())
	}

	got := getAdvisorySettings(t, s)
	if got.MaxFindings != 25 || !got.ShowAll || got.StalenessDays != 3 || got.PRAutoClose || got.PostRetryBackoffS != 900 {
		t.Fatalf("settings = %+v, want advisory round-trip fields", got)
	}
	if got.QueueHealth.BlockedPRPct != 51 || got.QueueHealth.NeedsHumanPRPct != 26 || got.QueueHealth.NeedsHumanIssuePct != 31 ||
		got.QueueHealth.StaleBlockedPRs != 11 || got.QueueHealth.LaneSharePct != 52 || got.QueueHealth.CheckSharePct != 53 {
		t.Fatalf("queue_health = %+v, want configured thresholds", got.QueueHealth)
	}
	if s.deps.Config.Governor.Advisory.PRAutoCloseEnabled() {
		t.Error("pr_autoclose: false must be stored as an explicit false pointer")
	}

	// Partial update: an absent key leaves its setting alone.
	if rec := doPut(s, "/api/config/governor/advisory", map[string]any{"max_findings": 5}); rec.Code != http.StatusOK {
		t.Fatalf("partial put: %d", rec.Code)
	}
	got = getAdvisorySettings(t, s)
	if got.MaxFindings != 5 {
		t.Errorf("MaxFindings = %d, want the updated 5", got.MaxFindings)
	}
	if !got.ShowAll || got.StalenessDays != 3 || got.PRAutoClose {
		t.Errorf("partial put changed untouched fields: %+v", got)
	}
}

// TestGovAdvisory_Validation refuses values that would silently discard the
// operator's intent (max_findings 0 resolves back to the default on load) or
// close findings faster than agents can re-report them.
func TestGovAdvisory_Validation(t *testing.T) {
	s := govServer(t)

	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{"zero max findings", map[string]any{"max_findings": 0}},
		{"negative max findings", map[string]any{"max_findings": -1}},
		{"zero staleness", map[string]any{"staleness_days": 0}},
		{"negative staleness", map[string]any{"staleness_days": -7}},
		{"negative update interval", map[string]any{"update_interval_s": -60}},
		{"update interval below minimum", map[string]any{"update_interval_s": 5}},
		{"update interval above maximum", map[string]any{"update_interval_s": 86400}},
		{"negative queue health", map[string]any{"queue_health": map[string]any{"blocked_pr_pct": -1}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if rec := doPut(s, "/api/config/governor/advisory", tc.body); rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d — %s", rec.Code, rec.Body.String())
			}
		})
	}

	if rec := doPutRaw(s, "/api/config/governor/advisory", "not json"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body: expected 400, got %d", rec.Code)
	}
}

// TestGovAdvisory_UpdateIntervalRoundTrip pins the #4820 knob's API contract:
// unset reports 0 (the "default cadence" sentinel), an in-range PUT round-trips
// verbatim, an explicit 0 resets to the default, and partial updates leave it
// untouched.
func TestGovAdvisory_UpdateIntervalRoundTrip(t *testing.T) {
	s := govServer(t)

	if got := getAdvisorySettings(t, s); got.UpdateIntervalS != 0 {
		t.Fatalf("untouched update_interval_s = %d, want the 0 default sentinel", got.UpdateIntervalS)
	}

	if rec := doPut(s, "/api/config/governor/advisory", map[string]any{"update_interval_s": 300}); rec.Code != http.StatusOK {
		t.Fatalf("put update_interval_s: %d — %s", rec.Code, rec.Body.String())
	}
	if got := getAdvisorySettings(t, s); got.UpdateIntervalS != 300 {
		t.Fatalf("update_interval_s = %d, want the configured 300", got.UpdateIntervalS)
	}
	if s.deps.Config.Governor.Advisory.UpdateIntervalS != 300 {
		t.Fatal("PUT did not store update_interval_s on the live config")
	}

	// A partial update of a sibling field leaves the interval alone.
	if rec := doPut(s, "/api/config/governor/advisory", map[string]any{"max_findings": 7}); rec.Code != http.StatusOK {
		t.Fatalf("partial put: %d", rec.Code)
	}
	if got := getAdvisorySettings(t, s); got.UpdateIntervalS != 300 {
		t.Fatalf("partial put changed update_interval_s to %d", got.UpdateIntervalS)
	}

	// Explicit 0 is the supported way back to the default cadence.
	if rec := doPut(s, "/api/config/governor/advisory", map[string]any{"update_interval_s": 0}); rec.Code != http.StatusOK {
		t.Fatalf("reset put: %d — %s", rec.Code, rec.Body.String())
	}
	if got := getAdvisorySettings(t, s); got.UpdateIntervalS != 0 {
		t.Fatalf("update_interval_s = %d after reset, want 0", got.UpdateIntervalS)
	}
}

// TestGovAdvisory_InGovernorConfigGet confirms the aggregated governor config
// carries the advisory section, which is what the dashboard tab prefills from.
func TestGovAdvisory_InGovernorConfigGet(t *testing.T) {
	s := govServer(t)
	rec := doOwnerGet(s, "/api/config/governor")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET governor config: %d", rec.Code)
	}
	var resp struct {
		Advisory *advisoryAPIResponse `json:"advisory"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding governor config: %v", err)
	}
	if resp.Advisory == nil {
		t.Fatal("governor config GET is missing the advisory section")
	}
}

// TestGovAdvisory_TargetRoundTrip covers the digest-target surface: the GET
// reports the resolved target (github when unset), the PUT stores target and
// linear_issue, a Linear target without an issue is rejected, and an unknown
// target is rejected — so the governor never has to fail closed on a value
// the dashboard could have refused.
func TestGovAdvisory_TargetRoundTrip(t *testing.T) {
	s := govServer(t)

	var got map[string]any
	rec := doOwnerGet(s, "/api/config/governor/advisory")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: %d — %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["target"] != "github" || got["linear_issue"] != "" {
		t.Fatalf("default target/linear_issue = %v/%v, want github/\"\"", got["target"], got["linear_issue"])
	}

	if rec := doPut(s, "/api/config/governor/advisory", map[string]any{"target": "linear"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("target=linear without linear_issue: %d, want 400 — %s", rec.Code, rec.Body.String())
	}
	if rec := doPut(s, "/api/config/governor/advisory", map[string]any{"target": "jira", "linear_issue": "X-1"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown target: %d, want 400 — %s", rec.Code, rec.Body.String())
	}
	if s.deps.Config.Governor.Advisory.Target != "" || s.deps.Config.Governor.Advisory.LinearIssue != "" {
		t.Fatal("rejected PUTs must not mutate config")
	}

	if rec := doPut(s, "/api/config/governor/advisory", map[string]any{"target": " Linear ", "linear_issue": " ONB-123 "}); rec.Code != http.StatusOK {
		t.Fatalf("PUT linear: %d — %s", rec.Code, rec.Body.String())
	}
	if a := s.deps.Config.Governor.Advisory; a.Target != "linear" || a.LinearIssue != "ONB-123" {
		t.Fatalf("stored = {%q %q}, want normalized {linear ONB-123}", a.Target, a.LinearIssue)
	}
	// Once an issue is stored, re-sending only the target is fine.
	if rec := doPut(s, "/api/config/governor/advisory", map[string]any{"target": "linear"}); rec.Code != http.StatusOK {
		t.Fatalf("PUT linear with stored issue: %d — %s", rec.Code, rec.Body.String())
	}
	// And switching back to GitHub leaves the Linear issue in place for a
	// later switch without forcing the operator to retype it.
	if rec := doPut(s, "/api/config/governor/advisory", map[string]any{"target": "github"}); rec.Code != http.StatusOK {
		t.Fatalf("PUT github: %d — %s", rec.Code, rec.Body.String())
	}
	if a := s.deps.Config.Governor.Advisory; a.ResolvedTarget() != "github" || a.LinearIssue != "ONB-123" {
		t.Fatalf("after switching back = {%q %q}, want {github ONB-123}", a.Target, a.LinearIssue)
	}
}
