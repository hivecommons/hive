package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// Part D1 of the your-turn signal (hivecommons/hive#11507, sub-issue #11567):
// the Overview equation, charts and export read the server's ownership
// decision. The fixture is the spec's mixed scenario at L5.

var overviewFixtureNow11567 = ownershipNow11565

func fixtureTime11567() time.Time { return overviewFixtureNow11567.Add(-time.Hour) }

func mixedOverviewStatus11567() *StatusPayload {
	at := fixtureTime11567()
	issue := func(number int, labels ...string) github.Issue {
		return github.Issue{Repo: "o/a", Number: number, Title: "issue", Labels: labels, CreatedAt: at, UpdatedAt: at}
	}
	pr := func(number int, mutate func(*github.PullRequest)) FrontendPR {
		p := github.PullRequest{Repo: "o/a", Number: number, Title: "pr", Author: "alice", CreatedAt: at, UpdatedAt: at}
		if mutate != nil {
			mutate(&p)
		}
		return FrontendPR{PullRequest: p}
	}
	hiveHeld := func(number int, sources ...string) FrontendPR {
		return pr(number, func(p *github.PullRequest) {
			p.HiveAgent = "scanner"
			p.Labels = []string{"hold"}
			p.HoldSources = sources
		})
	}
	return &StatusPayload{HiveID: "hive-test", Repos: []FrontendRepo{{
		Name: "a", Full: "o/a", Issues: 9, PRs: 8,
		ActionableIssues: []any{
			issue(1),                      // plain backlog: actionable
			issue(2, "agent/strategist"),  // unacknowledged agent proposal at L5: TRIAGE
			issue(9, "hive/already-done"), // verified done, could not close: CLOSE
		},
		HeldIssues: []any{
			github.HoldItem{Repo: "o/a", Number: 6, Type: "issue", Labels: []string{"hold"}, HoldSources: []string{github.HoldSourcePerson}, CreatedAt: at},
			github.HoldItem{Repo: "o/a", Number: 7, Type: "issue", Labels: []string{"hold"}, HoldSources: []string{github.HoldSourceSHAHold}, CreatedAt: at},
		},
		NonActionableIssues: []any{
			github.RepoWorkIssue{Repo: "o/a", Number: 3, Title: "untrusted", Bucket: repoWorkBucketReporterTriage, CreatedAt: at, UpdatedAt: at},
			github.RepoWorkIssue{Repo: "o/a", Number: 4, Title: "parked", Labels: []string{"needs-decision"}, Bucket: repoWorkBucketFiltered, Reason: "needs-decision", CreatedAt: at, UpdatedAt: at},
			github.RepoWorkIssue{Repo: "o/a", Number: 5, Title: "confirm", Labels: []string{"needs-reporter-confirmation"}, Bucket: repoWorkBucketFiltered, Reason: "needs-reporter-confirmation", CreatedAt: at, UpdatedAt: at},
			github.RepoWorkIssue{Repo: "o/a", Number: 8, Title: "exempt", Labels: []string{"no-ai"}, Bucket: repoWorkBucketFiltered, Reason: "Exempt label", CreatedAt: at, UpdatedAt: at},
		},
		OpenPrs: []any{
			pr(10, nil),
			pr(13, func(p *github.PullRequest) { p.HiveAgent = "scanner"; p.Draft = true }),
			pr(14, func(p *github.PullRequest) { p.CIStatus = "failing"; p.FailingChecks = []string{"ci/test"} }),
			pr(15, func(p *github.PullRequest) { p.HiveAgent = "scanner"; p.Labels = []string{"needs-human"} }),
		},
		HeldPrs: []any{
			hiveHeld(11, github.HoldSourceLevelGate),
			hiveHeld(12, github.HoldSourcePerson, github.HoldSourceLevelGate),
			hiveHeld(16, github.HoldSourceDashboardPause),
		},
		WorkBreakdown: &github.RepoWorkBreakdown{
			Issues: github.RepoIssueBreakdown{Actionable: 3, Hold: 2, Filtered: 3, ReporterTriage: 1, NeedsDecision: 1, ReporterConfirmation: 1, Exempt: 1},
			PRs:    github.RepoPRBreakdown{Actionable: 3, Hold: 3, Draft: 1, Filtered: 1},
		},
	}}}
}

func decideMixedOverview11567(t *testing.T) (*Server, *StatusPayload) {
	t.Helper()
	status := mixedOverviewStatus11567()
	s := newTestServer()
	s.deps = &Dependencies{Config: &config.Config{HiveID: "hive-test"}}
	decideRepoOwnership(status.Repos, config.DashboardIssueBandsConfig{}, ownershipEnv11565(5))
	return s, status
}

func eqTermCounts11567(eq *FrontendActionableEquation) map[string]int {
	out := map[string]int{}
	for _, term := range eq.Terms {
		out[term.Key] = term.Count
	}
	return out
}

func wireRows11567(t *testing.T, resp *StatusPayload) map[string]map[int]map[string]any {
	t.Helper()
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Repos []map[string]json.RawMessage `json:"repos"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	out := map[string]map[int]map[string]any{"issues": {}, "prs": {}}
	for list, kind := range map[string]string{"actionableIssues": "issues", "heldIssues": "issues", "nonActionableIssues": "issues", "openPrs": "prs", "heldPrs": "prs"} {
		var rows []map[string]any
		if err := json.Unmarshal(wire.Repos[0][list], &rows); err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			out[kind][int(row["number"].(float64))] = row
		}
	}
	return out
}

// The equation is an exact partition: each open item counts in the first term
// it matches, and each kind's terms sum to its open count.
func TestOverviewEquationIsExactPartitionOnMixedFixture11567(t *testing.T) {
	s, status := decideMixedOverview11567(t)
	resp := s.statusWithOverviewBands(status, overviewFixtureNow11567)
	now := resp.ActionableNow

	for name, want := range map[string]struct {
		eq    *FrontendActionableEquation
		terms map[string]int
		open  int
	}{
		"issues": {now.IssueEquation, map[string]int{"your_turn": 4, "waiting_elsewhere": 2, "held": 1, "outside": 1}, 9},
		"prs":    {now.PREquation, map[string]int{"your_turn": 1, "waiting_elsewhere": 3, "blocked": 1, "outside": 2}, 8},
	} {
		got := eqTermCounts11567(want.eq)
		sum := want.eq.Result
		for key, count := range got {
			sum += count
			if want.terms[key] != count {
				t.Fatalf("%s term %s = %d, want %d (%s)", name, key, count, want.terms[key], want.eq.Text)
			}
		}
		for key, count := range want.terms {
			if got[key] != count {
				t.Fatalf("%s term %s = %d, want %d (%s)", name, key, got[key], count, want.eq.Text)
			}
		}
		if want.eq.Open != want.open || sum != want.open {
			t.Fatalf("%s equation sums to %d over %d open, want %d: %s", name, sum, want.eq.Open, want.open, want.eq.Text)
		}
		if _, ok := got["other"]; ok {
			t.Fatalf("%s equation has an unclaimed remainder: %s", name, want.eq.Text)
		}
	}
	if now.IssueEquation.Result != 1 || now.PREquation.Result != 1 || now.Total != 2 || now.Issues != 1 || now.PRs != 1 {
		t.Fatalf("actionable = %+v, want one issue and one PR", now)
	}

	// The combined equation adds the two, in the order the first-match rule
	// applies, with the actionable remainder last.
	var order []string
	for _, term := range now.Equation.Terms {
		order = append(order, term.Key)
	}
	if want := []string{"your_turn", "waiting_elsewhere", "held", "blocked", "outside", "actionable"}; strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("combined term order = %v, want %v", order, want)
	}
	if want := "9 issues + 8 PRs = 5 Your turn + 5 Waiting on someone else + 1 held + 1 blocked + 3 outside + 2 actionable"; now.Equation.Text != want {
		t.Fatalf("equation text = %q, want %q", now.Equation.Text, want)
	}
	if now.Equation.TotalOpen != 17 {
		t.Fatalf("total open = %d, want 17", now.Equation.TotalOpen)
	}

	// Each item counts in exactly the term the spec names for it.
	rows := wireRows11567(t, resp)
	for number, want := range map[int]string{
		1: "actionable", 2: "your_turn", 3: "your_turn", 4: "your_turn", 5: "waiting_elsewhere",
		6: "waiting_elsewhere", 7: "held", 8: "outside", 9: "your_turn",
	} {
		if got := rows["issues"][number]["overview_term"]; got != want {
			t.Errorf("issue #%d term = %v, want %s", number, got, want)
		}
	}
	for number, want := range map[int]string{
		10: "actionable", 11: "your_turn", 12: "waiting_elsewhere", 13: "outside", 14: "blocked", 15: "waiting_elsewhere", 16: "waiting_elsewhere",
	} {
		if got := rows["prs"][number]["overview_term"]; got != want {
			t.Errorf("PR #%d term = %v, want %s", number, got, want)
		}
	}
}

// The Your turn term counts exactly the inbox's N plus M, and parked issues
// count there and not in Outside.
func TestOverviewYourTurnTermEqualsInboxCount11567(t *testing.T) {
	s, status := decideMixedOverview11567(t)
	now := s.statusWithOverviewBands(status, overviewFixtureNow11567).ActionableNow
	var n, m int
	for _, d := range status.Repos[0].Ownership {
		switch {
		case d.YourTurn && d.Verb == OwnershipVerbClose:
			m++
		case d.YourTurn:
			n++
		}
	}
	if n != 4 || m != 1 {
		t.Fatalf("inbox N/M = %d/%d, want 4/1", n, m)
	}
	if got := overviewTermCount(now.Equation, "your_turn"); got != n+m {
		t.Fatalf("Your turn term = %d, want inbox N+M = %d", got, n+m)
	}
	outside := overviewTerm(now.IssueEquation, "outside")
	if outside == nil || outside.Count != 1 {
		t.Fatalf("issue outside = %+v, want only the exempt issue", outside)
	}
	for _, row := range outside.Breakdown {
		if row.Key == "needs-decision" && row.Count != 0 {
			t.Fatalf("the parked needs-decision issue still counts in Outside: %+v", row)
		}
	}
	for _, term := range now.Equation.Terms {
		if strings.Contains(strings.ToLower(term.Label), "needs-human") || term.Key == "blocked_needs_human" {
			t.Fatalf("equation still has a blocked/needs-human term: %+v", term)
		}
	}
	if strings.Contains(now.Equation.Text, "needs-human") {
		t.Fatalf("equation text still says needs-human: %s", now.Equation.Text)
	}
	your := overviewTerm(now.Equation, "your_turn")
	if your == nil || len(your.Breakdown) == 0 {
		t.Fatalf("Your turn term carries no breakdown: %+v", your)
	}
	byCode := map[string]int{}
	for _, row := range your.Breakdown {
		byCode[row.Key] += row.Count
	}
	for code, want := range map[string]int{
		"your-turn-" + OwnershipReasonAgentFiledUnacknowledged: 1,
		"your-turn-" + OwnershipReasonUntrustedReporter:        1,
		"your-turn-" + OwnershipReasonParkedNeedsDecision:      1,
		"your-turn-" + OwnershipReasonVerifiedDone:             1,
		"your-turn-" + OwnershipReasonLevelGateHold:            1,
	} {
		if byCode[code] != want {
			t.Fatalf("Your turn breakdown %s = %d, want %d (%+v)", code, byCode[code], want, your.Breakdown)
		}
	}
}

// Every shape of the charts plots each item in exactly one slice, taken from
// the server decision.
func TestOverviewChartSlicesComeFromTheDecision11567(t *testing.T) {
	s, status := decideMixedOverview11567(t)
	resp := s.statusWithOverviewBands(status, overviewFixtureNow11567)
	rows := wireRows11567(t, resp)

	issueSlices := map[int]string{1: "ready", 2: "your-turn", 3: "your-turn", 4: "your-turn", 5: "waiting-elsewhere", 6: "waiting-elsewhere", 7: "ready", 9: "to-close"}
	for number, want := range issueSlices {
		if got := rows["issues"][number]["chart_slice"]; got != want {
			t.Errorf("issue #%d slice = %v, want %s", number, got, want)
		}
	}
	if _, plotted := rows["issues"][8]["chart_slice"]; plotted {
		t.Errorf("the exempt non-actionable issue is plotted: %v", rows["issues"][8]["chart_slice"])
	}
	prSlices := map[int]string{10: "open", 11: "your-turn", 12: "waiting-elsewhere", 13: "draft", 14: "blocked", 15: "waiting-elsewhere", 16: "waiting-elsewhere"}
	for number, want := range prSlices {
		if got := rows["prs"][number]["chart_slice"]; got != want {
			t.Errorf("PR #%d slice = %v, want %s", number, got, want)
		}
	}

	// Slice counts are the server's; every plotted row is counted once.
	for kind, specs := range map[string][]BandSpec{"issues": resp.OverviewSlices.Issues, "prs": resp.OverviewSlices.PRs} {
		bySlice := map[string]int{}
		for _, row := range rows[kind] {
			if slice, ok := row["chart_slice"].(string); ok {
				bySlice[slice]++
			}
		}
		total := 0
		for _, spec := range specs {
			if spec.Count != bySlice[spec.Key] {
				t.Errorf("%s slice %s count = %d, rows carrying it = %d", kind, spec.Key, spec.Count, bySlice[spec.Key])
			}
			total += spec.Count
			delete(bySlice, spec.Key)
		}
		if len(bySlice) != 0 {
			t.Errorf("%s rows sit in slices the taxonomy does not list: %v", kind, bySlice)
		}
		plotted := 0
		for _, row := range rows[kind] {
			if _, ok := row["chart_slice"]; ok {
				plotted++
			}
		}
		if total != plotted {
			t.Errorf("%s slices total %d, plotted rows %d", kind, total, plotted)
		}
	}

	// The two Your turn slices equal the inbox's N and the two To close
	// slices its M.
	count := func(specs []BandSpec, key string) int {
		for _, spec := range specs {
			if spec.Key == key {
				return spec.Count
			}
		}
		return -1
	}
	if n := count(resp.OverviewSlices.Issues, "your-turn") + count(resp.OverviewSlices.PRs, "your-turn"); n != 4 {
		t.Errorf("Your turn slices = %d, want inbox N = 4", n)
	}
	if m := count(resp.OverviewSlices.Issues, "to-close") + count(resp.OverviewSlices.PRs, "to-close"); m != 1 {
		t.Errorf("To close slices = %d, want inbox M = 1", m)
	}

	// No slice for the bands the cards keep: Needs human, Needs triage,
	// Confirm & close.
	for kind, specs := range map[string][]BandSpec{"issues": resp.OverviewSlices.Issues, "prs": resp.OverviewSlices.PRs} {
		for _, spec := range specs {
			switch spec.Key {
			case "waiting", "agent-filed", "done":
				t.Errorf("%s chart still has the %s slice", kind, spec.Key)
			}
			for _, retired := range []string{"Needs human", "Needs triage", "Confirm & close"} {
				if spec.Label == retired {
					t.Errorf("%s chart still labels a slice %q", kind, retired)
				}
			}
		}
	}
	// The cards still show their bands.
	if len(resp.OverviewBands.Issues) != len(issueBandOrder) || len(resp.OverviewBands.PRs) != len(prBandOrder) {
		t.Fatalf("overview_bands changed: %d issue / %d PR bands", len(resp.OverviewBands.Issues), len(resp.OverviewBands.PRs))
	}
}

// A band-less non-actionable row exports as band outside with its bucket, and
// every row carries its decision.
func TestOverviewExportIncludesNonActionableRowsWithDecision11567(t *testing.T) {
	s, status := decideMixedOverview11567(t)
	cfg := config.DashboardIssueBandsConfig{}
	items := overviewIssueItems(status, cfg, overviewFilters{}, overviewFixtureNow11567, s.selfAuthorizationHoldActiveForRepo)
	byNumber := map[int]overviewIssueItem{}
	for _, item := range items {
		byNumber[item.issue.Number] = item
	}
	if len(items) != 9 {
		t.Fatalf("overviewIssueItems returned %d rows, want all 9 open issues", len(items))
	}
	for number, want := range map[int]struct {
		band, bucket, verb string
		nonActionable      bool
	}{
		1: {"ready", "", "", false},
		3: {"outside", repoWorkBucketReporterTriage, OwnershipVerbAdmit, true},
		4: {"waiting", repoWorkBucketFiltered, OwnershipVerbDecide, true},
		5: {"outside", repoWorkBucketFiltered, "", true},
		8: {"outside", repoWorkBucketFiltered, "", true},
		9: {"done", "", OwnershipVerbClose, false},
	} {
		item := byNumber[number]
		verb := ""
		if item.ownership != nil {
			verb = item.ownership.Verb
		}
		if item.info.Band != want.band || item.bucket != want.bucket || verb != want.verb || item.nonActionable != want.nonActionable {
			t.Errorf("issue #%d = band %q bucket %q verb %q nonActionable %v, want %+v", number, item.info.Band, item.bucket, verb, item.nonActionable, want)
		}
	}

	rows, specs := s.overviewRows(status, overviewKindIssues, cfg, overviewFilters{}, overviewFixtureNow11567)
	if len(rows) != 9 {
		t.Fatalf("export has %d issue rows, want 9", len(rows))
	}
	var outside *BandSpec
	for i := range specs {
		if specs[i].Key == overviewBandOutside {
			outside = &specs[i]
		}
	}
	if outside == nil || outside.Count != 3 {
		t.Fatalf("export bands = %+v, want an outside band with the 3 band-less rows", specs)
	}
	cells := map[int]map[string]any{}
	for _, raw := range rows {
		row := raw.(map[string]any)
		cells[row["number"].(int)] = row
	}
	if r := cells[3]; r["band"] != "outside" || r["bucket"] != repoWorkBucketReporterTriage || r["your_turn"] != true || r["verb"] != OwnershipVerbAdmit || r["reason_code"] != OwnershipReasonUntrustedReporter || r["reason"] == "" {
		t.Errorf("untrusted reporter row = %+v", r)
	}
	if r := cells[5]; r["band"] != "outside" || r["your_turn"] != false || r["waiting_elsewhere"] != true {
		t.Errorf("reporter confirmation row = %+v", r)
	}
	if r := cells[8]; r["band"] != "outside" || r["your_turn"] != false || r["verb"] != "" || r["waiting_elsewhere"] != false {
		t.Errorf("exempt row = %+v", r)
	}
	if r := cells[4]; r["band"] != issueBandSpec("waiting", cfg).Label || r["verb"] != OwnershipVerbDecide || r["reason_code"] != OwnershipReasonParkedNeedsDecision {
		t.Errorf("parked row = %+v", r)
	}
	for _, column := range []string{"your_turn", "verb", "reason_code", "reason", "bucket", "slice"} {
		if !containsString(overviewCSVColumns(overviewKindIssues), column) {
			t.Errorf("issue export columns lack %s", column)
		}
	}
	for _, column := range []string{"your_turn", "verb", "reason_code", "reason", "slice"} {
		if !containsString(overviewCSVColumns(overviewKindPRs), column) {
			t.Errorf("PR export columns lack %s", column)
		}
	}
	prRows, _ := s.overviewRows(status, overviewKindPRs, cfg, overviewFilters{}, overviewFixtureNow11567)
	for _, raw := range prRows {
		row := raw.(map[string]any)
		if row["number"] == 11 && (row["verb"] != OwnershipVerbReview || row["reason_code"] != OwnershipReasonLevelGateHold || row["your_turn"] != true) {
			t.Errorf("level-held PR row = %+v", row)
		}
	}
}

// A slice's export link returns exactly the rows the slice counts, through
// the HTTP handler.
func TestOverviewExportSliceFilterMatchesChartSlice11567(t *testing.T) {
	s, status := decideMixedOverview11567(t)
	s.status = status
	get := func(path string) []map[string]any {
		t.Helper()
		rr := httptest.NewRecorder()
		s.handleOverviewIssuesJSON(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", path, rr.Code, rr.Body.String())
		}
		var resp struct {
			Rows []map[string]any `json:"rows"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		return resp.Rows
	}
	numbers := func(rows []map[string]any) []int {
		var out []int
		for _, row := range rows {
			out = append(out, int(row["number"].(float64)))
		}
		sort.Ints(out)
		return out
	}
	for query, want := range map[string][]int{
		"?slice=your-turn":         {2, 3, 4},
		"?slice=to-close":          {9},
		"?slice=waiting-elsewhere": {5, 6},
		"?slice=ready":             {1, 7},
		"?band=outside":            {3, 5, 8},
		"?band=waiting":            {4},
	} {
		got := numbers(get("/api/overview/issues.json" + query))
		if len(got) != len(want) {
			t.Errorf("%s rows = %v, want %v", query, got, want)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("%s rows = %v, want %v", query, got, want)
				break
			}
		}
	}
	rr := httptest.NewRecorder()
	s.handleOverviewIssuesJSON(rr, httptest.NewRequest(http.MethodGet, "/api/overview/issues.json?slice=done", nil))
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "valid slices") {
		t.Fatalf("an unknown slice = %d %q, want a 400 naming the valid slices", rr.Code, rr.Body.String())
	}
}

// The browser-facing labels: Your turn replaces blocked/needs-human on the
// tile and in the card filter it applies.
func TestOverviewLabelsSayYourTurn11567(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`{ key: 'your-turn', field: 'overviewYourTurn', label: 'Your turn'`,
		`{ key: 'waiting-elsewhere', field: 'overviewWaiting', label: 'Waiting on someone else'`,
		`'your-turn': { label: 'Your turn', kinds: ['issue', 'pr'], term: 'your_turn' }`,
		`'waiting-elsewhere': { label: 'Waiting on someone else'`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
	for _, gone := range []string{"blocked/needs-human", "Blocked / needs-human", "blocked-needs-human", "blocked_needs_human", "overviewBlockedHuman", "Blocked or needs-human"} {
		if strings.Contains(html, gone) {
			t.Errorf("index.html still says %q", gone)
		}
	}
	// The retired chart slices come from nowhere: the browser lists the
	// server's slices, not the band order.
	for _, want := range []string{"function overviewSliceSpecs(kind)", "(window._lastStatus || {}).overview_slices"} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
}

// Rows the totals did not count must not break the partition: the equation
// opens up to the rows it lists instead of summing past its open count.
func TestOverviewEquationStaysPartitionWhenForgeCountsLag11567(t *testing.T) {
	at := fixtureTime11567()
	status := &StatusPayload{Repos: []FrontendRepo{{
		Name: "a", Full: "o/a", Issues: 1,
		ActionableIssues: []any{github.Issue{Repo: "o/a", Number: 1, CreatedAt: at, UpdatedAt: at}},
		NonActionableIssues: []any{
			github.RepoWorkIssue{Repo: "o/a", Number: 2, Labels: []string{"needs-spec"}, Bucket: repoWorkBucketFiltered, Reason: "needs-spec", CreatedAt: at, UpdatedAt: at},
			github.RepoWorkIssue{Repo: "o/a", Number: 3, Labels: []string{"needs-direction"}, Bucket: repoWorkBucketFiltered, Reason: "needs-direction", CreatedAt: at, UpdatedAt: at},
		},
	}}}
	decideRepoOwnership(status.Repos, config.DashboardIssueBandsConfig{}, ownershipEnv11565(5))
	eq := newTestServer().statusWithOverviewBands(status, overviewFixtureNow11567).ActionableNow.IssueEquation
	sum := eq.Result
	for _, term := range eq.Terms {
		sum += term.Count
	}
	if sum != eq.Open || eq.Open != 3 || overviewTermCount(eq, "your_turn") != 2 {
		t.Fatalf("equation = %s (open %d, sum %d)", eq.Text, eq.Open, sum)
	}
}

// The browser's copy of the slice order stays in step with the server's.
func TestOverviewSliceTaxonomy11567(t *testing.T) {
	specs := OverviewSliceSpecs(config.DashboardIssueBandsConfig{})
	keys := func(list []BandSpec) string {
		var out []string
		for _, spec := range list {
			out = append(out, spec.Key)
		}
		return strings.Join(out, ",")
	}
	if got, want := keys(specs.Issues), "your-turn,to-close,waiting-elsewhere,ready,in-progress"; got != want {
		t.Fatalf("issue slices = %s, want %s", got, want)
	}
	if got, want := keys(specs.PRs), "your-turn,to-close,waiting-elsewhere,eligible,blocked,in-review,open,draft"; got != want {
		t.Fatalf("PR slices = %s, want %s", got, want)
	}
	for _, spec := range append(append([]BandSpec{}, specs.Issues...), specs.PRs...) {
		if spec.Label == "" || spec.Rule == "" {
			t.Fatalf("slice %s lacks a label or rule", spec.Key)
		}
	}
	if specs.Issues[0].Label != "Your turn" || specs.Issues[1].Label != "To close" || specs.Issues[2].Label != "Waiting on someone else" {
		t.Fatalf("ownership slice labels = %q / %q / %q", specs.Issues[0].Label, specs.Issues[1].Label, specs.Issues[2].Label)
	}
}

// The browser takes its slices and tile filters from the server's stamps: the
// chart slice counts equal the server's, and every tile's card filter counts
// the same items as the equation term it is labelled with.
func TestOverviewBrowserSlicesAndFiltersAgreeWithEquation11567(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: Overview browser agreement was not executed")
	}
	s, status := decideMixedOverview11567(t)
	resp := s.statusWithOverviewBands(status, overviewFixtureNow11567)
	payload, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	html := indexHTML(t)
	var script strings.Builder
	script.WriteString("const assert = require('node:assert/strict');\nconst window = {_lastStatus: " + string(payload) + "};\n")
	script.WriteString(`
const OVERVIEW_PROJECT_FILTERS = {
  'open-issues': { label: 'Total open issues', kinds: ['issue'], bucketKind: 'issues' },
  'open-prs': { label: 'Total open PRs', kinds: ['pr'], bucketKind: 'prs' },
  'actionable-now': { label: 'Actionable now', kinds: ['issue', 'pr'], term: 'actionable' },
  'your-turn': { label: 'Your turn', kinds: ['issue', 'pr'], term: 'your_turn' },
  'waiting-elsewhere': { label: 'Waiting on someone else', kinds: ['issue', 'pr'], term: 'waiting_elsewhere' },
  'held': { label: 'Held', kinds: ['issue', 'pr'], term: 'held' },
  'blocked': { label: 'Blocked', kinds: ['pr'], term: 'blocked' },
  'outside': { label: 'Outside', kinds: ['issue', 'pr'], term: 'outside', outside: true }
};
`)
	for _, name := range []string{"issueUpdatedAt", "prUpdatedAt", "prCreatedAt", "prReviewClassRank", "overviewRepoName", "overviewSliceSpecs", "overviewIssueBandSlices", "overviewPRBandSlices",
		"overviewRepoNumber", "overviewRepoOpenIssueCount", "overviewRepoOpenPRCount", "overviewProjectFilterSpec", "repoCardParkedIssues", "overviewRepoTrackedIssueEntries", "overviewRepoTrackedPREntries",
		"overviewNonActionableMoved", "overviewProjectOutsideIssueCount", "overviewProjectOutsidePRCount", "overviewProjectFilterMatchesEntry", "overviewProjectFilterRepoMatches",
		"overviewProjectFilterState", "overviewProjectFilterKeyFromURL", "overviewProjectFilterSummary", "overviewNavigateBand"} {
		script.WriteString(jsFunc(t, html, name))
		script.WriteByte('\n')
	}
	script.WriteString(`
const location = { hash: '', search: '' };
let _overviewProjectFilterKey = null;
let _overviewProjectFilterCount = null;
let applied = '';
function overviewApplyKPIProjectFilter(key) { applied = key; }
const repos = window._lastStatus.repos;
const server = window._lastStatus.overview_slices;
const issueSlices = overviewIssueBandSlices(repos);
const prSlices = overviewPRBandSlices(repos);
const counts = slices => Object.fromEntries(slices.map(s => [s.key, s.count]));
assert.deepEqual(counts(issueSlices), Object.fromEntries(server.issues.map(s => [s.key, s.count])));
assert.deepEqual(counts(prSlices), Object.fromEntries(server.prs.map(s => [s.key, s.count])));
assert.deepEqual(counts(issueSlices), { 'your-turn': 3, 'to-close': 1, 'waiting-elsewhere': 2, ready: 2, 'in-progress': 0 });
assert.deepEqual(counts(prSlices), { 'your-turn': 1, 'to-close': 0, 'waiting-elsewhere': 3, eligible: 0, blocked: 1, 'in-review': 0, open: 1, draft: 1 });
// every plotted item sits in exactly one slice
const seen = new Set();
for (const slice of issueSlices) for (const entry of slice.items) {
  assert.equal(entry.info.chart_slice, slice.key);
  assert.ok(!seen.has(entry.number), 'issue #' + entry.number + ' plotted twice');
  seen.add(entry.number);
}
assert.equal(seen.size, 8);
// the tiles' card filters count what the equation terms count
const eq = window._lastStatus.actionableNow.equation;
const term = key => (eq.terms.find(t => t.key === key) || { count: 0 }).count;
for (const [key, termKey] of [['your-turn', 'your_turn'], ['waiting-elsewhere', 'waiting_elsewhere'], ['held', 'held'], ['blocked', 'blocked'], ['outside', 'outside'], ['actionable-now', 'actionable']]) {
  assert.equal(overviewProjectFilterSummary(key, repos).count, term(termKey), key);
}
assert.equal(overviewProjectFilterSummary('open-issues', repos).count, 9);
assert.equal(overviewProjectFilterSummary('open-prs', repos).count, 8);
// a legend row opens the filter of the tile that counts the same rows
for (const [slice, want] of [['your-turn', 'your-turn'], ['to-close', 'your-turn'], ['waiting-elsewhere', 'waiting-elsewhere'], ['blocked', 'blocked'], ['draft', 'outside'], ['ready', 'actionable-now'], ['open', 'actionable-now']]) {
  overviewNavigateBand('issue', slice);
  assert.equal(applied, want, slice);
}
`)
	if out, err := exec.Command(node, "-e", script.String()).CombinedOutput(); err != nil {
		t.Fatalf("browser agreement failed: %v\n%s", err, out)
	}
}

// The PR term follows the spec's term order from the facts, not the
// merge-state band: a draft stays outside even when GitHub reports a review
// decision (which puts it in the in-review band), and a conflicting or
// failing PR is blocked even when it carries the auto-merge label (which puts
// it in the eligible band).
func TestOverviewPRTermClassifiesFromFacts11567(t *testing.T) {
	at := fixtureTime11567()
	pr := func(number int, mutate func(*github.PullRequest)) github.PullRequest {
		p := github.PullRequest{Repo: "o/a", Number: number, Title: "pr", CreatedAt: at, UpdatedAt: at}
		mutate(&p)
		return p
	}
	reviewRequired := &github.ProtectionFacts{ReviewDecision: github.ReviewDecisionReviewRequired}
	status := &StatusPayload{Repos: []FrontendRepo{{
		Name: "a", Full: "o/a", PRs: 5,
		OpenPrs: []any{
			FrontendPR{PullRequest: pr(1, func(p *github.PullRequest) {})},
			FrontendPR{PullRequest: pr(2, func(p *github.PullRequest) { p.Draft = true; p.Protection = reviewRequired })},
			FrontendPR{PullRequest: pr(3, func(p *github.PullRequest) {
				p.Labels = []string{github.AutoMergeQueuedLabel}
				p.Mergeable = github.MergeableNo
			})},
			FrontendPR{PullRequest: pr(4, func(p *github.PullRequest) {
				p.Labels = []string{github.AutoMergeQueuedLabel}
				p.CIStatus = "failing"
				p.FailingChecks = []string{"build"}
			})},
			FrontendPR{PullRequest: pr(5, func(p *github.PullRequest) { p.Draft = true })},
		},
		WorkBreakdown: &github.RepoWorkBreakdown{PRs: github.RepoPRBreakdown{Actionable: 3, Draft: 2}},
	}}}
	decideRepoOwnership(status.Repos, config.DashboardIssueBandsConfig{}, ownershipEnv11565(5))
	resp := newTestServer().statusWithOverviewBands(status, overviewFixtureNow11567)

	if got := overviewPRTerm(nil, false, pr(2, func(p *github.PullRequest) { p.Draft = true; p.Protection = reviewRequired }), nil, ""); got != overviewTermOutside {
		t.Fatalf("draft with REVIEW_REQUIRED term = %q, want outside", got)
	}
	if got := overviewPRTerm(nil, false, pr(3, func(p *github.PullRequest) {
		p.Labels = []string{github.AutoMergeQueuedLabel}
		p.Mergeable = github.MergeableNo
	}), nil, ""); got != overviewTermBlocked {
		t.Fatalf("auto-merge-labelled conflicting PR term = %q, want blocked", got)
	}

	wantTerm := map[int]string{1: "actionable", 2: "outside", 3: "blocked", 4: "blocked", 5: "outside"}
	gotTerm := map[int]string{}
	for _, raw := range resp.Repos[0].OpenPrs {
		row, ok := raw.(overviewStatusPR)
		if !ok {
			t.Fatalf("open PR row is %T, want overviewStatusPR", raw)
		}
		gotTerm[row.Number] = row.OverviewTerm
	}
	for number, want := range wantTerm {
		if gotTerm[number] != want {
			t.Errorf("PR #%d term = %q, want %s", number, gotTerm[number], want)
		}
	}
	eq := resp.ActionableNow.PREquation
	if eq == nil {
		t.Fatal("missing PR equation")
	}
	counts := eqTermCounts11567(eq)
	if eq.Result != 1 || counts["blocked"] != 2 || counts["outside"] != 2 {
		t.Fatalf("PR equation = %s, want 1 actionable + 2 blocked + 2 outside (counts %v)", eq.Text, counts)
	}
}

// A parked agent-filed issue a person acknowledged by a path other than the
// approved-direction label (a parent-issue acknowledgement, say) is reported
// acknowledged in the status payload and in the export.
func TestParkedRowCarriesHumanAcknowledgement11567(t *testing.T) {
	at := fixtureTime11567()
	status := &StatusPayload{HiveID: "hive-test", Repos: []FrontendRepo{{
		Name: "a", Full: "o/a", Issues: 2,
		NonActionableIssues: []any{
			github.RepoWorkIssue{Repo: "o/a", Number: 1, Title: "acked", Labels: []string{"agent/strategist", "needs-decision"}, Bucket: repoWorkBucketFiltered, Reason: "needs-decision", HumanAcknowledged: true, CreatedAt: at, UpdatedAt: at},
			github.RepoWorkIssue{Repo: "o/a", Number: 2, Title: "not acked", Labels: []string{"agent/strategist", "needs-decision"}, Bucket: repoWorkBucketFiltered, Reason: "needs-decision", CreatedAt: at, UpdatedAt: at},
		},
	}}}
	s := newTestServer()
	s.deps = &Dependencies{Config: &config.Config{HiveID: "hive-test"}}
	decideRepoOwnership(status.Repos, config.DashboardIssueBandsConfig{}, ownershipEnv11565(5))

	rows := wireRows11567(t, s.statusWithOverviewBands(status, overviewFixtureNow11567))
	for number, want := range map[int]bool{1: true, 2: false} {
		if got := rows["issues"][number]["acknowledged"]; got != want {
			t.Errorf("status row #%d acknowledged = %v, want %v", number, got, want)
		}
	}

	got := map[int]bool{}
	for _, item := range overviewIssueItems(status, config.DashboardIssueBandsConfig{}, overviewFilters{}, overviewFixtureNow11567, s.selfAuthorizationHoldActiveForRepo) {
		got[item.issue.Number] = item.info.Acknowledged
	}
	if want := map[int]bool{1: true, 2: false}; got[1] != want[1] || got[2] != want[2] || len(got) != 2 {
		t.Errorf("export acknowledged = %v, want %v", got, want)
	}
}
