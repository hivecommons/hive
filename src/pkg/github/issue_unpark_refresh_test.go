package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIssueDecisionRefreshesSelfEditedItem(t *testing.T) {
	for _, path := range []string{"comment", "dashboard"} {
		t.Run(path, func(t *testing.T) {
			issue := parkedIssue(7, humanComment(11, "maintainer", "/hive decision keep it advisory"))
			server, rec := newUnparkServer(t, "org", "repo", []unparkWireIssue{issue}, map[string]string{"maintainer": "write"})
			c := newTestClient(t, server, "org", []string{"repo"})
			var calls int
			previous := itemRefreshHook()
			SetItemRefreshHook(func(repo string, number int, isPR bool) {
				calls++
				if repo != "org/repo" || number != 7 || isPR {
					t.Errorf("refresh target = %s#%d pr=%t", repo, number, isPR)
				}
				if !labelPresent(rec.added[7], HumanAckLabel) || !labelPresent(rec.removed[7], issueNeedsHumanLabel) {
					t.Errorf("refresh preceded label writes: added=%v removed=%v", rec.added[7], rec.removed[7])
				}
				if labelPresent(rec.removed[7], "hold") {
					t.Error("unpark must preserve hold")
				}
			})
			t.Cleanup(func() { SetItemRefreshHook(previous) })
			if path == "comment" {
				result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{})
				if err != nil || len(result.Unparked) != 1 {
					t.Fatalf("sweep = %+v, %v", result, err)
				}
			} else if _, err := c.RecordOperatorIssueDecision(context.Background(), "org/repo", 7, "maintainer", "keep it advisory"); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("refresh calls = %d, want one", calls)
			}
		})
	}
}

func TestIssueDecisionDoesNotRefreshUnchangedItem(t *testing.T) {
	issue := parkedIssue(7, humanComment(11, "reader", "/hive approve"))
	server, _ := newUnparkServer(t, "org", "repo", []unparkWireIssue{issue}, map[string]string{"reader": "read"})
	c := newTestClient(t, server, "org", []string{"repo"})
	calls := 0
	previous := itemRefreshHook()
	SetItemRefreshHook(func(string, int, bool) { calls++ })
	t.Cleanup(func() { SetItemRefreshHook(previous) })

	result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{})
	if err != nil || len(result.Unparked) != 0 {
		t.Fatalf("unauthorized sweep = %+v, %v", result, err)
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(bad.Close)
	failed := newTestClient(t, bad, "org", []string{"repo"})
	if _, err := failed.clearIssueParkingLabels(context.Background(), "org", "repo", 7, []string{issueNeedsHumanLabel}, "maintainer", true); err == nil {
		t.Fatal("expected label removal failure")
	}
	if calls != 0 {
		t.Fatalf("unchanged or unauthorized issue triggered %d refreshes", calls)
	}
}
