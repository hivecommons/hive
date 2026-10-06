package dashboard

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/internal/testutil"
	"github.com/hivecommons/hive/pkg/claims"
)

func TestBlockedClaimsLoopReleasesAndStops(t *testing.T) {
	hub, s, ledger := claimsHub(t)
	if _, err := ledger.Claim(claims.Request{Repo: "o/r", Issue: 1, Holder: "worker", Kind: claims.KindAgent}); err != nil {
		t.Fatal(err)
	}
	ledger.SetAdmissionCheck(func(claims.Request) (string, error) { return "needs-human", nil })
	worker := &ContributeWSHub{server: s, logger: hub.logger, stopCh: make(chan struct{})}
	done := make(chan struct{})
	go func() { defer close(done); worker.blockedClaimsLoop(time.Millisecond) }()
	defer func() {
		close(worker.stopCh)
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("poller did not stop")
		}
	}()
	testutil.Eventually(t, time.Second, func() bool {
		_, ok := ledger.Lookup("o/r", 1)
		return !ok
	}, "blocked claim was not released")
}
