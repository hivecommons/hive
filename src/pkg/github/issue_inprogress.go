package github

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/issueclaim"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// The pr-in-progress stage label (`stage/030-pr-in-progress`, formerly
// `hive/in-progress`): a GitHub-visible, timestamped "an agent has started a
// PR for this issue" signal.
//
// Before this label the only forge-visible evidence that the scanner had
// picked an issue was the pull request itself. A kick lists up to
// governor.kick_limits.max_issues issues and the agent starts on few of them,
// so the hub cannot know which one it chose; the agent's own `hive-open-issue
// claim` request is the first moment anyone can. That request is the start
// signal (#10527) and this file turns it into a label plus a `hive:claim`
// marker comment carrying who, which hive and when, so a maintainer reading
// the issue, a second hive scanning the same repo, and the SLA metrics all
// read ONE record.
//
// Lifecycle:
//   - applied by the issue-request watcher on a `claim` request, next to
//     `hive/claimed-by-<agent>`; the marker comment is posted here only when
//     governor.claims is off — with it on, the ledger's 🔒 comment carries the
//     same marker (ClaimComment includes at=);
//   - removed by the pr-request watcher the moment a PR naming the issue
//     opens (the issue moves to `stage/040-covered-by-pr`), by the
//     ledger release hook (claims on), and by the enumeration sweep when the
//     marker has lapsed (agent TTL) or the issue carries PR evidence;
//   - while present with a live marker the issue is withheld from every kick
//     on every hive that enumerates the repo, whether or not that hive has
//     governor.claims on.

// InProgressLabel marks an issue a hive agent has started a PR for. It is
// the `stage/030-pr-in-progress` stage label (hivecommons/hive#12436);
// the `hive/in-progress` label it replaced is claims.LegacyLabelInProgress
// and is only recognised, never written, during the migration cycle.
var InProgressLabel = claims.LabelPRInProgress

const (
	// inProgressLabelDescription seeds the label when a repo does not define
	// it yet; the colour is the shared stage-family amber.
	inProgressLabelDescription = "An agent kick is running; no PR yet"
	// inProgressWorkBucket is the RepoWork detail bucket for a withheld issue.
	inProgressWorkBucket = "in_progress"
)

// inProgressTriggerLabels are the labels that make the sweep read the
// hive:claim marker: the live claimed / pr-in-progress stages and the
// retired `hive/in-progress`. The marker, not the label, decides whether
// the issue is withheld.
var inProgressTriggerLabels = []string{claims.LabelPRInProgress, claims.LabelClaimed, claims.LegacyLabelInProgress}

func hasInProgressTrigger(labels []string) bool {
	for _, l := range inProgressTriggerLabels {
		if issueHasLabel(labels, l) {
			return true
		}
	}
	return false
}

// InProgressSetting is the live hive/in-progress configuration.
type InProgressSetting struct {
	// Enabled turns the label, marker comment, sweep and kick filter on.
	Enabled bool
	// HiveID names this hive in the marker (hive=<id>).
	HiveID string
	// TTL is how long the start mark stands without a PR before the sweep
	// clears it. Zero → claims.DefaultAgentTTL.
	TTL time.Duration
}

// SetInProgress installs the live setting resolver. Nil disables.
func (c *Client) SetInProgress(fn func() InProgressSetting) {
	if c == nil {
		return
	}
	c.inProgress = fn
}

func (c *Client) inProgressSetting() InProgressSetting {
	if c == nil || c.inProgress == nil {
		return InProgressSetting{}
	}
	set := c.inProgress()
	if set.TTL <= 0 {
		set.TTL = claims.DefaultAgentTTL
	}
	return set
}

// InProgressCommentBody renders the start-mark comment: the canonical
// `hive:claim` marker (kind=agent, with at= and until=) so pkg/issueclaim and
// pkg/claims parse it like any other claim, then a human line that names the
// agent, the hive and both instants so nobody has to decode the marker.
func InProgressCommentBody(agent, hiveID string, started, expires time.Time) string {
	var b strings.Builder
	b.WriteString(claims.ClaimMarker(claims.Claim{
		Holder: agent, HolderID: agent, Kind: claims.KindAgent, Hive: hiveID,
		ClaimedAt: started, ExpiresAt: expires,
	}))
	b.WriteString("\n🛠️ Hive agent `")
	b.WriteString(agent)
	b.WriteString("`")
	if hiveID != "" {
		b.WriteString(" (hive ")
		b.WriteString(hiveID)
		b.WriteString(")")
	}
	fmt.Fprintf(&b, " started work on this issue at %s. A pull request referencing this issue supersedes this mark; it lapses on its own at %s if none appears.",
		started.UTC().Format(time.RFC3339), expires.UTC().Format(time.RFC3339))
	return b.String()
}

// claimOwnershipLabel is the per-agent ownership label a `claim` request
// applies (App bots can't be assignees). The stage the claim implies is not
// a label the request writes: it is an InProgress fact for the stage engine
// (see Client.StageFacts).
func claimOwnershipLabel(agent string) string {
	return claims.ClaimedByLabel(claims.Claim{Holder: agent, Kind: claims.KindAgent})
}

// submitInProgressFact tells the stage engine an agent kick started on the
// issue — the timestamped "an agent started a PR for this" signal (#10527),
// which derives to stage/030-pr-in-progress — when the in-progress mark is
// enabled. The engine re-reads the issue and replaces whatever stage it
// carries, so a second claim on an issue whose earlier labels lapsed is
// written, not replayed (hivecommons/hive#12512).
func (c *Client) submitInProgressFact(ctx context.Context, repo string, number int, agent string, now time.Time) error {
	set := c.inProgressSetting()
	if !set.Enabled {
		return nil
	}
	return c.StageFacts().InProgress(ctx, c.IssueStageRef(repo, number), agent, now.Add(set.TTL))
}

// markInProgressStarted is the post-label half of a `claim` request: it seeds
// the label definition and, when governor.claims is not going to post the 🔒
// comment, posts the start-mark comment. Both are best-effort — the label
// add already succeeded and is the request's result; a failed comment is
// logged, never retried as a failed request (a retry would re-post it).
func (c *Client) markInProgressStarted(ctx context.Context, repo string, number int, agent string, now time.Time) {
	set := c.inProgressSetting()
	if !set.Enabled {
		return
	}
	if err := c.EnsureIssueLabel(ctx, repo, InProgressLabel, stagelabels.StageColor, inProgressLabelDescription); err != nil && c.logger != nil {
		c.logger.Warn("in-progress: ensuring label definition failed", "repo", repo, "issue", number, "error", err)
	}
	if claimsOn, _ := c.issueClaimsSetting(); claimsOn {
		return
	}
	body := InProgressCommentBody(agent, set.HiveID, now, now.Add(set.TTL))
	if err := c.CreateIssueCommentBudgeted(ctx, repo, number, body); err != nil && c.logger != nil {
		c.logger.Warn("in-progress: posting start mark failed", "repo", repo, "issue", number, "agent", agent, "error", err)
	}
}

// clearInProgressForPR moves every issue a freshly opened PR names from the
// claimed / pr-in-progress stage to covered-by-pr: each is a CoveredBy fact
// for the stage engine, which re-reads the issue, takes the claimed /
// pr-in-progress (and retired) labels off and puts 040 on, journaled
// against that read so a later PR on the same issue can do it again.
// Best-effort and off the request path: the PR is the stronger signal and
// the stage reconciler converges the labels on the next cycle anyway.
func (c *Client) clearInProgressForPR(ctx context.Context, repo string, prNumber int, issues []int) {
	if !c.inProgressSetting().Enabled || len(issues) == 0 {
		return
	}
	pr := c.PRStageRef(repo, prNumber)
	for _, n := range issues {
		if n <= 0 {
			continue
		}
		if err := c.EnsureCoveredByPRLabel(ctx, repo); err != nil {
			if c.logger != nil {
				c.logger.Warn("in-progress: ensuring covered-by-pr label after PR open failed", "repo", repo, "issue", n, "error", err)
			}
			continue
		}
		if err := c.StageFacts().CoveredBy(ctx, c.IssueStageRef(repo, n), pr); err != nil && c.logger != nil {
			c.logger.Warn("in-progress: moving issue to covered-by-pr after PR open failed", "repo", repo, "issue", n, "error", err)
		}
	}
}

// SweepInProgressIssues reconciles the claimed / pr-in-progress stage labels
// (and the retired hive/in-progress) on the actionable issues and withholds
// the ones still legitimately in progress. For each issue carrying one:
//   - PR evidence on the issue (covered-by-pr, likely-done or a linked PR)
//     supersedes the mark: the label comes off, the issue stays as the
//     PR-claim guard left it;
//   - otherwise the latest hive:claim marker is read (through the
//     updated_at-keyed claim cache, so one comment fetch per activity
//     change): a live marker withholds the issue from this kick; a lapsed or
//     missing one takes the label off and leaves the issue actionable.
//
// A comment fetch failure keeps the label and withholds the issue (fail
// closed for one cycle — the mark was asserted; an unreadable one must not
// hand the issue to a second agent). It returns the number withheld.
func SweepInProgressIssues(ctx context.Context, client *Client, result *ActionableResult, now time.Time, logger *slog.Logger) int {
	if client == nil || result == nil || len(result.Issues.Items) == 0 {
		return 0
	}
	set := client.inProgressSetting()
	if !set.Enabled {
		return 0
	}
	kept := make([]Issue, 0, len(result.Issues.Items))
	withheld := 0
	for _, issue := range result.Issues.Items {
		if !hasInProgressTrigger(issue.Labels) {
			kept = append(kept, issue)
			continue
		}
		if issueHasPREvidence(issue) {
			client.removeInProgressLabel(ctx, &issue, "pr evidence", logger)
			kept = append(kept, issue)
			continue
		}
		owner, repoName := client.splitRepo(issue.Repo)
		claim, live, err := client.issueClaimFor(ctx, owner, repoName, &issue, set.TTL, now)
		switch {
		case err != nil:
			if logger != nil {
				logger.Warn("in-progress: marker fetch failed; withholding the issue this cycle",
					"repo", issue.Repo, "issue", issue.Number, "error", err)
			}
			withheld++
			appendRepoWorkIssueDetail(result, issue, inProgressWorkBucket, "in progress; marker unreadable")
		case live:
			withheld++
			appendRepoWorkIssueDetail(result, issue, inProgressWorkBucket, "In progress by "+claim.Identity+" since "+startedLabel(claim))
			if logger != nil {
				logger.Info("withholding issue: in-progress stage with a live start mark",
					"repo", issue.Repo, "issue", issue.Number, "agent", claim.Identity,
					"started_at", claim.StartedAt, "expires_at", claim.ExpiresAt)
			}
		default:
			client.removeInProgressLabel(ctx, &issue, "start mark lapsed", logger)
			kept = append(kept, issue)
		}
	}
	if withheld == 0 {
		// Label removals may have edited issue.Labels in place; keep them.
		result.Issues.Items = kept
		return 0
	}
	result.Issues = IssueResultFromItems(kept)
	return withheld
}

func startedLabel(claim issueclaim.Claim) string {
	if claim.StartedAt.IsZero() {
		return "an unrecorded time"
	}
	return claim.StartedAt.UTC().Format(time.RFC3339)
}

// issueHasPREvidence reports whether the PR-claim guard already attached a
// pull request to the issue this cycle.
func issueHasPREvidence(issue Issue) bool {
	return len(issue.LinkedPRs) > 0 || issue.ClaimContext != nil ||
		HasCoveredByPRLabel(issue.Labels) || issueHasLabel(issue.Labels, LikelyDoneLabel)
}

func (c *Client) removeInProgressLabel(ctx context.Context, issue *Issue, why string, logger *slog.Logger) {
	for _, l := range inProgressTriggerLabels {
		if !issueHasLabel(issue.Labels, l) {
			continue
		}
		issue.Labels = removeIssueLabel(issue.Labels, l)
		if c.client == nil {
			continue
		}
		// Journaled against the list payload's updated_at: the same label
		// removed from the same issue once before (a lapsed earlier mark)
		// must not replay as "already applied" and stay on the issue.
		if err := c.removeLabelObserved(ctx, issue.Repo, issue.Number, l, observedState(issue.UpdatedAt)); err != nil {
			if logger != nil {
				logger.Warn("in-progress: removing label failed", "repo", issue.Repo, "issue", issue.Number, "label", l, "why", why, "error", err)
			}
			continue
		}
		if logger != nil {
			logger.Info("in-progress: label removed", "repo", issue.Repo, "issue", issue.Number, "label", l, "why", why)
		}
	}
}
