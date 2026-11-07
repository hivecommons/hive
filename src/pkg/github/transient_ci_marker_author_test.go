package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The transient-CI marker now re-runs the failed jobs and holds the fix loop
// (hivecommons/hive#17002), so only the hive's own comments may set it: a
// marker anyone else posts on a public PR must not spend Actions minutes or
// pause repairs. The external-CI marker keeps its old any-author reading.
func TestEnrichCIStatus_TransientCITriagedOnlyFromHiveComments(t *testing.T) {
	head := strings.Repeat("a", 40)
	marker := "Infra flake.\n<!-- hive-ci-transient head=" + head + " check=build run=1 -->"
	type comment struct{ login, body string }
	comments := map[int][]comment{
		1: {{"hivecommons-hive[bot]", marker}},
		2: {{"mallory", marker}},
		3: {{"mallory", externalMarker(head, "build", 1)}},
	}
	commentsRE := regexp.MustCompile(`^/repos/org/repo/issues/(\d+)/comments$`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if m := commentsRE.FindStringSubmatch(r.URL.Path); m != nil {
			n, _ := strconv.Atoi(m[1])
			out := []map[string]any{}
			for _, cm := range comments[n] {
				out = append(out, map[string]any{"body": cm.body, "user": map[string]any{"login": cm.login}})
			}
			_ = json.NewEncoder(w).Encode(out)
			return
		}
		if regexp.MustCompile(`/commits/[^/]+/check-runs$`).MatchString(r.URL.Path) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"total_count": 1,
				"check_runs":  []map[string]any{{"id": 1, "name": "build", "status": "completed", "conclusion": "failure"}},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv, "org", []string{"repo"})
	c.SetAppBotLogin("hivecommons-hive[bot]")
	prs := []PullRequest{
		{Repo: "org/repo", Number: 1, HeadSHA: head},
		{Repo: "org/repo", Number: 2, HeadSHA: head},
		{Repo: "org/repo", Number: 3, HeadSHA: head},
	}
	c.EnrichCIStatus(context.Background(), prs)

	for _, pr := range prs {
		if pr.CIStatus != "failure" {
			t.Fatalf("PR %d CIStatus = %q, want failure", pr.Number, pr.CIStatus)
		}
	}
	if !prs[0].TransientCITriaged {
		t.Error("the hive's own transient marker did not triage the head")
	}
	if prs[1].TransientCITriaged {
		t.Error("a transient marker from someone outside the hive triaged the head (it would trigger a re-run)")
	}
	if !prs[2].ExternalCIDeferred {
		t.Error("the external-CI marker must keep reading every author's comments")
	}
}
