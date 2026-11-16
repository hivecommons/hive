package github

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/hivecommons/hive/pkg/writeback"
)

// projectbluefin/testsuite#1245: `dashboard-build` became required after the
// PR opened, so its head never ran it. The hive merges the base in, pinned to
// the head it judged, through the same gated update as the stale-red refresh.
func TestRefreshMissingCheckPR(t *testing.T) {
	pr := PullRequest{Repo: "acme/widgets", Number: 7, Author: "contributor", HeadSHA: "head1", BaseRef: "main",
		CIStatus: "success", FromFork: true, MaintainerCanModify: true,
		Protection: &ProtectionFacts{RequiredChecksKnown: true, MissingRequiredChecks: []string{"dashboard-build"}}}
	base := StaleRedBaseState{SHA: "base1", Reported: map[string]bool{"dashboard-build": true}}

	t.Run("behind: merges the base in, pinned to the evaluated head", func(t *testing.T) {
		fake := &staleRedRefreshFake{behindBy: 4}
		srv := httptest.NewServer(fake.handler(t))
		defer srv.Close()
		c := newTestClient(t, srv, "acme", []string{"widgets"})
		c.SetWritebackGate(writeback.New(writeback.Config{PerRepo: map[string]writeback.RepoOverride{"acme/widgets": {Mode: writeback.RepoModeFull}}}, writeback.Deps{}, ""))
		got, err := c.RefreshMissingCheckPR(context.Background(), pr, base, "dashboard-build")
		if err != nil {
			t.Fatalf("RefreshMissingCheckPR: %v", err)
		}
		if !got.Updated || !got.Settled || got.BehindBy != 4 {
			t.Fatalf("outcome = %+v, want updated and settled, behind by 4", got)
		}
		if len(fake.compares) != 1 || fake.compares[0] != "base1...head1" {
			t.Fatalf("compares = %v, want base1...head1", fake.compares)
		}
		if len(fake.updates) != 1 || fake.expectHeads[0] != "head1" {
			t.Fatalf("update-branch calls = %v (expected heads %v), want one pinned to head1", fake.updates, fake.expectHeads)
		}
	})

	t.Run("write-back gate refusal makes no call at all", func(t *testing.T) {
		fake := &staleRedRefreshFake{behindBy: 4}
		srv := httptest.NewServer(fake.handler(t))
		defer srv.Close()
		c := newTestClient(t, srv, "acme", []string{"widgets"})
		c.SetWritebackGate(writeback.New(writeback.Config{PerRepo: map[string]writeback.RepoOverride{
			"acme/widgets": {Mode: writeback.RepoModeOff},
		}}, writeback.Deps{}, ""))
		got, err := c.RefreshMissingCheckPR(context.Background(), pr, base, "dashboard-build")
		if err != nil {
			t.Fatalf("RefreshMissingCheckPR: %v", err)
		}
		if got.Updated || !got.Settled || got.Skipped != writeback.ReasonRepoOff {
			t.Fatalf("outcome = %+v, want settled and skipped for %q", got, writeback.ReasonRepoOff)
		}
		if len(fake.compares)+len(fake.updates) != 0 {
			t.Fatalf("gate refused, but called compare %v / update %v", fake.compares, fake.updates)
		}
	})
}
