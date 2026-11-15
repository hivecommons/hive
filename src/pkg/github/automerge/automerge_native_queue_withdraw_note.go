package automerge

import (
	"context"
	"fmt"
	"strings"

	gh "github.com/google/go-github/v72/github"

	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/prstatus"
	"github.com/hivecommons/hive/pkg/writeback"
)

// nativeQueueWithdrawnMarker names the status section that says why the
// hive took a PR out of GitHub's merge queue.
const nativeQueueWithdrawnMarker = "<!-- hive:native-queue-withdrawn -->"

// prStatusSectionWriter is the GitHub client's status-comment writer
// (hgithub.Client.UpsertPRStatusSection): one hive status comment per PR,
// edited in place.
type prStatusSectionWriter interface {
	UpsertPRStatusSection(ctx context.Context, repo string, number int, key, text string) (bool, error)
}

// noteNativeQueueWithdrawn writes one status line on a PR the hive just
// took out of GitHub's merge queue, saying why, so a person reading the PR
// sees that the label they added stopped the merge. It is a section of the
// PR's single hive status comment (edited in place, never a second
// comment) and goes through the write-back gate, as the PR may be another
// bot's. A failure is logged; the dequeue already happened.
func (c *Engine) noteNativeQueueWithdrawn(ctx context.Context, owner, repo string, pr *gh.PullRequest, reason string) {
	w, ok := c.transport.(prStatusSectionWriter)
	if !ok || pr == nil {
		return
	}
	text := nativeQueueWithdrawnNote(pr.GetHead().GetSHA(), reason)
	author := hgithub.SafeGetLogin(pr.GetUser())
	app := strings.TrimSpace(c.transport.AppBotLogin())
	item := writeback.Item{
		Number:       pr.GetNumber(),
		IsPR:         true,
		Author:       author,
		HiveAuthored: app != "" && strings.EqualFold(author, app),
		Labels:       labelNames(pr.Labels),
		CreatedAt:    pr.GetCreatedAt().Time,
	}
	if d := c.writebackAllowed(owner+"/"+repo, item, writeback.KindHoldGateNotice, text); !d.Allowed {
		c.info("native merge queue withdrawal note withheld by the write-back gate", "repo", owner+"/"+repo, "pr", pr.GetNumber(), "reason", d.Reason)
		return
	}
	if _, err := w.UpsertPRStatusSection(ctx, owner+"/"+repo, pr.GetNumber(), prstatus.KeyNativeQueueWithdrawn, text); err != nil {
		c.warn("native merge queue withdrawal note failed", "repo", owner+"/"+repo, "pr", pr.GetNumber(), "error", err)
	}
}

// nativeQueueWithdrawnNote is the status line for a PR taken out of the
// merge queue at head for the lane reason reason.
func nativeQueueWithdrawnNote(head, reason string) string {
	why, next := "it is no longer eligible to merge", "The hive queues it again once nothing blocks it."
	switch {
	case reason == "held" || reason == "label:hold":
		why = "a hold label was added"
	case strings.HasPrefix(reason, "label:"):
		why = "the `" + strings.TrimPrefix(reason, "label:") + "` label was added"
	case strings.HasPrefix(reason, "excluded-label:"):
		why = "the `" + strings.TrimPrefix(reason, "excluded-label:") + "` label keeps it out of this lane"
	case reason == autoMergeReasonHumanMergeRequired:
		why, next = "its title or labels now say the owner merges it", "The hive will not queue it again; the owner merges it."
	}
	if len(head) > 9 {
		head = head[:9]
	}
	return fmt.Sprintf("%s\nRemoved from GitHub's merge queue at `%s`: %s (`%s`). %s", nativeQueueWithdrawnMarker, head, why, reason, next)
}
