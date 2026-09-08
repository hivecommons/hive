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
	// Red again, then vanishes from the open set. One missing pass is NOT
	// enough to prune — a per-repo listing hiccup must not erase history —
	// but once it has been gone for PruneAfter it is (merged/closed).
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return now })
	s.Sweep([]Observation{obs("org/repo", 7, "sha4", true)}, 3)
	s.Sweep([]Observation{obs("org/repo", 8, "shaX", true)}, 3)
	if n := s.Attempts("org/repo", 7); n != 1 {
		t.Fatalf("PR missing from one pass must keep its history, got %d attempts", n)
	}
	now = now.Add(PruneAfter)
	s.Sweep([]Observation{obs("org/repo", 8, "shaX", true)}, 3)
	if n := s.Attempts("org/repo", 7); n != 0 {
		t.Fatalf("PR absent for PruneAfter must be pruned, got %d attempts", n)
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
	body := CommentBody(3, []string{"Coverage Suite", "build-gate"}, "ReferenceError: seedMission is not defined", false)
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

// A PR whose red SHA NEVER changes (fix attempts are not even pushed — e.g.
// agents lost write credentials) must still escalate once its re-engagement
// budget is exhausted. Without this, the distinct-SHA count never advances and
// the PR is nudged forever without ever reaching a human (kubestellar/console,
// 2026-08-22: eight red PRs re-engaged every cycle for 15h with zero pushes).
func TestSweep_EscalatesWhenReEngagementBudgetExhaustedOnUnchangedSHA(t *testing.T) {
	s := Load(filepath.Join(t.TempDir(), "streaks.json"))

	// One red SHA, observed; re-engage to the cap without the SHA ever moving.
	s.Sweep([]Observation{obs("org/repo", 9, "frozen", true)}, 3)
	for i := 0; i < MaxReEngagements; i++ {
		if !s.TryReEngage("org/repo", 9, "frozen") {
			t.Fatalf("re-engage %d should be allowed", i+1)
		}
	}
	if s.TryReEngage("org/repo", 9, "frozen") {
		t.Fatal("cap must halt further re-engagements")
	}

	// Next sweep: still the same red SHA, one distinct attempt — but the
	// budget is exhausted, so escalation must fire now.
	r := s.Sweep([]Observation{obs("org/repo", 9, "frozen", true)}, 3)
	got := r[Key("org/repo", 9)]
	if got.Attempts != 1 || !got.NewlyEscala {
		t.Fatalf("exhausted budget on unchanged SHA must escalate: got %+v", got)
	}

	// A pushed fix (new SHA) resets the budget — a freshly-moving PR must NOT
	// be treated as exhausted.
	s2 := Load(filepath.Join(t.TempDir(), "s2.json"))
	s2.Sweep([]Observation{obs("org/repo", 11, "a", true)}, 3)
	for i := 0; i < MaxReEngagements; i++ {
		s2.TryReEngage("org/repo", 11, "a")
	}
	r = s2.Sweep([]Observation{obs("org/repo", 11, "b", true)}, 3) // branch moved
	got = r[Key("org/repo", 11)]
	if got.NewlyEscala {
		t.Fatalf("new SHA resets the budget; must not escalate yet: got %+v", got)
	}
}

// Machinery amnesty: entries escalated under an older fix-dispatch generation
// (pre-#4828 kicks carried no CI evidence and most attempts produced no
// commit) get ONE fresh budget under the current generation — un-escalated,
// counters cleared, distinct-SHA ledger restarted — instead of staying
// human-parked forever. Entries already at the current generation keep their
// state untouched.
func TestSweep_MachineryAmnestyReleasesOldGenerationEscalations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "streaks.json")
	s := Load(path)

	// Simulate a generation-1 entry: escalated, budget exhausted, full ledger.
	key := Key("org/repo", 9)
	s.mu.Lock()
	s.entries[key] = &Entry{
		RedSHAs:       []string{"a", "b", "c"},
		Escalated:     true,
		CurRedSHA:     "c",
		ReEngagements: MaxReEngagements,
		Machinery:     1,
	}
	s.mu.Unlock()

	// Next sweep under generation 2: amnesty fires — not escalated, ledger
	// restarted at the observed SHA only, and it must NOT immediately
	// re-escalate off the old ledger.
	r := s.Sweep([]Observation{obs("org/repo", 9, "c", true)}, 3)
	got := r[key]
	if got.NewlyEscala {
		t.Fatalf("amnestied entry must not re-escalate on the old ledger: %+v", got)
	}
	if got.Attempts != 1 {
		t.Fatalf("ledger must restart: got %+v", got)
	}
	s.mu.Lock()
	stillEscalated := s.entries[key].Escalated
	s.mu.Unlock()
	if stillEscalated {
		t.Fatal("entry must be un-escalated after amnesty")
	}
	// Re-engagement budget is fresh.
	if !s.TryReEngage("org/repo", 9, "c") {
		t.Fatal("amnestied entry must have a fresh re-engagement budget")
	}
	// The amnesty fires ONCE: exhausting the fresh budget escalates again.
	for i := 0; i < MaxReEngagements; i++ {
		s.TryReEngage("org/repo", 9, "c")
	}
	if s.TryReEngage("org/repo", 9, "c") {
		t.Fatal("cap must hold at the current generation — amnesty is one-shot")
	}
}

// A pending pass (checks running, or the check-run fetch failed — which
// EnrichCIStatus also reports as "pending") is no information: it must leave
// the attempt history AND the Escalated marker untouched. Before this, every
// non-red pass was read as green and wiped the entry, so a single transient
// API error re-armed the escalation and the hub re-posted the comment on the
// next red pass (tuna-os/corral#268: fifteen identical comments in 32h).
func TestSweep_PendingLeavesHistoryAndEscalationIntact(t *testing.T) {
	s := Load(filepath.Join(t.TempDir(), "streaks.json"))
	pending := func(n int, sha string) Observation {
		return Observation{Repo: "org/repo", Number: n, HeadSHA: sha, Pending: true}
	}

	s.Sweep([]Observation{obs("org/repo", 7, "sha1", true)}, 3)
	s.Sweep([]Observation{obs("org/repo", 7, "sha2", true)}, 3)
	r := s.Sweep([]Observation{pending(7, "sha2")}, 3)
	if n := s.Attempts("org/repo", 7); n != 2 {
		t.Fatalf("pending pass must not touch attempts, got %d", n)
	}
	if got := r[Key("org/repo", 7)]; got.Attempts != 2 || got.NewlyEscala {
		t.Fatalf("pending pass result: got %+v", got)
	}
	r = s.Sweep([]Observation{obs("org/repo", 7, "sha3", true)}, 3)
	if got := r[Key("org/repo", 7)]; got.Attempts != 3 || !got.NewlyEscala {
		t.Fatalf("attempt 3 after a pending pass must escalate: got %+v", got)
	}
	s.MarkEscalated("org/repo", 7)

	// Escalated, then a pending pass, then red again: still escalated, and
	// never newly so.
	r = s.Sweep([]Observation{pending(7, "sha3")}, 3)
	if got := r[Key("org/repo", 7)]; !got.Escalated || got.NewlyEscala {
		t.Fatalf("pending pass must report the escalated state: got %+v", got)
	}
	r = s.Sweep([]Observation{obs("org/repo", 7, "sha3", true)}, 3)
	if got := r[Key("org/repo", 7)]; !got.Escalated || got.NewlyEscala {
		t.Fatalf("red after pending must not re-escalate: got %+v", got)
	}
	// A pending pass with no ledger entry creates nothing.
	s.Sweep([]Observation{pending(99, "zzz")}, 3)
	if n := s.Attempts("org/repo", 99); n != 0 {
		t.Fatalf("pending pass must not create an entry, got %d attempts", n)
	}
}

// The forge label is authoritative. A PR that already wears needs-human is
// escalated no matter what the ledger says — a wiped ledger (unwritable
// /data, restart without a PVC) can never cause a second comment.
func TestSweep_LabelPresentMeansEscalatedEvenWithEmptyLedger(t *testing.T) {
	s := Load(filepath.Join(t.TempDir(), "streaks.json"))
	labeled := Observation{Repo: "org/repo", Number: 7, HeadSHA: "sha1", Red: true, Labeled: true}

	r := s.Sweep([]Observation{labeled}, 3)
	got := r[Key("org/repo", 7)]
	if !got.Escalated || got.NewlyEscala || got.NeedsLabel {
		t.Fatalf("labeled PR must read as escalated, never newly: got %+v", got)
	}
	// Re-engagement is capped out for it too: the exhausted path must not
	// produce a fresh escalation on a labeled PR either.
	for i := 0; i < MaxReEngagements+1; i++ {
		s.TryReEngage("org/repo", 7, "sha1")
	}
	r = s.Sweep([]Observation{labeled}, 3)
	if got := r[Key("org/repo", 7)]; !got.Escalated || got.NewlyEscala {
		t.Fatalf("labeled PR must stay quietly escalated: got %+v", got)
	}
	// Reload from disk (simulating a restart): still escalated via the label.
	s2 := Load(filepath.Join(t.TempDir(), "fresh.json"))
	r = s2.Sweep([]Observation{labeled}, 3)
	if got := r[Key("org/repo", 7)]; !got.Escalated || got.NewlyEscala {
		t.Fatalf("fresh ledger + label must not re-escalate: got %+v", got)
	}
}

// Removing the label is the documented way to hand a PR back to the
// automated lane. Once the label was CONFIRMED applied, its absence must
// un-park the PR: fresh distinct-SHA count, fresh re-engagement budget, and
// a later crossing escalates again (with a new comment).
func TestSweep_LabelRemovedByHumanUnparksPR(t *testing.T) {
	s := Load(filepath.Join(t.TempDir(), "streaks.json"))
	for _, sha := range []string{"a", "b", "c"} {
		s.Sweep([]Observation{obs("org/repo", 7, sha, true)}, 3)
	}
	s.MarkEscalated("org/repo", 7)
	s.MarkLabelApplied("org/repo", 7)

	// Within the grace window an absent label is a stale listing, not an
	// un-park: nothing changes.
	r := s.Sweep([]Observation{obs("org/repo", 7, "c", true)}, 3)
	if got := r[Key("org/repo", 7)]; !got.Escalated || got.Attempts != 3 {
		t.Fatalf("absence inside LabelUnparkGrace must be ignored: got %+v", got)
	}

	// Human removes the label (after the grace); PR still red on the same head.
	now := time.Now().UTC().Add(LabelUnparkGrace + time.Minute)
	s.SetClock(func() time.Time { return now })
	r = s.Sweep([]Observation{obs("org/repo", 7, "c", true)}, 3)
	got := r[Key("org/repo", 7)]
	if got.Escalated || got.NewlyEscala {
		t.Fatalf("label removal must un-park without re-escalating: got %+v", got)
	}
	if got.Attempts != 1 {
		t.Fatalf("un-parked PR must restart its distinct-SHA count, got %d", got.Attempts)
	}
	if n := s.ReEngagements("org/repo", 7); n != 0 {
		t.Fatalf("un-parked PR must get a fresh re-engagement budget, got %d", n)
	}
	// Two more failed attempts cross the threshold again.
	s.Sweep([]Observation{obs("org/repo", 7, "d", true)}, 3)
	r = s.Sweep([]Observation{obs("org/repo", 7, "e", true)}, 3)
	if got := r[Key("org/repo", 7)]; !got.NewlyEscala {
		t.Fatalf("un-parked PR must be escalatable again: got %+v", got)
	}
}

// A label that was never confirmed (AddLabels failed at escalation time) is
// NOT a human un-park: the sweep must retry the label, not re-comment and not
// reset the budget.
func TestSweep_UnconfirmedLabelIsRetriedNotTreatedAsUnpark(t *testing.T) {
	s := Load(filepath.Join(t.TempDir(), "streaks.json"))
	for _, sha := range []string{"a", "b", "c"} {
		s.Sweep([]Observation{obs("org/repo", 7, sha, true)}, 3)
	}
	s.MarkEscalated("org/repo", 7) // comment landed, label call failed

	r := s.Sweep([]Observation{obs("org/repo", 7, "c", true)}, 3)
	got := r[Key("org/repo", 7)]
	if !got.Escalated || got.NewlyEscala || !got.NeedsLabel {
		t.Fatalf("unconfirmed label must be retried, never re-escalated: got %+v", got)
	}
	if got.Attempts != 3 {
		t.Fatalf("history must be intact, got %d attempts", got.Attempts)
	}
	s.MarkLabelApplied("org/repo", 7)
	r = s.Sweep([]Observation{Observation{Repo: "org/repo", Number: 7, HeadSHA: "c", Red: true, Labeled: true}}, 3)
	if got := r[Key("org/repo", 7)]; got.NeedsLabel || !got.Escalated {
		t.Fatalf("confirmed label must clear NeedsLabel: got %+v", got)
	}
}

func TestCommentBody_ExhaustedWording(t *testing.T) {
	body := CommentBody(1, []string{"test"}, "boom", true)
	if !strings.Contains(body, "no new commit pushed") || !strings.Contains(body, "1 distinct red head seen") {
		t.Fatalf("exhausted wording missing: %s", body)
	}
	if strings.Contains(body, "1 distinct fix attempts") {
		t.Fatalf("exhausted body must not claim distinct fix attempts: %s", body)
	}
}

func TestHasNeedsHumanLabel(t *testing.T) {
	if !HasNeedsHumanLabel([]string{"bug", "Needs-Human"}) {
		t.Fatal("case-insensitive match expected")
	}
	if HasNeedsHumanLabel([]string{"hold", "needs-review"}) {
		t.Fatal("unexpected match")
	}
}
