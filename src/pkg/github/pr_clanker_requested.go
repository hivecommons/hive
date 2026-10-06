package github

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// The clanker-requested policy (hivecommons/hive#10766) is the human-authored
// PR sibling of the #9665 reporter-trust hold. With
// project.issue_filter.reporter_trust.clanker_requested on, an open PR whose
// author the operator does not trust, and which did not come through the
// contributor relay, gets the configured label (a hold label, so the PR is
// parked out of review and merge automation) and a one-shot pointer to
// ClankeR. Only a person releases it: once anyone but the App removes the
// label, the App never applies it to that PR again.

// ClankerRequestedNoticeMarker marks the one-shot notice on a parked PR.
const ClankerRequestedNoticeMarker = "<!-- hive:clanker-requested -->"

// clankerRelayDocURL is the contributor-relay quick start the notices link to.
const clankerRelayDocURL = "https://github.com/hivecommons/hive/blob/v5/src/docs/contributor-relay.md#basic-setup"

// clankerRequestedPointer is the shared "what ClankeR is" paragraph used by
// the issue-side wait comment and the PR-side notice.
const clankerRequestedPointer = "This project takes AI-assisted contributions through **ClankeR**, the hive's contributor relay: " +
	"you run a small relay on your own machine with the AI CLI and model of your choice, and the hive hands it queued work " +
	"and coordinates the resulting PRs. To get started, follow [Basic setup](" + clankerRelayDocURL + ")."

// DefaultClankerRequestedMaxNotices caps new clanker-requested parkings per
// governor poll, for the same reason as DefaultReporterTrustWaitMaxNotices:
// switching the policy on must not label an entire PR backlog at once.
const DefaultClankerRequestedMaxNotices = DefaultReporterTrustWaitMaxNotices

func newClankerRequestedBudget() *reporterTrustWaitBudget {
	return &reporterTrustWaitBudget{remaining: DefaultClankerRequestedMaxNotices}
}

// SetRelayContributor installs the live relay-provenance predicate: true when
// login holds a live contributor claim, so its PRs came through the relay and
// are never parked. nil knows no relay contributors.
func (c *Client) SetRelayContributor(fn func(login string) bool) {
	if c == nil {
		return
	}
	c.reporterTrustMu.Lock()
	defer c.reporterTrustMu.Unlock()
	c.relayContributor = fn
}

func (c *Client) isRelayContributor(login string) bool {
	if c == nil {
		return false
	}
	c.reporterTrustMu.RLock()
	fn := c.relayContributor
	c.reporterTrustMu.RUnlock()
	if fn == nil {
		return false
	}
	return fn(login)
}

func clankerRequestedAddendum(cc ReporterTrustClankerConfig) string {
	if addendum := strings.TrimSpace(cc.ReporterTrustClankerRequestedAddendum()); addendum != "" {
		return "\n\n" + addendum
	}
	return ""
}

// clankerRequestedNotice is the one-shot comment on a parked PR.
func clankerRequestedNotice(label string, cc ReporterTrustClankerConfig) string {
	return ClankerRequestedNoticeMarker + "\n" +
		"Thanks for the PR. This hive only works changes from outside the project when they come through its contributor relay, " +
		"so this PR is parked with the `" + label + "` label until it is resubmitted through the relay or a maintainer removes the label.\n\n" +
		clankerRequestedPointer + clankerRequestedAddendum(cc)
}

// IsClankerRequestedNotice reports whether a PR comment is the parking notice.
func IsClankerRequestedNotice(body string) bool {
	return strings.Contains(body, ClankerRequestedNoticeMarker)
}

func hasClankerRequestedNotice(comments []*gh.IssueComment, appBotLogin string) bool {
	for _, comment := range comments {
		if comment == nil {
			continue
		}
		if strings.TrimSpace(appBotLogin) != "" && !strings.EqualFold(safeGetLogin(comment.GetUser()), appBotLogin) {
			continue
		}
		if IsClankerRequestedNotice(comment.GetBody()) {
			return true
		}
	}
	return false
}

// clankerRequestedLabelRemovedByHuman reports whether anyone other than the
// App has ever removed label from the PR — the permanent release.
func (c *Client) clankerRequestedLabelRemovedByHuman(ctx context.Context, owner, repo string, number int, label string) (bool, error) {
	opts := &gh.ListOptions{PerPage: 100}
	for {
		events, resp, err := c.client.Issues.ListIssueEvents(ctx, owner, repo, number, opts)
		if err != nil {
			return false, fmt.Errorf("listing issue events for %s/%s#%d: %w", owner, repo, number, err)
		}
		for _, event := range events {
			if event == nil || event.GetEvent() != "unlabeled" || !strings.EqualFold(strings.TrimSpace(event.GetLabel().GetName()), label) {
				continue
			}
			actor := safeGetLogin(event.GetActor())
			if strings.TrimSpace(c.appBotLogin) == "" || !strings.EqualFold(actor, c.appBotLogin) {
				return true, nil
			}
		}
		if resp == nil || resp.NextPage == 0 {
			return false, nil
		}
		opts.Page = resp.NextPage
	}
}

// parkClankerRequestedPR applies the clanker-requested label and its one-shot
// notice to an untrusted author's direct PR, and returns the PR's labels as
// they now stand. Every failure leaves the PR unparked: a park nobody can
// explain is worse than one a later poll applies.
func (c *Client) parkClankerRequestedPR(ctx context.Context, repo string, pr *gh.PullRequest, labels []string, budget *reporterTrustWaitBudget) []string {
	if c == nil || c.client == nil || pr == nil {
		return labels
	}
	cc, ok := reporterTrustClankerConfig(c.getIssueFilter())
	if !ok {
		return labels
	}
	label := strings.TrimSpace(cc.ReporterTrustClankerRequestedLabel())
	if label == "" || hasExactLabel(labels, label) {
		return labels
	}
	// App-authored (hive-open-pr), bot and hive-identity PRs are not a
	// stranger's direct PR.
	if !c.isHumanAuthor(pr.GetUser()) {
		return labels
	}
	login := strings.TrimSpace(pr.GetUser().GetLogin())
	if cc.ReporterTrustTrusts(login, strings.TrimSpace(pr.GetAuthorAssociation())) || c.isRelayContributor(login) {
		return labels
	}

	number := pr.GetNumber()
	owner, repoName := c.splitRepo(repo)
	comments, err := c.listIssueComments(ctx, owner, repoName, number)
	if err != nil {
		c.warnClankerRequested("comment scan failed", repo, number, err)
		return labels
	}
	if hasClankerRequestedNotice(comments, c.appBotLogin) {
		// Already parked once and the label is gone: released.
		return labels
	}
	released, err := c.clankerRequestedLabelRemovedByHuman(ctx, owner, repoName, number, label)
	if err != nil {
		c.warnClankerRequested("label history scan failed", repo, number, err)
		return labels
	}
	if released {
		return labels
	}
	if !budget.reserve() {
		if c.logger != nil {
			c.logger.Info("clanker-requested notice budget exhausted", slog.String("repo", repo), slog.Int("number", number))
		}
		return labels
	}

	if err := c.EnsureIssueLabel(ctx, repo, label, "ededed", "Parked until resubmitted through the ClankeR contributor relay"); err != nil {
		c.warnClankerRequested("label ensure failed", repo, number, err)
		return labels
	}
	if err := c.AddLabels(ctx, repo, number, []string{label}); err != nil {
		c.warnClankerRequested("label add failed", repo, number, err)
		return labels
	}
	if err := c.CreateIssueComment(ctx, repo, number, clankerRequestedNotice(label, cc)); err != nil {
		c.warnClankerRequested("comment post failed", repo, number, err)
		if removeErr := c.RemoveLabel(ctx, repo, number, label); removeErr != nil {
			c.warnClankerRequested("label cleanup failed", repo, number, removeErr)
		}
		return labels
	}
	c.recordWriteAudit(AuditActionHiveLabelApplied, hiveWriteMeta(),
		WriteTarget{Repo: c.reporterTrustAuditRepo(repo), Number: number}, "label", label, "reason", "clanker_requested")
	return append(labels, label)
}

func (c *Client) warnClankerRequested(msg, repo string, number int, err error) {
	if c == nil || c.logger == nil || err == nil {
		return
	}
	c.logger.Warn("clanker-requested "+msg, slog.String("repo", repo), slog.Int("number", number), slog.String("error", err.Error()))
}
