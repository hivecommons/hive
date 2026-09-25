package github

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests are the #3980 regression suite.
//
// A contributor's open PR kept failing to suppress its own issue because it
// referenced the issue WITHOUT a closing keyword — "Refs #3498 — no `Fixes`
// keyword, deliberately", the correct convention when a PR only partially
// addresses an issue. The claim ledger recorded nothing, so the contribute
// queue re-offered the issue every cooldown window, forever. Doing the right
// thing was what made the work invisible.

func TestParseReferencedIssues(t *testing.T) {
	const defaultRepo = "hivecommons/hive"

	tests := []struct {
		name string
		text string
		want []ClaimedRef
	}{
		// Every supported reference idiom.
		{"refs", "Refs #1", []ClaimedRef{{defaultRepo, 1}}},
		{"ref", "ref #2", []ClaimedRef{{defaultRepo, 2}}},
		{"references", "References #3", []ClaimedRef{{defaultRepo, 3}}},
		{"referencing", "referencing #4", []ClaimedRef{{defaultRepo, 4}}},
		{"addresses", "Addresses #5", []ClaimedRef{{defaultRepo, 5}}},
		{"address", "address #6", []ClaimedRef{{defaultRepo, 6}}},
		{"addressing", "Addressing #7", []ClaimedRef{{defaultRepo, 7}}},
		{"addressed", "addressed #8", []ClaimedRef{{defaultRepo, 8}}},
		{"part of", "Part of #9", []ClaimedRef{{defaultRepo, 9}}},
		{"towards", "Towards #10", []ClaimedRef{{defaultRepo, 10}}},
		{"toward", "toward #11", []ClaimedRef{{defaultRepo, 11}}},
		{"contributes to", "Contributes to #12", []ClaimedRef{{defaultRepo, 12}}},

		{"case-insensitive", "REFS #423", []ClaimedRef{{defaultRepo, 423}}},
		{"colon separator", "Refs: #13", []ClaimedRef{{defaultRepo, 13}}},
		{"cross-repo form", "Refs owner/other#55", []ClaimedRef{{"owner/other", 55}}},

		// The two real PR bodies from the #3980 report. These are the whole
		// point of the change; if either regresses, the reported loop returns.
		{
			name: "real PR #3898 body (issue 3498)",
			text: "Refs #3498 — no `Fixes` keyword, deliberately. The issue left the scaling curve open",
			want: []ClaimedRef{{defaultRepo, 3498}},
		},
		{
			name: "real PR #3979 body (issue 2364), keyword six words from the ref",
			text: "Addresses a `ci-maintainer` finding from the #2364 advisory digest:",
			want: []ClaimedRef{{defaultRepo, 2364}},
		},

		// Deliberately NOT claims: a topical cross-reference is not a statement
		// that this PR does work on that issue.
		{"see is excluded", "see #12 for background", nil},
		{"related to is excluded", "related to #12", nil},
		{"relates to is excluded", "relates to #12", nil},
		{"bare mention is excluded", "unlike #12, this uses a map", nil},
		{"bare number is excluded", "#12", nil},
		{"empty text", "", nil},

		// Gap bounds. The gap exists for prose like PR #3979, but must not let
		// a keyword reach across a sentence or past a nearer reference.
		{
			name: "gap cannot cross a sentence boundary",
			text: "Refs the earlier discussion. #123",
			want: nil,
		},
		{
			name: "gap stops at a nearer reference",
			text: "Addresses #100 which supersedes #200",
			want: []ClaimedRef{{defaultRepo, 100}},
		},
		{
			name: "gap cannot cross a newline",
			text: "Refs the thing\n#123",
			want: nil,
		},
		// The gap is measured from the end of the keyword, so it includes the
		// separating space: "Refs " + 39 filler = exactly 40.
		{
			name: "gap at the 40-char bound still matches",
			text: "Refs " + strings.Repeat("x", 39) + "#123",
			want: []ClaimedRef{{defaultRepo, 123}},
		},
		{
			name: "gap one past the 40-char bound does not match",
			text: "Refs " + strings.Repeat("x", 40) + "#123",
			want: nil,
		},

		{
			name: "de-duplicates repeated references to one issue",
			text: "Refs #1 and is part of #1",
			want: []ClaimedRef{{defaultRepo, 1}},
		},
		{
			name: "multiple distinct issues, first-seen order",
			text: "Refs #7. Part of #3.",
			want: []ClaimedRef{{defaultRepo, 7}, {defaultRepo, 3}},
		},

		// Lists (#7911). One keyword can introduce several references on a
		// line; before this every element after the first was invisible.
		{
			name: "comma list",
			text: "Refs #72, #74, #87",
			want: []ClaimedRef{{defaultRepo, 72}, {defaultRepo, 74}, {defaultRepo, 87}},
		},
		{
			name: "list joined with and",
			text: "Part of #5 and #6",
			want: []ClaimedRef{{defaultRepo, 5}, {defaultRepo, 6}},
		},
		{
			name: "list with an oxford comma",
			text: "Addresses #1, #2, and #3.",
			want: []ClaimedRef{{defaultRepo, 1}, {defaultRepo, 2}, {defaultRepo, 3}},
		},
		{
			name: "cross-repo element inside a list",
			text: "Refs #1, owner/other#2",
			want: []ClaimedRef{{defaultRepo, 1}, {"owner/other", 2}},
		},
		{
			name: "a list ends at the line",
			text: "Refs #1,\n#2 is unrelated",
			want: []ClaimedRef{{defaultRepo, 1}},
		},
		{
			name: "a list ends at prose",
			text: "Refs #1, #2 which supersedes #3",
			want: []ClaimedRef{{defaultRepo, 1}, {defaultRepo, 2}},
		},
		{
			// The real projectbluefin/server#210 body that motivated #7911:
			// the closing ref is the closing parser's; every listed
			// reference must come out of this one.
			name: "real PR #210 body: closing sentence then a reference list",
			text: "Closes #159. Refs #72, #74, #87, #193, #196, #197, #198, #199.",
			want: []ClaimedRef{
				{defaultRepo, 72}, {defaultRepo, 74}, {defaultRepo, 87}, {defaultRepo, 193},
				{defaultRepo, 196}, {defaultRepo, 197}, {defaultRepo, 198}, {defaultRepo, 199},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseReferencedIssues(tt.text, defaultRepo)
			if len(got) != len(tt.want) {
				t.Fatalf("ParseReferencedIssues(%q) = %+v, want %+v", tt.text, got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("ref[%d] = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// TestParseClaimedIssuesStillIgnoresNonClosingRefs pins the deliberate
// separation between the two parsers. The closing parser must NOT start
// matching references — its results suppress agent work, and a PR that says
// "Refs #N" has explicitly declined to claim it closes the issue.
func TestParseClaimedIssuesStillIgnoresNonClosingRefs(t *testing.T) {
	for _, text := range []string{
		"Refs #1", "Part of #2", "Addresses #3", "Towards #4", "see #5",
	} {
		if got := ParseClaimedIssues(text, "r"); got != nil {
			t.Errorf("ParseClaimedIssues(%q) = %+v, want nil (closing keywords only)", text, got)
		}
	}
	// Positive control: the closing parser still works.
	if got := ParseClaimedIssues("Fixes #6", "r"); len(got) != 1 || got[0].Issue != 6 {
		t.Fatalf("closing parser regressed: %+v", got)
	}
}

// TestFetchClaimsReferenceTier pins the three-tier precedence. The reference
// tier is LAST on purpose: it only fills in PRs that previously produced no
// claim at all, so no claim that existed before this change moves target or
// changes strength.
func TestFetchClaimsReferenceTier(t *testing.T) {
	prWithBranch := func(number int, title, body, branch string) map[string]any {
		p := pr(number, "clubanderson", title, body)
		p["head"] = map[string]any{"ref": branch}
		return p
	}

	type want struct {
		issue     int
		reference bool
	}
	tests := []struct {
		name string
		prs  []map[string]any
		want []want // in claim order; nil means no claim at all
	}{
		{
			name: "reference recovers a PR that claimed nothing before",
			prs:  []map[string]any{prWithBranch(3898, "autoscale", "Refs #3498 — no `Fixes` keyword, deliberately.", "feat/governor-autoscale-thresholds")},
			want: []want{{3498, true}},
		},
		{
			// #7911: the reference no longer disappears behind the closing
			// keyword. The closed issue keeps its strong claim, listed
			// first; the referenced one gains a weak claim it never had.
			name: "closing keyword still wins for its issue; the reference now claims its own",
			prs:  []map[string]any{prWithBranch(1, "t", "Fixes #100. Also refs #200.", "scratch")},
			want: []want{{100, false}, {200, true}},
		},
		{
			name: "branch heuristic still wins for its issue; the reference now claims its own",
			prs:  []map[string]any{prWithBranch(2, "t", "Refs #200", "issue-443")},
			want: []want{{443, false}, {200, true}},
		},
		{
			// An issue both closed and referenced is one claim, the strong one.
			name: "an issue both closed and referenced keeps the strong claim only",
			prs:  []map[string]any{prWithBranch(4, "t", "Fixes #100. Part of #100 and #101.", "scratch")},
			want: []want{{100, false}, {101, true}},
		},
		{
			// The real projectbluefin/server#210 body (#7911): one closed
			// issue and a list of eight references, all of which the
			// contribute queue must see.
			name: "real PR #210: closes one issue and references a list",
			prs:  []map[string]any{prWithBranch(210, "feat(kubernetes): replace the k0s sysext", "Closes #159. Refs #72, #74, #87, #193, #196, #197, #198, #199.", "fix/boot-zfs-udev-and-depmod")},
			want: []want{{159, false}, {72, true}, {74, true}, {87, true}, {193, true}, {196, true}, {197, true}, {198, true}, {199, true}},
		},
		{
			name: "a passing mention still claims nothing",
			prs:  []map[string]any{prWithBranch(3, "t", "unlike #999, this uses a map", "scratch")},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := prClaimServer(t, tt.prs, http.StatusOK)
			c := NewClientForTest(srv.URL, "hivecommons", []string{"hive"}, testLogger())
			claims, err := c.FetchClaims(context.Background(), HiveIdentity{AIAuthor: "clubanderson"})
			if err != nil {
				t.Fatal(err)
			}
			if len(claims) != len(tt.want) {
				t.Fatalf("got %d claims %+v, want %d %+v", len(claims), claims, len(tt.want), tt.want)
			}
			for i, w := range tt.want {
				if claims[i].Issue != w.issue {
					t.Errorf("claim[%d].Issue = %d, want %d", i, claims[i].Issue, w.issue)
				}
				if claims[i].Reference != w.reference {
					t.Errorf("claim[%d] (#%d).Reference = %v, want %v", i, claims[i].Issue, claims[i].Reference, w.reference)
				}
			}
		})
	}
}

// TestFilterClaimedIssuesDefersReferenceClaims: a reference claim DEFERS agent
// work rather than either freezing it or vanishing.
//
// This test previously asserted that a reference claim never suppresses agent
// work at all, on the reasoning that a PR which declined to say it closes the
// issue must not strand the remainder behind it. #4929 showed the hole in that:
// the scanner cannot run `gh pr list` under its hold-gated policy, so an issue
// handed back under a reference claim is not re-examined against the open PR,
// it is re-implemented. The invariant that reasoning protected — agents are
// never frozen behind a weak claim — is now asserted as the RELEASE half below,
// which is a stronger statement than the old "never suppresses": it pins the
// bound instead of merely observing an absence.
func TestFilterClaimedIssuesDefersReferenceClaims(t *testing.T) {
	build := func() (*ActionableResult, *ClaimLedger) {
		result := &ActionableResult{}
		result.Issues.Items = []Issue{
			{Repo: "hivecommons/hive", Number: 3498, Title: "referenced, not closed"},
			{Repo: "hivecommons/hive", Number: 3499, Title: "genuinely claimed"},
		}
		result.Issues.Count = 2

		l := NewClaimLedger(filepath.Join(t.TempDir(), "l.json"), testLogger())
		l.Reconcile([]IssueClaim{
			{Repo: "hivecommons/hive", Issue: 3498, PRNumber: 3898, PRRepo: "hivecommons/hive",
				PRAuthor: "clubanderson", ObservedAt: time.Now(), Reference: true},
			{Repo: "hivecommons/hive", Issue: 3499, PRNumber: 3899, PRRepo: "hivecommons/hive",
				PRAuthor: "clubanderson", ObservedAt: time.Now()},
		}, true)
		return result, l
	}

	// Inside the window: only the strong closing claim suppresses. A reference-only
	// claim is surfaced as pending context/label and remains actionable (#8876).
	result, l := build()
	if suppressed := FilterClaimedIssues(result, l, nil, testLogger()); suppressed != 1 {
		t.Fatalf("inside the window: suppressed = %d, want 1 (closing only)", suppressed)
	}
	if len(result.Issues.Items) != 1 || result.Issues.Items[0].Number != 3498 {
		t.Fatalf("inside the window only the reference issue should remain, got %+v", result.Issues.Items)
	}

	// Past the window: the reference claim releases its issue even though the
	// PR is still open, while the closing claim keeps suppressing. Nothing is
	// frozen; the remainder of a partially-addressed issue comes back for work.
	result, l = build()
	l.SetClock(func() time.Time { return time.Now().Add(weakClaimDeferWindow + time.Hour) })
	if suppressed := FilterClaimedIssues(result, l, nil, testLogger()); suppressed != 1 {
		t.Fatalf("past the window: suppressed = %d, want 1 (the closing claim only)", suppressed)
	}
	if len(result.Issues.Items) != 1 || result.Issues.Items[0].Number != 3498 {
		t.Fatalf("past the window the reference-claimed issue must return, got %+v", result.Issues.Items)
	}
}

// TestClaimLedgerReferencePrecedence: a weak reference claim must never
// displace a strong closing claim for the same issue, in either arrival order
// or across a non-authoritative refresh — the #3768 precedence rule, extended.
func TestClaimLedgerReferencePrecedence(t *testing.T) {
	strong := IssueClaim{
		Repo: "r", Issue: 1, PRNumber: 10, PRRepo: "r",
		PRURL: "strong-pr", PRAuthor: "clubanderson", ObservedAt: time.Now(),
	}
	weak := IssueClaim{
		Repo: "r", Issue: 1, PRNumber: 20, PRRepo: "r",
		PRURL: "weak-pr", PRAuthor: "clubanderson", ObservedAt: time.Now(),
		Reference: true,
	}

	t.Run("strong first", func(t *testing.T) {
		l := NewClaimLedger(filepath.Join(t.TempDir(), "l.json"), testLogger())
		l.Reconcile([]IssueClaim{strong, weak}, true)
		got, ok := l.Lookup("r", 1)
		if !ok || got.Reference || got.PRNumber != 10 {
			t.Fatalf("closing claim must win, got %+v (ok=%v)", got, ok)
		}
	})
	t.Run("weak first", func(t *testing.T) {
		l := NewClaimLedger(filepath.Join(t.TempDir(), "l.json"), testLogger())
		l.Reconcile([]IssueClaim{weak, strong}, true)
		got, ok := l.Lookup("r", 1)
		if !ok || got.Reference || got.PRNumber != 10 {
			t.Fatalf("closing claim must win regardless of order, got %+v (ok=%v)", got, ok)
		}
	})
	t.Run("partial refresh cannot demote a closing claim", func(t *testing.T) {
		l := NewClaimLedger(filepath.Join(t.TempDir(), "l.json"), testLogger())
		l.Reconcile([]IssueClaim{strong}, true)
		l.Reconcile([]IssueClaim{weak}, false)
		got, ok := l.Lookup("r", 1)
		if !ok || got.Reference || got.PRNumber != 10 {
			t.Fatalf("partial fetch must not demote a closing claim, got %+v (ok=%v)", got, ok)
		}
	})
	t.Run("reference-only claim is still recorded for the contribute queue", func(t *testing.T) {
		l := NewClaimLedger(filepath.Join(t.TempDir(), "l.json"), testLogger())
		l.Reconcile([]IssueClaim{weak}, true)
		got, ok := l.Lookup("r", 1)
		if !ok || !got.Reference || got.PRNumber != 20 {
			t.Fatalf("reference claim must be present, got %+v (ok=%v)", got, ok)
		}
	})
	t.Run("external precedence still holds within the closing tier", func(t *testing.T) {
		ext := strong
		ext.PRNumber = 30
		ext.ExternalAuthor = true
		l := NewClaimLedger(filepath.Join(t.TempDir(), "l.json"), testLogger())
		l.Reconcile([]IssueClaim{ext, strong}, true)
		got, ok := l.Lookup("r", 1)
		if !ok || got.ExternalAuthor || got.PRNumber != 10 {
			t.Fatalf("#3768 hive precedence regressed, got %+v (ok=%v)", got, ok)
		}
	})
}

// TestClaimLedgerBackCompatPreThreeNineEightZero: a ledger written before
// #3980 has no `reference` field. Every entry it holds was a closing-keyword
// (or branch-derived) claim, so it must load as a STRONG claim and keep
// suppressing agent work across the upgrade.
func TestClaimLedgerBackCompatPreThreeNineEightZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pr-claims.json")
	oldFormat := fmt.Sprintf(`{
  "saved_at": %[1]q,
  "claims": [
    {
      "repo": "hivecommons/hive",
      "issue": 42,
      "pr_number": 100,
      "pr_repo": "hivecommons/hive",
      "pr_url": "https://github.com/hivecommons/hive/pull/100",
      "pr_author": "clubanderson",
      "observed_at": %[1]q
    }
  ]
}`, time.Now().Format(time.RFC3339))
	if err := os.WriteFile(path, []byte(oldFormat), 0o644); err != nil {
		t.Fatal(err)
	}

	l, err := LoadClaimLedger(path, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	got, ok := l.Lookup("hivecommons/hive", 42)
	if !ok {
		t.Fatal("pre-#3980 claim did not load")
	}
	if got.Reference {
		t.Error("pre-#3980 claim must load as a strong (non-reference) claim")
	}

	// Positive control: it must still suppress agent work.
	result := &ActionableResult{}
	result.Issues.Items = []Issue{{Repo: "hivecommons/hive", Number: 42}}
	result.Issues.Count = 1
	if suppressed := FilterClaimedIssues(result, l, nil, testLogger()); suppressed != 1 {
		t.Fatalf("pre-#3980 claim stopped suppressing agent work: suppressed=%d", suppressed)
	}
}
