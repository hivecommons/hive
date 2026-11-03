package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// hivecommons/hive#12144: PR #12025 on branch `feat/12005-push` said
// `Refs #12005 (partial)`. The branch-name heuristic graded that as a strong
// claim, which the link sweep then turned into a GitHub closing link —
// arming auto-close on merge for a PR whose author had explicitly declined
// to close the issue. An explicit non-closing reference to the branch-name
// issue must grade the claim as Reference.
func TestClaimsFromPR_ExplicitRefsOverrulesBranchName(t *testing.T) {
	now := time.Now()
	build := func(body, branch string) *gh.PullRequest {
		return &gh.PullRequest{
			Number:  gh.Ptr(12025),
			Title:   gh.Ptr("✨ eval budget tiers"),
			Body:    gh.Ptr(body),
			State:   gh.Ptr("open"),
			HTMLURL: gh.Ptr("https://github.com/hivecommons/hive/pull/12025"),
			User:    &gh.User{Login: gh.Ptr("mrbobbytables")},
			Head:    &gh.PullRequestBranch{Ref: gh.Ptr(branch), SHA: gh.Ptr("ee10a12")},
		}
	}
	identity := HiveIdentity{AIAuthor: "hive-bot"}

	t.Run("Refs to the branch issue is a reference claim", func(t *testing.T) {
		claims := claimsFromPR(build("Part B.2 of #12005.\n\nRefs #12005 (partial; A and B.3 remain)", "feat/12005-push"), "hivecommons/hive", identity, now)
		if len(claims) != 1 {
			t.Fatalf("claims = %+v, want exactly one", claims)
		}
		c := claims[0]
		if c.Issue != 12005 || !c.Reference || c.Closing {
			t.Fatalf("claim = %+v, want issue 12005 graded Reference (not Closing)", c)
		}
	})
	t.Run("branch issue with no reference stays a strong claim", func(t *testing.T) {
		claims := claimsFromPR(build("Splits eval steps into tiers.", "feat/12005-push"), "hivecommons/hive", identity, now)
		if len(claims) != 1 || claims[0].Issue != 12005 || claims[0].Reference || claims[0].Closing {
			t.Fatalf("claims = %+v, want one strong branch-name claim on 12005", claims)
		}
	})
	t.Run("Closes still wins over both", func(t *testing.T) {
		claims := claimsFromPR(build("Closes #12005\n\nRefs #12005", "feat/12005-push"), "hivecommons/hive", identity, now)
		if len(claims) != 1 || claims[0].Issue != 12005 || claims[0].Reference || !claims[0].Closing {
			t.Fatalf("claims = %+v, want one closing claim on 12005", claims)
		}
	})
	t.Run("Refs to a different issue keeps the branch claim and adds the reference", func(t *testing.T) {
		claims := claimsFromPR(build("Refs #777", "feat/12005-push"), "hivecommons/hive", identity, now)
		got := map[int]IssueClaim{}
		for _, c := range claims {
			got[c.Issue] = c
		}
		if len(got) != 2 || got[12005].Reference || !got[777].Reference {
			t.Fatalf("claims = %+v, want strong 12005 and reference 777", claims)
		}
	})
}

// retractLinkFixture serves the GraphQL lookup RetractHiveClosingIssueLink
// issues and records removeCloseIssueReferences mutations. Only the fields
// the code reads are populated.
type retractLinkFixture struct {
	linked      []int
	connectedBy map[int][]string // issue → connect actors, oldest first
	mutationErr string
	lookups     atomic.Int32
	removals    atomic.Int32
	adds        atomic.Int32
}

func (f *retractLinkFixture) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !strings.HasSuffix(r.URL.Path, "/graphql") {
			// Label writes from the sweep.
			_, _ = w.Write([]byte(`{"name":"ok"}`))
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var req struct {
			Query string `json:"query"`
		}
		_ = json.Unmarshal(raw, &req)
		switch {
		case strings.Contains(req.Query, "removeCloseIssueReferences"):
			f.removals.Add(1)
			if f.mutationErr != "" {
				_ = json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]any{{"message": f.mutationErr}}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"removeCloseIssueReferences": map[string]any{"issue": map[string]any{"number": 12005}}}})
		case strings.Contains(req.Query, "addCloseIssueReferences"):
			f.adds.Add(1)
			t.Errorf("unexpected addCloseIssueReferences for a reference claim")
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{}})
		default:
			f.lookups.Add(1)
			nodes := []map[string]any{}
			for _, n := range f.linked {
				nodes = append(nodes, map[string]any{"number": n, "repository": map[string]any{"nameWithOwner": "hivecommons/hive"}})
			}
			events := []map[string]any{}
			for issue, actors := range f.connectedBy {
				for _, a := range actors {
					events = append(events, map[string]any{"actor": map[string]any{"login": a}, "subject": map[string]any{"number": issue}})
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
				"repository": map[string]any{
					"pullRequest": map[string]any{
						"id":                      "PR_12025",
						"state":                   "OPEN",
						"baseRefName":             "v5",
						"closingIssuesReferences": map[string]any{"nodes": nodes},
						"timelineItems":           map[string]any{"nodes": events},
					},
					"issue": map[string]any{"id": "I_12005", "state": "OPEN"},
				},
			}})
		}
	}
}

func newRetractLinkClient(t *testing.T, f *retractLinkFixture) *Client {
	t.Helper()
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	c := NewClientForTest(srv.URL, "hivecommons", []string{"hive"}, testLogger())
	c.SetAppBotLogin("hivecommons-hive[bot]")
	return c
}

func TestRetractHiveClosingIssueLink(t *testing.T) {
	t.Run("removes a link the hive App made", func(t *testing.T) {
		f := &retractLinkFixture{linked: []int{12005}, connectedBy: map[int][]string{12005: {"hivecommons-hive"}}}
		c := newRetractLinkClient(t, f)
		res, err := c.RetractHiveClosingIssueLink(context.Background(), "hivecommons/hive", 12025, 12005)
		if err != nil {
			t.Fatal(err)
		}
		if !res.Retracted || res.LinkedBy != "hivecommons-hive" || f.removals.Load() != 1 {
			t.Fatalf("res = %+v removals = %d, want retracted by hivecommons-hive", res, f.removals.Load())
		}
		// Memoised: a second call costs nothing.
		if _, err := c.RetractHiveClosingIssueLink(context.Background(), "hivecommons/hive", 12025, 12005); err != nil {
			t.Fatal(err)
		}
		if f.lookups.Load() != 1 || f.removals.Load() != 1 {
			t.Fatalf("after second call lookups=%d removals=%d, want 1/1", f.lookups.Load(), f.removals.Load())
		}
	})
	t.Run("leaves a link a person made", func(t *testing.T) {
		f := &retractLinkFixture{linked: []int{12005}, connectedBy: map[int][]string{12005: {"hivecommons-hive", "clubanderson"}}}
		c := newRetractLinkClient(t, f)
		res, err := c.RetractHiveClosingIssueLink(context.Background(), "hivecommons/hive", 12025, 12005)
		if err != nil {
			t.Fatal(err)
		}
		if res.Retracted || res.Skipped != "not-hive-linked" || res.LinkedBy != "clubanderson" || f.removals.Load() != 0 {
			t.Fatalf("res = %+v removals = %d, want not-hive-linked by clubanderson", res, f.removals.Load())
		}
	})
	t.Run("no App login means no retraction", func(t *testing.T) {
		f := &retractLinkFixture{linked: []int{12005}, connectedBy: map[int][]string{12005: {"hivecommons-hive"}}}
		c := newRetractLinkClient(t, f)
		c.SetAppBotLogin("")
		res, err := c.RetractHiveClosingIssueLink(context.Background(), "hivecommons/hive", 12025, 12005)
		if err != nil {
			t.Fatal(err)
		}
		if res.Retracted || res.Skipped != "not-hive-linked" || f.removals.Load() != 0 {
			t.Fatalf("res = %+v, want fail-closed skip without an App login", res)
		}
	})
	t.Run("not linked is a memoised no-op", func(t *testing.T) {
		f := &retractLinkFixture{}
		c := newRetractLinkClient(t, f)
		res, err := c.RetractHiveClosingIssueLink(context.Background(), "hivecommons/hive", 12025, 12005)
		if err != nil {
			t.Fatal(err)
		}
		if res.Retracted || res.Skipped != "not-linked" {
			t.Fatalf("res = %+v, want not-linked", res)
		}
		if _, err := c.RetractHiveClosingIssueLink(context.Background(), "hivecommons/hive", 12025, 12005); err != nil {
			t.Fatal(err)
		}
		if f.lookups.Load() != 1 {
			t.Fatalf("lookups = %d, want 1 (memoised)", f.lookups.Load())
		}
	})
	t.Run("mutation error is not memoised", func(t *testing.T) {
		f := &retractLinkFixture{linked: []int{12005}, connectedBy: map[int][]string{12005: {"hivecommons-hive"}}, mutationErr: "boom"}
		c := newRetractLinkClient(t, f)
		if _, err := c.RetractHiveClosingIssueLink(context.Background(), "hivecommons/hive", 12025, 12005); err == nil {
			t.Fatal("expected mutation error")
		}
		f.mutationErr = ""
		res, err := c.RetractHiveClosingIssueLink(context.Background(), "hivecommons/hive", 12025, 12005)
		if err != nil || !res.Retracted {
			t.Fatalf("retry: res = %+v err = %v, want retracted", res, err)
		}
	})
}

// The sweep never adds a closing link for a reference claim and retracts one
// the hive made earlier (the pair had been linked while the claim was graded
// strong). The ensure memo for the pair is forgotten on retraction so a body
// edited back to `Closes #N` links again.
func TestSyncIssuePRClaimLabels_ReferenceClaimRetractsHiveLink(t *testing.T) {
	f := &retractLinkFixture{linked: []int{12005}, connectedBy: map[int][]string{12005: {"hivecommons-hive"}}}
	c := newRetractLinkClient(t, f)
	c.closingLinks.mark(c.closingLinks.key("hivecommons", "hive", 12005, 12025))
	now := time.Now()
	ledger := NewClaimLedger("", testLogger())
	ledger.Reconcile([]IssueClaim{
		{Repo: "hivecommons/hive", Issue: 12005, PRNumber: 12025, PRRepo: "hivecommons/hive", PRState: "open", Reference: true, ExternalAuthor: true, ObservedAt: now, FirstObservedAt: now},
	}, true)
	result := &ActionableResult{}
	result.Issues.Items = []Issue{{Repo: "hivecommons/hive", Number: 12005}}
	SyncIssuePRClaimLabels(context.Background(), c, result, ledger, testLogger())
	if f.removals.Load() != 1 || f.adds.Load() != 0 {
		t.Fatalf("removals=%d adds=%d, want 1/0", f.removals.Load(), f.adds.Load())
	}
	if c.closingLinks.has(c.closingLinks.key("hivecommons", "hive", 12005, 12025)) {
		t.Fatal("ensure memo for the pair should be forgotten after retraction")
	}
	issue := result.Issues.Items[0]
	if !HasCoveredByPRLabel(issue.Labels) {
		t.Fatalf("labels = %v, want covered-by-PR kept on the reference claim", issue.Labels)
	}
	if len(issue.LinkedPRs) != 1 || !issue.LinkedPRs[0].Reference || issue.LinkedPRs[0].Closing {
		t.Fatalf("linked_prs = %+v, want one reference (non-closing) link", issue.LinkedPRs)
	}
}

// A suppressed (external strong claim) issue keeps its claim evidence in the
// claimed_by_pr work-detail entry so the repo card can render the glyph.
func TestFilterClaimedIssues_SuppressedIssueKeepsLinkedPRs(t *testing.T) {
	now := time.Now()
	ledger := NewClaimLedger("", testLogger())
	ledger.Reconcile([]IssueClaim{
		{Repo: "hivecommons/hive", Issue: 12002, PRNumber: 12045, PRRepo: "hivecommons/hive", PRURL: "https://github.com/hivecommons/hive/pull/12045", PRState: "open", ExternalAuthor: true, ObservedAt: now, FirstObservedAt: now},
	}, true)
	result := &ActionableResult{}
	result.Issues.Items = []Issue{{Repo: "hivecommons/hive", Number: 12002, Title: "needs-human semantics"}}
	if got := FilterClaimedIssues(result, ledger, nil, testLogger()); got != 1 {
		t.Fatalf("suppressed = %d, want 1", got)
	}
	details := result.WorkDetailsByRepo["hivecommons/hive"].Issues
	if len(details) != 1 || details[0].Bucket != "claimed_by_pr" {
		t.Fatalf("work details = %+v, want one claimed_by_pr entry", details)
	}
	d := details[0]
	if len(d.LinkedPRs) != 1 || d.LinkedPRs[0].Number != 12045 || d.LinkedPRs[0].URL == "" {
		t.Fatalf("linked_prs = %+v, want PR 12045 with URL", d.LinkedPRs)
	}
	if d.ClaimContext == nil || d.ClaimContext.PRNumber != 12045 {
		t.Fatalf("claim_context = %+v, want PR 12045", d.ClaimContext)
	}
	raw, _ := json.Marshal(d)
	if !strings.Contains(string(raw), `"linked_prs"`) || !strings.Contains(string(raw), `"claim_context"`) {
		t.Fatalf("payload %s lacks linked_prs/claim_context", raw)
	}
}
