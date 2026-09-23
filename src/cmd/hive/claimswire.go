package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/scheduler"
	"github.com/hivecommons/hive/pkg/worksource"
)

// Issue claims (hivecommons/hive#8380) — composition root.
//
// The ledger itself lives in pkg/claims; the dashboard owns the relay seam
// (auto-claim on lease, exclusion in selectTask, takeover → yank). This file
// wires the two remaining producers and consumers that only cmd/hive can see:
//   - the scheduler: withhold claimed issues from kicks, and record an agent
//     claim for every issue a kick hands one of the hive's own agents; and
//   - GitHub: post the claim / takeover / release comments and labels so a
//     human, a clanker polling the issue, or another hive can see the hold.

// claimsLedgerPath is the ledger's on-disk location (same /data volume as the
// PR-claim ledger and the relay leases; overridable in tests).
var claimsLedgerPath = "/data/issue-claims.json"

// claimsGitHubTimeout bounds each best-effort GitHub side effect.
const claimsGitHubTimeout = 15 * time.Second

// buildClaimsLedger constructs the ledger from config, or returns nil when
// `claims.enabled: false`. A corrupt ledger file is logged and the ledger
// starts empty rather than failing boot — losing claims is recoverable (they
// expire and re-form); a hive that will not start is not.
func buildClaimsLedger(cfg *config.Config, logger *slog.Logger) *claims.Ledger {
	if cfg == nil || !cfg.Governor.Claims.IsEnabled() {
		if logger != nil {
			logger.Info("issue-claims: disabled by config")
		}
		return nil
	}
	human, agent, contributor, max := cfg.Governor.Claims.TTLs()
	path := claimsLedgerPath
	ledger, err := claims.New(path, claims.PolicyWith(human, agent, contributor, max), claims.Hooks{})
	if err != nil && logger != nil {
		logger.Warn("issue-claims: could not load persisted ledger, starting empty", "path", path, "error", err)
	}
	if ledger != nil {
		ledger.SetHive(cfg.HiveID)
	}
	return ledger
}

// githubClaimHooks returns the hooks that mirror ledger transitions onto the
// GitHub issue. Every call is best-effort: a failed comment or label never
// affects the ledger, which is authoritative for the hub's own decisions.
//
// client is resolved per call because the GitHub client is (re)built after
// boot when App auth completes (see the ghClient swap in main); capturing the
// boot-time pointer would leave the hooks silently inert on App-auth hives.
func githubClaimHooks(ctx context.Context, cfg *config.Config, client func() *github.Client, logger *slog.Logger) claims.Hooks {
	if client == nil || cfg == nil {
		return claims.Hooks{}
	}
	comment := cfg.Governor.Claims.CommentEnabled()
	label := cfg.Governor.Claims.LabelEnabled()
	run := func(what string, c claims.Claim, fn func(ctx context.Context, gh *github.Client) error) {
		gh := client()
		if gh == nil {
			return
		}
		cctx, cancel := context.WithTimeout(ctx, claimsGitHubTimeout)
		defer cancel()
		if err := fn(cctx, gh); err != nil && logger != nil {
			logger.Warn("issue-claims: GitHub side effect failed", "what", what, "issue", c.Key(), "error", err)
		}
	}
	return claims.Hooks{
		OnClaimed: func(c claims.Claim, outcome claims.Outcome) {
			go func() {
				if comment && outcome == claims.OutcomeClaimed {
					run("claim comment", c, func(ctx context.Context, gh *github.Client) error {
						return gh.CreateIssueComment(ctx, c.Repo, c.Issue, claims.ClaimComment(c))
					})
				}
				if label {
					run("claimed label", c, func(ctx context.Context, gh *github.Client) error {
						return gh.AddLabels(ctx, c.Repo, c.Issue, []string{claims.LabelClaimed})
					})
				}
			}()
		},
		OnTakenOver: func(now, previous claims.Claim) {
			go func() {
				if comment {
					run("takeover comment", now, func(ctx context.Context, gh *github.Client) error {
						return gh.CreateIssueComment(ctx, now.Repo, now.Issue, claims.TakeoverComment(now, previous))
					})
				}
				if label {
					run("preempted label", now, func(ctx context.Context, gh *github.Client) error {
						return gh.AddLabels(ctx, now.Repo, now.Issue, []string{claims.LabelClaimed, claims.PreemptedLabel(previous.Holder)})
					})
				}
			}()
		},
		OnReleased: func(c claims.Claim, reason string) {
			go func() {
				if label {
					run("remove claimed label", c, func(ctx context.Context, gh *github.Client) error {
						return gh.RemoveLabel(ctx, c.Repo, c.Issue, claims.LabelClaimed)
					})
					// The preempted label names the holder this claim DISPLACED,
					// not the holder being released.
					if c.TakenFrom != "" {
						run("remove preempted label", c, func(ctx context.Context, gh *github.Client) error {
							return gh.RemoveLabel(ctx, c.Repo, c.Issue, claims.PreemptedLabel(c.TakenFrom))
						})
					}
				}
				// Releases are frequent (every relay task completion) and the
				// closing PR already tells the story; only narrate a release
				// that a person asked for.
				if comment && strings.HasPrefix(reason, "released by ") {
					run("release comment", c, func(ctx context.Context, gh *github.Client) error {
						return gh.CreateIssueComment(ctx, c.Repo, c.Issue, claims.ReleaseComment(c, reason))
					})
				}
			}()
		},
	}
}

// claimsInflightLookup makes the ledger a scheduler in-flight source: an
// issue with a live claim held by anyone is withheld from kicks. Agent claims
// are included on purpose — the agent that holds it already has the kick, and
// a second agent must not be handed the same issue.
func claimsInflightLookup(ledger *claims.Ledger, org string) scheduler.InflightLookup {
	if ledger == nil {
		return nil
	}
	return func(issue github.Issue) (string, bool) {
		if issue.Number <= 0 {
			return "", false
		}
		c, ok := ledger.Lookup(issue.Repo, issue.Number)
		if !ok && org != "" && !strings.Contains(issue.Repo, "/") {
			c, ok = ledger.Lookup(org+"/"+issue.Repo, issue.Number)
		}
		if !ok {
			return "", false
		}
		return fmt.Sprintf("claimed by %s (%s) until %s", c.Holder, c.Kind, c.ExpiresAt.UTC().Format(time.RFC3339)), true
	}
}

// composeInflight ORs several in-flight lookups; nil entries are skipped.
func composeInflight(lookups ...scheduler.InflightLookup) scheduler.InflightLookup {
	var live []scheduler.InflightLookup
	for _, fn := range lookups {
		if fn != nil {
			live = append(live, fn)
		}
	}
	if len(live) == 0 {
		return nil
	}
	return func(issue github.Issue) (string, bool) {
		for _, fn := range live {
			if holder, ok := fn(issue); ok {
				return holder, true
			}
		}
		return "", false
	}
}

// recordAgentKickClaims records an agent claim for every issue a delivered
// kick named. The agent's own earlier claim renews; a lower-ranked holder
// (contributor / external) is taken over and told to stop; a human's claim
// is left alone — the scheduler already withheld it, so a ref reaching here
// under a human claim is the rare race, logged and not escalated.
//
// Kick refs carry github.Issue.Repo, which is the bare project.repos entry on
// a default config; the relay keys claims on owner/repo, so the repo is
// qualified with org here or the two sides would never see each other.
func recordAgentKickClaims(dashSrv *dashboard.Server, org, agentName string, issueRefs []string, logger *slog.Logger) {
	ledger := dashSrv.IssueClaims()
	if ledger == nil || agentName == "" {
		return
	}
	for _, raw := range issueRefs {
		ref, ok := worksource.ParseKey(raw)
		if !ok || !ref.IsGitHubIssue() {
			continue
		}
		repo := ref.Repo
		if org != "" && !strings.Contains(repo, "/") {
			repo = org + "/" + repo
		}
		res, err := ledger.Claim(claims.Request{
			Repo: repo, Issue: ref.Number,
			Holder: agentName, HolderID: agentName, Kind: claims.KindAgent,
		})
		if err != nil && logger != nil {
			logger.Warn("issue-claims: agent claim not recorded", "agent", agentName, "issue", raw, "error", err)
			continue
		}
		if res.Outcome == claims.OutcomeRefused && logger != nil {
			logger.Warn("issue-claims: kick named an issue a higher-ranked holder claims",
				"agent", agentName, "issue", raw, "holder", res.Claim.Holder, "kind", res.Claim.Kind)
		}
	}
}
