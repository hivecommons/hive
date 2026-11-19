package dashboard

import (
	"testing"

	"github.com/hivecommons/hive/pkg/claims"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// An agent claim stays live while the hive's open PR in ANOTHER managed repo
// references the issue, and is released once that PR stops covering it
// (hivecommons/hive#18676: hivecommons/quorum-companion#3 `Refs
// hivecommons/hive#18660`, whose claim lapsed while the PR was open).
func TestClaims_AgentClaimHeldByCrossRepoPR(t *testing.T) {
	hub, s, l := claimsHub(t)
	covering := ghpkg.IssueClaim{Repo: "repo1", Issue: 1, PRRepo: "companion", PRNumber: 3,
		PRURL: "https://github.com/myorg/companion/pull/3", PRAuthor: "hive[bot]", PRState: ghpkg.PRStateOpen, Reference: true}
	open := true
	s.deps.IssueClaimed = func(repo string, number int) (ghpkg.IssueClaim, bool) {
		// The duplicate-PR ledger keys the claim by the configured (bare)
		// spelling; the sweep must find it from the claim's owner/repo key.
		if repo == "repo1" && number == 1 && open {
			return covering, true
		}
		return ghpkg.IssueClaim{}, false
	}
	res, err := l.Claim(claims.Request{Repo: "myorg/repo1", Issue: 1, Holder: "scanner", HolderID: "scanner", Kind: claims.KindAgent})
	if err != nil || !res.Outcome.Changed() {
		t.Fatalf("claim: %+v %v", res, err)
	}
	before := res.Claim.ExpiresAt

	if n := hub.sweepPRHeldClaims(); n != 0 {
		t.Fatalf("sweep released %d claims while the cross-repo PR is open", n)
	}
	c, ok := l.Lookup("myorg/repo1", 1)
	if !ok || c.Holder != "scanner" || !c.PRStarted || c.PRRepo != "companion" || c.PRNumber != 3 {
		t.Fatalf("claim after sweep = %+v ok=%v, want held for companion#3", c, ok)
	}
	if !c.ExpiresAt.After(before) {
		t.Fatalf("claim expiry %v not renewed past %v", c.ExpiresAt, before)
	}

	// External PRs and merged PRs never hold an agent's claim.
	for _, pr := range []ghpkg.IssueClaim{
		{Repo: "repo1", Issue: 2, PRRepo: "companion", PRNumber: 4, ExternalAuthor: true},
		{Repo: "repo1", Issue: 2, PRRepo: "companion", PRNumber: 4, MergedPR: true},
	} {
		if _, ok := (&ContributeWSHub{server: &Server{deps: &Dependencies{IssueClaimed: func(string, int) (ghpkg.IssueClaim, bool) { return pr, true }}}}).agentCoveringPR(claims.Claim{Repo: "myorg/repo1", Issue: 2}); ok {
			t.Fatalf("%+v must not hold an agent claim", pr)
		}
	}

	open = false
	if n := hub.sweepPRHeldClaims(); n != 1 {
		t.Fatalf("sweep released %d claims once the PR closed, want 1", n)
	}
	if _, ok := l.Lookup("myorg/repo1", 1); ok {
		t.Fatal("PR-held agent claim survived its PR closing")
	}
}

// An agent claim with no covering PR is left to its own TTL: the sweep never
// releases a claim it did not hold.
func TestClaims_AgentClaimWithoutPRLeftAlone(t *testing.T) {
	hub, s, l := claimsHub(t)
	s.deps.IssueClaimed = func(string, int) (ghpkg.IssueClaim, bool) { return ghpkg.IssueClaim{}, false }
	if _, err := l.Claim(claims.Request{Repo: "myorg/repo1", Issue: 5, Holder: "scanner", HolderID: "scanner", Kind: claims.KindAgent}); err != nil {
		t.Fatal(err)
	}
	if n := hub.sweepPRHeldClaims(); n != 0 {
		t.Fatalf("released %d", n)
	}
	if c, ok := l.Lookup("myorg/repo1", 5); !ok || c.PRStarted {
		t.Fatalf("claim = %+v ok=%v, want untouched", c, ok)
	}
}
