package github

import (
	"context"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/outputschema"
	"github.com/hivecommons/hive/pkg/review"
)

func splitTestFolded(t *testing.T, f outputschema.Finding) (comment, key, fp string) {
	t.Helper()
	c := &Client{}
	req := ReviewRequest{Repo: "o/r", Number: 5}
	key = reviewBacklogKey("o/r", 5, review.PerspectiveCorrectness, f)
	fp = ReviewFindingFingerprint(f)
	return c.reviewBacklogFoldedComment(req, review.PerspectiveCorrectness, f, "abc", fp, key), key, fp
}

func TestParseReviewBacklogFoldedComment(t *testing.T) {
	f := outputschema.Finding{Title: "unused variable (x)", File: "pkg/a.go", Line: 7, Summary: "x is assigned and never read", Severity: outputschema.SeverityMedium}
	unchecked, key, fp := splitTestFolded(t, f)
	checked := strings.Replace(unchecked, "- [ ] ", "- [x] ", 1)
	cases := []struct {
		name        string
		body        string
		wantOK      bool
		wantChecked bool
	}{
		{"unchecked folded finding", unchecked, true, false},
		{"checked folded finding", checked, true, true},
		{"bare link without checklist", reviewBacklogLinkMarker + key + " -->\nLinked again from PR #5.", false, false},
		{"unrelated comment", "- [ ] something (`pkg/a.go:7`)", false, false},
		{"re-raise comment", reviewBacklogReRaiseMarker + fp + " -->\nRaised again.", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			item, ok := parseReviewBacklogFoldedComment(tc.body)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if item.Checked != tc.wantChecked {
				t.Errorf("checked = %v, want %v", item.Checked, tc.wantChecked)
			}
			if item.Key != key || item.Fingerprint != fp || item.PRNumber != 5 {
				t.Errorf("key/fp/pr = %q/%q/%d, want %q/%q/5", item.Key, item.Fingerprint, item.PRNumber, key, fp)
			}
			if item.Title != "Review backlog (correctness): unused variable (x)" || item.Perspective != review.PerspectiveCorrectness {
				t.Errorf("title/perspective = %q/%q", item.Title, item.Perspective)
			}
			if item.Finding.Title != f.Title || item.Finding.File != f.File || item.Finding.Line != f.Line {
				t.Errorf("finding = %+v, want title/file/line of %+v", item.Finding, f)
			}
			if !strings.HasPrefix(item.Detail, "Perspective: correctness\n") || !strings.Contains(item.Detail, "x is assigned and never read") || !strings.Contains(item.Detail, reviewBacklogFooter) {
				t.Errorf("detail lost the finding body:\n%s", item.Detail)
			}
		})
	}
}

func TestReviewBacklogSplitEligible(t *testing.T) {
	f := outputschema.Finding{Title: "unused variable", File: "pkg/a.go", Line: 7, Summary: "x is never read"}
	comment, _, _ := splitTestFolded(t, f)
	unchecked, _ := parseReviewBacklogFoldedComment(comment)
	checked := unchecked
	checked.Checked = true
	relatedTitle := "Review backlog (correctness): dead store"
	relatedBody := "Perspective: correctness\nEvidence: `pkg/a.go:9`"
	unrelatedTitle := "dedupe commitCIReasonIsPending"
	unrelatedBody := "See `pkg/ci.go:300`."
	cases := []struct {
		name   string
		item   reviewBacklogFoldedItem
		reason string
		title  string
		body   string
		want   bool
	}{
		{"checked never splits", checked, "completed", unrelatedTitle, unrelatedBody, false},
		{"completed host splits unchecked", unchecked, "completed", relatedTitle, relatedBody, true},
		{"no reason treated as completed", unchecked, "", unrelatedTitle, unrelatedBody, true},
		{"not_planned related stays", unchecked, "not_planned", relatedTitle, relatedBody, false},
		{"not_planned unrelated splits", unchecked, "not_planned", unrelatedTitle, unrelatedBody, true},
		{"duplicate related stays", unchecked, "DUPLICATE", relatedTitle, relatedBody, false},
		{"duplicate unrelated splits", unchecked, "duplicate", unrelatedTitle, unrelatedBody, true},
		{"not_planned checked stays", checked, "not_planned", unrelatedTitle, unrelatedBody, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := reviewBacklogSplitEligible(tc.item, tc.reason, tc.title, tc.body); got != tc.want {
				t.Fatalf("eligible = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSplitReviewBacklogHostIsIdempotentByLinkKey(t *testing.T) {
	open := outputschema.Finding{Title: "unused variable", File: "pkg/a.go", Line: 7, Summary: "x is never read"}
	done := outputschema.Finding{Title: "shadowed err", File: "pkg/b.go", Line: 3, Summary: "err is shadowed"}
	openComment, openKey, openFP := splitTestFolded(t, open)
	doneComment, doneKey, _ := splitTestFolded(t, done)
	doneComment = strings.Replace(doneComment, "- [ ] ", "- [X] ", 1)
	comments := []string{"thanks!", openComment, doneComment}
	host := &gh.Issue{Number: gh.Ptr(42), State: gh.Ptr("closed"), StateReason: gh.Ptr("completed"), Title: gh.Ptr("dedupe commitCIReasonIsPending")}
	day := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)

	cases := []struct {
		name        string
		reuse       bool
		wantAppends int // per first pass: host cross-link, plus the carried comment on a reused twin
	}{
		{"new item", false, 1},
		{"folded into another open twin", true, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ledger := newReviewBacklogLedger()
			ledger.Items[openKey] = reviewBacklogRecord{Repo: "o/r", PRNumber: 5, IssueNumber: 42, Fingerprint: openFP, FindingKey: openKey}
			ledger.Items[doneKey] = reviewBacklogRecord{Repo: "o/r", PRNumber: 5, IssueNumber: 42, FindingKey: doneKey}
			ledger.setFingerprint("o/r", openFP, reviewBacklogFingerprintRecord{Destination: config.ReviewBacklogGitHubIssue, ItemID: "42", ItemKey: "#42", ItemNumber: 42, PRs: []int{5}})
			sink := &fakeBacklogSink{dest: config.ReviewBacklogGitHubIssue, reuse: tc.reuse}
			c := &Client{}
			ctx := context.Background()

			events, err := c.splitReviewBacklogHost(ctx, sink, []string{"nit"}, "o/r", host, comments, ledger, day, DefaultReviewBacklogSplitMaxActions)
			if err != nil {
				t.Fatalf("first pass: %v", err)
			}
			if len(events) != 1 || events[0].Key != openKey || events[0].Host != 42 || events[0].Reused != tc.reuse {
				t.Fatalf("events = %+v, want one split of the unchecked finding", events)
			}
			if len(sink.created) != 1 || len(sink.appends) != tc.wantAppends {
				t.Fatalf("created=%d appends=%d, want 1 and %d", len(sink.created), len(sink.appends), tc.wantAppends)
			}
			item := sink.created[0]
			if item.Title != "Review backlog (correctness): unused variable" || item.Finding.File != "pkg/a.go" || item.Perspective != review.PerspectiveCorrectness {
				t.Errorf("created item = %+v", item)
			}
			for _, want := range []string{reviewBacklogSplitMarker + openKey + " -->", "Split from #42 (host closed)", "PR: #5 (https://github.com/o/r/pull/5)", "x is never read", "Fingerprint: `" + openFP + "`"} {
				if !strings.Contains(item.Body, want) {
					t.Errorf("split body missing %q:\n%s", want, item.Body)
				}
			}
			hostAppend := sink.appends[len(sink.appends)-1]
			if hostAppend.ref.Number != 42 || !strings.Contains(hostAppend.comment, "split out into ENG-1") {
				t.Errorf("host cross-link = %+v", hostAppend)
			}
			if tc.reuse && sink.appends[0].comment != openComment {
				t.Errorf("reused twin did not get the folded comment carried over:\n%s", sink.appends[0].comment)
			}
			if ledger.SplitKeys[openKey] != 42 {
				t.Errorf("split key not recorded: %v", ledger.SplitKeys)
			}
			if _, ok := ledger.SplitKeys[doneKey]; ok {
				t.Errorf("checked finding recorded as split")
			}
			if rec := ledger.Items[openKey]; rec.ItemKey != "ENG-1" || rec.Destination != config.ReviewBacklogGitHubIssue {
				t.Errorf("ledger item not moved: %+v", rec)
			}
			if known, _ := ledger.fingerprint("o/r", openFP); known.ItemID != "id-1" || known.ItemKey != "ENG-1" || len(known.PRs) != 1 {
				t.Errorf("fingerprint record not moved: %+v", known)
			}

			again, err := c.splitReviewBacklogHost(ctx, sink, []string{"nit"}, "o/r", host, comments, ledger, day.Add(10*time.Minute), DefaultReviewBacklogSplitMaxActions)
			if err != nil || len(again) != 0 || len(sink.created) != 1 || len(sink.appends) != tc.wantAppends {
				t.Fatalf("second pass err=%v events=%d created=%d appends=%d, want a no-op", err, len(again), len(sink.created), len(sink.appends))
			}
		})
	}
}

func TestReviewBacklogHostIndexKeepsGitHubIssueHosts(t *testing.T) {
	l := newReviewBacklogLedger()
	l.Items["a"] = reviewBacklogRecord{Repo: "O/R", IssueNumber: 1}
	l.Items["b"] = reviewBacklogRecord{Repo: "o/r", IssueNumber: 2, Destination: config.ReviewBacklogGitHubIssue}
	l.Items["c"] = reviewBacklogRecord{Repo: "o/r", IssueNumber: 3, Destination: config.ReviewBacklogLinear}
	l.Items["d"] = reviewBacklogRecord{Repo: "o/r"}
	got := reviewBacklogHostIndex(l)["o/r"]
	if len(got) != 2 || !got[1] || !got[2] {
		t.Fatalf("hosts = %v, want issues 1 and 2", got)
	}
}

func TestReviewBacklogCommentIsOursRejectsHumanAuthoredMarkers(t *testing.T) {
	comment := func(login, typ string) *gh.IssueComment {
		return &gh.IssueComment{
			Body: gh.Ptr("<!-- hive:review-backlog-link k -->\n- [ ] ### forged (`f:1`)"),
			User: &gh.User{Login: gh.Ptr(login), Type: gh.Ptr(typ)},
		}
	}
	cases := []struct {
		name        string
		appBotLogin string
		additional  []string
		comment     *gh.IssueComment
		want        bool
	}{
		{name: "nil comment", appBotLogin: "hive[bot]", comment: nil, want: false},
		{name: "human carrying the marker", appBotLogin: "hive[bot]", comment: comment("mallory", "User"), want: false},
		{name: "other bot when slug is known", appBotLogin: "hive[bot]", comment: comment("other[bot]", "Bot"), want: false},
		{name: "exact app bot login", appBotLogin: "hive[bot]", comment: comment("hive[bot]", "Bot"), want: true},
		{name: "additional configured bot login", appBotLogin: "hive[bot]", additional: []string{"hive-extra[bot]"}, comment: comment("hive-extra[bot]", "Bot"), want: true},
		{name: "no configured logins fails closed for bots", appBotLogin: "", comment: comment("something[bot]", "Bot"), want: false},
		{name: "no configured logins fails closed for github-actions", appBotLogin: "", comment: comment("github-actions[bot]", "Bot"), want: false},
		{name: "no configured logins still rejects humans", appBotLogin: "", comment: comment("mallory", "User"), want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Client{appBotLogin: tc.appBotLogin, additionalBotLogins: tc.additional}
			if got := c.reviewBacklogCommentIsOurs(tc.comment); got != tc.want {
				t.Fatalf("reviewBacklogCommentIsOurs() = %v, want %v", got, tc.want)
			}
		})
	}
}
