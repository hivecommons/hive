package advisory

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
)

// TestAgentClosedFindingIsNotReportedResolved is the #6262 regression test.
//
// The guide agent re-checks its own findings each cycle and `bd close`s the
// ones it judges fixed. On the FMA digest of 2026-09-24 it closed "dual-pods
// controller log-tail capture ... is undocumented" although no docs had
// changed, and the digest struck it through as "resolved Sep 24". A close the
// hive did not verify must not be presented as a resolution; one it re-checked
// itself (here, a healed App-auth finding) still is.
func TestAgentClosedFindingIsNotReportedResolved(t *testing.T) {
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}
	const agentTitle = "dual-pods controller log-tail capture on stopped vLLM instances is undocumented in docs/dual-pods.md"
	agentClosed, err := store.Create(agentTitle, beads.TypeAdvisory, beads.PriorityMedium, "guide", "docs/dual-pods.md#launcher-based-pods")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(agentClosed.ID); err != nil { // what `bd close` does
		t.Fatal(err)
	}
	const healedTitle = "GitHub App cannot post the advisory digest"
	healed, err := store.Create(healedTitle, beads.TypeAdvisory, beads.PriorityMedium, "guide", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(healed.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadata(healed.ID, closeReasonMetadataKey, appAuthHealedCloseReason); err != nil {
		t.Fatal(err)
	}

	opts := DigestOptions{Org: "acme", PrimaryRepo: "widgets"}
	d := BuildDigestFromBeads(map[string]*beads.Store{"guide": store}, "busy", opts)
	want := map[string]CloseBasis{agentTitle: CloseBasisUnverified, healedTitle: CloseBasisHiveVerified}
	if len(d.RecentlyResolved) != len(want) {
		t.Fatalf("RecentlyResolved = %+v, want both closes listed", d.RecentlyResolved)
	}
	for _, r := range d.RecentlyResolved {
		if r.Basis != want[r.Title] {
			t.Errorf("%q Basis = %q, want %q", r.Title, r.Basis, want[r.Title])
		}
	}

	md := FormatDigestMarkdown(d, opts)
	if !strings.Contains(md, "### ✅ Recently Resolved (1)") || !strings.Contains(md, "### ☑️ Recently Closed — Fix Not Verified (1)") {
		t.Fatalf("want one verified resolution and one unverified close as separate sections:\n%s", md)
	}
	if strings.Contains(md, "~~"+agentTitle+"~~") {
		t.Errorf("agent-closed finding is struck through as resolved:\n%s", md)
	}
	if !strings.Contains(md, "_guide — closed "+time.Now().Format("Jan 2")+" (no evidence recorded), fix not verified_") {
		t.Errorf("agent-closed finding is not captioned as unverified:\n%s", md)
	}
	if !strings.Contains(md, "~~"+healedTitle+"~~") {
		t.Errorf("hive-verified resolution lost its resolved rendering:\n%s", md)
	}
}

// TestHeuristicClosesAreNotReportedResolved covers the two hive auto-closes
// that rest on inference rather than a re-check. A merged PR whose title merely
// shares words with a finding clears prLinkThreshold, and a finding can cite an
// issue that closes for reasons unrelated to its remedy. Either would strike
// through a finding whose condition still holds, so both render as closed with
// the fix not verified, each captioned with its basis.
func TestHeuristicClosesAreNotReportedResolved(t *testing.T) {
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}
	stores := map[string]*beads.Store{"guide": store}

	// The PR bumps the chart; the finding is about missing docs. Same words.
	const titleMatched = "helm chart metrics exporter values are undocumented"
	if _, err := store.Create(titleMatched, beads.TypeAdvisory, beads.PriorityMedium, "guide", ""); err != nil {
		t.Fatal(err)
	}
	if closed := ClosePRLinkedAdvisoryBeadsAt(stores, "helm chart: bump metrics exporter values", time.Now()); len(closed) != 1 {
		t.Fatalf("PR-linked close = %v, want the title-matched finding closed", closed)
	}

	// #12 is cited for context only; it closing says nothing about the flag.
	const citesUnrelated = "launcher README still documents the removed --pool flag (see #12 for the old design)"
	cited, err := store.Create(citesUnrelated, beads.TypeAdvisory, beads.PriorityMedium, "guide", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(cited.ID); err != nil {
		t.Fatal(err)
	}

	opts := DigestOptions{
		Org:         "acme",
		PrimaryRepo: "widgets",
		ResolveRef: func(owner, repo string, number int) (RefState, bool) {
			if owner != "acme" || repo != "widgets" || number != 12 {
				return RefState{}, false
			}
			return RefState{Closed: true, ClosedAt: time.Now().Add(-time.Hour)}, true
		},
	}
	d := BuildDigestFromBeads(stores, "busy", opts)
	want := map[string]CloseBasis{titleMatched: CloseBasisPRTitleMatch, citesUnrelated: CloseBasisCitedRefsClosed}
	if len(d.RecentlyResolved) != len(want) {
		t.Fatalf("RecentlyResolved = %+v, want both closes listed", d.RecentlyResolved)
	}
	for _, r := range d.RecentlyResolved {
		if r.Basis != want[r.Title] {
			t.Errorf("%q Basis = %q, want %q", r.Title, r.Basis, want[r.Title])
		}
	}

	md := FormatDigestMarkdown(d, opts)
	if strings.Contains(md, "~~") || strings.Contains(md, "Recently Resolved") {
		t.Errorf("heuristic closes are presented as resolved:\n%s", md)
	}
	if !strings.Contains(md, "(a merged PR's title matched), fix not verified_") {
		t.Errorf("PR title match is not captioned with its basis:\n%s", md)
	}
	if !strings.Contains(md, "(the issues/PRs it cites closed), fix not verified_") {
		t.Errorf("cited-refs close is not captioned with its basis:\n%s", md)
	}
}

// TestAllClearDigestDoesNotOverclaimAgentCloses guards the zero-open-findings
// rendering: with only unverified closes behind it, "all previously reported
// findings are resolved" is the same false claim in a different place.
func TestAllClearDigestDoesNotOverclaimAgentCloses(t *testing.T) {
	d := &Digest{
		GeneratedAt:      time.Now(),
		RecentlyResolved: []ResolvedFinding{{Agent: "guide", Title: "gap", ClosedAt: time.Now(), Basis: CloseBasisPRTitleMatch}},
	}
	md := FormatDigestMarkdown(d, DigestOptions{})
	if strings.Contains(md, "all previously reported findings are resolved") {
		t.Errorf("all-clear digest claims resolution on unverified closes alone:\n%s", md)
	}
	if !strings.Contains(md, "1 recently closed without a verified fix") {
		t.Errorf("all-clear digest does not report the unverified close:\n%s", md)
	}
}

// TestCappedUnverifiedClosesAreNotSummarizedAsResolved: the changelog cap
// keeps the newest entries, so older bare closes can all fall past it. The
// hidden remainder must not then be announced as "resolved", and the
// zero-findings summary must not claim everything was resolved.
func TestCappedUnverifiedClosesAreNotSummarizedAsResolved(t *testing.T) {
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}
	for i := range 3 {
		b, err := store.Create("bare close "+string(rune('a'+i)), beads.TypeAdvisory, beads.PriorityHigh, "guide", "")
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Close(b.ID); err != nil {
			t.Fatal(err)
		}
		// Older than every verified close below, so the cap drops these.
		if err := store.SetMetadata(b.ID, resolvedAtMetadataKey, formatResolvedAt(time.Now().Add(-time.Hour))); err != nil {
			t.Fatal(err)
		}
	}
	seedResolved(t, store, "guide", 2)

	opts := DigestOptions{MaxFindings: 2}
	d := BuildDigestFromBeads(map[string]*beads.Store{"guide": store}, "busy", opts)
	if d.ResolvedOverflowCount != 3 || d.UnverifiedOverflowCount != 3 {
		t.Fatalf("overflow = %d (unverified %d), want 3 (3)", d.ResolvedOverflowCount, d.UnverifiedOverflowCount)
	}
	md := FormatDigestMarkdown(d, opts)
	if strings.Contains(md, "all previously reported findings are resolved") {
		t.Errorf("summary claims all resolved while bare closes sit past the cap:\n%s", md)
	}
	if !strings.Contains(md, "…plus 3 more closed without a verified fix") {
		t.Errorf("hidden bare closes are not labelled unverified:\n%s", md)
	}
}

// TestGuideFindingNotPRLinkedByFMATitles pins that the #6262 guide finding was
// NOT retired by title-similarity PR auto-close: none of the FMA PRs merged
// around the false "resolved Sep 24" clears prLinkThreshold against it, so the
// close came from the agent and belongs in the unverified section.
func TestGuideFindingNotPRLinkedByFMATitles(t *testing.T) {
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}
	const title = "dual-pods controller log-tail capture on stopped vLLM instances is undocumented in docs/dual-pods.md and docs/launcher.md"
	if _, err := store.Create(title, beads.TypeAdvisory, beads.PriorityMedium, "guide", "docs/dual-pods.md#launcher-based-pods"); err != nil {
		t.Fatal(err)
	}
	stores := map[string]*beads.Store{"guide": store}
	for _, pr := range []string{
		"Tweak dependency bump review instructions",
		"deps(actions): bump docker/setup-buildx-action from 4.3.0 to 4.4.1",
		"deps(actions): bump docker/setup-qemu-action from 4.3.0 to 4.4.0",
		"deps(actions): bump docker/build-push-action from 7.3.0 to 7.4.0",
		"Correct testing of releases wrt --debug-gpu-memory",
	} {
		if closed := ClosePRLinkedAdvisoryBeads(stores, pr); len(closed) != 0 {
			t.Errorf("PR %q closed %v; the guide finding shares no fix with it", pr, closed)
		}
	}
}
