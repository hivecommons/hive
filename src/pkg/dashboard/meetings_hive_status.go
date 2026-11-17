package dashboard

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

// Hive status for the meeting attendee (hivecommons/hive#18590 finding 9).
// "Betty, how many open issues are there?" is answered from the same
// in-memory status the Overview tiles and the dashboard chat's grounded
// answers read (s.status via chatBuildSnapshot), in-process and in
// milliseconds, instead of going to the research agents. Nothing is
// fetched and nothing is written.

const (
	// hiveStatusMaxListed bounds each named list (working agents, recent
	// completions, open PRs) in the brief.
	hiveStatusMaxListed = 5
	// hiveStatusMaxChars bounds the whole brief.
	hiveStatusMaxChars = 2000
	// hiveStatusMaxRepos bounds the per-repo split in the brief.
	hiveStatusMaxRepos = 3
	// hiveStatusTextCap truncates one listed item's free text.
	hiveStatusTextCap = 80
)

// errHiveStatusUnavailable means the dashboard has no status snapshot yet.
var errHiveStatusUnavailable = errors.New("hive status is still initializing")

// HiveStatusBrief is the hive's live state as a few plain sentences for
// the meeting attendee: open and actionable issues and PRs, held and
// blocked counts, items awaiting a decision, agents (working, paused,
// blocked) and what they are doing or last finished, and the newest open
// PRs.
func (s *Server) HiveStatusBrief(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	status := s.chatStatusSnapshot()
	if status == nil {
		return "", errHiveStatusUnavailable
	}
	now := time.Now()
	status = hiveStatusEnriched(status, s.statusWithOverviewBands(status, now.UTC()))
	snap := chatBuildSnapshot(status, "", now, nil)
	return hiveStatusBrief(status, snap), nil
}

// hiveStatusEnriched is the status as /api/status serves it: s.status is
// the raw snapshot, and the open-items set, the Overview totals and
// actionable-now are derived when it is served (statusWithOverviewBands).
// Reading the raw snapshot is how she said "0 open issues" against about
// 35 across the hive's repos in the scored live test. A raw snapshot that
// already carries counts (tests, older producers) is kept when the
// derivation finds none.
func hiveStatusEnriched(raw, derived *StatusPayload) *StatusPayload {
	if derived == nil {
		return raw
	}
	if derived.OpenItems.Total == 0 && derived.OpenItems.Issues == 0 && derived.OpenItems.PRs == 0 &&
		(raw.OpenItems.Issues > 0 || raw.OpenItems.PRs > 0) {
		d := *derived
		d.OpenItems = raw.OpenItems
		if raw.ActionableNow.Total > 0 || raw.ActionableNow.Issues > 0 || raw.ActionableNow.PRs > 0 {
			d.ActionableNow = raw.ActionableNow
		}
		if raw.OverviewTotals.Issues.Forge > 0 || raw.OverviewTotals.PRs.Forge > 0 {
			d.OverviewTotals = raw.OverviewTotals
		}
		return &d
	}
	return derived
}

func hiveStatusBrief(status *StatusPayload, snap chatSnapshot) string {
	var b strings.Builder
	t := snap.Totals
	if t == nil {
		t = &chatSnapshotTotals{}
	}
	oi := status.OpenItems
	known := oi.Issues > 0 || oi.PRs > 0 || len(snap.PRs) == 0
	if known {
		fmt.Fprintf(&b, "Open now: %d issues and %d PRs", oi.Issues, oi.PRs)
		if oi.IssuesSyncing {
			b.WriteString(" (issue count still syncing, at least this many)")
		}
		hiveStatusScope(&b, oi)
		fmt.Fprintf(&b, ". Actionable: %d issues, %d PRs. Held: %d issues, %d PRs. Blocked: %d issues, %d PRs. Awaiting a decision: %d.",
			oi.Buckets.Actionable.Issues, oi.Buckets.Actionable.PRs, oi.Buckets.Held.Issues, oi.Buckets.Held.PRs,
			oi.Buckets.Blocked.Issues, oi.Buckets.Blocked.PRs, t.AwaitingDecision)
	} else {
		// Never state a number she cannot back: no counts beside listed
		// PRs means the counts are not in yet.
		b.WriteString("Open issue and PR counts are not available yet (the hive is still counting); do not state a count.")
	}
	hiveStatusHeadline(&b, status)

	var working, blocked, done []string
	for _, a := range snap.Agents {
		switch {
		case a.BlockedReason != "":
			blocked = append(blocked, a.Name)
		case a.Busy == "working":
			w := a.Name
			if a.Doing != "" {
				w += " (" + chatTruncate(a.Doing, hiveStatusTextCap) + ")"
			}
			working = append(working, w)
		}
		if a.LastCompleted != "" {
			done = append(done, a.Name+": "+chatTruncate(a.LastCompleted, hiveStatusTextCap))
		}
	}
	fmt.Fprintf(&b, " Agents: %d, %d working, %d paused, %d blocked.", t.Agents, len(working), t.AgentsPaused, len(blocked))
	if t.WorkerBeads > 0 || t.SupervisorBeads > 0 {
		fmt.Fprintf(&b, " Work queue: %d worker beads, %d supervisor beads.", t.WorkerBeads, t.SupervisorBeads)
	}
	hiveStatusList(&b, " Working on", working)
	hiveStatusList(&b, " Blocked", blocked)
	hiveStatusList(&b, " Recently finished", done)
	var prs []string
	for _, pr := range snap.PRs {
		line := pr.Ref + " " + chatTruncate(pr.Title, hiveStatusTextCap)
		if pr.CI != "" {
			line += " (CI " + pr.CI + ")"
		}
		prs = append(prs, line)
	}
	hiveStatusList(&b, " Open PRs", prs)
	if status.StaleSince != "" {
		b.WriteString(" Some counts are last-good figures from " + status.StaleSince + ".")
	}
	return chatTruncate(b.String(), hiveStatusMaxChars)
}

// hiveStatusHeadline appends the dashboard's headline metrics by the names
// the dashboard shows them: board pressure (the sidebar "BP = issues/PRs"
// pill, the open issues and PRs on this hive's board, #17240), actionable
// now, test coverage and stale claims. Asked "what is the board pressure of
// the hive?" in the live Discord test she had no live number and cited an
// old PR instead.
func hiveStatusHeadline(b *strings.Builder, status *StatusPayload) {
	oi := status.OpenItems
	if oi.Issues > 0 || oi.PRs > 0 {
		fmt.Fprintf(b, " Board pressure (BP): %d/%d, that is %d open issues and %d open PRs on the hive's board.", oi.Issues, oi.PRs, oi.Issues, oi.PRs)
	}
	if an := status.ActionableNow; an.Total > 0 || an.Issues > 0 || an.PRs > 0 {
		fmt.Fprintf(b, " Actionable now: %d (%d issues, %d PRs).", an.Total, an.Issues, an.PRs)
	}
	if c := status.OverviewCoverage; c != nil && c.Coverage > 0 {
		if c.Target > 0 {
			fmt.Fprintf(b, " Test coverage: %d%% (target %d%%).", c.Coverage, c.Target)
		} else {
			fmt.Fprintf(b, " Test coverage: %d%%.", c.Coverage)
		}
	}
	if sla := status.SLA; sla != nil && sla.StaleCount > 0 {
		fmt.Fprintf(b, " Stale claims past their SLA: %d.", sla.StaleCount)
	}
	hiveStatusBreakdown(b, " Open issues by stage", status.OverviewTotals.Issues)
	hiveStatusBreakdown(b, " Open PRs by stage", status.OverviewTotals.PRs)
}

// hiveStatusBreakdown appends the Overview tile's split of one total
// (tracked, held, outside and the named reasons), largest first, when the
// dashboard has one.
func hiveStatusBreakdown(b *strings.Builder, label string, t FrontendOverviewTotal) {
	type part struct {
		name string
		n    int
	}
	parts := []part{{"tracked", t.Tracked}, {"held", t.Held}}
	for k, v := range t.Breakdown {
		parts = append(parts, part{strings.ReplaceAll(k, "_", " "), v})
	}
	var out []string
	sort.SliceStable(parts, func(i, j int) bool {
		if parts[i].n != parts[j].n {
			return parts[i].n > parts[j].n
		}
		return parts[i].name < parts[j].name
	})
	for _, p := range parts {
		if p.n > 0 {
			out = append(out, fmt.Sprintf("%s %d", p.name, p.n))
		}
	}
	if len(out) == 0 {
		return
	}
	hiveStatusList(b, label, out)
}

// hiveStatusScope says what the counts cover: "across 13 repos", and the
// repos holding the most, so "35 open issues" is never read as one repo's.
func hiveStatusScope(b *strings.Builder, oi FrontendOpenItems) {
	if len(oi.Repos) == 0 {
		return
	}
	fmt.Fprintf(b, " across %d repos", len(oi.Repos))
	repos := slices.Clone(oi.Repos)
	sort.SliceStable(repos, func(i, j int) bool {
		if a, c := repos[i].Issues+repos[i].PRs, repos[j].Issues+repos[j].PRs; a != c {
			return a > c
		}
		return repos[i].Repo < repos[j].Repo
	})
	var parts []string
	for _, r := range repos {
		if r.Issues == 0 && r.PRs == 0 {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s %d issues, %d PRs", r.Repo, r.Issues, r.PRs))
		if len(parts) == hiveStatusMaxRepos {
			break
		}
	}
	if len(parts) > 0 {
		b.WriteString(" (" + strings.Join(parts, "; ") + ")")
	}
}

// hiveStatusList appends "<label>: a; b; c (and N more)." when list has items.
func hiveStatusList(b *strings.Builder, label string, list []string) {
	if len(list) == 0 {
		return
	}
	more := len(list) - hiveStatusMaxListed
	if more > 0 {
		list = list[:hiveStatusMaxListed]
	}
	b.WriteString(label + ": " + strings.Join(list, "; "))
	if more > 0 {
		fmt.Fprintf(b, " (and %d more)", more)
	}
	b.WriteString(".")
}

// HiveItemBrief describes one open issue or PR by number from the hive's
// live state ("Betty, what is pull request 18689 about?"), so a lookup is
// answered in-process instead of going to the research agents. ok is false
// when no open item has that number.
func (s *Server) HiveItemBrief(ctx context.Context, number int) (string, bool) {
	if ctx.Err() != nil || number <= 0 {
		return "", false
	}
	status := s.chatStatusSnapshot()
	if status == nil {
		return "", false
	}
	for _, repo := range status.Repos {
		name := overviewRepoName(repo)
		for _, held := range []bool{false, true} {
			for _, pr := range frontendRepoPRs(repo, held) {
				if pr.pr.Number != number {
					continue
				}
				line := fmt.Sprintf("PR %s#%d %q by %s", name, number, chatTruncate(pr.pr.Title, hiveStatusTextCap), pr.pr.Author)
				if pr.pr.Draft {
					line += ", draft"
				}
				if pr.pr.CIStatus != "" {
					line += ", CI " + pr.pr.CIStatus
				}
				if held {
					line += ", on hold"
				}
				if len(pr.pr.Labels) > 0 {
					line += ", labels " + strings.Join(pr.pr.Labels, ", ")
				}
				return line + ".", true
			}
			for _, is := range frontendRepoIssues(repo, held) {
				if is.Number != number {
					continue
				}
				line := fmt.Sprintf("Issue %s#%d %q", name, number, chatTruncate(is.Title, hiveStatusTextCap))
				if held {
					line += ", on hold"
				}
				if len(is.Labels) > 0 {
					line += ", labels " + strings.Join(is.Labels, ", ")
				}
				return line + ".", true
			}
		}
	}
	return "", false
}
