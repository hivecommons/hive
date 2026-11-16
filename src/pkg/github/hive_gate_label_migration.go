package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/delegatedecision"
	"github.com/hivecommons/hive/pkg/holdguard"
	"github.com/hivecommons/hive/pkg/writeback"
)

// hiveGateRelabelMarker marks the one comment the migration leaves on an
// item whose hive-applied `hold` it swapped (hivecommons/hive#18468).
const hiveGateRelabelMarker = "<!-- hive:hold-relabel-18468 -->"

// maxHiveGateRelabelsPerRepo bounds one boot's migration per repository so
// a repo with a long tail of old holds cannot spend the shared API budget in
// one burst; the rest migrate on the next boot.
const maxHiveGateRelabelsPerRepo = 50

// hiveGateRelabelPageSize is the page size for the held-item listing.
const hiveGateRelabelPageSize = 100

// HiveGateLabelMigrationResult counts what one migration run did.
type HiveGateLabelMigrationResult struct {
	Relabeled int `json:"relabeled"`
	// Skipped counts `hold` labels left alone: a person applied them, or
	// nothing on the thread proves which hive gate applied them.
	Skipped int      `json:"skipped"`
	Errors  []string `json:"errors,omitempty"`
}

// MigrateHiveAppliedHolds swaps the literal `hold` the hive's own gates
// applied before hivecommons/hive#18468 for the gate label that says who acts
// next, across the active repositories, and makes sure both gate labels
// exist with their descriptions.
//
// A `hold` is migrated only on two pieces of evidence, the same pair the
// level-hold and #5117 releases require: the newest `hold` label event is a
// `labeled` by the hive's App, and an App-authored notice from one of the
// hive's gates is on the thread. Anything else is a person's `hold` (or one
// the hive cannot attribute) and is never touched. The SHA-hold sweep's
// `hold` is left to that sweep.
//
// The gate label is added before `hold` is removed, so the item is never
// unblocked in between, and one short comment explains the swap.
func (c *Client) MigrateHiveAppliedHolds(ctx context.Context) (HiveGateLabelMigrationResult, error) {
	var res HiveGateLabelMigrationResult
	if c == nil || c.client == nil {
		return res, ErrNoGitHubClient
	}
	if strings.TrimSpace(c.appBotLogin) == "" {
		// Without the App login no `hold` can be proven hive-applied.
		return res, nil
	}
	for _, repoRef := range c.activeRepos() {
		if err := c.EnsureHiveGateLabels(ctx, repoRef); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: ensuring gate labels: %v", repoRef, err))
		}
		c.migrateRepoHiveHolds(ctx, repoRef, &res)
	}
	if len(res.Errors) > 0 {
		return res, fmt.Errorf("hive gate label migration: %d error(s); first: %s", len(res.Errors), res.Errors[0])
	}
	return res, nil
}

func (c *Client) migrateRepoHiveHolds(ctx context.Context, repoRef string, res *HiveGateLabelMigrationResult) {
	owner, repo := c.splitRepo(repoRef)
	opts := &gh.IssueListByRepoOptions{
		State:       "open",
		Labels:      []string{LegacyHiveHoldLabel},
		ListOptions: gh.ListOptions{PerPage: hiveGateRelabelPageSize},
	}
	done := 0
	for {
		items, resp, err := c.client.Issues.ListByRepo(ctx, owner, repo, opts)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: listing held items: %v", repoRef, err))
			return
		}
		for _, item := range items {
			if item == nil || !hasExactLabel(extractLabels(item.Labels), LegacyHiveHoldLabel) {
				continue
			}
			if done >= maxHiveGateRelabelsPerRepo {
				return
			}
			migrated, err := c.migrateOneHiveHold(ctx, owner, repo, item)
			if err != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("%s#%d: %v", repoRef, item.GetNumber(), err))
				continue
			}
			if migrated {
				res.Relabeled++
				done++
			} else {
				res.Skipped++
			}
		}
		if resp == nil || resp.NextPage == 0 {
			return
		}
		opts.ListOptions.Page = resp.NextPage
	}
}

func (c *Client) migrateOneHiveHold(ctx context.Context, owner, repo string, item *gh.Issue) (bool, error) {
	number := item.GetNumber()
	byApp, _, err := c.latestHoldLabelEventByApp(ctx, owner, repo, number)
	if err != nil {
		return false, err
	}
	if !byApp {
		// A person put (or last re-put) this `hold` on: hands-off.
		return false, nil
	}
	comments, err := c.listIssueComments(ctx, owner, repo, number)
	if err != nil {
		return false, err
	}
	label, relabeledBefore := c.hiveHoldGateLabelFromComments(comments)
	if label == "" {
		return false, nil
	}
	repoRef := owner + "/" + repo
	// The relabel goes through the one write-back gate like every other
	// unsolicited write (a paused or write-restricted repo keeps its `hold`,
	// which still blocks; the next boot retries).
	wbItem := writeback.Item{Number: number, IsPR: item.IsPullRequest(), Author: safeGetLogin(item.GetUser()), Labels: extractLabels(item.Labels)}
	if d := c.WritebackAllowed(repoRef, wbItem, writeback.KindHoldGateNotice, hiveGateRelabelComment(label)); !d.Allowed {
		return false, nil
	}
	if _, _, err := c.client.Issues.AddLabelsToIssue(ctx, owner, repo, number, []string{label}); err != nil { // writeback:gated
		return false, fmt.Errorf("adding %s: %w", label, err)
	}
	if _, err := c.client.Issues.RemoveLabelForIssue(ctx, owner, repo, number, url.PathEscape(LegacyHiveHoldLabel)); err != nil && !githubStatusError(err, http.StatusNotFound) { // writeback:gated
		return false, fmt.Errorf("removing hold: %w", err)
	}
	if !relabeledBefore {
		if _, _, err := c.client.Issues.CreateComment(ctx, owner, repo, number, &gh.IssueComment{Body: gh.Ptr(hiveGateRelabelComment(label))}); err != nil && c.logger != nil { // writeback:gated
			// The label swap is the substance; the comment is the courtesy.
			c.logger.Warn("hive gate label migration: swapped hold but could not comment", "repo", repoRef, "number", number, "label", label, "error", err)
		}
	}
	if c.logger != nil {
		c.logger.Info("hive gate label migration: swapped hive-applied hold", "repo", repoRef, "number", number, "label", label)
	}
	return true, nil
}

// hiveHoldGateLabelFromComments maps the hive gate notice on a thread to the
// gate label that replaces its `hold`: the level gate's notice to
// ready-for-review, every decision gate's notice to decision-needed. A
// decision gate wins over the level gate, the same precedence the level-hold
// release uses. "" when no App-authored gate notice is present. The second
// result reports whether this migration already commented.
func (c *Client) hiveHoldGateLabelFromComments(comments []*gh.IssueComment) (string, bool) {
	level, decision, relabeled := false, false, false
	for _, comment := range comments {
		if comment == nil || !c.isTrustedAppBotCommentAuthor(comment) {
			continue
		}
		body := comment.GetBody()
		switch {
		case strings.Contains(body, hiveGateRelabelMarker):
			relabeled = true
		case IsReporterTrustHoldNotice(body), IsSelfAuthorizationHoldNotice(body),
			strings.Contains(body, holdguard.CommentHeadingPhrase),
			strings.Contains(body, delegatedecision.MarkerPrefix) && strings.Contains(body, "owner action required"):
			decision = true
		default:
			if _, ok := levelHoldAgentFromNotice(body); ok {
				level = true
			}
		}
	}
	switch {
	case decision:
		return DecisionNeededLabelName(), relabeled
	case level:
		return ReadyForReviewLabelName(), relabeled
	}
	return "", relabeled
}

func hiveGateRelabelComment(label string) string {
	_, desc, _ := hiveGateLabelDefinition(label)
	return fmt.Sprintf("%s\nThe hive swapped the `hold` it had applied here for `%s`: %s The hive no longer applies `hold` itself; a `hold` a person adds still means hands-off (hivecommons/hive#18468).",
		hiveGateRelabelMarker, label, desc)
}
