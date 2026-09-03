package advisory

import (
	"sort"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
)

// The digest's top-N is a scarce resource: the 2026-08-24 advisory comment on
// kubestellar/hive#2364 spent 4 of its 10 critical slots on findings the
// renderer itself captioned "file path not found at analyzed commit — finding
// may be outdated", while 262 other findings went unshown. Staleness was
// computed only AFTER the cap had already been applied, so it could not
// influence which findings won a slot. These tests pin the ranking rule that
// fixes it.

// staleBase is a fixed clock for these tests: applyTopN breaks severity ties by
// recency, so findings need distinct, predictable timestamps.
var staleBase = time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

// finding builds a Finding with a file ref, at an age expressed in minutes
// before staleBase — smaller age means more recent, so it ranks higher.
func finding(agent, sev, title, file string, ageMinutes int) Finding {
	return Finding{
		Agent:     agent,
		Severity:  sev,
		Title:     title,
		File:      file,
		Timestamp: staleBase.Add(-time.Duration(ageMinutes) * time.Minute),
	}
}

// existsExcept returns a verify func reporting every path as present except the
// named ones, and a pointer to the count of lookups it performed.
func existsExcept(missing ...string) (func(string) bool, *int) {
	gone := make(map[string]bool, len(missing))
	for _, m := range missing {
		gone[m] = true
	}
	calls := 0
	return func(path string) bool {
		calls++
		return !gone[path]
	}, &calls
}

// keptTitles flattens an applyTopN result into a sorted title list so tests can
// assert on membership without depending on per-agent map ordering.
func keptTitles(byAgent map[string][]Finding) []string {
	var out []string
	for _, fs := range byAgent {
		for _, f := range fs {
			out = append(out, f.Title)
		}
	}
	sort.Strings(out)
	return out
}

func keptFinding(t *testing.T, byAgent map[string][]Finding, title string) Finding {
	t.Helper()
	for _, fs := range byAgent {
		for _, f := range fs {
			if f.Title == title {
				return f
			}
		}
	}
	t.Fatalf("finding %q not in the kept set (kept: %v)", title, keptTitles(byAgent))
	return Finding{}
}

func sameTitles(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// ── freshness within a severity band ────────────────────────────────────────

// TestApplyTopNPrefersFreshOverStaleWithinSeverity is the core fix: among
// equally-severe findings, the ones whose file still exists win the slots. The
// two stale findings here are the MOST RECENT, so under the old severity+recency
// ranking they would have taken both slots and rendered as "may be outdated".
func TestApplyTopNPrefersFreshOverStaleWithinSeverity(t *testing.T) {
	byAgent := map[string][]Finding{
		"quality": {
			finding("quality", "critical", "stale-newest", "src/pkg/gone.go", 1),
			finding("quality", "critical", "stale-newer", "src/pkg/removed.go", 2),
			finding("quality", "critical", "live-older", "src/pkg/agent/manager.go", 3),
			finding("quality", "critical", "live-oldest", "src/pkg/hub/oauth.go", 4),
		},
	}
	verify, calls := existsExcept("src/pkg/gone.go", "src/pkg/removed.go")

	kept, overflow := applyTopN(byAgent, 2, verify)

	if overflow != 2 {
		t.Errorf("overflow = %d, want 2", overflow)
	}
	got := keptTitles(kept)
	want := []string{"live-older", "live-oldest"}
	if !sameTitles(got, want) {
		t.Errorf("kept = %v, want %v — the live findings must take the slots even though the stale ones are newer", got, want)
	}
	if *calls != 4 {
		t.Errorf("verify called %d times, want 4 (one per distinct path examined)", *calls)
	}
}

// TestApplyTopNBackfillsWithStaleWhenFreshRunsOut: staleness demotes, it does
// not disqualify. A path that moved does not prove the problem is gone, so a
// slot no fresh finding claims still goes to a stale one rather than rendering
// a short digest.
func TestApplyTopNBackfillsWithStaleWhenFreshRunsOut(t *testing.T) {
	byAgent := map[string][]Finding{
		"quality": {
			finding("quality", "critical", "stale-a", "src/gone-a.go", 1),
			finding("quality", "critical", "stale-b", "src/gone-b.go", 2),
			finding("quality", "critical", "live", "src/here.go", 3),
		},
	}
	verify, _ := existsExcept("src/gone-a.go", "src/gone-b.go")

	kept, overflow := applyTopN(byAgent, 2, verify)

	if overflow != 1 {
		t.Errorf("overflow = %d, want 1", overflow)
	}
	got := keptTitles(kept)
	want := []string{"live", "stale-a"}
	if !sameTitles(got, want) {
		t.Errorf("kept = %v, want %v — the free slot backfills with the newest stale finding", got, want)
	}
}

// TestApplyTopNKeepsStaleMarkOnBackfilled: a stale finding that backfills a slot
// must still carry PathStale, or the renderer would cite a dead path as live —
// the exact #3704 regression this ranking must not reintroduce.
func TestApplyTopNKeepsStaleMarkOnBackfilled(t *testing.T) {
	byAgent := map[string][]Finding{
		"quality": {
			finding("quality", "critical", "stale", "src/gone.go", 1),
			finding("quality", "critical", "live", "src/here.go", 2),
			finding("quality", "high", "filler", "src/other.go", 3),
		},
	}
	verify, _ := existsExcept("src/gone.go")

	kept, _ := applyTopN(byAgent, 2, verify)

	if f := keptFinding(t, kept, "stale"); !f.PathStale {
		t.Error("backfilled finding has PathStale = false, want true so the renderer captions it as outdated")
	}
	if f := keptFinding(t, kept, "live"); f.PathStale {
		t.Error("live finding has PathStale = true, want false")
	}
}

// ── severity still dominates ────────────────────────────────────────────────

// TestApplyTopNSeverityBeatsFreshness guards the deliberate limit on the fix: a
// stale CRITICAL outranks a live LOW. PathStale means the cited path moved, not
// that the problem is gone, so letting freshness cross severity bands would let
// a cosmetic nit displace a security finding whose file was merely renamed.
func TestApplyTopNSeverityBeatsFreshness(t *testing.T) {
	byAgent := map[string][]Finding{
		"sec-check": {finding("sec-check", "critical", "stale-critical", "src/moved.go", 5)},
		"guide": {
			finding("guide", "low", "live-low-a", "docs/a.md", 1),
			finding("guide", "low", "live-low-b", "docs/b.md", 2),
		},
	}
	verify, _ := existsExcept("src/moved.go")

	kept, overflow := applyTopN(byAgent, 1, verify)

	if overflow != 2 {
		t.Errorf("overflow = %d, want 2", overflow)
	}
	got := keptTitles(kept)
	if !sameTitles(got, []string{"stale-critical"}) {
		t.Errorf("kept = %v, want [stale-critical] — severity must outrank freshness across bands", got)
	}
}

// ── lookup cost ─────────────────────────────────────────────────────────────

// TestApplyTopNVerifiesEachPathOnce: the same file commonly backs several
// findings, and existence at a fixed commit is stable for the whole cycle.
func TestApplyTopNVerifiesEachPathOnce(t *testing.T) {
	byAgent := map[string][]Finding{
		"quality": {
			finding("quality", "critical", "a", "src/same.go", 1),
			finding("quality", "critical", "b", "src/same.go", 2),
			finding("quality", "critical", "c", "src/same.go", 3),
		},
	}
	verify, calls := existsExcept()

	if _, overflow := applyTopN(byAgent, 2, verify); overflow != 1 {
		t.Errorf("overflow = %d, want 1", overflow)
	}
	if *calls != 1 {
		t.Errorf("verify called %d times, want 1 — results must be cached per distinct path", *calls)
	}
}

// TestApplyTopNStopsVerifyingOnceCapIsFilled bounds the added network cost:
// ranking the full set must not cost one lookup per finding. Verification walks
// in rank order and stops as soon as the cap is satisfied.
func TestApplyTopNStopsVerifyingOnceCapIsFilled(t *testing.T) {
	byAgent := map[string][]Finding{"quality": {}}
	for i := 0; i < 50; i++ {
		byAgent["quality"] = append(byAgent["quality"],
			finding("quality", "critical", string(rune('a'+i%26))+string(rune('a'+i/26)), "src/f"+string(rune('a'+i%26))+string(rune('a'+i/26))+".go", i))
	}
	verify, calls := existsExcept()

	applyTopN(byAgent, 3, verify)

	if *calls != 3 {
		t.Errorf("verify called %d times for a cap of 3 over 50 findings, want 3 — the walk must early-exit", *calls)
	}
}

// TestApplyTopNSkipsVerifyForIssueRefs: a "gh-123" or "owner/repo#1" ref is not
// a repo path, so it must never trigger a path lookup.
func TestApplyTopNSkipsVerifyForIssueRefs(t *testing.T) {
	byAgent := map[string][]Finding{
		"scanner": {
			finding("scanner", "critical", "issue-ref", "gh-4581", 1),
			finding("scanner", "critical", "inline-ref", "hivecommons/hive#3344", 2),
			finding("scanner", "critical", "no-ref", "", 3),
			finding("scanner", "critical", "path-ref", "src/real.go", 4),
		},
	}
	verify, calls := existsExcept()

	kept, _ := applyTopN(byAgent, 3, verify)

	if *calls != 0 {
		t.Errorf("verify called %d times, want 0 — only the path ref could qualify and the cap fills before it", *calls)
	}
	if got := keptTitles(kept); len(got) != 3 {
		t.Errorf("kept %d findings, want 3", len(got))
	}
}

// TestApplyTopNNilVerifyKeepsSeverityRecencyOrder pins backward compatibility:
// with no verifier the ranking is exactly the previous severity-then-recency
// behavior, so callers that never resolve a snapshot are unaffected.
func TestApplyTopNNilVerifyKeepsSeverityRecencyOrder(t *testing.T) {
	byAgent := map[string][]Finding{
		"quality": {
			finding("quality", "critical", "newest", "src/gone.go", 1),
			finding("quality", "critical", "older", "src/also-gone.go", 2),
			finding("quality", "high", "high-one", "src/here.go", 3),
		},
	}

	kept, overflow := applyTopN(byAgent, 2, nil)

	if overflow != 1 {
		t.Errorf("overflow = %d, want 1", overflow)
	}
	got := keptTitles(kept)
	if !sameTitles(got, []string{"newest", "older"}) {
		t.Errorf("kept = %v, want [newest older] — without a verifier, severity+recency alone decides", got)
	}
}

// ── through BuildDigestFromBeads ────────────────────────────────────────────

// TestBuildDigestRanksFreshOverStale is the end-to-end version of the fix: the
// digest a repo owner actually reads must not spend its cap on findings it then
// captions as outdated.
func TestBuildDigestRanksFreshOverStale(t *testing.T) {
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}
	// Distinct subject words: collapseNearDuplicates merges titles that share
	// their normalized key, and that runs before the cap under test.
	refs := map[string]string{
		"cache invalidation is wrong":  "src/pkg/gone.go",
		"token refresh is wrong":       "src/pkg/removed.go",
		"queue draining is wrong":      "src/pkg/agent/manager.go",
		"clock skew handling is wrong": "src/pkg/hub/oauth.go",
	}
	for title, ref := range refs {
		if _, err := store.Create(title, beads.TypeAdvisory, severityToPriority("critical"), "quality", ref); err != nil {
			t.Fatalf("creating bead: %v", err)
		}
	}
	verify, _ := existsExcept("src/pkg/gone.go", "src/pkg/removed.go")

	d := BuildDigestFromBeads(map[string]*beads.Store{"quality": store}, "advisory", DigestOptions{
		MaxFindings: 2,
		VerifyPath:  verify,
		Snapshot:    &Snapshot{Owner: "hivecommons", Repo: "hive", SHA: "deadbeef"},
	})

	if !d.Capped || d.OverflowCount != 2 {
		t.Errorf("Capped = %v, OverflowCount = %d; want true, 2", d.Capped, d.OverflowCount)
	}
	if d.AnalyzedSnapshot == nil || d.AnalyzedSnapshot.SHA != "deadbeef" {
		t.Errorf("AnalyzedSnapshot = %+v, want the snapshot passed in opts", d.AnalyzedSnapshot)
	}
	for _, f := range digestFindings(d) {
		if f.PathStale {
			t.Errorf("finding %q kept despite a missing path — live findings of equal severity were available", f.Title)
		}
	}
}

// TestBuildDigestVerifiesPathsWhenUnderCap covers the branch applyTopN cannot:
// when nothing overflows it returns before verifying anything, so the survivors
// still need their paths checked or the renderer would cite a dead path as live.
func TestBuildDigestVerifiesPathsWhenUnderCap(t *testing.T) {
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}
	if _, err := store.Create("cache invalidation is wrong", beads.TypeAdvisory, severityToPriority("critical"), "quality", "src/pkg/gone.go"); err != nil {
		t.Fatalf("creating bead: %v", err)
	}
	verify, _ := existsExcept("src/pkg/gone.go")

	d := BuildDigestFromBeads(map[string]*beads.Store{"quality": store}, "advisory", DigestOptions{
		MaxFindings: 10,
		VerifyPath:  verify,
		Snapshot:    &Snapshot{Owner: "hivecommons", Repo: "hive", SHA: "deadbeef"},
	})

	if d.Capped {
		t.Fatalf("digest capped unexpectedly (overflow %d)", d.OverflowCount)
	}
	all := digestFindings(d)
	if len(all) != 1 {
		t.Fatalf("got %d findings, want 1", len(all))
	}
	if !all[0].PathStale {
		t.Error("PathStale = false, want true — an uncapped digest must still verify its findings' paths")
	}
}
