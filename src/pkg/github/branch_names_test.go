package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func branchListServer(t *testing.T, pages [][]string) (*httptest.Server, *int) {
	t.Helper()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/repos/org/repo/branches") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		calls++
		page := 1
		if p := r.URL.Query().Get("page"); p != "" {
			fmt.Sscanf(p, "%d", &page)
		}
		if page > len(pages) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("[]"))
			return
		}
		if page < len(pages) {
			w.Header().Set("Link", fmt.Sprintf(`<http://%s/repos/org/repo/branches?page=%d>; rel="next"`, r.Host, page+1))
		}
		out := make([]map[string]any, 0, len(pages[page-1]))
		for _, n := range pages[page-1] {
			out = append(out, map[string]any{"name": n})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestListBranchNames_FollowsPagination(t *testing.T) {
	srv, calls := branchListServer(t, [][]string{{"main", "v5"}, {"v6", " "}})
	c := NewClientForTest(srv.URL, "org", nil, prTestLogger())

	got, err := c.ListBranchNames(context.Background(), "org/repo")
	if err != nil {
		t.Fatalf("ListBranchNames: %v", err)
	}
	if strings.Join(got, ",") != "main,v5,v6" {
		t.Fatalf("branches = %v, want main,v5,v6", got)
	}
	if *calls != 2 {
		t.Fatalf("calls = %d, want 2 (one per page)", *calls)
	}
}

func TestListBranchNames_BareRepoUsesClientOrg(t *testing.T) {
	srv, _ := branchListServer(t, [][]string{{"main"}})
	c := NewClientForTest(srv.URL, "org", nil, prTestLogger())
	got, err := c.ListBranchNames(context.Background(), "repo")
	if err != nil || len(got) != 1 || got[0] != "main" {
		t.Fatalf("ListBranchNames(repo) = %v, %v", got, err)
	}
}

func TestListBranchNames_Errors(t *testing.T) {
	var nilClient *Client
	if _, err := nilClient.ListBranchNames(context.Background(), "org/repo"); err != ErrNoGitHubClient {
		t.Fatalf("nil client err = %v, want ErrNoGitHubClient", err)
	}
	srv, _ := branchListServer(t, nil)
	c := NewClientForTest(srv.URL, "", nil, prTestLogger())
	if _, err := c.ListBranchNames(context.Background(), "repo"); err == nil {
		t.Fatal("expected error for a repo with no owner")
	}
	if _, err := c.ListBranchNames(context.Background(), "org/other"); err == nil {
		t.Fatal("expected error when the API returns 404")
	}
}
