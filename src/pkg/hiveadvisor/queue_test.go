package hiveadvisor

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// mkPRs builds n PRs with the given band; opt mutates each one.
func mkPRs(n int, band string, opt func(i int, pr *PRItem)) []PRItem {
	out := make([]PRItem, 0, n)
	for i := 0; i < n; i++ {
		pr := PRItem{Repo: "octo/demo", Number: 100 + i, Band: band, Author: "alice"}
		if opt != nil {
			opt(i, &pr)
		}
		out = append(out, pr)
	}
	return out
}

func mkIssues(n int, band string, opt func(i int, is *IssueItem)) []IssueItem {
	out := make([]IssueItem, 0, n)
	for i := 0; i < n; i++ {
		is := IssueItem{Repo: "octo/demo", Number: 500 + i, Band: band}
		if opt != nil {
			opt(i, &is)
		}
		out = append(out, is)
	}
	return out
}

func ciFailing(check string) func(int, *PRItem) {
	return func(_ int, pr *PRItem) { pr.CIFailing = true; pr.FailingChecks = []string{check} }
}

func hasID(recs []Recommendation, id string) bool {
	for _, r := range recs {
		if r.ID == id {
			return true
		}
	}
	return false
}

func find(t *testing.T, recs []Recommendation, id string) Recommendation {
	t.Helper()
	for _, r := range recs {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("recommendation %s missing from %v", id, ids(recs))
	return Recommendation{}
}

// TestQueueHealthThresholds pins each rule's trigger point one below and at
// the threshold, with the shipped defaults and with a custom threshold.
func TestQueueHealthThresholds(t *testing.T) {
	tests := []struct {
		name  string
		id    string
		th    Thresholds
		below Queue
		at    Queue
	}{
		{
			name:  "blocked ≥ 50% of open PRs",
			id:    "reduce-blocked-prs",
			below: Queue{PRs: append(mkPRs(49, PRBandBlocked, nil), mkPRs(51, "open", nil)...)},
			at:    Queue{PRs: append(mkPRs(50, PRBandBlocked, nil), mkPRs(50, "open", nil)...)},
		},
		{
			name:  "blocked ≥ custom 75%",
			id:    "reduce-blocked-prs",
			th:    Thresholds{BlockedPRPct: 75},
			below: Queue{PRs: append(mkPRs(74, PRBandBlocked, nil), mkPRs(26, "open", nil)...)},
			at:    Queue{PRs: append(mkPRs(75, PRBandBlocked, nil), mkPRs(25, "open", nil)...)},
		},
		{
			name:  "needs-human PRs ≥ 25%",
			id:    "clear-human-gate-prs",
			below: Queue{PRs: append(mkPRs(24, PRBandWaiting, nil), mkPRs(76, "open", nil)...)},
			at:    Queue{PRs: append(mkPRs(25, PRBandWaiting, nil), mkPRs(75, "open", nil)...)},
		},
		{
			name:  "needs-human issues ≥ 30%",
			id:    "unblock-human-issues",
			below: Queue{Issues: append(mkIssues(29, IssueBandWait, nil), mkIssues(71, "ready", nil)...)},
			at:    Queue{Issues: append(mkIssues(30, IssueBandWait, nil), mkIssues(70, "ready", nil)...)},
		},
		{
			name:  "confirm & close > 0",
			id:    "confirm-and-close",
			below: Queue{Issues: mkIssues(10, "ready", nil)},
			at:    Queue{Issues: append(mkIssues(1, IssueBandDone, nil), mkIssues(9, "ready", nil)...)},
		},
		{
			name:  "stale blocked PRs ≥ 10",
			id:    "review-stale-prs",
			below: Queue{PRs: append(mkPRs(9, PRBandBlocked, func(_ int, pr *PRItem) { pr.Stale = true }), mkPRs(91, "open", nil)...)},
			at:    Queue{PRs: append(mkPRs(10, PRBandBlocked, func(_ int, pr *PRItem) { pr.Stale = true }), mkPRs(90, "open", nil)...)},
		},
		{
			name:  "stale blocked PRs ≥ custom 3",
			id:    "review-stale-prs",
			th:    Thresholds{StaleBlockedPRs: 3},
			below: Queue{PRs: append(mkPRs(2, PRBandBlocked, func(_ int, pr *PRItem) { pr.Stale = true }), mkPRs(91, "open", nil)...)},
			at:    Queue{PRs: append(mkPRs(3, PRBandBlocked, func(_ int, pr *PRItem) { pr.Stale = true }), mkPRs(90, "open", nil)...)},
		},
		{
			name: "one lane ≥ 50% of blocked PRs",
			id:   "throttle-lane",
			below: Queue{PRs: mkPRs(10, PRBandBlocked, func(i int, pr *PRItem) {
				pr.Lane = fmt.Sprintf("lane-%d", i%3) // 4/3/3 split, top = 40%
			})},
			at: Queue{PRs: mkPRs(10, PRBandBlocked, func(i int, pr *PRItem) {
				pr.Lane = fmt.Sprintf("lane-%d", i%2) // 5/5 split, top = 50%
			})},
		},
		{
			name: "one check ≥ 50% of CI-blocked PRs",
			id:   "fix-failing-check",
			below: Queue{PRs: mkPRs(10, PRBandBlocked, func(i int, pr *PRItem) {
				ciFailing(fmt.Sprintf("check-%d", i%3))(i, pr)
			})},
			at: Queue{PRs: mkPRs(10, PRBandBlocked, func(i int, pr *PRItem) {
				ciFailing(fmt.Sprintf("check-%d", i%2))(i, pr)
			})},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if hasID(queueCandidates(tt.below, tt.th), tt.id) {
				t.Fatalf("%s fired below threshold", tt.id)
			}
			if !hasID(queueCandidates(tt.at, tt.th), tt.id) {
				t.Fatalf("%s did not fire at threshold", tt.id)
			}
		})
	}
}

func TestZeroThresholdsResolveToDefaults(t *testing.T) {
	if got, want := (Thresholds{}).normalized(), DefaultThresholds(); got != want {
		t.Fatalf("normalized zero thresholds = %+v, want %+v", got, want)
	}
	custom := Thresholds{BlockedPRPct: 60}.normalized()
	if custom.BlockedPRPct != 60 || custom.NeedsHumanPRPct != 25 {
		t.Fatalf("partial override = %+v", custom)
	}
}

// TestQueueHealthFiresInEveryMode: the same blocked pile produces the same
// leading advice whether the governor calls the hive IDLE, QUIET, BUSY or SURGE.
func TestQueueHealthFiresInEveryMode(t *testing.T) {
	q := Queue{PRs: append(mkPRs(70, PRBandBlocked, ciFailing("go-security-analysis")), mkPRs(23, "open", nil)...)}
	for _, mode := range []string{"idle", "quiet", "busy", "surge", "???"} {
		t.Run(mode, func(t *testing.T) {
			got := Recommend(Request{Now: time.Unix(1, 0), TopN: 10, Signals: Signals{Mode: mode, NoCadenceAgentCount: 1, HoldCount: 2, QueueIssues: 5, QueuePRs: 3, Queue: q}})
			if !hasID(got.Recommendations, "reduce-blocked-prs") || !hasID(got.Recommendations, "fix-failing-check") {
				t.Fatalf("mode %s = %v, want queue-health rules present", mode, ids(got.Recommendations))
			}
			// SURGE's own "Fix merge blockers first" (score 100) may still lead;
			// everywhere else the blocked pile outranks the mode-gated advice.
			if mode != "surge" && got.Recommendations[0].ID != "reduce-blocked-prs" {
				t.Fatalf("mode %s top = %v, want reduce-blocked-prs", mode, ids(got.Recommendations))
			}
		})
	}
}

// TestIssueScenarioLeadsWithBlockedPRs replays #9103's numbers in IDLE: 93
// PRs / 70 blocked, 295 issues / 117 needs-human / 92 confirm-&-close, one
// agent without cadence.
func TestIssueScenarioLeadsWithBlockedPRs(t *testing.T) {
	prs := mkPRs(70, PRBandBlocked, func(i int, pr *PRItem) {
		pr.Lane = "scanner"
		pr.IdleDays = 70 - i
		pr.AgeDays = 80 - i
		switch {
		case i < 41:
			ciFailing("go-security-analysis")(i, pr)
		case i < 60:
			pr.Conflict = true
		default:
			pr.VerdictBlocked = true
			pr.VerdictReason = "base protection"
		}
	})
	prs = append(prs, mkPRs(23, "in-review", nil)...)
	issues := mkIssues(117, IssueBandWait, func(i int, is *IssueItem) {
		if i%2 == 0 {
			is.NeedsDecision = true
		} else {
			is.Blocked = true
		}
	})
	issues = append(issues, mkIssues(92, IssueBandDone, nil)...)
	issues = append(issues, mkIssues(86, "ready", nil)...)
	q := Queue{
		PRs: prs, Issues: issues, StaleDays: 14,
		PRBands:    []BandRule{{Key: "blocked", Label: "Blocked", Rule: "blocked sweep verdict, merge conflicts, or failing CI"}},
		IssueBands: []BandRule{{Key: "waiting", Label: "Needs human", Rule: "labelled blocked, needs-decision"}, {Key: "done", Label: "Confirm & close", Rule: "an agent applied hive/likely-done"}},
	}
	got := Recommend(Request{Now: time.Unix(1, 0), TopN: 10, Signals: Signals{Mode: "idle", NoCadenceAgentCount: 1, QueuePRs: 0, QueueIssues: 12, Queue: q}})

	top := got.Recommendations[0]
	if top.ID != "reduce-blocked-prs" {
		t.Fatalf("top = %v", ids(got.Recommendations))
	}
	for _, want := range []string{"70 of 93 open PRs (75%)", "41 of 70 fail check `go-security-analysis`", "fix the check before touching cadence"} {
		if !strings.Contains(top.Rationale, want) {
			t.Fatalf("rationale %q missing %q", top.Rationale, want)
		}
	}
	if len(top.Links) != 2 || top.Links[0].URL != "/api/overview/prs.csv?band=blocked" || top.Links[1].URL != "/api/overview/prs.csv?band=blocked&repo=octo%2Fdemo" {
		t.Fatalf("links = %+v", top.Links)
	}
	if len(top.Items) != 5 || top.Items[0].Number != 100 || top.Items[0].Note != "fails go-security-analysis" {
		t.Fatalf("items = %+v", top.Items)
	}
	if !reflect.DeepEqual(top.Bands, []string{"pr/blocked"}) {
		t.Fatalf("bands = %v", top.Bands)
	}

	fix := find(t, got.Recommendations, "fix-failing-check")
	if fix.Title != "Fix check `go-security-analysis` first" || !strings.Contains(fix.Rationale, "41 of 41 CI-blocked PRs (100%)") {
		t.Fatalf("fix-failing-check = %+v", fix)
	}
	lane := find(t, got.Recommendations, "throttle-lane")
	if lane.Title != "Throttle lane `scanner`" {
		t.Fatalf("throttle-lane = %+v", lane)
	}
	human := find(t, got.Recommendations, "unblock-human-issues")
	if !strings.Contains(human.Rationale, "117 of 295 open issues (39%) need a human: 59 needs-decision, 58 blocked, 0 discussing") {
		t.Fatalf("unblock-human-issues = %q", human.Rationale)
	}
	done := find(t, got.Recommendations, "confirm-and-close")
	if !strings.Contains(done.Rationale, "92 issues are agent-marked done; confirming them shrinks the backlog by 31%") || done.Links[0].URL != "/api/overview/issues.csv?band=done" {
		t.Fatalf("confirm-and-close = %+v", done)
	}
	if hasID(got.Recommendations, "clear-human-gate-prs") {
		t.Fatal("clear-human-gate-prs fired with zero waiting PRs")
	}
	if !hasID(got.Recommendations, "add-agent-cadence") {
		t.Fatal("mode-gated rule dropped when queue rules fire")
	}

	wantBands := []BandRule{
		{Kind: "pr", Key: "blocked", Label: "Blocked", Rule: "blocked sweep verdict, merge conflicts, or failing CI"},
		{Kind: "issue", Key: "waiting", Label: "Needs human", Rule: "labelled blocked, needs-decision"},
		{Kind: "issue", Key: "done", Label: "Confirm & close", Rule: "an agent applied hive/likely-done"},
	}
	if !reflect.DeepEqual(got.Bands, wantBands) {
		t.Fatalf("bands = %+v", got.Bands)
	}
	if got.Counts != (Counts{GovernorIssues: 12, GovernorPRs: 0, OverviewIssues: 295, OverviewPRs: 93}) {
		t.Fatalf("counts = %+v", got.Counts)
	}
}

func TestBlockedFirstActionByDominantCause(t *testing.T) {
	tests := []struct {
		name string
		opt  func(int, *PRItem)
		want string
	}{
		{"conflicts dominate", func(i int, pr *PRItem) {
			pr.Conflict = true
			if i == 0 {
				ciFailing("lint")(i, pr)
			}
		}, "4 of 4 have merge conflicts — rebase or close the oldest."},
		{"verdict dominates", func(i int, pr *PRItem) { pr.VerdictBlocked = true; pr.VerdictReason = "base protection" }, "4 of 4 carry a blocked sweep verdict (top reason: base protection)"},
		{"no recorded cause", nil, "without a recorded cause"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recs := queueCandidates(Queue{PRs: mkPRs(4, PRBandBlocked, tt.opt)}, Thresholds{})
			if r := find(t, recs, "reduce-blocked-prs"); !strings.Contains(r.Rationale, tt.want) {
				t.Fatalf("rationale %q missing %q", r.Rationale, tt.want)
			}
		})
	}
}

func TestSummarizePRsSplitsWaitingAndOrdersOldestFirst(t *testing.T) {
	prs := []PRItem{
		{Repo: "a/b", Number: 1, Band: PRBandWaiting, Held: true, IdleDays: 3},
		{Repo: "a/b", Number: 2, Band: PRBandWaiting, NeedsHuman: true, IdleDays: 9},
		{Repo: "a/b", Number: 3, Band: PRBandWaiting, NeedsDecision: true, Held: true, IdleDays: 9},
		{Repo: "a/b", Number: 4, Band: "open"},
	}
	recs := queueCandidates(Queue{PRs: prs}, Thresholds{})
	r := find(t, recs, "clear-human-gate-prs")
	if !strings.Contains(r.Rationale, "3 of 4 open PRs (75%) wait on a human: 2 held, 1 labelled needs-human, 1 needs-decision") {
		t.Fatalf("rationale = %q", r.Rationale)
	}
	gotOrder := []int{r.Items[0].Number, r.Items[1].Number, r.Items[2].Number}
	if !reflect.DeepEqual(gotOrder, []int{2, 3, 1}) {
		t.Fatalf("items ordered %v, want idle-desc then number", gotOrder)
	}
	if r.Items[2].Note != "held" || r.Items[1].Note != "held, needs-decision" {
		t.Fatalf("notes = %+v", r.Items)
	}
}

// TestFrozenEpochRefreshesNumbers: inside the epoch the recommendation IDs
// and order stay, but every number is recomputed; a rule that stopped firing
// is marked cleared instead of repeating last week's count.
func TestFrozenEpochRefreshesNumbers(t *testing.T) {
	start := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	week1 := Queue{PRs: append(mkPRs(70, PRBandBlocked, ciFailing("ci/test")), mkPRs(23, "open", nil)...)}
	first := Recommend(Request{Now: start, Signals: Signals{Mode: "idle", NoCadenceAgentCount: 1, Queue: week1}})
	if got := ids(first.Recommendations); !reflect.DeepEqual(got, []string{"reduce-blocked-prs", "fix-failing-check", "add-agent-cadence"}) {
		t.Fatalf("first = %v", got)
	}

	// Two days later: 20 blocked, none failing the same check twice, cadence fixed.
	week1b := Queue{PRs: append(mkPRs(20, PRBandBlocked, func(i int, pr *PRItem) { ciFailing(fmt.Sprintf("c%d", i))(i, pr) }), mkPRs(20, "open", nil)...)}
	second := Recommend(Request{Now: start.Add(48 * time.Hour), Signals: Signals{Mode: "idle", Queue: week1b}, Previous: &first.Epoch})
	if !second.Frozen {
		t.Fatal("same mode inside the epoch must stay frozen")
	}
	if got := ids(second.Recommendations); !reflect.DeepEqual(got, ids(first.Recommendations)) {
		t.Fatalf("frozen ids changed: %v", got)
	}
	blocked := second.Recommendations[0]
	if blocked.Cleared || !strings.Contains(blocked.Rationale, "20 of 40 open PRs (50%)") {
		t.Fatalf("frozen recommendation kept stale numbers: %+v", blocked)
	}
	if !second.Recommendations[1].Cleared || len(second.Recommendations[1].Signals) != 0 || second.Recommendations[1].Title != "Fix check `ci/test` first" {
		t.Fatalf("fix-failing-check should be cleared: %+v", second.Recommendations[1])
	}
	if !second.Recommendations[2].Cleared {
		t.Fatalf("add-agent-cadence should be cleared: %+v", second.Recommendations[2])
	}
	if second.Counts.OverviewPRs != 40 {
		t.Fatalf("counts frozen: %+v", second.Counts)
	}
	if len(second.Bands) != 0 {
		// The test queue carries no band rule text; bands must reflect the current queue, not the frozen one.
		t.Fatalf("bands = %+v", second.Bands)
	}
	// The persisted epoch carries the refreshed bodies too, so a restart does
	// not resurrect the stale numbers.
	if second.Epoch.Recommendations[0].Rationale != blocked.Rationale {
		t.Fatal("epoch recommendations not refreshed")
	}

	// New rules that start firing mid-epoch wait for the next epoch.
	week1c := Queue{Issues: mkIssues(5, IssueBandDone, nil)}
	third := Recommend(Request{Now: start.Add(72 * time.Hour), Signals: Signals{Mode: "idle", Queue: week1c}, Previous: &second.Epoch})
	if !third.Frozen || hasID(third.Recommendations, "confirm-and-close") {
		t.Fatalf("mid-epoch rule leaked into the frozen list: %v", ids(third.Recommendations))
	}
	fourth := Recommend(Request{Now: start.Add(EpochLength + time.Second), Signals: Signals{Mode: "idle", Queue: week1c}, Previous: &second.Epoch})
	if fourth.Frozen || !hasID(fourth.Recommendations, "confirm-and-close") {
		t.Fatalf("expired epoch should refresh: %v", ids(fourth.Recommendations))
	}
}

// TestPlaceholderEpochDoesNotFreeze: a "keep observing" epoch must not hide
// a week of real advice that appears the next day.
func TestPlaceholderEpochDoesNotFreeze(t *testing.T) {
	start := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	first := Recommend(Request{Now: start, Signals: Signals{Mode: "idle", RepoCount: 5}})
	if got := ids(first.Recommendations); !reflect.DeepEqual(got, []string{placeholderID}) {
		t.Fatalf("first = %v", got)
	}
	q := Queue{PRs: append(mkPRs(3, PRBandBlocked, nil), mkPRs(1, "open", nil)...)}
	second := Recommend(Request{Now: start.Add(24 * time.Hour), Signals: Signals{Mode: "idle", RepoCount: 5, Queue: q}, Previous: &first.Epoch})
	if second.Frozen || second.Recommendations[0].ID != "reduce-blocked-prs" {
		t.Fatalf("placeholder epoch froze real advice out: frozen=%v ids=%v", second.Frozen, ids(second.Recommendations))
	}
	still := Recommend(Request{Now: start.Add(24 * time.Hour), Signals: Signals{Mode: "idle", RepoCount: 5}, Previous: &first.Epoch})
	if !still.Frozen {
		t.Fatal("placeholder epoch with nothing new should still freeze")
	}
}

func TestCopyRecommendationsIsDeep(t *testing.T) {
	in := []Recommendation{{ID: "x", Links: []Link{{Label: "a"}}, Items: []Item{{Number: 1}}, Bands: []string{"pr/blocked"}}}
	out := copyRecommendations(in)
	out[0].Links[0].Label, out[0].Items[0].Number, out[0].Bands[0] = "b", 2, "issue/done"
	if in[0].Links[0].Label != "a" || in[0].Items[0].Number != 1 || in[0].Bands[0] != "pr/blocked" {
		t.Fatalf("copy aliased input: %+v", in[0])
	}
}
