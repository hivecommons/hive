package automerge

// Tests for the contributor lane's adoption admission path: a PR the hive
// adopted IN PLACE (stage/074-adopted-by-hive) merges under the hive's own
// authority once the ordinary gates pass, without the original author's
// trust tier. No test sleeps; the fixture clock is pre-soaked.

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

func TestTrySweepContributorLanePRAdoptedByHiveAdmission(t *testing.T) {
	const (
		head    = "sha7"
		oldHead = "sha6"
	)
	// untrusted makes the author inadmissible on every existing path: no
	// claim marker, no closing keyword, a newcomer registry tier and
	// GitHub's first-timer association.
	untrusted := func(fx *contributorLaneFixture) {
		fx.body = "Part of #42\n\nSome change."
		fx.issueComments = nil
		fx.association = "FIRST_TIME_CONTRIBUTOR"
	}
	inPlace := func(h string) func(store *stagelabels.Store, key string, now time.Time) {
		return func(store *stagelabels.Store, key string, now time.Time) {
			store.Observe(key, stagelabels.AdoptedByHive, "")
			store.MarkAdoptedInPlace(key, h, AdoptReasonOffline, now)
		}
	}
	supersede := func(store *stagelabels.Store, key string, now time.Time) {
		store.Observe(key, stagelabels.AdoptedByHive, "")
		store.MarkAdopted(key, head, 99, now)
	}
	yielded := func(store *stagelabels.Store, key string, now time.Time) {
		inPlace(oldHead)(store, key, now)
		store.MarkYielded(key, head)
	}
	for _, tc := range []struct {
		name       string
		seed       func(store *stagelabels.Store, key string, now time.Time)
		mutate     func(fx *contributorLaneFixture)
		autoMerge  bool
		wantReason string
		wantRedCI  bool
		wantMerges int
	}{
		{
			name:       "in-place adopted head merges without author trust",
			seed:       inPlace(head),
			mutate:     untrusted,
			autoMerge:  true,
			wantMerges: 1,
		},
		{
			name:       "in-place adopted head merges for association none",
			seed:       inPlace(head),
			mutate:     func(fx *contributorLaneFixture) { untrusted(fx); fx.association = "NONE" },
			autoMerge:  true,
			wantMerges: 1,
		},
		{
			name: "adopted head still passes the workflow diff fence",
			seed: inPlace(head),
			mutate: func(fx *contributorLaneFixture) {
				untrusted(fx)
				fx.files = []map[string]any{{"filename": ".github/workflows/ci.yml", "status": "modified", "additions": 1, "deletions": 1}}
			},
			autoMerge:  true,
			wantReason: contributorLaneReasonWorkflowPath,
		},
		{
			name: "adopted head still passes the test-removal fence",
			seed: inPlace(head),
			mutate: func(fx *contributorLaneFixture) {
				untrusted(fx)
				fx.files = []map[string]any{{"filename": "pkg/x/x_test.go", "status": "removed", "additions": 0, "deletions": 120}}
			},
			autoMerge:  true,
			wantReason: contributorLaneReasonTestsRemoved,
		},
		{
			name:      "adopted head still needs green CI",
			seed:      inPlace(head),
			mutate:    func(fx *contributorLaneFixture) { untrusted(fx); fx.headStatus = "failure" },
			autoMerge: true,
			wantRedCI: true,
		},
		{
			name:       "contributor pushed over the adopted head falls back to author gates",
			seed:       inPlace(oldHead),
			mutate:     untrusted,
			autoMerge:  true,
			wantReason: contributorLaneReasonAuthorAssociation,
		},
		{
			name:       "yielded adoption falls back to author gates",
			seed:       yielded,
			mutate:     untrusted,
			autoMerge:  true,
			wantReason: contributorLaneReasonAuthorAssociation,
		},
		{
			name:       "superseding adoption is not admitted on the original PR",
			seed:       supersede,
			mutate:     untrusted,
			autoMerge:  true,
			wantReason: contributorLaneReasonAuthorAssociation,
		},
		{
			name:       "no stage record falls back to author gates",
			mutate:     untrusted,
			autoMerge:  true,
			wantReason: contributorLaneReasonAuthorAssociation,
		},
		{
			name:       "adoption auto-merge disabled falls back to author gates",
			seed:       inPlace(head),
			mutate:     untrusted,
			autoMerge:  false,
			wantReason: contributorLaneReasonAuthorAssociation,
		},
		{
			name:       "adopted head with a valid claim still merges",
			seed:       inPlace(head),
			autoMerge:  true,
			wantMerges: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := validContributorFixture()
			if tc.mutate != nil {
				tc.mutate(&fx)
			}
			api := newContributorLaneAPI(t, &fx)
			defer api.Close()
			policy := defaultContributorLanePolicy()
			policy.RegistryTiersSufficient = true
			clock := &contributorLaneClock{at: time.Date(2026, 10, 27, 1, 0, 0, 0, time.UTC)}
			store := stagelabels.NewStore(filepath.Join(t.TempDir(), "stage.json"), clock.now)
			key := stageKey("acme", "widget", 7)
			if tc.seed != nil {
				tc.seed(store, key, clock.at.Add(-time.Hour))
			}
			stagePolicy := StageLabelPolicy{Enabled: true, Adoption: true, AdoptInPlace: true, AdoptionAutoMerge: tc.autoMerge}
			engine := newContributorLaneEngine(api.URL, policy, clock, Options{
				ContributorRegistryTier: registryTiers(map[string]string{contributorLaneTestAuthor: "newcomer"}),
				ContributorSoak:         soakedTracker(clock),
				StageStore:              store,
				StageTracker:            NewStageTracker(),
				StageLabelPolicy:        func() StageLabelPolicy { return stagePolicy },
			})
			if tc.wantRedCI {
				// The fixture's base is unprotected; declare the check
				// required so a failing run is a definite red rather than
				// the unknown-required-set "ci-unverified" hold.
				engine.SetRequiredChecks(map[string]bool{"build": true})
			}
			event, reason, err := engine.trySweepContributorLanePR(context.Background(), "widget", "acme", "widget", 7, policy)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if tc.wantRedCI {
				if classifyCIReason(reason) != ciReasonRed {
					t.Fatalf("reason = %q, want a red-CI reason", reason)
				}
			} else if reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", reason, tc.wantReason)
			}
			if fx.merges != tc.wantMerges {
				t.Fatalf("merges = %d, want %d", fx.merges, tc.wantMerges)
			}
			if tc.wantMerges == 0 {
				return
			}
			if event.Tier != contributorLaneTier || event.Author != contributorLaneTestAuthor || event.HeadSHA != head {
				t.Fatalf("event = %+v, want contributor-lane merge of PR 7 at %s", event, head)
			}
			if !strings.Contains(fx.lastComment, "adopted by the hive") || !strings.Contains(fx.lastComment, stagelabels.AdoptedByHive.Label()) {
				t.Fatalf("comment = %q, want adoption admission", fx.lastComment)
			}
		})
	}
}

func TestContributorLaneAdoptionAdmitHelpers(t *testing.T) {
	claim := contributorClaimMatch{AdoptedHead: "abc"}
	if !claim.admittedByAdoption() || claim.admittedByRegistry() || claim.admittedByAutoPromotion() {
		t.Fatalf("adoption claim misclassified: %+v", claim)
	}
	if got := contributorLaneAdmitPath(claim); got != contributorLaneReasonAdoptedByHive {
		t.Fatalf("admit path = %q, want %q", got, contributorLaneReasonAdoptedByHive)
	}
	if got := contributorLaneAdmitPath(contributorClaimMatch{}); got != "claim" {
		t.Fatalf("admit path = %q, want claim", got)
	}
	comment := contributorLaneMergeComment("@bob", contributorClaimMatch{AdoptedHead: "0123456789abcdef"}, 2*time.Hour, ContributorLaneVerdict{Configured: true, Approved: true})
	for _, want := range []string{"≥120m", "reviewer verdict approve", "`0123456789ab`", "@bob", stagelabels.AdoptedByHive.Label()} {
		if !strings.Contains(comment, want) {
			t.Fatalf("comment = %q, missing %q", comment, want)
		}
	}

	var nilEngine *Engine
	if !nilEngine.adoptionAutoMergeEnabled() {
		t.Fatal("nil engine should default adoption auto-merge on")
	}
	if nilEngine.adoptedHeadOwnedByHive("acme", "widget", 7, "sha") {
		t.Fatal("nil engine must not vouch for a head")
	}
	if (&Engine{}).adoptedHeadOwnedByHive("acme", "widget", 7, "") {
		t.Fatal("empty head must not be vouched for")
	}
	off := &Engine{stageLabelPolicy: func() StageLabelPolicy { return StageLabelPolicy{} }}
	if off.adoptionAutoMergeEnabled() {
		t.Fatal("explicit policy with AdoptionAutoMerge=false must disable")
	}
}
