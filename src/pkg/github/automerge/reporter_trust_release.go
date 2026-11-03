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

// holdProvenance reads who set the `hold` label a PR wears and why: the
// newest `hold` label event names the actor and the time; a hive-set hold
// is attributed to the reporter-trust gate when the gate's notice comment
// (hivecommons/hive#9665) is on the thread and no later unlabel event has
// cleared it. The second result is the gate's notice when one was read.
// nil, nil when nothing about the hold could be read.
func (c *Engine) holdProvenance(ctx context.Context, owner, repo string, number int) (*stagelabels.HoldProvenance, *hgithub.ReporterTrustHoldNotice, error) {
	if c == nil || c.gh == nil || c.transport == nil {
		return nil, nil, nil
	}
	appBot := strings.TrimSpace(c.transport.AppBotLogin())
	comments, err := c.listAllIssueComments(ctx, owner, repo, number)
	if err != nil {
		return nil, nil, err
	}
	var notice *hgithub.ReporterTrustHoldNotice
	if n, ok := hgithub.ReporterTrustNoticeFromComments(comments, appBot); ok {
		notice = &n
	}
	latest, err := c.latestHoldLabelEvent(ctx, owner, repo, number)
	if err != nil {
		return nil, notice, err
	}
	if latest != nil && latest.GetEvent() == "unlabeled" {
		// The current label, if any, postdates what the events show; say
		// nothing rather than attribute it to a stale actor.
		return nil, notice, nil
	}
	if latest == nil {
		if notice == nil {
			return nil, nil, nil
		}
		prov := notice.Finding.HoldProvenance(notice.PostedAt)
		return prov, notice, nil
	}
	actor := strings.ToLower(strings.TrimSpace(hgithub.SafeGetLogin(latest.GetActor())))
	since := latest.GetCreatedAt().Time
	if actor == "" || !strings.EqualFold(actor, appBot) {
		return &stagelabels.HoldProvenance{By: actor, Source: stagelabels.HoldSourceLabel, Since: since}, notice, nil
	}
	// The hive set it. The reporter-trust notice explains it when it is the
	// hive's reason on record (posted around the label, never before an
	// intervening unlabel — latestHoldLabelEvent already ruled that out).
	if notice != nil && !notice.PostedAt.Before(since.Add(-reporterTrustNoticeSlack)) {
		return notice.Finding.HoldProvenance(since), notice, nil
	}
	return &stagelabels.HoldProvenance{By: stagelabels.HoldByHive, Source: stagelabels.HoldSourceLabel, Since: since}, notice, nil
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

// latestHoldLabelEvent returns the newest labeled / unlabeled event for the
// literal `hold` label, nil when there is none.
func (c *Engine) latestHoldLabelEvent(ctx context.Context, owner, repo string, number int) (*gh.IssueEvent, error) {
	return c.latestLabelEvent(ctx, owner, repo, number, "hold")
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
// SelfImposed && ReporterTrust) and the PR hive-authored. Removes `hold`, and
// `needs-human` when the gate's notice says it added it, and leaves a short
// comment. Holds a person set are never touched: the caller only reaches
// this with prov.By == "hive".
func (c *Engine) releaseSelfImposedReporterTrustHold(ctx context.Context, obs stageObservation, prov *stagelabels.HoldProvenance, notice *hgithub.ReporterTrustHoldNotice) (bool, error) {
	if c == nil || c.gh == nil || prov == nil || !prov.SelfImposed() || !prov.ReporterTrust() {
		return false, nil
	}
	remove := []string{"hold"}
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
