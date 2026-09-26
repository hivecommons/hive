package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// The list payload already carries requested reviewers and teams; the
// enumeration copies them onto every PR record it builds — actionable, held,
// and stale draft — at no extra API cost (hivecommons/hive#8968).
func TestFetchPRsCarriesRequestedReviewersAndTeams(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widget/pulls", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
		  {"number":101,"title":"fix: actionable","user":{"login":"bot"},"created_at":"2026-09-01T00:00:00Z",
		   "requested_reviewers":[{"login":"alice"},{"login":"bob"}],"requested_teams":[{"slug":"maintainers","name":"Maintainers"}]},
		  {"number":102,"title":"fix: held","user":{"login":"bot"},"labels":[{"name":"hold"}],"created_at":"2026-09-01T00:00:00Z",
		   "requested_reviewers":[{"login":"carol"}],"requested_teams":[{"name":"Only Name"}]},
		  {"number":103,"title":"wip: stale draft","user":{"login":"hive[bot]"},"draft":true,"created_at":"2026-09-01T00:00:00Z",
		   "requested_reviewers":[{"login":"dave"}]},
		  {"number":104,"title":"fix: nobody asked","user":{"login":"bot"},"created_at":"2026-09-01T00:00:00Z",
		   "requested_reviewers":[],"requested_teams":[]}
		]`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "acme", []string{"widget"})
	c.appBotLogin = "hive[bot]"

	actionable, _, heldPRs, staleDrafts, _, _, _, err := c.fetchPRs(t.Context(), "widget")
	if err != nil {
		t.Fatalf("fetchPRs: %v", err)
	}
	byNumber := map[int]PullRequest{}
	for _, pr := range actionable {
		byNumber[pr.Number] = pr
	}
	for _, pr := range heldPRs {
		byNumber[pr.Number] = pr
	}
	for _, pr := range staleDrafts {
		byNumber[pr.Number] = pr
	}
	want := map[int]struct{ logins, teams []string }{
		101: {[]string{"alice", "bob"}, []string{"maintainers"}},
		102: {[]string{"carol"}, []string{"Only Name"}},
		103: {[]string{"dave"}, nil},
		104: {nil, nil},
	}
	for number, w := range want {
		got, ok := byNumber[number]
		if !ok {
			t.Fatalf("PR #%d missing from fetchPRs output", number)
		}
		if !reflect.DeepEqual(got.RequestedReviewers, w.logins) {
			t.Errorf("#%d RequestedReviewers = %v, want %v", number, got.RequestedReviewers, w.logins)
		}
		if !reflect.DeepEqual(got.RequestedTeams, w.teams) {
			t.Errorf("#%d RequestedTeams = %v, want %v", number, got.RequestedTeams, w.teams)
		}
	}
	// nil, not [], so a PR nobody was asked to review carries no field at all.
	raw, err := json.Marshal(byNumber[104])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "requested_reviewers") || strings.Contains(string(raw), "requested_teams") {
		t.Errorf("empty review requests serialised: %s", raw)
	}
}

// Comment and review-thread totals and GitHub's closing-issue references ride
// the one-per-repository review-decision query and land on the PR itself —
// including when GitHub reports no review decision, which is the normal case
// on a repository whose base branch requires no review.
func TestEnrichCIStatus_CarriesConversationCountsAndLinkedIssues(t *testing.T) {
	p := &protectionServer{
		mergeableState: "clean",
		checkRuns:      []map[string]any{{"name": "build", "status": "completed", "conclusion": "success"}},
		comments:       3,
		reviewThreads:  2,
		closingIssues: []map[string]any{
			{"number": 12, "state": "OPEN", "url": "https://github.test/org/repo/issues/12", "repository": map[string]any{"nameWithOwner": "org/repo"}},
			{"number": 7, "state": "CLOSED", "url": "https://github.test/org/other/issues/7", "repository": map[string]any{"nameWithOwner": "org/other"}},
			{"number": 0},
		},
	}
	pr := enrichOnePR(t, p, nil)
	if pr.Protection != nil {
		t.Errorf("no review decision and no required checks must leave Protection nil, got %+v", pr.Protection)
	}
	if pr.CommentCount != 3 || pr.ReviewThreadCount != 2 {
		t.Errorf("counts = %d comments / %d threads, want 3 / 2", pr.CommentCount, pr.ReviewThreadCount)
	}
	wantLinked := []PRLinkedIssue{
		{Number: 12, Repo: "org/repo", State: "open", URL: "https://github.test/org/repo/issues/12"},
		{Number: 7, Repo: "org/other", State: "closed", URL: "https://github.test/org/other/issues/7"},
	}
	if !reflect.DeepEqual(pr.LinkedIssues, wantLinked) {
		t.Errorf("LinkedIssues = %+v, want %+v", pr.LinkedIssues, wantLinked)
	}

	// The query itself must ask for every field in one request.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		for _, field := range []string{"reviewDecision", "comments(first:1){totalCount}", "reviewThreads(first:1){totalCount}", "closingIssuesReferences("} {
			if !strings.Contains(string(body), field) {
				t.Errorf("graphql query does not ask for %s: %s", field, body)
			}
		}
		_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequests":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[]}}}}`))
	}))
	defer srv.Close()
	c := newTestClient(t, srv, "org", []string{"repo"})
	if got := c.fetchReviewDecisions(context.Background(), "org/repo"); got == nil {
		t.Fatal("fetchReviewDecisions returned nil for a successful empty page")
	}
}

// A base branch with no review rule reports a null reviewDecision. The
// reviewer opinions GitHub did return must still reach the wire — that is
// the only review evidence the pill has on such a repository — while the
// decision itself stays ReviewDecisionNone: unknown, never inferred.
func TestEnrichCIStatus_ReviewOpinionsSurviveNullDecision(t *testing.T) {
	p := &protectionServer{
		mergeableState: "clean",
		checkRuns:      []map[string]any{{"name": "build", "status": "completed", "conclusion": "success"}},
		reviews: []map[string]any{
			{"state": "APPROVED", "author": map[string]any{"login": "one"}},
			{"state": "CHANGES_REQUESTED", "author": map[string]any{"login": "octo"}},
		},
	}
	pr := enrichOnePR(t, p, nil)
	if pr.Protection == nil {
		t.Fatal("Protection dropped: reviewer opinions without a decision never reached the wire")
	}
	if pr.Protection.ReviewDecision != ReviewDecisionNone {
		t.Errorf("ReviewDecision = %q, want none: a decision must never be inferred from opinions", pr.Protection.ReviewDecision)
	}
	if pr.Protection.ApprovalsGiven != 1 || !reflect.DeepEqual(pr.Protection.ChangesRequestedBy, []string{"octo"}) {
		t.Errorf("opinions = %d approvals / %v, want 1 / [octo]", pr.Protection.ApprovalsGiven, pr.Protection.ChangesRequestedBy)
	}
	if _, ok := pr.BranchProtectionBlockReason(); ok {
		t.Error("opinions alone must not name a branch-protection block reason")
	}
	raw, _ := json.Marshal(pr)
	if !strings.Contains(string(raw), `"changes_requested_by":["octo"]`) || strings.Contains(string(raw), `"review_decision"`) {
		t.Errorf("wire = %s", raw)
	}
}

// A forge that rejects the extended query (a GHE without one of the added
// fields) must still yield the review decision: the display signals fall
// away, the merge-block wording does not. One retry, decision-only.
func TestEnrichCIStatus_SignalsQueryRejectionFallsBackToDecisionOnly(t *testing.T) {
	p := &protectionServer{
		mergeableState:  "blocked",
		reviewDecision:  "CHANGES_REQUESTED",
		reviews:         []map[string]any{{"state": "CHANGES_REQUESTED", "author": map[string]any{"login": "octo"}}},
		checkRuns:       []map[string]any{{"name": "build", "status": "completed", "conclusion": "success"}},
		comments:        9,
		closingIssues:   []map[string]any{{"number": 12, "state": "OPEN", "url": "u", "repository": map[string]any{"nameWithOwner": "org/repo"}}},
		signalsRejected: true,
	}
	pr := enrichOnePR(t, p, nil)
	if pr.Protection == nil || pr.Protection.ReviewDecision != ReviewDecisionChangesRequested {
		t.Fatalf("review decision lost to the rejected signals query: %+v", pr.Protection)
	}
	if got, ok := pr.BranchProtectionBlockReason(); !ok || got != "changes requested by @octo" {
		t.Errorf("reason = %q (ok=%v)", got, ok)
	}
	if pr.CommentCount != 0 || pr.LinkedIssues != nil {
		t.Errorf("display signals must be empty after fallback, got comments=%d linked=%v", pr.CommentCount, pr.LinkedIssues)
	}
	p.mu.Lock()
	calls := p.graphQLCalls
	p.mu.Unlock()
	if calls != 2 {
		t.Errorf("graphQL calls = %d, want exactly 2 (extended, then decision-only)", calls)
	}
}

// Stale drafts share the dashboard PR column but never go through
// EnrichCIStatus. EnrichReviewSignals gives them the same review/link
// signals from the one-per-repo query without any per-PR REST fetch — a
// draft is not a merge candidate, so its mergeability is never asked for.
func TestEnrichReviewSignals_StaleDraftsGetSignalsWithoutRESTCalls(t *testing.T) {
	p := &protectionServer{
		reviewDecision: "APPROVED",
		reviews:        []map[string]any{{"state": "APPROVED", "author": map[string]any{"login": "one"}}},
		comments:       2,
		reviewThreads:  1,
		closingIssues:  []map[string]any{{"number": 12, "state": "OPEN", "url": "u", "repository": map[string]any{"nameWithOwner": "org/repo"}}},
	}
	srv := p.start(t)
	c := newTestClient(t, srv, "org", []string{"repo"})
	prs := []PullRequest{
		{Repo: "org/repo", Number: 1, Draft: true, HeadSHA: "abc123", BaseRef: "v5"},
		{Repo: "org/repo", Number: 2, Draft: true, HeadSHA: "def456", BaseRef: "v5"},
	}
	c.EnrichReviewSignals(context.Background(), prs)
	if prs[0].Protection == nil || prs[0].Protection.ReviewDecision != ReviewDecisionApproved {
		t.Errorf("draft #1 Protection = %+v, want APPROVED", prs[0].Protection)
	}
	if prs[0].CommentCount != 2 || prs[0].ReviewThreadCount != 1 || len(prs[0].LinkedIssues) != 1 {
		t.Errorf("draft #1 signals = %d/%d/%v", prs[0].CommentCount, prs[0].ReviewThreadCount, prs[0].LinkedIssues)
	}
	if prs[0].Mergeable != MergeableUnknown || prs[0].CIStatus != "" {
		t.Errorf("draft #1 gained merge/CI state it must not have: mergeable=%q ci=%q", prs[0].Mergeable, prs[0].CIStatus)
	}
	p.mu.Lock()
	graphQL, rest := p.graphQLCalls, p.restCalls
	p.mu.Unlock()
	if graphQL != 1 || rest != 0 {
		t.Errorf("calls = %d graphQL / %d REST, want 1 / 0", graphQL, rest)
	}
}

// The wire names the dashboard reads. A rename here silently blanks the
// pill's badges, so the JSON shape is pinned alongside the JS that reads it.
func TestPullRequestReviewSignalsWireShape(t *testing.T) {
	pr := PullRequest{
		RequestedReviewers: []string{"alice"},
		RequestedTeams:     []string{"maintainers"},
		CommentCount:       3,
		ReviewThreadCount:  2,
		LinkedIssues:       []PRLinkedIssue{{Number: 12, Repo: "org/repo", State: "open", URL: "u"}},
	}
	raw, err := json.Marshal(pr)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"requested_reviewers", "requested_teams", "comment_count", "review_thread_count", "linked_issues"} {
		if _, ok := got[key]; !ok {
			t.Errorf("wire is missing %q: %s", key, raw)
		}
	}
	linked, _ := got["linked_issues"].([]any)
	if len(linked) != 1 {
		t.Fatalf("linked_issues = %v", got["linked_issues"])
	}
	first, _ := linked[0].(map[string]any)
	if first["number"] != float64(12) || first["repo"] != "org/repo" || first["state"] != "open" || first["url"] != "u" {
		t.Errorf("linked_issues[0] = %v", first)
	}
}
