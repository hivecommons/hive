package dashboard

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

// A PR whose body opens with `Refs #N` — or whose title ends in
// `(#N, part 1)` — has no closingIssuesReferences, and the tracker drew it
// with "no linked issue" (hivecommons/hive#13544, #13548). The enumeration's
// soft references must pair the PR with its issue exactly as a closing link
// does, tagged so the UI can tell the two apart, and both parts of a split
// must land on the one issue row.
func TestStageTrackerSoftReferenceLinks(t *testing.T) {
	now := time.Date(2026, 10, 22, 15, 0, 0, 0, time.UTC)
	s, store := stageTrackerFixture(t, now)
	repo := &s.status.Repos[0]
	repo.ActionableIssues = append(repo.ActionableIssues,
		github.Issue{Number: 13542, Title: "review: split the reviewer", Author: "pat", AuthorIsHuman: true, Labels: []string{"stage/030-in-progress"}})
	ref := func(n int, via string) github.PRLinkedIssue {
		return github.PRLinkedIssue{Number: n, Repo: "acme/console", Via: via}
	}
	repo.OpenPrs = append(repo.OpenPrs,
		// Part 1: body `Refs #13542 (part 1 of 2)`.
		FrontendPR{PullRequest: github.PullRequest{Number: 13544, Title: "✨ review: model heads per perspective (#13542, part 1)", Author: "pat", Labels: []string{"stage/050-review-pending"},
			ReferencedIssues: []github.PRLinkedIssue{ref(13542, github.PRLinkViaRefs)}}},
		// A closing keyword hive read itself (release-line PR GitHub never
		// linked) is a closing link: no Via.
		FrontendPR{PullRequest: github.PullRequest{Number: 13550, Title: "fix: thing", Author: "pat", Labels: []string{"stage/050-review-pending"},
			ReferencedIssues: []github.PRLinkedIssue{ref(13, github.PRLinkViaFixes)}}},
		// GitHub's closing link and a soft reference to the same issue:
		// the closing link wins and the number appears once.
		FrontendPR{PullRequest: github.PullRequest{Number: 13551, Title: "fix: both", Author: "pat", Labels: []string{"stage/050-review-pending"},
			LinkedIssues:     []github.PRLinkedIssue{{Number: 7, State: "open"}},
			ReferencedIssues: []github.PRLinkedIssue{ref(7, github.PRLinkViaRefs), ref(4, github.PRLinkViaMention), ref(8, github.PRLinkViaMention), ref(6, github.PRLinkViaRefs), {Number: 3, Repo: "acme/other", Via: github.PRLinkViaRefs}}}},
		// A squash title's `(#102)` names an open PR, not an issue: pruned.
		FrontendPR{PullRequest: github.PullRequest{Number: 13552, Title: "backport: stabilise (#102)", Author: "pat", Labels: []string{"stage/050-review-pending"},
			ReferencedIssues: []github.PRLinkedIssue{ref(102, github.PRLinkViaTitle)}}},
	)
	// Part 2: title `(#13542, part 2)` only, arriving as a sidelined draft.
	repo.NonActionablePrs = []any{
		github.RepoWorkPR{Repo: "acme/console", Number: 13548, Title: "✨ review: decision agent reconciles review heads (#13542, part 2)", Draft: true, Bucket: "draft", Reason: "Draft",
			ReferencedIssues: []github.PRLinkedIssue{ref(13542, github.PRLinkViaTitle)}},
	}
	resp := s.buildStageTracker(s.status, store, now)
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		pr  int
		via string
	}{{13544, github.PRLinkViaRefs}, {13548, github.PRLinkViaTitle}} {
		it := trackerItem(t, resp, "pr", tc.pr)
		if it.NestedUnder != 13542 || len(it.LinkedIssues) != 1 {
			t.Fatalf("PR #%d links = %+v (nested under %d)", tc.pr, it.LinkedIssues, it.NestedUnder)
		}
		li := it.LinkedIssues[0]
		if li.Number != 13542 || !li.Listed || li.State != "open" || li.Title != "review: split the reviewer" || li.Via != tc.via {
			t.Fatalf("PR #%d linked issue = %+v, want listed open #13542 via %q", tc.pr, li, tc.via)
		}
		if li.URL != "https://github.com/acme/console/issues/13542" {
			t.Fatalf("PR #%d linked issue URL = %q", tc.pr, li.URL)
		}
	}
	issue := trackerItem(t, resp, "issue", 13542)
	got := map[int]bool{}
	for _, lp := range issue.LinkedPRs {
		got[lp.Number] = true
	}
	if !got[13544] || !got[13548] || len(issue.LinkedPRs) != 2 {
		t.Fatalf("issue 13542 covering PRs = %+v, want both parts", issue.LinkedPRs)
	}

	fixes := trackerItem(t, resp, "pr", 13550)
	if fixes.NestedUnder != 13 || len(fixes.LinkedIssues) != 1 || fixes.LinkedIssues[0].Via != "" || fixes.LinkedIssues[0].Title != "Agents stand down" {
		t.Fatalf("hive-read closing keyword = %+v", fixes.LinkedIssues)
	}

	both := trackerItem(t, resp, "pr", 13551)
	// Closing #7 first (no Via, the duplicate soft #7 folded in), the soft
	// `Refs #6` to an unlisted issue kept as-is, the mention of listed #8
	// kept, the mention of unlisted #4 and the cross-repo ref dropped.
	if len(both.LinkedIssues) != 3 {
		t.Fatalf("mixed links = %+v", both.LinkedIssues)
	}
	if li := both.LinkedIssues[0]; li.Number != 7 || li.Via != "" || !li.Listed {
		t.Fatalf("closing link should lead and win: %+v", both.LinkedIssues)
	}
	if li := both.LinkedIssues[1]; li.Number != 8 || li.Via != github.PRLinkViaMention || !li.Listed {
		t.Fatalf("listed mention should survive: %+v", both.LinkedIssues)
	}
	if li := both.LinkedIssues[2]; li.Number != 6 || li.Via != github.PRLinkViaRefs || li.Listed || li.State != "" {
		t.Fatalf("unlisted refs link should survive untitled: %+v", both.LinkedIssues)
	}
	if both.NestedUnder != 7 {
		t.Fatalf("nested under %d, want the closing issue 7", both.NestedUnder)
	}

	squash := trackerItem(t, resp, "pr", 13552)
	if len(squash.LinkedIssues) != 0 || squash.NestedUnder != 0 {
		t.Fatalf("a title reference to an open PR must not link: %+v", squash.LinkedIssues)
	}
	// PR 102 keeps only its own pairing (issue 7 mirrors it in the fixture):
	// being named in another PR's title adds nothing to it.
	for _, li := range trackerItem(t, resp, "pr", 102).LinkedIssues {
		if li.Number != 7 || li.Via != "" {
			t.Fatalf("PR 102 gained a link from being named in a title: %+v", li)
		}
	}
}

func TestStageTrackerPRLinkedIssuesOrdersClosingFirst(t *testing.T) {
	pr := github.PullRequest{Repo: "acme/console", Number: 50,
		LinkedIssues:     []github.PRLinkedIssue{{Number: 9, Title: "Nine", State: "OPEN"}},
		ReferencedIssues: []github.PRLinkedIssue{{Number: 11, Repo: "acme/console", Via: github.PRLinkViaTitle}, {Number: 9, Repo: "acme/console", Via: github.PRLinkViaRefs}, {Number: 50, Repo: "acme/console", Via: github.PRLinkViaRefs}, {Number: 12, Repo: "acme/console", Via: github.PRLinkViaFixes}}}
	got := stageTrackerPRLinkedIssues(pr, "https://github.com")
	if len(got) != 3 || got[0].Number != 9 || got[0].Via != "" || got[0].State != "open" || got[1].Number != 11 || got[1].Via != github.PRLinkViaTitle || got[2].Number != 12 || got[2].Via != "" {
		t.Fatalf("got %+v", got)
	}
	if !stageTrackerLinkSoft(got[1]) || stageTrackerLinkSoft(got[2]) || stageTrackerLinkSoft(got[0]) {
		t.Fatalf("softness: %+v", got)
	}
}
