package automerge

import (
	"context"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

func TestBasePreparationProvenanceForEveryAdmittingLane(t *testing.T) {
	for _, lane := range []string{stageLaneSelf, stageLaneTrusted, stageLaneContributor} {
		t.Run(lane, func(t *testing.T) {
			api, srv := newPreparationAPI(t)
			c := preparationEngine(srv.URL, NewBasePreparationStore(""))
			if ev, _, _, err := c.currentBaseGate(context.Background(), "acme/widget", "acme", "widget", 1, "main", "head1", true, "behind", false, lane); err != nil || !ev.BranchUpdated {
				t.Fatalf("admission: %+v %v", ev, err)
			}
			for _, other := range []string{stageLaneSelf, stageLaneTrusted, stageLaneContributor} {
				if other != lane {
					c.recordStage(other, "acme/widget", "acme", "widget", api.prs[1], "untrusted-author-role", false, nil)
				}
			}
			if _, reason, _, err := preparationGate(c, 2, "main", "head2"); err != nil || reason != preparationQueued(1) {
				t.Fatalf("other lane revoked %s owner: %q %v", lane, reason, err)
			}
			c.recordStage(lane, "acme/widget", "acme", "widget", api.prs[1], "required-check-failing:build", false, nil)
			if ev, _, _, err := preparationGate(c, 2, "main", "head2"); err != nil || !ev.BranchUpdated {
				t.Fatalf("owning lane did not release: %+v %v", ev, err)
			}
		})
	}
}

func TestBasePreparationPushBeforeRefreshYieldsUnstartedOwner(t *testing.T) {
	api, srv := newPreparationAPI(t)
	c := preparationEngine(srv.URL, NewBasePreparationStore(""))
	if reason, err := c.admitBasePreparation(context.Background(), "acme", "widget", "main", 1, "head1", stageLaneSelf, false); err != nil || reason != "" {
		t.Fatalf("admission: %q %v", reason, err)
	}
	api.prs[1].Head.SHA = gh.Ptr("author-push")
	api.pending["author-push"] = true
	_, reason, err := c.updateBranchAndSkip(context.Background(), "acme/widget", "acme", "widget", 1, "head1", currentBaseUpdateReason, false)
	if err != nil || reason != basePreparationPrefix+"snapshot-changed" {
		t.Fatalf("stale refresh snapshot: %q %v", reason, err)
	}
	c.recordStage(stageLaneSelf, "acme/widget", "acme", "widget", api.prs[1], reason, false, nil)
	if ev, _, _, err := preparationGate(c, 2, "main", "head2"); err != nil || !ev.BranchUpdated {
		t.Fatalf("unstarted owner blocks qualified successor: %+v %v", ev, err)
	}
	if api.updates[1] != 0 || api.updates[2] != 1 {
		t.Fatalf("unsafe refreshes: %v", api.updates)
	}
}
