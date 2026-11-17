package dashboard

import (
	"context"
	"errors"
	"fmt"
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
	snap := chatBuildSnapshot(status, "", now, nil)
	return hiveStatusBrief(status, snap), nil
}

func hiveStatusBrief(status *StatusPayload, snap chatSnapshot) string {
	var b strings.Builder
	t := snap.Totals
	if t == nil {
		t = &chatSnapshotTotals{}
	}
	oi := status.OpenItems
	fmt.Fprintf(&b, "Open now: %d issues and %d PRs", oi.Issues, oi.PRs)
	if oi.IssuesSyncing {
		b.WriteString(" (issue count still syncing, at least this many)")
	}
	fmt.Fprintf(&b, ". Actionable: %d issues, %d PRs. Held: %d issues, %d PRs. Blocked: %d issues, %d PRs. Awaiting a decision: %d.",
		oi.Buckets.Actionable.Issues, oi.Buckets.Actionable.PRs, oi.Buckets.Held.Issues, oi.Buckets.Held.PRs,
		oi.Buckets.Blocked.Issues, oi.Buckets.Blocked.PRs, t.AwaitingDecision)
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
	fmt.Fprintf(&b, " Agents: %d, %d paused, %d blocked.", t.Agents, t.AgentsPaused, len(blocked))
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
	fmt.Fprintf(b, " Board pressure (BP): %d/%d, that is %d open issues and %d open PRs on the hive's board.", oi.Issues, oi.PRs, oi.Issues, oi.PRs)
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
