package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
			if item.Title != "[reviewer] Review backlog (correctness): unused variable (x)" || item.Perspective != review.PerspectiveCorrectness {
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

// A fold by shared symbol or exact-title twin can carry a finding with no
// File; reviewBacklogFoldedComment writes it as (`:0`) and the parser must
// still read it so it does not silently close with the host.
func TestParseReviewBacklogFoldedCommentWithoutFile(t *testing.T) {
	f := outputschema.Finding{Title: "dedupe commitCIReasonIsPending", Summary: "two copies of the helper", Severity: outputschema.SeverityLow}
	comment, key, _ := splitTestFolded(t, f)
	item, ok := parseReviewBacklogFoldedComment(comment)
	if !ok {
		t.Fatalf("folded comment without a file not parsed:\n%s", comment)
	}
	if item.Key != key || item.Checked || item.Finding.Title != f.Title || item.Finding.File != "" || item.Finding.Line != 0 {
		t.Errorf("item = %+v", item)
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

			split := c.splitReviewBacklogHost(ctx, sink, []string{"nit"}, "o/r", host, comments, ledger, day, DefaultReviewBacklogSplitMaxActions)
			if split.Err != nil || split.Skipped != 0 {
				t.Fatalf("first pass: err=%v skipped=%d", split.Err, split.Skipped)
			}
			events := split.Events
			if len(events) != 1 || events[0].Key != openKey || events[0].Host != 42 || events[0].Reused != tc.reuse {
				t.Fatalf("events = %+v, want one split of the unchecked finding", events)
			}
			if len(sink.created) != 1 || len(sink.appends) != tc.wantAppends {
				t.Fatalf("created=%d appends=%d, want 1 and %d", len(sink.created), len(sink.appends), tc.wantAppends)
			}
			item := sink.created[0]
			if item.Title != "[reviewer] Review backlog (correctness): unused variable" || item.Finding.File != "pkg/a.go" || item.Perspective != review.PerspectiveCorrectness {
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
			if !ledger.splitFrom(openKey, 42) {
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

			again := c.splitReviewBacklogHost(ctx, sink, []string{"nit"}, "o/r", host, comments, ledger, day.Add(10*time.Minute), DefaultReviewBacklogSplitMaxActions)
			if again.Err != nil || len(again.Events) != 0 || len(sink.created) != 1 || len(sink.appends) != tc.wantAppends {
				t.Fatalf("second pass err=%v events=%d created=%d appends=%d, want a no-op", again.Err, len(again.Events), len(sink.created), len(sink.appends))
			}
		})
	}
}

// A finding split out of host A into open twin B, then split again when B
// closes, must stay a no-op for both hosts: the first post-restart pass
// re-lists A, whose fold comment still carries the key.
func TestSplitReviewBacklogHostTracksEveryHostPerKey(t *testing.T) {
	f := outputschema.Finding{Title: "unused variable", File: "pkg/a.go", Line: 7, Summary: "x is never read"}
	comment, key, fp := splitTestFolded(t, f)
	hostA := &gh.Issue{Number: gh.Ptr(42), State: gh.Ptr("closed"), StateReason: gh.Ptr("completed"), Title: gh.Ptr("host a")}
	hostB := &gh.Issue{Number: gh.Ptr(43), State: gh.Ptr("closed"), StateReason: gh.Ptr("completed"), Title: gh.Ptr("host b")}
	day := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)
	ledger := newReviewBacklogLedger()
	ledger.Items[key] = reviewBacklogRecord{Repo: "o/r", PRNumber: 5, IssueNumber: 42, Fingerprint: fp, FindingKey: key}
	c := &Client{}
	ctx := context.Background()

	// A closes: the split lands in open twin B, which carries the comment.
	sink := &fakeBacklogSink{dest: config.ReviewBacklogGitHubIssue, reuse: true}
	if s := c.splitReviewBacklogHost(ctx, sink, nil, "o/r", hostA, []string{comment}, ledger, day, 10); s.Err != nil || len(s.Events) != 1 {
		t.Fatalf("split from A: events=%d err=%v", len(s.Events), s.Err)
	}
	// B closes: the same key splits again from B.
	sink = &fakeBacklogSink{dest: config.ReviewBacklogGitHubIssue}
	if s := c.splitReviewBacklogHost(ctx, sink, nil, "o/r", hostB, []string{comment}, ledger, day, 10); s.Err != nil || len(s.Events) != 1 || s.Events[0].Host != 43 {
		t.Fatalf("split from B: events=%+v err=%v", s.Events, s.Err)
	}
	if !ledger.splitFrom(key, 42) || !ledger.splitFrom(key, 43) || len(ledger.SplitKeys[key]) != 2 {
		t.Fatalf("split_keys = %v, want both hosts 42 and 43", ledger.SplitKeys)
	}
	// A is re-listed on the next pass: nothing fires from either host.
	for _, host := range []*gh.Issue{hostA, hostB} {
		sink = &fakeBacklogSink{dest: config.ReviewBacklogGitHubIssue}
		if s := c.splitReviewBacklogHost(ctx, sink, nil, "o/r", host, []string{comment}, ledger, day, 10); s.Err != nil || len(s.Events) != 0 || len(sink.created) != 0 || len(sink.appends) != 0 {
			t.Fatalf("re-run on #%d: events=%d created=%d appends=%d err=%v, want a no-op", host.GetNumber(), len(s.Events), len(sink.created), len(sink.appends), s.Err)
		}
	}
	ledger.markSplitFrom(key, 42)
	if len(ledger.SplitKeys[key]) != 2 {
		t.Fatalf("markSplitFrom duplicated a host: %v", ledger.SplitKeys[key])
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

// remaining bounds the findings one host contributes to a pass: with two
// unchecked folded findings and room for one, exactly one is filed and the
// other is left for a later pass.
func TestSplitReviewBacklogHostHonoursMaxActions(t *testing.T) {
	first, firstKey, _ := splitTestFolded(t, outputschema.Finding{Title: "unused variable", File: "pkg/a.go", Line: 7, Summary: "x is never read"})
	second, secondKey, _ := splitTestFolded(t, outputschema.Finding{Title: "shadowed err", File: "pkg/b.go", Line: 3, Summary: "err is shadowed"})
	host := &gh.Issue{Number: gh.Ptr(42), State: gh.Ptr("closed"), StateReason: gh.Ptr("completed"), Title: gh.Ptr("host")}
	ledger := newReviewBacklogLedger()
	ledger.Items[firstKey] = reviewBacklogRecord{Repo: "o/r", PRNumber: 5, IssueNumber: 42, FindingKey: firstKey}
	ledger.Items[secondKey] = reviewBacklogRecord{Repo: "o/r", PRNumber: 5, IssueNumber: 42, FindingKey: secondKey}
	sink := &fakeBacklogSink{dest: config.ReviewBacklogGitHubIssue}
	c := &Client{}
	day := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)

	split := c.splitReviewBacklogHost(context.Background(), sink, nil, "o/r", host, []string{first, second}, ledger, day, 1)
	if split.Err != nil {
		t.Fatalf("split: %v", split.Err)
	}
	events := split.Events
	if len(events) != 1 || events[0].Key != firstKey || len(sink.created) != 1 {
		t.Fatalf("events=%+v created=%d, want exactly one split", events, len(sink.created))
	}
	if ledger.splitFrom(secondKey, 42) {
		t.Errorf("second finding recorded as split despite the cap")
	}

	rest := c.splitReviewBacklogHost(context.Background(), sink, nil, "o/r", host, []string{first, second}, ledger, day, 1)
	if rest.Err != nil || len(rest.Events) != 1 || rest.Events[0].Key != secondKey || len(sink.created) != 2 {
		t.Fatalf("second pass err=%v events=%+v created=%d, want the remaining finding", rest.Err, rest.Events, len(sink.created))
	}
}

// A sink that files nothing for an eligible finding (the github_issue sink
// on a rejected twin elsewhere) is counted as a skip, not silently dropped,
// and the key is left unmarked so a later pass tries again.
func TestSplitReviewBacklogHostCountsEmptyRefAsSkipped(t *testing.T) {
	comment, key, _ := splitTestFolded(t, outputschema.Finding{Title: "unused variable", File: "pkg/a.go", Line: 7, Summary: "x is never read"})
	host := &gh.Issue{Number: gh.Ptr(42), State: gh.Ptr("closed"), StateReason: gh.Ptr("not_planned"), Title: gh.Ptr("host")}
	ledger := newReviewBacklogLedger()
	ledger.Items[key] = reviewBacklogRecord{Repo: "o/r", PRNumber: 5, IssueNumber: 42, FindingKey: key}
	sink := &fakeBacklogSink{dest: config.ReviewBacklogGitHubIssue, nothing: true}
	c := &Client{}

	split := c.splitReviewBacklogHost(context.Background(), sink, nil, "o/r", host, []string{comment}, ledger, time.Now(), 10)
	if split.Err != nil || len(split.Events) != 0 || split.Skipped != 1 {
		t.Fatalf("split = %+v, want one skip and nothing else", split)
	}
	if len(sink.created) != 1 || sink.created[0].SplitFrom != 42 {
		t.Fatalf("created = %+v, want one create naming host 42 in SplitFrom", sink.created)
	}
	if len(sink.appends) != 0 || ledger.splitFrom(key, 42) || ledger.Items[key].IssueNumber != 42 {
		t.Fatalf("appends=%d splitFrom=%v item=%+v, want no cross-link and the ledger untouched", len(sink.appends), ledger.splitFrom(key, 42), ledger.Items[key])
	}
}

// End to end through the GitHub API: the ledger points a folded finding at
// host #42, which closed not_planned and is itself a rejected App-bot
// issue over the finding's file. The sweep reads only the bot-authored
// fold comment, files the unrelated unchecked finding as #99 (the host is
// not its own rejected twin), cross-links the host, audits the split and
// saves the ledger, and a second pass is a no-op.
func TestSweepReviewBacklogSplitsFilesOutOfRejectedHost(t *testing.T) {
	withReviewBacklogState(t)
	f := outputschema.Finding{Title: "unused variable", File: "pkg/a.go", Line: 7, Summary: "x is never read"}
	comment, key, fp := splitTestFolded(t, f)
	forged, forgedKey, _ := splitTestFolded(t, outputschema.Finding{Title: "forged", File: "pkg/z.go", Line: 1})
	ledger := newReviewBacklogLedger()
	ledger.Items[key] = reviewBacklogRecord{Repo: "o/r", PRNumber: 5, IssueNumber: 42, Fingerprint: fp, FindingKey: key, Destination: config.ReviewBacklogGitHubIssue}
	ledger.Items[forgedKey] = reviewBacklogRecord{Repo: "o/r", PRNumber: 5, IssueNumber: 42, FindingKey: forgedKey, Destination: config.ReviewBacklogGitHubIssue}
	if err := ledger.save("", time.Now()); err != nil {
		t.Fatal(err)
	}

	recent := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	var created, hostComments int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues":
			if r.URL.Query().Get("state") != "closed" {
				_, _ = io.WriteString(w, `[]`)
				return
			}
			_, _ = io.WriteString(w, `[{"number":42,"title":"[scanner] unchecked Close error in pkg/a.go","body":"Evidence: `+"`pkg/a.go:5`"+` drops the Close error","state":"closed","state_reason":"not_planned","comments":2,"closed_at":"`+recent+`","updated_at":"`+recent+`","html_url":"https://github.example/o/r/issues/42","user":{"login":"`+testBot+`"}}]`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/42/comments":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 1, "body": forged, "user": map[string]any{"login": "mallory", "type": "User"}},
				{"id": 2, "body": comment, "user": map[string]any{"login": testBot, "type": "Bot"}},
			})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/o/r/labels/"):
			_, _ = io.WriteString(w, `{"name":"x"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/42/comments":
			hostComments++
			_, _ = io.WriteString(w, `{"id":3}`)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues":
			created++
			_, _ = io.WriteString(w, `{"number":99,"html_url":"https://github.example/o/r/issues/99"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprintf(w, `{"path":%q}`, r.URL.Path)
		}
	}))
	defer srv.Close()
	c := reviewTestClient(t, srv.URL)
	c.appBotLogin = testBot
	c.SetRepos([]string{"o/r"})

	var audited []ReviewBacklogSplitEvent
	res, err := c.SweepReviewBacklogSplits(context.Background(), ReviewBacklogSplitSweepOptions{
		Audit: func(ev ReviewBacklogSplitEvent) { audited = append(audited, ev) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Hosts != 1 || res.Skipped != 0 || res.Deferred != 0 || len(res.Split) != 1 {
		t.Fatalf("result = %+v, want one host and one split", res)
	}
	if ev := res.Split[0]; ev.Repo != "o/r" || ev.Host != 42 || ev.Number != 99 || ev.Reused || ev.Key != key {
		t.Fatalf("split event = %+v", ev)
	}
	if len(audited) != 1 || audited[0] != res.Split[0] {
		t.Fatalf("audited = %+v, want the split event once", audited)
	}
	if created != 1 || hostComments != 1 {
		t.Fatalf("created=%d hostComments=%d, want one issue and one cross-link", created, hostComments)
	}
	saved, err := loadReviewBacklog("")
	if err != nil {
		t.Fatal(err)
	}
	if !saved.splitFrom(key, 42) || saved.Items[key].IssueNumber != 99 || saved.Items[key].ItemKey != "#99" {
		t.Fatalf("ledger not saved with the split: split_keys=%v item=%+v", saved.SplitKeys, saved.Items[key])
	}
	if _, ok := saved.SplitKeys[forgedKey]; ok || saved.Items[forgedKey].IssueNumber != 42 {
		t.Fatalf("human-authored fold comment was split: %+v", saved.Items[forgedKey])
	}

	again, err := c.SweepReviewBacklogSplits(context.Background(), ReviewBacklogSplitSweepOptions{})
	if err != nil || again.Hosts != 1 || len(again.Split) != 0 || created != 1 || hostComments != 1 {
		t.Fatalf("second pass err=%v result=%+v created=%d hostComments=%d, want a no-op", err, again, created, hostComments)
	}
}
