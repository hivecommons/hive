package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

// Issue-author credit at merge time (hivecommons/hive#14581).

func TestIssueAuthorCreditTrailer(t *testing.T) {
	c := IssueAuthorCredit{Login: "Danathar", ID: 12345}
	if got, want := c.Trailer(), "Co-authored-by: Danathar <12345+Danathar@users.noreply.github.com>"; got != want {
		t.Fatalf("Trailer() = %q, want %q", got, want)
	}
	noID := IssueAuthorCredit{Login: "dana"}
	if got, want := noID.Email(), "dana@users.noreply.github.com"; got != want {
		t.Fatalf("Email() without id = %q, want %q", got, want)
	}
}

func TestMergeCreditClosingIssueRefs(t *testing.T) {
	for _, tc := range []struct {
		body string
		want []int
	}{
		{body: "Fixes #42", want: []int{42}},
		{body: "Closes #9 and fixes #3", want: []int{9, 3}},
		{body: "Resolves acme/widget#5", want: []int{5}},
		{body: "Fixes other/repo#5", want: nil},
		{body: "Refs #42\nRelated to #43", want: nil},
		{body: "Fixes #42\nFixes #42", want: []int{42}},
		{body: "fix: the thing (closes #60)", want: []int{60}},
		{body: "`Fixes #7` in a code span", want: nil},
		{body: "", want: nil},
	} {
		got := ClosingIssueRefs(tc.body, "acme/widget")
		if fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("ClosingIssueRefs(%q) = %v, want %v", tc.body, got, tc.want)
		}
	}
}

func commitWithMessage(msg string) *gh.RepositoryCommit {
	return &gh.RepositoryCommit{Commit: &gh.Commit{Message: gh.Ptr(msg)}, Parents: []*gh.Commit{{SHA: gh.Ptr("p")}}}
}

func TestUncreditedAuthorsSkipsTrailersTheBranchCarries(t *testing.T) {
	authors := []IssueAuthorCredit{
		{Login: "Danathar", ID: 12345},
		{Login: "gregoryhunt", ID: 77},
		{Login: "newperson", ID: 9},
	}
	commits := []*gh.RepositoryCommit{
		// issue-coauthor.sh writes the display name with the id-prefixed address.
		commitWithMessage("Fix\n\nSigned-off-by: K <k@example.com>\nCo-authored-by: Dana Thar <12345+danathar@users.noreply.github.com>"),
		// A hand-written trailer naming the login with a plain noreply address.
		commitWithMessage("More\n\nCo-authored-by: GregoryHunt <gregoryhunt@users.noreply.github.com>"),
	}
	got := uncreditedAuthors(commits, authors)
	if len(got) != 1 || got[0].Login != "newperson" {
		t.Fatalf("uncreditedAuthors = %+v, want only newperson", got)
	}
}

func TestSquashCreditMessagePreservesSignoffsAndAppendsTrailers(t *testing.T) {
	added := []IssueAuthorCredit{{Login: "Danathar", ID: 12345}, {Login: "bob", ID: 2}}
	t.Run("single commit keeps its body", func(t *testing.T) {
		commits := []*gh.RepositoryCommit{commitWithMessage("Fix the widget\n\nLonger text.\n\nSigned-off-by: Karibhou <k@example.com>")}
		got := squashCreditMessage(commits, added)
		want := "Longer text.\n\nSigned-off-by: Karibhou <k@example.com>\n\n" +
			"Co-authored-by: Danathar <12345+Danathar@users.noreply.github.com>\n" +
			"Co-authored-by: bob <2+bob@users.noreply.github.com>"
		if got != want {
			t.Fatalf("squashCreditMessage =\n%s\nwant\n%s", got, want)
		}
	})
	t.Run("several commits list each subject and body, merge commits dropped", func(t *testing.T) {
		mergeFromBase := commitWithMessage("Merge branch 'v5' into fix")
		mergeFromBase.Parents = append(mergeFromBase.Parents, &gh.Commit{SHA: gh.Ptr("q")})
		commits := []*gh.RepositoryCommit{
			commitWithMessage("First\n\nSigned-off-by: A <a@example.com>"),
			mergeFromBase,
			commitWithMessage("Second"),
		}
		got := squashCreditMessage(commits, added[:1])
		want := "* First\n\nSigned-off-by: A <a@example.com>\n\n* Second\n\n" +
			"Co-authored-by: Danathar <12345+Danathar@users.noreply.github.com>"
		if got != want {
			t.Fatalf("squashCreditMessage =\n%s\nwant\n%s", got, want)
		}
		if strings.Count(got, "Signed-off-by:") != 1 {
			t.Fatalf("sign-off lost or duplicated:\n%s", got)
		}
	})
	t.Run("merge commit body is the title then the trailers", func(t *testing.T) {
		got := mergeCommitCreditMessage("  Fix the widget ", added[:1])
		want := "Fix the widget\n\nCo-authored-by: Danathar <12345+Danathar@users.noreply.github.com>"
		if got != want {
			t.Fatalf("mergeCommitCreditMessage = %q, want %q", got, want)
		}
	})
}

// mergeCreditAPI is a one-PR GitHub API: issues by number with a configured
// author, and the PR's commits.
type mergeCreditAPI struct {
	issues      map[int]map[string]any
	commits     []string
	issueGets   atomic.Int32
	listGets    atomic.Int32
	commitsHTTP int
}

func (a *mergeCreditAPI) server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/acme/widget/issues/"):
			a.issueGets.Add(1)
			var n int
			fmt.Sscanf(strings.TrimPrefix(r.URL.Path, "/repos/acme/widget/issues/"), "%d", &n)
			issue, ok := a.issues[n]
			if !ok {
				http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
				return
			}
			json.NewEncoder(w).Encode(issue)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/7/commits":
			a.listGets.Add(1)
			if a.commitsHTTP != 0 {
				http.Error(w, "boom", a.commitsHTTP)
				return
			}
			out := make([]map[string]any, 0, len(a.commits))
			for i, msg := range a.commits {
				out = append(out, map[string]any{"sha": fmt.Sprintf("c%d", i), "commit": map[string]any{"message": msg}, "parents": []map[string]any{{"sha": "p"}}})
			}
			json.NewEncoder(w).Encode(out)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
			http.Error(w, "unexpected", http.StatusTeapot)
		}
	}))
}

func issueBy(login string, id int64, userType string) map[string]any {
	return map[string]any{"number": 1, "state": "open", "user": map[string]any{"login": login, "id": id, "type": userType}}
}

func newMergeCreditorForTest(t *testing.T, api *mergeCreditAPI, enabled func(string) bool) (*MergeCreditor, *mergeCreditAPI) {
	t.Helper()
	srv := api.server(t)
	t.Cleanup(srv.Close)
	c := NewClientForTest(srv.URL, "acme", []string{"widget"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return NewMergeCreditor(c.client, enabled, c.logger), api
}

func creditPR(body, author string) *gh.PullRequest {
	return &gh.PullRequest{Number: gh.Ptr(7), Title: gh.Ptr("Fix the widget"), Body: gh.Ptr(body), User: &gh.User{Login: gh.Ptr(author)}}
}

func TestMergeCreditorPrepareSquash(t *testing.T) {
	api := &mergeCreditAPI{
		issues: map[int]map[string]any{
			10: issueBy("Danathar", 12345, "User"),
			11: issueBy("Danathar", 12345, "User"), // same person twice → one trailer
			12: issueBy("dependabot[bot]", 1, "Bot"),
			13: issueBy("Karibhou", 5, "User"), // the PR author
			14: issueBy("gregoryhunt", 77, "User"),
		},
		commits: []string{"Fix the widget\n\nSigned-off-by: Karibhou <k@example.com>"},
	}
	m, _ := newMergeCreditorForTest(t, api, nil)
	pr := creditPR("Fixes #10, closes #11, resolves #12, fixes #13 and fixes #14.", "Karibhou")
	credit := m.Prepare(context.Background(), "acme", "widget", pr, "squash")
	if got := fmt.Sprint(credit.Logins()); got != "[Danathar gregoryhunt]" {
		t.Fatalf("Logins = %s, want [Danathar gregoryhunt]; credit=%+v", got, credit)
	}
	if fmt.Sprint(credit.Authors[0].Issues) != "[10 11]" {
		t.Fatalf("Danathar's issues = %v, want [10 11]", credit.Authors[0].Issues)
	}
	want := "Signed-off-by: Karibhou <k@example.com>\n\n" +
		"Co-authored-by: Danathar <12345+Danathar@users.noreply.github.com>\n" +
		"Co-authored-by: gregoryhunt <77+gregoryhunt@users.noreply.github.com>"
	if credit.CommitMessage != want {
		t.Fatalf("CommitMessage =\n%s\nwant\n%s", credit.CommitMessage, want)
	}
	if n := api.issueGets.Load(); n != 5 {
		t.Fatalf("issue GETs = %d, want 5 (one per referenced issue)", n)
	}
	// A second merge attempt for the same head costs no issue lookups.
	m.Prepare(context.Background(), "acme", "widget", pr, "squash")
	if n := api.issueGets.Load(); n != 5 {
		t.Fatalf("issue GETs after re-prepare = %d, want 5 (cached)", n)
	}
}

func TestMergeCreditorPrepareBranchAlreadyCredits(t *testing.T) {
	api := &mergeCreditAPI{
		issues:  map[int]map[string]any{10: issueBy("Danathar", 12345, "User")},
		commits: []string{"Fix\n\nSigned-off-by: hive[bot] <hive@example.com>\nCo-authored-by: Dana <12345+Danathar@users.noreply.github.com>"},
	}
	m, _ := newMergeCreditorForTest(t, api, nil)
	credit := m.Prepare(context.Background(), "acme", "widget", creditPR("Fixes #10", "hive[bot]"), "squash")
	if len(credit.Authors) != 1 || credit.Authors[0].Login != "Danathar" {
		t.Fatalf("Authors = %+v, want Danathar recorded for the evidence", credit.Authors)
	}
	if len(credit.Added) != 0 || credit.CommitMessage != "" {
		t.Fatalf("hive-opened PR already credits the author; got Added=%+v message=%q", credit.Added, credit.CommitMessage)
	}
}

func TestMergeCreditorPrepareOffPaths(t *testing.T) {
	ctx := context.Background()
	t.Run("disabled for the repo makes no request", func(t *testing.T) {
		api := &mergeCreditAPI{issues: map[int]map[string]any{10: issueBy("Danathar", 12345, "User")}}
		m, _ := newMergeCreditorForTest(t, api, func(repo string) bool { return repo != "acme/widget" })
		if credit := m.Prepare(ctx, "acme", "widget", creditPR("Fixes #10", "k"), "squash"); len(credit.Authors) != 0 || credit.CommitMessage != "" {
			t.Fatalf("disabled: got %+v", credit)
		}
		if api.issueGets.Load() != 0 {
			t.Fatal("disabled creditor still read the issue")
		}
	})
	t.Run("nil creditor and nil PR", func(t *testing.T) {
		var m *MergeCreditor
		if m.Enabled("acme/widget") {
			t.Fatal("nil creditor reports enabled")
		}
		if credit := m.Prepare(ctx, "acme", "widget", nil, "squash"); credit.CommitMessage != "" {
			t.Fatalf("nil: got %+v", credit)
		}
	})
	t.Run("rebase has no message to write", func(t *testing.T) {
		api := &mergeCreditAPI{issues: map[int]map[string]any{10: issueBy("Danathar", 12345, "User")}}
		m, _ := newMergeCreditorForTest(t, api, nil)
		if credit := m.Prepare(ctx, "acme", "widget", creditPR("Fixes #10", "k"), "rebase"); credit.CommitMessage != "" || api.issueGets.Load() != 0 {
			t.Fatalf("rebase: got %+v after %d issue GETs", credit, api.issueGets.Load())
		}
	})
	t.Run("no closing reference makes no request", func(t *testing.T) {
		api := &mergeCreditAPI{}
		m, _ := newMergeCreditorForTest(t, api, nil)
		if credit := m.Prepare(ctx, "acme", "widget", creditPR("Refs #10", "k"), "squash"); credit.CommitMessage != "" || api.issueGets.Load() != 0 {
			t.Fatalf("refs: got %+v", credit)
		}
	})
	t.Run("only bots and self leaves the default message", func(t *testing.T) {
		api := &mergeCreditAPI{issues: map[int]map[string]any{
			10: issueBy("github-actions[bot]", 1, "Bot"),
			11: issueBy("k", 2, "User"),
			12: issueBy("release-bot", 3, "User"),
		}}
		m, _ := newMergeCreditorForTest(t, api, nil)
		if credit := m.Prepare(ctx, "acme", "widget", creditPR("Fixes #10 #11\nCloses #12", "K"), "squash"); len(credit.Authors) != 0 || api.listGets.Load() != 0 {
			t.Fatalf("bots/self: got %+v (commit lists %d)", credit, api.listGets.Load())
		}
	})
	t.Run("missing issue is skipped, not fatal", func(t *testing.T) {
		api := &mergeCreditAPI{issues: map[int]map[string]any{11: issueBy("dana", 4, "User")}, commits: []string{"Fix"}}
		m, _ := newMergeCreditorForTest(t, api, nil)
		credit := m.Prepare(ctx, "acme", "widget", creditPR("Fixes #10, fixes #11", "k"), "squash")
		if got := fmt.Sprint(credit.Logins()); got != "[dana]" {
			t.Fatalf("missing issue: Logins = %s", got)
		}
	})
	t.Run("commit read failure keeps the default message", func(t *testing.T) {
		api := &mergeCreditAPI{issues: map[int]map[string]any{10: issueBy("dana", 4, "User")}, commitsHTTP: http.StatusBadGateway}
		m, _ := newMergeCreditorForTest(t, api, nil)
		credit := m.Prepare(ctx, "acme", "widget", creditPR("Fixes #10", "k"), "squash")
		if len(credit.Authors) != 1 || credit.CommitMessage != "" || len(credit.Added) != 0 {
			t.Fatalf("commits error: got %+v", credit)
		}
	})
	t.Run("merge commit credits on the title without reading commits", func(t *testing.T) {
		api := &mergeCreditAPI{issues: map[int]map[string]any{10: issueBy("dana", 4, "User")}}
		m, _ := newMergeCreditorForTest(t, api, nil)
		credit := m.Prepare(ctx, "acme", "widget", creditPR("Fixes #10", "k"), "merge")
		want := "Fix the widget\n\nCo-authored-by: dana <4+dana@users.noreply.github.com>"
		if credit.CommitMessage != want || api.listGets.Load() != 0 {
			t.Fatalf("merge: got %q (commit lists %d), want %q", credit.CommitMessage, api.listGets.Load(), want)
		}
	})
}

// The merge's pr_merged audit names the credited issue authors and the
// stash is consumed by the merge that noted it.
func TestRecordPRMergedAuditCarriesIssueAuthors(t *testing.T) {
	c := NewClientForTest("http://127.0.0.1:0", "acme", []string{"widget"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	var details []string
	c.SetAttributionAudit(func(action, detail, agent string) {
		if action == AuditActionPRMerged {
			details = append(details, detail)
		}
	})
	c.NoteMergeCredit("acme/widget", 7, MergeCredit{Authors: []IssueAuthorCredit{{Login: "gregoryhunt"}, {Login: "Danathar", ID: 1}}})
	c.RecordPRMergedAudit("acme/widget", 7, "squash", "m7", PRAuditPathSweep)
	c.RecordPRMergedAudit("acme/widget", 7, "squash", "m7", PRAuditPathSweep)
	if len(details) != 2 {
		t.Fatalf("pr_merged entries = %d, want 2", len(details))
	}
	if !strings.Contains(details[0], "issue_authors=Danathar,gregoryhunt") {
		t.Fatalf("first pr_merged detail lacks issue_authors: %q", details[0])
	}
	if strings.Contains(details[1], "issue_authors") {
		t.Fatalf("credit stash not consumed by the first audit: %q", details[1])
	}
	// An empty credit clears an earlier note.
	c.NoteMergeCredit("acme/widget", 8, MergeCredit{Authors: []IssueAuthorCredit{{Login: "x"}}})
	c.NoteMergeCredit("acme/widget", 8, MergeCredit{})
	if got := c.takeMergeCredit("acme/widget", 8); len(got.Authors) != 0 {
		t.Fatalf("empty note did not clear: %+v", got)
	}
}

// MergePRVia (hive-merge relay, forge adapter) sends the credited message
// with the squash and honours the off switch.
func TestMergePRViaSendsIssueAuthorCredit(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
	}{
		{name: "credit on", enabled: true},
		{name: "credit off", enabled: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			green := greenFixture()
			var mergeBody map[string]any
			issueGets := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// The PR itself is served here (serveCI's copy has no body);
				// CI, checks and commits come from the green fixture.
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls/42":
					json.NewEncoder(w).Encode(map[string]any{
						"number": 42, "state": "open", "title": "Fix the widget", "body": "Fixes #10",
						"user": map[string]any{"login": "karibhou"},
						"head": map[string]any{"sha": "abc123", "ref": "fix/widget"},
						"base": map[string]any{"ref": "v5"},
					})
				case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/10":
					issueGets++
					json.NewEncoder(w).Encode(issueBy("Danathar", 12345, "User"))
				case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls/42/commits":
					json.NewEncoder(w).Encode([]map[string]any{{"sha": "c0", "commit": map[string]any{"message": "Fix the widget\n\nSigned-off-by: K <k@example.com>"}, "parents": []map[string]any{{"sha": "p"}}}})
				case r.Method == http.MethodPut && r.URL.Path == "/repos/o/r/pulls/42/merge":
					mergeBody = map[string]any{}
					_ = json.NewDecoder(r.Body).Decode(&mergeBody)
					_, _ = io.WriteString(w, `{"sha":"deadbeef","merged":true,"message":"Pull Request successfully merged"}`)
				default:
					if green.serveCI(w, r) {
						return
					}
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()
			c := testMergeClient(t, srv.URL)
			c.SetCreditIssueAuthors(func(string) bool { return tc.enabled })
			var details []string
			c.SetAttributionAudit(func(action, detail, agent string) {
				if action == AuditActionPRMerged {
					details = append(details, detail)
				}
			})
			res, err := c.MergePRVia(context.Background(), "o/r", 42, "squash", "", PRAuditPathForge)
			if err != nil || !res.Merged {
				t.Fatalf("MergePRVia: %+v, %v", res, err)
			}
			msg, _ := mergeBody["commit_message"].(string)
			if !tc.enabled {
				if msg != "" || issueGets != 0 {
					t.Fatalf("credit off: commit_message=%q issueGets=%d", msg, issueGets)
				}
				if len(details) != 1 || strings.Contains(details[0], "issue_authors") {
					t.Fatalf("credit off: audit %v", details)
				}
				return
			}
			want := "Signed-off-by: K <k@example.com>\n\nCo-authored-by: Danathar <12345+Danathar@users.noreply.github.com>"
			if msg != want {
				t.Fatalf("commit_message = %q, want %q", msg, want)
			}
			if len(details) != 1 || !strings.Contains(details[0], "issue_authors=Danathar") {
				t.Fatalf("audit lacks issue_authors: %v", details)
			}
		})
	}
}
