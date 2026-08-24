package dashboard

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Tests for the three top-level config sections whose handlers had zero
// coverage: auto-merge (api_config_automerge.go), escalation
// (api_escalation.go) and review (api_config_review.go). Each section follows
// the same governor-config contract: owner-gated writes, "only what you send
// is changed" pointer semantics, and validate-before-mutate.

// doGetNoRole issues a GET with no role headers at all, for owner-gate
// negative checks on read endpoints.
func doGetNoRole(s *Server, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	s.mux.ServeHTTP(rec, req)
	return rec
}

// doPutNoRole issues a PUT with no role headers at all (unlike doPutRaw /
// doPutRawCovD, which both mark the request as a verified owner), for
// owner-gate negative checks on write endpoints.
func doPutNoRole(s *Server, path, raw string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, path, bytes.NewBufferString(raw))
	req.Header.Set("Content-Type", "application/json")
	s.mux.ServeHTTP(rec, req)
	return rec
}

// --- auto-merge -------------------------------------------------------------

func TestAutoMergeGet_OwnerSeesDefaults(t *testing.T) {
	s := covApiServer(t)
	rec := doOwnerGet(s, "/api/config/auto-merge")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET auto-merge: expected 200, got %d", rec.Code)
	}
	var body struct {
		SelfAuthored    bool `json:"self_authored"`
		SelfAuthoredSet bool `json:"self_authored_set"`
		MaxMerges       int  `json:"max_merges"`
		RequiredChecks  []string
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// nil SelfAuthored resolves to the effective default (enabled) with the
	// explicit-choice marker unset.
	if !body.SelfAuthored || body.SelfAuthoredSet {
		t.Fatalf("default tri-state wrong: self_authored=%v set=%v", body.SelfAuthored, body.SelfAuthoredSet)
	}
}

func TestAutoMergeGet_RejectsNonOwner(t *testing.T) {
	s := covApiServer(t)
	if rec := doGetNoRole(s, "/api/config/auto-merge"); rec.Code != http.StatusForbidden {
		t.Fatalf("un-gated GET auto-merge: expected 403, got %d", rec.Code)
	}
}

func TestAutoMergePut_ValidatesAndApplies(t *testing.T) {
	s := covApiServer(t)

	// Malformed body → 400.
	if rec := doPutRaw(s, "/api/config/auto-merge", "{nope"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body: expected 400, got %d", rec.Code)
	}
	// Negative max_merges must be refused BEFORE mutating.
	if rec := doPut(s, "/api/config/auto-merge", map[string]any{"max_merges": -1}); rec.Code != http.StatusBadRequest {
		t.Fatalf("negative max_merges: expected 400, got %d", rec.Code)
	}
	if s.deps.Config.AutoMerge.MaxMerges != 0 {
		t.Fatalf("rejected write still mutated max_merges: %d", s.deps.Config.AutoMerge.MaxMerges)
	}

	// Valid write applies every provided field; required_checks entries are
	// trimmed and blanks dropped.
	rec := doPut(s, "/api/config/auto-merge", map[string]any{
		"self_authored":   false,
		"max_merges":      3,
		"required_checks": []string{"  ci/test  ", "", "lint"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("valid put: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	am := s.deps.Config.AutoMerge
	if am.SelfAuthored == nil || *am.SelfAuthored {
		t.Fatalf("self_authored not applied: %+v", am.SelfAuthored)
	}
	if am.MaxMerges != 3 {
		t.Fatalf("max_merges not applied: %d", am.MaxMerges)
	}
	if len(am.RequiredChecks) != 2 || am.RequiredChecks[0] != "ci/test" || am.RequiredChecks[1] != "lint" {
		t.Fatalf("required_checks not normalized: %v", am.RequiredChecks)
	}

	// Absent keys leave settings untouched (pointer semantics).
	if rec := doPut(s, "/api/config/auto-merge", map[string]any{}); rec.Code != http.StatusOK {
		t.Fatalf("empty put: expected 200, got %d", rec.Code)
	}
	if s.deps.Config.AutoMerge.MaxMerges != 3 || len(s.deps.Config.AutoMerge.RequiredChecks) != 2 {
		t.Fatalf("empty put mutated config: %+v", s.deps.Config.AutoMerge)
	}
}

func TestAutoMergePut_RejectsNonOwner(t *testing.T) {
	s := covApiServer(t)
	if rec := doPutNoRole(s, "/api/config/auto-merge", `{"max_merges":9}`); rec.Code != http.StatusForbidden {
		t.Fatalf("un-gated PUT auto-merge: expected 403, got %d", rec.Code)
	}
	if s.deps.Config.AutoMerge.MaxMerges == 9 {
		t.Fatal("refused write still mutated auto_merge config")
	}
}

// --- escalation ---------------------------------------------------------------

func TestEscalationGetPut_RoundTrip(t *testing.T) {
	s := covApiServer(t)

	rec := doOwnerGet(s, "/api/config/escalation")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET escalation: expected 200, got %d", rec.Code)
	}
	var got struct {
		Disabled           bool `json:"disabled"`
		Threshold          int  `json:"threshold"`
		EffectiveThreshold int  `json:"effective_threshold"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// The breaker is opt-out: zero-value config must present as enabled with a
	// non-zero resolved threshold.
	if got.Disabled || got.EffectiveThreshold <= 0 {
		t.Fatalf("zero-value escalation defaults wrong: %+v", got)
	}

	// Malformed body → 400; negative threshold → 400 without mutating.
	if rec := doPutRaw(s, "/api/config/escalation", "{nope"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body: expected 400, got %d", rec.Code)
	}
	if rec := doPut(s, "/api/config/escalation", map[string]any{"threshold": -2}); rec.Code != http.StatusBadRequest {
		t.Fatalf("negative threshold: expected 400, got %d", rec.Code)
	}
	if s.deps.Config.Escalation.Threshold != 0 {
		t.Fatalf("rejected write still mutated threshold: %d", s.deps.Config.Escalation.Threshold)
	}

	// Valid write applies and echoes the updated section.
	rec = doPut(s, "/api/config/escalation", map[string]any{"disabled": true, "threshold": 7})
	if rec.Code != http.StatusOK {
		t.Fatalf("valid put: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !s.deps.Config.Escalation.Disabled || s.deps.Config.Escalation.Threshold != 7 {
		t.Fatalf("put not applied: %+v", s.deps.Config.Escalation)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Disabled || got.Threshold != 7 || got.EffectiveThreshold != 7 {
		t.Fatalf("response did not echo update: %+v", got)
	}

	// Absent keys leave settings untouched.
	if rec := doPut(s, "/api/config/escalation", map[string]any{}); rec.Code != http.StatusOK {
		t.Fatalf("empty put: expected 200, got %d", rec.Code)
	}
	if !s.deps.Config.Escalation.Disabled || s.deps.Config.Escalation.Threshold != 7 {
		t.Fatalf("empty put mutated config: %+v", s.deps.Config.Escalation)
	}
}

func TestEscalationOwnerGate(t *testing.T) {
	s := covApiServer(t)
	if rec := doGetNoRole(s, "/api/config/escalation"); rec.Code != http.StatusForbidden {
		t.Fatalf("un-gated GET escalation: expected 403, got %d", rec.Code)
	}
	if rec := doPutNoRole(s, "/api/config/escalation", `{"disabled":true}`); rec.Code != http.StatusForbidden {
		t.Fatalf("un-gated PUT escalation: expected 403, got %d", rec.Code)
	}
	if s.deps.Config.Escalation.Disabled {
		t.Fatal("refused write still disabled the escalation breaker")
	}
}

// --- review -------------------------------------------------------------------

func TestReviewConfigGet_ReturnsSection(t *testing.T) {
	s := covApiServer(t)
	rec := doGetNoRole(s, "/api/config/review")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET review: expected 200, got %d", rec.Code)
	}
	var got struct {
		RequireApproval bool `json:"require_approval"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.RequireApproval {
		t.Fatal("zero-value review config must not require approval")
	}
}

func TestReviewConfigPut_ValidatesAndApplies(t *testing.T) {
	s := covApiServer(t)

	if rec := doPutRaw(s, "/api/config/review", "{nope"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body: expected 400, got %d", rec.Code)
	}
	if rec := doPut(s, "/api/config/review", map[string]any{"max_parallel_reviews": -1}); rec.Code != http.StatusBadRequest {
		t.Fatalf("negative max_parallel_reviews: expected 400, got %d", rec.Code)
	}
	if s.deps.Config.Review.MaxParallelReviews != 0 {
		t.Fatalf("rejected write still mutated config: %d", s.deps.Config.Review.MaxParallelReviews)
	}

	rec := doPut(s, "/api/config/review", map[string]any{
		"require_approval":     true,
		"fan_out":              true,
		"max_parallel_reviews": 2,
		"reviewer_agents":      []string{" rev-a ", "", "rev-b"},
		"fixer_agent":          "  fixer  ",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("valid put: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	rv := s.deps.Config.Review
	if !rv.RequireApproval || !rv.FanOut || rv.MaxParallelReviews != 2 {
		t.Fatalf("scalars not applied: %+v", rv)
	}
	if len(rv.ReviewerAgents) != 2 || rv.ReviewerAgents[0] != "rev-a" || rv.ReviewerAgents[1] != "rev-b" {
		t.Fatalf("reviewer_agents not normalized: %v", rv.ReviewerAgents)
	}
	if rv.FixerAgent != "fixer" {
		t.Fatalf("fixer_agent not trimmed: %q", rv.FixerAgent)
	}

	// Absent keys leave settings untouched.
	if rec := doPut(s, "/api/config/review", map[string]any{}); rec.Code != http.StatusOK {
		t.Fatalf("empty put: expected 200, got %d", rec.Code)
	}
	if !s.deps.Config.Review.RequireApproval || s.deps.Config.Review.MaxParallelReviews != 2 {
		t.Fatalf("empty put mutated config: %+v", s.deps.Config.Review)
	}
}

func TestReviewConfigPut_RejectsNonOwner(t *testing.T) {
	s := covApiServer(t)
	if rec := doPutNoRole(s, "/api/config/review", `{"require_approval":true}`); rec.Code != http.StatusForbidden {
		t.Fatalf("un-gated PUT review: expected 403, got %d", rec.Code)
	}
	if s.deps.Config.Review.RequireApproval {
		t.Fatal("refused write still flipped require_approval")
	}
}
