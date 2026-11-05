package github

import "testing"

// projectbluefin/actions#543 (a data-loss bug in reusable-sync-branches.yml)
// received the repo's dedupe checklist as a "Folded finding" on 10-24: the
// checklist cited the same workflow, and three open bot issues on it put the
// request over the same-component fold. A dedupe checklist is filed on its
// own, like projectbluefin/utah#1085.
func TestDedupeChecklistIsNeverPathFolded(t *testing.T) {
	siblings := []openTwin{
		{number: 543, author: testBot, title: "[ci-maintainer] reusable-sync-branches.yml force-resets target_branch", body: "github/workflows/reusable-sync-branches.yml"},
		{number: 593, author: testBot, title: "[reviewer] inline inputs interpolation", body: "github/workflows/reusable-sync-branches.yml line 40"},
		{number: 678, author: testBot, title: "[scanner] sync step lacks a lock", body: "github/workflows/reusable-sync-branches.yml line 77"},
	}
	checklist := "<!-- hive:dedupe-checklist repo=o/r -->\nDecision required: no\n\n- [ ] PR #544 and PR #675 both rewrite github/workflows/reusable-sync-branches.yml"

	m := &consolidationMock{}
	srv := consolidationMockServer(t, siblings, m)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	c.appBotLogin = testBot
	_, res := fileRequest(t, c, withIssueDir(t), IssueRequest{
		Repo: "o/r", Agent: "dedupe",
		Title: "[dedupe] triage: duplicate and overlapping open issues/PRs to consolidate",
		Body:  checklist,
	})
	if m.created != 1 || len(m.commentedOn) != 0 {
		t.Fatalf("created=%d comments=%v, want the checklist filed on its own", m.created, m.commentedOn)
	}
	if !res.OK || res.Consolidated {
		t.Fatalf("result = %+v, want a plain create", res)
	}

	// Control: the same body without the marker folds, so the marker is what
	// keeps it out.
	m = &consolidationMock{}
	srv2 := consolidationMockServer(t, siblings, m)
	defer srv2.Close()
	c = issueTestClient(t, srv2.URL)
	c.appBotLogin = testBot
	_, res = fileRequest(t, c, withIssueDir(t), IssueRequest{
		Repo: "o/r", Agent: "dedupe",
		Title: "[dedupe] triage: duplicate and overlapping open issues/PRs to consolidate",
		Body:  "Decision required: no\n\n- [ ] PR #544 and PR #675 both rewrite github/workflows/reusable-sync-branches.yml",
	})
	if m.created != 0 || !res.Consolidated {
		t.Fatalf("control: created=%d result=%+v, want a fold without the marker", m.created, res)
	}
}
