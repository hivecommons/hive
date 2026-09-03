package advisory

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
)

// The finding from #5236, verbatim in shape: a shell-coverage claim against
// Danathar/atomic-image-builder that carried no provenance, was disproved by
// the implementation, and stayed live as "reported 3×" because every cached
// replay counted as fresh confirmation.
func shellCoverageFinding() Finding {
	return Finding{
		Agent:    "quality",
		Severity: "medium",
		Type:     "coverage-gap",
		Title:    "No coverage measurement for contrib/aib and container/entrypoint.sh shell scripts",
		Detail:   "Neither has any statement/branch coverage tool wired (no kcov/bashcov)",
		File:     "contrib/aib",
	}
}

func TestFindingEvidenceHashDiscriminatesEveryField(t *testing.T) {
	base := shellCoverageFinding()
	if findingEvidenceHash(base) != findingEvidenceHash(shellCoverageFinding()) {
		t.Fatal("identical findings must hash identically or every replay reads as new evidence")
	}
	for name, mutate := range map[string]func(*Finding){
		"title":  func(f *Finding) { f.Title += " (run #3291)" },
		"detail": func(f *Finding) { f.Detail = "re-checked: still no bashcov wired" },
		"file":   func(f *Finding) { f.File = "container/entrypoint.sh" },
		"line":   func(f *Finding) { f.Line = 12 },
	} {
		t.Run(name, func(t *testing.T) {
			f := shellCoverageFinding()
			mutate(&f)
			if findingEvidenceHash(f) == findingEvidenceHash(base) {
				t.Errorf("a changed %s must change the evidence hash — anything the producer touched is a recomputation, not replay", name)
			}
		})
	}
}

// TestPersistAsBeadsIdenticalNoProvenanceReplayDoesNotRefresh is the #5236
// regression fixture. The first report must create a normally-stamped bead (a
// genuinely new no-provenance finding is never silently discarded); identical
// cached re-reports must NOT refresh the staleness clock, so pruning finally
// retires the bead on the normal schedule.
func TestPersistAsBeadsIdenticalNoProvenanceReplayDoesNotRefresh(t *testing.T) {
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}
	stores := map[string]*beads.Store{"quality": store}

	if created := PersistAsBeads([]Finding{shellCoverageFinding()}, stores); created != 1 {
		t.Fatalf("first report created %d beads, want 1", created)
	}
	b := store.List(beads.ListFilter{})[0]
	if _, ok := b.LastSeen(); !ok {
		t.Fatal("first report did not stamp LastSeenAt — the staleness clock never started")
	}
	if got := b.Meta(evidenceHashMetadataKey); got != findingEvidenceHash(shellCoverageFinding()) {
		t.Errorf("evidence hash metadata = %q, want the report's own hash", got)
	}

	stale := time.Now().Add(-10 * 24 * time.Hour)
	if err := store.SetLastSeenAt(b.ID, stale); err != nil {
		t.Fatalf("stamping bead: %v", err)
	}

	// "reported 3×": two more byte-identical replays of the cached finding.
	for i := 0; i < 2; i++ {
		PersistAsBeads([]Finding{shellCoverageFinding()}, stores)
	}
	after, err := store.Get(b.ID)
	if err != nil {
		t.Fatalf("re-reading bead: %v", err)
	}
	seen, _ := after.LastSeen()
	if !seen.Equal(stale.UTC()) {
		t.Errorf("identical no-provenance replay refreshed LastSeenAt to %s; cached text must not count as confirmation", seen)
	}
	if got := after.Meta(evidenceReplayCountMetadataKey); got != "2" {
		t.Errorf("replay count metadata = %q, want %q", got, "2")
	}
	if n := len(store.List(beads.ListFilter{})); n != 1 {
		t.Fatalf("replays created extra beads: store holds %d", n)
	}

	// The whole point: the disproved finding now ages out within one window.
	if pruned := PruneStaleAdvisoryBeads(stores, 7*24*time.Hour); len(pruned) != 1 {
		t.Errorf("stale replayed finding was not retired: pruned %v", pruned)
	}
}

// Changed evidence IS a recomputation: a no-provenance re-report whose text
// differs in any way must keep refreshing exactly as before #5236, and the
// bead must adopt the new evidence identity with its replay count reset.
func TestPersistAsBeadsChangedNoProvenanceEvidenceRefreshes(t *testing.T) {
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}
	stores := map[string]*beads.Store{"quality": store}

	PersistAsBeads([]Finding{shellCoverageFinding()}, stores)
	b := store.List(beads.ListFilter{})[0]
	stale := time.Now().Add(-10 * 24 * time.Hour)
	if err := store.SetLastSeenAt(b.ID, stale); err != nil {
		t.Fatalf("stamping bead: %v", err)
	}
	// A replay first, so the reset below is observable.
	PersistAsBeads([]Finding{shellCoverageFinding()}, stores)

	changed := shellCoverageFinding()
	changed.Detail = "re-checked at HEAD: bashcov still absent from ci.yml"
	PersistAsBeads([]Finding{changed}, stores)

	after, err := store.Get(b.ID)
	if err != nil {
		t.Fatalf("re-reading bead: %v", err)
	}
	seen, _ := after.LastSeen()
	if !seen.After(stale.UTC()) {
		t.Error("a re-report with changed evidence must refresh LastSeenAt")
	}
	if got := after.Meta(evidenceHashMetadataKey); got != findingEvidenceHash(changed) {
		t.Errorf("evidence hash = %q, want the changed report's hash", got)
	}
	if got := after.Meta(evidenceReplayCountMetadataKey); got != "0" {
		t.Errorf("replay count = %q, want it reset to 0 on changed evidence", got)
	}
}

// Re-verification with newer, explicit provenance refreshes even when the
// finding text is byte-identical: the producer states the evidence was
// recomputed at a named commit, which is exactly the strong signal the
// evidence gate exists to stand in for.
func TestPersistAsBeadsExplicitProvenanceBypassesEvidenceGate(t *testing.T) {
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}
	stores := map[string]*beads.Store{"quality": store}

	PersistAsBeads([]Finding{shellCoverageFinding()}, stores)
	b := store.List(beads.ListFilter{})[0]
	stale := time.Now().Add(-10 * 24 * time.Hour)
	if err := store.SetLastSeenAt(b.ID, stale); err != nil {
		t.Fatalf("stamping bead: %v", err)
	}

	verified := shellCoverageFinding()
	verified.ProvenanceSHA = analyzedAtSHA
	PersistAsBeads([]Finding{verified}, stores)

	after, err := store.Get(b.ID)
	if err != nil {
		t.Fatalf("re-reading bead: %v", err)
	}
	seen, _ := after.LastSeen()
	if !seen.After(stale.UTC()) {
		t.Error("identical text re-verified under explicit provenance must refresh LastSeenAt")
	}
	if got := after.Meta(provenanceSHAMetadataKey); got != analyzedAtSHA {
		t.Errorf("provenance metadata = %q, want the newly stated %q", got, analyzedAtSHA)
	}
}

// The gate has to recognise the bead a replay would land on even when the
// stored evidence came in under a cosmetically drifted title that Upsert
// folded: the bead keeps its original title, the hash describes the drifted
// report, and a verbatim replay of THAT report must still be gated.
func TestPersistAsBeadsEvidenceGateFollowsTitleDrift(t *testing.T) {
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}
	stores := map[string]*beads.Store{"quality": store}

	first := Finding{Agent: "quality", Severity: "high", Title: "pr-verifier.yml failing (run #3279)"}
	PersistAsBeads([]Finding{first}, stores)
	b := store.List(beads.ListFilter{})[0]

	// Drifted title, so the hash differs: refreshes via the Upsert fold and
	// re-stamps the stored evidence hash with the drifted report's.
	drifted := Finding{Agent: "quality", Severity: "high", Title: "pr-verifier.yml failing (run #3291)"}
	PersistAsBeads([]Finding{drifted}, stores)
	if n := len(store.List(beads.ListFilter{})); n != 1 {
		t.Fatalf("drifted re-report created a second bead: store holds %d", n)
	}

	stale := time.Now().Add(-10 * 24 * time.Hour)
	if err := store.SetLastSeenAt(b.ID, stale); err != nil {
		t.Fatalf("stamping bead: %v", err)
	}
	PersistAsBeads([]Finding{drifted}, stores)

	after, err := store.Get(b.ID)
	if err != nil {
		t.Fatalf("re-reading bead: %v", err)
	}
	seen, _ := after.LastSeen()
	if !seen.Equal(stale.UTC()) {
		t.Errorf("verbatim replay of the folded report refreshed LastSeenAt to %s; the gate must match the bead the way Upsert would", seen)
	}
}

// Beads written before evidence_hash existed carry no stored hash, and "cannot
// tell" must mean "do not gate": the first re-report refreshes as before (and
// stamps the hash), so only the second identical replay is recognised.
func TestPersistAsBeadsLegacyBeadWithoutHashRefreshesOnce(t *testing.T) {
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}
	stores := map[string]*beads.Store{"quality": store}
	f := shellCoverageFinding()

	// A pre-#5236 bead: same title, no evidence metadata at all.
	legacy, err := store.Upsert(f.Title, beads.TypeAdvisory, beads.PriorityMedium, "quality", "")
	if err != nil {
		t.Fatalf("creating legacy bead: %v", err)
	}
	stale := time.Now().Add(-10 * 24 * time.Hour)
	if err := store.SetLastSeenAt(legacy.ID, stale); err != nil {
		t.Fatalf("stamping bead: %v", err)
	}

	PersistAsBeads([]Finding{f}, stores)
	after, err := store.Get(legacy.ID)
	if err != nil {
		t.Fatalf("re-reading bead: %v", err)
	}
	seen, _ := after.LastSeen()
	if !seen.After(stale.UTC()) {
		t.Fatal("a legacy bead with no stored hash must refresh on the first re-report — an empty hash is not evidence of replay")
	}
	if got := after.Meta(evidenceHashMetadataKey); got != findingEvidenceHash(f) {
		t.Errorf("first re-report did not stamp the evidence hash (got %q)", got)
	}

	if err := store.SetLastSeenAt(legacy.ID, stale); err != nil {
		t.Fatalf("re-stamping bead: %v", err)
	}
	PersistAsBeads([]Finding{f}, stores)
	after, err = store.Get(legacy.ID)
	if err != nil {
		t.Fatalf("re-reading bead: %v", err)
	}
	seen, _ = after.LastSeen()
	if !seen.Equal(stale.UTC()) {
		t.Error("once the hash is stamped, an identical replay must stop refreshing")
	}
}

// The reader-facing half of #5236: a finding kept in the digest only by cached
// replays must carry an unverified caption, so the digest never presents the
// possibly-disproved claim and a downstream refutation as equally live work.
// A finding without replays renders uncaptioned exactly as before.
func TestBuildDigestCaptionsCachedReplays(t *testing.T) {
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}
	stores := map[string]*beads.Store{"quality": store}

	replayed := shellCoverageFinding()
	fresh := Finding{Agent: "quality", Severity: "medium", Type: "coverage-gap", Title: "a finding reported exactly once"}
	PersistAsBeads([]Finding{replayed, fresh}, stores)
	// Two cached replays — the digest should say so, not count them as
	// confirmations.
	PersistAsBeads([]Finding{replayed}, stores)
	PersistAsBeads([]Finding{replayed}, stores)

	d := BuildDigestFromBeads(stores, "advisory", DigestOptions{
		Snapshot: &Snapshot{Owner: "Danathar", Repo: "atomic-image-builder", SHA: analyzedAtSHA},
	})
	var got Finding
	for _, f := range d.ByAgent["quality"] {
		if f.Title == replayed.Title {
			got = f
		}
	}
	if got.CachedReplays != 2 {
		t.Fatalf("digest finding carries CachedReplays=%d, want 2", got.CachedReplays)
	}

	out := FormatDigestMarkdown(d, DigestOptions{Org: "Danathar", PrimaryRepo: "atomic-image-builder"})
	want := "re-reported " + strconv.Itoa(got.CachedReplays) + "× from cached evidence, not re-verified"
	if !strings.Contains(out, want) {
		t.Errorf("replayed finding is not captioned as unverified:\n%s", out)
	}
	if strings.Contains(out, "a finding reported exactly once ⚠️") {
		t.Errorf("a finding with no cached replays must not be captioned:\n%s", out)
	}
}
