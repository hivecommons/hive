package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/forge"
)

func TestForgeReadRelayReviews(t *testing.T) {
	h := newReadHarness(t)
	h.api.reviews = []forge.Review{
		{ID: 11, Author: "alice", State: "APPROVED", CommitID: "abc", Official: true, CommentsRead: true},
		{ID: 12, Author: "bob", State: "REQUEST_CHANGES", CommitID: "old", Stale: true, CommentCount: 1, CommentsRead: true,
			Comments: []forge.ReviewComment{{ID: 21, Author: "bob", Path: "a.go", Line: 4, Body: "nit"}}},
		{ID: 13, Author: "carol", State: "APPROVED", Dismissed: true, CommentCount: 2},
		{ID: 14, Author: "dave", State: "COMMENT", CommentsRead: true},
	}
	var view struct {
		Number  int              `json:"number"`
		Reviews []map[string]any `json:"reviews"`
		Forge   string           `json:"forge"`
	}
	resp := h.readView(t, forgeReadRequest{Agent: "worker", Repo: "widgets", Kind: "reviews", Number: 50}, &view)
	if resp.Kind != "reviews" || view.Number != 50 || view.Forge != "forgejo" || len(view.Reviews) != 4 {
		t.Fatalf("resp = %+v, view = %+v", resp, view)
	}
	if h.api.maxCommentReads != forgeReadMaxReviewCommentReads {
		t.Errorf("comment-read budget = %d, want %d", h.api.maxCommentReads, forgeReadMaxReviewCommentReads)
	}
	for i, want := range []string{"APPROVED", "CHANGES_REQUESTED", "DISMISSED", "COMMENTED"} {
		if view.Reviews[i]["state"] != want {
			t.Errorf("review %d state = %v, want %s", i, view.Reviews[i]["state"], want)
		}
	}
	r := view.Reviews[1]
	if r["author"] != "bob" || r["commit"] != "old" || r["stale"] != true || r["commentsRead"] != true {
		t.Errorf("review 12 = %v", r)
	}
	comments, _ := r["comments"].([]any)
	if len(comments) != 1 || fmt.Sprint(comments[0].(map[string]any)["path"]) != "a.go" {
		t.Errorf("review 12 comments = %v", r["comments"])
	}
	if view.Reviews[2]["commentsRead"] != false {
		t.Errorf("review 13 commentsRead = %v, want false", view.Reviews[2]["commentsRead"])
	}
}

func TestForgeReadRelayComments(t *testing.T) {
	h := newReadHarness(t)
	h.api.comments = []forge.IssueComment{{ID: 31, Author: "carol", Body: "looks good", URL: "c31"}}
	var view struct {
		Comments []map[string]any `json:"comments"`
	}
	h.readView(t, forgeReadRequest{Agent: "worker", Repo: "widgets", Kind: "comments", Number: 50}, &view)
	if len(view.Comments) != 1 || view.Comments[0]["author"] != "carol" || view.Comments[0]["body"] != "looks good" || view.Comments[0]["url"] != "c31" {
		t.Errorf("comments view = %v", view.Comments)
	}
	// No comments is an empty list, not null.
	h = newReadHarness(t)
	var raw map[string]any
	h.readView(t, forgeReadRequest{Agent: "worker", Repo: "widgets", Kind: "comments", Number: 50}, &raw)
	if got, ok := raw["comments"].([]any); !ok || len(got) != 0 {
		t.Errorf("comments of an uncommented issue = %v, want []", raw["comments"])
	}
}

func TestForgeReadRelayRunLogs(t *testing.T) {
	jobs := []forge.ActionJob{
		{ID: 101, Name: "build", Status: "completed", Conclusion: "success"},
		{ID: 102, Name: "test", Status: "completed", Conclusion: "failure"},
	}
	logs := map[int64]string{101: "built\n", 102: "FAIL TestX\n"}
	type runView struct {
		RunID int              `json:"runId"`
		Jobs  []map[string]any `json:"jobs"`
	}

	h := newReadHarness(t)
	h.api.jobs, h.api.logs = jobs, logs
	var all runView
	h.readView(t, forgeReadRequest{Agent: "worker", Repo: "widgets", Kind: "run-log", Number: 5}, &all)
	if all.RunID != 5 || len(all.Jobs) != 2 || all.Jobs[0]["log"] != "built\n" || all.Jobs[1]["log"] != "FAIL TestX\n" ||
		all.Jobs[1]["conclusion"] != "FAILURE" || all.Jobs[1]["logTruncated"] != false {
		t.Errorf("run-log view = %+v", all)
	}
	if len(h.api.logLimits) != 2 || h.api.logLimits[0] != forgeReadJobLogLimit {
		t.Errorf("job log limits = %v, want %d per job", h.api.logLimits, forgeReadJobLogLimit)
	}

	h = newReadHarness(t)
	h.api.jobs, h.api.logs = jobs, logs
	var failed runView
	h.readView(t, forgeReadRequest{Agent: "worker", Repo: "widgets", Kind: "run-log-failed", Number: 5}, &failed)
	if len(failed.Jobs) != 2 || failed.Jobs[0]["log"] != nil || failed.Jobs[1]["log"] != "FAIL TestX\n" || len(h.api.logLimits) != 1 {
		t.Errorf("run-log-failed view = %+v (log reads %v)", failed, h.api.logLimits)
	}

	h = newReadHarness(t)
	h.api.logs = map[int64]string{102: "FAIL TestX\n"}
	var job map[string]any
	h.readView(t, forgeReadRequest{Agent: "worker", Repo: "widgets", Kind: "job-log", Number: 102}, &job)
	if job["jobId"] != float64(102) || job["log"] != "FAIL TestX\n" || job["logTruncated"] != false || job["forge"] != "forgejo" {
		t.Errorf("job-log view = %v", job)
	}
}

// A run's logs share forgeReadRunLogLimit: once it is spent, the remaining
// jobs are listed with logOmitted instead of a log.
func TestForgeReadRelayRunLogBudget(t *testing.T) {
	h := newReadHarness(t)
	big := strings.Repeat("y", forgeReadJobLogLimit+10)
	h.api.logs = map[int64]string{}
	for i := int64(1); i <= 3; i++ {
		h.api.jobs = append(h.api.jobs, forge.ActionJob{ID: i, Name: fmt.Sprint("job", i), Conclusion: "failure"})
		h.api.logs[i] = big
	}
	var view struct {
		Jobs []map[string]any `json:"jobs"`
	}
	h.readView(t, forgeReadRequest{Agent: "worker", Repo: "widgets", Kind: "run-log", Number: 5}, &view)
	if len(view.Jobs) != 3 {
		t.Fatalf("jobs = %d", len(view.Jobs))
	}
	total := 0
	for i, j := range view.Jobs[:2] {
		log, _ := j["log"].(string)
		total += len(log)
		if j["logTruncated"] != true {
			t.Errorf("job %d logTruncated = %v, want true", i, j["logTruncated"])
		}
	}
	if total > forgeReadRunLogLimit {
		t.Errorf("run view carries %d log bytes, over the %d limit", total, forgeReadRunLogLimit)
	}
	if view.Jobs[2]["log"] != nil || view.Jobs[2]["logOmitted"] == nil {
		t.Errorf("third job = %v, want no log and logOmitted", view.Jobs[2])
	}
}
