package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestParseClaimedIssues(t *testing.T) {
	const defaultRepo = "torch-spyre/spyre-inference"

	tests := []struct {
		name string
		text string
		want []ClaimedRef
	}{
		// Every GitHub closing keyword, in both singular and past-tense forms.
		{"close", "close #1", []ClaimedRef{{defaultRepo, 1}}},
		{"closes", "Closes #2", []ClaimedRef{{defaultRepo, 2}}},
		{"closed", "closed #3", []ClaimedRef{{defaultRepo, 3}}},
		{"fix", "fix #4", []ClaimedRef{{defaultRepo, 4}}},
		{"fixes", "Fixes #5", []ClaimedRef{{defaultRepo, 5}}},
		{"fixed", "FIXED #6", []ClaimedRef{{defaultRepo, 6}}},
		{"resolve", "resolve #7", []ClaimedRef{{defaultRepo, 7}}},
		{"resolves", "Resolves #8", []ClaimedRef{{defaultRepo, 8}}},
		{"resolved", "resolved #9", []ClaimedRef{{defaultRepo, 9}}},

		{
			name: "case-insensitive mixed case",
			text: "FiXeS #423",
			want: []ClaimedRef{{defaultRepo, 423}},
		},
		{
			name: "colon separator",
			text: "Fixes: #424",
			want: []ClaimedRef{{defaultRepo, 424}},
		},
		{
			name: "cross-repo form",
			text: "Fixes hivecommons/hive#2149",
			want: []ClaimedRef{{"hivecommons/hive", 2149}},
		},
		{
			name: "multiple refs, one per keyword",
			text: "Fixes #10, Closes #11\nResolves #12",
			want: []ClaimedRef{{defaultRepo, 10}, {defaultRepo, 11}, {defaultRepo, 12}},
		},
		{
			name: "de-duplicates repeated refs",
			text: "Fixes #20 and also fixes #20",
			want: []ClaimedRef{{defaultRepo, 20}},
		},
		{
			name: "body prose around the ref",
			text: "## Summary\n\nThis PR reworks the parser.\n\nFixes #443\n\nSigned-off-by: agent",
			want: []ClaimedRef{{defaultRepo, 443}},
		},

		// Negative cases — these must NOT claim anything.
		{
			name: "no issue reference at all (PR #443 in the real incident)",
			text: "test",
			want: nil,
		},
		{
			name: "empty text",
			text: "",
			want: nil,
		},
		{
			name: "bare mention without a closing keyword",
			text: "See #99 for context",
			want: nil,
		},
		{
			name: "related-to is not a closing keyword",
			text: "Related to #98",
			want: nil,
		},
		{
			name: "keyword embedded in a larger word does not match",
			text: "prefixes #97",
			want: nil,
		},
		{
			name: "issue number zero is rejected",
			text: "Fixes #0",
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseClaimedIssues(tt.text, defaultRepo)
			if len(got) != len(tt.want) {
				t.Fatalf("ParseClaimedIssues(%q) = %+v, want %+v", tt.text, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("ref[%d] = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestIssueFromBranchName(t *testing.T) {
	tests := []struct {
		name   string
		branch string
		want   int
		ok     bool
	}{
		{"issue-N", "issue-423", 423, true},
		{"issue/N", "issue/424", 424, true},
		{"issueN", "issue426", 426, true},
		{"fix-issue-N", "fix-issue-427", 427, true},
		{"fix/N", "fix/429", 429, true},
		{"gh-N", "gh-430", 430, true},
		{"N-slug", "431-add-retry-backoff", 431, true},
		{"nested ref", "agents/issue-439", 439, true},

		{"empty branch", "", 0, false},
		{"no digits at all", "feat-pr-dedup-guard", 0, false},
		{"main", "main", 0, false},
		{"zero is rejected", "issue-0", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := issueFromBranchName(tt.branch)
			if ok != tt.ok || got != tt.want {
				t.Errorf("issueFromBranchName(%q) = (%d, %v), want (%d, %v)", tt.branch, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestFetchClaimsBranchHeuristic(t *testing.T) {
	// PR #443 in the real incident had body "test" and no issue reference. The
	// branch-name heuristic is the only thing that can recover the link.
	prWithBranch := func(number int, title, body, branch string) map[string]any {
		p := pr(number, "clubanderson", title, body)
		p["head"] = map[string]any{"ref": branch}
		return p
	}

	tests := []struct {
		name       string
		prs        []map[string]any
		wantIssues []int
	}{
		{
			name:       "unreferenced PR is linked via its branch name",
			prs:        []map[string]any{prWithBranch(443, "test", "test", "issue-443")},
			wantIssues: []int{443},
		},
		{
			name:       "explicit Fixes wins over the branch name",
			prs:        []map[string]any{prWithBranch(423, "fix", "Fixes #100", "issue-999")},
			wantIssues: []int{100},
		},
		{
			name:       "unreferenced PR on a non-numeric branch claims nothing",
			prs:        []map[string]any{prWithBranch(443, "test", "test", "scratch-work")},
			wantIssues: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := prClaimServer(t, tt.prs, http.StatusOK)
			c := NewClientForTest(srv.URL, "torch-spyre", []string{"spyre-inference"}, testLogger())
			claims, err := c.FetchClaims(context.Background(), HiveIdentity{AIAuthor: "clubanderson"})
			if err != nil {
				t.Fatal(err)
			}
			if len(claims) != len(tt.wantIssues) {
				t.Fatalf("got %+v, want issues %v", claims, tt.wantIssues)
			}
			for i, want := range tt.wantIssues {
				if claims[i].Issue != want {
					t.Errorf("claim[%d].Issue = %d, want %d", i, claims[i].Issue, want)
				}
			}
		})
	}
}

func TestHiveIdentityMatches(t *testing.T) {
	tests := []struct {
		name     string
		identity HiveIdentity
		author   string
		want     bool
	}{
		{"ai author exact", HiveIdentity{AIAuthor: "clubanderson"}, "clubanderson", true},
		{"ai author case-insensitive", HiveIdentity{AIAuthor: "clubanderson"}, "ClubAnderson", true},
		{"app bot login", HiveIdentity{AppLogin: "kubestellar-hive[bot]"}, "kubestellar-hive[bot]", true},
		{"human contributor never matches", HiveIdentity{AIAuthor: "clubanderson"}, "some-outside-dev", false},
		{"other bot never matches", HiveIdentity{AppLogin: "kubestellar-hive[bot]"}, "dependabot[bot]", false},
		{"empty author never matches", HiveIdentity{AIAuthor: "clubanderson"}, "", false},
		{"zero identity never matches", HiveIdentity{}, "clubanderson", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.identity.Matches(tt.author); got != tt.want {
				t.Errorf("Matches(%q) = %v, want %v", tt.author, got, tt.want)
			}
		})
	}

	if !(HiveIdentity{}).IsZero() {
		t.Error("empty identity should report IsZero")
	}
	if (HiveIdentity{AIAuthor: "x"}).IsZero() {
		t.Error("identity with AIAuthor should not report IsZero")
	}
}

func TestFilterClaimedIssues(t *testing.T) {
	claim := IssueClaim{
		Repo: "spyre-inference", Issue: 100,
		PRNumber: 423, PRRepo: "spyre-inference",
		PRURL:      "https://github.com/torch-spyre/spyre-inference/pull/423",
		ObservedAt: time.Now(),
	}

	tests := []struct {
		name           string
		items          []Issue
		claims         []IssueClaim
		wantSuppressed int
		wantRemaining  []int
		wantSLA        int
	}{
		{
			name:           "claimed issue is suppressed",
			items:          []Issue{{Repo: "spyre-inference", Number: 100, AgeMinutes: 5}},
			claims:         []IssueClaim{claim},
			wantSuppressed: 1,
			wantRemaining:  nil,
		},
		{
			name: "unclaimed issues survive",
			items: []Issue{
				{Repo: "spyre-inference", Number: 100, AgeMinutes: 5},
				{Repo: "spyre-inference", Number: 101, AgeMinutes: 5},
			},
			claims:         []IssueClaim{claim},
			wantSuppressed: 1,
			wantRemaining:  []int{101},
		},
		{
			name:           "same number in a different repo is not suppressed",
			items:          []Issue{{Repo: "other-repo", Number: 100, AgeMinutes: 5}},
			claims:         []IssueClaim{claim},
			wantSuppressed: 0,
			wantRemaining:  []int{100},
		},
		{
			name:           "empty ledger suppresses nothing",
			items:          []Issue{{Repo: "spyre-inference", Number: 100}},
			claims:         nil,
			wantSuppressed: 0,
			wantRemaining:  []int{100},
		},
		{
			// #4929: an EXTERNAL claim (a contributor's PR) now DEFERS agent
			// work for the bounded window instead of being ignored outright,
			// so an agent that cannot check for existing PRs is not handed
			// work a live PR already covers.
			name:  "external claim defers agent work inside the window",
			items: []Issue{{Repo: "spyre-inference", Number: 100, AgeMinutes: 5}},
			claims: []IssueClaim{{
				Repo: "spyre-inference", Issue: 100,
				PRNumber: 900, PRRepo: "spyre-inference",
				PRURL:           "https://github.com/torch-spyre/spyre-inference/pull/900",
				PRAuthor:        "outside-dev",
				ObservedAt:      time.Now(),
				FirstObservedAt: time.Now(),
				ExternalAuthor:  true,
			}},
			wantSuppressed: 1,
			wantRemaining:  nil,
		},
		{
			// #3768's invariant, now expressed as a BOUND rather than an
			// absence: a stranger's PR delays the hive's own pipeline at most
			// one window and can never freeze it. An adversary has to keep
			// opening new PRs to extend the delay, which PR creation itself
			// rate-limits.
			name:  "external claim releases agent work past the window",
			items: []Issue{{Repo: "spyre-inference", Number: 100, AgeMinutes: 5}},
			claims: []IssueClaim{{
				Repo: "spyre-inference", Issue: 100,
				PRNumber: 900, PRRepo: "spyre-inference",
				PRURL:           "https://github.com/torch-spyre/spyre-inference/pull/900",
				PRAuthor:        "outside-dev",
				ObservedAt:      time.Now(),
				FirstObservedAt: time.Now().Add(-weakClaimDeferWindow - time.Hour),
				ExternalAuthor:  true,
			}},
			wantSuppressed: 0,
			wantRemaining:  []int{100},
		},
		{
			name: "SLA violations are recounted after suppression",
			items: []Issue{
				{Repo: "spyre-inference", Number: 100, AgeMinutes: slaThresholdMinutes + 1},
				{Repo: "spyre-inference", Number: 101, AgeMinutes: slaThresholdMinutes + 1},
			},
			claims:         []IssueClaim{claim},
			wantSuppressed: 1,
			wantRemaining:  []int{101},
			wantSLA:        1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ledger := NewClaimLedger(filepath.Join(t.TempDir(), "ledger.json"), testLogger())
			ledger.Reconcile(tt.claims, true)

			result := &ActionableResult{Issues: IssueResult{
				Count: len(tt.items),
				Items: tt.items,
			}}
			got := FilterClaimedIssues(result, ledger, nil, testLogger())
			if got != tt.wantSuppressed {
				t.Fatalf("suppressed = %d, want %d", got, tt.wantSuppressed)
			}
			if result.Issues.Count != len(tt.wantRemaining) {
				t.Errorf("Count = %d, want %d", result.Issues.Count, len(tt.wantRemaining))
			}
			for i, num := range tt.wantRemaining {
				if i >= len(result.Issues.Items) || result.Issues.Items[i].Number != num {
					t.Errorf("remaining[%d] mismatch: got %+v, want #%d", i, result.Issues.Items, num)
				}
			}
			if tt.wantSLA != 0 && result.Issues.SLAViolations != tt.wantSLA {
				t.Errorf("SLAViolations = %d, want %d", result.Issues.SLAViolations, tt.wantSLA)
			}
		})
	}
}

func TestFilterClaimedIssuesNilSafety(t *testing.T) {
	if got := FilterClaimedIssues(nil, nil, nil, testLogger()); got != 0 {
		t.Errorf("nil result/ledger should be a no-op, got %d", got)
	}
	ledger := NewClaimLedger(filepath.Join(t.TempDir(), "l.json"), testLogger())
	if got := FilterClaimedIssues(&ActionableResult{}, ledger, nil, nil); got != 0 {
		t.Errorf("empty ledger should suppress nothing, got %d", got)
	}
}

func TestClaimLedgerRoundTripThroughDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pr-claims.json")
	now := time.Now().UTC().Truncate(time.Second)

	original := NewClaimLedger(path, testLogger())
	original.Reconcile([]IssueClaim{
		{Repo: "spyre-inference", Issue: 100, PRNumber: 423, PRRepo: "spyre-inference",
			PRURL: "https://example.test/pull/423", PRAuthor: "clubanderson", ObservedAt: now},
		{Repo: "spyre-inference", Issue: 101, PRNumber: 424, PRRepo: "spyre-inference",
			PRURL: "https://example.test/pull/424", PRAuthor: "clubanderson", ObservedAt: now},
	}, true)
	if err := original.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// The temp file must not survive the atomic rename.
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temp file should not remain after atomic rename")
	}

	reloaded, err := LoadClaimLedger(path, testLogger())
	if err != nil {
		t.Fatalf("LoadClaimLedger: %v", err)
	}
	got := reloaded.Claims()
	if len(got) != 2 {
		t.Fatalf("reloaded %d claims, want 2", len(got))
	}
	c, ok := reloaded.Lookup("spyre-inference", 100)
	if !ok {
		t.Fatal("issue 100 missing after round-trip")
	}
	if c.PRNumber != 423 || c.PRURL != "https://example.test/pull/423" || c.PRAuthor != "clubanderson" {
		t.Errorf("claim fields lost in round-trip: %+v", c)
	}
	if !c.ObservedAt.Equal(now) {
		t.Errorf("ObservedAt = %v, want %v", c.ObservedAt, now)
	}
}

func TestLoadClaimLedgerEdgeCases(t *testing.T) {
	t.Run("missing file yields empty ledger without error", func(t *testing.T) {
		l, err := LoadClaimLedger(filepath.Join(t.TempDir(), "nope.json"), testLogger())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(l.Claims()) != 0 {
			t.Errorf("expected empty ledger, got %d claims", len(l.Claims()))
		}
	})

	t.Run("corrupt file reports error but yields usable ledger", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "bad.json")
		if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
			t.Fatal(err)
		}
		l, err := LoadClaimLedger(path, testLogger())
		if err == nil {
			t.Error("expected a parse error")
		}
		if l == nil {
			t.Fatal("ledger must never be nil, even on parse failure")
		}
		if len(l.Claims()) != 0 {
			t.Errorf("corrupt ledger should load empty, got %d", len(l.Claims()))
		}
	})

	t.Run("entries past the TTL are dropped on load", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "old.json")
		stale := time.Now().Add(-claimLedgerTTL - time.Hour)
		fresh := time.Now()
		data, _ := json.Marshal(ledgerFile{Claims: []IssueClaim{
			{Repo: "r", Issue: 1, PRNumber: 10, ObservedAt: stale},
			{Repo: "r", Issue: 2, PRNumber: 11, ObservedAt: fresh},
		}})
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		l, err := LoadClaimLedger(path, testLogger())
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := l.Lookup("r", 1); ok {
			t.Error("stale claim should have been pruned on load")
		}
		if _, ok := l.Lookup("r", 2); !ok {
			t.Error("fresh claim should have survived load")
		}
	})

	t.Run("malformed entries are skipped", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "malformed.json")
		data, _ := json.Marshal(ledgerFile{Claims: []IssueClaim{
			{Repo: "", Issue: 1, ObservedAt: time.Now()},
			{Repo: "r", Issue: 0, ObservedAt: time.Now()},
			{Repo: "r", Issue: 5, ObservedAt: time.Now()},
		}})
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		l, _ := LoadClaimLedger(path, testLogger())
		if len(l.Claims()) != 1 {
			t.Errorf("expected only the valid claim to load, got %+v", l.Claims())
		}
	})
}

func TestClaimLedgerReconcile(t *testing.T) {
	now := time.Now()
	existing := IssueClaim{Repo: "r", Issue: 1, PRNumber: 423, ObservedAt: now}

	tests := []struct {
		name          string
		seed          []IssueClaim
		live          []IssueClaim
		authoritative bool
		wantIssues    []int
		desc          string
	}{
		{
			name:          "authoritative scan releases a closed PR's claim",
			seed:          []IssueClaim{existing},
			live:          nil,
			authoritative: true,
			wantIssues:    nil,
			desc:          "PR 423 closed/merged, so issue 1 goes back on the work list",
		},
		{
			name:          "authoritative scan replaces the whole set",
			seed:          []IssueClaim{existing},
			live:          []IssueClaim{{Repo: "r", Issue: 2, PRNumber: 424, ObservedAt: now}},
			authoritative: true,
			wantIssues:    []int{2},
		},
		{
			name:          "non-authoritative scan retains prior claims (fail closed)",
			seed:          []IssueClaim{existing},
			live:          nil,
			authoritative: false,
			wantIssues:    []int{1},
			desc:          "API failed; keeping the claim is what prevents the duplicate",
		},
		{
			name:          "non-authoritative scan merges partial results",
			seed:          []IssueClaim{existing},
			live:          []IssueClaim{{Repo: "r", Issue: 2, PRNumber: 424, ObservedAt: now}},
			authoritative: false,
			wantIssues:    []int{1, 2},
		},
		{
			name:          "invalid live entries are ignored",
			seed:          nil,
			live:          []IssueClaim{{Repo: "", Issue: 3}, {Repo: "r", Issue: 0}},
			authoritative: true,
			wantIssues:    nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := NewClaimLedger(filepath.Join(t.TempDir(), "l.json"), testLogger())
			l.Reconcile(tt.seed, true)
			l.Reconcile(tt.live, tt.authoritative)

			got := l.Claims()
			if len(got) != len(tt.wantIssues) {
				t.Fatalf("got %+v, want issues %v (%s)", got, tt.wantIssues, tt.desc)
			}
			for i, want := range tt.wantIssues {
				if got[i].Issue != want {
					t.Errorf("claim[%d].Issue = %d, want %d", i, got[i].Issue, want)
				}
			}
		})
	}
}

func TestClaimLedgerPrunesStaleOnNonAuthoritativeReconcile(t *testing.T) {
	l := NewClaimLedger(filepath.Join(t.TempDir(), "l.json"), testLogger())
	l.Reconcile([]IssueClaim{
		{Repo: "r", Issue: 1, PRNumber: 423, ObservedAt: time.Now().Add(-claimLedgerTTL - time.Hour)},
		{Repo: "r", Issue: 2, PRNumber: 424, ObservedAt: time.Now()},
	}, true)

	// A failing API keeps the ledger, but the TTL still expires old entries so
	// a permanently-broken token cannot pin a claim forever.
	l.Reconcile(nil, false)

	if _, ok := l.Lookup("r", 1); ok {
		t.Error("stale claim should have been pruned")
	}
	if _, ok := l.Lookup("r", 2); !ok {
		t.Error("fresh claim should have been retained")
	}
}

func TestClaimLedgerSetTTLAndClock(t *testing.T) {
	l := NewClaimLedger(filepath.Join(t.TempDir(), "l.json"), testLogger())
	base := time.Now()
	l.SetClock(func() time.Time { return base })
	l.SetTTL(time.Minute)

	l.Reconcile([]IssueClaim{{Repo: "r", Issue: 1, PRNumber: 1, ObservedAt: base.Add(-2 * time.Minute)}}, true)
	l.Reconcile(nil, false)
	if _, ok := l.Lookup("r", 1); ok {
		t.Error("claim older than the overridden TTL should be pruned")
	}

	// Zero/nil overrides must be ignored rather than corrupting the ledger.
	l.SetTTL(0)
	l.SetClock(nil)
	if l.ttl != time.Minute {
		t.Errorf("SetTTL(0) should be ignored, ttl = %v", l.ttl)
	}
}

func TestNilLedgerMethodsAreSafe(t *testing.T) {
	var l *ClaimLedger
	l.SetTTL(time.Hour)
	l.SetClock(time.Now)
	l.Reconcile([]IssueClaim{{Repo: "r", Issue: 1}}, true)
	if got := l.Claims(); got != nil {
		t.Errorf("nil ledger Claims() = %+v, want nil", got)
	}
	if _, ok := l.Lookup("r", 1); ok {
		t.Error("nil ledger should never report a claim")
	}
	if err := l.Save(); err != nil {
		t.Errorf("nil ledger Save() = %v, want nil", err)
	}
}

// prClaimServer stands up a fake GitHub API serving open PRs for one repo.
func prClaimServer(t *testing.T, prs []map[string]any, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(prs)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func pr(number int, author, title, body string) map[string]any {
	return map[string]any{
		"number":   number,
		"title":    title,
		"body":     body,
		"user":     map[string]any{"login": author},
		"html_url": fmt.Sprintf("https://github.com/torch-spyre/spyre-inference/pull/%d", number),
	}
}

func TestFetchClaims(t *testing.T) {
	tests := []struct {
		name       string
		prs        []map[string]any
		identity   HiveIdentity
		wantIssues []int
		// wantExternal[i] is claim[i]'s expected ExternalAuthor flag (#3768):
		// false for a hive-authored PR, true for anyone else's.
		wantExternal []bool
	}{
		{
			name:         "hive-authored PR with Fixes claims the issue",
			prs:          []map[string]any{pr(423, "clubanderson", "fix the thing", "Fixes #100")},
			identity:     HiveIdentity{AIAuthor: "clubanderson"},
			wantIssues:   []int{100},
			wantExternal: []bool{false},
		},
		{
			name:         "claim in the title is honoured",
			prs:          []map[string]any{pr(424, "clubanderson", "Closes #101 — fix the thing", "")},
			identity:     HiveIdentity{AIAuthor: "clubanderson"},
			wantIssues:   []int{101},
			wantExternal: []bool{false},
		},
		{
			name:         "PR authored by the app bot claims too",
			prs:          []map[string]any{pr(425, "kubestellar-hive[bot]", "t", "Resolves #102")},
			identity:     HiveIdentity{AppLogin: "kubestellar-hive[bot]"},
			wantIssues:   []int{102},
			wantExternal: []bool{false},
		},
		{
			// #3768: a human contributor's PR now claims its issue too — marked
			// external so FilterClaimedIssues can keep ignoring it for agent
			// work while the contribute queue honours it.
			name:         "PR from a human contributor claims, marked external",
			prs:          []map[string]any{pr(426, "outside-dev", "t", "Fixes #103")},
			identity:     HiveIdentity{AIAuthor: "clubanderson"},
			wantIssues:   []int{103},
			wantExternal: []bool{true},
		},
		{
			name:       "PR with no issue reference claims nothing (the #443 gap)",
			prs:        []map[string]any{pr(443, "clubanderson", "test", "")},
			identity:   HiveIdentity{AIAuthor: "clubanderson"},
			wantIssues: nil,
		},
		{
			name: "multiple PRs, mixed authorship",
			prs: []map[string]any{
				pr(423, "clubanderson", "a", "Fixes #100"),
				pr(426, "outside-dev", "b", "Fixes #200"),
				pr(427, "clubanderson", "c", "Closes #101"),
			},
			identity:     HiveIdentity{AIAuthor: "clubanderson"},
			wantIssues:   []int{100, 200, 101},
			wantExternal: []bool{false, true, false},
		},
		{
			name:       "no open PRs at all",
			prs:        []map[string]any{},
			identity:   HiveIdentity{AIAuthor: "clubanderson"},
			wantIssues: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := prClaimServer(t, tt.prs, http.StatusOK)
			c := NewClientForTest(srv.URL, "torch-spyre", []string{"spyre-inference"}, testLogger())

			claims, err := c.FetchClaims(context.Background(), tt.identity)
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
				if claims[i].PRURL == "" {
					t.Errorf("claim[%d] missing PRURL — suppression logs would be undebuggable", i)
				}
				if i < len(tt.wantExternal) && claims[i].ExternalAuthor != tt.wantExternal[i] {
					t.Errorf("claim[%d].ExternalAuthor = %v, want %v (author %q)",
						i, claims[i].ExternalAuthor, tt.wantExternal[i], claims[i].PRAuthor)
				}
			}
		})
	}
}

func TestFetchClaimsErrors(t *testing.T) {
	t.Run("API failure is reported", func(t *testing.T) {
		srv := prClaimServer(t, nil, http.StatusUnauthorized)
		c := NewClientForTest(srv.URL, "torch-spyre", []string{"spyre-inference"}, testLogger())
		if _, err := c.FetchClaims(context.Background(), HiveIdentity{AIAuthor: "clubanderson"}); err == nil {
			t.Fatal("expected an error from a 401 response")
		}
	})

	t.Run("zero identity is rejected rather than trusting strangers' PRs", func(t *testing.T) {
		srv := prClaimServer(t, nil, http.StatusOK)
		c := NewClientForTest(srv.URL, "torch-spyre", []string{"spyre-inference"}, testLogger())
		if _, err := c.FetchClaims(context.Background(), HiveIdentity{}); err == nil {
			t.Fatal("expected an error when no hive identity is configured")
		}
	})

	t.Run("nil client is rejected", func(t *testing.T) {
		var c *Client
		if _, err := c.FetchClaims(context.Background(), HiveIdentity{AIAuthor: "x"}); err == nil {
			t.Fatal("expected an error from a nil client")
		}
	})
}

// TestApplyDuplicatePRGuardFailClosed is the regression test for the real
// incident: the GitHub API returned `401 Bad credentials`, and a purely live
// check would have seen zero claims and re-offered the issue, producing yet
// another duplicate PR. The persisted ledger must keep suppressing.
func TestApplyDuplicatePRGuardFailClosed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pr-claims.json")
	identity := HiveIdentity{AIAuthor: "clubanderson"}

	// Cycle 1: the API works, the hive sees its own PR #423 claiming issue 100.
	okSrv := prClaimServer(t, []map[string]any{pr(423, "clubanderson", "fix", "Fixes #100")}, http.StatusOK)
	okClient := NewClientForTest(okSrv.URL, "torch-spyre", []string{"spyre-inference"}, testLogger())

	ledger, err := LoadClaimLedger(path, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	result := &ActionableResult{Issues: IssueResult{Count: 1, Items: []Issue{
		{Repo: "spyre-inference", Number: 100},
	}}}
	if got := ApplyDuplicatePRGuard(context.Background(), okClient, ledger, identity, result, nil, testLogger()); got != 1 {
		t.Fatalf("cycle 1 suppressed = %d, want 1", got)
	}

	// The pod restarts: a brand-new ledger is loaded from the PVC, and the API
	// is now failing with 401. This is exactly the incident's condition.
	failSrv := prClaimServer(t, nil, http.StatusUnauthorized)
	failClient := NewClientForTest(failSrv.URL, "torch-spyre", []string{"spyre-inference"}, testLogger())

	restarted, err := LoadClaimLedger(path, testLogger())
	if err != nil {
		t.Fatalf("ledger must reload across a restart: %v", err)
	}
	if _, ok := restarted.Lookup("spyre-inference", 100); !ok {
		t.Fatal("claim did not survive the restart — the ledger is the whole point")
	}

	result2 := &ActionableResult{Issues: IssueResult{Count: 1, Items: []Issue{
		{Repo: "spyre-inference", Number: 100},
	}}}
	got := ApplyDuplicatePRGuard(context.Background(), failClient, restarted, identity, result2, nil, testLogger())
	if got != 1 {
		t.Fatalf("fail-closed: suppressed = %d, want 1 (API down must NOT re-offer the claimed issue)", got)
	}
	if result2.Issues.Count != 0 {
		t.Errorf("claimed issue was re-offered despite the API failure: %+v", result2.Issues.Items)
	}
}

// TestApplyDuplicatePRGuardClosedPRReleasesClaim covers the other direction: a
// successful scan that no longer sees the PR must hand the issue back.
func TestApplyDuplicatePRGuardClosedPRReleasesClaim(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pr-claims.json")
	identity := HiveIdentity{AIAuthor: "clubanderson"}

	okSrv := prClaimServer(t, []map[string]any{pr(423, "clubanderson", "fix", "Fixes #100")}, http.StatusOK)
	okClient := NewClientForTest(okSrv.URL, "torch-spyre", []string{"spyre-inference"}, testLogger())
	ledger, _ := LoadClaimLedger(path, testLogger())
	ApplyDuplicatePRGuard(context.Background(), okClient, ledger,
		identity, &ActionableResult{Issues: IssueResult{Items: []Issue{{Repo: "spyre-inference", Number: 100}}}}, nil, testLogger())

	// PR #423 is now closed without merging, so the repo returns no open PRs.
	emptySrv := prClaimServer(t, []map[string]any{}, http.StatusOK)
	emptyClient := NewClientForTest(emptySrv.URL, "torch-spyre", []string{"spyre-inference"}, testLogger())

	result := &ActionableResult{Issues: IssueResult{Count: 1, Items: []Issue{
		{Repo: "spyre-inference", Number: 100},
	}}}
	if got := ApplyDuplicatePRGuard(context.Background(), emptyClient, ledger, identity, result, nil, testLogger()); got != 0 {
		t.Fatalf("suppressed = %d, want 0 (a closed PR must release its claim)", got)
	}
	if result.Issues.Count != 1 {
		t.Errorf("issue should be actionable again, got %+v", result.Issues.Items)
	}

	// And the release must be persisted, not just in memory.
	reloaded, _ := LoadClaimLedger(path, testLogger())
	if _, ok := reloaded.Lookup("spyre-inference", 100); ok {
		t.Error("released claim should not survive on disk")
	}
}

func TestApplyDuplicatePRGuardNilInputs(t *testing.T) {
	if got := ApplyDuplicatePRGuard(context.Background(), nil, nil, HiveIdentity{}, nil, nil, testLogger()); got != 0 {
		t.Errorf("nil ledger/result should be a no-op, got %d", got)
	}
	ledger := NewClaimLedger(filepath.Join(t.TempDir(), "l.json"), testLogger())
	// A nil client must fail closed (empty ledger => nothing to suppress), not panic.
	if got := ApplyDuplicatePRGuard(context.Background(), nil, ledger, HiveIdentity{AIAuthor: "x"},
		&ActionableResult{}, nil, testLogger()); got != 0 {
		t.Errorf("nil client should suppress nothing, got %d", got)
	}
}

// TestFilterClaimedIssues_RedStaleReleases is Fix #3: a claimed issue whose
// claiming PR is red+stale is RELEASED (kept actionable), while a healthy
// claiming PR still suppresses its issue. GENERIC: the predicate keys only off
// check state + staleness, supplied by the caller.
func TestFilterClaimedIssues_RedStaleReleases(t *testing.T) {
	ledger := NewClaimLedger(filepath.Join(t.TempDir(), "l.json"), testLogger())
	// Two claims: PR #101 claims issue #1 (red+stale), PR #202 claims issue #2
	// (healthy). Both issues are otherwise actionable.
	ledger.Reconcile([]IssueClaim{
		{Repo: "r", Issue: 1, PRNumber: 101, PRRepo: "r", PRURL: "u1", PRAuthor: "bot"},
		{Repo: "r", Issue: 2, PRNumber: 202, PRRepo: "r", PRURL: "u2", PRAuthor: "bot"},
	}, true)

	result := &ActionableResult{Issues: IssueResult{Count: 2, Items: []Issue{
		{Repo: "r", Number: 1, Title: "one"},
		{Repo: "r", Number: 2, Title: "two"},
	}}}

	// redStale reports true only for the red+stale claiming PR #101.
	redStale := func(prRepo string, prNumber int) bool {
		return prRepo == "r" && prNumber == 101
	}

	suppressed := FilterClaimedIssues(result, ledger, redStale, testLogger())
	// Only issue #2 (healthy claiming PR) is suppressed; #1 is released.
	if suppressed != 1 {
		t.Fatalf("suppressed = %d, want 1 (only the healthy-claim issue)", suppressed)
	}
	if result.Issues.Count != 1 || result.Issues.Items[0].Number != 1 {
		t.Fatalf("issue #1 must be released (kept actionable); got %+v", result.Issues.Items)
	}
}

// TestFilterClaimedIssues_NilRedStalePreservesSuppression proves the release
// valve is opt-in: a nil predicate reproduces the original unconditional
// suppression, so healthy deployments are byte-for-byte unaffected.
func TestFilterClaimedIssues_NilRedStalePreservesSuppression(t *testing.T) {
	ledger := NewClaimLedger(filepath.Join(t.TempDir(), "l.json"), testLogger())
	ledger.Reconcile([]IssueClaim{
		{Repo: "r", Issue: 1, PRNumber: 101, PRRepo: "r", PRURL: "u1", PRAuthor: "bot"},
	}, true)
	result := &ActionableResult{Issues: IssueResult{Count: 1, Items: []Issue{{Repo: "r", Number: 1}}}}
	if got := FilterClaimedIssues(result, ledger, nil, testLogger()); got != 1 {
		t.Fatalf("nil predicate must suppress unconditionally, got %d", got)
	}
	if result.Issues.Count != 0 {
		t.Fatalf("claimed issue must be suppressed with nil predicate; got %+v", result.Issues.Items)
	}
}

// TestClaimLedgerHivePrecedence: when the same issue is claimed by both a
// hive-authored PR and an external contributor's PR (#3768), the hive claim
// must win the ledger slot in EITHER arrival order — otherwise an external PR
// racing the map insert would blind FilterClaimedIssues (which honours only
// hive claims) and re-open the very duplicate-PR hole the guard exists for.
func TestClaimLedgerHivePrecedence(t *testing.T) {
	hive := IssueClaim{
		Repo: "r", Issue: 1, PRNumber: 10, PRRepo: "r",
		PRURL: "hive-pr", PRAuthor: "clubanderson", ObservedAt: time.Now(),
	}
	external := IssueClaim{
		Repo: "r", Issue: 1, PRNumber: 20, PRRepo: "r",
		PRURL: "ext-pr", PRAuthor: "outside-dev", ObservedAt: time.Now(),
		ExternalAuthor: true,
	}

	t.Run("authoritative, hive first", func(t *testing.T) {
		l := NewClaimLedger(filepath.Join(t.TempDir(), "l.json"), testLogger())
		l.Reconcile([]IssueClaim{hive, external}, true)
		got, ok := l.Lookup("r", 1)
		if !ok || got.ExternalAuthor || got.PRNumber != 10 {
			t.Fatalf("hive claim must win, got %+v (ok=%v)", got, ok)
		}
	})
	t.Run("authoritative, external first", func(t *testing.T) {
		l := NewClaimLedger(filepath.Join(t.TempDir(), "l.json"), testLogger())
		l.Reconcile([]IssueClaim{external, hive}, true)
		got, ok := l.Lookup("r", 1)
		if !ok || got.ExternalAuthor || got.PRNumber != 10 {
			t.Fatalf("hive claim must win regardless of order, got %+v (ok=%v)", got, ok)
		}
	})
	t.Run("non-authoritative refresh cannot demote a hive claim", func(t *testing.T) {
		l := NewClaimLedger(filepath.Join(t.TempDir(), "l.json"), testLogger())
		l.Reconcile([]IssueClaim{hive}, true)
		l.Reconcile([]IssueClaim{external}, false)
		got, ok := l.Lookup("r", 1)
		if !ok || got.ExternalAuthor || got.PRNumber != 10 {
			t.Fatalf("partial fetch must not replace a hive claim with an external one, got %+v (ok=%v)", got, ok)
		}
	})
	t.Run("external-only claim is still recorded", func(t *testing.T) {
		l := NewClaimLedger(filepath.Join(t.TempDir(), "l.json"), testLogger())
		l.Reconcile([]IssueClaim{external}, true)
		got, ok := l.Lookup("r", 1)
		if !ok || !got.ExternalAuthor || got.PRNumber != 20 {
			t.Fatalf("external claim must be present for the contribute queue, got %+v (ok=%v)", got, ok)
		}
	})
}

// TestClaimLedgerBackCompatPreThreeSevenSixEight: a ledger written before
// #3768 has no external_author field. Every entry it holds was by definition
// hive-authored (the old FetchClaims recorded nothing else), so it must load
// as hive-authored (ExternalAuthor=false) and KEEP suppressing agent work
// across the upgrade — positive control included.
func TestClaimLedgerBackCompatPreThreeSevenSixEight(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pr-claims.json")
	oldFormat := fmt.Sprintf(`{
  "saved_at": %[1]q,
  "claims": [
    {"repo": "r", "issue": 1, "pr_number": 10, "pr_repo": "r",
     "pr_url": "u", "pr_author": "clubanderson", "observed_at": %[1]q}
  ]
}`, time.Now().Format(time.RFC3339Nano))
	if err := os.WriteFile(path, []byte(oldFormat), 0o644); err != nil {
		t.Fatal(err)
	}

	l, err := LoadClaimLedger(path, testLogger())
	if err != nil {
		t.Fatalf("old-format ledger must load cleanly: %v", err)
	}
	claim, ok := l.Lookup("r", 1)
	if !ok {
		t.Fatal("old-format claim must survive the load")
	}
	if claim.ExternalAuthor {
		t.Fatal("pre-#3768 claim must read as hive-authored, not external")
	}

	// Positive control: it must still suppress agent work after the upgrade.
	result := &ActionableResult{Issues: IssueResult{Count: 1, Items: []Issue{{Repo: "r", Number: 1}}}}
	if got := FilterClaimedIssues(result, l, nil, testLogger()); got != 1 {
		t.Fatalf("upgraded ledger stopped suppressing: got %d, want 1", got)
	}
}
