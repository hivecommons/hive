package advisory

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
)

// severityPriority maps a digest severity back to the bead priority that
// produces it, so these tests can seed stores in the terms the digest reads.
func severityPriority(sev string) beads.Priority {
	return severityToPriority(sev)
}

// seedFindings creates n open advisory beads of one severity in store, drawing
// titles from subjects[start:] so that repeated calls never reuse a subject —
// two titles sharing their subject words would be collapsed as near-duplicates
// before the cap under test ever saw them.
func seedFindings(t *testing.T, store *beads.Store, agent, sev string, start, n int) {
	t.Helper()
	// Distinct subject nouns, not numbered variants: findingTokens drops digits,
	// so "finding 1"/"finding 2" would collapse into a single entry and the cap
	// under test would never be exercised.
	subjects := []string{
		"database connection pooling", "template escaping", "retry backoff",
		"cache invalidation", "signal handling", "manifest validation",
		"token refresh", "queue draining", "log rotation", "socket teardown",
		"header parsing", "timezone conversion", "checksum verification",
		"path traversal guard", "worker shutdown", "index rebuilding",
		"metric cardinality", "cursor pagination", "lease renewal", "clock skew",
	}
	if start+n > len(subjects) {
		t.Fatalf("seedFindings: asked for subjects [%d,%d), only %d available", start, start+n, len(subjects))
	}
	for i := start; i < start+n; i++ {
		title := fmt.Sprintf("%s is broken in %s", subjects[i], agent)
		if _, err := store.Create(title, beads.TypeAdvisory, severityPriority(sev), agent, ""); err != nil {
			t.Fatalf("creating bead: %v", err)
		}
	}
}

func digestFindings(d *Digest) []Finding {
	var all []Finding
	for _, fs := range d.ByAgent {
		all = append(all, fs...)
	}
	return all
}

// TestBuildDigestTopN is the cap's contract: only the most severe findings
// survive, the digest says how many were withheld, and its own TotalCount
// describes what is actually rendered.
func TestBuildDigestTopN(t *testing.T) {
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}
	seedFindings(t, store, "scanner", "medium", 0, 6)
	seedFindings(t, store, "reviewer", "critical", 6, 2)
	seedFindings(t, store, "ci-maintainer", "high", 8, 3)

	// One store, keyed once: the seeded beads carry their own agent names, and
	// mapping several keys to the same store would read the same beads twice.
	d := BuildDigestFromBeads(map[string]*beads.Store{"scanner": store}, "busy", DigestOptions{MaxFindings: 5})

	if !d.Capped {
		t.Fatal("Capped = false, want true — 11 findings were capped to 5")
	}
	if got, want := d.TotalCount, 5; got != want {
		t.Errorf("TotalCount = %d, want %d (the cap describes what is RENDERED)", got, want)
	}
	if got, want := d.OverflowCount, 6; got != want {
		t.Errorf("OverflowCount = %d, want %d", got, want)
	}
	kept := digestFindings(d)
	if len(kept) != 5 {
		t.Fatalf("rendered %d findings, want 5", len(kept))
	}

	// Ordering: every critical must be present, then the highs; no medium may
	// displace a more severe finding.
	counts := map[string]int{}
	for _, f := range kept {
		counts[f.Severity]++
	}
	if counts["critical"] != 2 {
		t.Errorf("kept %d critical findings, want all 2", counts["critical"])
	}
	if counts["high"] != 3 {
		t.Errorf("kept %d high findings, want all 3", counts["high"])
	}
	if counts["medium"] != 0 {
		t.Errorf("kept %d medium findings, want 0 — they must not displace critical/high", counts["medium"])
	}
}

// TestBuildDigestTopNRecencyWithinSeverity pins the second sort key: among
// equally severe findings the most recently reported one is the one still
// happening, so it is the one that survives the cap.
func TestBuildDigestTopNRecencyWithinSeverity(t *testing.T) {
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}
	if _, err := store.Create("older high finding about lease renewal", beads.TypeAdvisory, beads.PriorityHigh, "scanner", ""); err != nil {
		t.Fatalf("creating older bead: %v", err)
	}
	// Created second, so its CreatedAt — the timestamp the digest ranks on — is
	// strictly later.
	time.Sleep(2 * time.Millisecond)
	const newerTitle = "newer high finding about clock skew"
	if _, err := store.Create(newerTitle, beads.TypeAdvisory, beads.PriorityHigh, "scanner", ""); err != nil {
		t.Fatalf("creating newer bead: %v", err)
	}

	d := BuildDigestFromBeads(map[string]*beads.Store{"scanner": store}, "busy", DigestOptions{MaxFindings: 1})
	kept := digestFindings(d)
	if len(kept) != 1 {
		t.Fatalf("rendered %d findings, want 1", len(kept))
	}
	if !d.Capped || d.OverflowCount != 1 {
		t.Errorf("Capped=%v OverflowCount=%d, want true/1", d.Capped, d.OverflowCount)
	}
	if kept[0].Title != newerTitle {
		t.Errorf("kept %q, want the more recent %q", kept[0].Title, newerTitle)
	}
}

// TestBuildDigestShowAll verifies the owner opt-in: ShowAll defeats the cap
// completely, whatever MaxFindings says.
func TestBuildDigestShowAll(t *testing.T) {
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}
	seedFindings(t, store, "scanner", "medium", 0, 12)

	d := BuildDigestFromBeads(map[string]*beads.Store{"scanner": store}, "busy",
		DigestOptions{MaxFindings: 3, ShowAll: true})

	if d.Capped {
		t.Error("Capped = true, want false — ShowAll must bypass the cap")
	}
	if d.OverflowCount != 0 {
		t.Errorf("OverflowCount = %d, want 0", d.OverflowCount)
	}
	if got := len(digestFindings(d)); got != 12 {
		t.Errorf("rendered %d findings, want all 12", got)
	}
	if d.TotalCount != 12 {
		t.Errorf("TotalCount = %d, want 12", d.TotalCount)
	}
}

// TestBuildDigestNoCapWhenUnderLimit confirms a small digest is untouched: no
// note, no overflow, identical to the pre-cap behavior.
func TestBuildDigestNoCapWhenUnderLimit(t *testing.T) {
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}
	seedFindings(t, store, "scanner", "high", 0, 3)

	d := BuildDigestFromBeads(map[string]*beads.Store{"scanner": store}, "busy", DigestOptions{MaxFindings: 10})
	if d.Capped || d.OverflowCount != 0 {
		t.Errorf("Capped=%v OverflowCount=%d, want false/0", d.Capped, d.OverflowCount)
	}
	if d.TotalCount != 3 {
		t.Errorf("TotalCount = %d, want 3", d.TotalCount)
	}
}

// TestBuildDigestZeroMaxFindingsIsUnlimited pins the documented meaning of 0.
func TestBuildDigestZeroMaxFindingsIsUnlimited(t *testing.T) {
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}
	seedFindings(t, store, "scanner", "low", 0, 12)

	d := BuildDigestFromBeads(map[string]*beads.Store{"scanner": store}, "busy", DigestOptions{MaxFindings: 0})
	if d.Capped {
		t.Error("Capped = true, want false — MaxFindings 0 means unlimited")
	}
	if got := len(digestFindings(d)); got != 12 {
		t.Errorf("rendered %d findings, want all 12", got)
	}
}

// TestFormatDigestMarkdownCapNote verifies the reader is TOLD the list is
// partial, and told how to see the rest. A silently shortened digest would be
// worse than a long one.
func TestFormatDigestMarkdownCapNote(t *testing.T) {
	d := &Digest{
		GeneratedAt: time.Now(),
		Mode:        "busy",
		ByAgent: map[string][]Finding{
			"scanner": {{Agent: "scanner", Severity: "high", Type: "security", Title: "a finding"}},
		},
		TotalCount:    1,
		Capped:        true,
		OverflowCount: 24,
	}
	md := FormatDigestMarkdown(d, DigestOptions{Org: "acme", PrimaryRepo: "widgets"})
	if !strings.Contains(md, "Showing top 1 findings (by severity)") {
		t.Errorf("digest is missing the cap note:\n%s", md)
	}
	if !strings.Contains(md, "24 more exist") {
		t.Errorf("digest does not report the withheld count:\n%s", md)
	}
	if !strings.Contains(md, "governor.advisory.show_all: true") {
		t.Errorf("digest does not name the setting that lifts the cap:\n%s", md)
	}
	// The note must sit above the findings, where it is read before the list it
	// qualifies.
	if strings.Index(md, "Showing top") > strings.Index(md, "a finding") {
		t.Error("cap note is rendered below the findings it qualifies")
	}
}

// TestFormatDigestMarkdownNoCapNoteWhenUncapped keeps an ordinary digest free of
// the note.
func TestFormatDigestMarkdownNoCapNoteWhenUncapped(t *testing.T) {
	d := &Digest{
		GeneratedAt: time.Now(),
		Mode:        "busy",
		ByAgent: map[string][]Finding{
			"scanner": {{Agent: "scanner", Severity: "high", Type: "security", Title: "a finding"}},
		},
		TotalCount: 1,
	}
	md := FormatDigestMarkdown(d, DigestOptions{Org: "acme", PrimaryRepo: "widgets"})
	if strings.Contains(md, "Showing top") {
		t.Errorf("uncapped digest carries a cap note:\n%s", md)
	}
}
