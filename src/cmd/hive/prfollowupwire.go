package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/escalation"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/prfollowup"
)

// PR follow-up session resume wiring (hivecommons/hive#9583). Every piece is
// a no-op unless turn.pr_follow_up.enabled (or HIVE_PR_FOLLOWUP_RESUME) is on:
//
//   - recordPRFollowUpPointer runs from the PR-request watcher's PR-opened
//     detail hook and saves (repo, PR) -> authoring agent + its live CLI
//     session + the PR's handoff note.
//   - routePRFollowUps runs on the eval tick and feeds CI failures,
//     changes-requested reviews, new review-bot threads and human feedback on
//     those PRs back into the authoring session. Anything it cannot resume is
//     left to the existing fix-before-new path (ci-failing.json /
//     review-threads.json), which is unchanged; the scheduler adds the PR's
//     handoff note (and any human feedback) to that fresh kick.
//   - sweepPRFollowUps deletes pointers of merged or closed PRs and any
//     pointer past turn.pr_follow_up.retention.

const (
	// prFollowUpCommentsRefreshInterval is how often human PR feedback is
	// re-fetched. Between refreshes the last result is reused; the router's
	// journal dedupes, so a reused comment is never delivered twice.
	prFollowUpCommentsRefreshInterval = 5 * time.Minute
	// prFollowUpCommentsMaxPRsPerPass bounds the GraphQL calls one refresh
	// spends (one per PR with a pointer).
	prFollowUpCommentsMaxPRsPerPass = 25
	// prFollowUpSweepInterval is how often pointers are pruned.
	prFollowUpSweepInterval = 30 * time.Minute
	// prFollowUpSweepMaxLookups bounds the PR-state GETs one sweep spends on
	// pointers whose PR left this tick's open list.
	prFollowUpSweepMaxLookups = 20
)

// Eval-tick state, like reviewThreadsLastRefresh: the eval tick is the only
// caller, from one goroutine.
var (
	prFollowUpCommentsLastRefresh time.Time
	prFollowUpCommentsCache       map[string][]github.PRComment
	prFollowUpLastSweep           time.Time
)

// qualifyPRFollowUpRepo returns repo as "owner/name". Config repos and PR
// requests may name a repo bare; the pointer, the review-thread report and
// the router must all agree on one spelling.
func qualifyPRFollowUpRepo(org, repo string) string {
	repo = strings.TrimSpace(repo)
	if repo != "" && !strings.Contains(repo, "/") && org != "" {
		return org + "/" + repo
	}
	return repo
}

// prFollowUpSessions is the slice of *agent.Manager the PR-opened hook needs.
type prFollowUpSessions interface {
	SessionID(name string) (string, bool)
}

// prFollowUpResumeCapturer is the optional half: a session source that can
// also name the agent's backend-native resume handle (hivecommons/hive#9606).
// *agent.Manager implements it; a source that does not simply records a
// pointer with no handle, which is the pre-#9606 behaviour.
type prFollowUpResumeCapturer interface {
	ResumeHandle(name string) (agent.ResumeHandle, bool)
}

// capturePRFollowUpResume reads the authoring agent's backend resume handle,
// or a zero handle when the backend keeps none (or no manager is wired yet).
func capturePRFollowUpResume(sessions prFollowUpSessions, agentName string) prfollowup.ResumeHandle {
	capturer, ok := sessions.(prFollowUpResumeCapturer)
	if !ok || capturer == nil {
		return prfollowup.ResumeHandle{}
	}
	h, ok := capturer.ResumeHandle(agentName)
	if !ok {
		return prfollowup.ResumeHandle{}
	}
	return prfollowup.ResumeHandle{
		Backend:    h.Backend,
		SessionID:  h.SessionID,
		Transcript: h.Transcript,
		Command:    h.Command,
		CapturedAt: h.CapturedAt,
	}
}

// recordPRFollowUpPointer saves the pointer (and handoff note) for a PR the
// watcher just opened. Failures are logged, never fatal: without a pointer
// the PR simply keeps today's fresh-dispatch follow-up path.
func recordPRFollowUpPointer(cfg *config.Config, sessions prFollowUpSessions, d github.PROpenedDetail, now time.Time, logger *slog.Logger) {
	if !cfg.PRFollowUpResumeEnabled() {
		return
	}
	session := ""
	resume := prfollowup.ResumeHandle{}
	if sessions != nil {
		session, _ = sessions.SessionID(d.Agent)
		resume = capturePRFollowUpResume(sessions, d.Agent)
	}
	repo := qualifyPRFollowUpRepo(cfg.Project.Org, d.Repo)
	note := prfollowup.BuildHandoffNote(d.Handoff, d.Body)
	if err := prfollowup.RecordWithResume(context.Background(), prfollowup.Dir(), d.Agent, repo, d.Number, d.URL, session, note, resume, now); err != nil && logger != nil {
		logger.Warn("failed to record PR follow-up pointer", "agent", d.Agent, "repo", repo, "pr", d.Number, "error", err)
	}
}

// prFollowUpResumer adapts *agent.Manager to prfollowup.Resumer, translating
// the manager's transient refusal into prfollowup.ErrBusy.
type prFollowUpResumer struct{ mgr *agent.Manager }

func (r prFollowUpResumer) SessionID(name string) (string, bool) {
	return r.mgr.SessionID(name)
}

func (r prFollowUpResumer) SendResumeKick(name, message, sessionID string) error {
	err := r.mgr.SendResumeKick(name, message, sessionID)
	if errors.Is(err, agent.ErrResumeBusy) {
		return fmt.Errorf("%w: %v", prfollowup.ErrBusy, err)
	}
	return err
}

// routePRFollowUps is the eval-tick half. It reads what the tick already has
// (the enumerated PRs, the escalation verdicts, review-threads.json), plus
// the human feedback on PRs with a pointer (a bounded, rate-limited GraphQL
// read), then runs the pointer sweep.
func routePRFollowUps(ctx context.Context, cfg *config.Config, client *github.Client, actionable *github.ActionableResult, escalatedPRs map[string]bool, agentMgr *agent.Manager, logger *slog.Logger) []prfollowup.Outcome {
	if actionable == nil || !cfg.PRFollowUpResumeEnabled() {
		return nil
	}
	now := time.Now()
	dir := prfollowup.Dir()
	org := cfg.Project.Org
	prs := make([]github.PullRequest, len(actionable.PRs.Items))
	copy(prs, actionable.PRs.Items)
	for i := range prs {
		prs[i].Repo = qualifyPRFollowUpRepo(org, prs[i].Repo)
	}
	var resumer prfollowup.Resumer
	opts := prfollowup.Options{
		Dir:    dir,
		MaxAge: cfg.PRFollowUpMaxAge(),
		Skip: func(repo string, number int) bool {
			return escalatedPRs[escalation.Key(repo, number)] ||
				escalatedPRs[escalation.Key(strings.TrimPrefix(repo, org+"/"), number)]
		},
		Comments: collectPRFollowUpComments(ctx, client, dir, prs, now, logger),
		Logger:   logger,
	}
	if agentMgr != nil {
		resumer = prFollowUpResumer{mgr: agentMgr}
		opts.Audit = agentMgr.RecordAudit
	}
	if report, err := github.ReadReviewThreadsReport(""); err == nil {
		opts.Threads = prfollowup.ThreadsFromReport(report)
	}
	outcomes := prfollowup.Route(ctx, prs, resumer, opts, now)
	sweepPRFollowUps(ctx, cfg, client, dir, prs, actionable.Hold.Items, agentMgr, now, logger)
	return outcomes
}

// collectPRFollowUpComments fetches human feedback for the open PRs this
// hive holds a pointer for, at most once per prFollowUpCommentsRefreshInterval
// and at most prFollowUpCommentsMaxPRsPerPass PRs per refresh. The result is
// keyed by prfollowup.ThreadsKey. A PR whose fetch fails is skipped this
// pass (the safe direction: no follow-up rather than a wrong one).
func collectPRFollowUpComments(ctx context.Context, client *github.Client, dir string, prs []github.PullRequest, now time.Time, logger *slog.Logger) map[string][]github.PRComment {
	if client == nil {
		return nil
	}
	if !prFollowUpCommentsLastRefresh.IsZero() && now.Sub(prFollowUpCommentsLastRefresh) < prFollowUpCommentsRefreshInterval {
		return prFollowUpCommentsCache
	}
	prFollowUpCommentsLastRefresh = now
	out := map[string][]github.PRComment{}
	fetched := 0
	for _, pr := range prs {
		if pr.Draft || pr.FromFork || pr.Number <= 0 || pr.Repo == "" {
			continue
		}
		since, ok := prfollowup.PointerCreatedAt(ctx, dir, pr.Repo, pr.Number)
		if !ok {
			continue // not a PR this hive's agents opened: never read
		}
		if fetched >= prFollowUpCommentsMaxPRsPerPass {
			break
		}
		fetched++
		comments, err := client.FetchHumanPRComments(ctx, pr.Repo, pr.Number, since)
		if err != nil {
			if logger != nil {
				logger.Warn("PR follow-up: human comment fetch failed, PR skipped this pass", "repo", pr.Repo, "pr", pr.Number, "error", err)
			}
			continue
		}
		if len(comments) > 0 {
			out[prfollowup.ThreadsKey(pr.Repo, pr.Number)] = comments
		}
	}
	prFollowUpCommentsCache = out
	return out
}

// prLifecycleState maps a fetched PR state onto the sweep's vocabulary.
func prLifecycleState(st github.PRState) string {
	switch {
	case !st.MergedAt.IsZero():
		return prfollowup.PRStateMerged
	case strings.EqualFold(st.State, "closed"):
		return prfollowup.PRStateClosed
	}
	return prfollowup.PRStateOpen
}

// sweepPRFollowUps prunes pointers at most once per prFollowUpSweepInterval.
// PRs in this tick's open list (and hold-gated PRs, which are open too) are
// known open; any other pointer's PR is looked up, within budget.
func sweepPRFollowUps(ctx context.Context, cfg *config.Config, client *github.Client, dir string, prs []github.PullRequest, held []github.HoldItem, agentMgr *agent.Manager, now time.Time, logger *slog.Logger) *prfollowup.SweepResult {
	if !prFollowUpLastSweep.IsZero() && now.Sub(prFollowUpLastSweep) < prFollowUpSweepInterval {
		return nil
	}
	prFollowUpLastSweep = now
	open := make(map[string]bool, len(prs)+len(held))
	for _, pr := range prs {
		open[prfollowup.ThreadsKey(pr.Repo, pr.Number)] = true
	}
	for _, h := range held {
		if h.Type == "pr" {
			open[prfollowup.ThreadsKey(qualifyPRFollowUpRepo(cfg.Project.Org, h.Repo), h.Number)] = true
		}
	}
	opts := prfollowup.SweepOptions{
		Dir:        dir,
		Open:       open,
		Retention:  cfg.PRFollowUpRetention(),
		MaxLookups: prFollowUpSweepMaxLookups,
		Logger:     logger,
	}
	if client != nil {
		opts.State = func(ctx context.Context, repo string, number int) (string, error) {
			st, err := client.GetPRState(ctx, repo, number)
			if err != nil {
				return "", err
			}
			return prLifecycleState(st), nil
		}
	}
	if agentMgr != nil {
		opts.Audit = agentMgr.RecordAudit
	}
	res := prfollowup.Sweep(ctx, opts, now)
	return &res
}

// prFollowUpMetricsCounters is the dashboard's /metrics provider for the
// follow-up counters. It reads stats.json from the pointer directory on each
// scrape; a missing, never-written or unreadable counter file reports nothing.
func prFollowUpMetricsCounters() (dashboard.PRFollowUpCounters, bool) {
	s, err := prfollowup.ReadStats(prfollowup.Dir())
	if err != nil || s.UpdatedAt.IsZero() {
		return dashboard.PRFollowUpCounters{}, false
	}
	return dashboard.PRFollowUpCounters{
		Resumed:           s.Resumed,
		Fallback:          s.Fallback,
		Skipped:           s.Skipped,
		Deferred:          s.Deferred,
		HandoffsQueued:    s.HandoffsQueued,
		HandoffsDelivered: s.HandoffsDelivered,
		Pruned:            s.Pruned,
	}, true
}
