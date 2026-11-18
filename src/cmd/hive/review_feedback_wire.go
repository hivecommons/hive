package main

import (
	"context"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/proxy"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/reviewfeedback"
	"github.com/hivecommons/hive/pkg/reviewrules"
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
	// reviewFeedbackStore is shared by the sweep and the proxy's refused-reply
	// hook (#19096), which run on different goroutines; open it through
	// openReviewFeedbackStore.
	reviewFeedbackStore   *reviewfeedback.Store
	reviewRuleStore       *reviewrules.Store
	reviewFeedbackStoreMu sync.Mutex
)

// openReviewFeedbackStore opens the sweep's state once and returns it.
func openReviewFeedbackStore(logger *slog.Logger) *reviewfeedback.Store {
	reviewFeedbackStoreMu.Lock()
	defer reviewFeedbackStoreMu.Unlock()
	if reviewFeedbackStore == nil {
		st, err := reviewfeedback.Open(reviewfeedback.Dir())
		if err != nil {
			logger.Warn("review feedback: state unreadable or not writable; handled ids kept in memory only", "dir", reviewfeedback.Dir(), "error", err)
		}
		reviewFeedbackStore = st
	}
	return reviewFeedbackStore
}

// reviewThreadOwnersTTL bounds how long the audit-trail PR ownership map the
// proxy's review-thread gate reads is reused before it is read again.
const reviewThreadOwnersTTL = time.Minute

// reviewThreadOwners caches auditPRAgents for the proxy gate, which is asked
// per request: reading the audit log on every reply would be wasteful, and a
// PR opened a minute ago is not yet being reviewed.
type reviewThreadOwners struct {
	mu     sync.Mutex
	at     time.Time
	owners map[string]string
	org    func() string
	load   func(org string, since time.Time) map[string]string
	now    func() time.Time
}

// owner returns the agent that opened repo#number, from the same audit-trail
// source the sweep uses (agent_pr_created within auditPRAttributionWindow).
func (o *reviewThreadOwners) owner(repo string, number int) (string, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	now := o.now()
	if o.owners == nil || now.Sub(o.at) >= reviewThreadOwnersTTL {
		o.owners = o.load(o.org(), now.Add(-auditPRAttributionWindow))
		o.at = now
	}
	key := reviewfeedback.Key(repo, number)
	if agent, ok := o.owners[key]; ok && agent != "" {
		return agent, true
	}
	for k, agent := range o.owners {
		if agent != "" && strings.EqualFold(k, key) {
			return agent, true
		}
	}
	return "", false
}

// newReviewThreadGuard builds the proxy's review-thread gate seams (#19096):
// PR ownership from the audit trail, the hive's App bot login, and the hook
// that keeps a refused reply for the sweep's needs-human note.
func newReviewThreadGuard(org func() string, botLogin func() string, logger *slog.Logger) proxy.ReviewThreadGuard {
	owners := &reviewThreadOwners{
		org: org,
		load: func(org string, since time.Time) map[string]string {
			return auditPRAgents(org, since, "")
		},
		now: time.Now,
	}
	return proxy.ReviewThreadGuard{
		OwnPR: owners.owner,
		HiveLogins: func() []string {
			if l := strings.TrimSpace(botLogin()); l != "" {
				return []string{l}
			}
			return nil
		},
		OnRefused: func(r proxy.ReviewWriteRefusal) {
			key := reviewfeedback.Key(r.Repo, r.Number)
			err := openReviewFeedbackStore(logger).RecordBlockedReply(key, reviewfeedback.ReplyDraft{InReplyTo: r.InReplyTo, Body: r.Body, Agent: r.Agent, At: r.At})
			if err != nil {
				logger.Warn("review feedback: refused reply not kept for the needs-human note", "repo", r.Repo, "pr", r.Number, "agent", r.Agent, "error", err)
				return
			}
			logger.Info("review feedback: reply refused by the proxy, kept for a person", "repo", r.Repo, "pr", r.Number, "agent", r.Agent, "in_reply_to", r.InReplyTo)
		},
	}
}

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
	// Rules captures reviewer-stated rules into the hive's knowledge
	// (hivecommons/hive#19046). Nil turns capture off; the sweep is
	// otherwise unchanged.
	Rules *reviewrules.Store
	// Lane maps the authoring agent to the base lane its rules apply to.
	Lane func(owner string) string
	// Managed says whether the hive manages a repo (owner/name).
	Managed func(repo string) bool
}

// reviewFeedbackResult counts one pass, for tests and the summary line.
type reviewFeedbackResult struct {
	Checked, Kicked, Debounced, CapReached, NeedsHuman, Errors int
	RulesCaptured, RulesQueued, RulesMerged                    int
	ReplyMissingRules                                          int
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
		if !fb.Open() {
			continue
		}
		if s.Rules != nil {
			s.applyRuleReplies(repo, number, fb.HiveReplies)
			s.postRuleNotices(ctx, repo, number)
			s.checkRuleReply(ctx, key, repo, number, fb.AgentReplies, &res)
			s.captureRules(ctx, repo, number, redPRFixOwner(s.Owners[key]), fb.RuleCandidates, &res)
		}
		// Replies the agent drafted but the proxy refused (#19096): hand them
		// to a person instead of letting them vanish.
		if drafts := s.Store.BlockedDraftsDue(key, s.Now, s.Limits.Window); len(drafts) > 0 {
			s.needsHuman(ctx, key, repo, number, "the hive's proxy refused the reply, so it was not posted", nil, drafts, &res)
		}
		if len(fb.Items) == 0 {
			continue
		}
		d := s.Store.Plan(key, fb.Items, s.Now, s.Limits)
		switch d.Verdict {
		case reviewfeedback.VerdictDebounce:
			res.Debounced++
			s.Logger.Debug("review feedback: burst still arriving, waiting to coalesce", "repo", repo, "pr", number, "comments", len(d.Items))
		case reviewfeedback.VerdictCapReached:
			res.CapReached++
			s.needsHuman(ctx, key, repo, number, "it has been routed to its agent "+strconv.Itoa(s.Limits.WindowCap)+" times in "+s.Limits.Window.String()+"; further feedback is waiting, and a new comment from a person resets this", d.Items, nil, &res)
		case reviewfeedback.VerdictKick:
			recorded := s.recordedRules(repo, number, d.Items)
			if route.Agent == "" {
				s.needsHuman(ctx, key, repo, number, route.Reason, d.Items, nil, &res)
				continue
			}
			brief := reviewfeedback.BuildBriefWithRules(repo, number, fb.HeadRef, s.WritingGuide, d.Items, recorded)
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
			if len(recorded) > 0 {
				ids := make([]string, 0, len(recorded))
				for _, r := range recorded {
					ids = append(ids, r.ID)
				}
				if err := s.Store.RecordPendingReply(key, ids, s.Now); err != nil {
					s.Logger.Warn("review feedback: could not persist the pending reply check", "repo", repo, "pr", number, "error", err)
				}
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

// needsHuman posts the needs-human note: once per PR when feedback cannot be
// routed, or with drafts set, the replies the agent drafted but could not
// post (#19096), at most once per Limits.Window (BlockedDraftsDue).
func (s reviewFeedbackSweep) needsHuman(ctx context.Context, key, repo string, number int, reason string, items []github.FeedbackItem, drafts []reviewfeedback.ReplyDraft, res *reviewFeedbackResult) {
	if len(drafts) == 0 && s.Store.NeedsHumanNoted(key) {
		return
	}
	s.Logger.Warn("review feedback: cannot route to an agent, asking a person", "repo", repo, "pr", number, "reason", reason, "comments", len(items), "drafted_replies", len(drafts))
	if err := s.Source.CreateIssueComment(ctx, repo, number, reviewfeedback.NeedsHumanNote(reason, items, drafts)); err != nil { // writeback:solicited a person's review feedback on this hive-authored PR
		s.Logger.Warn("review feedback: needs-human note not posted", "repo", repo, "pr", number, "error", err)
		return
	}
	if len(drafts) > 0 {
		if err := s.Store.MarkBlockedNoted(key, drafts, s.Now); err != nil {
			s.Logger.Warn("review feedback: could not persist drafted-replies hand-off", "repo", repo, "pr", number, "error", err)
		}
	} else if _, err := s.Store.MarkNeedsHuman(key, s.Now); err != nil {
		s.Logger.Warn("review feedback: could not persist needs-human flag", "repo", repo, "pr", number, "error", err)
	}
	res.NeedsHuman++
}

// captureRules is the deterministic half of rule capture (#19046): every human
// comment on the PR that states a rule is written to the hive's knowledge store
// whether or not the agent later records it, idempotently per comment. It reads
// its own candidate set (#19136), not the routed burst, so a comment that was
// already handled, sits in a resolved thread, or does not address the hive is
// still classified. Each comment is classified once: the rule index keeps the
// evaluated ids, so the first pass over an index without them is a one-time
// backfill and later passes read only new comments. A comment newer than the
// debounce may still be edited and waits for a later pass.
func (s reviewFeedbackSweep) captureRules(ctx context.Context, repo string, number int, owner string, candidates []github.FeedbackItem, res *reviewFeedbackResult) {
	if s.Rules == nil {
		return
	}
	lane := owner
	if s.Lane != nil {
		lane = s.Lane(owner)
	}
	managed := s.Managed != nil && s.Managed(repo)
	var evaluated []string
	for _, it := range candidates {
		if s.Now.Sub(it.CreatedAt) < s.Limits.Debounce || s.Rules.Evaluated(repo, number, it.ID) {
			continue
		}
		c := reviewrules.Comment{Repo: repo, PR: number, CommentID: it.ID, URL: it.URL, Reviewer: it.Author,
			Association: it.AuthorAssociation, Body: it.Body, At: it.CreatedAt, Lane: lane, ManagedRepo: managed}
		if err := s.Rules.Remember(c, s.Now); err != nil {
			s.Logger.Warn("review feedback: could not remember the comment for rule evidence", "repo", repo, "pr", number, "comment", it.ID, "error", err)
		}
		r := s.Rules.CaptureComment(c, s.Now)
		if r.Outcome != reviewrules.OutcomeRefused {
			evaluated = append(evaluated, it.ID) // a refused rule is tried again on a later pass
		}
		switch r.Outcome {
		case reviewrules.OutcomeNotRule:
			continue
		case reviewrules.OutcomeRefused:
			s.Logger.Warn("review feedback: rule not recorded", "repo", repo, "pr", number, "comment", it.ID, "reviewer", it.Author, "signals", r.Signals, "error", r.Err)
			continue
		case reviewrules.OutcomeCaptured:
			res.RulesCaptured++
		case reviewrules.OutcomeQueued:
			res.RulesQueued++
		case reviewrules.OutcomeMerged:
			res.RulesMerged++
		}
		if r.Outcome != reviewrules.OutcomeDuplicate {
			s.Logger.Info("review feedback: reviewer-stated rule recorded in the hive's knowledge", "repo", repo, "pr", number, "comment", it.ID,
				"reviewer", it.Author, "association", it.AuthorAssociation, "outcome", string(r.Outcome), "rule", r.Rule.ID, "scope", string(r.Rule.Scope),
				"applies_to", r.Rule.AppliesTo, "signals", r.Signals)
		}
	}
	if err := s.Rules.MarkEvaluated(repo, number, evaluated, s.Now); err != nil {
		s.Logger.Warn("review feedback: could not persist the evaluated comment ids; a later pass reads them again", "repo", repo, "pr", number, "error", err)
	}
	s.postRuleNotices(ctx, repo, number)
}

// recordedRules returns the live rules the routed comments were captured as,
// for the agent's brief, whichever pass captured them.
func (s reviewFeedbackSweep) recordedRules(repo string, number int, items []github.FeedbackItem) []reviewfeedback.RecordedRule {
	if s.Rules == nil {
		return nil
	}
	var recorded []reviewfeedback.RecordedRule
	for _, it := range items {
		r := s.Rules.RuleForComment(repo, number, it.ID)
		if r == nil || (r.Status != reviewrules.StatusActive && r.Status != reviewrules.StatusPending) {
			continue
		}
		recorded = append(recorded, reviewfeedback.RecordedRule{ID: r.ID, Text: r.Text, Applies: r.Describe(),
			Queued: r.Status == reviewrules.StatusPending, SourceURL: it.URL})
	}
	return recorded
}

// checkRuleReply verifies the agent's reply after a kick that carried captured
// rules names each of them. When it does not, the miss is logged and one
// needs-human note is posted; the check is then cleared.
func (s reviewFeedbackSweep) checkRuleReply(ctx context.Context, key, repo string, number int, replies []github.FeedbackMessage, res *reviewFeedbackResult) {
	p, ok := s.Store.PendingReplyFor(key)
	if !ok {
		return
	}
	verdict, missing := reviewfeedback.CheckReply(p, replies, s.Now)
	if verdict == reviewfeedback.ReplyWaiting {
		return
	}
	if verdict == reviewfeedback.ReplyMissing {
		s.Logger.Warn("review feedback: agent reply does not name the captured rules, asking a person", "repo", repo, "pr", number, "rules", missing)
		if err := s.Source.CreateIssueComment(ctx, repo, number, reviewfeedback.ReplyCheckNote(missing)); err != nil { // writeback:solicited a person's review feedback on this hive-authored PR
			s.Logger.Warn("review feedback: reply-check note not posted, retrying next pass", "repo", repo, "pr", number, "error", err)
			return
		}
		res.ReplyMissingRules++
	}
	if err := s.Store.ClearPendingReply(key, s.Now); err != nil {
		s.Logger.Warn("review feedback: could not clear the pending reply check", "repo", repo, "pr", number, "error", err)
	}
}

// applyRuleReplies reads the structured block in the agent's own replies on the
// PR. Each reply is applied once; an entry that does not validate, or does not
// cite a comment the hive saw, is logged and dropped.
func (s reviewFeedbackSweep) applyRuleReplies(repo string, number int, replies []github.FeedbackMessage) {
	for _, rp := range replies {
		applied := s.Rules.ApplyReply(reviewfeedback.Key(repo, number)+":"+rp.ID, rp.Body, s.Managed, s.Now)
		for _, a := range applied {
			if a.Outcome == reviewrules.OutcomeRejected {
				s.Logger.Warn("review feedback: agent rule entry rejected", "repo", repo, "pr", number, "reply", rp.ID, "reason", a.Reason)
				continue
			}
			s.Logger.Info("review feedback: agent rule block applied", "repo", repo, "pr", number, "reply", rp.ID, "outcome", string(a.Outcome), "rule", a.Rule.ID)
		}
	}
}

// postRuleNotices tells reviewers what was captured and posts the one
// needs-human proposal for a rule whose home is another project's repo.
func (s reviewFeedbackSweep) postRuleNotices(ctx context.Context, repo string, number int) {
	if acks := s.Rules.UnackedFor(repo, number); len(acks) > 0 {
		if s.acknowledgeRules(ctx, repo, number, reviewrules.AckComment(acks)) {
			ids := make([]string, 0, len(acks))
			for _, a := range acks {
				ids = append(ids, a.CommentID)
			}
			if err := s.Rules.MarkAcked(repo, number, ids); err != nil {
				s.Logger.Warn("review feedback: could not persist rule acknowledgement", "repo", repo, "pr", number, "error", err)
			}
		}
	}
	for _, r := range s.Rules.HomeProposals() {
		if len(r.Provenance) == 0 || r.Provenance[0].Repo != repo || r.Provenance[0].PR != number {
			continue
		}
		if s.proposeRuleHome(ctx, repo, number, reviewrules.HomeProposalNote(r)) {
			if err := s.Rules.MarkHomeNoted(r.ID); err != nil {
				s.Logger.Warn("review feedback: could not persist rule proposal flag", "rule", r.ID, "error", err)
			}
		}
	}
}

// acknowledgeRules posts the reply that says which rules were recorded.
func (s reviewFeedbackSweep) acknowledgeRules(ctx context.Context, repo string, number int, body string) bool {
	if err := s.Source.CreateIssueComment(ctx, repo, number, body); err != nil { // writeback:solicited a person's review feedback on this hive-authored PR
		s.Logger.Warn("review feedback: rule acknowledgement not posted, retrying next pass", "repo", repo, "pr", number, "error", err)
		return false
	}
	return true
}

// proposeRuleHome posts the one needs-human note carrying the proposed text of
// a rule that belongs in a repo the hive does not manage.
func (s reviewFeedbackSweep) proposeRuleHome(ctx context.Context, repo string, number int, body string) bool {
	if err := s.Source.CreateIssueComment(ctx, repo, number, body); err != nil { // writeback:solicited a person's review feedback on this hive-authored PR
		s.Logger.Warn("review feedback: rule proposal note not posted, retrying next pass", "repo", repo, "pr", number, "error", err)
		return false
	}
	return true
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
	store := openReviewFeedbackStore(logger)
	if reviewRuleStore == nil && reviewrules.Enabled() {
		rs, err := reviewrules.Open(reviewrules.Dir(), reviewrules.VaultDir(), reviewrules.ConfigFromEnv())
		if err != nil {
			logger.Warn("review feedback: rule index unreadable or not writable; rules kept in memory only", "dir", reviewrules.Dir(), "error", err)
		}
		reviewRuleStore = rs
	}
	if reviewRuleStore != nil {
		for _, line := range reviewRuleStore.Reconcile(now) {
			logger.Info("review feedback: rule changed by an operator", "change", line)
		}
	}
	available := agentAvailability(cfg, agentMgr)
	sweep := reviewFeedbackSweep{
		Source:       client,
		Store:        store,
		Owners:       auditPRAgents(cfg.Project.Org, now.Add(-auditPRAttributionWindow), ""),
		Kicker:       agentKicker{mgr: agentMgr},
		WritingGuide: cfg.Project.WritingGuideSection(),
		Limits:       reviewfeedback.LimitsFromEnv(),
		MaxPRs:       reviewFeedbackMaxPRsPerPass,
		Cursor:       reviewFeedbackCursor,
		Now:          now,
		Logger:       logger,
		Rules:        reviewRuleStore,
		Lane:         cfg.BaseAgentName,
		Managed:      cfg.IsManagedRepo,
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
			"cap_reached", res.CapReached, "needs_human", res.NeedsHuman, "errors", res.Errors,
			"rules_captured", res.RulesCaptured, "rules_queued", res.RulesQueued, "rules_merged", res.RulesMerged)
	}
}
