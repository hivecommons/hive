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
	// hiveStatusMaxRepos bounds the per-repo split in the brief; it is
	// wide enough that a question about one repo finds that repo's count.
	hiveStatusMaxRepos = 8
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
	return hiveStatusBrief(status, snap, now), nil
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

func hiveStatusBrief(status *StatusPayload, snap chatSnapshot, now time.Time) string {
	var b strings.Builder
	t := snap.Totals
	if t == nil {
		t = &chatSnapshotTotals{}
	}
	oi := status.OpenItems
	sc := hiveStatusCountScope(status)
	known := oi.Issues > 0 || oi.PRs > 0 || len(snap.PRs) == 0
	if known {
		// The scope rides on every count, worded the same way each time,
		// and a partial count is never worded as a total
		// (meetings_hive_scope.go).
		b.WriteString(sc.OpenSentence(oi.Issues, oi.PRs))
		hiveStatusRepoSplit(&b, oi)
		scope := sc.Scope()
		fmt.Fprintf(&b, ". Actionable: %d issues, %d PRs%s. Held: %d issues, %d PRs%s. Blocked: %d issues, %d PRs%s. Awaiting a decision: %d.",
			oi.Buckets.Actionable.Issues, oi.Buckets.Actionable.PRs, scope, oi.Buckets.Held.Issues, oi.Buckets.Held.PRs, scope,
			oi.Buckets.Blocked.Issues, oi.Buckets.Blocked.PRs, scope, t.AwaitingDecision)
		if u := oi.Unlisted; u.Issues > 0 || u.PRs > 0 {
			fmt.Fprintf(&b, " Reported open but not yet listed, so not in these counts: %d issues, %d PRs.", u.Issues, u.PRs)
		}
		b.WriteString(sc.Guide())
		b.WriteString(hiveStatusFreshness(status, sc, now))
	} else {
		// Never state a number she cannot back: no counts beside listed
		// PRs means the counts are not in yet.
		b.WriteString("Open issue and PR counts are not available yet (the hive is still counting); do not state a count.")
	}
	hiveStatusHeadline(&b, status, sc)
	hiveStatusAgents(&b, status.Agents)
	if t.WorkerBeads > 0 || t.SupervisorBeads > 0 {
		fmt.Fprintf(&b, " Work queue: %d worker beads, %d supervisor beads.", t.WorkerBeads, t.SupervisorBeads)
	}
	var prs []string
	for _, pr := range snap.PRs {
		line := pr.Ref + " " + chatTruncate(pr.Title, hiveStatusTextCap)
		if pr.CI != "" {
			line += " (CI " + pr.CI + ")"
		}
		prs = append(prs, line)
	}
	hiveStatusList(&b, " Open PRs", prs)
	return chatTruncate(b.String(), hiveStatusMaxChars)
}

// hiveStatusHeadline appends the dashboard's headline metrics by the names
// the dashboard shows them: board pressure (the sidebar "BP = issues/PRs"
// pill, the open issues and PRs on this hive's board, #17240), actionable
// now, test coverage and stale claims. Asked "what is the board pressure of
// the hive?" in the live Discord test she had no live number and cited an
// old PR instead.
func hiveStatusHeadline(b *strings.Builder, status *StatusPayload, sc hiveCountScope) {
	oi := status.OpenItems
	scope := sc.Scope()
	if oi.Issues > 0 || oi.PRs > 0 {
		fmt.Fprintf(b, " Board pressure (BP): %s%d/%d, that is %s%d open issues and %s%d open PRs on the hive's board%s.",
			atLeast(sc.Partial()), oi.Issues, oi.PRs, atLeast(sc.IssuesPartial()), oi.Issues, atLeast(sc.PRsPartial()), oi.PRs, scope)
	}
	if an := status.ActionableNow; an.Total > 0 || an.Issues > 0 || an.PRs > 0 {
		fmt.Fprintf(b, " Actionable now: %d (%d issues, %d PRs)%s.", an.Total, an.Issues, an.PRs, scope)
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
	hiveStatusBreakdown(b, " Open issues by stage"+scope, status.OverviewTotals.Issues)
	hiveStatusBreakdown(b, " Open PRs by stage"+scope, status.OverviewTotals.PRs)
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

// hiveStatusRepoSplit lists the repos holding the open items, largest
// first, so "35 open issues" is never read as one repo's and a question
// about one repo has that repo's count. A repo whose issue list is still
// pending says so instead of printing a zero.
func hiveStatusRepoSplit(b *strings.Builder, oi FrontendOpenItems) {
	repos := slices.Clone(oi.Repos)
	sort.SliceStable(repos, func(i, j int) bool {
		if a, c := repos[i].Issues+repos[i].PRs, repos[j].Issues+repos[j].PRs; a != c {
			return a > c
		}
		return repos[i].Repo < repos[j].Repo
	})
	var parts []string
	more := 0
	for _, r := range repos {
		if r.Issues == 0 && r.PRs == 0 {
			continue
		}
		if len(parts) == hiveStatusMaxRepos {
			more++
			continue
		}
		if r.IssuesSyncing {
			parts = append(parts, fmt.Sprintf("%s issues still syncing, %d PRs", r.Repo, r.PRs))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s %d issues, %d PRs", r.Repo, r.Issues, r.PRs))
	}
	if len(parts) == 0 {
		return
	}
	b.WriteString(" (" + strings.Join(parts, "; "))
	if more > 0 {
		fmt.Fprintf(b, "; and %d more repos", more)
	}
	b.WriteString(")")
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
