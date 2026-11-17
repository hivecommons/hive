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
	hiveStatusMaxChars = 1500
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
