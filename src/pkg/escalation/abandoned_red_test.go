package escalation

import (
	"path/filepath"
	"testing"
	"time"
)

// The claim-release valve must not let a covered issue go while the hive is
// still re-engaging the covering PR: hivecommons/hive#16475 fell from
// 040-covered-by-pr to 010-actionable ten minutes after #16492 went red, with
// the reaper's first re-engagement for that same head landing a minute later
// (hivecommons/hive#16522).
func TestAbandonedRed(t *testing.T) {
	newStore := func(t *testing.T) (*Store, *time.Time) {
		t.Helper()
		s := Load(filepath.Join(t.TempDir(), "ledger.json"))
		now := time.Now().UTC()
		s.SetClock(func() time.Time { return now })
		s.ObserveRed([]Observation{{Repo: "org/repo", Number: 1, HeadSHA: "sha1", Red: true}})
		return s, &now
	}

	t.Run("stale inside the re-engagement budget keeps the claim", func(t *testing.T) {
		s, now := newStore(t)
		*now = now.Add(RedPRStaleAfter + time.Minute)
		if !s.StaleRed("org/repo", 1, "sha1") {
			t.Fatal("precondition: head must read stale")
		}
		if s.AbandonedRed("org/repo", 1, "sha1") {
			t.Fatal("a stale red head the reaper is still re-engaging must not release its issue")
		}
		s.RecordReEngagement("org/repo", 1)
		*now = now.Add(ReEngageCooldown + time.Minute)
		if s.AbandonedRed("org/repo", 1, "sha1") {
			t.Fatal("one delivered re-engagement is the fix loop working, not giving up")
		}
	})

	t.Run("fresh red is never abandoned", func(t *testing.T) {
		s, _ := newStore(t)
		s.MarkEscalated("org/repo", 1)
		if s.AbandonedRed("org/repo", 1, "sha1") {
			t.Fatal("a head younger than RedPRStaleAfter must not release, even escalated")
		}
	})

	t.Run("escalated to a human releases", func(t *testing.T) {
		s, now := newStore(t)
		*now = now.Add(RedPRStaleAfter + time.Minute)
		s.MarkEscalated("org/repo", 1)
		if !s.AbandonedRed("org/repo", 1, "sha1") {
			t.Fatal("a stale red head handed to a human has left the fix loop")
		}
	})

	t.Run("spent re-engagement budget releases", func(t *testing.T) {
		s, now := newStore(t)
		*now = now.Add(RedPRStaleAfter + time.Minute)
		for i := 0; i < MaxReEngagements; i++ {
			s.RecordReEngagement("org/repo", 1)
		}
		if !s.AbandonedRed("org/repo", 1, "sha1") {
			t.Fatal("a stale red head with no re-engagements left has been given up on")
		}
	})

	t.Run("unreachable owner releases after the whole window", func(t *testing.T) {
		s, now := newStore(t)
		*now = now.Add(ClaimReleaseAfter - time.Minute)
		if s.AbandonedRed("org/repo", 1, "sha1") {
			t.Fatal("still inside the re-engagement window")
		}
		*now = now.Add(2 * time.Minute)
		if !s.AbandonedRed("org/repo", 1, "sha1") {
			t.Fatal("a head red for the whole re-engagement window with nobody reachable must release")
		}
	})

	t.Run("a moved head resets the verdict", func(t *testing.T) {
		s, now := newStore(t)
		*now = now.Add(ClaimReleaseAfter + time.Minute)
		s.ObserveRed([]Observation{{Repo: "org/repo", Number: 1, HeadSHA: "sha2", Red: true}})
		if s.AbandonedRed("org/repo", 1, "sha2") {
			t.Fatal("a new red head is a fix attempt: the clock and the budget start over")
		}
		if s.AbandonedRed("org/repo", 1, "sha1") {
			t.Fatal("a head the store no longer tracks is not stale")
		}
	})
}
