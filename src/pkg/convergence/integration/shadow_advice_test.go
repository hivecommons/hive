package integration

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func TestAdvisoryUsesLiveClaimScopesWithoutTakingReservations(t *testing.T) {
	store := testStore(t)
	a, b, otherBase := testIntent(1), testIntent(2), testIntent(3)
	a.Paths, b.Paths = []string{"a.go"}, []string{"b.go"}
	a.Contracts, b.Contracts = []string{"wire/v1"}, []string{"wire/v1"}
	a.Relations = []RelationAssessment{{Key: b.Key, Kind: "semantic", Disposition: "false-positive", Provenance: "owner-reviewed"}}
	otherBase.Base = "release"
	for _, intent := range []Intent{a, b, otherBase} {
		putIntent(t, store, intent)
	}
	now := epoch.Add(time.Hour)
	for _, live := range [][]string{nil, {a.Key, b.Key, b.Key, otherBase.Key, "missing"}} {
		decision, err := store.Decide(configFor(config.IntegrationAdvisory), Input{Key: a.Key, Repo: a.Repo, Base: a.Base, Now: now, LiveKeys: live, PlanApproved: true, PlanRevision: 1})
		if err != nil || !decision.Allowed {
			t.Fatalf("shadow advice blocked: %+v %v", decision, err)
		}
		if len(live) > 0 && (len(decision.Hints) != 1 || !strings.Contains(decision.Hints[0], "semantic interaction") || !strings.Contains(decision.Hints[0], "false-positive")) {
			t.Fatalf("lost disjoint-file semantic advice: %+v", decision)
		}
		if len(live) == 0 && len(decision.Hints) != 0 {
			t.Fatal("unclaimed intent treated as active")
		}
	}
	if len(store.Snapshot(now).Reservations) != 0 {
		t.Fatal("advisory created an exclusive reservation")
	}
}
