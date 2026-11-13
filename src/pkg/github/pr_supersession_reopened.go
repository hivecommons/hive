package github

import (
	"context"
	"fmt"
	"strings"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/writeback"
)

// A PR that is open again and not superseded drops the sweep's close mark.
//
// Closing a superseded PR leaves two things on it: the `hive/superseded`
// label and the sweep's notice, which ends "Closing this hive-authored PR
// because …" or announces the grace window. Both invite a reopen when the
// close was wrong, and neither was taken back when someone did reopen it:
// Danathar/zfs-aurora-complex#313 was reopened with its body corrected,
// reviewed, approved and merged twelve hours later still labelled
// `hive/superseded` under a comment saying it was being closed.

// SupersessionActionClearedSuperseded: the sweep took `hive/superseded` off
// an open PR it no longer finds superseded and rewrote its own notice to
// say so.
const SupersessionActionClearedSuperseded = "cleared-superseded"

// supersessionRetractedPhrase opens the rewritten notice; a notice carrying
// it parks nothing and announces no close.
const supersessionRetractedPhrase = "This PR is open again and the supersession sweep no longer finds it superseded"

// supersessionNoticeFactPrefixes are the bullet lines of a sweep notice that
// name the issue and the merged PR; the rewritten notice keeps them so the
// history of what was announced stays readable.
var supersessionNoticeFactPrefixes = []string{"- Claimed issue: ", "- Closing PR: "}

// renderSupersessionRetractedComment rewrites the sweep's notice for a PR
// that is open and not superseded. earlier is the notice being replaced; its
// issue and closer lines are carried over. Rendering the result again yields
// the same text, so a repeated pass edits nothing.
func renderSupersessionRetractedComment(earlier string) string {
	var facts []string
	for _, line := range strings.Split(earlier, "\n") {
		line = strings.TrimRight(line, " \t\r")
		for _, prefix := range supersessionNoticeFactPrefixes {
			if strings.HasPrefix(line, prefix) {
				facts = append(facts, line)
			}
		}
	}
	var b strings.Builder
	fmt.Fprintln(&b, supersessionSweepMarker)
	fmt.Fprintf(&b, "%s, so the earlier notice here no longer applies and the `%s` label has been removed.\n", supersessionRetractedPhrase, SupersededLabel)
	if len(facts) > 0 {
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "The earlier notice named:")
		fmt.Fprintln(&b)
		for _, line := range facts {
			fmt.Fprintln(&b, line)
		}
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "_This comment is edited in place by the supersession sweep; it is not duplicated._")
	return b.String()
}

// releaseStaleSupersessionMarks is what the sweep does for an open PR it
// does not find superseded: lift its own park (releaseStaleSupersessionPark)
// and, in the same pass, take back its own close mark.
func (c *Client) releaseStaleSupersessionMarks(ctx context.Context, prRepo, owner, repo string, pr *gh.PullRequest) (SupersessionSweepEvent, string, error) {
	event, action, err := c.releaseStaleSupersessionPark(ctx, prRepo, owner, repo, pr)
	if err != nil {
		return event, action, err
	}
	cleared, err := c.clearStaleSupersededMark(ctx, prRepo, owner, repo, pr)
	if err != nil {
		return SupersessionSweepEvent{}, "", err
	}
	if cleared && action == "" {
		event = SupersessionSweepEvent{Repo: prRepo, Number: pr.GetNumber(), Author: safeGetLogin(pr.GetUser()), Action: SupersessionActionClearedSuperseded}
		action = "resolved"
	}
	return event, action, nil
}

// clearStaleSupersededMark removes `hive/superseded` from an open PR the
// sweep does not find superseded and rewrites the sweep's notice. The notice
// is the attribution: the label is also applied by the adoption, re-author
// and duplicate-fix closes, which leave their own note and undo their own
// label, so a labelled PR with no sweep notice is left alone. Both writes
// pass the write-back gate as the notice they replace does, so a paused
// repository, or a contributor's PR the hive may not write on, keeps its
// mark. The notice is rewritten before the label goes, so a failed label
// removal is retried on the next pass against an unchanged notice.
func (c *Client) clearStaleSupersededMark(ctx context.Context, prRepo, owner, repo string, pr *gh.PullRequest) (bool, error) {
	if !supersessionHasLabel(pr, SupersededLabel) {
		return false, nil
	}
	number := pr.GetNumber()
	notice, err := c.findSupersessionComment(ctx, owner, repo, number)
	if err != nil {
		return false, err
	}
	if notice == nil {
		return false, nil
	}
	if d := c.WritebackGate().Allowed(prRepo, c.sentinelWritebackItem(pr), writeback.KindSupersessionNotice); !d.Allowed {
		return false, nil
	}
	if _, err := c.ensureSupersessionComment(ctx, owner, repo, number, renderSupersessionRetractedComment(notice.GetBody())); err != nil {
		return false, err
	}
	if err := c.RemoveLabel(ctx, prRepo, number, SupersededLabel); err != nil { // writeback:gated
		return false, fmt.Errorf("removing %s from reopened PR %s#%d: %w", SupersededLabel, prRepo, number, err)
	}
	return true, nil
}
