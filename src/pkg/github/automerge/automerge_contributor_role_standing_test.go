package automerge

import (
	"context"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// A maintainer whose registry profile still says newcomer is trusted through
// the clean-merged-PR ratchet. When the hive merges main into their branch
// and carries their own Approve onto the merge commit (or owns the head
// through an in-place adoption), the lane admits that head by the
// head-scoped route and returns before the ratchet runs. The role label must
// still show the author's standing, not the raw registry tier: the head's
// author (the App) and the admission route say nothing about the PR author
// (projectbluefin/utah#1132 went role/trusted -> role/newcomer three minutes
// after the hive's merge commit and stayed there).
func TestAdmitContributorHeadRoutesKeepAuthorStanding(t *testing.T) {
	const mergeHead = "sha7" // the hive's "Merge branch 'main' into …" commit
	cleanHistory := func(n int) []map[string]any {
		out := make([]map[string]any, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, mergedHistoryPR(contributorLaneTestAuthor, "fix: clean change"))
		}
		return out
	}
	for _, tc := range []struct {
		name         string
		route        string // "operator" or "adopted"
		registryTier string
		history      []map[string]any
		wantTier     string
		wantRole     stagelabels.Role
	}{
		{name: "approval carried onto the hive merge commit keeps trusted", route: "operator", registryTier: "newcomer", history: cleanHistory(5), wantTier: "trusted", wantRole: stagelabels.RoleTrusted},
		{name: "hive-owned adopted head keeps trusted", route: "adopted", registryTier: "newcomer", history: cleanHistory(5), wantTier: "trusted", wantRole: stagelabels.RoleTrusted},
		{name: "short history still shows the registry tier", route: "operator", registryTier: "newcomer", history: cleanHistory(4), wantTier: "newcomer", wantRole: stagelabels.RoleNewcomer},
		{name: "revoked author is never promoted by history", route: "operator", registryTier: "revoked", history: cleanHistory(5), wantTier: "revoked", wantRole: stagelabels.RoleUntrusted},
		{name: "registry tier in require_tiers needs no history", route: "operator", registryTier: "merger", wantTier: "merger", wantRole: stagelabels.RoleTrusted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := validContributorFixture()
			fx.mergedHistory = tc.history
			api := newContributorLaneAPI(t, &fx)
			defer api.Close()
			policy := defaultContributorLanePolicy()
			policy.AutoPromoteAfterMergedPR = 5
			clock := &contributorLaneClock{at: time.Date(2026, 11, 7, 5, 11, 35, 0, time.UTC)}
			store := stagelabels.NewStore("", clock.now)
			engine := newContributorLaneEngine(api.URL, policy, clock, Options{
				StageStore:              store,
				ContributorRegistryTier: registryTiers(map[string]string{contributorLaneTestAuthor: tc.registryTier}),
			})
			key := stageKey("acme", "widget", 7)
			store.Observe(key, stagelabels.ReviewPending, "contributor-lane:workflow-path")
			switch tc.route {
			case "operator":
				if !store.NoteAdmission(key, mergeHead, stagelabels.OperatorApprovalAdmission(contributorLaneTestAuthor), clock.now()) {
					t.Fatal("NoteAdmission refused")
				}
			case "adopted":
				store.MarkAdoptedInPlace(key, mergeHead, "check-failing", clock.now())
			}
			pr := &gh.PullRequest{Number: gh.Ptr(7), Body: gh.Ptr("Fixes #42"), User: &gh.User{Login: gh.Ptr(contributorLaneTestAuthor)}, Head: &gh.PullRequestBranch{SHA: gh.Ptr(mergeHead)}}
			match, err := engine.admitContributor(context.Background(), "acme", "widget", pr, policy)
			if err != nil {
				t.Fatalf("admit err = %v", err)
			}
			switch tc.route {
			case "operator":
				if !match.admittedByOperator() {
					t.Fatalf("not admitted by operator: %+v", match)
				}
			case "adopted":
				if !match.admittedByAdoption() {
					t.Fatalf("not admitted by adoption: %+v", match)
				}
			}
			if match.ObservedTier != tc.wantTier {
				t.Fatalf("ObservedTier = %q, want %q", match.ObservedTier, tc.wantTier)
			}
			if got := stagelabels.RoleFromTier(match.ObservedTier, policy.RequireTiers); got != tc.wantRole {
				t.Fatalf("role = %s, want %s", got.Label(), tc.wantRole.Label())
			}
			// Labelling never announces a promotion; only the author's own
			// admission route does.
			if fx.comments != 0 {
				t.Fatalf("comments = %d, want 0 (last %q)", fx.comments, fx.lastComment)
			}
		})
	}
}
