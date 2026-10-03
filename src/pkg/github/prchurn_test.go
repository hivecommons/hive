package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestPRTerminalMergeAttributionBuckets(t *testing.T) {
	identity := HiveIdentity{AIAuthor: "hive-ai", AppLogin: "hive-app[bot]"}
	tests := []struct {
		name        string
		actor       string
		appBotLogin string
		want        string
	}{
		{name: "blank actor is unknown", actor: "", want: "unknown"},
		{name: "admin merge by maintainer is human", actor: "clubanderson", want: "human"},
		{name: "hive ai identity without audit is hive", actor: "hive-ai", want: "hive"},
		{name: "hive app identity without audit is hive", actor: "hive-app[bot]", want: "hive"},
		{name: "configured app bot login without identity is hive", actor: "hive-installation[bot]", appBotLogin: "hive-installation[bot]", want: "hive"},
		{name: "non hive bot is other automation", actor: "dependabot[bot]", want: "other_automation"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := prTerminalMergeAttribution(tt.actor, identity, tt.appBotLogin); got != tt.want {
				t.Fatalf("prTerminalMergeAttribution(%q) = %q, want %q", tt.actor, got, tt.want)
			}
		})
	}
}

// ── #7995: the churn guard ────────────────────────────────────────────────────
//
// projectbluefin/documentation#1232 consumed seven pull requests — three
// merged, four closed unmerged — and was still NEXT UP. The claim ledger could
// not see the shape: a merged or closed PR leaves the claim map immediately,
// and a PR closed without merging never entered it at all.

func churnLedger(t *testing.T) *ClaimLedger {
	t.Helper()
	return NewClaimLedger(t.TempDir()+"/pr-claims.json", testLogger())
}

func churnRecord(issue, prNumber int, state string, observedAt time.Time) IssuePRRecord {
	return IssuePRRecord{
		Repo:       "documentation",
		Issue:      issue,
		PRNumber:   prNumber,
		PRRepo:     "documentation",
		PRURL:      "https://github.com/projectbluefin/documentation/pull/" + strconv.Itoa(prNumber),
		PRAuthor:   "kylerankin",
		State:      state,
		ObservedAt: observedAt,
	}
}

// TestChurnThresholds pins WHEN an issue stops being dispatchable work. Two
// finished pull requests of either kind is the line: at two merged, merging
// work against the issue has already been shown not to settle it.
func TestChurnThresholds(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name    string
		records []IssuePRRecord
		want    bool
	}{
		{
			name:    "no history at all",
			records: nil,
			want:    false,
		},
		{
			name:    "one merged PR is ordinary progress",
			records: []IssuePRRecord{churnRecord(1232, 1236, PRStateMerged, now)},
			want:    false,
		},
		{
			name: "two merged PRs on one issue need a human",
			records: []IssuePRRecord{
				churnRecord(1232, 1236, PRStateMerged, now),
				churnRecord(1232, 1295, PRStateMerged, now),
			},
			want: true,
		},
		{
			name:    "one abandoned PR is ordinary attrition",
			records: []IssuePRRecord{churnRecord(1232, 1286, PRStateClosed, now)},
			want:    false,
		},
		{
			name: "two abandoned PRs need a human",
			records: []IssuePRRecord{
				churnRecord(1232, 1286, PRStateClosed, now),
				churnRecord(1232, 1294, PRStateClosed, now),
			},
			want: true,
		},
		{
			name: "open PRs are the claim gate's business, not churn",
			records: []IssuePRRecord{
				churnRecord(1232, 1300, PRStateOpen, now),
				churnRecord(1232, 1301, PRStateOpen, now),
				churnRecord(1232, 1302, PRStateOpen, now),
			},
			want: false,
		},
		{
			name: "one merged and one closed is below both thresholds",
			records: []IssuePRRecord{
				churnRecord(1232, 1236, PRStateMerged, now),
				churnRecord(1232, 1286, PRStateClosed, now),
			},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := churnLedger(t)
			l.RecordPRHistory(tt.records)
			churn, _ := l.Churn("documentation", 1232)
			if got := churn.NeedsHumanTriage(); got != tt.want {
				t.Fatalf("NeedsHumanTriage() = %v, want %v (churn %+v)", got, tt.want, churn)
			}
		})
	}
}

// TestChurnAccumulatesAcrossScans is the mechanism the guard rests on. No
// single scan can see #1232's shape: the settle scan reads a 72-hour window
// and those seven PRs spanned a week. Only a record that survives between
// scans can count them.
func TestChurnAccumulatesAcrossScans(t *testing.T) {
	l := churnLedger(t)
	day := 24 * time.Hour
	now := time.Now()

	// Three scans, days apart, each seeing only what was in its own window.
	l.RecordPRHistory([]IssuePRRecord{churnRecord(1232, 1236, PRStateMerged, now.Add(-6*day))})
	l.RecordPRHistory([]IssuePRRecord{churnRecord(1232, 1286, PRStateClosed, now.Add(-2*day))})
	l.RecordPRHistory([]IssuePRRecord{
		churnRecord(1232, 1294, PRStateClosed, now),
		churnRecord(1232, 1295, PRStateMerged, now),
	})

	churn, ok := l.Churn("documentation", 1232)
	if !ok {
		t.Fatal("Churn() reported nothing for an issue with four observed PRs")
	}
	if len(churn.Merged) != 2 || len(churn.ClosedUnmerged) != 2 {
		t.Fatalf("churn = %d merged / %d closed, want 2 / 2", len(churn.Merged), len(churn.ClosedUnmerged))
	}
	if !churn.NeedsHumanTriage() {
		t.Error("an issue with two merged and two closed PRs must route to a human")
	}
	want := "An issue with 2 merged and 2 closed PRs needs a maintainer to say what is left"
	if churn.Reason() != want {
		t.Errorf("Reason() = %q, want %q", churn.Reason(), want)
	}
	if prs := churn.PRNumbers(); len(prs) != 4 || prs[0] != "#1236" {
		t.Errorf("PRNumbers() = %v, want the four PRs, merged first", prs)
	}
}

// TestChurnHistoryExpires bounds the guard. It stops offering an issue; it
// must not park one forever. Once the churn stops, the records age out and the
// issue is dispatchable again with no operator action.
func TestChurnHistoryExpires(t *testing.T) {
	l := churnLedger(t)
	l.SetChurnHistoryTTL(time.Hour)
	l.RecordPRHistory([]IssuePRRecord{
		churnRecord(1232, 1236, PRStateMerged, time.Now().Add(-2*time.Hour)),
		churnRecord(1232, 1295, PRStateMerged, time.Now().Add(-2*time.Hour)),
	})
	if churn, ok := l.Churn("documentation", 1232); ok && churn.NeedsHumanTriage() {
		t.Fatalf("stale churn still withholds the issue: %+v", churn)
	}
}

// TestChurnHistorySurvivesReconcile separates the two records. Reconcile
// REPLACES the claim map every authoritative cycle — that is what releases an
// issue whose PR closed — and the churn history must not be replaced with it,
// or it could never count anything that already finished.
func TestChurnHistorySurvivesReconcile(t *testing.T) {
	l := churnLedger(t)
	l.RecordPRHistory([]IssuePRRecord{
		churnRecord(1232, 1236, PRStateMerged, time.Now()),
		churnRecord(1232, 1295, PRStateMerged, time.Now()),
	})
	l.Reconcile(nil, true)
	if l.Len() != 0 {
		t.Fatalf("Len() = %d, want 0 — an authoritative empty scan clears claims", l.Len())
	}
	churn, ok := l.Churn("documentation", 1232)
	if !ok || !churn.NeedsHumanTriage() {
		t.Fatalf("churn history was cleared by Reconcile: %+v (ok=%v)", churn, ok)
	}
}

// TestChurnHistoryPersistsAcrossRestart pins the other half of "accumulates":
// the hive restarts, and a guard that forgot on restart would be a guard that
// never fires on a spoke that redeploys weekly.
func TestChurnHistoryPersistsAcrossRestart(t *testing.T) {
	path := t.TempDir() + "/pr-claims.json"
	l := NewClaimLedger(path, testLogger())
	l.RecordPRHistory([]IssuePRRecord{
		churnRecord(1232, 1286, PRStateClosed, time.Now()),
		churnRecord(1232, 1294, PRStateClosed, time.Now()),
	})
	if err := l.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reloaded, err := LoadClaimLedger(path, testLogger())
	if err != nil {
		t.Fatalf("LoadClaimLedger: %v", err)
	}
	churn, ok := reloaded.Churn("documentation", 1232)
	if !ok {
		t.Fatal("reloaded ledger remembers no churn")
	}
	if len(churn.ClosedUnmerged) != 2 || !churn.NeedsHumanTriage() {
		t.Fatalf("reloaded churn = %+v, want two closed PRs and a triage verdict", churn)
	}
}

// TestChurnHistoryKeepsTerminalState pins the one ordering rule: a merged or
// closed pull request cannot reopen, so a late "open" sighting — a retried
// page, a stale persisted ledger — must not demote the outcome and erase the
// count.
func TestChurnHistoryKeepsTerminalState(t *testing.T) {
	l := churnLedger(t)
	now := time.Now()
	l.RecordPRHistory([]IssuePRRecord{churnRecord(1232, 1236, PRStateMerged, now)})
	l.RecordPRHistory([]IssuePRRecord{churnRecord(1232, 1236, PRStateOpen, now.Add(time.Minute))})

	churn, _ := l.Churn("documentation", 1232)
	if len(churn.Merged) != 1 || len(churn.Open) != 0 {
		t.Fatalf("churn = %+v, want the merge to stand", churn)
	}
}

// TestChurnHistoryIsPerIssue pins that churn does not leak between issues: two
// PRs each on their own issue is not one issue with two PRs.
func TestChurnHistoryIsPerIssue(t *testing.T) {
	l := churnLedger(t)
	now := time.Now()
	l.RecordPRHistory([]IssuePRRecord{
		churnRecord(1232, 1236, PRStateMerged, now),
		churnRecord(1233, 1295, PRStateMerged, now),
	})
	for _, issue := range []int{1232, 1233} {
		churn, _ := l.Churn("documentation", issue)
		if churn.NeedsHumanTriage() {
			t.Errorf("issue %d withheld on one merged PR: %+v", issue, churn)
		}
	}
}

// TestFetchClaimScanRecordsClosedAndMergedPRs is the scan half: the settle
// scan's closed-PR listing already contains every abandoned pull request, and
// #7995 stops discarding them. A closed-without-merge PR still claims NOTHING
// — the issue stays released for work — it only counts.
func TestFetchClaimScanRecordsClosedAndMergedPRs(t *testing.T) {
	now := time.Now()
	recent := now.Add(-2 * time.Hour)
	srv := mergedClaimServer(t,
		[]map[string]any{pr(1300, "kubestellar-hive[bot]", "in flight", "Refs #1232")},
		[]map[string]any{
			mergedPR(1236, "kylerankin", "converge fetch sites", "Resolves architect issue #1232", recent, recent),
			mergedPR(1286, "kubestellar-hive[bot]", "superseded", "Fixes #1232", time.Time{}, recent),
			mergedPR(1294, "kubestellar-hive[bot]", "rebase blocked", "Fixes #1232", time.Time{}, recent),
		},
	)
	c := NewClientForTest(srv.URL, "torch-spyre", []string{"spyre-inference"}, testLogger())

	scan, err := c.FetchClaimScan(context.Background(), HiveIdentity{AppLogin: "kubestellar-hive[bot]"})
	if err != nil {
		t.Fatalf("FetchClaimScan: %v", err)
	}
	// The claim set is unchanged by #7995: the open PR's weak reference and
	// the merged PR's (now parseable) closing claim, and nothing from the two
	// abandoned PRs.
	if len(scan.Claims) != 2 {
		t.Fatalf("claims = %+v, want the open reference and the merged claim only", scan.Claims)
	}

	l := churnLedger(t)
	l.RecordPRHistory(scan.History)
	churn, ok := l.Churn("spyre-inference", 1232)
	if !ok {
		t.Fatal("the scan recorded no churn for an issue with four PRs against it")
	}
	if len(churn.Open) != 1 || len(churn.Merged) != 1 || len(churn.ClosedUnmerged) != 2 {
		t.Fatalf("churn = %d open / %d merged / %d closed, want 1 / 1 / 2",
			len(churn.Open), len(churn.Merged), len(churn.ClosedUnmerged))
	}
	if !churn.NeedsHumanTriage() {
		t.Error("two abandoned PRs on one issue must route to a human")
	}
}

func TestFetchClaimScanLooksUpMergedByWhenListOmitsActor(t *testing.T) {
	now := time.Now().UTC()
	recent := now.Add(-time.Hour)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("state") == "closed" {
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{
					"number":     77,
					"title":      "operator merge",
					"body":       "Fixes #700",
					"state":      "closed",
					"head":       map[string]any{"ref": "operator-merge", "sha": "head-77"},
					"user":       map[string]any{"login": "hive-app[bot]"},
					"html_url":   "https://github.com/torch-spyre/spyre-inference/pull/77",
					"updated_at": recent.Format(time.RFC3339),
					"merged_at":  recent.Format(time.RFC3339),
				},
			})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/pulls/77") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number":    77,
				"state":     "closed",
				"merged_at": recent.Format(time.RFC3339),
				"merged_by": map[string]any{"login": "clubanderson"},
			})
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	}))
	t.Cleanup(srv.Close)
	c := NewClientForTest(srv.URL, "torch-spyre", []string{"spyre-inference"}, testLogger())
	var got []PRTerminalObservation
	c.SetPRTerminalObservedHook(func(obs PRTerminalObservation) { got = append(got, obs) })
	if _, err := c.FetchClaimScan(context.Background(), HiveIdentity{AppLogin: "hive-app[bot]"}); err != nil {
		t.Fatalf("FetchClaimScan: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("terminal observations = %+v, want one", got)
	}
	if got[0].Actor != "clubanderson" || got[0].Attribution != "human" {
		t.Fatalf("terminal observation = %+v, want clubanderson/human", got[0])
	}
}

// TestFetchClaimsProjectionUnchanged pins that the churn history rides along
// without changing what the duplicate-PR guard consumes.
func TestFetchClaimsProjectionUnchanged(t *testing.T) {
	now := time.Now()
	srv := mergedClaimServer(t,
		[]map[string]any{pr(423, "kubestellar-hive[bot]", "wip", "Fixes #100")},
		[]map[string]any{mergedPR(500, "kubestellar-hive[bot]", "done", "Fixes #200", now.Add(-time.Hour), now.Add(-time.Hour))},
	)
	c := NewClientForTest(srv.URL, "torch-spyre", []string{"spyre-inference"}, testLogger())

	claims, err := c.FetchClaims(context.Background(), HiveIdentity{AppLogin: "kubestellar-hive[bot]"})
	if err != nil {
		t.Fatalf("FetchClaims: %v", err)
	}
	scan, err := c.FetchClaimScan(context.Background(), HiveIdentity{AppLogin: "kubestellar-hive[bot]"})
	if err != nil {
		t.Fatalf("FetchClaimScan: %v", err)
	}
	if len(claims) != len(scan.Claims) || len(claims) != 2 {
		t.Fatalf("FetchClaims = %+v, FetchClaimScan.Claims = %+v — the projection must be identical", claims, scan.Claims)
	}
}
