package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// An issue with an open sub-issue is a parent waiting on its children: it is
// not work to hand to a contributor, and a contributor's word that a merged
// PR "already did it" does not make it done.
//
// The enumeration already resolves the relation onto every row
// (github.Issue.Children, serialised as `children`: GitHub's sub-issue
// summary first, else the split-parents ledger and `hive:parent` body
// markers), and the issue stage sweep already reads it. This file is the
// contribute hub's reading of the same data: the one tracker rule both offer
// ladders call, and the check the verdict settle makes before it writes
// "done" on an issue.

// contributeTrackerHold is the outcome of the tracker rule for one candidate.
type contributeTrackerHold struct {
	// held: the candidate is coordination-only and is never offered.
	held bool
	// openSubIssues is set when the candidate is held because its children
	// are still open rather than because the enumerator flagged it
	// (is_tracker: label, title prefix or a child list in the body).
	openSubIssues int
}

// contributeTrackerHoldFor is the single tracker rule of the contributor
// offer path. selectTask (assignment) and admissionQueueSnapshot (the queue
// projection and its Withheld list) both call it, so the two cannot disagree
// about what a tracker is. It reads only the candidate's own row: the
// enumerator's is_tracker flag, unchanged, and the children the enumeration
// put on it. No GitHub call.
func contributeTrackerHoldFor(issue map[string]any) contributeTrackerHold {
	if isTracker, _ := issue["is_tracker"].(bool); isTracker {
		return contributeTrackerHold{held: true}
	}
	if open := childrenFromIssueMap(issue).Open(); open > 0 {
		return contributeTrackerHold{held: true, openSubIssues: open}
	}
	return contributeTrackerHold{}
}

// childrenFromIssueMap reads the `children` block (total / completed) off a
// decoded actionable-issue row. The zero value means the row carries none.
func childrenFromIssueMap(issue map[string]any) ghpkg.IssueChildren {
	raw, ok := issue["children"].(map[string]any)
	if !ok {
		return ghpkg.IssueChildren{}
	}
	return ghpkg.IssueChildren{Total: intFromAny(raw["total"]), Completed: intFromAny(raw["completed"])}
}

// openSubIssuesPhrase is "has N open sub-issue(s)", the words an operator
// reads in the Withheld list.
func openSubIssuesPhrase(open int) string {
	if open == 1 {
		return "has 1 open sub-issue"
	}
	return fmt.Sprintf("has %d open sub-issues", open)
}

// withheldTrackerItem is the Withheld row for a tracker refusal. A flagged
// tracker keeps the plain label it always had; a parent held for its open
// children says how many are open, which is what tells a maintainer when it
// will be offered again.
func withheldTrackerItem(c withheldCandidate, hold contributeTrackerHold) AdmissionWithheldItem {
	item := newWithheldItem(c, withheldReasonTracker)
	if hold.openSubIssues > 0 {
		item.Detail = fmt.Sprintf("%s: %s", withheldReasonLabel(withheldReasonTracker), openSubIssuesPhrase(hold.openSubIssues))
	}
	return item
}

// statusIssueChildren returns the children the last enumeration put on the
// issue's row in the published status, looking in the actionable and the
// sidelined lists (both carry `children`). The zero value means no row was
// found or the row has none.
func (h *ContributeWSHub) statusIssueChildren(repo string, number int) ghpkg.IssueChildren {
	if h == nil || h.server == nil || repo == "" || number <= 0 {
		return ghpkg.IssueChildren{}
	}
	h.server.statusMu.RLock()
	status := h.server.status
	h.server.statusMu.RUnlock()
	if status == nil {
		return ghpkg.IssueChildren{}
	}
	for _, r := range status.Repos {
		if !strings.EqualFold(r.Full, repo) {
			continue
		}
		for _, list := range [][]any{r.ActionableIssues, r.NonActionableIssues} {
			for _, raw := range list {
				b, err := json.Marshal(raw)
				if err != nil {
					continue
				}
				var row struct {
					Number   int                  `json:"number"`
					Children *ghpkg.IssueChildren `json:"children"`
				}
				if err := json.Unmarshal(b, &row); err != nil || row.Number != number {
					continue
				}
				if row.Children != nil && row.Children.Total > 0 {
					return *row.Children
				}
			}
		}
	}
	return ghpkg.IssueChildren{}
}

// verdictHeldForOpenChildren decides, for a no_work_needed verdict whose
// citation verified as merged work, whether the hub may go on to record the
// settlement and mark the issue done (hive/likely-done, the already-done
// label and comment, the close). It reports held=true when it may not, with
// the disposition to return for the run.
//
// Which data decides:
//   - The issue's enumeration row says a child is open: held, no GitHub call.
//   - Otherwise the children are re-read from GitHub (one issue GET, plus one
//     GET per ledger-recorded child when the issue has no native sub-issues),
//     because the row can be minutes old and a close is not easily undone. An
//     open child there: held. A re-read that fails: held as well, since
//     nothing downstream may be written on a guess.
//   - No GitHub client wired (nothing could be written through it either):
//     not held, exactly as before this check existed.
//
// A held issue is left as it was: no claim is recorded, so the offer hold the
// completion booked for the no_work_needed verdict stays in force (the claim
// path lifts it, the caller does that only after this returns false), and
// the tracker rule keeps the parent out of the offer pool while a child is
// open.
func (h *ContributeWSHub) verdictHeldForOpenChildren(ctx context.Context, repo string, number int, ref string, alreadyDone bool) (held bool, disposition string) {
	if h == nil {
		return false, ""
	}
	kids, source := h.statusIssueChildren(repo, number), "enumeration"
	if kids.Open() == 0 {
		if h.server == nil || h.server.deps == nil || h.server.deps.GHClient == nil {
			return false, ""
		}
		live, err := h.server.deps.GHClient.LiveIssueChildren(ghpkg.WithRESTCaller(ctx, "hive:verdict_settle"), repo, number)
		if errors.Is(err, ghpkg.ErrNoGitHubClient) {
			return false, ""
		}
		if err != nil {
			h.logger.Warn("[contribute-ws] verdict settle: sub-issues unreadable; not marking done",
				"repo", repo, "number", number, "ref", ref, "error", err.Error())
			if alreadyDone {
				return true, verdictDispositionAlreadyDoneVerified
			}
			return true, ""
		}
		kids, source = live, "github"
	}
	if kids.Open() == 0 {
		return false, ""
	}
	h.logger.Info("[contribute-ws] parent has open sub-issues; not marking done",
		"repo", repo, "number", number, "open", kids.Open(), "total", kids.Total, "ref", ref, "source", source)
	if alreadyDone {
		return true, verdictDispositionAlreadyDoneOpenSubIssues
	}
	return true, ""
}
