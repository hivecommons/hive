package forge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestForgejoReadAPIReviewsAndComments(t *testing.T) {
	sweep, calls := newSweepAPIServer(t, map[string]string{
		"GET " + repoAPI + "/pulls/7/reviews": `[
			{"id":11,"user":{"login":"alice"},"state":"APPROVED","body":"ok","commit_id":"abc","official":true,"html_url":"r11","comments_count":0},
			{"id":12,"user":{"login":"bob"},"state":"REQUEST_CHANGES","body":"fix","commit_id":"old","stale":true,"comments_count":1},
			{"id":13,"user":null,"state":"COMMENT","comments_count":2}]`,
		"GET " + repoAPI + "/pulls/7/reviews/12/comments": `[{"id":21,"user":{"login":"bob"},"path":"a.go","position":0,"original_position":4,
			"body":"nit","commit_id":"","original_commit_id":"old","html_url":"c21"}]`,
		"GET " + repoAPI + "/issues/7/comments": `[{"id":31,"user":{"login":"carol"},"body":"looks good","html_url":"c31"}]`,
	})
	api := sweep.(ReadAPI)
	ctx := context.Background()

	reviews, err := api.PullReviews(ctx, "widgets", 7, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(reviews) != 3 {
		t.Fatalf("reviews = %+v", reviews)
	}
	if r := reviews[0]; r.Author != "alice" || r.State != "APPROVED" || r.CommitID != "abc" || !r.Official || !r.CommentsRead || len(r.Comments) != 0 {
		t.Errorf("review 11 = %+v", r)
	}
	r := reviews[1]
	if r.Author != "bob" || r.State != "REQUEST_CHANGES" || !r.Stale || !r.CommentsRead || len(r.Comments) != 1 {
		t.Fatalf("review 12 = %+v", r)
	}
	if c := r.Comments[0]; c.Author != "bob" || c.Path != "a.go" || c.Line != 4 || c.CommitID != "old" || c.Body != "nit" {
		t.Errorf("review 12 comment = %+v", c)
	}
	// The comment-read budget (1) is spent on review 12, so review 13's
	// comments are left unread, and its endpoint is never called.
	if r := reviews[2]; r.Author != "" || r.CommentsRead || r.CommentCount != 2 {
		t.Errorf("review 13 = %+v", r)
	}
	for _, c := range *calls {
		if strings.Contains(c, "/reviews/13/") || strings.Contains(c, "/reviews/11/") {
			t.Errorf("read comments the budget or count rules out: %s", c)
		}
	}

	comments, err := api.IssueComments(ctx, "widgets", 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 1 || comments[0].Author != "carol" || comments[0].Body != "looks good" || comments[0].ID != 31 {
		t.Errorf("comments = %+v", comments)
	}
	if _, err := api.PullReviews(ctx, "widgets", 99, 5); err == nil {
		t.Error("reviews of a missing pull request: want an error")
	}
}

func TestForgejoReadAPIRunJobsAndLogs(t *testing.T) {
	long := strings.Repeat("x", 10) + "\nline two\nline three\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "token fj-token" {
			t.Errorf("%s Authorization = %q", r.URL.Path, got)
		}
		switch r.URL.Path {
		case repoAPI + "/actions/runs/5/jobs":
			_, _ = w.Write([]byte(`{"total_count":2,"jobs":[{"id":101,"run_id":5,"name":"build","status":"completed","conclusion":"success"},
				{"id":102,"run_id":5,"name":"test","status":"completed","conclusion":"failure","html_url":"j102"}]}`))
		case repoAPI + "/actions/jobs/102/logs":
			_, _ = w.Write([]byte(long))
		case repoAPI + "/actions/jobs/103/logs":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"token does not have at least one of required scope(s)"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	sweep, err := NewForgejoSweepAPI("fj-token", Options{BaseURL: srv.URL, Org: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	api := sweep.(ReadAPI)
	ctx := context.Background()

	jobs, err := api.RunJobs(ctx, "widgets", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 || jobs[1].ID != 102 || jobs[1].Name != "test" || jobs[1].Conclusion != "failure" || jobs[1].URL != "j102" {
		t.Errorf("jobs = %+v", jobs)
	}

	log, truncated, err := api.JobLog(ctx, "widgets", 102, 1<<20)
	if err != nil || truncated || log != long {
		t.Errorf("JobLog whole = %q %v %v", log, truncated, err)
	}
	// A limit inside the first line keeps the tail from the next whole line.
	log, truncated, err = api.JobLog(ctx, "widgets", 102, len(long)-5)
	if err != nil || !truncated || log != "line two\nline three\n" {
		t.Errorf("JobLog tail = %q %v %v", log, truncated, err)
	}
	if _, _, err := api.JobLog(ctx, "widgets", 103, 100); err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("JobLog refused: err = %v, want the 403", err)
	}
	if _, _, err := api.JobLog(ctx, "widgets", 102, 0); err == nil {
		t.Error("JobLog with no limit: want an error")
	}
}
