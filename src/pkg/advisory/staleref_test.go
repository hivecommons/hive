package advisory

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
)

// #6080: the 2026-09-05 advisory digest on Danathar/atomic-image-builder#11
// carried two findings computed at older commits into its counted, severity
// ranked, top-10 OPEN list. One was listed open at the exact commit that fixed
// it. The other named its own remediation in its own body, and both of the
// items it named had closed roughly 23.5 hours before the digest ran.
//
// The pipeline already held everything it needed: the per-finding provenance
// metadata differed from the digest's analyzed commit, and the finding text
// named the work that settled it. Nothing acted on either.
//
// Every case below that shows a finding being retired has a counterpart showing
// the same finding staying open when one input changes. A guard that retires
// findings is one bug away from silently deleting live ones.

// refBase is a fixed clock: closure ages are measured against refClosedWindow.
var refBase = time.Date(2026, 9, 5, 11, 48, 0, 0, time.UTC)

func refKey(owner, repo string, number int) string {
	result := fmt.Sprintf("%s/%s#%d", owner, repo, number)
	return result
}

// closedRefs builds a ResolveRef over a fixed table, plus a lookup counter. A
// ref absent from the table reports OPEN; one mapped to the zero time reports
// closed with no timestamp.
func closedRefs(closed map[string]time.Time) (ResolveRef, *int) {
	calls := 0
	resolve := func(owner, repo string, number int) (RefState, bool) {
		calls++
		at, ok := closed[refKey(owner, repo, number)]
		if !ok {
			return RefState{}, true
		}
		return RefState{Closed: true, ClosedAt: at}, true
	}
	return resolve, &calls
}

// unknownRefs is a resolver that can never tell -- the network-error and
// rate-limit shape. It must never retire anything.
func unknownRefs() ResolveRef {
	return func(string, string, int) (RefState, bool) { return RefState{}, false }
}

// TestFindingIssueRefsExtraction pins where a finding may name GitHub work.
// The bare "#208" form is the one that matters most: linkifyRefs deliberately
// ignores it because GitHub autolinks it, and that is exactly how finding 2 in
// #6080 named the issue and PR that had already settled it.
func TestFindingIssueRefsExtraction(t *testing.T) {
	cases := []struct {
		name string
		f    Finding
		want []issueRef
	}{
		{
			name: "bare refs in the detail resolve against the analyzed repo",
			f:    Finding{Detail: "Filed issue #208, hold-gated PR #209"},
			want: []issueRef{{"Danathar", "atomic-image-builder", 208}, {"Danathar", "atomic-image-builder", 209}},
		},
		{
			name: "a bare ref in parentheses still counts",
			f:    Finding{Detail: "see the tracking issue (#208) for context"},
			want: []issueRef{{"Danathar", "atomic-image-builder", 208}},
		},
		{
			name: "an inline repo ref names its own repo",
			f:    Finding{Detail: "fixed by arch-bootc#7"},
			want: []issueRef{{"Danathar", "arch-bootc", 7}},
		},
		{
			name: "a fully qualified ref names its own owner",
			f:    Finding{Detail: "fixed by hivecommons/hive#4242"},
			want: []issueRef{{"hivecommons", "hive", 4242}},
		},
		{
			name: "a gh-N external ref resolves against the analyzed repo",
			f:    Finding{File: "gh-99"},
			want: []issueRef{{"Danathar", "atomic-image-builder", 99}},
		},
		{
			name: "a gh-prefixed qualified external ref loses the prefix",
			f:    Finding{File: "gh-Danathar/arch-bootc#123"},
			want: []issueRef{{"Danathar", "arch-bootc", 123}},
		},
		{
			name: "a file path names nothing",
			f:    Finding{File: "src/pkg/advisory/advisory.go"},
			want: nil,
		},
		{
			name: "an inline ref is not also read as a bare one",
			f:    Finding{Detail: "fixed by arch-bootc#7"},
			want: []issueRef{{"Danathar", "arch-bootc", 7}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := findingIssueRefs(tc.f, "Danathar", "atomic-image-builder")
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("ref %d = %v, want %v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestStaleFindingSettledUnanimity is the safety property. Findings routinely
// name several items -- the tracking issue, the PR meant to fix it, an
// unrelated one for context -- so "any closed ref retires the finding" would
// retire live work the moment one of them healed.
func TestStaleFindingSettledUnanimity(t *testing.T) {
	closedAt := refBase.Add(-24 * time.Hour)
	refs := []issueRef{
		{"Danathar", "atomic-image-builder", 208},
		{"Danathar", "atomic-image-builder", 209},
	}

	t.Run("every named item closed retires the finding", func(t *testing.T) {
		resolve, _ := closedRefs(map[string]time.Time{
			"Danathar/atomic-image-builder#208": closedAt,
			"Danathar/atomic-image-builder#209": closedAt.Add(time.Minute),
		})
		at, ok := staleFindingSettled(Finding{}, refs, resolve, refBase)
		if !ok {
			t.Fatal("ok = false; both named items were closed before the digest ran")
		}
		if !at.Equal(closedAt.Add(time.Minute)) {
			t.Errorf("closedAt = %v, want the LATEST closure %v", at, closedAt.Add(time.Minute))
		}
	})

	t.Run("one still-open item keeps the finding open", func(t *testing.T) {
		resolve, _ := closedRefs(map[string]time.Time{
			"Danathar/atomic-image-builder#208": closedAt,
		})
		if _, ok := staleFindingSettled(Finding{}, refs, resolve, refBase); ok {
			t.Error("ok = true; #209 is still open, so this finding still names live work")
		}
	})

	t.Run("a lookup that cannot tell keeps the finding open", func(t *testing.T) {
		if _, ok := staleFindingSettled(Finding{}, refs, unknownRefs(), refBase); ok {
			t.Error("ok = true on a failed lookup; a network error is not evidence a finding healed")
		}
	})

	t.Run("no resolver keeps the finding open", func(t *testing.T) {
		if _, ok := staleFindingSettled(Finding{}, refs, nil, refBase); ok {
			t.Error("ok = true with no resolver configured")
		}
	})

	t.Run("a finding naming nothing is never retired", func(t *testing.T) {
		resolve, calls := closedRefs(nil)
		if _, ok := staleFindingSettled(Finding{}, nil, resolve, refBase); ok {
			t.Error("ok = true for a finding that names no GitHub work")
		}
		if *calls != 0 {
			t.Errorf("made %d lookups for a finding naming nothing", *calls)
		}
	})

	t.Run("a closure older than the window is not why this finding healed", func(t *testing.T) {
		resolve, _ := closedRefs(map[string]time.Time{
			"Danathar/atomic-image-builder#208": refBase.Add(-refClosedWindow - time.Hour),
			"Danathar/atomic-image-builder#209": closedAt,
		})
		if _, ok := staleFindingSettled(Finding{}, refs, resolve, refBase); ok {
			t.Error("ok = true on a year-old closure; that is stale context, not a fix")
		}
	})

	t.Run("a closure with no timestamp still counts, dated now", func(t *testing.T) {
		resolve, _ := closedRefs(map[string]time.Time{
			"Danathar/atomic-image-builder#208": {},
			"Danathar/atomic-image-builder#209": {},
		})
		at, ok := staleFindingSettled(Finding{}, refs, resolve, refBase)
		if !ok {
			t.Fatal("ok = false; closed-without-a-timestamp is still closed")
		}
		if !at.Equal(refBase) {
			t.Errorf("closedAt = %v, want now (%v) rather than the zero time", at, refBase)
		}
	})
}

// TestFormatFindingRefStripsGHSourcePrefix covers the dead-link half of #6080.
// Advisory beads carry ExternalRef "gh-<owner>/<repo>#<n>"; the prefix was read
// as part of the owner, so every cross-repo reference in the digest linked to
// github.com/gh-<owner>, an org that does not exist.
func TestFormatFindingRefStripsGHSourcePrefix(t *testing.T) {
	got := formatFindingRef("gh-Danathar/arch-bootc#123", 0, "hivecommons", "hive", "")
	want := " [Danathar/arch-bootc#123](https://github.com/Danathar/arch-bootc/issues/123)"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if strings.Contains(got, "gh-Danathar") {
		t.Error("the gh- source prefix reached the URL; github.com/gh-Danathar is not an org")
	}
}

// The discriminating counterpart: "gh-123" is a DIFFERENT and legitimate form
// -- an issue number with no repo, resolved against the digest's own repo. A
// strip that swallowed it would break every same-repo reference to fix the
// cross-repo ones.
func TestFormatFindingRefKeepsBareGHNumberForm(t *testing.T) {
	got := formatFindingRef("gh-42", 0, "hivecommons", "hive", "")
	want := " [#42](https://github.com/hivecommons/hive/issues/42)"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// stripGHSourcePrefix must leave anything that is not the qualified form alone.
func TestStripGHSourcePrefixScope(t *testing.T) {
	cases := map[string]string{
		"gh-Danathar/arch-bootc#123": "Danathar/arch-bootc#123",
		"gh-42":                      "gh-42",
		"hivecommons/hive#7":         "hivecommons/hive#7",
		"src/pkg/gh-thing.go":        "src/pkg/gh-thing.go",
		"":                           "",
	}
	for in, want := range cases {
		if got := stripGHSourcePrefix(in); got != want {
			t.Errorf("stripGHSourcePrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

// staleFindingStore builds a store holding the #6080 finding: an open advisory
// bead whose detail names the commit it was computed at and the work that
// settled it.
func staleFindingStore(t *testing.T, detail string) *beads.Store {
	t.Helper()
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}
	b, err := store.Create("format_markdown_tables absent from coveragerc",
		beads.TypeAdvisory, severityToPriority("high"), "quality", "")
	if err != nil {
		t.Fatalf("creating bead: %v", err)
	}
	if err := store.Update(b.ID, func(bead *beads.Bead) { bead.Notes = detail }); err != nil {
		t.Fatalf("setting notes: %v", err)
	}
	return store
}

// analyzedAt is the commit the digest footer names. The finding below was
// computed at a different one, which is the whole premise.
const analyzedAt = "2d64e0c1e1668adfe0c5cbd1cdbfa614e517a824"
const computedAt = "d476116953ab"

// staleDetail reproduces finding 2 from #6080: it names the commit its evidence
// came from, and names its own remediation.
var staleDetail = "Coverage computed at " + computedAt + ". Filed issue #208, hold-gated PR #209"

// TestBuildDigestRetiresSettledStaleFinding is the reported bug, end to end: a
// finding computed at an older commit, naming an issue and a PR that both
// closed before the digest ran, must not be rendered as counted open work.
func TestBuildDigestRetiresSettledStaleFinding(t *testing.T) {
	store := staleFindingStore(t, staleDetail)
	closedAt := time.Now().Add(-24 * time.Hour)
	resolve, calls := closedRefs(map[string]time.Time{
		"Danathar/atomic-image-builder#208": closedAt,
		"Danathar/atomic-image-builder#209": closedAt.Add(time.Minute),
	})

	d := BuildDigestFromBeads(map[string]*beads.Store{"quality": store}, "advisory", DigestOptions{
		MaxFindings: 10,
		ResolveRef:  resolve,
		Snapshot:    &Snapshot{Owner: "Danathar", Repo: "atomic-image-builder", SHA: analyzedAt},
	})

	if got := len(digestFindings(d)); got != 0 {
		t.Errorf("%d findings still open; the one finding names work that closed 24h before this digest", got)
	}
	if d.TotalCount != 0 {
		t.Errorf("TotalCount = %d, want 0 -- the header must not count a retired finding", d.TotalCount)
	}
	if len(d.RecentlyResolved) != 1 {
		t.Fatalf("RecentlyResolved has %d entries, want 1 -- a retired finding is moved, not deleted", len(d.RecentlyResolved))
	}
	if !d.RecentlyResolved[0].ClosedAt.Equal(closedAt.Add(time.Minute)) {
		t.Errorf("ClosedAt = %v, want the latest closure %v", d.RecentlyResolved[0].ClosedAt, closedAt.Add(time.Minute))
	}
	if *calls == 0 {
		t.Error("no lookups were made; the finding names two items and both had to be checked")
	}
}

// The discriminating counterpart, and the one that stops this guard becoming a
// way to lose findings: the SAME finding, naming the SAME closed work, computed
// AT the analyzed commit. That is current evidence, and a report that an issue
// was closed too early is exactly the finding a maintainer needs to see.
func TestBuildDigestKeepsFreshFindingNamingClosedWork(t *testing.T) {
	store := staleFindingStore(t, "Coverage computed at "+analyzedAt+". Filed issue #208, hold-gated PR #209")
	resolve, _ := closedRefs(map[string]time.Time{
		"Danathar/atomic-image-builder#208": time.Now().Add(-24 * time.Hour),
		"Danathar/atomic-image-builder#209": time.Now().Add(-24 * time.Hour),
	})

	d := BuildDigestFromBeads(map[string]*beads.Store{"quality": store}, "advisory", DigestOptions{
		MaxFindings: 10,
		ResolveRef:  resolve,
		Snapshot:    &Snapshot{Owner: "Danathar", Repo: "atomic-image-builder", SHA: analyzedAt},
	})

	if got := len(digestFindings(d)); got != 1 {
		t.Errorf("%d findings open, want 1 -- evidence computed AT the analyzed commit is current", got)
	}
	if len(d.RecentlyResolved) != 0 {
		t.Errorf("a finding computed at the analyzed commit was retired; it reports something the closure did not settle")
	}
}

// A stale finding whose named work is still OPEN stays open. Without this the
// suite would pass on an implementation that retired every stale finding.
func TestBuildDigestKeepsStaleFindingWithOpenWork(t *testing.T) {
	store := staleFindingStore(t, staleDetail)
	resolve, _ := closedRefs(map[string]time.Time{
		"Danathar/atomic-image-builder#208": time.Now().Add(-24 * time.Hour),
	})

	d := BuildDigestFromBeads(map[string]*beads.Store{"quality": store}, "advisory", DigestOptions{
		MaxFindings: 10,
		ResolveRef:  resolve,
		Snapshot:    &Snapshot{Owner: "Danathar", Repo: "atomic-image-builder", SHA: analyzedAt},
	})

	if got := len(digestFindings(d)); got != 1 {
		t.Errorf("%d findings open, want 1 -- PR #209 is still open, so this finding still names live work", got)
	}
}

// With no resolver the pipeline behaves exactly as it did before #6080: the
// finding is captioned and demoted, never retired. This is what makes the
// feature safe to ship ahead of the client wiring.
func TestBuildDigestWithoutResolverRetiresNothing(t *testing.T) {
	store := staleFindingStore(t, staleDetail)

	d := BuildDigestFromBeads(map[string]*beads.Store{"quality": store}, "advisory", DigestOptions{
		MaxFindings: 10,
		Snapshot:    &Snapshot{Owner: "Danathar", Repo: "atomic-image-builder", SHA: analyzedAt},
	})

	all := digestFindings(d)
	if len(all) != 1 {
		t.Fatalf("%d findings open, want 1 -- with no resolver nothing may be retired", len(all))
	}
	if !all[0].ProvenanceStale {
		t.Error("ProvenanceStale = false; the #5130 caption must still be applied")
	}
}

// The point of retiring BEFORE the cap. In the reported digest the stale
// findings were not merely mislabelled -- they were counted, severity-ranked
// and holding top-10 slots while other findings went unshown. Retiring after
// the cap would leave the slot spent.
func TestSettledStaleFindingFreesItsTopNSlot(t *testing.T) {
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}
	// The settled one is HIGH; the live one is MEDIUM, so severity alone would
	// always rank the settled finding first and take the single slot.
	settled, err := store.Create("format_markdown_tables absent from coveragerc",
		beads.TypeAdvisory, severityToPriority("high"), "quality", "")
	if err != nil {
		t.Fatalf("creating bead: %v", err)
	}
	if err := store.Update(settled.ID, func(b *beads.Bead) { b.Notes = staleDetail }); err != nil {
		t.Fatalf("setting notes: %v", err)
	}
	if _, err := store.Create("retry budget exhausted on the relay path",
		beads.TypeAdvisory, severityToPriority("medium"), "quality", ""); err != nil {
		t.Fatalf("creating bead: %v", err)
	}

	closedAt := time.Now().Add(-24 * time.Hour)
	resolve, _ := closedRefs(map[string]time.Time{
		"Danathar/atomic-image-builder#208": closedAt,
		"Danathar/atomic-image-builder#209": closedAt,
	})

	d := BuildDigestFromBeads(map[string]*beads.Store{"quality": store}, "advisory", DigestOptions{
		MaxFindings: 1,
		ResolveRef:  resolve,
		Snapshot:    &Snapshot{Owner: "Danathar", Repo: "atomic-image-builder", SHA: analyzedAt},
	})

	all := digestFindings(d)
	if len(all) != 1 {
		t.Fatalf("%d findings rendered under a cap of 1", len(all))
	}
	if all[0].Title != "retry budget exhausted on the relay path" {
		t.Errorf("the single slot went to %q; the settled HIGH must not outrank a live MEDIUM", all[0].Title)
	}
}

// The mixed case, and the one a naive "skip what you cannot read" loop gets
// wrong: one item closed, one unreadable. Skipping the unreadable one would
// retire the finding on the strength of the half that could be checked -- and
// the unreadable half is exactly where a still-open item would hide.
func TestStaleFindingSettledUnknownRefBlocksEvenWhenOthersClosed(t *testing.T) {
	refs := []issueRef{
		{"Danathar", "atomic-image-builder", 208},
		{"Danathar", "atomic-image-builder", 209},
	}
	closedAt := refBase.Add(-24 * time.Hour)
	resolve := func(owner, repo string, number int) (RefState, bool) {
		if number == 208 {
			return RefState{Closed: true, ClosedAt: closedAt}, true
		}
		return RefState{}, false
	}
	if _, ok := staleFindingSettled(Finding{}, refs, resolve, refBase); ok {
		t.Error("ok = true with one item unreadable; a lookup that failed is not evidence it closed")
	}
}
