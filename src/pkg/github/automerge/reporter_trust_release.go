package automerge

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// reporterTrustReleaseMarker opens the comment the stage-label pass leaves
// when it lifts a reporter-trust hold the hive had put on its own PR
// (hivecommons/hive#15020).
const reporterTrustReleaseMarker = "<!-- hive:reporter-trust-release -->"

// holdProvenance reads who set the hold-class label a PR wears (holdLabel:
// the decision-needed gate label or a literal `hold`) and why: the newest
// event for that label names the actor and the time; a hive-set hold
// is attributed to the reporter-trust gate when the gate's notice comment
// (hivecommons/hive#9665) is on the thread and no later unlabel event has
// cleared it. The second result is the gate's notice when one was read.
// nil, nil when nothing about the hold could be read.
func (c *Engine) holdProvenance(ctx context.Context, owner, repo string, number int, holdLabel string) (*stagelabels.HoldProvenance, *hgithub.ReporterTrustHoldNotice, error) {
	prov, notice, _, err := c.holdProvenanceWithComments(ctx, owner, repo, number, holdLabel)
	return prov, notice, err
}

// holdProvenanceWithComments is holdProvenance that also returns the PR's
// complete comment list it read (nil when it could not), so the stage pass's
// human-merge-path lifts do not list the comments a second time.
func (c *Engine) holdProvenanceWithComments(ctx context.Context, owner, repo string, number int, holdLabel string) (*stagelabels.HoldProvenance, *hgithub.ReporterTrustHoldNotice, []*gh.IssueComment, error) {
	prov, notice, comments, err := c.readHoldProvenance(ctx, owner, repo, number, holdLabel)
	if prov != nil && !strings.EqualFold(holdLabel, hgithub.LegacyHiveHoldLabel) {
		prov.Label = holdLabel
	}
	return prov, notice, comments, err
}

func (c *Engine) readHoldProvenance(ctx context.Context, owner, repo string, number int, holdLabel string) (*stagelabels.HoldProvenance, *hgithub.ReporterTrustHoldNotice, []*gh.IssueComment, error) {
	if c == nil || c.gh == nil || c.transport == nil {
		return nil, nil, nil, nil
	}
	appBot := strings.TrimSpace(c.transport.AppBotLogin())
	comments, err := c.listAllIssueComments(ctx, owner, repo, number)
	if err != nil {
		return nil, nil, nil, err
	}
	var notice *hgithub.ReporterTrustHoldNotice
	if n, ok := hgithub.ReporterTrustNoticeFromComments(comments, appBot); ok {
		notice = &n
	}
	latest, err := c.latestLabelEvent(ctx, owner, repo, number, holdLabel)
	if err != nil {
		return nil, notice, comments, err
	}
	if latest != nil && latest.GetEvent() == "unlabeled" {
		// The current label, if any, postdates what the events show; say
		// nothing rather than attribute it to a stale actor.
		return nil, notice, comments, nil
	}
	if latest == nil {
		if notice == nil {
			return nil, nil, comments, nil
		}
		prov := notice.Finding.HoldProvenance(notice.PostedAt)
		return prov, notice, comments, nil
	}
	actor := strings.ToLower(strings.TrimSpace(hgithub.SafeGetLogin(latest.GetActor())))
	since := latest.GetCreatedAt().Time
	if actor == "" || !strings.EqualFold(actor, appBot) {
		return &stagelabels.HoldProvenance{By: actor, Source: stagelabels.HoldSourceLabel, Since: since}, notice, comments, nil
	}
	// The hive set it. The reporter-trust notice explains it when it is the
	// hive's reason on record (posted around the label, never before an
	// intervening unlabel — latestLabelEvent already ruled that out).
	if notice != nil && !notice.PostedAt.Before(since.Add(-reporterTrustNoticeSlack)) {
		return notice.Finding.HoldProvenance(since), notice, comments, nil
	}
	// The human-merge-path rule now applies hgithub.HumanMergePathLabel,
	// never `hold` (humanMergePathLabelProvenance). A `hold` the App wrote
	// beside the notice from when the rule applied `hold` is that rule's
	// until the boot migration swaps it, and reads as a person's merge
	// (projectbluefin/utah#1097: told "the lane merges on its next pass"
	// while the notice said a person must merge it). A newer notice explains
	// no hold.
	if strings.EqualFold(holdLabel, hgithub.LegacyHumanMergePathHoldLabel) && legacyHumanMergePathNoticed(comments, appBot) {
		return &stagelabels.HoldProvenance{By: stagelabels.HoldByHive, Source: stagelabels.HoldSourceHumanMergePath,
			Reason: "it changes " + stagelabels.HumanMergePathReason, Since: since}, notice, comments, nil
	}
	return &stagelabels.HoldProvenance{By: stagelabels.HoldByHive, Source: stagelabels.HoldSourceLabel, Since: since}, notice, comments, nil
}

// legacyHumanMergePathNoticed reports whether the App posted the
// human-merge-path notice from when the rule applied `hold`.
func legacyHumanMergePathNoticed(comments []*gh.IssueComment, appBot string) bool {
	for _, cm := range comments {
		if cm != nil && strings.EqualFold(hgithub.SafeGetLogin(cm.GetUser()), appBot) && hgithub.IsLegacyHumanMergePathHoldNotice(cm.GetBody()) {
			return true
		}
	}
	return false
}

// humanMergePathNoticed reports whether the App posted the human-merge-path
// notice on the thread.
func humanMergePathNoticed(comments []*gh.IssueComment, appBot string) bool {
	for _, cm := range comments {
		if cm != nil && strings.EqualFold(hgithub.SafeGetLogin(cm.GetUser()), appBot) && strings.Contains(cm.GetBody(), hgithub.HumanMergePathMarker) {
			return true
		}
	}
	return false
}

// humanMergePathLabelProvenance reports whether the human-merge-path rule
// put hgithub.HumanMergePathLabel on PR number: the App posted the rule's
// notice on the thread and the newest labeled or unlabeled event for the
// label is the App's application. The rule posts its notice once per PR and
// re-adds the label on later heads, so the notice explains every App
// application. A label a person added (or re-added) last is theirs: false.
// It also returns the PR's complete comment list it read, for the lifts.
func (c *Engine) humanMergePathLabelProvenance(ctx context.Context, owner, repo string, number int) (bool, []*gh.IssueComment, error) {
	if c == nil || c.gh == nil || c.transport == nil {
		return false, nil, nil
	}
	appBot := strings.TrimSpace(c.transport.AppBotLogin())
	if appBot == "" {
		return false, nil, nil
	}
	comments, err := c.listAllIssueComments(ctx, owner, repo, number)
	if err != nil {
		return false, nil, err
	}
	if !humanMergePathNoticed(comments, appBot) {
		return false, comments, nil
	}
	latest, err := c.latestLabelEvent(ctx, owner, repo, number, hgithub.HumanMergePathLabel)
	if err != nil {
		return false, comments, err
	}
	if latest == nil || latest.GetEvent() != "labeled" {
		return false, comments, nil
	}
	return strings.EqualFold(strings.TrimSpace(hgithub.SafeGetLogin(latest.GetActor())), appBot), comments, nil
}

// reporterTrustNoticeSlack is how far before the `hold` label event the
// gate's notice may have been posted and still explain it: the watcher
// labels first and comments seconds later, but a stale notice from an
// earlier, released hold must not explain a fresh one.
const reporterTrustNoticeSlack = 2 * time.Minute

func (c *Engine) listAllIssueComments(ctx context.Context, owner, repo string, number int) ([]*gh.IssueComment, error) {
	var all []*gh.IssueComment
	opts := &gh.IssueListCommentsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	for {
		comments, resp, err := c.gh.Issues.ListComments(ctx, owner, repo, number, opts)
		if err != nil {
			return nil, fmt.Errorf("listing comments: %w", err)
		}
		all = append(all, comments...)
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return all, nil
}

// wornHoldLabel names the hold-class label whose provenance the stage pass
// reads: the decision-needed gate label the reporter-trust gate applies
// (hivecommons/hive#18468), else a literal `hold`, else "".
func wornHoldLabel(labels []string) string {
	if gate := hgithub.DecisionNeededLabelName(); hasLabelFold(labels, gate) {
		return gate
	}
	if hasLabelFold(labels, hgithub.LegacyHiveHoldLabel) {
		return hgithub.LegacyHiveHoldLabel
	}
	return ""
}

// latestLabelEvent returns the newest labeled / unlabeled event for the
// named label, nil when there is none.
func (c *Engine) latestLabelEvent(ctx context.Context, owner, repo string, number int, name string) (*gh.IssueEvent, error) {
	var latest *gh.IssueEvent
	opts := &gh.ListOptions{PerPage: 100}
	for {
		events, resp, err := c.gh.Issues.ListIssueEvents(ctx, owner, repo, number, opts)
		if err != nil {
			return nil, fmt.Errorf("listing issue events: %w", err)
		}
		for _, event := range events {
			if event == nil || !strings.EqualFold(event.GetLabel().GetName(), name) {
				continue
			}
			if event.GetEvent() != "labeled" && event.GetEvent() != "unlabeled" {
				continue
			}
			if latest == nil || event.GetCreatedAt().Time.After(latest.GetCreatedAt().Time) {
				latest = event
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return latest, nil
}

// releaseSelfImposedReporterTrustHold lifts a reporter-trust hold the hive
// put on a PR it authored itself (hivecommons/hive#15020): the hive is the
// author, so the issue reporter's trust is not what decides whether its own
// fix merges. prov must be a hive-set reporter-trust hold (the caller checked
// SelfImposed && ReporterTrust) and the PR hive-authored. Removes the
// decision-needed gate label (never a literal `hold`), and
// `needs-human` when the gate's notice says it added it, and leaves a short
// comment. Holds a person set are never touched: the caller only reaches
// this with prov.By == "hive".
func (c *Engine) releaseSelfImposedReporterTrustHold(ctx context.Context, obs stageObservation, prov *stagelabels.HoldProvenance, notice *hgithub.ReporterTrustHoldNotice) (bool, error) {
	if c == nil || c.gh == nil || prov == nil || !prov.SelfImposed() || !prov.ReporterTrust() {
		return false, nil
	}
	remove := []string{hgithub.DecisionNeededLabelName()}
	if notice != nil && notice.Finding.OwnsNeedsHuman && hasLabelFold(obs.Labels, "needs-human") {
		remove = append(remove, "needs-human")
	}
	for _, label := range remove {
		if _, err := c.gh.Issues.RemoveLabelForIssue(ctx, obs.Owner, obs.Repo, obs.Number, url.PathEscape(label)); err != nil && !isGitHubStatus(err, http.StatusNotFound) {
			return false, fmt.Errorf("removing %s label: %w", label, err)
		}
	}
	body := reporterTrustReleaseMarker + "\nHold released: the hive had held its own PR for " + strings.TrimSpace(prov.Reason) +
		". The hive is this PR's author, so the issue reporter's trust does not gate its merge; the automerge lane re-evaluates it on its next pass. A hold a person sets is never released this way."
	if _, _, err := c.gh.Issues.CreateComment(ctx, obs.Owner, obs.Repo, obs.Number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil {
		return false, fmt.Errorf("posting release comment: %w", err)
	}
	c.noteOwnStageComment(obs.Owner, obs.Repo, obs.Number)
	c.info("stage labels: releasing self-imposed reporter-trust hold", "repo", obs.Display, "pr", obs.Number, "reason", prov.Reason, "issue", prov.Issue, "reporter", prov.Reporter, "removed", strings.Join(remove, ","))
	return true, nil
}

// IsReporterTrustReleaseComment reports whether body is the comment
// releaseSelfImposedReporterTrustHold leaves.
func IsReporterTrustReleaseComment(body string) bool {
	return strings.HasPrefix(strings.TrimSpace(body), reporterTrustReleaseMarker)
}
