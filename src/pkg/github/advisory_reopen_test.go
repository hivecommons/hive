package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// advisoryReopenMux builds a GitHub stub whose repo has NO open advisory issue
// but one closed one, so EnsureAdvisoryIssue must take the reuse path.
// reopenStatus is what the reopen PATCH answers with.
func advisoryReopenMux(t *testing.T, org, repo string, closedNum int, reopenStatus int, created *bool, reopened *bool) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 0, "items": []any{}})
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/labels", org, repo), func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"name": advisoryLabelName})
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/%d/labels", org, repo, closedNum), func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{{"name": advisoryLabelName}})
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/%d", org, repo, closedNum), func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			*reopened = true
			if reopenStatus != http.StatusOK {
				w.WriteHeader(reopenStatus)
				_ = json.NewEncoder(w).Encode(map[string]any{"message": "Resource not accessible by integration"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"number": closedNum, "title": advisoryTitle, "state": "open"})
		}
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues", org, repo), func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			*created = true
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 999, "title": advisoryTitle})
			return
		}
		if r.URL.Query().Get("state") == "closed" {
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"number": closedNum, "title": advisoryTitle, "state": "closed"},
			})
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	})
	return mux
}

// A closed advisory issue must be REOPENED, never duplicated: the digest has to
// keep landing on the issue subscribers already watch, otherwise their comment
// silently stops updating and looks like a wedged hive (#4167).
func TestEnsureAdvisoryIssue_ReopensClosedInsteadOfDuplicating(t *testing.T) {
	org, repo := "testorg", "testrepo"
	var created, reopened bool
	server := httptest.NewServer(advisoryReopenMux(t, org, repo, 55, http.StatusOK, &created, &reopened))
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	num, err := c.EnsureAdvisoryIssue(context.Background(), repo)
	if err != nil {
		t.Fatalf("EnsureAdvisoryIssue: %v", err)
	}
	if num != 55 {
		t.Errorf("issue number = %d, want 55 (the reopened issue)", num)
	}
	if !reopened {
		t.Error("closed advisory issue should have been reopened")
	}
	if created {
		t.Error("a duplicate advisory issue was created despite a reusable closed one")
	}
}

// When the closed issue cannot be reopened, the hive must still end up with a
// LIVE issue to post to — posting into a closed issue would be invisible.
func TestEnsureAdvisoryIssue_CreatesWhenReopenForbidden(t *testing.T) {
	org, repo := "testorg", "testrepo"
	var created, reopened bool
	server := httptest.NewServer(advisoryReopenMux(t, org, repo, 55, http.StatusForbidden, &created, &reopened))
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	num, err := c.EnsureAdvisoryIssue(context.Background(), repo)
	if err != nil {
		t.Fatalf("EnsureAdvisoryIssue: %v", err)
	}
	if !reopened {
		t.Error("reopen should have been attempted")
	}
	if !created {
		t.Error("a new advisory issue should have been created after the reopen failed")
	}
	if num != 999 {
		t.Errorf("issue number = %d, want 999 (the newly created issue)", num)
	}
}

// findClosedAdvisoryIssue must ignore pull requests and unrelated issues.
func TestFindClosedAdvisoryIssue_IgnoresPRsAndOtherTitles(t *testing.T) {
	org, repo := "testorg", "testrepo"
	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues", org, repo), func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"number": 7, "title": advisoryTitle, "pull_request": map[string]any{"url": "x"}},
			{"number": 8, "title": "something else"},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	if num, ok := c.findClosedAdvisoryIssue(context.Background(), org, repo); ok {
		t.Errorf("findClosedAdvisoryIssue = %d, want no match", num)
	}
}
