package automerge

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/writeback"
)

// humanMergePathLiftMarker opens the comment the stage-label pass leaves when
// it lifts a human-merge-path hold the PR's current head no longer explains.
// The head SHA follows it, so each head is noticed at most once.
const humanMergePathLiftMarker = "<!-- hive:human-merge-path-lift "

// humanMergePathStaleReason is the hold reason the row and the 085 comment
// carry while a stale human-merge-path hold stays on (a second hold source,
// or the write-back gate refused the lift): never "it changes a human-merge
// path" for a head that changes none.
const humanMergePathStaleReason = "a human-merge path an earlier head changed; the current head touches none"

// otherHoldSourcesTransport is the optional transport hook that lists a held
// PR's hold sources besides the human-merge-path refusal
// (hgithub.Client.OtherHoldSourcesForPR). Without it no hold is lifted.
type otherHoldSourcesTransport interface {
	OtherHoldSourcesForPR(repo string, number int, labels []string, comments []*gh.IssueComment) []string
}

// humanMergePathLiftNotice is the lift comment's text after its marker line.
func humanMergePathLiftNotice(head string) string {
	return "Hold lifted: this PR no longer touches a path in `" + hgithub.HumanMergePathsConfigKey +
		"` at " + head + ". The hive set `hold` when an earlier head changed one; the merge lanes take the PR from here. A hold a person sets is never lifted this way."
}

// hasHumanMergePathLiftComment reports whether the App already left the lift
// notice for head.
func hasHumanMergePathLiftComment(comments []*gh.IssueComment, appBot, head string) bool {
	for _, cm := range comments {
		if cm != nil && strings.EqualFold(hgithub.SafeGetLogin(cm.GetUser()), appBot) && strings.HasPrefix(strings.TrimSpace(cm.GetBody()), humanMergePathLiftMarker+head+" ") {
			return true
		}
	}
	return false
}

// staleHumanMergePathHold is the provenance a hive-set human-merge-path hold
// is recorded with once the current head touches no such path.
func staleHumanMergePathHold(prov *stagelabels.HoldProvenance) *stagelabels.HoldProvenance {
	return &stagelabels.HoldProvenance{By: stagelabels.HoldByHive, Source: stagelabels.HoldSourceLabel, Reason: humanMergePathStaleReason, Since: prov.Since}
}

// liftStaleHumanMergePathHold re-matches the current head of a PR the hive
// held for auto_merge.human_merge_paths against those paths, with the matcher
// the sweep lanes use. stale is true when the complete changed-file list
// matches none of them. It then removes `hold` and posts one notice for the
// head, but only when prov is the hive's own human-merge-path hold, no other
// hold source exists, the head has not been noticed before, the write-back
// gate allows human_merge_path_release, and the head did not move while the
// files were read. A hold a person set never reaches this (the caller only
// passes a hive-set provenance). Anything it cannot establish keeps the hold.
func (c *Engine) liftStaleHumanMergePathHold(ctx context.Context, obs stageObservation, prov *stagelabels.HoldProvenance) (lifted, stale bool, err error) {
	if c == nil || c.gh == nil || c.transport == nil || !prov.SelfImposed() || !prov.HumanMergePath() {
		return false, false, nil
	}
	repo := obs.Owner + "/" + obs.Repo
	patterns := c.currentHumanMergePaths(repo)
	if len(patterns) == 0 {
		return false, false, nil
	}
	head, matches, err := hgithub.CurrentHumanMergePathMatches(ctx, c.gh, obs.Owner, obs.Repo, obs.Number, patterns)
	if err != nil {
		c.debug("stage labels: human-merge-path hold kept; current head's files unreadable", "repo", obs.Display, "pr", obs.Number, "error", err)
		return false, false, nil
	}
	if len(matches) > 0 || head == "" {
		return false, false, nil
	}
	if obs.HeadSHA != "" && obs.HeadSHA != head {
		// The pass observed an older head; the next pass judges this one.
		return false, true, nil
	}
	others, ok := c.transport.(otherHoldSourcesTransport)
	if !ok {
		return false, true, nil
	}
	comments, err := c.listAllIssueComments(ctx, obs.Owner, obs.Repo, obs.Number)
	if err != nil {
		return false, true, fmt.Errorf("listing comments: %w", err)
	}
	if sources := others.OtherHoldSourcesForPR(repo, obs.Number, obs.Labels, comments); len(sources) > 0 {
		c.info("stage labels: human-merge-path hold no longer applies but another hold source keeps it", "repo", obs.Display, "pr", obs.Number, "head", head, "sources", strings.Join(sources, ","))
		return false, true, nil
	}
	appBot := strings.TrimSpace(c.transport.AppBotLogin())
	if hasHumanMergePathLiftComment(comments, appBot, head) {
		// Lifted once at this head and held again since: whatever held it
		// again owns it now, so the pass does not undo it a second time.
		return false, true, nil
	}
	body := humanMergePathLiftMarker + head + " -->\n" + humanMergePathLiftNotice(head)
	item := writebackItemForObservation(obs)
	item.HiveAuthored = strings.EqualFold(normalizeBotLogin(obs.Author), normalizeBotLogin(appBot))
	if d := c.writebackAllowed(repo, item, writeback.KindHumanMergePathRelease, ""); !d.Allowed {
		c.info("stage labels: human-merge-path hold no longer applies; lift refused by the write-back gate", "repo", obs.Display, "pr", obs.Number, "head", head, "reason", d.Reason)
		return false, true, nil
	}
	// The files endpoint is not SHA-addressable: confirm the head did not
	// move while it was paged before taking the hold off.
	current, _, err := c.gh.PullRequests.Get(ctx, obs.Owner, obs.Repo, obs.Number)
	if err != nil {
		return false, true, fmt.Errorf("re-reading head: %w", err)
	}
	if current.GetHead().GetSHA() != head {
		return false, true, nil
	}
	if _, err := c.gh.Issues.RemoveLabelForIssue(ctx, obs.Owner, obs.Repo, obs.Number, url.PathEscape(hgithub.HumanMergePathHoldLabel)); err != nil && !isGitHubStatus(err, http.StatusNotFound) { // writeback:gated
		return false, true, fmt.Errorf("removing %s label: %w", hgithub.HumanMergePathHoldLabel, err)
	}
	if _, _, err := c.gh.Issues.CreateComment(ctx, obs.Owner, obs.Repo, obs.Number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil { // writeback:gated
		return true, true, fmt.Errorf("posting lift comment: %w", err)
	}
	c.noteOwnStageComment(obs.Owner, obs.Repo, obs.Number)
	c.info("stage labels: lifted human-merge-path hold; current head touches no human-merge path", "repo", obs.Display, "pr", obs.Number, "head", head)
	return true, true, nil
}
