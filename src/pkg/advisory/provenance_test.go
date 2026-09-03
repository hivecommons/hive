package advisory

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
)

// The two commits from #5130: the digest's analyzed-at HEAD, and the older
// commit one of its republished findings was actually computed at.
const (
	analyzedAtSHA   = "29cb70657c255aceaf53b6b2bc50bbf5433e9a00"
	provenanceOfOne = "c9546a8a24b3dded3146e3ab7a93dd99edc56fa3"
)

func TestNormalizeSHA(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"full sha", analyzedAtSHA, analyzedAtSHA},
		{"abbreviated", "c9546a8", "c9546a8"},
		{"uppercase folds", "C9546A8", "c9546a8"},
		{"backticks and spaces stripped", "  `c9546a8` ", "c9546a8"},
		{"too short", "c9546a", ""},
		{"too long", strings.Repeat("a", 41), ""},
		{"not hex", "notahexsha", ""},
		{"the SHA: unknown footer literal", "unknown", ""},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeSHA(tc.in); got != tc.want {
				t.Errorf("normalizeSHA(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestSameCommitToleratesAbbreviation is the comparison that decides whether a
// finding is republished under a commit it was not computed at. Findings cite
// short SHAs while the snapshot carries the full 40, so exact equality would
// mark every single finding stale.
func TestSameCommitToleratesAbbreviation(t *testing.T) {
	if !sameCommit("29cb706", analyzedAtSHA) {
		t.Error("abbreviated SHA should match the full SHA it prefixes")
	}
	if !sameCommit(analyzedAtSHA, analyzedAtSHA) {
		t.Error("a SHA should match itself")
	}
	if sameCommit(provenanceOfOne, analyzedAtSHA) {
		t.Error("two different commits must not compare equal")
	}
	// "Cannot tell" is never "same": an unusable id must not silently suppress
	// the staleness marker, nor gate a finding out of the keep-alive path.
	if sameCommit("", analyzedAtSHA) || sameCommit("unknown", analyzedAtSHA) {
		t.Error("an unusable commit id must not compare equal to anything")
	}
}

// TestExtractProvenanceSHA covers the recovery path that makes this fix work on
// findings that already exist: agents write provenance as prose, so the SHAs in
// the #5130 digest are only reachable by reading Detail.
func TestExtractProvenanceSHA(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "finding 1 wording from #5130",
			in:   "contrib/aib has no test coverage; revision " + provenanceOfOne,
			want: provenanceOfOne,
		},
		{
			name: "finding 2 wording from #5130",
			in:   "36 remaining partial branches (CI run 33187518367, commit 9a6313c)",
			want: "9a6313c",
		},
		{"backticked", "computed at `29cb706`", "29cb706"},
		{"provenance label", "provenance: c9546a8", "c9546a8"},
		{"as of", "as of 29cb706 the suite passes", "29cb706"},
		{"no keyword means no match", "hash deadbeef1234 appeared in the log", ""},
		{"unrelated long numbers", "CI run 33187518367 failed", ""},
		{"sha unknown is not a commit", "SHA: unknown", ""},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractProvenanceSHA(tc.in); got != tc.want {
				t.Errorf("extractProvenanceSHA(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestMarkStaleProvenanceFlagsOlderEvidence reproduces #5130 directly: a digest
// stamped "Analyzed at 29cb706" carrying a finding whose evidence was computed
// at c9546a8 must say so, while a finding computed at the analyzed commit is
// left clean.
func TestMarkStaleProvenanceFlagsOlderEvidence(t *testing.T) {
	d := &Digest{
		AnalyzedSnapshot: &Snapshot{Owner: "Danathar", Repo: "atomic-image-builder", SHA: analyzedAtSHA},
		ByAgent: map[string][]Finding{
			"quality": {
				{
					Agent:  "quality",
					Title:  "contrib/aib has no test coverage",
					Detail: "revision " + provenanceOfOne,
				},
				{
					Agent:         "quality",
					Title:         "computed against the analyzed commit",
					ProvenanceSHA: "29cb706",
				},
				{
					Agent: "quality",
					Title: "no provenance anywhere",
				},
			},
		},
	}
	MarkStaleProvenance(d)

	got := d.ByAgent["quality"]
	if !got[0].ProvenanceStale {
		t.Error("a finding computed at an older commit must be marked stale")
	}
	if got[0].ProvenanceSHA != provenanceOfOne {
		t.Errorf("provenance SHA = %q, want the one recovered from Detail (%q)", got[0].ProvenanceSHA, provenanceOfOne)
	}
	if got[1].ProvenanceStale {
		t.Error("a finding computed AT the analyzed commit must not be marked stale")
	}
	// Silence about provenance is not a freshness claim in either direction:
	// an unmarked finding must not be captioned as verified OR as stale.
	if got[2].ProvenanceStale || got[2].ProvenanceSHA != "" {
		t.Errorf("a finding with no provenance must be left unmarked, got stale=%v sha=%q", got[2].ProvenanceStale, got[2].ProvenanceSHA)
	}
}

// A digest with no pinned snapshot cannot judge freshness, so it must not
// pretend to. This keeps every caller that does not resolve a snapshot (older
// flows, tests) on exactly the previous behavior.
func TestMarkStaleProvenanceNoopWithoutSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name string
		d    *Digest
	}{
		{"nil digest", nil},
		{"no snapshot", &Digest{ByAgent: map[string][]Finding{"q": {{Detail: "commit " + provenanceOfOne}}}}},
		{"snapshot with unusable sha", &Digest{
			AnalyzedSnapshot: &Snapshot{SHA: "unknown"},
			ByAgent:          map[string][]Finding{"q": {{Detail: "commit " + provenanceOfOne}}},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			MarkStaleProvenance(tc.d)
			if tc.d == nil {
				return
			}
			if tc.d.ByAgent["q"][0].ProvenanceStale {
				t.Error("nothing may be marked stale without a usable analyzed commit")
			}
		})
	}
}

// TestFormatDigestMarkdownCaptionsStaleProvenance is the reader-facing half of
// the fix: the #5130 finding must carry its own provenance caption rather than
// being covered by the digest-wide "Analyzed at" stamp, and the footer must
// stop claiming more freshness than it has.
func TestFormatDigestMarkdownCaptionsStaleProvenance(t *testing.T) {
	d := &Digest{
		GeneratedAt:      time.Now(),
		TotalCount:       2,
		AnalyzedSnapshot: &Snapshot{Owner: "Danathar", Repo: "atomic-image-builder", SHA: analyzedAtSHA},
		ByAgent: map[string][]Finding{
			"quality": {
				{
					Agent:    "quality",
					Type:     "coverage-gap",
					Severity: "high",
					Title:    "contrib/aib has no test coverage",
					Detail:   "revision " + provenanceOfOne,
				},
				{
					Agent:    "quality",
					Type:     "coverage-gap",
					Severity: "high",
					Title:    "still reproduces at the analyzed commit",
				},
			},
		},
	}
	MarkStaleProvenance(d)
	out := FormatDigestMarkdown(d, DigestOptions{Org: "Danathar", PrimaryRepo: "atomic-image-builder"})

	if !strings.Contains(out, "evidence computed at `c9546a8`, not re-verified at the analyzed commit") {
		t.Errorf("stale-provenance finding is not captioned:\n%s", out)
	}
	if strings.Contains(out, "still reproduces at the analyzed commit ⚠️") {
		t.Errorf("a finding with no stale provenance must not be captioned:\n%s", out)
	}
	if !strings.Contains(out, "have NOT been re-verified here") {
		t.Errorf("footer does not disclose that ⚠️ findings are unverified:\n%s", out)
	}
}

// TestPersistAsBeadsGatesKeepAliveOnProvenance is the behavioral half: a
// re-report from the SAME evidence must not refresh the staleness clock, which
// is what let the #5130 findings survive five regeneration cycles after their
// fix landed.
func TestPersistAsBeadsGatesKeepAliveOnProvenance(t *testing.T) {
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}
	stores := map[string]*beads.Store{"quality": store}
	finding := Finding{
		Agent:         "quality",
		Severity:      "high",
		Type:          "coverage-gap",
		Title:         "contrib/aib has no test coverage",
		ProvenanceSHA: provenanceOfOne,
	}

	if created := PersistAsBeads([]Finding{finding}, stores); created != 1 {
		t.Fatalf("first report created %d beads, want 1", created)
	}
	open := store.List(beads.ListFilter{})
	if len(open) != 1 {
		t.Fatalf("store holds %d beads, want 1", len(open))
	}
	b := open[0]
	if got := b.Meta(provenanceSHAMetadataKey); got != provenanceOfOne {
		t.Errorf("provenance metadata = %q, want %q", got, provenanceOfOne)
	}

	// Age the bead past a staleness window, then re-report the identical
	// evidence. The clock must keep running.
	stale := time.Now().Add(-10 * 24 * time.Hour)
	if err := store.SetLastSeenAt(b.ID, stale); err != nil {
		t.Fatalf("stamping bead: %v", err)
	}
	PersistAsBeads([]Finding{finding}, stores)
	after, err := store.Get(b.ID)
	if err != nil {
		t.Fatalf("re-reading bead: %v", err)
	}
	seen, ok := after.LastSeen()
	if !ok {
		t.Fatal("bead lost its LastSeenAt stamp")
	}
	if !seen.Equal(stale.UTC()) {
		t.Errorf("identical-provenance re-report refreshed LastSeenAt to %s; it must leave the staleness clock running", seen)
	}
	if pruned := PruneStaleAdvisoryBeads(stores, 7*24*time.Hour); len(pruned) != 1 {
		t.Errorf("stale finding was not retired: pruned %v", pruned)
	}
}

// The mirror of the test above: evidence RE-COMPUTED at a newer commit is a
// genuine confirmation, so it must refresh the clock and record the new
// provenance. Without this the gate would retire findings that still hold.
func TestPersistAsBeadsRefreshesOnNewProvenance(t *testing.T) {
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}
	stores := map[string]*beads.Store{"quality": store}
	title := "contrib/aib has no test coverage"

	PersistAsBeads([]Finding{{
		Agent: "quality", Severity: "high", Title: title, ProvenanceSHA: provenanceOfOne,
	}}, stores)
	b := store.List(beads.ListFilter{})[0]
	stale := time.Now().Add(-10 * 24 * time.Hour)
	if err := store.SetLastSeenAt(b.ID, stale); err != nil {
		t.Fatalf("stamping bead: %v", err)
	}

	PersistAsBeads([]Finding{{
		Agent: "quality", Severity: "high", Title: title, ProvenanceSHA: analyzedAtSHA,
	}}, stores)

	after, err := store.Get(b.ID)
	if err != nil {
		t.Fatalf("re-reading bead: %v", err)
	}
	seen, _ := after.LastSeen()
	if !seen.After(stale.UTC()) {
		t.Error("a re-report computed at a NEWER commit must refresh LastSeenAt")
	}
	if got := after.Meta(provenanceSHAMetadataKey); got != analyzedAtSHA {
		t.Errorf("provenance metadata = %q, want the newly recorded %q", got, analyzedAtSHA)
	}
}

// The "findings without provenance are unaffected" contract that used to be
// pinned here was consciously retired by #5236: a byte-identical no-provenance
// re-report is now recognised as cached replay and no longer refreshes
// LastSeenAt. The replacement contracts live in evidence_test.go.

// The prose-inferred SHA is good enough to caption a finding but must never
// decide whether it ages out: "fixed in commit <sha>" in a Detail would
// otherwise retire a finding that still holds. The re-report varies its
// wording (while still citing the same commit) because a byte-identical
// replay is now gated on evidence identity (#5236), which is not what this
// test is about.
func TestPersistAsBeadsIgnoresProseProvenance(t *testing.T) {
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}
	stores := map[string]*beads.Store{"quality": store}
	f := Finding{
		Agent:    "quality",
		Severity: "high",
		Title:    "a finding whose detail merely mentions a commit",
		Detail:   "regressed in commit " + provenanceOfOne,
	}

	PersistAsBeads([]Finding{f}, stores)
	b := store.List(beads.ListFilter{})[0]
	if got := b.Meta(provenanceSHAMetadataKey); got != "" {
		t.Errorf("prose-inferred SHA was recorded as provenance metadata (%q); only explicit provenance may be", got)
	}
	stale := time.Now().Add(-10 * 24 * time.Hour)
	if err := store.SetLastSeenAt(b.ID, stale); err != nil {
		t.Fatalf("stamping bead: %v", err)
	}

	f.Detail = "still regressed in commit " + provenanceOfOne + ", re-checked today"
	PersistAsBeads([]Finding{f}, stores)
	after, err := store.Get(b.ID)
	if err != nil {
		t.Fatalf("re-reading bead: %v", err)
	}
	seen, _ := after.LastSeen()
	if !seen.After(stale.UTC()) {
		t.Error("a prose-only commit mention must not gate the keep-alive refresh")
	}
}

// The gate has to recognise the same bead Upsert would, or an agent's cosmetic
// title drift ("run #3279" -> "run #3291") would slip identical evidence past
// it every cycle — exactly the drift beads.UpsertTitleKey exists to fold.
func TestPersistAsBeadsGateFollowsTitleDrift(t *testing.T) {
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}
	stores := map[string]*beads.Store{"quality": store}

	PersistAsBeads([]Finding{{
		Agent: "quality", Severity: "high",
		Title:         "pr-verifier.yml failing (run #3279)",
		ProvenanceSHA: provenanceOfOne,
	}}, stores)
	b := store.List(beads.ListFilter{})[0]
	stale := time.Now().Add(-10 * 24 * time.Hour)
	if err := store.SetLastSeenAt(b.ID, stale); err != nil {
		t.Fatalf("stamping bead: %v", err)
	}

	PersistAsBeads([]Finding{{
		Agent: "quality", Severity: "high",
		Title:         "pr-verifier.yml failing (run #3291)",
		ProvenanceSHA: provenanceOfOne,
	}}, stores)

	if n := len(store.List(beads.ListFilter{})); n != 1 {
		t.Fatalf("drifted re-report created a second bead: store holds %d", n)
	}
	after, err := store.Get(b.ID)
	if err != nil {
		t.Fatalf("re-reading bead: %v", err)
	}
	seen, _ := after.LastSeen()
	if !seen.Equal(stale.UTC()) {
		t.Errorf("title drift let identical evidence refresh LastSeenAt (now %s)", seen)
	}
}

// Provenance recorded on the bead must survive the round trip into the digest,
// so a finding persisted with explicit provenance is captioned on the next
// cycle without re-parsing prose.
func TestBuildDigestFromBeadsCarriesProvenance(t *testing.T) {
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}
	stores := map[string]*beads.Store{"quality": store}
	PersistAsBeads([]Finding{{
		Agent: "quality", Severity: "high", Type: "coverage-gap",
		Title:         "contrib/aib has no test coverage",
		ProvenanceSHA: provenanceOfOne,
	}}, stores)

	d := BuildDigestFromBeads(stores, "advisory", DigestOptions{
		Snapshot: &Snapshot{Owner: "Danathar", Repo: "atomic-image-builder", SHA: analyzedAtSHA},
	})
	if len(d.ByAgent["quality"]) != 1 {
		t.Fatalf("digest holds %d findings, want 1", len(d.ByAgent["quality"]))
	}
	f := d.ByAgent["quality"][0]
	if f.ProvenanceSHA != provenanceOfOne {
		t.Errorf("provenance SHA = %q, want %q", f.ProvenanceSHA, provenanceOfOne)
	}
	if !f.ProvenanceStale {
		t.Error("a finding computed at an older commit must be marked stale in the built digest")
	}
}
