package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/claims"
)

// An owner's claim outside the ledger releases the contributor's live lease
// and yanks the relay task (hivecommons/hive#18899): the release tick drops
// the claim with claims.HumanClaimReason, the hook moves the session to other
// work and records the person's claim in the ledger.
func TestClaims_HumanClaimReleaseYanksContributorAndRecordsPerson(t *testing.T) {
	hub, s, l := claimsHub(t)
	setStatusIssues(s,
		intgIssue(1, "claimed by the owner out of band", "someone", nil),
		intgIssue(2, "other work", "someone", nil),
	)
	profile := &ContributorProfile{GitHubUsername: "worker", ContributorID: "c-worker", TrustTier: "contributor"}
	held := &ContributorConnection{
		profile: profile, session: "a",
		currentTask:    &WSTaskAssign{TaskID: "t-1", Repo: "myorg/repo1", Number: 1},
		currentTaskGen: 7, lastLeaseRenew: time.Now(), lastPong: time.Now(),
	}
	hub.mu.Lock()
	hub.connections["conn-a"] = held
	hub.mu.Unlock()
	if _, err := l.Claim(claims.Request{Repo: "myorg/repo1", Issue: 1, Holder: "worker", HolderID: identityOf(held), Kind: claims.KindContributor, Session: "a"}); err != nil {
		t.Fatal(err)
	}

	// The forge now shows a person's claim: the admission check says so for
	// the live contributor claim, as the GitHub-backed check does.
	humanReason := claims.HumanClaimReason("assigned", "andy")
	l.SetAdmissionCheck(func(req claims.Request) (string, error) {
		if req.Kind == claims.KindContributor {
			return humanReason, nil
		}
		return "", nil
	})
	if n := l.ReleaseBlocked(); n != 1 {
		t.Fatalf("ReleaseBlocked released %d claims, want 1", n)
	}

	held.mu.Lock()
	cur, gen := held.currentTask, held.currentTaskGen
	held.mu.Unlock()
	if cur == nil || cur.Number != 2 {
		t.Fatalf("contributor not yanked onto other work: %+v", cur)
	}
	if gen == 7 {
		t.Fatal("assignment generation not bumped — a stale completion would not be fenced")
	}
	now, ok := l.Lookup("myorg/repo1", 1)
	if !ok || now.Kind != claims.KindHuman || now.Holder != "andy" {
		t.Fatalf("ledger claim = %+v (live %v), want the person's claim by andy", now, ok)
	}
}

// A disconnected relay has no socket to yank: the lease itself is revoked so
// a reconnect cannot resume the issue the owner claimed.
func TestClaims_HumanClaimReleaseRevokesDisconnectedRelayLease(t *testing.T) {
	hub, _, l := claimsHub(t)
	const identity = "c-worker#a"
	if err := hub.recordLease(identity, "t-1", "myorg/repo1", 1, "contributor", 3, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Claim(claims.Request{Repo: "myorg/repo1", Issue: 1, Holder: "worker", HolderID: identity, Kind: claims.KindContributor, Session: "a"}); err != nil {
		t.Fatal(err)
	}
	l.SetAdmissionCheck(func(req claims.Request) (string, error) {
		return claims.HumanClaimReason("/claim comment", "andy"), nil
	})
	if n := l.ReleaseBlocked(); n != 1 {
		t.Fatalf("ReleaseBlocked released %d claims, want 1", n)
	}
	if hub.lookupLease(identity, "t-1", "myorg/repo1", 1, 3, time.Now()) != nil {
		t.Fatal("disconnected relay's lease survived the owner's claim — it could resume #1")
	}
}

// A lease renewal that meets the person's claim releases the contributor at
// once, without waiting for the five-minute release tick.
func TestClaims_RenewalMeetingAHumanClaimReleasesTheContributor(t *testing.T) {
	hub, _, l := claimsHub(t)
	const identity = "c-worker#a"
	now := time.Now()
	if err := hub.recordLease(identity, "t-1", "myorg/repo1", 1, "contributor", 3, now); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Claim(claims.Request{Repo: "myorg/repo1", Issue: 1, Holder: "worker", HolderID: identity, Kind: claims.KindContributor, Session: "a"}); err != nil {
		t.Fatal(err)
	}
	l.SetAdmissionCheck(func(req claims.Request) (string, error) {
		return claims.HumanClaimReason("assigned", "andy"), nil
	})
	if err := hub.renewLease(identity, "t-1", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c, ok := l.Lookup("myorg/repo1", 1); ok && c.Kind == claims.KindHuman && c.Holder == "andy" {
			return
		}
		<-time.After(10 * time.Millisecond)
	}
	c, ok := l.Lookup("myorg/repo1", 1)
	t.Fatalf("renewal did not hand the issue to the person: claim %+v (live %v)", c, ok)
}

// Only a human-claim release is this hook's: an ordinary release leaves the
// ledger alone.
func TestClaims_OtherReleaseReasonsDoNotRecordAPerson(t *testing.T) {
	_, _, l := claimsHub(t)
	if _, err := l.Claim(claims.Request{Repo: "myorg/repo1", Issue: 1, Holder: "worker", HolderID: "c-worker#a", Kind: claims.KindContributor}); err != nil {
		t.Fatal(err)
	}
	l.ForceRelease("myorg/repo1", 1, "lease expired")
	if c, ok := l.Lookup("myorg/repo1", 1); ok {
		t.Fatalf("ordinary release left a claim: %+v", c)
	}
	if login, ok := claims.ParseHumanClaimReason("lease expired"); ok || login != "" {
		t.Fatalf("parsed %q from an ordinary reason", login)
	}
	if got, ok := claims.ParseHumanClaimReason(claims.HumanClaimReason("assigned", "andy")); !ok || got != "andy" || !strings.HasPrefix(claims.HumanClaimReason("assigned", "andy"), claims.ReasonHumanClaim) {
		t.Fatalf("round trip = %q, %v", got, ok)
	}
}
