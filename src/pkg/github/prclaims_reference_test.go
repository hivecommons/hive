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
	const defaultRepo = "kubestellar/hive"

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

	tests := []struct {
		name          string
		prs           []map[string]any
		wantIssue     int
		wantReference bool
		wantNoClaim   bool
	}{
		{
			name:          "reference recovers a PR that claimed nothing before",
			prs:           []map[string]any{prWithBranch(3898, "autoscale", "Refs #3498 — no `Fixes` keyword, deliberately.", "feat/governor-autoscale-thresholds")},
			wantIssue:     3498,
			wantReference: true,
		},
		{
			name:          "closing keyword still wins over a reference",
			prs:           []map[string]any{prWithBranch(1, "t", "Fixes #100. Also refs #200.", "scratch")},
			wantIssue:     100,
			wantReference: false,
		},
		{
			name:          "branch heuristic still wins over a reference",
			prs:           []map[string]any{prWithBranch(2, "t", "Refs #200", "issue-443")},
			wantIssue:     443,
			wantReference: false,
		},
		{
			name:        "a passing mention still claims nothing",
			prs:         []map[string]any{prWithBranch(3, "t", "unlike #999, this uses a map", "scratch")},
			wantNoClaim: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := prClaimServer(t, tt.prs, http.StatusOK)
			c := NewClientForTest(srv.URL, "kubestellar", []string{"hive"}, testLogger())
			claims, err := c.FetchClaims(context.Background(), HiveIdentity{AIAuthor: "clubanderson"})
			if err != nil {
				t.Fatal(err)
			}
			if tt.wantNoClaim {
				if len(claims) != 0 {
					t.Fatalf("expected no claims, got %+v", claims)
				}
				return
			}
			if len(claims) != 1 {
				t.Fatalf("expected exactly one claim, got %+v", claims)
			}
			if claims[0].Issue != tt.wantIssue {
				t.Errorf("Issue = %d, want %d", claims[0].Issue, tt.wantIssue)
			}
			if claims[0].Reference != tt.wantReference {
				t.Errorf("Reference = %v, want %v", claims[0].Reference, tt.wantReference)
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
			{Repo: "kubestellar/hive", Number: 3498, Title: "referenced, not closed"},
			{Repo: "kubestellar/hive", Number: 3499, Title: "genuinely claimed"},
		}
		result.Issues.Count = 2

		l := NewClaimLedger(filepath.Join(t.TempDir(), "l.json"), testLogger())
		l.Reconcile([]IssueClaim{
			{Repo: "kubestellar/hive", Issue: 3498, PRNumber: 3898, PRRepo: "kubestellar/hive",
				PRAuthor: "clubanderson", ObservedAt: time.Now(), Reference: true},
			{Repo: "kubestellar/hive", Issue: 3499, PRNumber: 3899, PRRepo: "kubestellar/hive",
				PRAuthor: "clubanderson", ObservedAt: time.Now()},
		}, true)
		return result, l
	}

	// Inside the window: both the closing claim and the reference claim
	// suppress. This is the #4929 fix — the reference-claimed issue is not
	// handed back to a scanner that cannot see the PR covering it.
	result, l := build()
	if suppressed := FilterClaimedIssues(result, l, nil, testLogger()); suppressed != 2 {
		t.Fatalf("inside the window: suppressed = %d, want 2 (closing + reference)", suppressed)
	}
	if len(result.Issues.Items) != 0 {
		t.Fatalf("inside the window both issues must be withheld, got %+v", result.Issues.Items)
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
      "repo": "kubestellar/hive",
      "issue": 42,
      "pr_number": 100,
      "pr_repo": "kubestellar/hive",
      "pr_url": "https://github.com/kubestellar/hive/pull/100",
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
	got, ok := l.Lookup("kubestellar/hive", 42)
	if !ok {
		t.Fatal("pre-#3980 claim did not load")
	}
	if got.Reference {
		t.Error("pre-#3980 claim must load as a strong (non-reference) claim")
	}

	// Positive control: it must still suppress agent work.
	result := &ActionableResult{}
	result.Issues.Items = []Issue{{Repo: "kubestellar/hive", Number: 42}}
	result.Issues.Count = 1
	if suppressed := FilterClaimedIssues(result, l, nil, testLogger()); suppressed != 1 {
		t.Fatalf("pre-#3980 claim stopped suppressing agent work: suppressed=%d", suppressed)
	}
}
