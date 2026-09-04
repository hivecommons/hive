package advisory

import (
	"fmt"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/beads"
)

// seedResolved creates n advisory beads and closes them, so they land in the
// digest's "Recently Resolved" changelog.
func seedResolved(t *testing.T, store *beads.Store, agent string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		b, err := store.Create(fmt.Sprintf("healed finding %d", i), beads.TypeAdvisory, beads.PriorityHigh, agent, "")
		if err != nil {
			t.Fatalf("creating bead %d: %v", i, err)
		}
		if err := store.Close(b.ID); err != nil {
			t.Fatalf("closing bead %d: %v", i, err)
		}
	}
}

func countLines(md, prefix string) int {
	n := 0
	for _, line := range strings.Split(md, "\n") {
		if strings.HasPrefix(line, prefix) {
			n++
		}
	}
	return n
}

// TestRecentlyResolvedRespectsFindingCap is the #2364 regression test.
//
// max_findings bounded only the OPEN findings, while the changelog of healed
// ones was fixed at 100. On the live digest of 2026-09-03 that inverted the
// comment: 10 open findings in 4,937 characters under a "286 more exist" note,
// and 100 resolved ones in 22,138 — 82% of what a repo owner opened was work
// already done. The changelog must answer to the same dial.
func TestRecentlyResolvedRespectsFindingCap(t *testing.T) {
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}
	seedFindings(t, store, "scanner", "high", 0, 2)
	seedResolved(t, store, "scanner", 30)

	opts := DigestOptions{MaxFindings: 10, Org: "acme", PrimaryRepo: "widgets"}
	d := BuildDigestFromBeads(map[string]*beads.Store{"scanner": store}, "busy", opts)

	if got := len(d.RecentlyResolved); got != 10 {
		t.Errorf("RecentlyResolved = %d entries, want 10 (the max_findings cap)", got)
	}
	if d.ResolvedOverflowCount != 20 {
		t.Errorf("ResolvedOverflowCount = %d, want 20", d.ResolvedOverflowCount)
	}
	// The open findings are untouched by the resolved cap.
	if d.TotalCount != 2 {
		t.Errorf("TotalCount = %d, want 2 — capping the changelog must not drop open findings", d.TotalCount)
	}

	md := FormatDigestMarkdown(d, opts)
	if !strings.Contains(md, "### ✅ Recently Resolved (10)") {
		t.Errorf("resolved section header does not report the rendered count:\n%s", md)
	}
	if n := countLines(md, "- ~~"); n != 10 {
		t.Errorf("rendered %d resolved entries, want 10", n)
	}
	// Withheld entries are announced, not silently absent.
	if !strings.Contains(md, "…plus 20 more resolved in the last 48h") {
		t.Errorf("collapsed changelog remainder is not announced:\n%s", md)
	}
}

// TestRecentlyResolvedNotCappedBelowItsSize guards the common case: a hive with
// fewer resolved findings than the cap renders all of them and says nothing
// about an overflow that does not exist.
func TestRecentlyResolvedNotCappedBelowItsSize(t *testing.T) {
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}
	seedResolved(t, store, "scanner", 3)

	opts := DigestOptions{MaxFindings: 10, Org: "acme", PrimaryRepo: "widgets"}
	d := BuildDigestFromBeads(map[string]*beads.Store{"scanner": store}, "busy", opts)

	if got := len(d.RecentlyResolved); got != 3 {
		t.Errorf("RecentlyResolved = %d entries, want 3", got)
	}
	if d.ResolvedOverflowCount != 0 {
		t.Errorf("ResolvedOverflowCount = %d, want 0", d.ResolvedOverflowCount)
	}
	if md := FormatDigestMarkdown(d, opts); strings.Contains(md, "more resolved in the last 48h") {
		t.Errorf("uncapped changelog claims a remainder it does not have:\n%s", md)
	}
}

// TestRecentlyResolvedShowAllKeepsCeiling pins both halves of the show_all
// contract: it lifts the max_findings cap off the changelog as well, and the
// 100-entry comment-size ceiling still holds above it — now announced instead
// of trimming in silence.
//
// This case also exercises the all-clear rendering path: with no open findings
// the digest takes FormatDigestMarkdown's zero-count branch, which shares
// writeRecentlyResolved with the normal one.
func TestRecentlyResolvedShowAllKeepsCeiling(t *testing.T) {
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}
	seedResolved(t, store, "scanner", maxRecentlyResolved+5)

	opts := DigestOptions{MaxFindings: 10, ShowAll: true, Org: "acme", PrimaryRepo: "widgets"}
	d := BuildDigestFromBeads(map[string]*beads.Store{"scanner": store}, "busy", opts)

	if d.TotalCount != 0 {
		t.Fatalf("TotalCount = %d, want 0 — this case is meant to render the all-clear digest", d.TotalCount)
	}
	if got := len(d.RecentlyResolved); got != maxRecentlyResolved {
		t.Errorf("RecentlyResolved = %d entries, want %d", got, maxRecentlyResolved)
	}
	if d.ResolvedOverflowCount != 5 {
		t.Errorf("ResolvedOverflowCount = %d, want 5", d.ResolvedOverflowCount)
	}
	if md := FormatDigestMarkdown(d, opts); !strings.Contains(md, "…plus 5 more resolved in the last 48h") {
		t.Errorf("ceiling overflow is not announced:\n%s", md)
	}
}

// TestResolvedRenderCap covers the dial directly, including the unset case
// (MaxFindings 0) that config defaulting is supposed to prevent but tests and
// older callers still pass.
func TestResolvedRenderCap(t *testing.T) {
	tests := []struct {
		name string
		opts DigestOptions
		want int
	}{
		{"default cap", DigestOptions{MaxFindings: 10}, 10},
		{"unset falls back to the ceiling", DigestOptions{}, maxRecentlyResolved},
		{"show_all falls back to the ceiling", DigestOptions{MaxFindings: 10, ShowAll: true}, maxRecentlyResolved},
		{"a cap above the ceiling cannot raise it", DigestOptions{MaxFindings: maxRecentlyResolved + 50}, maxRecentlyResolved},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.opts.resolvedRenderCap(); got != tt.want {
				t.Errorf("resolvedRenderCap() = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestCappedChangelogShrinksTheDigest measures the fix the way the bug was
// measured: the same store rendered under the default cap must be a fraction
// of what it was when the changelog ignored that cap.
func TestCappedChangelogShrinksTheDigest(t *testing.T) {
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}
	seedFindings(t, store, "scanner", "critical", 0, 5)
	seedResolved(t, store, "scanner", maxRecentlyResolved)
	stores := map[string]*beads.Store{"scanner": store}

	render := func(o DigestOptions) string {
		return FormatDigestMarkdown(BuildDigestFromBeads(stores, "busy", o), o)
	}
	// MaxFindings at the ceiling reproduces the old resolved-section size (100
	// entries) while capping nothing the open findings care about.
	before := render(DigestOptions{MaxFindings: maxRecentlyResolved, Org: "acme", PrimaryRepo: "widgets"})
	after := render(DigestOptions{MaxFindings: 10, Org: "acme", PrimaryRepo: "widgets"})

	if countLines(before, "- ~~") != maxRecentlyResolved {
		t.Fatalf("baseline rendered %d resolved entries, want %d", countLines(before, "- ~~"), maxRecentlyResolved)
	}
	if len(after) >= len(before)/2 {
		t.Errorf("capped digest is %d chars vs %d uncapped — the changelog still dominates the comment", len(after), len(before))
	}
}
