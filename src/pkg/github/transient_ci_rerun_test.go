package github

import (
	"context"
	"reflect"
	"testing"

	"github.com/hivecommons/hive/pkg/writeback"
)

// A transient-triaged red PR gets its failed jobs re-run through
// rerun-failed-jobs, never a push (projectbluefin/common#1647), and the
// re-run is an unsolicited write the write-back gate can refuse.
func TestRerunTransientCIFailures(t *testing.T) {
	failedPR := func() []*sharedCIRerunPR {
		return []*sharedCIRerunPR{{Number: 1, SHA: "aaaaaaaaaa", Runs: []sharedCIRerunRun{
			failedRun(11, 1),
			{ID: 12, WorkflowID: 2, RunNumber: 1, Attempt: 1, Status: "completed", Conclusion: "success"},
		}}}
	}
	pr := PullRequest{Repo: "org/repo", Number: 1, HeadSHA: "aaaaaaaaaa", AppAuthored: true, CIStatus: "failure", TransientCITriaged: true}

	t.Run("re-runs the failed workflow runs on the head", func(t *testing.T) {
		srv, fake := newSharedCIRerunFake(t, nil, failedPR())
		c := newTestClient(t, srv, "org", []string{"org/repo"})
		c.SetWritebackGate(writeback.New(writeback.Config{}, writeback.Deps{}, ""))
		got, err := c.RerunTransientCIFailures(context.Background(), pr)
		if err != nil {
			t.Fatalf("rerun: %v", err)
		}
		reruns, posted, _ := fake.snap()
		if !reflect.DeepEqual(reruns, []int64{11}) {
			t.Fatalf("reruns = %v, want [11] (the failed run only)", reruns)
		}
		if !got.Settled || !reflect.DeepEqual(got.RunIDs, []int64{11}) || got.Skipped != "" {
			t.Fatalf("outcome = %+v, want settled with run 11", got)
		}
		if len(posted) != 0 {
			t.Fatalf("a transient re-run must not comment, posted %v", posted)
		}
	})

	t.Run("write-back gate refusal makes no GitHub write", func(t *testing.T) {
		srv, fake := newSharedCIRerunFake(t, nil, failedPR())
		c := newTestClient(t, srv, "org", []string{"org/repo"})
		c.SetWritebackGate(writeback.New(writeback.Config{PerRepo: map[string]writeback.RepoOverride{
			"org/repo": {Mode: writeback.RepoModeOff},
		}}, writeback.Deps{}, ""))
		got, err := c.RerunTransientCIFailures(context.Background(), pr)
		if err != nil {
			t.Fatalf("rerun: %v", err)
		}
		if reruns, _, _ := fake.snap(); len(reruns) != 0 {
			t.Fatalf("gate refused, but reran %v", reruns)
		}
		if !got.Settled || len(got.RunIDs) != 0 || got.Skipped != writeback.ReasonRepoOff {
			t.Fatalf("outcome = %+v, want settled and skipped for %q", got, writeback.ReasonRepoOff)
		}
	})

	t.Run("forbidden re-run settles the attempt", func(t *testing.T) {
		srv, fake := newSharedCIRerunFake(t, nil, failedPR())
		fake.forbidRerun = true
		c := newTestClient(t, srv, "org", []string{"org/repo"})
		got, err := c.RerunTransientCIFailures(context.Background(), pr)
		if err != nil {
			t.Fatalf("rerun: %v", err)
		}
		if !got.Settled || len(got.RunIDs) != 0 || got.Skipped != "rerun-forbidden" {
			t.Fatalf("outcome = %+v, want settled rerun-forbidden", got)
		}
	})

	t.Run("nothing failed to re-run", func(t *testing.T) {
		srv, fake := newSharedCIRerunFake(t, nil, []*sharedCIRerunPR{{Number: 1, SHA: "aaaaaaaaaa"}})
		c := newTestClient(t, srv, "org", []string{"org/repo"})
		got, err := c.RerunTransientCIFailures(context.Background(), pr)
		if err != nil {
			t.Fatalf("rerun: %v", err)
		}
		if reruns, _, _ := fake.snap(); len(reruns) != 0 {
			t.Fatalf("reran %v with no failed runs", reruns)
		}
		if !got.Settled || got.Skipped != "no-failed-runs" {
			t.Fatalf("outcome = %+v, want settled no-failed-runs", got)
		}
	})
}
