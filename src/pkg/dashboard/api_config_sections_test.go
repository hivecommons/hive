package dashboard

import (
	"bytes"
	"encoding/json"
	"github.com/hivecommons/hive/pkg/config"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestReviewConfigPut_MaxPerspectivesPerPR(t *testing.T) {
	s := covApiServer(t)

	if rec := doPut(s, "/api/config/review", map[string]any{"max_perspectives_per_pr": -1}); rec.Code != http.StatusBadRequest {
		t.Fatalf("negative max_perspectives_per_pr: expected 400, got %d", rec.Code)
	}
	if s.deps.Config.Review.MaxPerspectivesPerPR != 0 {
		t.Fatalf("rejected write still mutated config: %d", s.deps.Config.Review.MaxPerspectivesPerPR)
	}

	if rec := doPut(s, "/api/config/review", map[string]any{"max_perspectives_per_pr": 1}); rec.Code != http.StatusOK {
		t.Fatalf("valid put: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.deps.Config.Review.MaxPerspectivesPerPR != 1 {
		t.Fatalf("cap not applied: %d", s.deps.Config.Review.MaxPerspectivesPerPR)
	}

	// Absent key leaves the cap untouched; an explicit 0 clears it back to
	// "no cap", which is the documented way to turn the cap off.
	if rec := doPut(s, "/api/config/review", map[string]any{}); rec.Code != http.StatusOK {
		t.Fatalf("empty put: expected 200, got %d", rec.Code)
	}
	if s.deps.Config.Review.MaxPerspectivesPerPR != 1 {
		t.Fatalf("empty put mutated the cap: %d", s.deps.Config.Review.MaxPerspectivesPerPR)
	}
	if rec := doPut(s, "/api/config/review", map[string]any{"max_perspectives_per_pr": 0}); rec.Code != http.StatusOK {
		t.Fatalf("clearing put: expected 200, got %d", rec.Code)
	}
	if s.deps.Config.Review.MaxPerspectivesPerPR != 0 {
		t.Fatalf("cap not cleared: %d", s.deps.Config.Review.MaxPerspectivesPerPR)
	}
}

func TestReviewConfigPut_Recommendations(t *testing.T) {
	s := covApiServer(t)

	// A label list configured in hive.yaml has no control in the Features
	// dialog, so the browser omits it. It must survive a toggle from the UI.
	s.deps.Config.Review.Recommendations.Labels = []string{"triage"}

	if rec := doPut(s, "/api/config/review", map[string]any{
		"recommendations": map[string]any{
			"enabled":           true,
			"repos":             []string{" owner/one ", "", "owner/two"},
			"min_ready_to_open": 2,
		},
	}); rec.Code != http.StatusOK {
		t.Fatalf("valid put: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	got := s.deps.Config.Review.Recommendations
	if !got.Enabled {
		t.Fatal("recommendations not enabled")
	}
	if len(got.Repos) != 2 || got.Repos[0] != "owner/one" || got.Repos[1] != "owner/two" {
		t.Fatalf("repos not normalized: %v", got.Repos)
	}
	if got.MinReadyToOpen != 2 {
		t.Fatalf("min_ready_to_open not applied: %d", got.MinReadyToOpen)
	}
	if len(got.Labels) != 1 || got.Labels[0] != "triage" {
		t.Fatalf("labels erased by a dialog that cannot set them: %v", got.Labels)
	}

	// Absent key leaves the whole block untouched.
	if rec := doPut(s, "/api/config/review", map[string]any{}); rec.Code != http.StatusOK {
		t.Fatalf("empty put: expected 200, got %d", rec.Code)
	}
	if !s.deps.Config.Review.Recommendations.Enabled {
		t.Fatal("empty put disabled recommendations")
	}

	// Turning it back off is an explicit false, not an absent key.
	if rec := doPut(s, "/api/config/review", map[string]any{
		"recommendations": map[string]any{"enabled": false},
	}); rec.Code != http.StatusOK {
		t.Fatalf("disabling put: expected 200, got %d", rec.Code)
	}
	if s.deps.Config.Review.Recommendations.Enabled {
		t.Fatal("recommendations not disabled")
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

// The perspective set is validated by the same resolver the hive loads it
// with, so what the dialog accepts is exactly what will run. A typo must be a
// 400 with the offending name, not a silently dropped perspective.
func TestReviewConfigPut_Perspectives(t *testing.T) {
	s := covApiServer(t)

	rec := doPut(s, "/api/config/review", map[string]any{"perspectives": []string{"correctness", "sekurity"}})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "sekurity") {
		t.Fatalf("typo: expected 400 naming it, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(s.deps.Config.Review.Perspectives) != 0 {
		t.Fatalf("rejected write mutated config: %v", s.deps.Config.Review.Perspectives)
	}

	// A hive-defined perspective is only valid alongside its prompt.
	if rec := doPut(s, "/api/config/review", map[string]any{"perspectives": []string{"api-compat"}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("custom without prompt: expected 400, got %d", rec.Code)
	}
	rec = doPut(s, "/api/config/review", map[string]any{
		"perspectives":          []string{" Security ", "api-compat"},
		"perspective_prompts":   map[string]string{"api-compat": "public API breakage", "style": "  "},
		"combined_perspectives": true,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("valid put: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	rv := s.deps.Config.Review
	if len(rv.Perspectives) != 2 || rv.Perspectives[0] != "security" || rv.Perspectives[1] != "api-compat" {
		t.Fatalf("perspectives = %v", rv.Perspectives)
	}
	if rv.PerspectivePrompts["api-compat"] != "public API breakage" {
		t.Fatalf("prompts = %v", rv.PerspectivePrompts)
	}
	if _, ok := rv.PerspectivePrompts["style"]; ok {
		t.Fatal("blank prompt stored instead of dropped")
	}
	if !rv.CombinedPerspectives {
		t.Fatal("combined_perspectives not applied")
	}

	// Removing the prompt from under a selected custom perspective is refused:
	// the pair must stay valid.
	if rec := doPut(s, "/api/config/review", map[string]any{"perspective_prompts": map[string]string{}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("orphaning api-compat: expected 400, got %d", rec.Code)
	}

	// Absent keys leave everything untouched.
	if rec := doPut(s, "/api/config/review", map[string]any{}); rec.Code != http.StatusOK {
		t.Fatalf("empty put: %d", rec.Code)
	}
	if len(s.deps.Config.Review.Perspectives) != 2 || !s.deps.Config.Review.CombinedPerspectives {
		t.Fatalf("empty put mutated config: %+v", s.deps.Config.Review)
	}
}

// The revisit lane's two switches are writable through the API, a malformed
// cutoff is refused before it reaches config, and the write is pushed into
// whatever caches review settings at boot instead of waiting for a restart.
func TestReviewConfigPut_Revise(t *testing.T) {
	s := covApiServer(t)
	var applied []config.ReviewConfig
	s.deps.ReviewConfigApplied = func(rc config.ReviewConfig) { applied = append(applied, rc) }

	rec := doPut(s, "/api/config/review", map[string]any{"revise_verdicts_before": "yesterday"})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "RFC 3339") {
		t.Fatalf("bad cutoff: expected 400 naming the format, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.deps.Config.Review.ReviseVerdictsBefore != "" || len(applied) != 0 {
		t.Fatalf("rejected write leaked: cutoff=%q applied=%d", s.deps.Config.Review.ReviseVerdictsBefore, len(applied))
	}

	rec = doPut(s, "/api/config/review", map[string]any{
		"revise_repos":           []string{" o/r ", "", "o/s"},
		"revise_verdicts_before": "2026-09-19T14:00:00Z",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("valid put: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	rv := s.deps.Config.Review
	if len(rv.ReviseRepos) != 2 || rv.ReviseRepos[0] != "o/r" || rv.ReviseRepos[1] != "o/s" {
		t.Fatalf("revise_repos = %v", rv.ReviseRepos)
	}
	if rv.ReviseVerdictsBefore != "2026-09-19T14:00:00Z" {
		t.Fatalf("cutoff = %q", rv.ReviseVerdictsBefore)
	}
	if len(applied) != 1 || len(applied[0].ReviseRepos) != 2 {
		t.Fatalf("hook not called with the new config: %+v", applied)
	}

	// Empty cutoff clears the pilot; an omitted key leaves it alone.
	doPut(s, "/api/config/review", map[string]any{"combined_perspectives": true})
	if s.deps.Config.Review.ReviseVerdictsBefore == "" {
		t.Fatal("omitted key cleared the cutoff")
	}
	doPut(s, "/api/config/review", map[string]any{"revise_verdicts_before": ""})
	if s.deps.Config.Review.ReviseVerdictsBefore != "" {
		t.Fatal("empty cutoff did not clear")
	}
}
