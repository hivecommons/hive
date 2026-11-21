package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/effects"
	"github.com/hivecommons/hive/pkg/outputschema"
	"github.com/hivecommons/hive/pkg/review"
)

// Bodies the hive's reviewer wrote on the two PRs this fix comes from
// (projectbluefin/gutenprint-printer-app#254, projectbluefin/ps-printer-app#221).
const (
	ownBodyStyleInfo    = "Style, info, non-blocking. The bot wrote the package name as `PyYAML`. Every other entry in this file is lowercase. You can ignore it."
	ownBodyStyleLow     = "**style (low):** `PARENT_RETRY_DELAY=0` is never read: the `prepare-parent` recipe hard-codes `sleep 15` (`Justfile:91`)."
	ownBodyUnlabelledFx = "The PR body says the backoff comes from `PARENT_RETRY_DELAY` (default 15). This line ignores it and always sleeps 15s. Fix:"
)

func ownMarked(body, sev string) string {
	return body + "\n\n" + reviewFindingSeverityMarker + " " + sev + " -->"
}

func TestFreeTextNonBlocking(t *testing.T) {
	for _, tc := range []struct {
		body string
		want bool
	}{
		{ownBodyStyleInfo, true},
		{ownBodyStyleLow, true},
		{"**docs-currency (info):** the header still names 6.0.3.", true},
		{"Nit: trailing whitespace.", true},
		{"Low, non-blocking. `.github/requirements-ci.txt:3` still says 6.0.3.", true},
		{ownBodyUnlabelledFx, false},
		{"**correctness (high):** x may be nil here.", false},
		{"Blocking: this drops the error. Not a non-blocking note.", false},
		{"Low priority, but this must be fixed before merge.", false},
		{"non-blocking for now, but critical once the flag ships", false},
		{"", false},
		{"A finding.\nnon-blocking on the second line only", false},
	} {
		if got := freeTextNonBlocking(tc.body); got != tc.want {
			t.Errorf("freeTextNonBlocking(%q) = %v, want %v", tc.body, got, tc.want)
		}
	}
}

// typedRawThread is rawThread with each comment's GraphQL author type kept.
func typedRawThread(id string, resolved, outdated bool, comments ...gqlComment) rawReviewThread {
	t := rawThread(id, resolved, outdated, comments...)
	for i, cmt := range comments {
		t.Comments.Nodes[i].Author.Typename = cmt.AuthorType
	}
	return t
}

func TestClassifyOwnReviewThread(t *testing.T) {
	c := testClient(t, "http://unused.invalid")
	c.SetAppBotLogin("hive[bot]")
	// project.ai_author is never the hive's own here: it can be a person.
	c.SetHiveIdentity(HiveIdentity{AIAuthor: "operator", AppLogin: "hive[bot]"})
	own := func(body string) gqlComment { return gqlComment{Author: "hive", AuthorType: "Bot", Body: body} }
	for _, tc := range []struct {
		name    string
		thread  rawReviewThread
		blockAt string
		want    ownThreadClass
	}{
		{name: "marked low", thread: typedRawThread("A", false, false, own(ownMarked("a note", "low"))), want: ownThreadNonBlocking},
		{name: "marked info, prose says fix", thread: typedRawThread("A", false, false, own(ownMarked(ownBodyUnlabelledFx, "info"))), want: ownThreadNonBlocking},
		{name: "marked high, prose says non-blocking", thread: typedRawThread("A", false, false, own(ownMarked(ownBodyStyleInfo, "high"))), want: ownThreadBlocking},
		{name: "marked unmatched, prose says non-blocking", thread: typedRawThread("A", false, false, own(ownMarked(ownBodyStyleInfo, reviewFindingSeverityUnmatched))), want: ownThreadBlocking},
		{name: "marked required", thread: typedRawThread("A", false, false, own(ownMarked("x", "required"))), blockAt: "P1", want: ownThreadBlocking},
		{name: "marked high, outdated", thread: typedRawThread("A", false, true, own(ownMarked("x", "high"))), want: ownThreadBlocking},
		{name: "legacy unlabelled, outdated", thread: typedRawThread("A", false, true, own(ownBodyUnlabelledFx)), want: ownThreadBlocking},
		{name: "legacy style (low)", thread: typedRawThread("A", false, false, own(ownBodyStyleLow)), want: ownThreadNonBlocking},
		{name: "legacy unlabelled", thread: typedRawThread("A", false, false, own(ownBodyUnlabelledFx)), want: ownThreadBlocking},
		{name: "medium, no block_at", thread: typedRawThread("A", false, false, own(ownMarked("x", "medium"))), want: ownThreadBlocking},
		{name: "medium below P1", thread: typedRawThread("A", false, false, own(ownMarked("x", "medium"))), blockAt: "P1", want: ownThreadNonBlocking},
		{name: "high at P1", thread: typedRawThread("A", false, false, own(ownMarked("x", "high"))), blockAt: "P1", want: ownThreadBlocking},
		{name: "low at P3", thread: typedRawThread("A", false, false, own(ownMarked("x", "low"))), blockAt: "P3", want: ownThreadBlocking},
		{name: "legacy at P3", thread: typedRawThread("A", false, false, own(ownBodyStyleInfo)), blockAt: "P3", want: ownThreadBlocking},
		{name: "REST-spelled bot login", thread: typedRawThread("A", false, false, gqlComment{Author: "hive[bot]", AuthorType: "Bot", Body: ownBodyStyleInfo}), want: ownThreadNonBlocking},
		{name: "app login without a type", thread: typedRawThread("A", false, false, gqlComment{Author: "hive[bot]", Body: ownBodyStyleInfo}), want: ownThreadOther},
		{name: "a user named like the app", thread: typedRawThread("A", false, false, gqlComment{Author: "hive", AuthorType: "User", Body: ownBodyStyleInfo}), want: ownThreadOther},
		{name: "project.ai_author", thread: typedRawThread("A", false, false, gqlComment{Author: "operator", AuthorType: "User", Body: ownBodyStyleInfo}), want: ownThreadOther},
		{name: "person's thread", thread: typedRawThread("A", false, true, gqlComment{Author: "maintainer", AuthorType: "User", Body: "Nit: rename this."}), want: ownThreadOther},
		{name: "review bot's thread", thread: typedRawThread("A", false, false, gqlComment{Author: "chatgpt-codex-connector", AuthorType: "Bot", Body: "Low, non-blocking."}), want: ownThreadOther},
		{name: "hive thread a person answered", thread: typedRawThread("A", false, true, own(ownBodyStyleInfo), gqlComment{Author: "author", AuthorType: "User", Body: "I disagree"}), want: ownThreadAnswered},
		{name: "hive thread ai_author answered", thread: typedRawThread("A", false, false, own(ownBodyStyleInfo), gqlComment{Author: "operator", AuthorType: "User", Body: "ok"}), want: ownThreadAnswered},
		{name: "hive thread the hive answered", thread: typedRawThread("A", false, false, own(ownBodyStyleInfo), own("done")), want: ownThreadNonBlocking},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyOwnReviewThread(tc.thread, c.isOwnThreadAuthor, tc.blockAt); got != tc.want {
				t.Fatalf("class = %v, want %v", got, tc.want)
			}
		})
	}
	// No App identity: nothing is the hive's own.
	noApp := testClient(t, "http://unused.invalid")
	noApp.SetHiveIdentity(HiveIdentity{AIAuthor: "operator"})
	if got := classifyOwnReviewThread(typedRawThread("A", false, false, gqlComment{Author: "operator", AuthorType: "User", Body: ownBodyStyleInfo}), noApp.isOwnThreadAuthor, ""); got != ownThreadOther {
		t.Fatalf("without an App login: class = %v, want other", got)
	}
}

func TestIsUnresolvedConversationsError(t *testing.T) {
	gql := &graphQLErrors{Errors: []graphQLError{{Type: "UNPROCESSABLE", Message: "Pull request All comments must be resolved."}}}
	rest := &gh.ErrorResponse{Response: &http.Response{StatusCode: http.StatusMethodNotAllowed}, Message: "Repository rule violations found\n\nAll comments must be resolved."}
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{gql, true},
		{effects.NotApplied(gql), true},
		{fmt.Errorf("effect outcome uncertain, reconciliation required: %w", gql), true},
		{rest, true},
		{fmt.Errorf("merging PR: %w", rest), true},
		{errors.New("graphql: UNPROCESSABLE: Pull request All comments must be resolved"), true},
		{&graphQLErrors{Errors: []graphQLError{{Type: "UNPROCESSABLE", Message: "Pull request is in clean status"}}}, false},
		{errors.New("expectedHeadOid mismatch"), false},
		{nil, false},
	} {
		if got := IsUnresolvedConversationsError(tc.err); got != tc.want {
			t.Errorf("IsUnresolvedConversationsError(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

func ownThreadsMock(threads ...*gqlThread) *gqlMock {
	m := newGQLMock()
	for _, th := range threads {
		th.PRNumber, th.RepoOwner, th.RepoName = 9, "o", "r"
		m.add(th)
	}
	return m
}

func ownThreadsClient(t *testing.T, m *gqlMock) *Client {
	t.Helper()
	srv := httptest.NewServer(m.handler(t))
	t.Cleanup(srv.Close)
	return reviewThreadTestClient(t, srv.URL, config.ReviewBotsConfig{})
}

// The merge lanes' path: with only the hive's own non-blocking threads open
// (marked, or labelled in prose before the marker existed), every one is
// resolved and the PR is cleared; resolved threads are left alone.
func TestResolveOwnReviewThreadsForMerge_AllOwn(t *testing.T) {
	m := ownThreadsMock(
		&gqlThread{ID: "T-info", Comments: []gqlComment{{Author: "hive", AuthorType: "Bot", Body: ownBodyStyleInfo}}},
		&gqlThread{ID: "T-low", Outdated: true, Comments: []gqlComment{{Author: "hive", AuthorType: "Bot", Body: ownBodyStyleLow}}},
		&gqlThread{ID: "T-marked", Comments: []gqlComment{{Author: "hive", AuthorType: "Bot", Body: ownMarked("rename", "low")}}},
		&gqlThread{ID: "T-done", Resolved: true, Comments: []gqlComment{{Author: "maintainer", AuthorType: "User", Body: "blocking"}}},
	)
	c := ownThreadsClient(t, m)
	out, err := c.ResolveOwnReviewThreadsForMerge(context.Background(), "o/r", 9)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Cleared() || out.NeedsPerson() || out.Unresolved != 3 || out.Resolved != 3 {
		t.Fatalf("outcome = %+v, want all three open threads resolved", out)
	}
	got := append([]string(nil), m.resolved...)
	slices.Sort(got)
	if want := []string{"T-info", "T-low", "T-marked"}; !slices.Equal(got, want) {
		t.Fatalf("resolved %v, want %v", got, want)
	}
}

// Any thread the hive may not resolve leaves the PR exactly as it was, and
// the outcome says what is in the way.
func TestResolveOwnReviewThreadsForMerge_LeavesOthersAlone(t *testing.T) {
	ownNote := func() *gqlThread {
		return &gqlThread{ID: "T-own", Comments: []gqlComment{{Author: "hive", AuthorType: "Bot", Body: ownBodyStyleInfo}}}
	}
	for _, tc := range []struct {
		name  string
		other *gqlThread
		check func(OwnReviewThreadsOutcome) bool
	}{
		{"person's thread", &gqlThread{ID: "T-x", Comments: []gqlComment{{Author: "maintainer", AuthorType: "User", Body: "Nit: rename."}}},
			func(o OwnReviewThreadsOutcome) bool { return o.Others == 1 }},
		{"review bot's thread", &gqlThread{ID: "T-x", Comments: []gqlComment{{Author: "chatgpt-codex-connector", AuthorType: "Bot", Body: "![P2 Badge] low"}}},
			func(o OwnReviewThreadsOutcome) bool { return o.Others == 1 }},
		{"blocking hive finding", &gqlThread{ID: "T-x", Comments: []gqlComment{{Author: "hive", AuthorType: "Bot", Body: ownMarked("x is nil here", "high")}}},
			func(o OwnReviewThreadsOutcome) bool { return o.OwnBlocking == 1 }},
		// A Renovate branch the bot regenerated: the finding is outdated,
		// but nothing says it was non-blocking, and this lane may have
		// merged with no approving review at all.
		{"outdated blocking hive finding", &gqlThread{ID: "T-x", Outdated: true, Comments: []gqlComment{{Author: "hive", AuthorType: "Bot", Body: ownMarked("pin is unsigned", "high")}}},
			func(o OwnReviewThreadsOutcome) bool { return o.OwnBlocking == 1 }},
		{"outdated unlabelled hive finding", &gqlThread{ID: "T-x", Outdated: true, Comments: []gqlComment{{Author: "hive", AuthorType: "Bot", Body: ownBodyUnlabelledFx}}},
			func(o OwnReviewThreadsOutcome) bool { return o.OwnBlocking == 1 }},
		{"new comment no finding matched", &gqlThread{ID: "T-x", Comments: []gqlComment{{Author: "hive", AuthorType: "Bot", Body: ownMarked(ownBodyStyleInfo, reviewFindingSeverityUnmatched)}}},
			func(o OwnReviewThreadsOutcome) bool { return o.OwnBlocking == 1 }},
		{"hive thread a person answered", &gqlThread{ID: "T-x", Comments: []gqlComment{{Author: "hive", AuthorType: "Bot", Body: ownBodyStyleInfo}, {Author: "author", AuthorType: "User", Body: "not fixing"}}},
			func(o OwnReviewThreadsOutcome) bool { return o.OwnAnswered == 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := ownThreadsMock(ownNote(), tc.other)
			c := ownThreadsClient(t, m)
			out, err := c.ResolveOwnReviewThreadsForMerge(context.Background(), "o/r", 9)
			if err != nil {
				t.Fatal(err)
			}
			if out.Resolved != 0 || out.Cleared() || !out.NeedsPerson() || len(m.resolved) != 0 || out.Skipped != "not-every-thread-resolvable" || !tc.check(out) {
				t.Fatalf("outcome = %+v resolved=%v, want nothing resolved", out, m.resolved)
			}
		})
	}
}

func TestResolveOwnReviewThreadsPaused(t *testing.T) {
	m := ownThreadsMock(&gqlThread{ID: "T-own", Comments: []gqlComment{{Author: "hive", AuthorType: "Bot", Body: ownBodyStyleInfo}}})
	c := ownThreadsClient(t, m)
	c.SetRepoPausedFunc(func(string) bool { return true })
	if out, err := c.ResolveOwnReviewThreadsForMerge(context.Background(), "o/r", 9); err != nil || out.Skipped != "paused" || m.queryCount() != 0 {
		t.Fatalf("paused: %+v %v queries=%d", out, err, m.queryCount())
	}
}

// review.severity.block_at: P3 (every finding blocks) is the opt-out: no
// hive thread is non-blocking, marked or prose-labelled, so nothing is
// resolved and the outcome needs a person (the lane parks at 085). The same
// threads under the default line are all resolved.
func TestResolveOwnReviewThreadsForMerge_BlockAtP3ResolvesNothing(t *testing.T) {
	threads := func() []*gqlThread {
		return []*gqlThread{
			{ID: "T-marked-info", Comments: []gqlComment{{Author: "hive", AuthorType: "Bot", Body: ownMarked("typo", "info")}}},
			{ID: "T-marked-low", Comments: []gqlComment{{Author: "hive", AuthorType: "Bot", Body: ownMarked("rename", "low")}}},
			{ID: "T-legacy", Comments: []gqlComment{{Author: "hive", AuthorType: "Bot", Body: ownBodyStyleInfo}}},
		}
	}
	for _, blockAt := range []string{"P3", ""} {
		m := ownThreadsMock(threads()...)
		c := ownThreadsClient(t, m)
		c.SetReviewBacklogRouting(func() ReviewBacklogRouting {
			return ReviewBacklogRouting{Severity: config.ReviewSeverityConfig{BlockAt: blockAt}}
		})
		out, err := c.ResolveOwnReviewThreadsForMerge(context.Background(), "o/r", 9)
		if err != nil {
			t.Fatal(err)
		}
		if blockAt == "P3" {
			if out.Resolved != 0 || len(m.resolved) != 0 || out.OwnBlocking != 3 || !out.NeedsPerson() || out.Cleared() {
				t.Fatalf("block_at P3: outcome = %+v resolved=%v, want nothing resolved and a park", out, m.resolved)
			}
			continue
		}
		if !out.Cleared() || out.Resolved != 3 {
			t.Fatalf("default line: outcome = %+v, want all three resolved", out)
		}
	}
}

// isolateReviewRelayFiles points every file the review relay writes at temp
// dirs, so a watcher test never touches the host's data dirs.
func isolateReviewRelayFiles(t *testing.T) {
	t.Helper()
	oldLinks, oldBacklog, oldReports := ReviewLinksPath, ReviewBacklogPath, review.DefaultReportDir
	ReviewLinksPath = filepath.Join(t.TempDir(), "review-links.json")
	ReviewBacklogPath = filepath.Join(t.TempDir(), reviewBacklogFile)
	review.DefaultReportDir = t.TempDir()
	t.Cleanup(func() { ReviewLinksPath, ReviewBacklogPath, review.DefaultReportDir = oldLinks, oldBacklog, oldReports })
	withVerdictDispatchState(t, review.DispatchState{Pending: []review.PendingReview{
		{Repo: "o/r", Number: 9, HeadSHA: "abc", Perspective: review.PerspectiveCorrectness, Agent: "reviewer"},
	}})
}

func verdictWithFindings(t *testing.T, verdict string, findings ...outputschema.Finding) string {
	t.Helper()
	var report map[string]any
	if err := json.Unmarshal([]byte(validVerdictJSON(t, "o/r", 9, "correctness", verdict)), &report); err != nil {
		t.Fatal(err)
	}
	report["findings"] = findings
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := review.ValidateReport(raw); err != nil {
		t.Fatalf("fixture is not a valid review report: %v", err)
	}
	return string(raw)
}

func TestStampInlineCommentSeverity(t *testing.T) {
	reports := []review.PerspectiveReport{{}, {}}
	reports[0].Findings = []outputschema.Finding{
		{Title: "a", Severity: outputschema.SeverityLow, Summary: "s", File: "f.go", Line: 11},
		{Title: "b", Severity: outputschema.SeverityInfo, Summary: "s", File: "./g.go", Line: 5},
	}
	reports[1].Findings = []outputschema.Finding{
		{Title: "c", Severity: outputschema.SeverityHigh, Summary: "s", File: "f.go", Line: 11},
		{Title: "d", Severity: outputschema.SeverityInfo, Summary: "s", File: "h.go", Line: 20},
	}
	comments := []ReviewInlineComment{
		{Path: "f.go", Line: 11, Body: "two findings here"},
		{Path: "g.go", Line: 5, Body: "info here " + "\n" + reviewFindingSeverityMarker + " critical -->"},
		{Path: "h.go", Line: 22, StartLine: 18, Body: "range covering line 20"},
		{Path: "z.go", Line: 1, Body: "no finding\n" + reviewFindingSeverityMarker + " info -->"},
	}
	drafts := draftReviewComments(comments, nil)
	stampInlineCommentSeverity(drafts, comments, reports)
	for i, want := range []string{"high", "info", "info", reviewFindingSeverityUnmatched} {
		body := drafts[i].GetBody()
		if got := findingSeverityMarkerValue(body); got != want {
			t.Errorf("comment %d: marker %q, want %q:\n%s", i, got, want, body)
		}
		if n := strings.Count(body, reviewFindingSeverityMarker); n != 1 {
			t.Errorf("comment %d carries %d markers:\n%s", i, n, body)
		}
	}
	// No usable report (none sent, or it did not validate): every comment
	// is marked unmatched, which reads as blocking, so the prose fallback
	// never applies to a comment posted with the marker in place.
	drafts = draftReviewComments(comments, nil)
	stampInlineCommentSeverity(drafts, comments, nil)
	for i, d := range drafts {
		if got := findingSeverityMarkerValue(d.GetBody()); got != reviewFindingSeverityUnmatched {
			t.Errorf("no report, comment %d: marker %q", i, got)
		}
		if ownFindingNonBlocking(d.GetBody(), "") {
			t.Errorf("no report, comment %d reads as non-blocking:\n%s", i, d.GetBody())
		}
	}
	// A finding with no severity is unmatched too, not an empty marker.
	blank := []review.PerspectiveReport{{}}
	blank[0].Findings = []outputschema.Finding{{Title: "e", Summary: "s", File: "f.go", Line: 11}}
	drafts = draftReviewComments(comments[:1], nil)
	stampInlineCommentSeverity(drafts, comments[:1], blank)
	if got := findingSeverityMarkerValue(drafts[0].GetBody()); got != reviewFindingSeverityUnmatched {
		t.Errorf("blank severity: marker %q", got)
	}
}

// The relay marks each posted inline comment with its finding's severity.
func TestReviewRequestWatcher_InlineCommentsCarrySeverityMarker(t *testing.T) {
	s := &inlineReviewServer{}
	srv := s.start(t)
	defer srv.Close()
	c := reviewTestClient(t, srv.URL)
	dir := withReviewDir(t)
	isolateReviewRelayFiles(t)
	req := inlineReviewRequest()
	req.Report = verdictWithFindings(t, "changes_requested",
		outputschema.Finding{Title: "rename", Severity: outputschema.SeverityLow, Summary: "naming", File: "f.go", Line: 11, ReviewScope: "in-scope"})
	if _, err := WriteReviewRequest(dir, req); err != nil {
		t.Fatal(err)
	}
	c.ProcessReviewRequestsOnce(context.Background())
	if len(s.posts) != 1 || len(s.posts[0].Comments) != 1 {
		t.Fatalf("posts = %+v, want one review with one inline comment", s.posts)
	}
	body := s.posts[0].Comments[0].GetBody()
	if !strings.HasPrefix(body, "anchored finding") || findingSeverityMarkerValue(body) != "low" {
		t.Fatalf("inline comment body = %q, want the finding with a low severity marker", body)
	}
}
