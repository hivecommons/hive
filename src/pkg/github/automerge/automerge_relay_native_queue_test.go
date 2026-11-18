package automerge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// The merge-request relay enqueues an opted-in repo's PR through the lanes'
// own native-queue path (projectbluefin/utah-packages#1199, 2026-11-18),
// and leaves every other repo to its direct merge.
func TestRelayNativeQueueMerge(t *testing.T) {
	for _, mode := range []string{"opted-in", "empty-expect-sha", "other-repo", "base-not-listed", "head-moved", "mixed-authorship"} {
		t.Run(mode, func(t *testing.T) {
			// The squash convention's head-history read fails here, which
			// keeps the requested squash (ResolveForwardMergeMethod).
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) }))
			defer api.Close()
			c := newAutoMergeSweepClient(api.URL)
			enableNativeQueue(c)
			f := &nativeQueueFake{Transport: c.transport, state: hgithub.NativeMergeQueueState{ID: "PR_7", HeadRefOid: "sha7", IsMergeQueueEnabled: true}}
			c.transport = f
			pr := &gh.PullRequest{
				Number: gh.Ptr(7), NodeID: gh.Ptr("PR_7"), User: &gh.User{Login: gh.Ptr("alice")},
				Base: &gh.PullRequestBranch{Ref: gh.Ptr("main")}, Head: &gh.PullRequestBranch{SHA: gh.Ptr("sha7")},
			}
			expect := "sha7"
			switch mode {
			case "empty-expect-sha":
				expect = ""
			case "other-repo":
				enableNativeQueue(c, NativeMergeQueuePolicy{Enabled: true, Repos: map[string]bool{"acme/other": true}})
			case "base-not-listed":
				enableNativeQueue(c, NativeMergeQueuePolicy{Enabled: true, Bases: map[string]bool{"acme/widget#release": true}})
			case "head-moved":
				f.state.HeadRefOid = "sha8"
			case "mixed-authorship":
				pr.Labels = []*gh.Label{{Name: gh.Ptr(stagelabels.MergeCommitLabel)}}
			}

			result, err := c.RelayNativeQueueMerge(context.Background(), "acme/widget", pr, expect)
			switch mode {
			case "opted-in", "empty-expect-sha":
				if err != nil || !result.OptedIn || !result.Queued || !strings.HasPrefix(result.Reason, nativeQueueReason) || f.enqueues != 1 || f.expectedHead != "sha7" {
					t.Fatalf("result=%+v err=%v enqueues=%d head=%q, want one enqueue pinned to sha7", result, err, f.enqueues, f.expectedHead)
				}
			case "other-repo", "base-not-listed":
				if err != nil || result.OptedIn || result.Queued || result.Reason != "" || f.enqueues != 0 {
					t.Fatalf("result=%+v err=%v enqueues=%d, want not opted in and no queue traffic", result, err, f.enqueues)
				}
			case "head-moved":
				if err != nil || !result.OptedIn || result.Queued || result.Reason != "head-changed-since-eval" || f.enqueues != 0 {
					t.Fatalf("result=%+v err=%v enqueues=%d, want head-changed-since-eval and no enqueue", result, err, f.enqueues)
				}
			case "mixed-authorship":
				if err == nil || !result.OptedIn || result.Queued || result.Reason != nativeQueueReasonMethod || f.enqueues != 0 {
					t.Fatalf("result=%+v err=%v enqueues=%d, want the merge-method refusal of a SQUASH queue", result, err, f.enqueues)
				}
			}
		})
	}
}
