package escalation

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func obs(repo string, num int, sha string, red bool) Observation {
	return Observation{Repo: repo, Number: num, HeadSHA: sha, Red: red, Excerpt: "ReferenceError: seedMission is not defined"}
}

func TestSweep_EscalatesAtThresholdOfDistinctRedSHAs(t *testing.T) {
	s := Load(filepath.Join(t.TempDir(), "streaks.json"))

	// Attempt 1: red.
	r := s.Sweep([]Observation{obs("org/repo", 7, "sha1", true)}, 3)
	if got := r[Key("org/repo", 7)]; got.Attempts != 1 || got.NewlyEscala {
		t.Fatalf("attempt 1: got %+v", got)
	}
	// Same SHA re-observed: NOT a new attempt.
	r = s.Sweep([]Observation{obs("org/repo", 7, "sha1", true)}, 3)
	if got := r[Key("org/repo", 7)]; got.Attempts != 1 {
		t.Fatalf("same sha must not increment: got %+v", got)
	}
	// Attempts 2 and 3: new SHAs, still red — threshold crossed on 3.
	s.Sweep([]Observation{obs("org/repo", 7, "sha2", true)}, 3)
	r = s.Sweep([]Observation{obs("org/repo", 7, "sha3", true)}, 3)
	got := r[Key("org/repo", 7)]
	if got.Attempts != 3 || !got.NewlyEscala {
		t.Fatalf("attempt 3 must escalate: got %+v", got)
	}

	// After MarkEscalated, further sweeps must not re-fire.
	s.MarkEscalated("org/repo", 7)
	r = s.Sweep([]Observation{obs("org/repo", 7, "sha4", true)}, 3)
	got = r[Key("org/repo", 7)]
	if got.NewlyEscala || !got.Escalated {
		t.Fatalf("escalation must fire once: got %+v", got)
	}
}

func TestSweep_GreenResetsAndAbsencePrunes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "streaks.json")
	s := Load(path)

	s.Sweep([]Observation{obs("org/repo", 7, "sha1", true)}, 3)
	s.Sweep([]Observation{obs("org/repo", 7, "sha2", true)}, 3)
	// Goes green: history forgotten.
	s.Sweep([]Observation{obs("org/repo", 7, "sha3", false)}, 3)
	if n := s.Attempts("org/repo", 7); n != 0 {
		t.Fatalf("green must reset, got %d attempts", n)
	}
	// Red again, then vanishes from the open set (merged/closed): pruned.
	s.Sweep([]Observation{obs("org/repo", 7, "sha4", true)}, 3)
	s.Sweep([]Observation{obs("org/repo", 8, "shaX", true)}, 3)
	if n := s.Attempts("org/repo", 7); n != 0 {
		t.Fatalf("absent PR must be pruned, got %d attempts", n)
	}

	// Persistence across Load.
	s2 := Load(path)
	if n := s2.Attempts("org/repo", 8); n != 1 {
		t.Fatalf("ledger must persist, got %d attempts for #8", n)
	}
}

func TestExcerpt_ReturnsStoredEvidenceOrEmpty(t *testing.T) {
	s := Load(filepath.Join(t.TempDir(), "streaks.json"))

	s.Sweep([]Observation{obs("org/repo", 7, "sha1", true)}, 3)
	if got := s.Excerpt("org/repo", 7); got != "ReferenceError: seedMission is not defined" {
		t.Fatalf("Excerpt = %q, want the stored CI evidence", got)
	}
	if got := s.Excerpt("org/repo", 8); got != "" {
		t.Fatalf("Excerpt for an unknown PR = %q, want empty", got)
	}
}

func TestCommentBody_LeadsWithEvidence(t *testing.T) {
	body := CommentBody(3, []string{"Coverage Suite", "build-gate"}, "ReferenceError: seedMission is not defined")
	for _, want := range []string{"3 distinct fix attempts", "Coverage Suite", "seedMission is not defined", NeedsHumanLabel} {
		if !strings.Contains(body, want) {
			t.Fatalf("comment body missing %q:\n%s", want, body)
		}
	}
}

// --- Staleness + re-engagement (red-PR re-engagement machinery) ---

// mkClock returns a controllable clock and a pointer to advance it.
func mkClock(start time.Time) (func() time.Time, *time.Time) {
	cur := start
	return func() time.Time { return cur }, &cur
}

func TestStaleRed_FreshRedNotStale_AgedRedStale(t *testing.T) {
	s := Load(filepath.Join(t.TempDir(), "streaks.json"))
	clock, cur := mkClock(time.Unix(1_000_000, 0).UTC())
	s.SetClock(clock)

	// A red PR just observed: NOT stale (clock has not advanced).
	s.ObserveRed([]Observation{{Repo: "org/repo", Number: 7, HeadSHA: "sha1", Red: true}})
	if s.StaleRed("org/repo", 7, "sha1") {
		t.Fatal("a just-seen red SHA must not be stale")
	}
	// Advance past the threshold on the SAME SHA: now stale.
	*cur = cur.Add(RedPRStaleAfter + time.Second)
	s.ObserveRed([]Observation{{Repo: "org/repo", Number: 7, HeadSHA: "sha1", Red: true}})
	if !s.StaleRed("org/repo", 7, "sha1") {
		t.Fatal("a red SHA unchanged past RedPRStaleAfter must be stale")
	}
	// A NEW red SHA resets the clock: not stale again (agent pushed a fix).
	s.ObserveRed([]Observation{{Repo: "org/repo", Number: 7, HeadSHA: "sha2", Red: true}})
	if s.StaleRed("org/repo", 7, "sha2") {
		t.Fatal("a freshly-changed red SHA must not be stale")
	}
	// StaleRed for a SHA that is not the tracked current one is never stale.
	if s.StaleRed("org/repo", 7, "sha1") {
		t.Fatal("stale check must key off the CURRENT red SHA only")
	}
}

func TestStaleRed_HealthyPRNeverStale(t *testing.T) {
	s := Load(filepath.Join(t.TempDir(), "streaks.json"))
	clock, cur := mkClock(time.Unix(2_000_000, 0).UTC())
	s.SetClock(clock)

	// Pending/green PR: ObserveRed with Red:false must leave no staleness record.
	s.ObserveRed([]Observation{{Repo: "org/repo", Number: 9, HeadSHA: "greenSHA", Red: false}})
	*cur = cur.Add(2 * RedPRStaleAfter)
	if s.StaleRed("org/repo", 9, "greenSHA") {
		t.Fatal("a healthy PR must never be reported stale")
	}
	// A PR that was red then went green: staleness record cleared.
	s.ObserveRed([]Observation{{Repo: "org/repo", Number: 9, HeadSHA: "redSHA", Red: true}})
	*cur = cur.Add(2 * RedPRStaleAfter)
	s.ObserveRed([]Observation{{Repo: "org/repo", Number: 9, HeadSHA: "redSHA", Red: false}})
	if s.StaleRed("org/repo", 9, "redSHA") {
		t.Fatal("a PR that recovered to green must not stay stale")
	}
}

func TestTryReEngage_CapHaltsPermanentlyRedPR(t *testing.T) {
	s := Load(filepath.Join(t.TempDir(), "streaks.json"))
	clock, _ := mkClock(time.Unix(3_000_000, 0).UTC())
	s.SetClock(clock)

	s.ObserveRed([]Observation{{Repo: "org/repo", Number: 5, HeadSHA: "stuck", Red: true}})
	// Exactly MaxReEngagements dispatches are allowed for one unchanged red SHA.
	for i := 0; i < MaxReEngagements; i++ {
		if !s.TryReEngage("org/repo", 5, "stuck") {
			t.Fatalf("re-engagement %d must be allowed (cap is %d)", i+1, MaxReEngagements)
		}
	}
	if s.TryReEngage("org/repo", 5, "stuck") {
		t.Fatal("re-engagement past the cap must be refused — a never-moving red PR must stop being nudged")
	}
	if got := s.ReEngagements("org/repo", 5); got != MaxReEngagements {
		t.Fatalf("re-engagement count = %d, want %d", got, MaxReEngagements)
	}
	// A NEW red SHA (agent pushed a fix, still red) resets the cap.
	if !s.TryReEngage("org/repo", 5, "moved") {
		t.Fatal("a changed red SHA must reset the re-engagement cap")
	}
	if got := s.ReEngagements("org/repo", 5); got != 1 {
		t.Fatalf("after SHA change re-engagement count = %d, want 1", got)
	}
}

func TestTryReEngage_EmptySHAReusesTrackedSHA(t *testing.T) {
	// The merge-watcher hook passes an empty head SHA (it does not re-fetch the
	// head); the store must reuse the SHA last observed and NOT reset the cap.
	s := Load(filepath.Join(t.TempDir(), "streaks.json"))
	clock, _ := mkClock(time.Unix(4_000_000, 0).UTC())
	s.SetClock(clock)

	s.ObserveRed([]Observation{{Repo: "org/repo", Number: 3, HeadSHA: "abc", Red: true}})
	for i := 0; i < MaxReEngagements; i++ {
		if !s.TryReEngage("org/repo", 3, "") {
			t.Fatalf("empty-SHA re-engagement %d must be allowed", i+1)
		}
	}
	if s.TryReEngage("org/repo", 3, "") {
		t.Fatal("empty-SHA re-engagement must respect the cap on the tracked SHA")
	}
}
