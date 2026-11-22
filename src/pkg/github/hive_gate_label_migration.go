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

// HumanMergePathRelabelMarker marks the one comment the migration leaves on
// a PR whose human-merge-path `hold` it swapped for HumanMergePathLabel. The
// human-merge-path releases read it as the label's first application
// (humanMergePathHoldProven).
const HumanMergePathRelabelMarker = "<!-- hive:hold-relabel-human-merge-path -->"

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
//
// A `hold` the human-merge-path rule applied before it used
// HumanMergePathLabel is swapped for that label on stricter evidence
// (migrateHumanMergePathHold): the rule's own legacy notice, no other hold
// source, and every `hold` event on the PR the App's.
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
		return c.migrateHumanMergePathHold(ctx, owner, repo, item, comments)
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

// migrateHumanMergePathHold swaps, once, the `hold` the human-merge-path rule
// applied before it used HumanMergePathLabel for that label, so the PR's
// fixer, CI repair, base refreshes and review run again while only a person
// merges it. Every piece of evidence must hold, or the `hold` stays: the PR
// is a pull request; the App's notice from when the rule applied `hold`
// (IsLegacyHumanMergePathHoldNotice) is on the thread; no earlier swap
// comment is (the swap happens once, and a `hold` back after it is not the
// rule's); every `hold` labeled or unlabeled event is the App's and the
// newest is an application (no person ever touched it); and nothing else
// explains the hold (OtherHoldSourcesForPR). The caller has already found
// no other hive gate notice. The label is added before `hold` comes off, so
// the PR is never mergeable in between (the merge lanes re-check the files
// either way), and one comment explains the swap.
func (c *Client) migrateHumanMergePathHold(ctx context.Context, owner, repo string, item *gh.Issue, comments []*gh.IssueComment) (bool, error) {
	if item == nil || !item.IsPullRequest() {
		return false, nil
	}
	notice := false
	for _, comment := range comments {
		if comment == nil || !c.isTrustedAppBotCommentAuthor(comment) {
			continue
		}
		body := comment.GetBody()
		if strings.Contains(body, HumanMergePathRelabelMarker) {
			return false, nil
		}
		if IsLegacyHumanMergePathHoldNotice(body) {
			notice = true
		}
	}
	if !notice {
		return false, nil
	}
	number := item.GetNumber()
	// On a hive whose own hold label is `hold` (no hive_id), the dashboard
	// Hold button and the admin MCP hold write `hold` as the App too, with no
	// notice and no ledger entry: after a lift, an App re-application may be
	// a person's hold. There only the rule's single, never-removed
	// application is provably the rule's; lift and re-hold cycles are
	// accepted only when the hive's hold label is its own hive-pause/<id>.
	onlyApp, err := c.labelEventsOnlyAppApplied(ctx, owner, repo, number, LegacyHumanMergePathHoldLabel, !c.appHoldsAreTheGates())
	if err != nil || !onlyApp {
		return false, err
	}
	repoRef := owner + "/" + repo
	labels := extractLabels(item.Labels)
	var rest []string
	for _, label := range labels {
		if !strings.EqualFold(strings.TrimSpace(label), LegacyHumanMergePathHoldLabel) {
			rest = append(rest, label)
		}
	}
	if others := c.OtherHoldSourcesForPR(repoRef, number, rest, comments); len(others) > 0 {
		return false, nil
	}
	// A head that no longer touches a listed path has no reason left for
	// either label: the `hold` comes off with the stale lift's notice (the
	// lift used to do this; it now handles only the new label).
	staleHead, stale := c.humanMergePathStaleHead(ctx, owner, repo, number)
	if stale && HasHumanMergePathLiftNotice(comments, c.appBotLogin, staleHead) {
		// The stage lift's rule: a head the hive lifted once and that is
		// held again stays held; whatever held it again owns it now.
		return false, nil
	}
	body := humanMergePathRelabelBody()
	if stale {
		body = HumanMergePathLiftMarker + staleHead + " -->\n" + legacyHumanMergePathLiftNotice(staleHead)
	}
	author := safeGetLogin(item.GetUser())
	wbItem := writeback.Item{Number: number, IsPR: true, Author: author, Labels: labels, CreatedAt: item.GetCreatedAt().Time, HiveAuthored: c.IsHiveAuthorLogin(author)}
	// The human-merge-path release kind: this takes the rule's own `hold`
	// off, on the same floor as the releases that take its label off.
	if d := c.WritebackAllowed(repoRef, wbItem, writeback.KindHumanMergePathRelease, body); !d.Allowed {
		if c.logger != nil {
			c.logger.Info("hive gate label migration: human-merge-path hold kept; the write-back gate refused the swap", "repo", repoRef, "number", number, "reason", d.Reason)
		}
		return false, nil
	}
	if stale {
		// The files endpoint is not SHA-addressable: the head must still be
		// the one whose files were read.
		pr, _, err := c.client.PullRequests.Get(ctx, owner, repo, number)
		if err != nil {
			return false, fmt.Errorf("re-reading head: %w", err)
		}
		if pr.GetHead().GetSHA() != staleHead {
			return false, nil
		}
	} else if _, _, err := c.client.Issues.AddLabelsToIssue(ctx, owner, repo, number, []string{HumanMergePathLabel}); err != nil { // writeback:gated
		return false, fmt.Errorf("adding %s: %w", HumanMergePathLabel, err)
	}
	if _, err := c.client.Issues.RemoveLabelForIssue(ctx, owner, repo, number, url.PathEscape(LegacyHumanMergePathHoldLabel)); err != nil && !githubStatusError(err, http.StatusNotFound) { // writeback:gated
		return false, fmt.Errorf("removing hold: %w", err)
	}
	if _, _, err := c.client.Issues.CreateComment(ctx, owner, repo, number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil && c.logger != nil { // writeback:gated
		// The swap is the substance. Without the comment the pin-only and
		// owner-approval releases cannot prove the swapped label (the stale
		// lift still can); a person removes it then.
		c.logger.Warn("hive gate label migration: swapped human-merge-path hold but could not comment", "repo", repoRef, "number", number, "label", HumanMergePathLabel, "error", err)
	}
	if c.logger != nil {
		if stale {
			c.logger.Info("hive gate label migration: lifted hive-applied human-merge-path hold; current head touches no human-merge path", "repo", repoRef, "number", number, "head", staleHead)
		} else {
			c.logger.Info("hive gate label migration: swapped hive-applied human-merge-path hold", "repo", repoRef, "number", number, "label", HumanMergePathLabel)
		}
	}
	return true, nil
}

// humanMergePathStaleHead returns PR number's current head and true when the
// repository has human_merge_paths and that head's complete changed-file
// list touches none of them. Anything it cannot establish answers false.
func (c *Client) humanMergePathStaleHead(ctx context.Context, owner, repo string, number int) (string, bool) {
	patterns := c.humanMergePathsFor(owner + "/" + repo)
	if len(patterns) == 0 || c.client == nil {
		return "", false
	}
	head, matches, err := CurrentHumanMergePathMatches(ctx, c.client, owner, repo, number, patterns)
	if err != nil || head == "" || len(matches) > 0 {
		return "", false
	}
	return head, true
}

// legacyHumanMergePathLiftNotice is the migration's lift comment after its
// marker line, for a `hold` the rule applied on an earlier head that changed
// a listed path.
func legacyHumanMergePathLiftNotice(head string) string {
	return "Hold lifted: this PR no longer touches a path in `" + HumanMergePathsConfigKey + "` at " + head +
		". The hive set `hold` when an earlier head changed one (it now uses `" + HumanMergePathLabel + "` instead); the merge lanes take the PR from here. A `hold` a person sets is never lifted this way."
}

// appHoldsAreTheGates reports whether every `hold` the App writes is a gate's:
// the hive's own hold label (CanonicalHoldLabel, the dashboard and admin MCP
// hold) is set and is not `hold`. Unknown answers false.
func (c *Client) appHoldsAreTheGates() bool {
	label := strings.TrimSpace(c.CanonicalHoldLabel())
	return label != "" && !strings.EqualFold(label, LegacyHumanMergePathHoldLabel)
}

// labelEventsOnlyAppApplied reports whether every labeled or unlabeled event
// for label on item number is the App's and the newest is a `labeled`. With
// single, the history must be exactly one `labeled` event. No event, or no
// App login, answers false.
func (c *Client) labelEventsOnlyAppApplied(ctx context.Context, owner, repo string, number int, label string, single bool) (bool, error) {
	app := strings.TrimSpace(c.appBotLogin)
	if app == "" {
		return false, nil
	}
	seen := false
	count := 0
	latest := ""
	var latestAt gh.Timestamp
	opts := &gh.ListOptions{PerPage: 100}
	for {
		events, resp, err := c.client.Issues.ListIssueEvents(ctx, owner, repo, number, opts)
		if err != nil {
			return false, fmt.Errorf("listing issue events for %s/%s#%d: %w", owner, repo, number, err)
		}
		for _, event := range events {
			if event == nil || !strings.EqualFold(event.GetLabel().GetName(), label) {
				continue
			}
			kind := event.GetEvent()
			if kind != "labeled" && kind != "unlabeled" {
				continue
			}
			if !strings.EqualFold(safeGetLogin(event.GetActor()), app) {
				return false, nil
			}
			if !seen || event.GetCreatedAt().After(latestAt.Time) {
				latest, latestAt = kind, event.GetCreatedAt()
			}
			seen = true
			count++
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	if single && count != 1 {
		return false, nil
	}
	return seen && latest == "labeled", nil
}

// humanMergePathRelabelBody is the one comment the migration leaves on a PR
// whose human-merge-path `hold` it swapped.
func humanMergePathRelabelBody() string {
	return HumanMergePathRelabelMarker + "\nThe hive swapped the `hold` it had applied here for a human-merge path (`" + HumanMergePathsConfigKey + "`) for `" + HumanMergePathLabel + "`. It is not a hold: the hive's agents fix, review and update this PR again, and only a person merges it. A `hold` a person adds still means hands-off (hivecommons/hive#18468)."
}
