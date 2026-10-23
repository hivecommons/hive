package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/config"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// hivecommons/hive#13579: a contributor relay handed #13579 dropped its
// socket a minute later, 18 times in 12 hours. The disconnect release booked
// a run-log row and a cooldown but never released the issue claim, and every
// resume renewed it, so the hive's own scanner/coder saw "live issue claim"
// for 15 hours. A disconnect release that survives the #7838 grace window
// must release the claim.
func TestContributorDisconnect_ReleasesIssueClaim(t *testing.T) {
	shortDisconnectGrace(t, 0)
	hub, s, l := claimsHub(t)
	drainGraceBookings(t, hub)
	setStatusIssues(s, intgIssue(7, "flappy", "someone", nil))
	c := &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "repires", ContributorID: "c-repires", TrustTier: "contributor"},
		lastPong: time.Now(),
	}
	msg := hub.selectTask(c)
	if msg == nil || msg.Number != 7 {
		t.Fatalf("assignment = %+v, want #7", msg)
	}
	if _, ok := l.Lookup("myorg/repo1", 7); !ok {
		t.Fatal("precondition: contributor claim recorded on assignment")
	}
	hub.setLeaseClaim("c-repires", msg.TaskID, ghpkg.IssueClaimMark{Identity: "repires", ExpiresAt: time.Now().Add(time.Hour)}, true)

	task := &WSTaskAssign{TaskID: msg.TaskID, Repo: "myorg/repo1", Number: 7, Key: "myorg/repo1#7"}
	c.mu.Lock()
	c.currentTask = nil
	c.mu.Unlock()
	hub.bookAbandonmentAfterGrace(c, task, time.Now().Add(-time.Minute))

	if got, ok := l.Lookup("myorg/repo1", 7); ok {
		t.Fatalf("disconnect release left the issue claim live: %+v", got)
	}
	// The lease itself stays resumable (#4260) but no longer mirrors a claim.
	hub.leaseMu.Lock()
	lease := hub.leases[leaseKey("c-repires", msg.TaskID)]
	hub.leaseMu.Unlock()
	if lease == nil {
		t.Fatal("lease should survive the disconnect release so a late resume can re-adopt it")
	}
	if lease.claimedBy != "" || !lease.claimExpiresAt.IsZero() || lease.claimPosted {
		t.Fatalf("lease still mirrors a released claim: by=%q until=%v posted=%v", lease.claimedBy, lease.claimExpiresAt, lease.claimPosted)
	}
}

// A disconnect release withdrawn inside the grace window (the relay came
// back on a live connection) must NOT release the claim or count a drop.
func TestContributorDisconnect_WithdrawnReleaseKeepsClaim(t *testing.T) {
	shortDisconnectGrace(t, 0)
	hub, s, l := claimsHub(t)
	drainGraceBookings(t, hub)
	setStatusIssues(s, intgIssue(7, "flappy", "someone", nil))
	c := &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "repires", ContributorID: "c-repires", TrustTier: "contributor"},
		lastPong: time.Now(),
	}
	msg := hub.selectTask(c)
	if msg == nil {
		t.Fatal("no assignment")
	}
	task := &WSTaskAssign{TaskID: msg.TaskID, Repo: "myorg/repo1", Number: 7, Key: "myorg/repo1#7"}
	// The same identity re-adopted the task on a second, live socket.
	again := &ContributorConnection{profile: c.profile, lastPong: time.Now(), currentTask: task}
	hub.mu.Lock()
	hub.connections["conn-again"] = again
	hub.mu.Unlock()

	hub.bookAbandonmentAfterGrace(c, task, time.Now().Add(-time.Minute))
	if _, ok := l.Lookup("myorg/repo1", 7); !ok {
		t.Fatal("withdrawn disconnect release must keep the claim")
	}
	if _, ok := hub.contributorCoolOffAt("myorg/repo1#7", time.Now()); ok {
		t.Fatal("withdrawn release counted toward the flap cool-off")
	}
}

// A lease nobody resumed (pruned past leaseTTL) takes its claim with it.
func TestPruneExpiredLeases_ReleasesIssueClaim(t *testing.T) {
	hub, s, l := claimsHub(t)
	setStatusIssues(s, intgIssue(7, "flappy", "someone", nil))
	c := &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "repires", ContributorID: "c-repires", TrustTier: "contributor"},
		lastPong: time.Now(),
	}
	msg := hub.selectTask(c)
	if msg == nil {
		t.Fatal("no assignment")
	}
	if _, ok := l.Lookup("myorg/repo1", 7); !ok {
		t.Fatal("precondition: claim recorded")
	}
	if dropped := hub.pruneExpiredLeases(time.Now().Add(leaseTTL + time.Minute)); dropped != 1 {
		t.Fatalf("pruned %d leases, want 1", dropped)
	}
	if got, ok := l.Lookup("myorg/repo1", 7); ok {
		t.Fatalf("pruned lease left its claim live: %+v", got)
	}
}

// The flap tracker: N drops inside the window trip a cool-off exactly once;
// the cool-off reads with an injected clock and lapses on schedule; config
// knobs are honoured (negative threshold = off).
func TestContributorFlap_CoolOffTripsAndLapses(t *testing.T) {
	hub, s := covK2Hub(t)
	if s.deps == nil || s.deps.Config == nil {
		t.Skip("hub has no config to tune")
	}
	base := time.Date(2026, 10, 23, 4, 0, 0, 0, time.UTC)
	key := "myorg/repo1#13579"

	if _, tripped := hub.noteContributorDrop(key, base); tripped {
		t.Fatal("one drop must not trip")
	}
	if _, tripped := hub.noteContributorDrop(key, base.Add(20*time.Minute)); tripped {
		t.Fatal("two drops must not trip")
	}
	co, tripped := hub.noteContributorDrop(key, base.Add(40*time.Minute))
	if !tripped {
		t.Fatal("third drop inside 2h must trip the cool-off")
	}
	if co.Drops != 3 || co.Window != config.DefaultContributorFlapWindow || !co.Until.Equal(base.Add(40*time.Minute+config.DefaultContributorFlapCooloff)) {
		t.Fatalf("cool-off = %+v", co)
	}
	if got := co.Reason(); got != "contributor cool-off: 3 drops in 2h0m0s" {
		t.Fatalf("reason = %q", got)
	}
	if _, tripped := hub.noteContributorDrop(key, base.Add(41*time.Minute)); tripped {
		t.Fatal("a drop inside an active cool-off must not re-trip (one INFO per trip)")
	}
	if got, ok := hub.contributorCoolOffAt(key, base.Add(5*time.Hour)); !ok || got.Drops != 3 {
		t.Fatalf("cool-off should be active at +5h: %+v ok=%v", got, ok)
	}
	if _, ok := hub.contributorCoolOffAt("myorg/repo1#1", base.Add(time.Hour)); ok {
		t.Fatal("cool-off leaked to another issue")
	}
	if _, ok := hub.contributorCoolOffAt(key, base.Add(40*time.Minute+config.DefaultContributorFlapCooloff+time.Second)); ok {
		t.Fatal("cool-off should have lapsed")
	}
	reason, ok := s.ContributorCoolOffReason("myorg/repo1", 13579)
	if ok || reason != "" {
		t.Fatalf("lapsed cool-off still reported to the governor: %q", reason)
	}

	// Drops older than the window do not count.
	key2 := "myorg/repo1#2"
	hub.noteContributorDrop(key2, base)
	hub.noteContributorDrop(key2, base.Add(time.Hour))
	if _, tripped := hub.noteContributorDrop(key2, base.Add(2*time.Hour+time.Minute)); tripped {
		t.Fatal("the first drop aged out of the window; two drops must not trip")
	}

	// Config: threshold 2, 30 min window, 10 min cool-off.
	s.deps.Config.Governor.Claims.ContributorFlapThreshold = 2
	s.deps.Config.Governor.Claims.ContributorFlapWindowS = 1800
	s.deps.Config.Governor.Claims.ContributorFlapCooloffS = 600
	key3 := "myorg/repo1#3"
	hub.noteContributorDrop(key3, base)
	co, tripped = hub.noteContributorDrop(key3, base.Add(time.Minute))
	if !tripped || co.Drops != 2 || !co.Until.Equal(base.Add(11*time.Minute)) {
		t.Fatalf("configured threshold/cool-off not honoured: %+v tripped=%v", co, tripped)
	}
	if !strings.Contains(co.Reason(), "2 drops in 30m") {
		t.Fatalf("reason = %q", co.Reason())
	}
	// Negative threshold turns the tracker off.
	s.deps.Config.Governor.Claims.ContributorFlapThreshold = -1
	key4 := "myorg/repo1#4"
	for i := 0; i < 5; i++ {
		if _, tripped := hub.noteContributorDrop(key4, base.Add(time.Duration(i)*time.Minute)); tripped {
			t.Fatal("tracker should be off")
		}
	}
}

// An issue in contributor cool-off is skipped by selectTask for every
// contributor, and the governor-facing accessor reports why.
func TestContributorFlap_SelectTaskWithholdsCooledOffIssue(t *testing.T) {
	hub, s, _ := claimsHub(t)
	setStatusIssues(s,
		intgIssue(1, "flappy", "someone", nil),
		intgIssue(2, "free", "someone", nil),
	)
	now := time.Now()
	for i := 0; i < config.DefaultContributorFlapThreshold; i++ {
		hub.noteContributorDrop("myorg/repo1#1", now)
	}
	reason, ok := s.ContributorCoolOffReason("myorg/repo1", 1)
	if !ok || !strings.HasPrefix(reason, "contributor cool-off: 3 drops in ") {
		t.Fatalf("accessor = %q ok=%v", reason, ok)
	}
	if _, ok := s.ContributorCoolOffReason("myorg/repo1", 2); ok {
		t.Fatal("#2 is not in cool-off")
	}
	c := &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "arteeh", ContributorID: "c-arteeh", TrustTier: "contributor"},
		lastPong: now,
	}
	msg := hub.selectTask(c)
	if msg == nil || msg.Number != 2 {
		t.Fatalf("contributor should be offered #2 only, got %+v", msg)
	}
}

// The ledger's own expiry reason is the one release that posts no forge
// release marker; the lease reasons are spelled as constants so the hook and
// the hub agree.
func TestClaimReleaseReasons(t *testing.T) {
	if claims.ReasonExpired != "expired" {
		t.Fatalf("ReasonExpired = %q", claims.ReasonExpired)
	}
	if claimReleaseReasonDisconnect == claims.ReasonExpired || claims.ReasonLeaseExpired == claims.ReasonExpired {
		t.Fatal("lease release reasons must not collide with the ledger expiry reason")
	}
}
