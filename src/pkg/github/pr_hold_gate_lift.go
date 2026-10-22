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
// branch (#13603). The hold guard uses it to tell "a human reviewed and
// lifted on the current head" from "the branch moved after the human lifted".
//
// ok is false when the timeline carries no gate removal. The head's arrival
// time is taken from server-timestamped signals only, in this order: the
// `head_ref_force_pushed` timeline event whose commit_id is headSHA
// (event time), then the earliest check suite or commit status GitHub
// created for headSHA (both stamped when the SHA first reaches the
// repository). Commit committer dates are never used: git sets them
// client-side, so a backdated commit pushed after the lift would otherwise
// look covered. With neither signal the time is zero, which the guard reads
// as unknown and fails closed on.
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
		return lift, true, nil
	}
	if headSHA != "" {
		at, evidence, err := c.headFirstSeenAt(ctx, owner, repoName, headSHA)
		if err != nil {
			return holdguard.GateLift{}, false, err
		}
		lift.LatestPushAt, lift.PushEvidence = at, evidence
	}
	return lift, true, nil
}

// headFirstSeenAt returns the earliest server-stamped created_at GitHub
// recorded for sha across its check suites and commit statuses, naming which
// kind supplied it. Both are created when the SHA first arrives at the
// repository, so the earliest one dates the push that brought it. Zero when
// the SHA has neither.
func (c *Client) headFirstSeenAt(ctx context.Context, owner, repo, sha string) (time.Time, string, error) {
	var (
		earliest time.Time
		evidence string
	)
	note := func(at time.Time, kind string) {
		if at.IsZero() {
			return
		}
		if earliest.IsZero() || at.Before(earliest) {
			earliest, evidence = at, kind
		}
	}
	suiteOpts := &gh.ListCheckSuiteOptions{ListOptions: gh.ListOptions{PerPage: labelTimelinePageSize}}
	for {
		res, resp, err := c.client.Checks.ListCheckSuitesForRef(ctx, owner, repo, sha, suiteOpts)
		if err != nil {
			return time.Time{}, "", err
		}
		if res != nil {
			for _, s := range res.CheckSuites {
				note(s.GetCreatedAt().Time, holdguard.PushEvidenceCheckSuite)
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		suiteOpts.Page = resp.NextPage
	}
	statusOpts := &gh.ListOptions{PerPage: labelTimelinePageSize}
	for {
		statuses, resp, err := c.client.Repositories.ListStatuses(ctx, owner, repo, sha, statusOpts)
		if err != nil {
			return time.Time{}, "", err
		}
		for _, s := range statuses {
			note(s.GetCreatedAt().Time, holdguard.PushEvidenceStatus)
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		statusOpts.Page = resp.NextPage
	}
	return earliest, evidence, nil
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
