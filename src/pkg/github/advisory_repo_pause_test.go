package github

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/writeback"
)

// The advisory digest must honour an operator repo pause (#6203): on the
// a1-vllm-spyre spoke every agent and both repos were paused, the automerge
// sweeps correctly stood down, yet the governor still edited the pinned
// "## 🐝 Advisory Digest" comment on torch-spyre#406 at startup. The digest
// is a write like any other; a paused repo gets neither an edit nor a new
// comment, and the first cycle after the pause lifts posts normally.
func TestPostAdvisoryDigest_HonoursRepoPause(t *testing.T) {
	org, repo := "testorg", "testrepo"
	digest := advisoryDigestPrefix + "\nFrozen for governor mode SURGE"

	cases := []struct {
		name      string
		pausedVia func(c *Client, paused *bool)
		wantBy    string
	}{
		{
			name: "bare predicate (SetRepoPausedFunc)",
			pausedVia: func(c *Client, paused *bool) {
				c.SetRepoPausedFunc(func(string) bool { return *paused })
			},
		},
		{
			name: "write-back gate with provenance",
			pausedVia: func(c *Client, paused *bool) {
				c.SetWritebackGate(writeback.New(writeback.Config{}, writeback.Deps{
					RepoPause: func(string) (writeback.RepoPauseInfo, bool) {
						return writeback.RepoPauseInfo{By: "clubanderson", Reason: "release freeze"}, *paused
					},
				}, ""))
			},
			wantBy: "clubanderson",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newDigestCountingServer(t, org, repo)
			c := newTestClient(t, srv.Server, org, []string{repo})
			paused := true
			tc.pausedVia(c, &paused)

			// Paused across several cycles: nothing reaches the forge, not
			// even the comment scan, and the caller is told why.
			for i := 0; i < 3; i++ {
				err := c.PostAdvisoryDigest(context.Background(), repo, 10, digest)
				var perr *RepoPausedError
				if !errors.As(err, &perr) {
					t.Fatalf("cycle %d: err = %v, want *RepoPausedError", i, err)
				}
				if perr.Repo != repo || perr.By != tc.wantBy {
					t.Fatalf("cycle %d: perr = %+v, want repo %q by %q", i, perr, repo, tc.wantBy)
				}
			}
			if srv.editCalls != 0 || srv.listCalls != 0 {
				t.Fatalf("paused: editCalls=%d listCalls=%d, want 0/0", srv.editCalls, srv.listCalls)
			}

			// Pause lifted: the next cycle posts exactly like the first after
			// boot (the pause left no hash state behind).
			paused = false
			if err := c.PostAdvisoryDigest(context.Background(), repo, 10, digest); err != nil {
				t.Fatalf("after unpause: %v", err)
			}
			if srv.editCalls != 1 {
				t.Fatalf("after unpause: editCalls = %d, want 1", srv.editCalls)
			}
		})
	}
}

func TestPostAdvisoryDigest_UnpausedRepoUnchanged(t *testing.T) {
	org, repo := "testorg", "testrepo"
	srv := newDigestCountingServer(t, org, repo)
	c := newTestClient(t, srv.Server, org, []string{repo})
	// Some OTHER repo is paused; this one is not.
	c.SetRepoPausedFunc(pauseAll("some-other-repo"))
	c.SetWritebackGate(writeback.New(writeback.Config{}, writeback.Deps{
		RepoPause: func(r string) (writeback.RepoPauseInfo, bool) { return writeback.RepoPauseInfo{}, r == "some-other-repo" },
	}, ""))

	if err := c.PostAdvisoryDigest(context.Background(), repo, 10, advisoryDigestPrefix+"\nlive"); err != nil {
		t.Fatalf("unpaused post: %v", err)
	}
	if srv.editCalls != 1 {
		t.Fatalf("unpaused: editCalls = %d, want 1", srv.editCalls)
	}
}

func TestRepoPausedError_Message(t *testing.T) {
	e := &RepoPausedError{Repo: "org/r", By: "ops", Reason: "freeze"}
	for _, want := range []string{"org/r", "ops", "freeze", "resume"} {
		if !strings.Contains(e.Error(), want) {
			t.Errorf("Error() = %q, want it to mention %q", e.Error(), want)
		}
	}
}
