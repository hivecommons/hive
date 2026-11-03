package evidence

import (
	"testing"
	"time"
)

func TestLooksLikeRevert(t *testing.T) {
	for _, tc := range []struct {
		title, body string
		want        bool
	}{
		{`Revert "✨ Add the thing"`, "", true},
		{`🔙 Revert "Add the thing" (#12)`, "", true},
		{"fix: revert the rate limiter", "", false},
		{"Tidy up", "Reverts hivecommons/hive#1234", true},
		{"Tidy up", "This reverts commit 0123456789abcdef0123456789abcdef01234567.", true},
		{"Tidy up", "reverts commit abcdef1", true},
		{"Tidy up", "reverts commit abcde", false},
		{"Nothing to see", "Fixes #12", false},
	} {
		if got := LooksLikeRevert(tc.title, tc.body); got != tc.want {
			t.Errorf("LooksLikeRevert(%q, %q) = %v, want %v", tc.title, tc.body, got, tc.want)
		}
	}
}

func TestParseRevertTargets(t *testing.T) {
	got := ParseRevertTargets("a/b", 50, `Revert "Add thing" (#40)`,
		"Reverts a/b#40\nThis reverts commit ABCDEF1234567.\nThis reverts commit abcdef1234567.\nreverts commit abc\nReverts other/repo#9 and #50")
	if len(got.PRs) != 1 || got.PRs[0] != 40 {
		t.Errorf("PRs = %v, want [40] (other-repo and self refs dropped)", got.PRs)
	}
	if len(got.SHAs) != 1 || got.SHAs[0] != "abcdef1234567" {
		t.Errorf("SHAs = %v, want one lower-cased 13-char prefix", got.SHAs)
	}
	if empty := ParseRevertTargets("a/b", 1, "Add thing", "Fixes #3"); len(empty.PRs)+len(empty.SHAs) != 0 {
		t.Errorf("non-revert text must yield nothing: %+v", empty)
	}
	// GitHub's generated revert title quotes the squash-merge title, which
	// ends with the original PR number.
	if q := ParseRevertTargets("a/b", 51, `Revert "✨ Add thing (#40)"`, ""); len(q.PRs) != 1 || q.PRs[0] != 40 {
		t.Errorf("quoted title PRs = %v, want [40]", q.PRs)
	}
	if q := ParseRevertTargets("a/b", 40, `Revert "Add thing (#40)"`, ""); len(q.PRs) != 0 {
		t.Errorf("self reference must be dropped: %v", q.PRs)
	}
}

func TestRevertsMergedBundle(t *testing.T) {
	merged := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	b := &Bundle{HeadSHA: "1111111aaaaaaa", Merge: &MergeEvent{At: merged, SHA: "2222222bbbbbbb"}}
	cases := []struct {
		name string
		b    *Bundle
		sha  string
		at   time.Time
		want bool
	}{
		{"merge sha prefix", b, "2222222", merged.Add(24 * time.Hour), true},
		{"head sha full upper", b, "1111111AAAAAAA", merged.Add(24 * time.Hour), true},
		{"short prefix", b, "222222", merged.Add(24 * time.Hour), false},
		{"outside window", b, "2222222", merged.Add(LinkWindow + time.Hour), false},
		{"before merge", b, "2222222", merged.Add(-time.Hour), false},
		{"unmerged", &Bundle{HeadSHA: "2222222bbbbbbb"}, "2222222", merged, false},
		{"nil", nil, "2222222", merged, false},
	}
	for _, tc := range cases {
		if got := RevertsMergedBundle(tc.b, tc.sha, tc.at); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestChangeFailureAndIssueReopens(t *testing.T) {
	merged := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	b := &Bundle{Merge: &MergeEvent{At: merged}}
	if !b.AddIssueReopen(IssueReopen{Issue: 7, At: merged.Add(2 * 24 * time.Hour), Source: IssueReopenSourceClaimLedger}) {
		t.Fatal("first reopen must record")
	}
	if b.AddIssueReopen(IssueReopen{Issue: 7, At: merged.Add(3 * 24 * time.Hour), Source: IssueReopenSourceClaimLedger}) {
		t.Error("same issue must not record twice")
	}
	if b.AddIssueReopen(IssueReopen{Issue: 0, At: merged, Source: "x"}) || b.AddIssueReopen(IssueReopen{Issue: 8, Source: "x"}) {
		t.Error("invalid reopens must be rejected")
	}
	if !b.AddIssueReopen(IssueReopen{Issue: 7, Repo: "other/repo", At: merged, Source: "x"}) {
		t.Error("same number in another repo is a different issue")
	}

	p := &PR{Bundle: b, Merge: b.Merge, IssueReopens: b.IssueReopens}
	if rv, ro := p.ChangeFailure(); rv || !ro {
		t.Errorf("reopen within 14 d: reverted=%v reopened=%v", rv, ro)
	}
	late := &PR{Bundle: b, Merge: b.Merge, IssueReopens: []IssueReopen{{Issue: 9, At: merged.Add(LinkWindow + time.Minute), Source: "x"}}}
	if _, ro := late.ChangeFailure(); ro {
		t.Error("a reopen after 14 d is not a change failure")
	}
	rev := &PR{Bundle: b, Merge: b.Merge, RevertedBy: []string{"a/b#9@abc"}}
	if rv, ro := rev.ChangeFailure(); !rv || ro {
		t.Errorf("reverted_by link: reverted=%v reopened=%v", rv, ro)
	}
	open := &PR{Bundle: &Bundle{}, RevertedBy: []string{"a/b#9@abc"}, IssueReopens: b.IssueReopens}
	if rv, ro := open.ChangeFailure(); rv || ro {
		t.Error("an unmerged PR has no change failure")
	}
}

func TestFoldPRsUnionsIssueReopens(t *testing.T) {
	merged := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	mk := func(head string, created time.Time, reopens ...IssueReopen) *Bundle {
		return &Bundle{SchemaVersion: SchemaVersion, ID: "a/b#1@" + head, Repo: "a/b", Number: 1, HeadSHA: head,
			Author: Author{Login: "x", Kind: AuthorAgent}, CreatedAt: created, UpdatedAt: created, IssueReopens: reopens}
	}
	r1 := IssueReopen{Issue: 5, At: merged.Add(time.Hour), Source: "x"}
	r2 := IssueReopen{Issue: 6, At: merged.Add(time.Hour), Source: "x"}
	old := mk("aaa", merged.Add(-2*time.Hour), r1)
	newer := mk("bbb", merged.Add(-time.Hour), r1, r2)
	newer.Merge = &MergeEvent{Actor: "h", Method: "squash", At: merged, SHA: "bbb"}
	prs := FoldPRs([]*Bundle{old, newer}, "", "")
	p := prs[PRKey("a/b", 1)]
	if p == nil || len(p.IssueReopens) != 2 {
		t.Fatalf("fold must union reopens by issue: %+v", p)
	}
	if _, ro := p.ChangeFailure(); !ro {
		t.Error("folded PR must report the reopen")
	}
}

func TestBundleValidateIssueReopens(t *testing.T) {
	b := &Bundle{SchemaVersion: SchemaVersion, ID: "a/b#1@h", Repo: "a/b", Number: 1, BaseSHA: "b", HeadSHA: "h",
		Author: Author{Login: "x", Kind: AuthorAgent}, CI: CI{CapturedAt: time.Now()}, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := b.Validate(); err != nil {
		t.Fatalf("baseline bundle invalid: %v", err)
	}
	b.IssueReopens = []IssueReopen{{Issue: 3, At: time.Now(), Source: IssueReopenSourceClaimLedger}}
	if err := b.Validate(); err != nil {
		t.Errorf("valid reopen rejected: %v", err)
	}
	b.IssueReopens = []IssueReopen{{Issue: 0, Source: ""}}
	if err := b.Validate(); err == nil {
		t.Error("invalid reopen accepted")
	}
}

func TestRevertBundleID(t *testing.T) {
	if got := RevertBundleID("a/b", 9, " abc "); got != "a/b#9@abc" {
		t.Errorf("got %q", got)
	}
	if got := RevertBundleID("a/b", 9, ""); got != "a/b#9@merged" {
		t.Errorf("got %q", got)
	}
}
