package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/escalation"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/scheduler"
)

// Claim escalation gate (hivecommons/hive#10527).
//
// An agent claim expires after agent_ttl_s and the issue goes back into the
// work lists, so nothing stopped the same agent re-claiming an issue forever:
// each claim looked like progress while nothing moved. The gate sits in the
// scheduler's in-flight seam, in front of the kick that would record the next
// claim. When one agent's last escalate_after_claims claims on a free issue
// were followed by no linked PR, referencing commit, label or assignee change
// and no close/reopen, the issue is withheld from every work list, labelled
// needs-human, and given one comment summarising the claims (ADR-0019). The
// needs-human label then keeps it out of the issue enumeration until a person
// removes it — which is itself progress, so the next claim goes ahead.

// claimProgressRecheck bounds how long a "nothing moved" verdict, or a failed
// timeline read, is reused before the timeline is read again. A verdict that
// something moved holds for as long as the run it was read for.
const claimProgressRecheck = 5 * time.Minute

// claimEscalationGate decides, per issue, whether a stalled run of agent
// claims withholds it.
type claimEscalationGate struct {
	ctx       context.Context
	ledger    *claims.Ledger
	org       string
	threshold int
	// events reads the issue timeline after since.
	events func(ctx context.Context, repo string, issue int, since time.Time) ([]github.IssueEvent, error)
	// escalate applies the needs-human label and posts the summary; called
	// once per stalled run.
	escalate func(claims.Stall)
	now      func() time.Time
	logger   *slog.Logger

	mu       sync.Mutex
	verdicts map[string]progressVerdict
}

// progressVerdict caches one timeline read for one stalled run.
type progressVerdict struct {
	since time.Time // Since of the run the verdict was read for
	allow bool
	until time.Time // zero: valid until the run changes
}

// claimEscalationLookup returns the gate as a scheduler in-flight lookup, or
// nil when the ledger is off or escalate_after_claims is negative.
func claimEscalationLookup(ctx context.Context, cfg *config.Config, ledger *claims.Ledger, client func() *github.Client, logger *slog.Logger) scheduler.InflightLookup {
	if ledger == nil || cfg == nil || client == nil {
		return nil
	}
	threshold := cfg.Governor.Claims.EscalationThreshold()
	if threshold <= 0 {
		return nil
	}
	g := &claimEscalationGate{
		ctx:       ctx,
		ledger:    ledger,
		org:       cfg.Project.Org,
		threshold: threshold,
		events: func(ctx context.Context, repo string, issue int, since time.Time) ([]github.IssueEvent, error) {
			gh := client()
			if gh == nil {
				return nil, github.ErrNoGitHubClient
			}
			return gh.IssueTimelineSince(ctx, repo, issue, since)
		},
		escalate: func(s claims.Stall) {
			go func() {
				gh := client()
				if gh == nil {
					return
				}
				cctx, cancel := context.WithTimeout(ctx, claimsGitHubTimeout)
				defer cancel()
				if err := gh.AddLabels(cctx, s.Repo, s.Issue, []string{escalation.NeedsHumanLabel}); err != nil && logger != nil {
					logger.Warn("issue-claims: escalation label failed", "issue", s.Key(), "error", err)
				}
				if err := gh.CreateIssueComment(cctx, s.Repo, s.Issue, claims.EscalationComment(s, escalation.NeedsHumanLabel)); err != nil && logger != nil {
					logger.Warn("issue-claims: escalation comment failed", "issue", s.Key(), "error", err)
				}
			}()
		},
		now:    time.Now,
		logger: logger,
	}
	return g.lookup
}

// lookup is the scheduler.InflightLookup. An issue is withheld only when a
// stalled run exists and the timeline shows nothing moved since it began; a
// timeline read that fails lets the issue through, so a GitHub outage never
// parks work on its own.
func (g *claimEscalationGate) lookup(issue github.Issue) (string, bool) {
	if issue.Number <= 0 {
		return "", false
	}
	repo := issue.Repo
	if g.org != "" && !strings.Contains(repo, "/") {
		repo = g.org + "/" + repo
	}
	stall, ok := g.ledger.Stall(repo, issue.Number, g.threshold)
	if !ok {
		return "", false
	}
	since := stall.Since()
	held := fmt.Sprintf("escalated to %s: %d claims by %s moved nothing since %s",
		escalation.NeedsHumanLabel, len(stall.Attempts), stall.Holder, since.UTC().Format(time.RFC3339))
	now := g.now()
	if v, ok := g.verdict(stall.Key(), since, now); ok {
		if v.allow {
			return "", false
		}
		return held, true
	}

	ctx, cancel := context.WithTimeout(g.ctx, claimsGitHubTimeout)
	events, err := g.events(ctx, repo, issue.Number, since)
	cancel()
	if err != nil {
		if g.logger != nil {
			g.logger.Warn("issue-claims: could not read timeline for escalation check, allowing the claim",
				"issue", stall.Key(), "holder", stall.Holder, "error", err)
		}
		g.remember(stall.Key(), progressVerdict{since: since, allow: true, until: now.Add(claimProgressRecheck)})
		return "", false
	}
	if why, moved := claimProgress(events); moved {
		g.remember(stall.Key(), progressVerdict{since: since, allow: true})
		if g.logger != nil {
			g.logger.Debug("issue-claims: re-claim allowed, issue moved", "issue", stall.Key(), "holder", stall.Holder, "progress", why)
		}
		return "", false
	}
	g.remember(stall.Key(), progressVerdict{since: since, until: now.Add(claimProgressRecheck)})
	if g.ledger.MarkEscalated(stall) {
		if g.logger != nil {
			g.logger.Info("issue-claims: escalating instead of re-claiming",
				"issue", stall.Key(), "holder", stall.Holder, "claims", len(stall.Attempts), "since", since)
		}
		g.escalate(stall)
	}
	return held, true
}

func (g *claimEscalationGate) verdict(key string, since, now time.Time) (progressVerdict, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	v, ok := g.verdicts[key]
	if !ok || !v.since.Equal(since) || (!v.until.IsZero() && !now.Before(v.until)) {
		return progressVerdict{}, false
	}
	return v, true
}

func (g *claimEscalationGate) remember(key string, v progressVerdict) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.verdicts == nil {
		g.verdicts = map[string]progressVerdict{}
	}
	g.verdicts[key] = v
}

// claimProgress reports the first timeline event that counts as the issue
// moving: a pull request referencing or linked to it, a commit referencing it,
// an assignee change, a close/reopen, or a label change. The claim machinery's
// own labels (claimed, preempted:*) and the gate's own needs-human label being
// ADDED do not count — otherwise every re-claim, and every escalation, would
// read as progress. needs-human being REMOVED does count: that is a person
// sending the issue back.
func claimProgress(events []github.IssueEvent) (string, bool) {
	for _, e := range events {
		switch e.Event {
		case "cross-referenced":
			if e.SourcePR > 0 {
				return fmt.Sprintf("pull request #%d referenced it", e.SourcePR), true
			}
		case "connected":
			return "a pull request was linked", true
		case "referenced":
			return "commit " + e.CommitID + " referenced it", true
		case "assigned", "unassigned":
			return "assignees changed", true
		case "closed", "reopened":
			return "it was " + e.Event, true
		case "labeled", "unlabeled":
			if strings.EqualFold(e.Label, claims.LabelClaimed) ||
				strings.HasPrefix(strings.ToLower(e.Label), claims.LabelPreemptedPrefix) {
				continue
			}
			if e.Event == "labeled" && strings.EqualFold(e.Label, escalation.NeedsHumanLabel) {
				continue
			}
			return fmt.Sprintf("label %q %s", e.Label, e.Event), true
		}
	}
	return "", false
}
