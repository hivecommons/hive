package automerge

import (
	"context"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// A parked contributor PR (needs-rebase, hold, excluded label) short-cuts
// the lane before admitContributor runs, so the stage pass used to fall back
// to the role/<tier> label a previous pass wrote and a registry tier change
// never reached it: hivecommons/hive#14822 (needs-rebase, stale role) and
// #14947 (deep-evaluated each sweep, fresh role) by the same author showed
// different tiers. The lane now notes the CURRENT registry tier on the
// prefilter path too, every sweep.
func TestContributorLanePrefilteredPRNotesCurrentRegistryTierEachSweep(t *testing.T) {
	fx := validContributorFixture()
	fx.labels = []string{"needs-rebase", "role/contributor"}
	api := newContributorLaneAPI(t, &fx)
	defer api.Close()
	policy := defaultContributorLanePolicy()
	policy.RegistryTiersSufficient = true
	clock := &contributorLaneClock{at: time.Date(2026, 10, 28, 17, 59, 0, 0, time.UTC)}
	tiers := map[string]string{contributorLaneTestAuthor: "contributor"}
	tracker := NewStageTracker()
	engine := newContributorLaneEngine(api.URL, policy, clock, Options{
		ContributorRegistryTier: registryTiers(tiers),
		StageTracker:            tracker,
		StageLabelPolicy:        func() StageLabelPolicy { return StageLabelPolicy{Enabled: true} },
	})
	ctx := context.Background()
	key := stageKey("acme", "widget", 7)

	roleNoted := func() stagelabels.Role {
		tracker.mu.Lock()
		defer tracker.mu.Unlock()
		return tracker.details[key].Role
	}

	_, reason, err := engine.trySweepContributorLanePR(ctx, "widget", "acme", "widget", 7, policy)
	if err != nil || reason != "label:needs-rebase" {
		t.Fatalf("parked pass reason=%q err=%v, want the prefilter short-cut", reason, err)
	}
	if got := roleNoted(); got != stagelabels.RoleContributor {
		t.Fatalf("role noted on the parked pass = %v, want contributor (registry)", got)
	}

	// The operator promotes the author in the registry. Nothing else about
	// the PR changes — it is still parked on needs-rebase — yet the very
	// next sweep must carry the new tier.
	tiers[contributorLaneTestAuthor] = "trusted"
	tracker.drain()
	if _, reason, err = engine.trySweepContributorLanePR(ctx, "widget", "acme", "widget", 7, policy); err != nil || reason != "label:needs-rebase" {
		t.Fatalf("second parked pass reason=%q err=%v", reason, err)
	}
	if got := roleNoted(); got != stagelabels.RoleTrusted {
		t.Fatalf("role noted after promotion = %v, want trusted: the parked PR kept a stale tier", got)
	}

	// And a demotion is picked up the same way.
	tiers[contributorLaneTestAuthor] = "newcomer"
	tracker.drain()
	if _, _, err = engine.trySweepContributorLanePR(ctx, "widget", "acme", "widget", 7, policy); err != nil {
		t.Fatal(err)
	}
	if got := roleNoted(); got != stagelabels.RoleNewcomer {
		t.Fatalf("role noted after demotion = %v, want newcomer", got)
	}

	// An author with no registry profile notes nothing on the prefilter
	// path: the marker/label evidence stays authoritative for them.
	delete(tiers, contributorLaneTestAuthor)
	tracker.drain()
	if _, _, err = engine.trySweepContributorLanePR(ctx, "widget", "acme", "widget", 7, policy); err != nil {
		t.Fatal(err)
	}
	if got := roleNoted(); got != stagelabels.RoleNone {
		t.Fatalf("role noted for an unregistered author = %v, want none", got)
	}
}

// The stage pass must let a tier a lane read from the registry THIS pass
// beat the role/<tier> label a previous pass wrote — on weak stages too, where
// the label used to be merged first and a trusted label stuck through a
// demotion. A lane that merely cannot vouch for the author (untrusted) still
// does not override a label another lane wrote (#13637 flapping guard).
func TestResolveStageFreshLaneTierBeatsStaleRoleLabelOnWeakStage(t *testing.T) {
	cases := []struct {
		name string
		list []stageObservation
		want stagelabels.Role
	}{
		{
			name: "demotion: trusted label, registry now contributor",
			list: []stageObservation{
				{Lane: stageLaneTrusted, Reason: "untrusted-author-role", Labels: []string{"role/trusted", "needs-rebase"}},
				{Lane: stageLaneContributor, Reason: "label:needs-rebase", Labels: []string{"role/trusted", "needs-rebase"}, Detail: stageDetail{Role: stagelabels.RoleContributor}},
			},
			want: stagelabels.RoleContributor,
		},
		{
			name: "promotion: contributor label, registry now trusted",
			list: []stageObservation{
				{Lane: stageLaneTrusted, Reason: "untrusted-author-role", Labels: []string{"role/contributor", "needs-rebase"}},
				{Lane: stageLaneContributor, Reason: "label:needs-rebase", Labels: []string{"role/contributor", "needs-rebase"}, Detail: stageDetail{Role: stagelabels.RoleTrusted}},
			},
			want: stagelabels.RoleTrusted,
		},
		{
			name: "cannot-vouch does not demote a label another lane wrote",
			list: []stageObservation{
				{Lane: stageLaneContributor, Reason: contributorLaneReasonNoClaim, Labels: []string{"role/trusted"}, Detail: stageDetail{Role: stagelabels.RoleUntrusted}},
			},
			want: stagelabels.RoleTrusted,
		},
		{
			name: "no lane tier keeps the label",
			list: []stageObservation{
				{Lane: stageLaneTrusted, Reason: "untrusted-author-role", Labels: []string{"role/contributor"}},
			},
			want: stagelabels.RoleContributor,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, got, _ := resolveStage(tc.list); got != tc.want {
				t.Fatalf("role = %v, want %v", got, tc.want)
			}
		})
	}
}
