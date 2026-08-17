package github

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

// #3980 regression suite: a contributor's open PR that references its issue
// with a NON-closing keyword (`Refs #N` — deliberately not `Fixes`, because
// the issue is maintainer-gated) was invisible to the claim ledger, so the
// contribute queue re-offered the issue to the same contributor every 4h
// cooldown window, forever. These tests cover the soft-reference keyword
// grammar, FetchClaims' soft-claim records, ledger strength precedence, and
// the bounded agent-side deferral that replaces #3792's all-or-nothing
// external-claim bypass.

func TestParseSoftReferencedIssues(t *testing.T) {
	const defaultRepo = "torch-spyre/spyre-inference"

	tests := []struct {
		name string
		text string
		want []ClaimedRef
	}{
		// Every recognized soft keyword form.
		{"refs", "Refs #3498", []ClaimedRef{{defaultRepo, 3498}}},
		{"ref", "Ref #10", []ClaimedRef{{defaultRepo, 10}}},
		{"part of", "Part of #11", []ClaimedRef{{defaultRepo, 11}}},
		{"relates to", "Relates to #12", []ClaimedRef{{defaultRepo, 12}}},
		{"related to", "Related to #13", []ClaimedRef{{defaultRepo, 13}}},
		{"addresses", "Addresses #14", []ClaimedRef{{defaultRepo, 14}}},

		{
			name: "case-insensitive mixed case",
			text: "rEfS #423",
			want: []ClaimedRef{{defaultRepo, 423}},
		},
		{
			name: "colon separator",
			text: "Refs: #424",
			want: []ClaimedRef{{defaultRepo, 424}},
		},
		{
			name: "cross-repo form",
			text: "Part of kubestellar/hive#2149",
			want: []ClaimedRef{{"kubestellar/hive", 2149}},
		},
		{
			name: "multi-word keyword tolerates extra whitespace",
			text: "Part  of #30",
			want: []ClaimedRef{{defaultRepo, 30}},
		},
		{
			name: "the PR #3898 body shape from the incident",
			text: "## Summary\n\nRefs #3498 — no `Fixes` keyword, deliberately.\n\nSigned-off-by: agent",
			want: []ClaimedRef{{defaultRepo, 3498}},
		},
		{
			name: "de-duplicates repeated refs",
			text: "Refs #20 and again refs #20",
			want: []ClaimedRef{{defaultRepo, 20}},
		},
		{
			// A mixed body: the SOFT parser reports only the soft reference;
			// the closing keyword belongs to ParseClaimedIssues.
			name: "Fixes and Refs mixed — soft parser sees only the soft ref",
			text: "Fixes #100\n\nRefs #200",
			want: []ClaimedRef{{defaultRepo, 200}},
		},

		// Negative cases — these must NOT register.
		{
			name: "bare changelog-style mention never locks an issue",
			text: "* #99 fixed the flaky test\n* #98 docs",
			want: nil,
		},
		{
			name: "prose mention without a keyword",
			text: "See #99 for context",
			want: nil,
		},
		{
			name: "keyword embedded in a larger word does not match",
			text: "xrefs #97 and unrelated to #96",
			want: nil,
		},
		{
			name: "git ref path is not a reference keyword",
			text: "update refs/heads#5 handling",
			want: nil,
		},
		{
			name: "closing keywords are not soft references",
			text: "Fixes #95, Closes #94, Resolves #93",
			want: nil,
		},
		{
			name: "issue number zero is rejected",
			text: "Refs #0",
			want: nil,
		},
		{
			name: "empty text",
			text: "",
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseSoftReferencedIssues(tt.text, defaultRepo)
			if len(got) != len(tt.want) {
				t.Fatalf("ParseSoftReferencedIssues(%q) = %+v, want %+v", tt.text, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("ref[%d] = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// draftPR mirrors pr() with the draft flag set — a draft is still "someone is
// on it" for offer suppression, and FetchClaims must keep seeing drafts after
// #3970 changed how fetchPRs (a separate listing) handles them.
func draftPR(number int, author, title, body string) map[string]any {
	p := pr(number, author, title, body)
	p["draft"] = true
	return p
}

func TestFetchClaimsSoftReferences(t *testing.T) {
	tests := []struct {
		name       string
		prs        []map[string]any
		wantIssues []int
		wantSoft   []bool
		wantExt    []bool
	}{
		{
			// The exact #3980 incident shape: PR #3898's body says
			// "Refs #3498 — no Fixes keyword, deliberately".
			name:       "external Refs PR claims its issue as a soft external claim",
			prs:        []map[string]any{pr(3898, "Danathar", "tests: notify on cancel", "Refs #3498 — no `Fixes` keyword, deliberately.")},
			wantIssues: []int{3498},
			wantSoft:   []bool{true},
			wantExt:    []bool{true},
		},
		{
			name:       "Fixes and Refs in one PR yield one hard and one soft claim",
			prs:        []map[string]any{pr(500, "clubanderson", "fix", "Fixes #100\n\nRefs #200")},
			wantIssues: []int{100, 200},
			wantSoft:   []bool{false, true},
			wantExt:    []bool{false, false},
		},
		{
			name:       "Fixes and refs to the SAME issue collapse to the hard claim",
			prs:        []map[string]any{pr(501, "clubanderson", "fix", "Fixes #100 — refs #100 in the changelog")},
			wantIssues: []int{100},
			wantSoft:   []bool{false},
			wantExt:    []bool{false},
		},
		{
			// #3970 interaction pin: FetchClaims lists open PRs itself and
			// must keep counting DRAFTS as claims — once, not twice — even
			// though fetchPRs (the actionable-list path) filters/loosens
			// drafts separately.
			name:       "a draft PR still claims its issue exactly once",
			prs:        []map[string]any{draftPR(502, "outside-dev", "wip", "Refs #300")},
			wantIssues: []int{300},
			wantSoft:   []bool{true},
			wantExt:    []bool{true},
		},
		{
			name:       "a draft PR with a closing keyword hard-claims",
			prs:        []map[string]any{draftPR(503, "clubanderson", "wip", "Fixes #301")},
			wantIssues: []int{301},
			wantSoft:   []bool{false},
			wantExt:    []bool{false},
		},
		{
			// The branch-name heuristic behaves exactly as before #3980: it
			// fires when no CLOSING reference exists, so a numeric branch
			// still hard-claims alongside the body's soft reference.
			name: "numeric branch hard-claims even when a soft ref is present",
			prs: []map[string]any{func() map[string]any {
				p := pr(504, "clubanderson", "wip", "Refs #400")
				p["head"] = map[string]any{"ref": "fix-401"}
				return p
			}()},
			wantIssues: []int{401, 400},
			wantSoft:   []bool{false, true},
			wantExt:    []bool{false, false},
		},
		{
			name:       "cross-repo soft reference keys on the referenced repo",
			prs:        []map[string]any{pr(505, "outside-dev", "t", "Part of kubestellar/hive#2149")},
			wantIssues: []int{2149},
			wantSoft:   []bool{true},
			wantExt:    []bool{true},
		},
		{
			// Positive control for the "not every #N locks an issue" rule.
			name:       "bare mentions claim nothing",
			prs:        []map[string]any{pr(506, "outside-dev", "t", "see #12, and #13 is related reading")},
			wantIssues: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := prClaimServer(t, tt.prs, http.StatusOK)
			c := NewClientForTest(srv.URL, "torch-spyre", []string{"spyre-inference"}, testLogger())
			claims, err := c.FetchClaims(context.Background(), HiveIdentity{AIAuthor: "clubanderson"})
			if err != nil {
				t.Fatalf("FetchClaims: %v", err)
			}
			if len(claims) != len(tt.wantIssues) {
				t.Fatalf("got %+v, want issues %v", claims, tt.wantIssues)
			}
			for i, want := range tt.wantIssues {
				if claims[i].Issue != want {
					t.Errorf("claim[%d].Issue = %d, want %d", i, claims[i].Issue, want)
				}
				if claims[i].SoftReference != tt.wantSoft[i] {
					t.Errorf("claim[%d].SoftReference = %v, want %v", i, claims[i].SoftReference, tt.wantSoft[i])
				}
				if claims[i].ExternalAuthor != tt.wantExt[i] {
					t.Errorf("claim[%d].ExternalAuthor = %v, want %v", i, claims[i].ExternalAuthor, tt.wantExt[i])
				}
				if claims[i].FirstObservedAt.IsZero() {
					t.Errorf("claim[%d].FirstObservedAt must be stamped on fetch", i)
				}
			}
		})
	}
}

// TestFilterClaimedIssues_SoftClaimBoundedDeferral pins the #3980 claim-scope
// split on the agent side: a soft claim — even a hive-authored one — DEFERS
// agent work only within weakClaimAgentDeferralWindow, while a hive-authored
// closing claim suppresses without any window (positive control). This is the
// junk/stalled-PR guard invariant in its bounded form.
func TestFilterClaimedIssues_SoftClaimBoundedDeferral(t *testing.T) {
	mk := func(soft, external bool, firstObserved time.Time) IssueClaim {
		return IssueClaim{
			Repo: "spyre-inference", Issue: 100,
			PRNumber: 900, PRRepo: "spyre-inference",
			PRURL: "u", PRAuthor: "someone",
			ObservedAt: time.Now(), FirstObservedAt: firstObserved,
			SoftReference: soft, ExternalAuthor: external,
		}
	}
	aged := time.Now().Add(-weakClaimAgentDeferralWindow - time.Hour)

	tests := []struct {
		name           string
		claim          IssueClaim
		wantSuppressed int
	}{
		{"fresh hive soft claim defers", mk(true, false, time.Now()), 1},
		{"aged hive soft claim releases", mk(true, false, aged), 0},
		{"fresh external soft claim defers", mk(true, true, time.Now()), 1},
		{"aged external soft claim releases", mk(true, true, aged), 0},
		// Positive control: the hive's own closing claim has NO deferral
		// window — it suppresses for as long as the PR is open, exactly as
		// before #3980.
		{"aged hive hard claim still suppresses", mk(false, false, aged), 1},
		// Back-compat: a pre-#3980 ledger entry has a zero FirstObservedAt;
		// its ObservedAt must anchor the window instead, so an old external
		// entry past the window releases rather than deferring forever.
		{
			"zero FirstObservedAt falls back to ObservedAt",
			IssueClaim{
				Repo: "spyre-inference", Issue: 100,
				PRNumber: 900, PRRepo: "spyre-inference",
				PRURL: "u", PRAuthor: "someone",
				ObservedAt:     aged,
				ExternalAuthor: true,
			},
			0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ledger := NewClaimLedger(filepath.Join(t.TempDir(), "l.json"), testLogger())
			ledger.SetTTL(365 * 24 * time.Hour) // keep aged claims alive past the default TTL
			ledger.Reconcile([]IssueClaim{tt.claim}, true)
			result := &ActionableResult{Issues: IssueResult{Count: 1, Items: []Issue{
				{Repo: "spyre-inference", Number: 100},
			}}}
			if got := FilterClaimedIssues(result, ledger, nil, testLogger()); got != tt.wantSuppressed {
				t.Fatalf("suppressed = %d, want %d", got, tt.wantSuppressed)
			}
		})
	}
}

// TestClaimLedgerStrengthPrecedence generalizes #3768's hive-precedence rule:
// a soft reference must never overwrite a hard claim for the same issue in
// either arrival order, or the agent-side filter (which hard-suppresses only
// on hive-hard claims) could be blinded by a weaker racing PR.
func TestClaimLedgerStrengthPrecedence(t *testing.T) {
	hard := IssueClaim{
		Repo: "r", Issue: 1, PRNumber: 10, PRRepo: "r",
		PRURL: "hard-pr", PRAuthor: "clubanderson", ObservedAt: time.Now(),
	}
	soft := IssueClaim{
		Repo: "r", Issue: 1, PRNumber: 20, PRRepo: "r",
		PRURL: "soft-pr", PRAuthor: "clubanderson", ObservedAt: time.Now(),
		SoftReference: true,
	}
	softExt := IssueClaim{
		Repo: "r", Issue: 1, PRNumber: 30, PRRepo: "r",
		PRURL: "soft-ext-pr", PRAuthor: "outside-dev", ObservedAt: time.Now(),
		SoftReference: true, ExternalAuthor: true,
	}

	t.Run("hard first, soft cannot demote", func(t *testing.T) {
		l := NewClaimLedger(filepath.Join(t.TempDir(), "l.json"), testLogger())
		l.Reconcile([]IssueClaim{hard, soft}, true)
		got, ok := l.Lookup("r", 1)
		if !ok || got.SoftReference || got.PRNumber != 10 {
			t.Fatalf("hard claim must win, got %+v (ok=%v)", got, ok)
		}
	})
	t.Run("soft first, hard takes over", func(t *testing.T) {
		l := NewClaimLedger(filepath.Join(t.TempDir(), "l.json"), testLogger())
		l.Reconcile([]IssueClaim{soft, hard}, true)
		got, ok := l.Lookup("r", 1)
		if !ok || got.SoftReference || got.PRNumber != 10 {
			t.Fatalf("hard claim must win regardless of order, got %+v (ok=%v)", got, ok)
		}
	})
	t.Run("hive soft outranks external soft", func(t *testing.T) {
		l := NewClaimLedger(filepath.Join(t.TempDir(), "l.json"), testLogger())
		l.Reconcile([]IssueClaim{softExt, soft}, true)
		got, ok := l.Lookup("r", 1)
		if !ok || got.ExternalAuthor || got.PRNumber != 20 {
			t.Fatalf("hive soft claim must win, got %+v (ok=%v)", got, ok)
		}
	})
	t.Run("soft-only claim is still recorded for the contribute queue", func(t *testing.T) {
		l := NewClaimLedger(filepath.Join(t.TempDir(), "l.json"), testLogger())
		l.Reconcile([]IssueClaim{softExt}, true)
		got, ok := l.Lookup("r", 1)
		if !ok || !got.SoftReference || got.PRNumber != 30 {
			t.Fatalf("soft claim must be present, got %+v (ok=%v)", got, ok)
		}
	})
	t.Run("re-observing the same PR keeps its FirstObservedAt", func(t *testing.T) {
		l := NewClaimLedger(filepath.Join(t.TempDir(), "l.json"), testLogger())
		first := time.Now().Add(-48 * time.Hour)
		earlier := soft
		earlier.FirstObservedAt = first
		l.Reconcile([]IssueClaim{earlier}, true)
		refreshed := soft
		refreshed.FirstObservedAt = time.Now()
		l.Reconcile([]IssueClaim{refreshed}, true)
		got, _ := l.Lookup("r", 1)
		if !got.FirstObservedAt.Equal(first) {
			t.Fatalf("FirstObservedAt must survive re-observation: got %v, want %v — a scan-refreshed window would never expire", got.FirstObservedAt, first)
		}
	})
	t.Run("a different PR gets its own FirstObservedAt", func(t *testing.T) {
		l := NewClaimLedger(filepath.Join(t.TempDir(), "l.json"), testLogger())
		old := softExt
		old.FirstObservedAt = time.Now().Add(-100 * time.Hour)
		l.Reconcile([]IssueClaim{old}, true)
		fresh := soft // different PR number
		freshAt := time.Now()
		fresh.FirstObservedAt = freshAt
		l.Reconcile([]IssueClaim{fresh}, true)
		got, _ := l.Lookup("r", 1)
		if !got.FirstObservedAt.Equal(freshAt) {
			t.Fatalf("a NEW claiming PR must not inherit the old PR's window: got %v", got.FirstObservedAt)
		}
	})
}

// TestSoftReferenceClaimLifecycle replays #3980 end-to-end at the ledger
// level, mirroring #3792's replay: an open `Refs #N` PR is fetched, claims its
// issue (so the contribute queue's IssueClaimed hook — a ledger Lookup — will
// suppress the offer), and once the PR closes an authoritative rescan releases
// the issue back into the offer pool.
func TestSoftReferenceClaimLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pr-claims.json")
	identity := HiveIdentity{AIAuthor: "clubanderson"}

	// Cycle 1: the contributor's Refs-PR is open.
	openSrv := prClaimServer(t, []map[string]any{
		pr(3898, "Danathar", "tests: notify on cancel", "Refs #3498 — no `Fixes` keyword, deliberately."),
	}, http.StatusOK)
	openClient := NewClientForTest(openSrv.URL, "torch-spyre", []string{"spyre-inference"}, testLogger())

	ledger, err := LoadClaimLedger(path, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	live, err := openClient.FetchClaims(context.Background(), identity)
	if err != nil {
		t.Fatal(err)
	}
	ledger.Reconcile(live, true)

	claim, ok := ledger.Lookup("spyre-inference", 3498)
	if !ok {
		t.Fatal("the Refs-PR must claim its issue — this absence IS bug #3980")
	}
	if !claim.SoftReference || !claim.ExternalAuthor {
		t.Fatalf("claim must be soft+external, got %+v", claim)
	}

	// The agent pipeline is only DEFERRED by this weak claim, never frozen:
	// fresh → suppressed; past the window → released.
	fresh := &ActionableResult{Issues: IssueResult{Count: 1, Items: []Issue{{Repo: "spyre-inference", Number: 3498}}}}
	if got := FilterClaimedIssues(fresh, ledger, nil, testLogger()); got != 1 {
		t.Fatalf("fresh weak claim should defer agent work, suppressed = %d", got)
	}

	// Cycle 2: the PR merges/closes; the authoritative rescan sees no open
	// PRs, so the claim is released and the issue is offerable again.
	emptySrv := prClaimServer(t, []map[string]any{}, http.StatusOK)
	emptyClient := NewClientForTest(emptySrv.URL, "torch-spyre", []string{"spyre-inference"}, testLogger())
	live, err = emptyClient.FetchClaims(context.Background(), identity)
	if err != nil {
		t.Fatal(err)
	}
	ledger.Reconcile(live, true)
	if _, ok := ledger.Lookup("spyre-inference", 3498); ok {
		t.Fatal("a closed PR's soft claim must release its issue back to the offer pool")
	}
}
