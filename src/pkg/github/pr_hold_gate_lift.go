package github

import (
	"context"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/holdguard"
)

// LatestHoldGateLift reads a PR's timeline and reports the most recent
// removal of a hold-gate label — any label the enumeration hold gate
// recognizes (HoldLabels, the configured extra hold labels, the sentinel
// alert label) — alongside the server-stamped time headSHA arrived on the
// branch, when a force-push produced it (#13603). The hold guard uses it to
// tell "a human reviewed and lifted on the current head" from "the branch
// moved after the human lifted".
//
// ok is false when the timeline carries no gate removal. The head's arrival
// time comes only from the `head_ref_force_pushed` timeline event whose
// commit_id is headSHA (GitHub stamps its created_at). Commit committer dates
// are never used: git sets them client-side, so a backdated commit pushed
// after the lift would otherwise look covered. Check suite / status times are
// not used either: they are keyed by SHA, so a SHA pushed elsewhere in the
// repository before the lift carries pre-lift times even when it reaches
// this branch afterwards. For an ordinary push the time is zero; the guard
// then relies on the head it observed itself while the PR was held
// (holdguard.LiftCoversObservedHead) and otherwise fails closed.
func (c *Client) LatestHoldGateLift(ctx context.Context, repo string, number int, headSHA string) (holdguard.GateLift, bool, error) {
	if c == nil {
		return holdguard.GateLift{}, false, ErrNoGitHubClient
	}
	owner, repoName := c.splitRepo(repo)
	headSHA = strings.TrimSpace(headSHA)
	opts := &gh.ListOptions{PerPage: labelTimelinePageSize}
	var (
		lift        holdguard.GateLift
		found       bool
		forcePushAt time.Time
	)
	for {
		page, resp, err := c.client.Issues.ListIssueTimeline(ctx, owner, repoName, number, opts)
		if err != nil {
			return holdguard.GateLift{}, false, err
		}
		for _, ev := range page {
			if ev == nil {
				continue
			}
			switch strings.ToLower(ev.GetEvent()) {
			case "unlabeled":
				label := strings.TrimSpace(ev.GetLabel().GetName())
				if label == "" || !c.isHoldGateLabel(label) {
					continue
				}
				at := ev.GetCreatedAt().Time
				if found && !at.After(lift.LiftedAt) {
					continue
				}
				actor := ev.GetActor()
				lift = holdguard.GateLift{
					LiftedAt:    at,
					ActorIsHive: c.isHiveAccount(actor),
					ActorIsBot:  isBotUser(actor),
				}
				found = true
			case "head_ref_force_pushed":
				// Only a force-push that produced the current head dates its
				// arrival; an earlier force-push followed by an ordinary push
				// says nothing about when the ordinary push landed.
				if headSHA == "" || !strings.EqualFold(strings.TrimSpace(ev.GetCommitID()), headSHA) {
					continue
				}
				if at := ev.GetCreatedAt().Time; at.After(forcePushAt) {
					forcePushAt = at
				}
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	if !found {
		return holdguard.GateLift{}, false, nil
	}
	if !forcePushAt.IsZero() {
		lift.LatestPushAt, lift.PushEvidence = forcePushAt, holdguard.PushEvidenceForcePush
	}
	return lift, true, nil
}

// isHoldGateLabel reports whether a single label is one that puts a PR in the
// hold set: the same predicate enumeration uses (isHeld) plus the sentinel
// alert label, which blocks approval and auto-merge the same way.
func (c *Client) isHoldGateLabel(label string) bool {
	if c.isHeld([]string{label}) {
		return true
	}
	sentinel := c.sentinelAlertLabel()
	return sentinel != "" && strings.EqualFold(strings.TrimSpace(label), sentinel)
}

// isBotUser reports whether user is a bot or GitHub App account, by account
// type or the `[bot]` login suffix GitHub gives App installations.
func isBotUser(user *gh.User) bool {
	if user == nil {
		return false
	}
	return strings.EqualFold(user.GetType(), "Bot") ||
		strings.HasSuffix(strings.ToLower(user.GetLogin()), "[bot]")
}
