package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux/agent"
	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/issueclaim"
	"github.com/hivecommons/hive/pkg/scheduler"
	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/worksource"
)

// Issue claims (hivecommons/hive#8380) — composition root.
//
// The ledger itself lives in pkg/claims; the dashboard owns the relay seam
// (auto-claim on lease, exclusion in selectTask, takeover → yank). This file
// wires the two remaining producers and consumers that only cmd/hive can see:
//   - the scheduler: withhold claimed and kick-listed issues from kicks, list
//     every issue a kick hands one of the hive's own agents, and record the
//     agent claim on its first start signal (#10527); and
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

// installClaimAdmissionCheck gates every automated ledger producer on current
// labels, including renewals and forced takeovers. Resolve the client per call
// because App setup can finish after boot.
func installClaimAdmissionCheck(ctx context.Context, ledger *claims.Ledger, client func() *github.Client) {
	if ledger == nil || client == nil {
		return
	}
	ledger.SetAdmissionCheck(func(req claims.Request) (string, error) {
		ctx, cancel := context.WithTimeout(ctx, claimsGitHubTimeout)
		defer cancel()
		gh := client()
		if gh == nil {
			return "", github.ErrNoGitHubClient
		}
		return gh.IssueClaimBlockReason(ctx, req.Repo, req.Issue)
	})
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
						return gh.CreateIssueCommentBudgeted(ctx, c.Repo, c.Issue, claims.ClaimComment(c))
					})
				}
				if label {
					// The stage is a fact for the engine (ADR-0025): a live
					// claim derives to stage/020-claimed, or stage/030 when
					// it carries PR evidence. The engine re-reads the issue
					// and replaces whatever stage it carries, so a claim
					// landing seconds after the reconciler staged it 010
					// replaces that label rather than joining it (#12512).
					run("claimed stage", c, func(ctx context.Context, gh *github.Client) error {
						return gh.StageFacts().Claimed(ctx, gh.IssueStageRef(c.Repo, c.Issue), claimFactFor(c))
					})
					// The "who" modifier: generic `human` for a human principal,
					// agent name for a hive-agent claim, never a login or a
					// backend name (claims.ClaimedByPrefix).
					run("claimed-by label", c, func(ctx context.Context, gh *github.Client) error {
						_, _, err := gh.ApplyClaimedByLabel(ctx, c.Repo, c.Issue, c)
						return err
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
					run("claimed stage", now, func(ctx context.Context, gh *github.Client) error {
						return gh.StageFacts().Claimed(ctx, gh.IssueStageRef(now.Repo, now.Issue), claimFactFor(now))
					})
					run("preempted label", now, func(ctx context.Context, gh *github.Client) error {
						return gh.AddLabels(ctx, now.Repo, now.Issue, []string{claims.PreemptedLabel(previous.Holder)})
					})
					run("claimed-by label", now, func(ctx context.Context, gh *github.Client) error {
						if err := gh.RemoveClaimedByLabel(ctx, previous.Repo, previous.Issue, previous); err != nil {
							return err
						}
						_, _, err := gh.ApplyClaimedByLabel(ctx, now.Repo, now.Issue, now)
						return err
					})
				}
			}()
		},
		OnReleased: func(c claims.Claim, reason string) {
			go func() {
				if label {
					// One fresh read, then only the "who" labels still present
					// come off, journaled against that read: the plain remove
					// replayed as "already applied" the second time a claim on
					// the same issue lapsed (hivecommons/hive#12512). The
					// preempted label names the holder this claim DISPLACED,
					// not the holder being released. The claimed-by labels are
					// this claim's own (generic human label plus the retired
					// per-login spelling); another holder's survives. The
					// stage is the engine's: the release is a fact and the
					// engine re-derives the issue (010 while it is open,
					// nothing once it closed).
					remove := claims.ClaimedByLabels(c)
					if c.TakenFrom != "" {
						remove = append(remove, claims.PreemptedLabel(c.TakenFrom))
					}
					if len(remove) > 0 {
						run("remove claim labels", c, func(ctx context.Context, gh *github.Client) error {
							return gh.RemoveLabelsFresh(ctx, c.Repo, c.Issue, remove...)
						})
					}
					run("released stage", c, func(ctx context.Context, gh *github.Client) error {
						return gh.StageFacts().ClaimReleased(ctx, gh.IssueStageRef(c.Repo, c.Issue), c.Holder)
					})
				}
				// Every early release needs a marker: the 🔒 comment is
				// never edited, and issueclaim.Latest otherwise reads the
				// hold as live until until=. This includes relay disconnect,
				// revoke and lease-expiry releases: omitting them starved
				// hive agents while relays flapped (hivecommons/hive#13579).
				// TTL expiry needs no marker: until= has already lapsed.
				// Relay task completions also revoke leases, so share the
				// claim comment budget instead of posting without a cap
				// (hivecommons/hive#13766). If a marker is suppressed,
				// SetIssueClaimLedgerHive still prevents this hive from
				// believing its own stale hold.
				if comment && reason != claims.ReasonExpired {
					run("release comment", c, func(ctx context.Context, gh *github.Client) error {
						return gh.CreateIssueCommentBudgeted(ctx, c.Repo, c.Issue, claims.ReleaseComment(c, reason))
					})
				}
			}()
		},
	}
}

// claimFactFor renders a ledger claim as the fact the stage engine derives a
// stage from (hivecommons/hive#12436): a live claim is stage/020-claimed
// whatever its kind; it is stage/030-pr-in-progress only when the claim
// carries PR evidence (Claim.PRStarted: a PR request naming the issue or a
// start marker naming a branch / PR) — the label promises a PR, so a bare
// agent claim must not wear it (hivecommons/hive#13069). The ledger, not
// the label, is what claim detection reads.
func claimFactFor(c claims.Claim) stageengine.ClaimFact {
	return stageengine.ClaimFact{Holder: c.Holder, Kind: c.Kind, Until: c.ExpiresAt, PRStarted: c.PRStarted}
}

// claimsInflightLookup makes the ledger a scheduler in-flight source: an
// issue with a live claim held by anyone, or a live kick listing (#10527), is
// withheld from kicks. Agent claims and listings are included on purpose — the
// agent that holds it already has the kick, and a second agent must not be
// handed the same issue.
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
		if ok {
			return fmt.Sprintf("claimed by %s (%s) until %s", c.Holder, c.Kind, c.ExpiresAt.UTC().Format(time.RFC3339)), true
		}
		li, ok := ledger.Listed(issue.Repo, issue.Number)
		if !ok && org != "" && !strings.Contains(issue.Repo, "/") {
			li, ok = ledger.Listed(org+"/"+issue.Repo, issue.Number)
		}
		if ok {
			return fmt.Sprintf("listed in a live kick to %s until %s", li.Holder, li.ExpiresAt.UTC().Format(time.RFC3339)), true
		}
		return "", false
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

// recordAgentKickListings records a kick listing for every issue a delivered
// kick named (#10527). A listing posts nothing on GitHub and is not a claim
// attempt: a kick names up to governor.kick_limits.max_issues issues and the
// agent starts on few of them, so claiming the whole list put a 🔒 comment on
// every listed issue every agent TTL and made the escalation gate count
// issues nobody started. The listing holds the issue back from other agents
// and relay contributors while the kick is live; the agent's first start
// signal on it (recordAgentStart) records the real claim.
//
// Kick refs carry github.Issue.Repo, which is the bare project.repos entry on
// a default config; the relay keys claims on owner/repo, so the repo is
// qualified with org here or the two sides would never see each other.
func recordAgentKickListings(dashSrv *dashboard.Server, org, agentName string, issueRefs []string, logger *slog.Logger) {
	ledger := dashSrv.IssueClaims()
	if ledger == nil || agentName == "" {
		return
	}
	for _, raw := range issueRefs {
		ref, ok := worksource.ParseKey(raw)
		if !ok || !ref.IsGitHubIssue() {
			continue
		}
		if _, listed := ledger.MarkListed(qualifyClaimRepo(org, ref.Repo), ref.Number, agentName); !listed && logger != nil {
			logger.Debug("issue-claims: kick named an issue someone else holds; not listed", "agent", agentName, "issue", raw)
		}
	}
}

// recordAgentStart is the start-signal seam (#10527): the agent's own request
// shows it STARTING the issue — a claim request, a comment with an explicit
// start statement, or a pull-request request naming it (never a report-only
// comment or a label request; see pkg/github/agent_start_hook.go). The first such signal on an issue a kick
// listed to the agent records the agent claim (comment and label included)
// that the escalation gate counts; a later one renews it. A signal on an
// issue no kick listed to the agent records nothing.
func recordAgentStart(ledger *claims.Ledger, org, agentName, repo string, issue int, signal string, logger *slog.Logger) {
	if ledger == nil || agentName == "" || issue <= 0 {
		return
	}
	res, started, err := ledger.ClaimOnStart(qualifyClaimRepo(org, repo), issue, agentName, github.StartSignalIsPREvidence(signal))
	if logger == nil || !started {
		return
	}
	if err != nil {
		logger.Warn("issue-claims: agent claim not recorded", "agent", agentName, "repo", repo, "issue", issue, "signal", signal, "error", err)
		return
	}
	switch res.Outcome {
	case claims.OutcomeRefused, claims.OutcomeHeld:
		// Refused by admission (res.Reason set: hold, needs-human, a park
		// label, a never-claim label such as `epic`, or the number is a
		// pull request) or by another holder's live claim.
		logger.Warn("issue-claims: agent start signal did not record a claim",
			"agent", agentName, "repo", repo, "issue", issue, "signal", signal, "reason", res.Reason, "holder", res.Claim.Holder, "kind", res.Claim.Kind)
	case claims.OutcomeClaimed, claims.OutcomeTakenOver:
		logger.Info("issue-claims: agent claim recorded on start signal", "agent", agentName, "issue", res.Claim.Key(), "signal", signal)
	}
}

// qualifyClaimRepo qualifies a bare repo name with org.
func qualifyClaimRepo(org, repo string) string {
	repo = strings.TrimSpace(repo)
	if org != "" && repo != "" && !strings.Contains(repo, "/") {
		return org + "/" + repo
	}
	return repo
}

// ledgerClaimLookup adapts the ledger to the GitHub client's fallback claim
// source (github.Client.SetIssueClaimLedger): the enumeration-time claim
// read and the in-progress sweep see a ledger claim whose 🔒 marker comment
// was never posted (per-issue comment budget, forge write failure) instead
// of reading "no claim" and flipping stage/020-claimed back to actionable.
// The ledger is resolved per call because it is built after the client can
// first be configured.
func ledgerClaimLookup(ledger func() *claims.Ledger) func(repo string, number int) (issueclaim.Claim, bool) {
	if ledger == nil {
		return nil
	}
	return func(repo string, number int) (issueclaim.Claim, bool) {
		l := ledger()
		if l == nil || number <= 0 {
			return issueclaim.Claim{}, false
		}
		c, ok := l.Lookup(repo, number)
		if !ok {
			return issueclaim.Claim{}, false
		}
		return issueclaim.Claim{
			Identity:  c.Holder,
			StartedAt: c.ClaimedAt,
			ExpiresAt: c.ExpiresAt,
			Source:    issueclaim.SourceLease,
			Kind:      string(c.Kind),
			Hive:      c.Hive,
			Tier:      c.Tier,
			Verified:  c.Verified,
		}, true
	}
}

// releaseClaimsOnAgentRelaunch wires the agent manager's relaunch notice to
// the ledger: a fresh CLI process means the context that claimed an issue is
// gone, so its KindAgent claims are released at once instead of parking the
// issue at stage/020-claimed until the agent TTL lapses. Live case
// (hivecommons/hive, 2026-10-20): the scanner claimed #13083 at 14:21Z, two
// image rollouts relaunched every agent by 15:35Z, and the issue sat
// claimed-by-scanner with nothing working it until the 16:21Z until=.
func releaseClaimsOnAgentRelaunch(mgr *agent.Manager, ledger func() *claims.Ledger, logger *slog.Logger) {
	if mgr == nil || ledger == nil {
		return
	}
	mgr.SetRelaunchObserver(agentRelaunchReleaser(ledger, logger))
}

// agentRelaunchReleaser is the relaunch observer body: release the agent's
// KindAgent claims (nothing else) and log which issues went back.
func agentRelaunchReleaser(ledger func() *claims.Ledger, logger *slog.Logger) func(agentName string) {
	return func(agentName string) {
		l := ledger()
		if l == nil {
			return
		}
		released := l.ReleaseAgentClaims(agentName, claims.ReasonHolderRelaunched)
		if len(released) == 0 || logger == nil {
			return
		}
		keys := make([]string, 0, len(released))
		for _, c := range released {
			keys = append(keys, c.Key())
		}
		logger.Info("issue-claims: released agent claims on relaunch", "agent", agentName, "issues", keys)
	}
}
