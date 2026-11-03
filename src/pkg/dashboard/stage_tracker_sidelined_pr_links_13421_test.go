package dashboard

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

// A sidelined PR (draft / exempt label) reaches the tracker as a
// RepoWorkPR; its closing references must pair exactly like an actionable
// PR's, or every draft with a valid `Fixes #N` renders "no linked issue".
func TestStageTrackerSidelinedPRCarriesClosingReferences(t *testing.T) {
	now := time.Date(2026, 10, 21, 19, 0, 0, 0, time.UTC)
	s, store := stageTrackerFixture(t, now)
	s.status.Repos[0].NonActionablePrs = []any{
		github.RepoWorkPR{Repo: "acme/console", Number: 130, Title: "Draft fix", Draft: true, Bucket: "draft", Reason: "Draft",
			LinkedIssues: []github.PRLinkedIssue{{Number: 8, Repo: "acme/console", State: "open"}}},
		// Enrichment stamps every closing reference GitHub reports, including
		// a cross-repository `Fixes other/repo#N`; the tracker's same-repo
		// guard must drop it at render time, as it does for actionable PRs.
		&github.RepoWorkPR{Repo: "acme/console", Number: 131, Title: "Exempt fix", Bucket: "filtered", Reason: "Exempt label",
			LinkedIssues: []github.PRLinkedIssue{
				{Number: 10, State: "open", Title: "Not listed", URL: "https://github.com/acme/console/issues/10"},
				{Number: 44, Repo: "acme/other", State: "open", Title: "Cross-repo", URL: "https://github.com/acme/other/issues/44"},
			}},
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
	if exempt.LinkedIssues[0].Number != 10 {
		t.Fatalf("cross-repo closing reference survived the same-repo guard: %+v", exempt.LinkedIssues)
	}
}
