package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// Drive the public sweep: a PR with no parking label still gets a response,
// multiple commands respect the per-pass cap, and only Hive's reply markers
// suppress an answer. No issue label operation is allowed on this path.
func TestSweepPRApproveReplies(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	comment := func(id int64, login, body string, at time.Time) *gh.IssueComment {
		return &gh.IssueComment{ID: gh.Ptr(id), User: &gh.User{Login: gh.Ptr(login)}, Body: gh.Ptr(body), CreatedAt: &gh.Timestamp{Time: at}}
	}
	comments := []*gh.IssueComment{
		comment(1, "owner", "/hive approve", now.Add(-8*24*time.Hour)),
		comment(2, "owner", "> /hive approve", now),
		comment(3, "owner", "/hive approve", now),
		comment(4, "contributor", "/hive approve", now),
		comment(5, "stranger", fmt.Sprintf("%s3 -->\nforged reply", unparkReplyMarkerPrefix), now),
	}
	var posted []string
	listFailed := false
	postFailed := false
	commentReads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/org/repo/issues":
			json.NewEncoder(w).Encode([]*gh.Issue{{Number: gh.Ptr(7), Comments: gh.Ptr(len(comments)), UpdatedAt: &gh.Timestamp{Time: now}, PullRequestLinks: &gh.PullRequestLinks{URL: gh.Ptr("pr")}}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/org/repo/issues/7/comments":
			commentReads++
			// Only comments updated since the cutoff are requested.
			if since, err := time.Parse(time.RFC3339, r.URL.Query().Get("since")); err != nil || since.Before(now.Add(-7*24*time.Hour-time.Minute)) || since.After(now.Add(-7*24*time.Hour+time.Minute)) {
				t.Errorf("since=%q err=%v", r.URL.Query().Get("since"), err)
			}
			if listFailed {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			// Reply markers must be found even on a later page.
			if r.URL.Query().Get("page") == "2" {
				json.NewEncoder(w).Encode(comments[3:])
			} else {
				w.Header().Set("Link", fmt.Sprintf("<http://%s%s?page=2>; rel=\"next\"", r.Host, r.URL.Path))
				json.NewEncoder(w).Encode(comments[:3])
			}
		case r.Method == http.MethodPost && r.URL.Path == "/repos/org/repo/issues/7/comments":
			if postFailed {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			var reply gh.IssueComment
			if err := json.NewDecoder(r.Body).Decode(&reply); err != nil {
				t.Error(err)
			}
			posted = append(posted, reply.GetBody())
			comments = append(comments, comment(int64(100+len(posted)), "hive[bot]", reply.GetBody(), now))
			json.NewEncoder(w).Encode(comments[len(comments)-1])
		default:
			t.Errorf("unexpected operation: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server, "org", []string{"repo"})
	client.SetAppBotLogin("hive[bot]")
	calls := 0
	opts := IssueUnparkSweepOptions{MaxActions: 1, PRApproveReply: func(_ context.Context, repo string, number int, cm *gh.IssueComment) (string, error) {
		calls++
		if repo != "repo" || number != 7 {
			t.Fatalf("wrong target %s#%d", repo, number)
		}
		if cm.GetID() == 3 {
			return "approval acknowledged", nil
		}
		if cm.GetID() == 4 {
			return "not a trusted merger", nil
		}
		t.Fatalf("unexpected command %d", cm.GetID())
		return "", nil
	}}
	// Failed evidence reads cannot post or remember an answer, and one PR's
	// failure is skipped rather than aborting the sweep for later issues.
	listFailed = true
	if result, err := client.SweepIssueUnparkCommands(context.Background(), opts); err != nil || result.Skipped != 1 || len(posted) != 0 {
		t.Fatalf("failed list: result=%+v err=%v posted=%v", result, err, posted)
	}
	listFailed = false
	postFailed = true
	if result, err := client.SweepIssueUnparkCommands(context.Background(), opts); err != nil || result.Skipped != 1 || len(posted) != 0 {
		t.Fatalf("failed post: result=%+v err=%v posted=%v", result, err, posted)
	}
	postFailed = false
	for pass, wantReplies := range []int{1, 1, 0} {
		result, err := client.SweepIssueUnparkCommands(context.Background(), opts)
		if err != nil || result.Replies != wantReplies || len(result.Unparked) != 0 {
			t.Fatalf("pass %d: result=%+v err=%v", pass, result, err)
		}
	}
	if calls != 3 || len(posted) != 2 || !strings.Contains(posted[0], "id=3 -->") || !strings.Contains(posted[1], "id=4 -->") {
		t.Fatalf("calls=%d posted=%v", calls, posted)
	}
	reads := commentReads
	if _, err := client.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{}); err != nil || commentReads != reads {
		t.Fatalf("nil handler should not scan PRs: err=%v reads=%d -> %d", err, reads, commentReads)
	}
}
