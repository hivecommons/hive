package github

import (
	"context"
	"fmt"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// Hive parks some of its own work on hard-suppress labels: the SHA-hold sweep
// applies `hold`, and a contributor relay applies `blocked` on a blocked
// verdict. Both drop the issue out of every lane, and before
// hivecommons/hive#12002 nothing in hive ever lifted them once the reason
// lapsed — #11732 kept the SHA `hold` after its fix PR merged, #11854 kept a
// demoted tooling-failure `blocked` through five merged PRs.
//
// ReconcileHiveHardSuppress is the reconcile: for each open issue carrying one
// of those labels it lifts the label only when BOTH
//
//   - hive provably applied it: the newest labeled/unlabeled event for the
//     label is a `labeled` by the App bot, and for `hold` an App-authored
//     SHA-hold notice (<!-- hive:sha-hold -->) was posted with it — so a hold
//     another subsystem applied through the same App on an operator's behalf
//     is not mistaken for the SHA sweep's; and
//   - the reason lapsed: a pull request cross-referencing the issue merged
//     after the label was applied, or (for `blocked`) every native GitHub
//     "blocked by" dependency of the issue is now closed (hivecommons/hive#12522
//     — a child parked on an open blocker is unblocked by the blocker's own
//     PR, which never mentions the child).
//
// A label a human applied (or re-applied) is never touched.

// DefaultHardSuppressReconcileLabels are the hard-suppress labels the
// reconcile considers.
var DefaultHardSuppressReconcileLabels = []string{"hold", "blocked"}

// hardSuppressLiftedMarker tags the one-line note the reconcile leaves.
const hardSuppressLiftedMarker = "<!-- hive:hard-suppress-lifted -->"

// hardSuppressReconcileMaxPages bounds the open-issue listing per label.
const hardSuppressReconcileMaxPages = 3

// HardSuppressReconcileConfig scopes one reconcile pass.
type HardSuppressReconcileConfig struct {
	Repos []string
	// Labels overrides DefaultHardSuppressReconcileLabels.
	Labels []string
}

// HardSuppressReconcileResult reports one reconcile pass.
type HardSuppressReconcileResult struct {
	Lifted  int      `json:"lifted"`
	Skipped int      `json:"skipped"`
	Items   []string `json:"items,omitempty"`
}

// ReconcileHiveHardSuppress lifts hive-applied hard-suppress labels whose
// reason has lapsed. See the file comment for the evidence it requires.
func (c *Client) ReconcileHiveHardSuppress(ctx context.Context, cfg HardSuppressReconcileConfig) (*HardSuppressReconcileResult, error) {
	if c == nil {
		return nil, ErrNoGitHubClient
	}
	result := &HardSuppressReconcileResult{}
	if strings.TrimSpace(c.appBotLogin) == "" {
		return result, nil
	}
	labels := cfg.Labels
	if len(labels) == 0 {
		labels = DefaultHardSuppressReconcileLabels
	}
	var firstErr error
	for _, repoRef := range cfg.Repos {
		owner, repo := c.splitRepo(repoRef)
		if owner == "" || repo == "" {
			continue
		}
		for _, label := range labels {
			label = strings.TrimSpace(label)
			if label == "" {
				continue
			}
			issues, err := c.listOpenIssuesWithLabel(ctx, owner, repo, label)
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			for _, issue := range issues {
				if issue == nil || issue.IsPullRequest() {
					continue
				}
				c.reconcileHardSuppressIssue(ctx, owner, repo, issue.GetNumber(), label, result)
			}
		}
	}
	return result, firstErr
}

func (c *Client) listOpenIssuesWithLabel(ctx context.Context, owner, repo, label string) ([]*gh.Issue, error) {
	opts := &gh.IssueListByRepoOptions{
		State:       "open",
		Labels:      []string{label},
		ListOptions: gh.ListOptions{PerPage: 100},
	}
	var out []*gh.Issue
	for page := 0; page < hardSuppressReconcileMaxPages; page++ {
		issues, resp, err := c.client.Issues.ListByRepo(ctx, owner, repo, opts)
		if err != nil {
			return out, fmt.Errorf("listing open %q issues for %s/%s: %w", label, owner, repo, err)
		}
		out = append(out, issues...)
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.ListOptions.Page = resp.NextPage
	}
	return out, nil
}

func (c *Client) reconcileHardSuppressIssue(ctx context.Context, owner, repo string, number int, label string, result *HardSuppressReconcileResult) {
	byApp, labeledAt, err := c.latestLabelEventByApp(ctx, owner, repo, number, label)
	if err != nil {
		c.logger.Warn("hard-suppress reconcile: could not read label events", "repo", repo, "issue", number, "label", label, "error", err)
		result.Skipped++
		return
	}
	if !byApp {
		result.Skipped++
		return
	}
	if strings.EqualFold(label, "hold") {
		own, err := c.shaHoldIsOwn(ctx, owner, repo, number)
		if err != nil || !own {
			result.Skipped++
			return
		}
	}
	mergedPR, err := c.prMergedSinceForIssue(ctx, owner, repo, number, labeledAt.Time)
	if err != nil {
		c.logger.Warn("hard-suppress reconcile: could not read timeline", "repo", repo, "issue", number, "label", label, "error", err)
		result.Skipped++
		return
	}
	reason := ""
	if mergedPR != "" {
		reason = mergedPR + " merged since"
	} else if strings.EqualFold(label, blockedLabel) {
		closed, err := c.blockersAllClosed(ctx, owner, repo, number)
		if err != nil {
			c.logger.Warn("hard-suppress reconcile: could not read \"blocked by\" dependencies", "repo", repo, "issue", number, "label", label, "error", err)
			result.Skipped++
			return
		}
		if len(closed) > 0 {
			reason = "its \"blocked by\" dependencies (" + strings.Join(closed, ", ") + ") have all closed"
		}
	}
	if reason == "" {
		result.Skipped++
		return
	}
	if _, err := c.client.Issues.RemoveLabelForIssue(ctx, owner, repo, number, label); err != nil {
		c.logger.Warn("hard-suppress reconcile: could not remove label", "repo", repo, "issue", number, "label", label, "error", err)
		result.Skipped++
		return
	}
	note := hardSuppressLiftedNote(label, labeledAt.Time, reason)
	if _, _, err := c.client.Issues.CreateComment(ctx, owner, repo, number, &gh.IssueComment{Body: gh.Ptr(note)}); err != nil {
		c.logger.Warn("hard-suppress reconcile: could not post note", "repo", repo, "issue", number, "label", label, "error", err)
	}
	result.Lifted++
	result.Items = append(result.Items, fmt.Sprintf("%s/%s#%d:%s", owner, repo, number, label))
	c.logger.Info("HARD-SUPPRESS-LIFT", "repo", repo, "issue", number, "label", label, "reason", reason)
}

// blockedLabel is the hard-suppress label a contributor relay applies on a
// blocked verdict; it is the only one a "blocked by" dependency can explain.
const blockedLabel = "blocked"

// blockersAllClosed returns the "owner/repo#N" refs of the issue's native
// GitHub "blocked by" dependencies when there is at least one and none is
// still open, or nil when the issue has no blockers or any remains open.
// Same-repo blockers are rendered as a bare #N.
func (c *Client) blockersAllClosed(ctx context.Context, owner, repo string, number int) ([]string, error) {
	blockers, err := c.fetchBlockedBy(ctx, owner, repo, number)
	if err != nil {
		return nil, err
	}
	if len(blockers) == 0 {
		return nil, nil
	}
	refs := make([]string, 0, len(blockers))
	for _, b := range blockers {
		if b.State != "closed" {
			return nil, nil
		}
		ref := fmt.Sprintf("#%d", b.Number)
		if !strings.EqualFold(b.Owner, owner) || !strings.EqualFold(b.Repo, repo) {
			ref = b.Owner + "/" + b.Repo + ref
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

// prMergedSinceForIssue returns the first pull request cross-referencing the
// issue that merged after since ("owner/repo#N"), or "" when none did.
func (c *Client) prMergedSinceForIssue(ctx context.Context, owner, repo string, number int, since time.Time) (string, error) {
	opts := &gh.ListOptions{PerPage: 100}
	for {
		events, resp, err := c.client.Issues.ListIssueTimeline(ctx, owner, repo, number, opts)
		if err != nil {
			return "", fmt.Errorf("listing timeline for %s/%s#%d: %w", owner, repo, number, err)
		}
		for _, event := range events {
			if event == nil || !strings.EqualFold(event.GetEvent(), "cross-referenced") {
				continue
			}
			src := event.GetSource()
			if src == nil || src.Issue == nil || !src.Issue.IsPullRequest() {
				continue
			}
			mergedAt := src.Issue.GetPullRequestLinks().GetMergedAt()
			if mergedAt.IsZero() || !mergedAt.After(since) {
				continue
			}
			ref := fmt.Sprintf("#%d", src.Issue.GetNumber())
			if full := src.Issue.GetRepository().GetFullName(); full != "" && !strings.EqualFold(full, owner+"/"+repo) {
				ref = full + ref
			}
			return ref, nil
		}
		if resp == nil || resp.NextPage == 0 {
			return "", nil
		}
		opts.Page = resp.NextPage
	}
}

// hardSuppressLiftedNote renders the one-line note; reason is a clause such
// as "#12 merged since" or "its \"blocked by\" dependencies (#3) have all closed".
func hardSuppressLiftedNote(label string, labeledAt time.Time, reason string) string {
	return fmt.Sprintf("%s\nHive lifted the `%s` label it applied on %s: %s, so the reason for the park no longer holds and the issue is back in the normal flow. Labels applied by a human are never lifted by this sweep (hivecommons/hive#12002).",
		hardSuppressLiftedMarker, label, labeledAt.UTC().Format(time.RFC3339), reason)
}
