package github

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestEnumerateActionableKeepsSidelinedPRDetails proves that the existing PR
// list response also preserves the open drafts and exempt PRs that the queue
// must not consume, so display consumers can list them without another call.
func TestEnumerateActionableKeepsSidelinedPRDetails(t *testing.T) {
	org, repo := "org", "repo"
	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues", org, repo), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/pulls", org, repo), func(w http.ResponseWriter, r *http.Request) {
		prs := []wirePR{
			{Number: 1, Title: "exempt PR", User: wireUser{"pat"}, Labels: []wireLabel{{Name: "LFX"}}, CreatedAt: hoursAgo(1)},
			{Number: 2, Title: "draft PR", User: wireUser{"zoe"}, Draft: true, CreatedAt: hoursAgo(2)},
			{Number: 3, Title: "held draft", User: wireUser{"pat"}, Draft: true, Labels: []wireLabel{{Name: "hold"}}, CreatedAt: hoursAgo(3)},
			{Number: 4, Title: "actionable PR", User: wireUser{"pat"}, CreatedAt: hoursAgo(4)},
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(mustMarshal(t, prs))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := newTestClient(t, server, org, []string{repo})
	client.SetExemptLabels([]string{"LFX"})
	result, err := client.EnumerateActionable(context.Background())
	if err != nil {
		t.Fatalf("EnumerateActionable: %v", err)
	}

	if result.PRs.Count != 1 || len(result.PRs.Items) != 1 || result.PRs.Items[0].Number != 4 {
		t.Fatalf("actionable PRs = count %d items %+v, want only #4", result.PRs.Count, result.PRs.Items)
	}
	details, ok := result.WorkDetailsByRepo[repo]
	if !ok {
		t.Fatalf("missing work details for %q: %+v", repo, result.WorkDetailsByRepo)
	}
	if len(details.PRs) != 3 {
		t.Fatalf("sidelined PRs = %d, want 3: %+v", len(details.PRs), details.PRs)
	}
	byNumber := make(map[int]RepoWorkPR, len(details.PRs))
	for _, pr := range details.PRs {
		byNumber[pr.Number] = pr
	}
	for _, tc := range []struct {
		number int
		bucket string
		draft  bool
		reason string
	}{
		{number: 1, bucket: workBucketPRFiltered, reason: "Exempt label"},
		{number: 2, bucket: workBucketPRDraft, draft: true, reason: "Draft"},
		{number: 3, bucket: workBucketPRDraft, draft: true, reason: "Held draft"},
	} {
		pr, ok := byNumber[tc.number]
		if !ok || pr.Bucket != tc.bucket || pr.Draft != tc.draft || pr.Reason != tc.reason || pr.Title == "" || pr.CreatedAt.IsZero() {
			t.Fatalf("sidelined #%d = %+v, want bucket=%q draft=%v reason=%q", tc.number, pr, tc.bucket, tc.draft, tc.reason)
		}
	}
	if result.WorkBreakdownByRepo[repo].PRs.Filtered != 1 || result.WorkBreakdownByRepo[repo].PRs.Draft != 1 || result.WorkBreakdownByRepo[repo].PRs.Hold != 1 {
		t.Fatalf("PR breakdown = %+v", result.WorkBreakdownByRepo[repo].PRs)
	}
}
