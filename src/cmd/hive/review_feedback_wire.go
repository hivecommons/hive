package main

import (
	"context"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/reviewfeedback"
)

// Review-feedback response sweep (hivecommons/hive#19027). A person's review
// on a PR the hive's own agent opened (an inline comment, a COMMENTED or
// CHANGES_REQUESTED review, a conversation comment addressed to the hive)
// kicks that agent to verify the claim and push a revision or reply with
// evidence. It does not depend on draft state, the ready-for-review hold, the
// repo lane, or a CHANGES_REQUESTED verdict, and it polls, so hives without
// webhooks see feedback too. Ownership comes from the audit trail's
// agent_pr_created entries, the same source the red-PR reaper uses, so it
// reaches remote-lane repos the governor does not enumerate.

const (
	// reviewFeedbackRefreshInterval is how often the sweep runs.
	reviewFeedbackRefreshInterval = 5 * time.Minute
	// reviewFeedbackMaxPRsPerPass bounds the PRs read per pass; the cursor
	// rotates so every PR is reached over successive passes.
	reviewFeedbackMaxPRsPerPass = 25
)

var (
	reviewFeedbackLastRun time.Time
	reviewFeedbackCursor  int
	reviewFeedbackStore   *reviewfeedback.Store
)

// reviewFeedbackSource is the GitHub surface the sweep needs.
type reviewFeedbackSource interface {
	FetchPRFeedback(ctx context.Context, repo string, number int) (github.PRFeedback, error)
	CreateIssueComment(ctx context.Context, repo string, number int, body string) error
}

// reviewFeedbackRoute says where a PR's feedback goes.
type reviewFeedbackRoute struct {
	// Agent is the agent to kick, "" when none can be.
	Agent string
	// Paused means the owner is paused and stays so: skip quietly.
	Paused bool
	// Reason is shown when Agent is empty and the owner is not paused.
	Reason string
}

type reviewFeedbackSweep struct {
	Source       reviewFeedbackSource
	Store        *reviewfeedback.Store
	Owners       map[string]string // "owner/repo#n" -> authoring agent
	Kicker       fixKicker
	Route        func(owner string) reviewFeedbackRoute
	WritingGuide string
	Limits       reviewfeedback.Limits
	MaxPRs       int
	Cursor       int
	Now          time.Time
	Logger       *slog.Logger
}

// reviewFeedbackResult counts one pass, for tests and the summary line.
type reviewFeedbackResult struct {
	Checked, Kicked, Debounced, CapReached, NeedsHuman, Errors int
	NextCursor                                                 int
}

func (s reviewFeedbackSweep) run(ctx context.Context, parse func(key string) (repo string, number int, ok bool)) reviewFeedbackResult {
	s.Limits = s.Limits.Normalized()
	keys := make([]string, 0, len(s.Owners))
	for k := range s.Owners {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var res reviewFeedbackResult
	if len(keys) == 0 {
		return res
	}
	start := s.Cursor % len(keys)
	n := s.MaxPRs
	if n <= 0 || n > len(keys) {
		n = len(keys)
	}
	res.NextCursor = (start + n) % len(keys)
	for i := 0; i < n; i++ {
		key := keys[(start+i)%len(keys)]
		repo, number, ok := parse(key)
		if !ok {
			continue
		}
		route := s.Route(redPRFixOwner(s.Owners[key]))
		if route.Paused {
			continue // paused agents stay paused
		}
		fb, err := s.Source.FetchPRFeedback(ctx, repo, number)
		res.Checked++
		if err != nil {
			res.Errors++
			s.Logger.Warn("review feedback: fetch failed, PR skipped this pass", "repo", repo, "pr", number, "error", err)
			continue
		}
		if !fb.Open() || len(fb.Items) == 0 {
			continue
		}
		d := s.Store.Plan(key, fb.Items, s.Now, s.Limits)
		switch d.Verdict {
		case reviewfeedback.VerdictDebounce:
			res.Debounced++
			s.Logger.Debug("review feedback: burst still arriving, waiting to coalesce", "repo", repo, "pr", number, "comments", len(d.Items))
		case reviewfeedback.VerdictCapReached:
			res.CapReached++
			s.needsHuman(ctx, key, repo, number, "it has been routed to its agent "+strconv.Itoa(s.Limits.WindowCap)+" times in "+s.Limits.Window.String()+"; further feedback is waiting, and a new comment from a person resets this", d.Items, &res)
		case reviewfeedback.VerdictKick:
			if route.Agent == "" {
				s.needsHuman(ctx, key, repo, number, route.Reason, d.Items, &res)
				continue
			}
			brief := reviewfeedback.BuildBrief(repo, number, fb.HeadRef, s.WritingGuide, d.Items)
			if err := s.Kicker.Kick(route.Agent, brief); err != nil {
				level := slog.LevelWarn
				if isSharedPoolBusyErr(err) {
					level = slog.LevelInfo
				}
				s.Logger.Log(ctx, level, "review feedback: kick undeliverable, retrying next pass", "repo", repo, "pr", number, "agent", route.Agent, "error", err)
				continue
			}
			if err := s.Store.RecordKick(key, d.Items, s.Now); err != nil {
				s.Logger.Warn("review feedback: could not persist handled ids; a later pass may re-kick", "repo", repo, "pr", number, "error", err)
			}
			res.Kicked++
			for _, it := range d.Items {
				s.Logger.Info("review feedback: agent kicked to respond", "repo", repo, "pr", number, "agent", route.Agent,
					"comment", it.ID, "thread", it.ThreadID, "author", it.Author, "draft", fb.Draft, "source", fb.Source)
			}
		}
	}
	return res
}

func (s reviewFeedbackSweep) needsHuman(ctx context.Context, key, repo string, number int, reason string, items []github.FeedbackItem, res *reviewFeedbackResult) {
	if s.Store.NeedsHumanNoted(key) {
		return
	}
	s.Logger.Warn("review feedback: cannot route to an agent, asking a person", "repo", repo, "pr", number, "reason", reason, "comments", len(items))
	if err := s.Source.CreateIssueComment(ctx, repo, number, reviewfeedback.NeedsHumanNote(reason, items)); err != nil { // writeback:solicited a person's review feedback on this hive-authored PR
		s.Logger.Warn("review feedback: needs-human note not posted", "repo", repo, "pr", number, "error", err)
		return
	}
	if _, err := s.Store.MarkNeedsHuman(key, s.Now); err != nil {
		s.Logger.Warn("review feedback: could not persist needs-human flag", "repo", repo, "pr", number, "error", err)
	}
	res.NeedsHuman++
}

// parseReviewFeedbackKey splits "owner/repo#n".
func parseReviewFeedbackKey(key string) (string, int, bool) {
	i := strings.LastIndex(key, "#")
	if i <= 0 {
		return "", 0, false
	}
	n := 0
	for _, r := range key[i+1:] {
		if r < '0' || r > '9' {
			return "", 0, false
		}
		n = n*10 + int(r-'0')
	}
	if n <= 0 || !strings.Contains(key[:i], "/") {
		return "", 0, false
	}
	return key[:i], n, true
}

// sweepReviewFeedback is the eval-tick entry point.
func sweepReviewFeedback(ctx context.Context, cfg *config.Config, client *github.Client, agentMgr crux.Runtime, logger *slog.Logger) {
	if cfg == nil || client == nil || !reviewfeedback.Enabled() {
		return
	}
	now := time.Now()
	if !reviewFeedbackLastRun.IsZero() && now.Sub(reviewFeedbackLastRun) < reviewFeedbackRefreshInterval {
		return
	}
	reviewFeedbackLastRun = now
	if reviewFeedbackStore == nil {
		st, err := reviewfeedback.Open(reviewfeedback.Dir())
		if err != nil {
			logger.Warn("review feedback: state unreadable or not writable; handled ids kept in memory only", "dir", reviewfeedback.Dir(), "error", err)
		}
		reviewFeedbackStore = st
	}
	available := agentAvailability(cfg, agentMgr)
	sweep := reviewFeedbackSweep{
		Source:       client,
		Store:        reviewFeedbackStore,
		Owners:       auditPRAgents(cfg.Project.Org, now.Add(-auditPRAttributionWindow), ""),
		Kicker:       agentKicker{mgr: agentMgr},
		WritingGuide: cfg.Project.WritingGuideSection(),
		Limits:       reviewfeedback.LimitsFromEnv(),
		MaxPRs:       reviewFeedbackMaxPRsPerPass,
		Cursor:       reviewFeedbackCursor,
		Now:          now,
		Logger:       logger,
		Route: func(owner string) reviewFeedbackRoute {
			if ac, ok := cfg.Agents[owner]; (agentMgr != nil && agentMgr.IsPaused(owner)) || (ok && ac.Paused) {
				return reviewFeedbackRoute{Paused: true}
			}
			if agentMgr == nil {
				return reviewFeedbackRoute{Reason: "no agent manager is running"}
			}
			if ac, ok := cfg.Agents[owner]; ok && ac.Enabled && available(owner) {
				return reviewFeedbackRoute{Agent: owner}
			}
			if fixer := resolveRedPRFixer(cfg, owner, available); fixer != "" {
				return reviewFeedbackRoute{Agent: fixer}
			}
			return reviewFeedbackRoute{Reason: "the agent that opened it is not available and no fallback agent is configured"}
		},
	}
	res := sweep.run(ctx, parseReviewFeedbackKey)
	reviewFeedbackCursor = res.NextCursor
	if res.Kicked+res.NeedsHuman+res.Debounced+res.CapReached+res.Errors > 0 {
		logger.Info("review feedback pass", "checked", res.Checked, "kicked", res.Kicked, "debounced", res.Debounced,
			"cap_reached", res.CapReached, "needs_human", res.NeedsHuman, "errors", res.Errors)
	}
}
