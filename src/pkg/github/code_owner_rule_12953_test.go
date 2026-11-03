package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// hivecommons/hive#12953: a ruleset (or classic protection) that requires a
// code owner's review with required_approving_review_count 0 makes GitHub
// report reviewDecision null while the PR stays "blocked". The protection
// facts must carry that rule so HumanReviewBlocksMerge names it.
type codeOwnerServer struct {
	mu             sync.Mutex
	mergeableState string
	reviewDecision string
	checkRuns      []map[string]any
	// branchRules is the rules/branches answer; nil answers 404.
	branchRules []map[string]any
	// rulesetName is the GET rulesets/{id} name; "" answers 404.
	rulesetName string
	// classic is the classic required_pull_request_reviews answer; nil
	// answers 403 as it does for an App without Administration permission.
	classic    map[string]any
	rulesCalls int
	// rulesPaths records the escaped request path of every rules/branches call.
	rulesPaths  []string
	classicCall int
}

func (s *codeOwnerServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/graphql"):
			var decision any
			if s.reviewDecision != "" {
				decision = s.reviewDecision
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequests": map[string]any{
				"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
				"nodes": []map[string]any{{
					"number":                   1,
					"reviewDecision":           decision,
					"latestOpinionatedReviews": map[string]any{"nodes": []any{}},
				}},
			}}}})
		case strings.HasSuffix(path, "/pulls/1"):
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 1, "mergeable": false, "mergeable_state": s.mergeableState})
		case strings.Contains(path, "/check-runs"):
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(s.checkRuns), "check_runs": s.checkRuns})
		case strings.Contains(path, "/rules/branches/"):
			s.rulesCalls++
			s.rulesPaths = append(s.rulesPaths, r.URL.EscapedPath())
			if s.branchRules == nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(s.branchRules)
		case strings.Contains(path, "/rulesets/"):
			if s.rulesetName == "" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 7, "name": s.rulesetName, "enforcement": "active"})
		case strings.HasSuffix(path, "/protection/required_pull_request_reviews"):
			s.classicCall++
			if s.classic == nil {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(s.classic)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func codeOwnerPullRequestRule(codeOwner bool) map[string]any {
	return map[string]any{
		"type":                "pull_request",
		"ruleset_source_type": "Repository",
		"ruleset_source":      "org/repo",
		"ruleset_id":          7,
		"parameters": map[string]any{
			"required_approving_review_count":   0,
			"require_code_owner_review":         codeOwner,
			"dismiss_stale_reviews_on_push":     false,
			"require_last_push_approval":        false,
			"required_review_thread_resolution": false,
			"allowed_merge_methods":             []string{"squash"},
		},
	}
}

func requiredBuildRule() map[string]any {
	return map[string]any{
		"type":                "required_status_checks",
		"ruleset_source_type": "Repository",
		"ruleset_source":      "org/repo",
		"ruleset_id":          7,
		"parameters": map[string]any{
			"strict_required_status_checks_policy": true,
			"required_status_checks":               []map[string]any{{"context": "build"}},
		},
	}
}

func greenBuild() []map[string]any {
	return []map[string]any{{"name": "build", "status": "completed", "conclusion": "success"}}
}

func enrichCodeOwnerPRs(t *testing.T, s *codeOwnerServer, n int) []PullRequest {
	t.Helper()
	return enrichCodeOwnerPRsOn(t, s, n, "main", []string{"alice"})
}

func enrichCodeOwnerPRsOn(t *testing.T, s *codeOwnerServer, n int, baseRef string, requested []string) []PullRequest {
	t.Helper()
	t.Cleanup(resetRequiredChecksForbiddenCacheForTest(time.Now, time.Hour))
	srv := s.start(t)
	c := newTestClient(t, srv, "org", []string{"repo"})
	prs := make([]PullRequest, n)
	for i := range prs {
		prs[i] = PullRequest{Repo: "org/repo", Number: 1, HeadSHA: "abc123", BaseRef: baseRef, RequestedReviewers: requested}
	}
	c.EnrichCIStatus(context.Background(), prs)
	return prs
}

func TestEnrichCIStatus_RulesetCodeOwnerReviewBlocks12953(t *testing.T) {
	s := &codeOwnerServer{
		mergeableState: "blocked",
		checkRuns:      greenBuild(),
		branchRules:    []map[string]any{codeOwnerPullRequestRule(true), requiredBuildRule()},
		rulesetName:    "protect main",
	}
	prs := enrichCodeOwnerPRs(t, s, 2)
	pr := prs[0]
	if pr.Protection == nil || !pr.Protection.CodeOwnerReviewRequired {
		t.Fatalf("Protection = %+v, want CodeOwnerReviewRequired", pr.Protection)
	}
	if pr.Protection.ReviewDecision != ReviewDecisionNone {
		t.Fatalf("ReviewDecision = %q, want none", pr.Protection.ReviewDecision)
	}
	want := `code-owner review required by ruleset "protect main"`
	if got, blocks := pr.HumanReviewBlocksMerge(); !blocks || got != want {
		t.Errorf("HumanReviewBlocksMerge() = (%q, %v), want (%q, true)", got, blocks, want)
	}
	if got, ok := pr.BranchProtectionBlockReason(); !ok || got != want {
		t.Errorf("BranchProtectionBlockReason() = (%q, %v), want (%q, true)", got, ok, want)
	}
	if s.classicCall != 0 {
		t.Errorf("classic protection asked %d times although a ruleset already answered", s.classicCall)
	}
	// rules/branches also serves the required-check fallback (one call); the
	// code-owner lookup adds one more per repo+branch, not one per PR.
	if s.rulesCalls != 2 {
		t.Errorf("rules/branches called %d times for 2 PRs on one branch, want the code-owner lookup memoised", s.rulesCalls)
	}
}

// GetRulesForBranch does not escape the branch, so release/1.x used to 404 and
// the rule was never seen. The lookup must send the branch path-escaped.
func TestEnrichCIStatus_SlashBranchCodeOwnerRule12953(t *testing.T) {
	s := &codeOwnerServer{
		mergeableState: "blocked",
		checkRuns:      greenBuild(),
		branchRules:    []map[string]any{codeOwnerPullRequestRule(true), requiredBuildRule()},
		rulesetName:    "protect release",
	}
	pr := enrichCodeOwnerPRsOn(t, s, 1, "release/1.x", []string{"alice"})[0]
	want := `code-owner review required by ruleset "protect release"`
	if got, blocks := pr.HumanReviewBlocksMerge(); !blocks || got != want {
		t.Errorf("HumanReviewBlocksMerge() = (%q, %v), want (%q, true)", got, blocks, want)
	}
	if len(s.rulesPaths) == 0 {
		t.Fatal("rules/branches never called")
	}
	for _, p := range s.rulesPaths {
		if !strings.HasSuffix(p, "/rules/branches/release%2F1.x") {
			t.Errorf("rules/branches path = %q, want the branch escaped as release%%2F1.x", p)
		}
	}
}

// With no requested reviewer or team nothing says a code owner is the one
// GitHub waits on: still a block, but not named as code-owner review.
func TestEnrichCIStatus_CodeOwnerRuleWithoutRequestedReviewerIsNeutral12953(t *testing.T) {
	s := &codeOwnerServer{
		mergeableState: "blocked",
		checkRuns:      greenBuild(),
		branchRules:    []map[string]any{codeOwnerPullRequestRule(true), requiredBuildRule()},
		rulesetName:    "protect main",
	}
	pr := enrichCodeOwnerPRsOn(t, s, 1, "main", nil)[0]
	want := `blocked by a branch rule Hive does not read (ruleset "protect main" requires code-owner review)`
	if got, blocks := pr.HumanReviewBlocksMerge(); !blocks || got != want {
		t.Errorf("HumanReviewBlocksMerge() = (%q, %v), want (%q, true)", got, blocks, want)
	}
}

func TestEnrichCIStatus_ApprovedCodeOwnerReviewDoesNotBlock12953(t *testing.T) {
	s := &codeOwnerServer{
		mergeableState: "blocked",
		reviewDecision: "APPROVED",
		checkRuns:      greenBuild(),
		branchRules:    []map[string]any{codeOwnerPullRequestRule(true), requiredBuildRule()},
		rulesetName:    "protect main",
	}
	pr := enrichCodeOwnerPRs(t, s, 1)[0]
	if pr.Protection == nil || pr.Protection.ReviewDecision != ReviewDecisionApproved {
		t.Fatalf("Protection = %+v, want APPROVED", pr.Protection)
	}
	if pr.Protection.CodeOwnerReviewRequired {
		t.Error("CodeOwnerReviewRequired set on an approved PR")
	}
	if got, blocks := pr.HumanReviewBlocksMerge(); blocks {
		t.Errorf("HumanReviewBlocksMerge() = (%q, true), want no block once approved", got)
	}
}

func TestEnrichCIStatus_ClassicCodeOwnerReviewBlocks12953(t *testing.T) {
	s := &codeOwnerServer{
		mergeableState: "blocked",
		checkRuns:      greenBuild(),
		branchRules:    []map[string]any{codeOwnerPullRequestRule(false), requiredBuildRule()},
		classic:        map[string]any{"require_code_owner_reviews": true, "required_approving_review_count": 0},
	}
	pr := enrichCodeOwnerPRs(t, s, 1)[0]
	want := "code-owner review required by classic branch protection"
	if got, blocks := pr.HumanReviewBlocksMerge(); !blocks || got != want {
		t.Errorf("HumanReviewBlocksMerge() = (%q, %v), want (%q, true)", got, blocks, want)
	}
}

func TestEnrichCIStatus_UnnamedRulesetIsNamedByID12953(t *testing.T) {
	s := &codeOwnerServer{
		mergeableState: "blocked",
		checkRuns:      greenBuild(),
		branchRules:    []map[string]any{codeOwnerPullRequestRule(true), requiredBuildRule()},
	}
	pr := enrichCodeOwnerPRs(t, s, 1)[0]
	want := "code-owner review required by ruleset #7"
	if got, blocks := pr.HumanReviewBlocksMerge(); !blocks || got != want {
		t.Errorf("HumanReviewBlocksMerge() = (%q, %v), want (%q, true)", got, blocks, want)
	}
}

// No code-owner rule anywhere, classic protection unreadable: the PR keeps
// today's classification, and the 403 is not asked again for the next pass.
func TestEnrichCIStatus_NoCodeOwnerRuleKeepsClassification12953(t *testing.T) {
	s := &codeOwnerServer{
		mergeableState: "blocked",
		checkRuns:      greenBuild(),
		branchRules:    []map[string]any{codeOwnerPullRequestRule(false), requiredBuildRule()},
	}
	t.Cleanup(resetRequiredChecksForbiddenCacheForTest(time.Now, time.Hour))
	srv := s.start(t)
	c := newTestClient(t, srv, "org", []string{"repo"})
	for pass := 0; pass < 2; pass++ {
		prs := []PullRequest{{Repo: "org/repo", Number: 1, HeadSHA: "abc123", BaseRef: "main"}}
		c.EnrichCIStatus(context.Background(), prs)
		if prs[0].Protection != nil && prs[0].Protection.CodeOwnerReviewRequired {
			t.Fatalf("pass %d: CodeOwnerReviewRequired set with no code-owner rule", pass)
		}
		if got, blocks := prs[0].HumanReviewBlocksMerge(); blocks {
			t.Fatalf("pass %d: HumanReviewBlocksMerge() = (%q, true), want no block", pass, got)
		}
	}
	if s.classicCall != 1 {
		t.Errorf("classic protection asked %d times across two passes, want 1 (403 cached)", s.classicCall)
	}
}

// Only a blocked PR is looked up: a clean one costs no extra call.
func TestEnrichCIStatus_CodeOwnerRuleNotReadUnlessBlocked12953(t *testing.T) {
	s := &codeOwnerServer{
		mergeableState: "clean",
		checkRuns:      greenBuild(),
		classic:        map[string]any{"require_code_owner_reviews": true},
	}
	pr := enrichCodeOwnerPRs(t, s, 1)[0]
	if pr.Protection != nil && pr.Protection.CodeOwnerReviewRequired {
		t.Error("CodeOwnerReviewRequired set on a clean PR")
	}
	if s.classicCall != 0 {
		t.Errorf("classic protection asked %d times for a clean PR, want 0", s.classicCall)
	}
}

func TestCodeOwnerReviewRuleWithoutClient12953(t *testing.T) {
	var nilClient *Client
	if got := nilClient.codeOwnerReviewRule(context.Background(), "org", "repo", "main"); got.required {
		t.Errorf("nil client: %+v", got)
	}
	if got := (&Client{}).codeOwnerReviewRule(context.Background(), "org", "repo", "main"); got.required {
		t.Errorf("no REST client: %+v", got)
	}
	if got := (&Client{}).rulesetLabel(context.Background(), "org", "repo", 0); got != "a ruleset" {
		t.Errorf("rulesetLabel(0) = %q", got)
	}
}

func TestCodeOwnerReviewBlockReason12953(t *testing.T) {
	requested := []string{"alice"}
	rule := func(f ProtectionFacts) *ProtectionFacts {
		f.CodeOwnerReviewRequired = true
		if f.CodeOwnerRuleSource == "" {
			f.CodeOwnerRuleSource = `ruleset "protect main"`
		}
		return &f
	}
	const want = `code-owner review required by ruleset "protect main"`
	tests := []struct {
		name       string
		pr         PullRequest
		want       string
		wantBlocks bool
	}{
		{name: "blocked, CI green", pr: PullRequest{RequestedReviewers: requested, MergeableState: "blocked", CIStatus: "success", Protection: rule(ProtectionFacts{})}, want: want, wantBlocks: true},
		{name: "blocked, only a non-required check red", pr: PullRequest{RequestedReviewers: requested, MergeableState: "BLOCKED", CIStatus: "failure", Protection: rule(ProtectionFacts{RequiredChecksKnown: true})}, want: want, wantBlocks: true},
		{name: "no rule source falls back to branch protection", pr: PullRequest{RequestedReviewers: requested, MergeableState: "blocked", CIStatus: "success", Protection: &ProtectionFacts{CodeOwnerReviewRequired: true}}, want: "code-owner review required by branch protection", wantBlocks: true},
		{name: "no requested reviewer is neutral but still blocks", pr: PullRequest{MergeableState: "blocked", CIStatus: "success", Protection: rule(ProtectionFacts{})}, want: `blocked by a branch rule Hive does not read (ruleset "protect main" requires code-owner review)`, wantBlocks: true},
		{name: "requested team names code-owner review", pr: PullRequest{RequestedTeams: []string{"owners"}, MergeableState: "blocked", CIStatus: "success", Protection: rule(ProtectionFacts{})}, want: want, wantBlocks: true},
		{name: "required check red", pr: PullRequest{RequestedReviewers: requested, MergeableState: "blocked", CIStatus: "failure", Protection: rule(ProtectionFacts{RequiredChecksKnown: true, FailingRequiredChecks: []string{"build"}})}},
		{name: "required check missing", pr: PullRequest{RequestedReviewers: requested, MergeableState: "blocked", CIStatus: "failure", Protection: rule(ProtectionFacts{RequiredChecksKnown: true, MissingRequiredChecks: []string{"build"}})}},
		{name: "CI pending", pr: PullRequest{RequestedReviewers: requested, MergeableState: "blocked", CIStatus: "pending", Protection: rule(ProtectionFacts{RequiredChecksKnown: true})}},
		{name: "CI red, required set unknown", pr: PullRequest{RequestedReviewers: requested, MergeableState: "blocked", CIStatus: "failure", Protection: rule(ProtectionFacts{})}},
		{name: "not blocked", pr: PullRequest{RequestedReviewers: requested, MergeableState: "clean", CIStatus: "success", Protection: rule(ProtectionFacts{})}},
		{name: "approved", pr: PullRequest{RequestedReviewers: requested, MergeableState: "blocked", CIStatus: "success", Protection: rule(ProtectionFacts{ReviewDecision: ReviewDecisionApproved})}},
		{name: "no code-owner rule", pr: PullRequest{RequestedReviewers: requested, MergeableState: "blocked", CIStatus: "success", Protection: &ProtectionFacts{}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, blocks := tc.pr.HumanReviewBlocksMerge()
			if blocks != tc.wantBlocks || got != tc.want {
				t.Fatalf("HumanReviewBlocksMerge() = (%q, %v), want (%q, %v)", got, blocks, tc.want, tc.wantBlocks)
			}
		})
	}
}
