package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// protectionServer is a fake forge that answers the three endpoints
// EnrichCIStatus touches: the single-PR GET (mergeability), the check-run
// listing, and the GraphQL query that carries review decisions.
type protectionServer struct {
	mu             sync.Mutex
	graphQLCalls   int
	checkRuns      []map[string]any
	mergeableState string
	reviewDecision string
	reviews        []map[string]any
	graphQLFails   bool
	// Triage signals riding the same query (#8968). signalsRejected makes the
	// forge refuse the extended query and answer only the decision-only one,
	// as a GHE lacking one of the added fields would. restCalls counts every
	// non-GraphQL request.
	comments        int
	reviewThreads   int
	closingIssues   []map[string]any
	signalsRejected bool
	restCalls       int
}

func (p *protectionServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/graphql") {
			p.mu.Lock()
			p.restCalls++
			p.mu.Unlock()
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/graphql"):
			p.mu.Lock()
			p.graphQLCalls++
			fails := p.graphQLFails
			rejectSignals := p.signalsRejected
			p.mu.Unlock()
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), "reviewDecision") {
				t.Errorf("graphql query did not ask for reviewDecision: %s", body)
			}
			if rejectSignals && strings.Contains(string(body), "closingIssuesReferences") {
				// GitHub validates the document and answers 200 with errors.
				_, _ = w.Write([]byte(`{"data":null,"errors":[{"message":"Field 'closingIssuesReferences' doesn't exist on type 'PullRequest'"}]}`))
				return
			}
			if fails {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"message":"no scope"}`))
				return
			}
			node := map[string]any{
				"number":                   1,
				"reviewDecision":           p.reviewDecision,
				"latestOpinionatedReviews": map[string]any{"nodes": p.reviews},
			}
			// Like the real forge, answer only the fields the document asked for.
			if strings.Contains(string(body), "closingIssuesReferences") {
				node["comments"] = map[string]any{"totalCount": p.comments}
				node["reviewThreads"] = map[string]any{"totalCount": p.reviewThreads}
				node["closingIssuesReferences"] = map[string]any{"nodes": p.closingIssues}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"repository": map[string]any{
						"pullRequests": map[string]any{
							"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
							"nodes":    []map[string]any{node},
						},
					},
				},
			})
		case strings.HasSuffix(r.URL.Path, "/pulls/1"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number":          1,
				"mergeable":       false,
				"mergeable_state": p.mergeableState,
			})
		case strings.Contains(r.URL.Path, "/check-runs"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"total_count": len(p.checkRuns),
				"check_runs":  p.checkRuns,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func enrichOnePR(t *testing.T, p *protectionServer, required map[string]bool) PullRequest {
	t.Helper()
	srv := p.start(t)
	c := newTestClient(t, srv, "org", []string{"repo"})
	if required != nil {
		c.SetRequiredChecks(required)
	}
	prs := []PullRequest{{Repo: "org/repo", Number: 1, HeadSHA: "abc123", BaseRef: "v4"}}
	c.EnrichCIStatus(context.Background(), prs)
	return prs[0]
}

// A red REQUIRED check is the commonest thing behind GitHub's "blocked", and
// the sweep already knows the required set from auto_merge.required_checks —
// no branch-protection API call is needed for this path at all.
func TestEnrichCIStatus_ProtectionNamesFailingRequiredCheck(t *testing.T) {
	p := &protectionServer{
		mergeableState: "blocked",
		reviewDecision: "APPROVED",
		checkRuns: []map[string]any{
			{"name": "build", "status": "completed", "conclusion": "failure"},
			{"name": "lint", "status": "completed", "conclusion": "success"},
		},
	}
	pr := enrichOnePR(t, p, map[string]bool{"build": true, "lint": true})
	if pr.Protection == nil {
		t.Fatal("Protection is nil; the blocked pill has nothing to say")
	}
	if !pr.Protection.RequiredChecksKnown {
		t.Error("RequiredChecksKnown = false, want true (the config set is installed)")
	}
	got, ok := pr.BranchProtectionBlockReason()
	if !ok || got != `required check "build" is failing` {
		t.Errorf("reason = %q (ok=%v), want `required check \"build\" is failing`", got, ok)
	}
}

// A required check that never produced a check run is "expected" in GitHub's
// vocabulary and invisible in every REST field the sweep reads.
func TestEnrichCIStatus_ProtectionNamesRequiredCheckThatNeverReported(t *testing.T) {
	p := &protectionServer{
		mergeableState: "blocked",
		reviewDecision: "APPROVED",
		checkRuns: []map[string]any{
			{"name": "build", "status": "completed", "conclusion": "success"},
		},
	}
	pr := enrichOnePR(t, p, map[string]bool{"build": true, "validate": true})
	if pr.Protection == nil {
		t.Fatal("Protection is nil")
	}
	got, ok := pr.BranchProtectionBlockReason()
	if !ok || got != `required check "validate" has not reported` {
		t.Errorf("reason = %q (ok=%v), want `required check \"validate\" has not reported`", got, ok)
	}
}

// The guard that keeps this honest: on a repository whose required contexts
// arrive as COMMIT STATUSES, not check runs, every required context is absent
// from the check-run listing. Claiming all of them "never reported" would be
// confidently wrong, so nothing at all is claimed.
func TestEnrichCIStatus_NoRequiredCheckObservedMakesNoMissingClaim(t *testing.T) {
	p := &protectionServer{
		mergeableState: "blocked",
		reviewDecision: "APPROVED",
		checkRuns: []map[string]any{
			{"name": "some-unrelated-check", "status": "completed", "conclusion": "success"},
		},
	}
	pr := enrichOnePR(t, p, map[string]bool{"validate": true, "dco": true})
	if pr.Protection == nil {
		t.Fatal("Protection is nil")
	}
	if len(pr.Protection.MissingRequiredChecks) != 0 {
		t.Errorf("MissingRequiredChecks = %v, want none: no required context was seen as a check run, so their absence proves nothing", pr.Protection.MissingRequiredChecks)
	}
	if got, ok := pr.BranchProtectionBlockReason(); ok {
		t.Errorf("reason = %q, want none (nothing supports a claim here)", got)
	}
}

// A check-run listing that could not be fetched must never be read as "the
// required check never reported".
func TestEnrichCIStatus_CheckRunFetchFailureMakesNoCheckClaim(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/graphql"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
				"repository": map[string]any{"pullRequests": map[string]any{
					"pageInfo": map[string]any{"hasNextPage": false},
					"nodes": []map[string]any{{
						"number": 1, "reviewDecision": "APPROVED",
						"latestOpinionatedReviews": map[string]any{"nodes": []map[string]any{}},
					}},
				}},
			}})
		case strings.HasSuffix(r.URL.Path, "/pulls/1"):
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 1, "mergeable_state": "blocked"})
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	c := newTestClient(t, srv, "org", []string{"repo"})
	c.SetRequiredChecks(map[string]bool{"validate": true})
	prs := []PullRequest{{Repo: "org/repo", Number: 1, HeadSHA: "abc123", BaseRef: "v4"}}
	c.EnrichCIStatus(context.Background(), prs)

	if prs[0].Protection != nil && prs[0].Protection.RequiredChecksKnown {
		t.Error("RequiredChecksKnown = true with no check-run listing; absence of evidence is not evidence")
	}
	if got, ok := prs[0].BranchProtectionBlockReason(); ok {
		t.Errorf("reason = %q, want none", got)
	}
}

// GitHub's own review verdict is the only signal that separates "nobody has
// reviewed" from "somebody said no", and REST exposes neither.
func TestEnrichCIStatus_ProtectionCarriesReviewDecision(t *testing.T) {
	t.Run("changes requested names the reviewer", func(t *testing.T) {
		p := &protectionServer{
			mergeableState: "blocked",
			reviewDecision: "CHANGES_REQUESTED",
			reviews: []map[string]any{
				{"state": "CHANGES_REQUESTED", "author": map[string]any{"login": "octo"}},
			},
			checkRuns: []map[string]any{{"name": "build", "status": "completed", "conclusion": "success"}},
		}
		pr := enrichOnePR(t, p, nil)
		if pr.Protection == nil || pr.Protection.ReviewDecision != ReviewDecisionChangesRequested {
			t.Fatalf("ReviewDecision not carried: %+v", pr.Protection)
		}
		got, ok := pr.BranchProtectionBlockReason()
		if !ok || got != "changes requested by @octo" {
			t.Errorf("reason = %q (ok=%v), want %q", got, ok, "changes requested by @octo")
		}
	})

	t.Run("review required counts the approvals given", func(t *testing.T) {
		p := &protectionServer{
			mergeableState: "blocked",
			reviewDecision: "REVIEW_REQUIRED",
			reviews: []map[string]any{
				{"state": "APPROVED", "author": map[string]any{"login": "one"}},
			},
			checkRuns: []map[string]any{{"name": "build", "status": "completed", "conclusion": "success"}},
		}
		pr := enrichOnePR(t, p, nil)
		if pr.Protection == nil {
			t.Fatal("Protection is nil")
		}
		if pr.Protection.ApprovalsGiven != 1 {
			t.Errorf("ApprovalsGiven = %d, want 1", pr.Protection.ApprovalsGiven)
		}
		got, ok := pr.BranchProtectionBlockReason()
		want := "an approving review is required by branch protection (1 given)"
		if !ok || got != want {
			t.Errorf("reason = %q (ok=%v), want %q", got, ok, want)
		}
	})
}

// A forge that will not answer the GraphQL query (no scope, an older GHE)
// must leave the review decision undetermined — never "approved" — and must
// not be asked again for every PR in the repository.
func TestEnrichCIStatus_ReviewDecisionFailureIsOnePerRepoAndNotApproved(t *testing.T) {
	p := &protectionServer{
		mergeableState: "blocked",
		graphQLFails:   true,
		checkRuns:      []map[string]any{{"name": "build", "status": "completed", "conclusion": "success"}},
	}
	srv := p.start(t)
	c := newTestClient(t, srv, "org", []string{"repo"})
	prs := []PullRequest{
		{Repo: "org/repo", Number: 1, HeadSHA: "abc123", BaseRef: "v4"},
		{Repo: "org/repo", Number: 1, HeadSHA: "abc123", BaseRef: "v4"},
		{Repo: "org/repo", Number: 1, HeadSHA: "abc123", BaseRef: "v4"},
	}
	c.EnrichCIStatus(context.Background(), prs)

	p.mu.Lock()
	calls := p.graphQLCalls
	p.mu.Unlock()
	if calls != 1 {
		t.Errorf("graphQL called %d times for one repository, want 1: a per-PR query is unaffordable at a 400-PR queue", calls)
	}
	for i := range prs {
		if prs[i].Protection != nil && prs[i].Protection.ReviewDecision == ReviewDecisionApproved {
			t.Errorf("pr %d: an unanswerable query became %q", i, ReviewDecisionApproved)
		}
	}
}

// One GraphQL query serves every PR in the repository.
func TestEnrichCIStatus_ReviewDecisionIsOneQueryPerRepo(t *testing.T) {
	p := &protectionServer{
		mergeableState: "blocked",
		reviewDecision: "REVIEW_REQUIRED",
		checkRuns:      []map[string]any{{"name": "build", "status": "completed", "conclusion": "success"}},
	}
	srv := p.start(t)
	c := newTestClient(t, srv, "org", []string{"repo"})
	prs := make([]PullRequest, 5)
	for i := range prs {
		prs[i] = PullRequest{Repo: "org/repo", Number: 1, HeadSHA: "abc123", BaseRef: "v4"}
	}
	c.EnrichCIStatus(context.Background(), prs)

	p.mu.Lock()
	calls := p.graphQLCalls
	p.mu.Unlock()
	if calls != 1 {
		t.Errorf("graphQL called %d times for 5 PRs in one repository, want 1", calls)
	}
}
