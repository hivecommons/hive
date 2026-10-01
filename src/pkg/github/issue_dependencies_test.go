package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// wireBlocker is one entry the fake serves from .../dependencies/blocked_by.
type wireBlocker struct {
	Number        int    `json:"number"`
	State         string `json:"state"`
	HTMLURL       string `json:"html_url,omitempty"`
	RepositoryURL string `json:"repository_url,omitempty"`
}

func blockedSummary(n int) *wireDepsSummary { return &wireDepsSummary{BlockedBy: n, TotalBlockedBy: n} }

// TestEnumerateActionable_BlockedByBecomesDependsOn pins the read half of
// #9839: GitHub "blocked by" links on an open issue become Issue.DependsOn
// edges, resolved iff the blocker is closed; only issues whose summary reports
// an open blocker are fetched; cross-repo blockers keep their full repo; an
// unreadable blocker list fails open with the issue unblocked; and a mutual
// block is dropped from both sides rather than hiding both forever.
func TestEnumerateActionable_BlockedByBecomesDependsOn(t *testing.T) {
	org, repo := "org", "repo"
	issues := []wireIssue{
		{Number: 20, Title: "blocked by open 21", User: wireUser{"u"}, CreatedAt: hoursAgo(1), IssueDependenciesSummary: blockedSummary(1)},
		{Number: 21, Title: "the blocker", User: wireUser{"u"}, CreatedAt: hoursAgo(2)},
		{Number: 22, Title: "blocked by closed 23", User: wireUser{"u"}, CreatedAt: hoursAgo(1), IssueDependenciesSummary: blockedSummary(1)},
		{Number: 24, Title: "blocked cross-repo", User: wireUser{"u"}, CreatedAt: hoursAgo(1), IssueDependenciesSummary: blockedSummary(1)},
		{Number: 25, Title: "blocker list unreadable", User: wireUser{"u"}, CreatedAt: hoursAgo(1), IssueDependenciesSummary: blockedSummary(1)},
		{Number: 26, Title: "summary says zero", User: wireUser{"u"}, CreatedAt: hoursAgo(1), IssueDependenciesSummary: &wireDepsSummary{BlockedBy: 0, TotalBlockedBy: 2}},
		{Number: 30, Title: "mutual a", User: wireUser{"u"}, CreatedAt: hoursAgo(1), IssueDependenciesSummary: blockedSummary(1)},
		{Number: 31, Title: "mutual b", User: wireUser{"u"}, CreatedAt: hoursAgo(1), IssueDependenciesSummary: blockedSummary(1)},
	}
	blockers := map[int][]wireBlocker{
		20: {{Number: 21, State: "open", RepositoryURL: "https://api.github.com/repos/org/repo"}},
		22: {{Number: 23, State: "closed", RepositoryURL: "https://api.github.com/repos/org/repo"}},
		24: {{Number: 5, State: "open", HTMLURL: "https://github.com/other/x/issues/5"}},
		30: {{Number: 31, State: "open", RepositoryURL: "https://api.github.com/repos/org/repo"}},
		31: {{Number: 30, State: "open", RepositoryURL: "https://api.github.com/repos/org/repo"}},
	}
	fetched := map[int]int{}
	mux := buildMux(t, org, repo, issues, nil)
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/", org, repo), func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/dependencies/blocked_by") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var num int
		fmt.Sscanf(strings.TrimPrefix(r.URL.Path, fmt.Sprintf("/repos/%s/%s/issues/", org, repo)), "%d/", &num)
		fetched[num]++
		if num == 25 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(mustMarshal(t, blockers[num]))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	result, err := c.EnumerateActionable(context.Background())
	if err != nil {
		t.Fatalf("EnumerateActionable: %v", err)
	}
	byNumber := map[int]Issue{}
	for _, is := range result.Issues.Items {
		byNumber[is.Number] = is
	}
	if len(byNumber) != len(issues) {
		t.Fatalf("actionable = %d, want %d: blocked issues stay in the actionable set (the kick list and admission decide what to offer)", len(byNumber), len(issues))
	}

	if got := byNumber[20].DependsOn; len(got) != 1 || got[0].Key != "repo#21" || got[0].Resolved {
		t.Errorf("#20 depends_on = %+v, want [{repo#21 false}]", got)
	}
	if !byNumber[20].IsBlocked() || strings.Join(byNumber[20].OpenBlockers(), ",") != "repo#21" {
		t.Errorf("#20 should be blocked by repo#21: blocked=%v open=%v", byNumber[20].IsBlocked(), byNumber[20].OpenBlockers())
	}
	if got := byNumber[22].DependsOn; len(got) != 1 || got[0].Key != "repo#23" || !got[0].Resolved {
		t.Errorf("#22 depends_on = %+v, want [{repo#23 true}]", got)
	}
	if byNumber[22].IsBlocked() {
		t.Errorf("#22's only blocker is closed; it must be ready")
	}
	if got := byNumber[24].DependsOn; len(got) != 1 || got[0].Key != "other/x#5" || got[0].Resolved {
		t.Errorf("#24 depends_on = %+v, want [{other/x#5 false}]", got)
	}
	if byNumber[25].IsBlocked() || len(byNumber[25].DependsOn) != 0 {
		t.Errorf("#25's blocker list was unreadable; it must fail OPEN, got %+v", byNumber[25].DependsOn)
	}
	if byNumber[30].IsBlocked() || byNumber[31].IsBlocked() {
		t.Errorf("mutual block #30<->#31 must be dropped from both: %+v / %+v", byNumber[30].DependsOn, byNumber[31].DependsOn)
	}

	// Only issues whose summary reported an open blocker were fetched.
	for _, n := range []int{20, 22, 24, 25, 30, 31} {
		if fetched[n] != 1 {
			t.Errorf("blocked_by fetched %d times for #%d, want 1", fetched[n], n)
		}
	}
	for _, n := range []int{21, 26} {
		if fetched[n] != 0 {
			t.Errorf("#%d reports no open blocker but was fetched %d times", n, fetched[n])
		}
	}
}

func TestBlockerRepoFromURLs(t *testing.T) {
	cases := []struct{ repoURL, htmlURL, wantOwner, wantRepo string }{
		{"https://api.github.com/repos/org/repo", "", "org", "repo"},
		{"https://ghe.example/api/v3/repos/Org/Repo", "", "Org", "Repo"},
		{"", "https://github.com/other/x/issues/5", "other", "x"},
		{"", "https://github.com/other/x/pull/5", "", ""},
		{"", "", "", ""},
	}
	for _, tc := range cases {
		o, r := blockerRepoFromURLs(tc.repoURL, tc.htmlURL)
		if o != tc.wantOwner || r != tc.wantRepo {
			t.Errorf("blockerRepoFromURLs(%q,%q) = %q/%q, want %q/%q", tc.repoURL, tc.htmlURL, o, r, tc.wantOwner, tc.wantRepo)
		}
	}
}

// TestIssueRequestWatcher_LinksBlockedBy pins the write half of #9839: an
// issue create carrying blocked_by resolves each blocker to its database id
// and posts a "blocked by" dependency on the new issue; a blocker that cannot
// be resolved is reported without failing the create or the other links.
func TestIssueRequestWatcher_LinksBlockedBy(t *testing.T) {
	created := 0
	var posted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case r.Method == "GET" && strings.Contains(p, "/labels/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"name":"x"}`)
		case r.Method == "POST" && strings.HasSuffix(p, "/labels"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"name":"x"}`)
		case r.Method == "GET" && strings.HasSuffix(p, "/issues"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `[]`)
		case r.Method == "POST" && strings.HasSuffix(p, "/issues"):
			created++
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":990099,"number":99,"html_url":"https://github.example/o/r/issues/99"}`)
		case r.Method == "GET" && strings.HasSuffix(p, "/issues/10"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":1010,"number":10,"state":"open"}`)
		case r.Method == "GET" && strings.HasSuffix(p, "/issues/11"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":1011,"number":11,"state":"open"}`)
		case r.Method == "GET" && strings.HasSuffix(p, "/issues/12"):
			w.WriteHeader(http.StatusNotFound)
		case r.Method == "POST" && strings.HasSuffix(p, "/dependencies/blocked_by"):
			body, _ := io.ReadAll(r.Body)
			posted = append(posted, p+" "+strings.TrimSpace(string(body)))
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	dir := withIssueDir(t)

	reqPath, err := WriteIssueRequest(dir, IssueRequest{
		Repo: "o/r", Title: "[scanner] second child", Body: "Part of #100; lands after #10", Agent: "scanner",
		BlockedBy: []int{10, 11, 12, 10, 99},
	})
	if err != nil {
		t.Fatal(err)
	}

	c.ProcessIssueRequestsOnce(context.Background())

	if created != 1 {
		t.Fatalf("expected 1 issue created, got %d", created)
	}
	if len(posted) != 2 ||
		!strings.Contains(posted[0], "/repos/o/r/issues/99/dependencies/blocked_by {\"issue_id\":1010}") ||
		!strings.Contains(posted[1], "/repos/o/r/issues/99/dependencies/blocked_by {\"issue_id\":1011}") {
		t.Fatalf("blocked_by posts = %q, want ids 1010 and 1011 on #99 (duplicates and self skipped, missing #12 not posted)", posted)
	}

	resultData, err := os.ReadFile(strings.TrimSuffix(reqPath, ".json") + ".result.json")
	if err != nil {
		t.Fatalf("reading result file: %v", err)
	}
	var resp IssueResponse
	if err := json.Unmarshal(resultData, &resp); err != nil {
		t.Fatalf("unmarshaling result: %v", err)
	}
	if !resp.OK || resp.Number != 99 {
		t.Fatalf("expected OK create of #99, got %+v", resp)
	}
	if fmt.Sprint(resp.BlockedByLinked) != "[10 11]" {
		t.Errorf("blocked_by_linked = %v, want [10 11]", resp.BlockedByLinked)
	}
	if len(resp.BlockedByErrors) != 1 || !strings.HasPrefix(resp.BlockedByErrors[0], "#12:") {
		t.Errorf("blocked_by_errors = %v, want one entry for #12", resp.BlockedByErrors)
	}
}
