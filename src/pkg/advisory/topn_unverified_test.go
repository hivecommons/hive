package advisory

import (
	"strconv"
	"testing"

	"github.com/hivecommons/hive/pkg/beads"
)

// The digest's top-N is scarce, and staleness of the CITED PATH was only ever
// half of what makes a finding unverified. The advisory report on
// hivecommons/hive#2364 for 2026-09-04 rendered 10 findings out of 302, and
// three of them — including two of the four CRITICAL slots — carried a caption
// saying the reader should not trust them. The renderer has three such captions
// (missing path #3704, evidence from another commit #5130, cached replay
// #5236), but only the first one influenced which findings won a slot, so a
// finding nothing had re-checked could still displace one that was confirmed at
// the analyzed commit. These tests pin the generalized rule.

// unverifiedBase reuses the fixed clock and helpers from topn_stale_test.go
// (staleBase, finding, existsExcept, keptTitles, keptFinding, sameTitles).

// withProvenance stamps a finding with the commit its evidence was computed at.
func withProvenance(f Finding, sha string) Finding {
	f.ProvenanceSHA = sha
	return f
}

// withCachedReplays marks a finding as having been re-reported n times from
// byte-identical cached text, with no provenance commit of its own.
func withCachedReplays(f Finding, n int) Finding {
	f.CachedReplays = n
	return f
}

// ── provenance staleness demotes, like a missing path ───────────────────────

// TestApplyTopNPrefersConfirmedOverStaleProvenance: among equally-severe
// findings, the ones whose evidence was computed AT the analyzed commit win the
// slots. The stale-provenance findings here are the most recent, so under
// severity+recency alone they would have taken both slots and then been
// captioned "not re-verified at the analyzed commit".
func TestApplyTopNPrefersConfirmedOverStaleProvenance(t *testing.T) {
	byAgent := map[string][]Finding{
		"quality": {
			markProvStale(finding("quality", "critical", "stale-newest", "src/a.go", 1)),
			markProvStale(finding("quality", "critical", "stale-newer", "src/b.go", 2)),
			withProvenance(finding("quality", "critical", "confirmed-older", "src/c.go", 3), analyzedAtSHA),
			withProvenance(finding("quality", "critical", "confirmed-oldest", "src/d.go", 4), analyzedAtSHA),
		},
	}
	verify, _ := existsExcept()

	kept, overflow := applyTopN(byAgent, 2, verify)

	if overflow != 2 {
		t.Errorf("overflow = %d, want 2", overflow)
	}
	got := keptTitles(kept)
	want := []string{"confirmed-older", "confirmed-oldest"}
	if !sameTitles(got, want) {
		t.Errorf("kept = %v, want %v — evidence computed at the analyzed commit must take the slots even though the stale findings are newer", got, want)
	}
}

// markProvStale is what markStaleProvenanceIn would have produced for a finding
// whose evidence was computed at some other commit.
func markProvStale(f Finding) Finding {
	f.ProvenanceSHA = provenanceOfOne
	f.ProvenanceStale = true
	return f
}

// TestApplyTopNPrefersConfirmedOverCachedReplay: the #5236 signal demotes too. A
// finding whose only "confirmations" were byte-identical replays of cached text
// was never re-checked, so it does not outrank a finding of equal severity that
// was.
func TestApplyTopNPrefersConfirmedOverCachedReplay(t *testing.T) {
	byAgent := map[string][]Finding{
		"ci-maintainer": {
			withCachedReplays(finding("ci-maintainer", "high", "replayed-newest", "src/a.go", 1), 4),
			withCachedReplays(finding("ci-maintainer", "high", "replayed-newer", "src/b.go", 2), 1),
			finding("ci-maintainer", "high", "fresh-older", "src/c.go", 3),
		},
	}
	verify, _ := existsExcept()

	kept, overflow := applyTopN(byAgent, 2, verify)

	if overflow != 1 {
		t.Errorf("overflow = %d, want 1", overflow)
	}
	got := keptTitles(kept)
	want := []string{"fresh-older", "replayed-newest"}
	if !sameTitles(got, want) {
		t.Errorf("kept = %v, want %v — the freshly reported finding takes a slot, the newest replay backfills the other", got, want)
	}
}

// A replay count on a finding that DOES carry a provenance commit is not the
// #5236 signal — the renderer captions those by provenance, not by replay — so
// it must not demote on its own.
func TestApplyTopNReplayCountWithProvenanceDoesNotDemote(t *testing.T) {
	byAgent := map[string][]Finding{
		"quality": {
			withCachedReplays(withProvenance(finding("quality", "high", "replayed-but-pinned", "src/a.go", 1), analyzedAtSHA), 3),
			finding("quality", "high", "plain", "src/b.go", 2),
		},
	}
	verify, _ := existsExcept()

	kept, _ := applyTopN(byAgent, 1, verify)

	if got := keptTitles(kept); !sameTitles(got, []string{"replayed-but-pinned"}) {
		t.Errorf("kept = %v, want [replayed-but-pinned] — a replay count alongside a provenance commit is not the cached-replay signal", got)
	}
}

// ── the demotion stays a demotion ───────────────────────────────────────────

// TestApplyTopNBackfillsWithUnverifiedWhenConfirmedRunsOut: unverified means
// nobody re-checked it, not that it is fixed. A slot no confirmed finding claims
// still goes to an unverified one rather than rendering a short digest.
func TestApplyTopNBackfillsWithUnverifiedWhenConfirmedRunsOut(t *testing.T) {
	byAgent := map[string][]Finding{
		"quality": {
			markProvStale(finding("quality", "critical", "stale-a", "src/a.go", 1)),
			markProvStale(finding("quality", "critical", "stale-b", "src/b.go", 2)),
			markProvStale(finding("quality", "critical", "stale-c", "src/c.go", 3)),
			finding("quality", "critical", "confirmed", "src/d.go", 4),
		},
	}
	verify, _ := existsExcept()

	kept, overflow := applyTopN(byAgent, 3, verify)

	if overflow != 1 {
		t.Errorf("overflow = %d, want 1", overflow)
	}
	got := keptTitles(kept)
	want := []string{"confirmed", "stale-a", "stale-b"}
	if !sameTitles(got, want) {
		t.Errorf("kept = %v, want %v — slots no confirmed finding claims backfill with the newest unverified ones rather than rendering a short digest", got, want)
	}
}

// TestApplyTopNKeepsUnverifiedMarksOnBackfilled: a demoted finding that
// backfills a slot must arrive with its marks intact, or the renderer would
// publish it as confirmed — which is the overclaim (#5130) the caption exists
// to prevent.
func TestApplyTopNKeepsUnverifiedMarksOnBackfilled(t *testing.T) {
	byAgent := map[string][]Finding{
		"quality": {
			markProvStale(finding("quality", "critical", "stale-prov", "src/a.go", 1)),
			withCachedReplays(finding("quality", "critical", "replayed", "src/b.go", 2), 2),
			markProvStale(finding("quality", "critical", "surplus", "src/c.go", 3)),
		},
	}
	verify, _ := existsExcept()

	// Every finding is unverified, so both slots are filled by the backfill
	// path — which is the one that has to carry the marks through.
	kept, overflow := applyTopN(byAgent, 2, verify)

	if overflow != 1 {
		t.Fatalf("overflow = %d, want 1", overflow)
	}

	if f := keptFinding(t, kept, "stale-prov"); !f.ProvenanceStale || f.ProvenanceSHA != provenanceOfOne {
		t.Errorf("backfilled finding lost its provenance marks: stale=%v sha=%q", f.ProvenanceStale, f.ProvenanceSHA)
	}
	if f := keptFinding(t, kept, "replayed"); f.CachedReplays != 2 {
		t.Errorf("backfilled finding lost its replay count: got %d, want 2", f.CachedReplays)
	}
}

// TestApplyTopNSeverityBeatsEvidenceFreshness guards the same deliberate limit
// the path-staleness rule has: an unverified CRITICAL outranks a confirmed LOW.
// Unverified is "nobody re-checked this", not "this is gone", so letting it
// cross severity bands would let a nit displace a security finding.
func TestApplyTopNSeverityBeatsEvidenceFreshness(t *testing.T) {
	byAgent := map[string][]Finding{
		"sec-check": {markProvStale(finding("sec-check", "critical", "stale-critical", "src/a.go", 5))},
		"guide": {
			finding("guide", "low", "confirmed-low-a", "docs/a.md", 1),
			finding("guide", "low", "confirmed-low-b", "docs/b.md", 2),
		},
	}
	verify, _ := existsExcept()

	kept, overflow := applyTopN(byAgent, 1, verify)

	if overflow != 2 {
		t.Errorf("overflow = %d, want 2", overflow)
	}
	if got := keptTitles(kept); !sameTitles(got, []string{"stale-critical"}) {
		t.Errorf("kept = %v, want [stale-critical] — severity must outrank evidence freshness across bands", got)
	}
}

// The demotion must work with no path verifier at all: provenance staleness and
// replay counts are resolved from the findings themselves, so a caller that
// never resolves a snapshot path-checker still gets the ranking.
func TestApplyTopNDemotesUnverifiedWithoutVerifier(t *testing.T) {
	byAgent := map[string][]Finding{
		"quality": {
			markProvStale(finding("quality", "critical", "stale-newest", "src/a.go", 1)),
			finding("quality", "critical", "confirmed-older", "src/b.go", 2),
		},
	}

	kept, _ := applyTopN(byAgent, 1, nil)

	if got := keptTitles(kept); !sameTitles(got, []string{"confirmed-older"}) {
		t.Errorf("kept = %v, want [confirmed-older] — provenance ranking needs no path verifier", got)
	}
}

// ── through BuildDigestFromBeads ────────────────────────────────────────────

// TestBuildDigestRanksConfirmedOverStaleProvenance is the end-to-end version:
// the digest a repo owner actually reads must not spend its cap on findings it
// then captions "not re-verified at the analyzed commit". Provenance staleness
// therefore has to be resolved BEFORE the cap, which is the ordering bug this
// fixes — it used to run only after applyTopN had already chosen.
func TestBuildDigestRanksConfirmedOverStaleProvenance(t *testing.T) {
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}
	// Distinct subject words: collapseNearDuplicates merges titles that share
	// their subject tokens, and it runs before the cap under test.
	// Seeded oldest-first, with the stale-provenance findings LAST so they are
	// the most recent: severity+recency alone would hand them both slots, which
	// is exactly the ranking under test.
	seed := []struct {
		title string
		prov  string
	}{
		{"queue draining is wrong", analyzedAtSHA},
		{"clock skew handling is wrong", analyzedAtSHA},
		{"cache invalidation is wrong", provenanceOfOne},
		{"token refresh is wrong", provenanceOfOne},
	}
	for _, s := range seed {
		b, err := store.Create(s.title, beads.TypeAdvisory, severityToPriority("critical"), "quality", "")
		if err != nil {
			t.Fatalf("creating bead: %v", err)
		}
		if err := store.SetMetadata(b.ID, provenanceSHAMetadataKey, s.prov); err != nil {
			t.Fatalf("stamping provenance: %v", err)
		}
	}

	d := BuildDigestFromBeads(map[string]*beads.Store{"quality": store}, "advisory", DigestOptions{
		MaxFindings: 2,
		Snapshot:    &Snapshot{Owner: "hivecommons", Repo: "hive", SHA: analyzedAtSHA},
	})

	if !d.Capped || d.OverflowCount != 2 {
		t.Fatalf("Capped = %v, OverflowCount = %d; want true, 2", d.Capped, d.OverflowCount)
	}
	for _, f := range digestFindings(d) {
		if f.ProvenanceStale {
			t.Errorf("finding %q kept despite evidence from another commit — findings confirmed at the analyzed commit were available", f.Title)
		}
	}
}

// TestBuildDigestRanksConfirmedOverCachedReplay is the #5236 half end-to-end:
// the replay count lives in bead metadata, so the digest can and must read it
// before deciding which findings are worth a slot.
func TestBuildDigestRanksConfirmedOverCachedReplay(t *testing.T) {
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}
	// Replayed findings seeded LAST, so they are the most recent — see the
	// ordering note in TestBuildDigestRanksConfirmedOverStaleProvenance.
	seed := []struct {
		title   string
		replays int
	}{
		{"queue draining is wrong", 0},
		{"clock skew handling is wrong", 0},
		{"cache invalidation is wrong", 5},
		{"token refresh is wrong", 3},
	}
	for _, s := range seed {
		b, err := store.Create(s.title, beads.TypeAdvisory, severityToPriority("high"), "quality", "")
		if err != nil {
			t.Fatalf("creating bead: %v", err)
		}
		if err := store.SetMetadata(b.ID, evidenceReplayCountMetadataKey, strconv.Itoa(s.replays)); err != nil {
			t.Fatalf("stamping replay count: %v", err)
		}
	}

	d := BuildDigestFromBeads(map[string]*beads.Store{"quality": store}, "advisory", DigestOptions{
		MaxFindings: 2,
		Snapshot:    &Snapshot{Owner: "hivecommons", Repo: "hive", SHA: analyzedAtSHA},
	})

	if !d.Capped || d.OverflowCount != 2 {
		t.Fatalf("Capped = %v, OverflowCount = %d; want true, 2", d.Capped, d.OverflowCount)
	}
	for _, f := range digestFindings(d) {
		if f.CachedReplays > 0 {
			t.Errorf("finding %q kept despite being pure cached replay — freshly reported findings were available", f.Title)
		}
	}
}

// A digest with no pinned snapshot cannot judge provenance freshness, so the
// ranking must be exactly what it was before. This keeps every caller that does
// not resolve a snapshot on the previous severity+recency behavior.
func TestBuildDigestWithoutSnapshotKeepsSeverityRecencyRanking(t *testing.T) {
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}
	for _, title := range []string{"cache invalidation is wrong", "token refresh is wrong"} {
		b, err := store.Create(title, beads.TypeAdvisory, severityToPriority("high"), "quality", "")
		if err != nil {
			t.Fatalf("creating bead: %v", err)
		}
		// Provenance that does not match anything: with no snapshot there is
		// nothing to compare it against, so it must not be treated as stale.
		if err := store.SetMetadata(b.ID, provenanceSHAMetadataKey, provenanceOfOne); err != nil {
			t.Fatalf("stamping provenance: %v", err)
		}
	}

	d := BuildDigestFromBeads(map[string]*beads.Store{"quality": store}, "advisory", DigestOptions{MaxFindings: 1})

	all := digestFindings(d)
	if len(all) != 1 {
		t.Fatalf("got %d findings, want 1", len(all))
	}
	if all[0].ProvenanceStale {
		t.Error("ProvenanceStale = true without an analyzed snapshot — nothing may be judged stale against a commit the digest does not name")
	}
}
