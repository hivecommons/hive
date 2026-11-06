package github

import (
	"context"
	"log/slog"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/writeback"
)

// reconcileDoneVerdictLabels settles an issue that wears both
// hive/already-done and hive/verified-open: the later verdict stands and the
// other label comes off. projectbluefin/testsuite#1023 got already-done at
// 11-04 01:47Z and verified-open at 03:13Z ("Nothing more can be implemented
// here until #706"), and the inbox offered it as "verified done but could
// not close". New verdicts remove the other label as they land (the label
// relay and the contribute verdict); this heals issues labelled before that.
// It reads the issue's events only for an issue wearing both, and writes
// through the write-back gate. Returns whether a label was removed.
func reconcileDoneVerdictLabels(ctx context.Context, client *Client, in IssueStageInput, logger *slog.Logger) bool {
	if client == nil || client.client == nil || !issueHasLabel(in.Labels, AlreadyDoneLabel) || !issueHasLabel(in.Labels, VerifiedOpenLabel) {
		return false
	}
	owner, name := client.splitRepo(in.Repo)
	done, open, err := client.latestLabeledAt(ctx, owner, name, in.Number, AlreadyDoneLabel, VerifiedOpenLabel)
	if err != nil || done.IsZero() || open.IsZero() || done.Equal(open) {
		if err != nil && logger != nil {
			logger.Warn("done verdict labels: reading issue events failed; retrying next sweep", "repo", in.Repo, "issue", in.Number, "error", err)
		}
		return false
	}
	stale := AlreadyDoneLabel
	if done.After(open) {
		stale = VerifiedOpenLabel
	}
	item, err := client.WritebackItemForNumber(ctx, in.Repo, in.Number)
	if err != nil {
		return false
	}
	if d := client.WritebackAllowed(owner+"/"+name, item, writeback.KindStageLabel, stale); !d.Allowed {
		return false
	}
	if err := client.RemoveLabelsFresh(ctx, in.Repo, in.Number, stale); err != nil { // writeback:gated
		if logger != nil {
			logger.Warn("done verdict labels: removing the older verdict failed", "repo", in.Repo, "issue", in.Number, "label", stale, "error", err)
		}
		return false
	}
	if logger != nil {
		logger.Info("done verdict labels: the later verdict stands", "repo", in.Repo, "issue", in.Number, "removed", stale)
	}
	return true
}

// latestLabeledAt returns when each of a and b was last added to the issue
// (zero when never seen in its events).
func (c *Client) latestLabeledAt(ctx context.Context, owner, name string, number int, a, b string) (atA, atB time.Time, err error) {
	opts := &gh.ListOptions{PerPage: 100}
	for {
		events, resp, err := c.client.Issues.ListIssueEvents(ctx, owner, name, number, opts)
		if err != nil {
			return atA, atB, err
		}
		for _, ev := range events {
			if ev.GetEvent() != "labeled" {
				continue
			}
			at := ev.GetCreatedAt().Time
			switch label := ev.GetLabel().GetName(); {
			case strings.EqualFold(label, a) && at.After(atA):
				atA = at
			case strings.EqualFold(label, b) && at.After(atB):
				atB = at
			}
		}
		if resp == nil || resp.NextPage == 0 {
			return atA, atB, nil
		}
		opts.Page = resp.NextPage
	}
}
