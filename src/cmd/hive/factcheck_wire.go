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
	"github.com/hivecommons/hive/pkg/factcheck"
	"github.com/hivecommons/hive/pkg/github"
)

// Analysis fact-check sweep (hivecommons/hive#19074). For the PRs the
// configured agents opened as analyses or reports, an independent reviewer
// agent re-verifies the work, the author revises what it got wrong, and the
// loop stops on a passing verdict, the round limit, or a person reviewing.
//
// It is a separate path from the review-feedback sweep (#19027). That sweep
// ignores bot comments so two agents can never answer each other forever; this
// one reacts only to the hive's own marked fact-check reviews, bounds the
// rounds in persisted state, and kicks the author itself, so neither sweep
// needs the other's files and the loop-safety argument stays local.
//
// Owners come from the audit trail's agent_pr_created entries, so it reaches
// repos the hive does not manage. A PR a person opened is not enumerated.

const (
	// factCheckRefreshInterval is how often the sweep runs.
	factCheckRefreshInterval = 5 * time.Minute
	// factCheckMaxPRsPerPass bounds the PRs read per pass; the cursor rotates
	// so every PR is reached over successive passes.
	factCheckMaxPRsPerPass = 25
	// factCheckReviewerRole is the role name of the default reviewer lane.
	factCheckReviewerRole = "reviewer"
)

var (
	factCheckLastRun time.Time
	factCheckCursor  int
	factCheckStore   *factcheck.Store
)

// factCheckSource is the GitHub surface the sweep needs.
type factCheckSource interface {
	FetchPRFactCheck(ctx context.Context, repo string, number int) (github.FactCheckPR, error)
	MarkPRReadyForReview(ctx context.Context, nodeID string) error
	CreateIssueComment(ctx context.Context, repo string, number int, body string) error
}

// factCheckReviewers is what the sweep knows about the reviewer lane right now.
type factCheckReviewers struct {
	// Configured is how many reviewer agents are named at all.
	Configured int
	// Available are the ones a kick would reach now, in preference order.
	Available []factcheck.Agent
}

type factCheckSweep struct {
	Source             factCheckSource
	Store              *factcheck.Store
	Policy             factcheck.Policy
	Owners             map[string]string // "owner/repo#n" -> authoring agent
	Kicker             fixKicker
	Author             func(owner string) factcheck.Agent
	AuthorPaused       func(owner string) bool
	AuthorRoute        func(owner string) (agent, reason string)
	Reviewers          func() factCheckReviewers
	AuthoritativeFiles []string
	Rubric             string
	WritingGuide       string
	MaxPRs             int
	Cursor             int
	Now                time.Time
	Logger             *slog.Logger
}

// factCheckResult counts one pass, for tests and the summary line.
type factCheckResult struct {
	Checked, Reviews, Revisions, Readied, NeedsHuman, Errors int
	NextCursor                                               int
}

// factCheckMarkers returns the fact-check markers on the hive's own reviews.
// A marker on anyone else's comment is ignored: it asserts that the pass held,
// and a person or another bot can type one.
func factCheckMarkers(pr github.FactCheckPR) []factcheck.Marker {
	var out []factcheck.Marker
	for _, r := range pr.Reviews {
		if !r.Hive {
			continue
		}
		if m, ok := factcheck.ParseMarker(r.Body); ok {
			out = append(out, m)
		}
	}
	return out
}

// factCheckNeedsHumanPosted reports whether the hive already left the hand-off
// note, as a comment or inside a review.
func factCheckNeedsHumanPosted(pr github.FactCheckPR) bool {
	for _, c := range pr.Comments {
		if c.Hive && factcheck.HasNeedsHumanMarker(c.Body) {
			return true
		}
	}
	for _, r := range pr.Reviews {
		if r.Hive && factcheck.HasNeedsHumanMarker(r.Body) {
			return true
		}
	}
	return false
}

// factCheckParticipants lists who reviewed, for factcheck.HumanTookOver.
func factCheckParticipants(pr github.FactCheckPR) []factcheck.Participant {
	out := make([]factcheck.Participant, 0, len(pr.Reviews))
	for _, r := range pr.Reviews {
		out = append(out, factcheck.Participant{Login: r.Author, Human: r.Human, Substantive: true})
	}
	return out
}

// latestFactCheckReview is the hive's marked review with the highest round that
// names the PR's current head.
func latestFactCheckReview(pr github.FactCheckPR) (github.FactCheckReview, factcheck.Marker, bool) {
	var bestR github.FactCheckReview
	var bestM factcheck.Marker
	found := false
	for _, r := range pr.Reviews {
		if !r.Hive {
			continue
		}
		m, ok := factcheck.ParseMarker(r.Body)
		if !ok || !factcheck.SameHead(m.Head, pr.HeadSHA) {
			continue
		}
		if !found || m.Round > bestM.Round {
			bestR, bestM, found = r, m, true
		}
	}
	return bestR, bestM, found
}

func factCheckFindings(r github.FactCheckReview) []factcheck.Finding {
	out := make([]factcheck.Finding, 0, len(r.Inline))
	for _, in := range r.Inline {
		out = append(out, factcheck.Finding{Path: in.Path, Line: in.Line, Body: in.Body, URL: in.URL})
	}
	return out
}

func (s factCheckSweep) agentQualifies(owner string) bool {
	ok, _ := factcheck.Qualifies(s.Policy, factcheck.PR{Open: true}, owner)
	return ok
}

func (s factCheckSweep) run(ctx context.Context, parse func(key string) (repo string, number int, ok bool)) factCheckResult {
	keys := make([]string, 0, len(s.Owners))
	for k := range s.Owners {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var res factCheckResult
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
		owner := s.Owners[key]
		// With no labels configured the authoring agent alone decides, so a
		// PR by another agent costs no read.
		if len(s.Policy.Labels) == 0 && !s.agentQualifies(owner) {
			continue
		}
		if s.AuthorPaused != nil && s.AuthorPaused(owner) {
			continue // paused agents stay paused
		}
		pr, err := s.Source.FetchPRFactCheck(ctx, repo, number)
		res.Checked++
		if err != nil {
			res.Errors++
			s.Logger.Warn("analysis fact-check: fetch failed, PR skipped this pass", "repo", repo, "pr", number, "error", err)
			continue
		}
		view := factcheck.PR{Repo: repo, Number: number, Open: pr.Open(), Draft: pr.Draft, HeadSHA: pr.HeadSHA, Labels: pr.Labels}
		if ok, _ := factcheck.Qualifies(s.Policy, view, owner); !ok {
			continue
		}
		d := factcheck.Decide(factcheck.Input{
			Policy:           s.Policy,
			PR:               view,
			Markers:          factCheckMarkers(pr),
			NeedsHumanPosted: factCheckNeedsHumanPosted(pr),
			HumanTookOver:    factcheck.HumanTookOver(factCheckParticipants(pr)),
			State:            s.Store.Get(key),
			Now:              s.Now,
		})
		switch d.Action {
		case factcheck.ActionReview:
			s.review(ctx, key, repo, number, owner, pr, d, &res)
		case factcheck.ActionRevise:
			s.revise(key, repo, number, owner, pr, &res)
		case factcheck.ActionMarkReady:
			s.markReady(ctx, key, repo, number, pr, d, &res)
		case factcheck.ActionNeedsHuman:
			s.needsHuman(ctx, key, repo, number, d.Reason, d.Round, &res)
		}
	}
	return res
}

func (s factCheckSweep) review(ctx context.Context, key, repo string, number int, owner string, pr github.FactCheckPR, d factcheck.Decision, res *factCheckResult) {
	lane := s.Reviewers()
	if lane.Configured == 0 {
		s.needsHuman(ctx, key, repo, number, "no reviewer agent is configured for the fact-check pass", d.Round-1, res)
		return
	}
	if len(lane.Available) == 0 {
		s.Logger.Info("analysis fact-check: no reviewer agent is reachable, retrying next pass", "repo", repo, "pr", number)
		return
	}
	reviewer, why, ok := factcheck.PickReviewer(s.Policy, s.Author(owner), lane.Available)
	if !ok {
		s.needsHuman(ctx, key, repo, number, "no independent reviewer is available ("+why+")", d.Round-1, res)
		return
	}
	brief := factcheck.BuildReviewBrief(factcheck.ReviewBrief{
		Repo: repo, Number: number, HeadSHA: pr.HeadSHA, HeadRef: pr.HeadRef,
		Round: d.Round, MaxRounds: s.Policy.MaxRounds, AuthorAgent: owner,
		ReviewerModel: s.Policy.ReviewerModel, AuthoritativeFiles: s.AuthoritativeFiles,
		Rubric: s.Rubric, WritingGuide: s.WritingGuide,
	})
	if err := s.Kicker.Kick(reviewer.Name, brief); err != nil {
		level := slog.LevelWarn
		if isSharedPoolBusyErr(err) {
			level = slog.LevelInfo
		}
		s.Logger.Log(ctx, level, "analysis fact-check: review kick undeliverable, retrying next pass", "repo", repo, "pr", number, "agent", reviewer.Name, "error", err)
		return
	}
	if err := s.Store.Update(key, s.Now, func(st *factcheck.State) {
		if d.Round > st.ReviewKicks {
			st.ReviewKicks = d.Round
		}
		st.LastReviewKick, st.LastReviewHead = s.Now, pr.HeadSHA
	}); err != nil {
		s.Logger.Warn("analysis fact-check: could not persist the round; the marked reviews on the PR still bound it", "repo", repo, "pr", number, "error", err)
	}
	res.Reviews++
	s.Logger.Info("analysis fact-check: reviewer kicked", "repo", repo, "pr", number, "agent", reviewer.Name,
		"author", owner, "round", d.Round, "max_rounds", s.Policy.MaxRounds, "head", pr.HeadSHA, "draft", pr.Draft)
}

func (s factCheckSweep) revise(key, repo string, number int, owner string, pr github.FactCheckPR, res *factCheckResult) {
	rev, marker, ok := latestFactCheckReview(pr)
	if !ok {
		return // the marked review vanished between reads; the next pass decides again
	}
	agent, reason := s.AuthorRoute(owner)
	if agent == "" {
		s.Logger.Warn("analysis fact-check: no agent can take the findings, the revision wait will hand the PR to a person",
			"repo", repo, "pr", number, "reason", reason)
		if err := s.Store.Update(key, s.Now, func(st *factcheck.State) {
			st.RevisionHead, st.RevisionAt = pr.HeadSHA, s.Now
		}); err != nil {
			s.Logger.Warn("analysis fact-check: could not persist the revision wait", "repo", repo, "pr", number, "error", err)
		}
		return
	}
	brief := factcheck.BuildRevisionBrief(factcheck.RevisionBrief{
		Repo: repo, Number: number, HeadRef: pr.HeadRef, Round: marker.Round, MaxRounds: s.Policy.MaxRounds,
		ReviewURL: rev.URL, Summary: rev.Body, Findings: factCheckFindings(rev), WritingGuide: s.WritingGuide,
	})
	if err := s.Kicker.Kick(agent, brief); err != nil {
		s.Logger.Warn("analysis fact-check: revision kick undeliverable, retrying next pass", "repo", repo, "pr", number, "agent", agent, "error", err)
		return
	}
	if err := s.Store.Update(key, s.Now, func(st *factcheck.State) {
		st.RevisionHead, st.RevisionAt = pr.HeadSHA, s.Now
	}); err != nil {
		s.Logger.Warn("analysis fact-check: could not persist the revision kick; a later pass may re-kick", "repo", repo, "pr", number, "error", err)
	}
	res.Revisions++
	s.Logger.Info("analysis fact-check: author kicked to revise", "repo", repo, "pr", number, "agent", agent,
		"round", marker.Round, "findings", len(rev.Inline), "head", pr.HeadSHA)
}

func (s factCheckSweep) markReady(ctx context.Context, key, repo string, number int, pr github.FactCheckPR, d factcheck.Decision, res *factCheckResult) {
	if err := s.Source.MarkPRReadyForReview(ctx, pr.NodeID); err != nil { // writeback:solicited ready_policy: hive is an explicit operator opt-in
		s.Logger.Warn("analysis fact-check: could not mark the PR ready, retrying next pass", "repo", repo, "pr", number, "error", err)
		return
	}
	if err := s.Store.Update(key, s.Now, func(st *factcheck.State) { st.ReadyMarked = true }); err != nil {
		s.Logger.Warn("analysis fact-check: could not persist the ready mark", "repo", repo, "pr", number, "error", err)
	}
	res.Readied++
	s.Logger.Info("analysis fact-check: passed, PR marked ready for review", "repo", repo, "pr", number, "round", d.Round, "head", pr.HeadSHA)
}

func (s factCheckSweep) needsHuman(ctx context.Context, key, repo string, number int, reason string, round int, res *factCheckResult) {
	if s.Store.Get(key).NeedsHumanNoted {
		return
	}
	s.Logger.Warn("analysis fact-check: stopping, asking a person", "repo", repo, "pr", number, "reason", reason)
	if err := s.Source.CreateIssueComment(ctx, repo, number, factcheck.NeedsHumanNote(reason, round, s.Policy.MaxRounds)); err != nil { // writeback:solicited the opted-in fact-check pass hands its own PR to a person
		s.Logger.Warn("analysis fact-check: hand-off note not posted", "repo", repo, "pr", number, "error", err)
		return
	}
	if err := s.Store.Update(key, s.Now, func(st *factcheck.State) { st.NeedsHumanNoted = true }); err != nil {
		s.Logger.Warn("analysis fact-check: could not persist the hand-off flag", "repo", repo, "pr", number, "error", err)
	}
	res.NeedsHuman++
}

// parseFactCheckKey splits "owner/repo#n".
func parseFactCheckKey(key string) (string, int, bool) {
	i := strings.LastIndex(key, "#")
	if i <= 0 || !strings.Contains(key[:i], "/") {
		return "", 0, false
	}
	n, err := strconv.Atoi(key[i+1:])
	if err != nil || n <= 0 {
		return "", 0, false
	}
	return key[:i], n, true
}

func trimFactCheckNames(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// factCheckReviewerNames resolves who may run the pass: the block's own list,
// else review.reviewer_agents, else the enabled agents with the reviewer role.
func factCheckReviewerNames(cfg *config.Config) []string {
	if n := trimFactCheckNames(cfg.Review.AnalysisFactCheck.ReviewerAgents); len(n) > 0 {
		return n
	}
	if n := trimFactCheckNames(cfg.Review.ReviewerAgents); len(n) > 0 {
		return n
	}
	var names []string
	for name, ac := range cfg.Agents {
		if ac.Enabled && ac.ReplicaOf == "" && strings.EqualFold(strings.TrimSpace(ac.Role), factCheckReviewerRole) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func factCheckAgent(cfg *config.Config, name string) factcheck.Agent {
	ac, ok := cfg.Agents[name]
	if !ok {
		ac, ok = cfg.Agents[cfg.BaseAgentName(name)]
	}
	if !ok {
		return factcheck.Agent{Name: name}
	}
	return factcheck.Agent{Name: name, Backend: ac.Backend, Model: ac.Model}
}

// factCheckPolicy resolves the config block into the pure policy.
func factCheckPolicy(fc config.AnalysisFactCheckConfig) factcheck.Policy {
	agents, labels := fc.Qualifiers()
	return factcheck.Policy{
		AuthorAgents:  agents,
		Labels:        labels,
		Independence:  factcheck.Independence(fc.EffectiveIndependence()),
		ReviewerModel: strings.TrimSpace(fc.ReviewerModel),
		MaxRounds:     fc.EffectiveMaxRounds(),
		ReadyPolicy:   factcheck.ReadyPolicy(fc.EffectiveReadyPolicy()),
		RevisionWait:  fc.EffectiveRevisionWait(),
	}
}

// sweepAnalysisFactCheck is the eval-tick entry point.
func sweepAnalysisFactCheck(ctx context.Context, cfg *config.Config, client *github.Client, agentMgr crux.Runtime, logger *slog.Logger) {
	if cfg == nil || client == nil || !cfg.Review.AnalysisFactCheck.Enabled {
		return
	}
	now := time.Now()
	if !factCheckLastRun.IsZero() && now.Sub(factCheckLastRun) < factCheckRefreshInterval {
		return
	}
	factCheckLastRun = now
	if factCheckStore == nil {
		st, err := factcheck.Open(factcheck.Dir())
		if err != nil {
			logger.Warn("analysis fact-check: state unreadable or not writable; rounds kept in memory only", "dir", factcheck.Dir(), "error", err)
		}
		factCheckStore = st
	}
	fc := cfg.Review.AnalysisFactCheck
	available := agentAvailability(cfg, agentMgr)
	sweep := factCheckSweep{
		Source:             client,
		Store:              factCheckStore,
		Policy:             factCheckPolicy(fc),
		Owners:             auditPRAgents(cfg.Project.Org, now.Add(-auditPRAttributionWindow), ""),
		Kicker:             agentKicker{mgr: agentMgr},
		Author:             func(owner string) factcheck.Agent { return factCheckAgent(cfg, owner) },
		AuthoritativeFiles: fc.EffectiveAuthoritativeFiles(),
		Rubric:             fc.Rubric,
		WritingGuide:       cfg.Project.WritingGuideSection(),
		MaxPRs:             factCheckMaxPRsPerPass,
		Cursor:             factCheckCursor,
		Now:                now,
		Logger:             logger,
		AuthorPaused: func(owner string) bool {
			ac, ok := cfg.Agents[owner]
			return (agentMgr != nil && agentMgr.IsPaused(owner)) || (ok && ac.Paused)
		},
		AuthorRoute: func(owner string) (string, string) {
			if agentMgr == nil {
				return "", "no agent manager is running"
			}
			if ac, ok := cfg.Agents[owner]; ok && ac.Enabled && available(owner) {
				return owner, ""
			}
			if fixer := resolveRedPRFixer(cfg, owner, available); fixer != "" {
				return fixer, ""
			}
			return "", "the agent that opened it is not available and no fallback agent is configured"
		},
		Reviewers: func() factCheckReviewers {
			names := factCheckReviewerNames(cfg)
			lane := factCheckReviewers{Configured: len(names)}
			for _, name := range names {
				if ac, ok := cfg.Agents[name]; ok && ac.Enabled && available(name) {
					lane.Available = append(lane.Available, factCheckAgent(cfg, name))
				}
			}
			return lane
		},
	}
	res := sweep.run(ctx, parseFactCheckKey)
	factCheckCursor = res.NextCursor
	if res.Reviews+res.Revisions+res.Readied+res.NeedsHuman+res.Errors > 0 {
		logger.Info("analysis fact-check pass", "checked", res.Checked, "reviews", res.Reviews, "revisions", res.Revisions,
			"readied", res.Readied, "needs_human", res.NeedsHuman, "errors", res.Errors)
	}
}
