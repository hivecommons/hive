package dashboard

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

func TestStageTrackerPRSideClosingReferences(t *testing.T) {
	now := time.Date(2026, 10, 21, 19, 0, 0, 0, time.UTC)
	s, store := stageTrackerFixture(t, now)
	s.status.Repos[0].OpenPrs = []any{FrontendPR{PullRequest: github.PullRequest{
		Number: 120, Title: "PR-side link", Labels: []string{"stage/050-review-pending"},
		LinkedIssues: []github.PRLinkedIssue{
			{Number: 8, Repo: "acme/console", State: "OPEN"},
			{Number: 8, Repo: "acme/console", State: "open"}, // duplicate
			{Number: 9, State: "CLOSED", Title: "Already closed", URL: "https://github.com/acme/console/issues/9"},
			{Number: 10, State: "open", Title: "Not listed"},
			{Number: 7, Repo: "other/console", State: "open"},
			{Number: 120, State: "open"}, // self-link
			{Number: 0},
		},
	}}}
	resp := s.buildStageTracker(s.status, store, now)
	// Assert the wire payload, not just the internal representation.
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		t.Fatal(err)
	}
	pr := trackerItem(t, resp, "pr", 120)
	// Duplicate, cross-repo, self and zero references are dropped on the wire.
	if pr.NestedUnder != 8 || len(pr.LinkedIssues) != 3 {
		t.Fatalf("PR links = %+v", pr)
	}
	li := pr.LinkedIssues[0]
	if !li.Listed || li.State != "open" || li.Title != "Flaky test" || li.URL == "" {
		t.Fatalf("listed issue = %+v", li)
	}
	if li := pr.LinkedIssues[1]; li.Listed || li.State != "closed" || li.Title != "Already closed" || li.URL == "" {
		t.Fatalf("closed issue = %+v", li)
	}
	if li := pr.LinkedIssues[2]; li.Listed || li.Title != "Not listed" {
		t.Fatalf("unlisted issue = %+v", li)
	}
	issue := trackerItem(t, resp, "issue", 8)
	if len(issue.LinkedPRs) != 1 || issue.LinkedPRs[0].Number != 120 || issue.LinkedPRs[0].Title != pr.Title || issue.LinkedPRs[0].Stage != pr.Stage {
		t.Fatalf("PR-side pair = %+v", issue.LinkedPRs)
	}
}

func TestStageTrackerPRSidePairingKeepsPrimaryAndRepoBoundary(t *testing.T) {
	items := []StageTrackerItem{
		{Repo: "a/b", Kind: "issue", Number: 7, LinkedPR: 102},
		{Repo: "a/b", Kind: "issue", Number: 8},
		{Repo: "other/b", Kind: "issue", Number: 6},
		{Repo: "a/b", Kind: "pr", Number: 101, LinkedIssues: []StageTrackerLinkedIssue{{Number: 8, State: "open"}, {Number: 7, State: "open"}, {Number: 6, State: "open"}}},
		{Repo: "a/b", Kind: "pr", Number: 102},
	}
	stageTrackerResolveLinkedPRs(items)
	if items[3].NestedUnder != 7 || len(items[0].LinkedPRs) != 2 || items[0].LinkedPRs[0].Number != 102 || items[0].LinkedPRs[1].Number != 101 {
		t.Fatalf("primary/order = %+v", items)
	}
	if len(items[2].LinkedPRs) != 0 || items[3].LinkedIssues[2].Listed {
		t.Fatalf("paired across repos: %+v", items)
	}
}

// A sidelined PR (draft / exempt label) reaches the tracker as a
// RepoWorkPR; its closing references must pair exactly like an actionable
// PR's, or every draft with a valid `Fixes #N` renders "no linked issue".
func TestStageTrackerSidelinedPRCarriesClosingReferences(t *testing.T) {
	now := time.Date(2026, 10, 21, 19, 0, 0, 0, time.UTC)
	s, store := stageTrackerFixture(t, now)
	s.status.Repos[0].NonActionablePrs = []any{
		github.RepoWorkPR{Repo: "acme/console", Number: 130, Title: "Draft fix", Draft: true, Bucket: "draft", Reason: "Draft",
			LinkedIssues: []github.PRLinkedIssue{{Number: 8, Repo: "acme/console", State: "open"}}},
		&github.RepoWorkPR{Repo: "acme/console", Number: 131, Title: "Exempt fix", Bucket: "filtered", Reason: "Exempt label",
			LinkedIssues: []github.PRLinkedIssue{{Number: 10, State: "open", Title: "Not listed", URL: "https://github.com/acme/console/issues/10"}}},
	}
	resp := s.buildStageTracker(s.status, store, now)
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		t.Fatal(err)
	}
	draft := trackerItem(t, resp, "pr", 130)
	if draft.NestedUnder != 8 || len(draft.LinkedIssues) != 1 || !draft.LinkedIssues[0].Listed || draft.LinkedIssues[0].Title != "Flaky test" {
		t.Fatalf("draft links = %+v", draft)
	}
	issue := trackerItem(t, resp, "issue", 8)
	found := false
	for _, lp := range issue.LinkedPRs {
		found = found || lp.Number == 130
	}
	if !found {
		t.Fatalf("issue 8 does not nest the draft: %+v", issue.LinkedPRs)
	}
	exempt := trackerItem(t, resp, "pr", 131)
	if exempt.NestedUnder != 0 || len(exempt.LinkedIssues) != 1 || exempt.LinkedIssues[0].Listed || exempt.LinkedIssues[0].State != "open" || exempt.LinkedIssues[0].Title != "Not listed" {
		t.Fatalf("exempt links = %+v", exempt)
	}
}
