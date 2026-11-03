package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/adminmcp"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/hiveadvisor"
	"github.com/hivecommons/hive/pkg/review"
)

// Part D2 of the "your turn" spec (hivecommons/hive#11507, sub-issue #11569):
// the Agreement success metric. For any status snapshot, with every
// repository selected, no pill filter and the Overview scope set to all, the
// card markers, the inbox rows, the Overview "Your turn" term, the Overview
// chart slices, the admin MCP review queue and the owner advice all describe
// the same set of N + M your-turn items, and the Projects counter shows that
// set's N and M, with zero discrepancies.
//
// Each surface is read through the code that really serves it: the server's
// decision (decideRepoOwnership), statusWithOverviewBands, the Overview export
// builders, the admin MCP provider over the dashboard's own HTTP routes, the
// owner-advice queue builder, and the page's own functions under node (like the
// other node-driven dashboard tests, the browser half skips loudly when node is
// not installed). The expected set is read from the stored decisions of the
// snapshot, never recomputed.

// agreementItem is one item as the surfaces name it.
type agreementItem struct {
	Repo   string
	Kind   string
	Number int
}

func (i agreementItem) String() string { return fmt.Sprintf("%s %s#%d", i.Repo, i.Kind, i.Number) }

// agreementSet maps an item to the verb its surface shows for it.
type agreementSet map[agreementItem]string

func (s agreementSet) keys() []string {
	out := make([]string, 0, len(s))
	for item, verb := range s {
		out = append(out, fmt.Sprintf("%s=%s", item, verb))
	}
	sort.Strings(out)
	return out
}

func requireSameSet11569(t *testing.T, surface string, got, want agreementSet) {
	t.Helper()
	g, w := strings.Join(got.keys(), "\n"), strings.Join(want.keys(), "\n")
	if g != w {
		t.Fatalf("%s disagrees with the decision.\nsurface:\n%s\n\nwant:\n%s", surface, g, w)
	}
}

// ---- fixtures ---------------------------------------------------------------

type agreementFixture struct {
	name      string
	hiveLevel int
	levels    map[string]int
	repos     func() []FrontendRepo
}

// agreementTableRepo holds one item for every row of the "Ownership by level"
// table of #11507 (in every state the table distinguishes), in repository repo.
func agreementTableRepo(repo string) FrontendRepo {
	full := "o/" + repo
	at := func(d time.Duration) time.Time { return ownershipNow11565.Add(-d) }
	fresh := at(time.Hour)
	stale := at(40 * 24 * time.Hour)
	issue := func(number int, created, updated time.Time, labels ...string) github.Issue {
		return github.Issue{Repo: full, Number: number, Title: fmt.Sprintf("issue %d", number), Labels: labels, CreatedAt: created, UpdatedAt: updated}
	}
	parked := func(number int, bucket, reason string, labels ...string) github.RepoWorkIssue {
		return github.RepoWorkIssue{Repo: full, Number: number, Title: fmt.Sprintf("issue %d", number), Labels: labels, Bucket: bucket, Reason: reason, CreatedAt: fresh, UpdatedAt: fresh}
	}
	pr := func(number int, hive bool, mutate func(*github.PullRequest)) FrontendPR {
		p := github.PullRequest{Repo: full, Number: number, Title: fmt.Sprintf("pr %d", number), Author: "alice", CreatedAt: fresh, UpdatedAt: fresh}
		if hive {
			p.HiveAgent = "scanner"
		}
		if mutate != nil {
			mutate(&p)
		}
		return FrontendPR{PullRequest: p}
	}
	held := func(number int, hive bool, sources ...string) FrontendPR {
		return pr(number, hive, func(p *github.PullRequest) {
			p.Labels = []string{"hold"}
			p.HoldSources = sources
		})
	}
	return FrontendRepo{
		Name: repo, Full: full,
		ActionableIssues: []any{
			issue(1, fresh, fresh),                                    // unclaimed, filed by a person: not yours
			issue(2, fresh, fresh, "agent/strategist"),                // agent-filed, unacknowledged: TRIAGE while the hold is active
			issue(3, fresh, fresh, "hive/already-done"),               // verified done: CLOSE
			issue(4, fresh, fresh, "blocked", "Epic", "2-discussing"), // stay in the agent queue
			issue(5, stale, stale),                                    // safety net at L3 and above
			issue(6, fresh, fresh, "hive/likely-done"),                // likely done, actionable
		},
		HeldIssues: []any{
			github.HoldItem{Repo: full, Number: 7, Type: "issue", Labels: []string{"hold"}, HoldSources: []string{github.HoldSourcePerson}, CreatedAt: fresh},
			github.HoldItem{Repo: full, Number: 8, Type: "issue", Labels: []string{"hold"}, HoldSources: []string{github.HoldSourceReporterTrust}, CreatedAt: fresh},
			github.HoldItem{Repo: full, Number: 9, Type: "issue", Labels: []string{"hold", "agent/strategist"}, HoldSources: []string{github.HoldSourceSelfAuthorization}, CreatedAt: fresh},
		},
		NonActionableIssues: []any{
			parked(10, repoWorkBucketFiltered, "needs-decision", "needs-decision"),
			parked(11, repoWorkBucketFiltered, "needs-direction", "needs-direction"),
			parked(12, repoWorkBucketFiltered, "needs-human", "needs-human"),
			parked(13, repoWorkBucketFiltered, "needs-spec", "wants-a-spec"),
			parked(14, repoWorkBucketReporterTriage, "Awaiting maintainer triage label"),
			parked(15, repoWorkBucketClaimedByPR, "Open hive-authored PR #30 already claims this issue", "hive/covered-by-pr"),
			parked(16, repoWorkBucketFiltered, "needs-reporter-confirmation", "needs-reporter-confirmation"),
			github.RepoWorkIssue{Repo: full, Number: 17, Title: "stale confirmation", Labels: []string{"needs-reporter-confirmation"}, Bucket: repoWorkBucketFiltered, Reason: "needs-reporter-confirmation", CreatedAt: at(60 * 24 * time.Hour), UpdatedAt: at(40 * 24 * time.Hour)},
			parked(18, repoWorkBucketFiltered, "Exempt label", "no-ai"),
		},
		OpenPrs: []any{
			pr(30, true, func(p *github.PullRequest) { p.Labels = []string{"needs-human"} }),                             // hive PR, adjudicator has not passed
			pr(31, true, func(p *github.PullRequest) { p.Labels = []string{"needs-human", "reviewer-recommend-close"} }), // adjudicator recommended close
			pr(32, true, func(p *github.PullRequest) { p.Labels = []string{"needs-human", "reviewer-passed"} }),          // re-escalated
			pr(33, false, func(p *github.PullRequest) { p.Labels = []string{"needs-human"} }),                            // person's PR
			pr(34, false, func(p *github.PullRequest) { p.Labels = []string{config.DefaultSentinelLabel} }),              // sentinel-flagged
			pr(35, true, func(p *github.PullRequest) { p.CreatedAt, p.UpdatedAt = stale, stale }),                        // unheld hive PR, safety net
			pr(36, true, func(p *github.PullRequest) { p.Draft = true }),                                                 // draft
			pr(37, false, func(p *github.PullRequest) { p.CIStatus = "failing"; p.FailingChecks = []string{"ci/test"} }), // failing CI
			pr(38, false, nil), // ordinary
			pr(39, false, func(p *github.PullRequest) { p.HeadSHA = "head-39" }), // review requires a human on the current head (#12329)
		},
		HeldPrs: []any{
			held(40, true, github.HoldSourceLevelGate),
			held(41, false, github.HoldSourcePerson, github.HoldSourceLevelGate),
			held(42, true, github.HoldSourceOutreach),
			held(43, true, github.HoldSourceUnknown),
			held(44, true, github.HoldSourceDashboardPause),
			held(45, true, github.HoldSourceLevelGate, github.HoldSourceReporterTrust),
			held(46, true, github.HoldSourceHumanMergePath),
		},
	}
}

func agreementFixtures() []agreementFixture {
	var out []agreementFixture
	// The level table: a hive and its repository at each level L1-L6.
	for level := 1; level <= 6; level++ {
		level := level
		out = append(out, agreementFixture{
			name:      fmt.Sprintf("level table L%d", level),
			hiveLevel: level,
			repos:     func() []FrontendRepo { return []FrontendRepo{agreementTableRepo("table")} },
		})
	}
	// A mixed hive: repositories at several levels (an override can only lower
	// a repository's level), with parked issues, held PRs of every hold
	// source, `blocked` issues, reporter-trust holds, a sentinel PR and
	// likely-done issues, across more than one repository.
	out = append(out, agreementFixture{
		name:      "mixed fixture",
		hiveLevel: 5,
		levels:    map[string]int{"o/low": 3, "o/advisory": 2},
		repos: func() []FrontendRepo {
			return []FrontendRepo{agreementTableRepo("table"), agreementTableRepo("low"), agreementTableRepo("advisory")}
		},
	})
	return out
}

// ---- the surfaces ------------------------------------------------------------

// agreementServer is a server whose status is the decided snapshot, the way a
// status refresh stores it.
func agreementServer11569(t *testing.T, fx agreementFixture) (*Server, *StatusPayload) {
	t.Helper()
	s := NewServerWithAuth(0, "secret", nil)
	s.deps = &Dependencies{Config: &config.Config{HiveID: "h", Dashboard: config.DashboardConfig{AuthToken: "secret"}}}
	repos := fx.repos()
	env := ownershipEnv11565(fx.hiveLevel)
	env.repoLevel = func(repo string) int { return fx.levels[repo] }
	for _, repo := range repos {
		env.reviewVerdicts = append(env.reviewVerdicts, review.Aggregate{Repo: repo.Full, Number: 39, HeadSHA: "head-39", Verdict: review.VerdictRequiresHuman, RequiresHuman: true})
	}
	decideRepoOwnership(repos, config.DashboardIssueBandsConfig{}, env)
	status := &StatusPayload{HiveID: "h", ACMMLevel: fx.hiveLevel, AdmissionLabels: []string{"triage-ok"}, Repos: repos}
	s.statusMu.Lock()
	s.status = status
	s.statusMu.Unlock()
	return s, status
}

// storedDecisions reads the your-turn set straight from the stored decisions.
func storedDecisions11569(status *StatusPayload) (agreementSet, map[agreementItem]int, int, int) {
	set := agreementSet{}
	ranks := map[agreementItem]int{}
	var n, m int
	for _, repo := range status.Repos {
		for key, o := range repo.Ownership {
			if !o.YourTurn {
				continue
			}
			kind, num, _ := strings.Cut(key, "#")
			var number int
			fmt.Sscanf(num, "%d", &number)
			item := agreementItem{Repo: overviewRepoName(repo), Kind: kind, Number: number}
			set[item] = o.Verb
			ranks[item] = o.InboxRank
			if o.Verb == OwnershipVerbClose {
				m++
			} else {
				n++
			}
		}
	}
	return set, ranks, n, m
}

func wireStatus11569(t *testing.T, s *Server, status *StatusPayload) map[string]any {
	t.Helper()
	raw, err := json.Marshal(s.statusWithOverviewBands(status, ownershipNow11565))
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	return wire
}

// pageSurfaces11569 is what the page's own functions draw from the wire
// payload with every repository selected and no filter.
type pageSurfaces11569 struct {
	Markers []map[string]string `json:"markers"`
	Inbox   []map[string]string `json:"inbox"`
	Header  string              `json:"header"`
	Counter string              `json:"counter"`
	Slices  struct {
		Issues map[string][]string `json:"issues"`
		PRs    map[string][]string `json:"prs"`
	} `json:"slices"`
}

func runNodeOutput11569(t *testing.T, script string) []byte {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: the browser surfaces of the agreement check were NOT executed by this run")
	}
	path := filepath.Join(t.TempDir(), "agreement.js")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, path)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("agreement browser run failed: %v\n%s", err, stderr.String())
	}
	return out
}

func browserSurfaces11569(t *testing.T, wire map[string]any, repoNames []string) pageSurfaces11569 {
	t.Helper()
	html := indexHTML(t)
	skip := map[string]bool{"renderOverviewCharts": true, "refreshSectionCardShell": true, "ensureRepoHoldPermission": true, "renderRepoLegend": true}
	body := `
function renderOverviewCharts() {} function refreshSectionCardShell() {} function ensureRepoHoldPermission() {} function renderRepoLegend() {}
window._lastStatus = ` + yourTurnJSON(t, wire) + `;
window._hiveSentinelLabel = 'sentinel-alert';
localStorage.setItem('hive-repos-yourturn-expanded:h', JSON.stringify(` + yourTurnJSON(t, repoNames) + `));
renderRepos(window._lastStatus.repos);
const cards = els['repos'].innerHTML;
const strip = els['repos-inbox'].innerHTML;
const attrsOf = tag => { const a = {}; tag.replace(/([a-z-]+)="([^"]*)"/g, (_, k, v) => { a[k] = v; }); return a; };
const markerAttrs = (cards.match(/<button type="button" class="yt-menu-btn yt-marker"[^>]*>/g) || []).map(attrsOf);
const inbox = (strip.match(/<div class="yt-inbox-row">.*?<\/div>/gs) || []).map(row => {
  const a = attrsOf(row.match(/<button type="button" class="yt-menu-btn yt-inbox-btn"[^>]*>/)[0]);
  a.verb = row.match(/<span class="yt-verb">([A-Z]+)<\/span>/)[1];
  return a;
});
const header = (strip.match(/id="yt-inbox-head">([^<]*)</) || [])[1] || '';
const sliceItems = slices => Object.fromEntries(slices.map(s => [s.key, s.items.map(e => e.repo + '#' + e.number)]));
process.stdout.write(JSON.stringify({
  markers: markerAttrs, inbox, header, counter: els['repos-needs-human'].textContent,
  slices: { issues: sliceItems(overviewIssueBandSlices(window._lastStatus.repos)), prs: sliceItems(overviewPRBandSlices(window._lastStatus.repos)) }
}));
`
	script := yourTurnPageScript(t, skip, body, "renderRepos", "overviewIssueBandSlices", "overviewPRBandSlices")
	_ = html
	var out pageSurfaces11569
	if err := json.Unmarshal(runNodeOutput11569(t, script), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// exportSet reads a surface's your-turn set from Overview export rows (the
// rows the CSV/JSON endpoints, the admin MCP and the owner advice walk).
func exportSet11569(rows []any) (agreementSet, map[agreementItem]int) {
	set := agreementSet{}
	ranks := map[agreementItem]int{}
	for _, raw := range rows {
		row := raw.(map[string]any)
		if yours, _ := row["your_turn"].(bool); !yours {
			continue
		}
		kind := "issue"
		if _, ok := row["draft"]; ok {
			kind = "pr"
		}
		number, _ := row["number"].(int)
		item := agreementItem{Repo: row["repo"].(string), Kind: kind, Number: number}
		set[item] = row["verb"].(string)
		ranks[item], _ = row["inbox_rank"].(int)
	}
	return set, ranks
}

// ---- the agreement check ------------------------------------------------------

func TestYourTurnSurfacesAgreeOnTheSameSet11569(t *testing.T) {
	for _, fx := range agreementFixtures() {
		fx := fx
		t.Run(fx.name, func(t *testing.T) {
			s, status := agreementServer11569(t, fx)
			want, wantRanks, n, m := storedDecisions11569(status)
			if len(want) == 0 {
				t.Fatalf("fixture %s has no your-turn items; the check would be vacuous", fx.name)
			}
			wire := wireStatus11569(t, s, status)
			repoNames := make([]string, 0, len(status.Repos))
			for _, repo := range status.Repos {
				repoNames = append(repoNames, repo.Full)
			}

			// Inbox positions are the server's: 1..N+M, once each.
			seen := map[int]bool{}
			for item, rank := range wantRanks {
				if rank < 1 || rank > n+m || seen[rank] {
					t.Fatalf("%s has inbox_rank %d; ranks must be 1..%d, once each", item, rank, n+m)
				}
				seen[rank] = true
			}

			// Overview export rows: the one walk the CSV/JSON endpoints, the
			// admin MCP and the owner advice read.
			cfg := s.issueBandsConfig()
			issueRows, _ := s.overviewIssueRows(status, cfg, overviewFilters{}, ownershipNow11565)
			prRows, _ := s.overviewPRRows(status, cfg, overviewFilters{}, ownershipNow11565)
			exportIssues, issueRanks := exportSet11569(issueRows)
			exportPRs, prRanks := exportSet11569(prRows)
			export := agreementSet{}
			exportRanks := map[agreementItem]int{}
			for k, v := range exportIssues {
				export[k] = v
				exportRanks[k] = issueRanks[k]
			}
			for k, v := range exportPRs {
				export[k] = v
				exportRanks[k] = prRanks[k]
			}
			requireSameSet11569(t, "Overview export rows", export, want)
			for item, rank := range wantRanks {
				if exportRanks[item] != rank {
					t.Fatalf("export row %s inbox_rank = %d, want %d", item, exportRanks[item], rank)
				}
			}

			// Overview "Your turn" term: the equation, per row.
			now := s.statusWithOverviewBands(status, ownershipNow11565).ActionableNow
			if got := overviewTermCount(now.Equation, "your_turn"); got != n+m {
				t.Fatalf("Overview Your turn term = %d, want N+M = %d", got, n+m)
			}
			termSet := agreementSet{}
			sliceSet := agreementSet{}
			sliceCounts := map[string]int{}
			for _, repo := range wire["repos"].([]any) {
				r := repo.(map[string]any)
				name := r["full"].(string)
				for list, kind := range map[string]string{"actionableIssues": "issue", "heldIssues": "issue", "nonActionableIssues": "issue", "openPrs": "pr", "heldPrs": "pr"} {
					rows, _ := r[list].([]any)
					for _, raw := range rows {
						row := raw.(map[string]any)
						item := agreementItem{Repo: name, Kind: kind, Number: int(row["number"].(float64))}
						own, _ := row["ownership"].(map[string]any)
						verb, _ := own["verb"].(string)
						if row["overview_term"] == overviewTermYourTurn {
							termSet[item] = verb
						}
						if slice, _ := row["chart_slice"].(string); slice == overviewSliceYourTurn || slice == overviewSliceToClose {
							sliceSet[item] = verb
							sliceCounts[slice]++
							if (slice == overviewSliceToClose) != (verb == OwnershipVerbClose) {
								t.Fatalf("%s is in slice %s with verb %s", item, slice, verb)
							}
						}
					}
				}
			}
			requireSameSet11569(t, "Overview rows' Your turn term", termSet, want)

			// Overview chart slices: the server's slices and the page's own.
			requireSameSet11569(t, "Overview chart slices", sliceSet, want)
			if sliceCounts[overviewSliceYourTurn] != n || sliceCounts[overviewSliceToClose] != m {
				t.Fatalf("chart slices Your turn / To close = %d / %d, want N / M = %d / %d", sliceCounts[overviewSliceYourTurn], sliceCounts[overviewSliceToClose], n, m)
			}

			// Admin MCP review queue, through the dashboard's own provider,
			// read a page at a time as a caller would.
			type queueRow struct {
				Kind       string  `json:"kind"`
				Repo       string  `json:"repo"`
				Number     float64 `json:"number"`
				Verb       string  `json:"verb"`
				ReasonCode string  `json:"reason_code"`
				Reason     string  `json:"reason"`
				InboxRank  int     `json:"inbox_rank"`
			}
			var queue struct {
				Total  int `json:"total"`
				Counts struct {
					YourTurn int `json:"your_turn"`
					ToClose  int `json:"to_close"`
				} `json:"counts"`
				Rows       []queueRow `json:"rows"`
				Truncated  bool       `json:"rows_truncated"`
				NextOffset int        `json:"next_offset"`
			}
			var rows []queueRow
			for offset := 0; ; {
				result, err := dashboardAdminMCPProvider{server: s}.Read(context.Background(), adminmcp.ToolReviewQueue, map[string]any{"limit": float64(adminmcp.MaxResultLimit), "offset": float64(offset)})
				if err != nil {
					t.Fatal(err)
				}
				encoded, err := json.Marshal(result)
				if err != nil {
					t.Fatal(err)
				}
				queue.Rows, queue.Truncated, queue.NextOffset = nil, false, 0
				if err := json.Unmarshal(encoded, &queue); err != nil {
					t.Fatal(err)
				}
				rows = append(rows, queue.Rows...)
				if !queue.Truncated {
					break
				}
				offset = queue.NextOffset
			}
			reviewSet := agreementSet{}
			for i, row := range rows {
				item := agreementItem{Repo: row.Repo, Kind: row.Kind, Number: int(row.Number)}
				reviewSet[item] = row.Verb
				if row.ReasonCode == "" || row.Reason == "" || row.InboxRank != i+1 || wantRanks[item] != i+1 {
					t.Fatalf("review_queue row %d = %+v for %s, want inbox position %d with a reason code and a reason", i, row, item, wantRanks[item])
				}
			}
			requireSameSet11569(t, "admin MCP review queue", reviewSet, want)
			if queue.Total != n+m || queue.Counts.YourTurn != n || queue.Counts.ToClose != m {
				t.Fatalf("review_queue counts = total %d N %d M %d, want the inbox header's N=%d M=%d", queue.Total, queue.Counts.YourTurn, queue.Counts.ToClose, n, m)
			}

			// Owner advice: the items it counts as needing a human, for the
			// snapshot the advice was computed from.
			adviceQueue := s.buildHiveAdvisorQueue(status, ownershipNow11565)
			adviceSet := agreementSet{}
			for _, item := range hiveadvisor.NeedsHumanItems(adviceQueue) {
				adviceSet[agreementItem{Repo: item.Repo, Kind: item.Kind, Number: item.Number}] = item.Verb
			}
			requireSameSet11569(t, "owner advice needs-human set", adviceSet, want)
			if an, am := hiveadvisor.YourTurnCounts(adviceQueue); an != n || am != m {
				t.Fatalf("owner advice N/M = %d/%d, want %d/%d", an, am, n, m)
			}
			advice := s.AttachHiveAdvice(status, ownershipNow11565)
			if advice == nil || advice.Counts.YourTurn != n || advice.Counts.ToClose != m {
				t.Fatalf("advice counts = %+v, want N=%d M=%d", advice, n, m)
			}

			// Card markers, inbox rows, section counter and the page's chart
			// slices, drawn by the page itself.
			page := browserSurfaces11569(t, wire, repoNames)
			markerSet := agreementSet{}
			for _, a := range page.Markers {
				var number int
				fmt.Sscanf(a["data-yt-number"], "%d", &number)
				markerSet[agreementItem{Repo: a["data-yt-repo"], Kind: a["data-yt-kind"], Number: number}] = ""
			}
			wantMarkers := agreementSet{}
			for item := range want {
				wantMarkers[item] = ""
			}
			requireSameSet11569(t, "card markers", markerSet, wantMarkers)
			if len(page.Markers) != len(want) {
				t.Fatalf("%d markers drawn for %d your-turn items; a row carries one marker", len(page.Markers), len(want))
			}
			inboxSet := agreementSet{}
			for i, a := range page.Inbox {
				var number int
				fmt.Sscanf(a["data-yt-number"], "%d", &number)
				item := agreementItem{Repo: a["data-yt-repo"], Kind: a["data-yt-kind"], Number: number}
				inboxSet[item] = a["verb"]
				if wantRanks[item] != i+1 {
					t.Fatalf("inbox row %d is %s, whose inbox_rank is %d", i+1, item, wantRanks[item])
				}
			}
			requireSameSet11569(t, "inbox rows", inboxSet, want)
			wantHeader := fmt.Sprintf("Your turn · %d", n)
			wantCounter := fmt.Sprintf("· %d your turn", n)
			if m > 0 {
				wantHeader += fmt.Sprintf(" · %d to close", m)
				wantCounter += fmt.Sprintf(" · %d to close", m)
			}
			if page.Header != wantHeader || page.Counter != wantCounter {
				t.Fatalf("inbox header %q and Projects counter %q, want %q and %q", page.Header, page.Counter, wantHeader, wantCounter)
			}
			// Slice membership, not just totals: the page's Your turn and To
			// close slices hold exactly the your-turn items, each in the slice
			// its verb names.
			pageSlices := agreementSet{}
			add := func(kind string, slices map[string][]string) {
				for _, slice := range []string{overviewSliceYourTurn, overviewSliceToClose} {
					for _, id := range slices[slice] {
						repo, num, _ := strings.Cut(id, "#")
						item := agreementItem{Repo: repo, Kind: kind, Number: atoiOrFail(t, num)}
						if _, dup := pageSlices[item]; dup {
							t.Fatalf("%s is plotted in more than one slice", item)
						}
						if (slice == overviewSliceToClose) != (want[item] == OwnershipVerbClose) {
							t.Fatalf("%s is in page slice %s with verb %s", item, slice, want[item])
						}
						pageSlices[item] = want[item]
					}
				}
			}
			add("issue", page.Slices.Issues)
			add("pr", page.Slices.PRs)
			requireSameSet11569(t, "the page's chart slices", pageSlices, want)
		})
	}
}

// The fixtures are only worth their agreement if they exercise the situations
// the metric names. This pins that the mixed fixture really contains them, so
// a future change to a fixture cannot silently make the check vacuous.
func TestYourTurnAgreementFixturesCoverTheNamedSituations11569(t *testing.T) {
	fixtures := agreementFixtures()
	mixed := fixtures[len(fixtures)-1]
	_, status := agreementServer11569(t, mixed)
	table := status.Repos[0]
	want := map[string]struct {
		kind, verb, code string
		yours            bool
	}{
		"issue#10": {"issue", OwnershipVerbDecide, OwnershipReasonParkedNeedsDecision, true}, // parked issue
		"issue#13": {"issue", OwnershipVerbSpec, OwnershipReasonParkedNeedsSpec, true},       // parked issue (renamed label)
		"issue#14": {"issue", OwnershipVerbAdmit, OwnershipReasonUntrustedReporter, true},    // reporter trust
		"issue#4":  {"issue", "", "", false},                                                 // blocked, Epic, 2-discussing
		"issue#8":  {"issue", "", "", false},                                                 // reporter-trust hold on an issue
		"pr#40":    {"pr", OwnershipVerbReview, OwnershipReasonLevelGateHold, true},          // held PR
		"pr#41":    {"pr", "", "", false},                                                    // held PR with a person source
		"pr#45":    {"pr", OwnershipVerbReview, OwnershipReasonPolicyHold, true},             // reporter-trust hold on a PR
		"pr#43":    {"pr", OwnershipVerbReview, OwnershipReasonHoldSourceUnknown, true},      // unknown source
		"pr#44":    {"pr", "", "", false},                                                    // paused from the dashboard
		"issue#3":  {"issue", OwnershipVerbClose, OwnershipReasonVerifiedDone, true},         // to close
		"pr#39":    {"pr", OwnershipVerbReview, OwnershipReasonReviewRequiresHuman, true},    // review requires a human (#12329)
	}
	for key, w := range want {
		kind, num, _ := strings.Cut(key, "#")
		o := table.Ownership[ownershipKey(kind, atoiOrFail(t, num))]
		if o.YourTurn != w.yours || o.Verb != w.verb || o.ReasonCode != w.code {
			t.Errorf("%s = %+v, want your turn %v %s/%s", key, o, w.yours, w.verb, w.code)
		}
	}
	_, _, n, m := storedDecisions11569(status)
	if n < 10 || m < 3 {
		t.Fatalf("mixed fixture has N=%d M=%d; want a split large enough to exercise both counters", n, m)
	}
}

func atoiOrFail(t *testing.T, s string) int {
	t.Helper()
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		t.Fatal(err)
	}
	return n
}
