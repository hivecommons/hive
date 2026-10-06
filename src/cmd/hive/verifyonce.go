package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/scheduler"
)

// Verify-once gate (hivecommons/hive#10527, the #10509 pattern).
//
// An issue a merged pull request referenced without closing it is kept
// actionable with the hive/likely-done label, and the kick asks the agent to
// "verify once": close it if resolved, otherwise label it hive/verified-open.
// A human-filed bug cannot be closed by the hive before the reporter confirms,
// so the issue stayed likely-done and every later kick verified it again — #10509
// collected eight near-identical verification comments. Once an agent has
// started on the issue after the merge (its claim, recorded on its first
// start signal, is the verification), the issue is withheld from kicks until
// a person — the reporter or a maintainer — comments, labels, assigns, closes
// or reopens it. That one verification plus the reporter-confirmation request
// is the single nudge; silence follows.

// verifyOnceGate withholds a likely-done issue an agent already verified.
type verifyOnceGate struct {
	ctx    context.Context
	ledger *claims.Ledger
	org    string
	// events reads the issue timeline after since.
	events func(ctx context.Context, repo string, issue int, since time.Time) ([]github.IssueEvent, error)
	now    func() time.Time
	logger *slog.Logger

	mu       sync.Mutex
	verdicts map[string]progressVerdict
}

// verifyOnceLookup returns the gate as a scheduler in-flight lookup, or nil
// when the claims ledger is off (it is where the verification is recorded).
func verifyOnceLookup(ctx context.Context, org string, ledger *claims.Ledger, client func() *github.Client, logger *slog.Logger) scheduler.InflightLookup {
	if ledger == nil || client == nil {
		return nil
	}
	g := &verifyOnceGate{
		ctx:    ctx,
		ledger: ledger,
		org:    org,
		events: func(ctx context.Context, repo string, issue int, since time.Time) ([]github.IssueEvent, error) {
			gh := client()
			if gh == nil {
				return nil, github.ErrNoGitHubClient
			}
			return gh.IssueTimelineSince(ctx, repo, issue, since)
		},
		now:    time.Now,
		logger: logger,
	}
	return g.lookup
}

// lookup is the scheduler.InflightLookup. It only ever looks at an issue
// carrying merged-PR context that is not yet hive/verified-open, and only
// withholds it when an agent claim on it postdates the merge and no person
// has acted on the issue since. A timeline read that fails lets the issue
// through: a duplicate verification is a smaller harm than parking work on a
// GitHub outage.
func (g *verifyOnceGate) lookup(issue github.Issue) (string, bool) {
	cc := issue.ClaimContext
	if issue.Number <= 0 || cc == nil || !cc.MergedPR || cc.MergedAt.IsZero() || hasLabel(issue.Labels, github.VerifiedOpenLabel) {
		return "", false
	}
	repo := qualifyClaimRepo(g.org, issue.Repo)
	attempt, ok := g.ledger.LastAttempt(repo, issue.Number)
	if !ok || !attempt.ClaimedAt.After(cc.MergedAt) {
		return "", false
	}
	key := claims.Key(repo, issue.Number)
	since := attempt.ClaimedAt
	held := fmt.Sprintf("verified once by %s at %s after %s#%d merged; waiting for the reporter or a maintainer — do not re-verify or comment",
		attempt.Holder, since.UTC().Format(time.RFC3339), cc.PRRepo, cc.PRNumber)
	now := g.now()
	if v, ok := g.verdict(key, since, now); ok {
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
			g.logger.Warn("issue-claims: could not read timeline for verify-once check, allowing the kick",
				"issue", key, "error", err)
		}
		g.remember(key, progressVerdict{since: since, allow: true, until: now.Add(claimProgressRecheck)})
		return "", false
	}
	if why, acted := personActed(events); acted {
		g.remember(key, progressVerdict{since: since, allow: true})
		if g.logger != nil {
			g.logger.Debug("issue-claims: likely-done issue re-offered, a person acted", "issue", key, "action", why)
		}
		return "", false
	}
	g.remember(key, progressVerdict{since: since, until: now.Add(claimProgressRecheck)})
	return held, true
}

func (g *verifyOnceGate) verdict(key string, since, now time.Time) (progressVerdict, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	v, ok := g.verdicts[key]
	if !ok || !v.since.Equal(since) || (!v.until.IsZero() && !now.Before(v.until)) {
		return progressVerdict{}, false
	}
	return v, true
}

func (g *verifyOnceGate) remember(key string, v progressVerdict) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.verdicts == nil {
		g.verdicts = map[string]progressVerdict{}
	}
	g.verdicts[key] = v
}

// personActed reports the first timeline event in which a person (not a bot
// or App account, which is how the hive itself comments and labels) commented
// on, labelled, assigned, closed or reopened the issue.
func personActed(events []github.IssueEvent) (string, bool) {
	for _, e := range events {
		if e.Actor == "" || e.ActorIsBot {
			continue
		}
		switch e.Event {
		case "commented", "labeled", "unlabeled", "assigned", "unassigned", "closed", "reopened":
			return fmt.Sprintf("%s %s", e.Actor, e.Event), true
		}
	}
	return "", false
}
