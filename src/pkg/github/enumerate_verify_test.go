package github

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// emptyIssueMux serves one repo whose issue list is empty while its PR list
// is not. The repository record reports openIssues open items (GitHub
// counts pull requests in open_issues_count). When issuesHidden is false the
// issue list is served normally.
func emptyIssueMux(t *testing.T, org string, issuesHidden *atomic.Bool, openIssues int, repoStatus int) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	issues := []wireIssue{
		{Number: 1, Title: "issue a", User: wireUser{"alice"}, CreatedAt: hoursAgo(3)},
		{Number: 2, Title: "issue b", User: wireUser{"bob"}, CreatedAt: hoursAgo(2)},
	}
	prs := []wirePR{
		{Number: 20, Title: "pr a", User: wireUser{"carol"}, CreatedAt: hoursAgo(1)},
		{Number: 21, Title: "pr b", User: wireUser{"dave"}, CreatedAt: hoursAgo(1)},
	}
	mux.HandleFunc(fmt.Sprintf("/repos/%s/repo/issues", org), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if issuesHidden.Load() {
			w.Write([]byte(`[]`))
			return
		}
		w.Write(mustMarshal(t, issues))
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/repo/pulls", org), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(mustMarshal(t, prs))
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/repo", org), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if repoStatus != http.StatusOK {
			w.WriteHeader(repoStatus)
			fmt.Fprint(w, `{"message":"unavailable"}`)
			return
		}
		fmt.Fprintf(w, `{"name":"repo","open_issues_count":%d}`, openIssues)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[]`))
	})
	return mux
}

// An issue list that comes back empty beside a populated PR list must not be
// published as "0 issues · N PRs" when the repository record says open
// issues exist (hosted hive, 2026-10-22: every repo showed 0 issues with
// plausible PR counts after an update). With no prior enumeration the PRs
// are kept, the missing issues are reported as an unlisted remainder and the
// repo is marked stale; with a prior enumeration that enumeration is served.
func TestEnumerateActionable_EmptyIssueListBesideOpenPRsIsNotZero(t *testing.T) {
	t.Run("fresh process", func(t *testing.T) {
		var hidden atomic.Bool
		hidden.Store(true)
		// 2 PRs + 25 issues per the repo record.
		srv := httptest.NewServer(emptyIssueMux(t, "org", &hidden, 27, http.StatusOK))
		defer srv.Close()
		c := newTestClient(t, srv, "org", []string{"repo"})

		res, err := c.EnumerateActionable(context.Background())
		if err != nil {
			t.Fatalf("enumerate: %v", err)
		}
		if res.PRs.Count != 2 {
			t.Fatalf("PRs.Count = %d, want 2 (the PR side did not fail)", res.PRs.Count)
		}
		if !res.Stale() || len(res.StaleRepos) != 1 || res.StaleRepos[0] != "repo" {
			t.Fatalf("StaleRepos = %v, want [repo]", res.StaleRepos)
		}
		if res.StaleSince.IsZero() {
			t.Fatalf("StaleSince must be set when the issue count is unverified")
		}
		if got := res.TotalByRepo["repo"]; got != (RepoCounts{Issues: 25, PRs: 2}) {
			t.Fatalf("TotalByRepo[repo] = %+v, want issues 25 (unlisted remainder) prs 2", got)
		}

		// The list recovers: the fresh fetch is published and stale clears.
		hidden.Store(false)
		res, err = c.EnumerateActionable(context.Background())
		if err != nil {
			t.Fatalf("recovered enumerate: %v", err)
		}
		if res.Issues.Count != 2 || res.Stale() {
			t.Fatalf("recovered pass = issues %d stale %v, want 2/false", res.Issues.Count, res.Stale())
		}
	})

	t.Run("serves last-good", func(t *testing.T) {
		var hidden atomic.Bool
		srv := httptest.NewServer(emptyIssueMux(t, "org", &hidden, 4, http.StatusOK))
		defer srv.Close()
		c := newTestClient(t, srv, "org", []string{"repo"})

		first, err := c.EnumerateActionable(context.Background())
		if err != nil {
			t.Fatalf("first enumerate: %v", err)
		}
		if first.Issues.Count != 2 || first.PRs.Count != 2 || first.Stale() {
			t.Fatalf("first pass = issues %d prs %d stale %v, want 2/2/false", first.Issues.Count, first.PRs.Count, first.Stale())
		}

		hidden.Store(true)
		second, err := c.EnumerateActionable(context.Background())
		if err != nil {
			t.Fatalf("second enumerate: %v", err)
		}
		if second.Issues.Count != 2 || second.PRs.Count != 2 {
			t.Fatalf("second pass = issues %d prs %d, want last-good 2/2", second.Issues.Count, second.PRs.Count)
		}
		if !second.Stale() || second.TotalByRepo["repo"] != (RepoCounts{Issues: 2, PRs: 2}) {
			t.Fatalf("second pass stale=%v totals=%+v, want stale last-good 2/2", second.Stale(), second.TotalByRepo["repo"])
		}
	})

	t.Run("record confirms empty", func(t *testing.T) {
		var hidden atomic.Bool
		hidden.Store(true)
		// open_issues_count == PR total: the repo truly has no open issues.
		srv := httptest.NewServer(emptyIssueMux(t, "org", &hidden, 2, http.StatusOK))
		defer srv.Close()
		c := newTestClient(t, srv, "org", []string{"repo"})

		res, err := c.EnumerateActionable(context.Background())
		if err != nil {
			t.Fatalf("enumerate: %v", err)
		}
		if res.Issues.Count != 0 || res.PRs.Count != 2 || res.Stale() {
			t.Fatalf("got issues %d prs %d stale %v, want a clean 0/2/false", res.Issues.Count, res.PRs.Count, res.Stale())
		}
		if got := res.TotalByRepo["repo"]; got != (RepoCounts{Issues: 0, PRs: 2}) {
			t.Fatalf("TotalByRepo[repo] = %+v, want 0/2", got)
		}
	})

	t.Run("record unavailable is not zero", func(t *testing.T) {
		var hidden atomic.Bool
		hidden.Store(true)
		srv := httptest.NewServer(emptyIssueMux(t, "org", &hidden, 0, http.StatusServiceUnavailable))
		defer srv.Close()
		c := newTestClient(t, srv, "org", []string{"repo"})

		res, err := c.EnumerateActionable(context.Background())
		if err != nil {
			t.Fatalf("enumerate: %v", err)
		}
		if res.PRs.Count != 2 || !res.Stale() {
			t.Fatalf("got prs %d stale %v, want 2/true: an unconfirmed empty list is never a clean zero", res.PRs.Count, res.Stale())
		}
	})
}
