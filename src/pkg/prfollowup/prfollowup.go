// Package prfollowup keeps each agent-opened PR tied to the CLI session that
// authored it, and routes follow-up events on that PR (a CI failure, a
// changes-requested review, new review threads) back into that session as its
// next turn instead of a fresh, context-cleared dispatch
// (hivecommons/hive#9583). "Changes requested" is GitHub's review decision;
// "review threads" are the unresolved review-bot threads from review-threads.json (#7360), which are
// already filtered to non-hive authors, so the agent's own replies can never
// trigger another follow-up. Human feedback (conversation comments, review
// bodies, inline comments) arrives through Options.Comments with the same
// per-comment authorship guarantee (github.FetchHumanPRComments).
//
// When the authoring conversation is gone (a pod restart, or the agent's next
// regular kick /clear'd it), a follow-up falls back to the ordinary
// fresh-dispatch path. The PR's handoff note (handoff.go) travels with that
// fallback: HandoffSection renders it, plus any human feedback that had no
// other route, into the agent's next kick, so the fresh session still starts
// from the original reasoning.
//
// It builds on the re-entrant turn model (pkg/turn, RFC #4002) rather than
// adding a parallel mechanism: the pointer for one PR IS a
// turn.SessionEnvelope persisted through turn.FileStore, each delivered
// follow-up is appended to its conversation as a user turn, and every
// delivery is journaled (turn.OpFollowUpKick) before it happens, so a restart
// between intent and settle re-queues the event and a settled event is never
// delivered twice.
//
// Eligibility is the pointer itself. A pointer is written only from the
// PR-request watcher's PR-opened hook, which fires only for a NEW PR the
// hive's App opened for one of its own agents. A PR with no pointer (a
// human's PR, another hive's PR, a PR in a repo no hub dispatched work for)
// is never touched, which closes the #6908 class of bug (an account-scoped
// sweep reaching PRs no hive authorised) by construction.
package prfollowup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/toolapprove"
	"github.com/hivecommons/hive/pkg/turn"
)

const (
	// DirEnvVar overrides the directory pointers are persisted under.
	DirEnvVar = "HIVE_PR_FOLLOWUP_DIR"
	// DefaultDir sits on the /data volume beside the contributor turn
	// envelopes, so pointers survive pod rolls and upgrades.
	DefaultDir = "/data/turn/pr-followups"

	// sessionIDPrefix namespaces pointer envelopes from any other envelope
	// kind that may share a store.
	sessionIDPrefix = "pr-followup:"

	varRepo       = "pr_repo"
	varNumber     = "pr_number"
	varURL        = "pr_url"
	varCLISession = "cli_session"
	// varNote holds the PR's handoff note (see handoff.go).
	varNote = "handoff_note"
	// varLive is "1" while the PR has follow-up activity the authoring agent
	// has not finished with; HandoffSection only renders live PRs.
	varLive = "followup_live"
	// varPending holds the human feedback queued for the next fresh kick
	// (JSON []pendingHandoff).
	varPending = "handoff_pending"
	// varBaseMovedHeadPrefix records the first dirty head seen for a base SHA.
	// A later head SHA while GitHub still reports dirty for that same base is
	// a stale push on top of a known-conflicted branch.
	varBaseMovedHeadPrefix    = "base_moved_head:"
	varStaleHeadNotePrefix    = "stale_head_note:"
	varBaseSyncSuccessPrefix  = "base_sync_success:"
	varBaseSyncConflictPrefix = "base_sync_conflict:"
	varBaseSyncStalePrefix    = "base_sync_stale:"
	// varHandoffs counts human-feedback events handed to a fresh kick; with
	// TurnCount it is the per-PR follow-up budget MaxFollowUpsPerPR bounds.
	varHandoffs = "handoff_count"
	// varSkip records why the PR is currently skipped, so a skip is counted
	// once per transition rather than once per tick.
	varSkip  = "skip_reason"
	liveFlag = "1"

	// The backend-native resume handle captured when the PR opened
	// (hivecommons/hive#9606): the CLI's own conversation id, the transcript
	// it persisted, the command that reopens it, and when it was captured
	// (the staleness clock).
	varResumeBackend    = "resume_backend"
	varResumeID         = "resume_id"
	varResumeTranscript = "resume_transcript"
	varResumeCommand    = "resume_command"
	varResumeAt         = "resume_captured_at"

	// messageTriggerKey tags each follow-up turn appended to the envelope.
	messageTriggerKey = "trigger"
	// TriggerFollowUp is the trigger recorded on those turns.
	TriggerFollowUp = "pr_follow_up"

	// MaxFollowUpsPerPR caps how many follow-ups one PR may feed into its
	// authoring session. A PR that is still red after this many resumed
	// rounds is looping; the existing fix-before-new and escalation paths
	// own it from there.
	MaxFollowUpsPerPR = 8

	// ciExcerptRunes bounds the CI failure excerpt carried into the kick.
	ciExcerptRunes = 1500
	// threadBodyRunes bounds each review-thread excerpt carried into the kick.
	threadBodyRunes = 500
	// shortSHALen is how much of a head SHA the kick names.
	shortSHALen = 7

	// externalRefResumed marks a journal entry delivered into the session.
	externalRefResumed = "resumed"
	// fallbackPrefix prefixes the reason recorded on a journal entry that was
	// handed back to the ordinary fresh-dispatch path.
	fallbackPrefix = "fallback: "
)

// Fallback reasons, recorded on the journal entry and in the Outcome.
const (
	ReasonExpired      = "session older than max age"
	ReasonSessionGone  = "authoring session no longer live"
	ReasonSessionMoved = "agent moved on to other work since the PR opened"
	ReasonCapReached   = "follow-up cap reached"
	ReasonNoResumer    = "no agent manager"
	ReasonSuperseded   = "superseded before delivery"
)

// Skip reasons: a PR with a pointer that is currently not routed at all.
const (
	SkipDraft     = "draft"
	SkipFork      = "fork"
	SkipEscalated = "escalated to a human"
)

// Audit actions recorded through Options.Audit / SweepOptions.Audit.
const (
	AuditActionRouted  = "pr_followup_routed"
	AuditActionSkipped = "pr_followup_skipped"
	AuditActionPruned  = "pr_followup_pruned"
)

// AuditFunc records one audit event (see agentaudit.AuditSink.Record; the
// actor is always the hive itself).
type AuditFunc func(action, agent string, fields map[string]any)

// storeMu serialises every read-modify-write of the pointer store in this
// process: the eval tick (Route, Sweep) and the PR-opened hook (Record) run on
// different goroutines. HandoffSection only reads and does not take it.
var storeMu sync.Mutex

// Route values reported in Outcome.
const (
	RouteResumed  = "resumed"
	RouteFallback = "fallback"
	RouteDeferred = "deferred"
	RouteUpdated  = "updated_branch"
)

// ErrBusy is returned by a Resumer when the authoring session exists but
// cannot take the follow-up right now. The follow-up stays queued (its journal
// entry stays intended) and is retried on the next Route call.
var ErrBusy = errors.New("prfollowup: session busy")

// Resumer is the agent-manager surface Route needs. SessionID names the
// conversation an agent's CLI currently holds; SendResumeKick delivers a
// message into exactly that conversation without clearing it, returning an
// error wrapping ErrBusy for a transient refusal and any other error when the
// session is gone.
type Resumer interface {
	SessionID(agent string) (string, bool)
	SendResumeKick(agent, message, sessionID string) error
}

// Dir resolves the pointer directory: DirEnvVar, else DefaultDir.
func Dir() string {
	if d := strings.TrimSpace(os.Getenv(DirEnvVar)); d != "" {
		return d
	}
	return DefaultDir
}

// ThreadsKey keys Options.Threads.
func ThreadsKey(repo string, number int) string {
	return repo + "#" + strconv.Itoa(number)
}

// ThreadsFromReport indexes a review-threads.json report for Options.Threads,
// dropping PRs escalated to a human.
func ThreadsFromReport(report github.ReviewThreadsReport) map[string][]github.ReviewThread {
	out := map[string][]github.ReviewThread{}
	for _, pr := range report.PRs {
		if pr.Escalated || len(pr.Threads) == 0 {
			continue
		}
		out[ThreadsKey(pr.Repo, pr.Number)] = pr.Threads
	}
	return out
}

// PointerID is the envelope session ID for one PR.
func PointerID(repo string, number int) string {
	return fmt.Sprintf("%s%s#%d", sessionIDPrefix, repo, number)
}

// Record saves (or re-points) the pointer for a PR an agent just opened.
// cliSession is the agent's live session ID at that moment; empty means the
// agent had no resumable session, and the pointer is still written so the PR
// is known to be this hive's (later follow-ups then fall back).
func Record(ctx context.Context, dir, agentName, repo string, number int, url, cliSession string, now time.Time) error {
	return RecordWithNote(ctx, dir, agentName, repo, number, url, cliSession, "", now)
}

// RecordWithNote is Record plus the PR's handoff note (BuildHandoffNote). An
// empty note leaves any note already saved for the PR in place.
func RecordWithNote(ctx context.Context, dir, agentName, repo string, number int, url, cliSession, note string, now time.Time) error {
	return RecordWithResume(ctx, dir, agentName, repo, number, url, cliSession, note, ResumeHandle{}, now)
}

// RecordWithResume is RecordWithNote plus the backend-native resume handle
// captured for the authoring conversation (hivecommons/hive#9606). A zero
// handle leaves any handle already saved for the PR in place, so a re-point
// from a backend that exposes none never erases one.
func RecordWithResume(ctx context.Context, dir, agentName, repo string, number int, url, cliSession, note string, resume ResumeHandle, now time.Time) error {
	storeMu.Lock()
	defer storeMu.Unlock()
	if strings.TrimSpace(agentName) == "" || strings.TrimSpace(repo) == "" || number <= 0 {
		return fmt.Errorf("prfollowup: agent, repo and PR number are required (agent=%q repo=%q number=%d)", agentName, repo, number)
	}
	store := turn.FileStore{Dir: dir}
	id := PointerID(repo, number)
	env, err := store.Load(ctx, id)
	switch {
	case err == nil && pointerIs(env, repo, number):
		// Same PR seen again: keep its journal and history, re-point.
	case err == nil || errors.Is(err, fs.ErrNotExist):
		// New PR, or a filename collision with a different PR (the store
		// sanitises IDs): start a fresh pointer.
		env = turn.SessionEnvelope{
			SessionID:   id,
			Status:      turn.StatusActive,
			WorkingRepo: repo,
			CreatedAt:   now,
			Variables: map[string]string{
				varRepo:   repo,
				varNumber: strconv.Itoa(number),
			},
		}
		env.Messages = append(env.Messages, turn.Message{
			Role:      turn.RoleSystem,
			Content:   fmt.Sprintf("PR follow-up pointer: %s#%d was opened by agent %s; follow-up events on it are routed back to this session.", repo, number, agentName),
			Timestamp: now,
		})
	default:
		return err
	}
	env.Agent = toolapprove.AgentIdentity{Name: agentName}
	env.Variables[varURL] = url
	env.Variables[varCLISession] = cliSession
	if note = strings.TrimSpace(note); note != "" {
		env.Variables[varNote] = note
	}
	setResumeHandle(&env, resume)
	env.UpdatedAt = now
	return store.Persist(ctx, env)
}

// PointerCreatedAt returns when the pointer for repo#number was created, and
// whether this hive holds one (the eligibility test: a PR without a pointer
// is not this hive's to follow up).
func PointerCreatedAt(ctx context.Context, dir, repo string, number int) (time.Time, bool) {
	storeMu.Lock()
	defer storeMu.Unlock()
	env, err := turn.FileStore{Dir: dir}.Load(ctx, PointerID(repo, number))
	if err != nil || !pointerIs(env, repo, number) {
		return time.Time{}, false
	}
	return env.CreatedAt, true
}

// pointerIs guards against the store's filename sanitisation mapping two
// different PRs onto one file: the envelope must name exactly this PR.
func pointerIs(env turn.SessionEnvelope, repo string, number int) bool {
	return env.Variables[varRepo] == repo && env.Variables[varNumber] == strconv.Itoa(number)
}

// EventKind classifies a follow-up event.
type EventKind string

const (
	EventBaseMoved        EventKind = "base_moved"
	EventStaleHeadPush    EventKind = "stale_head_push"
	EventCIFailure        EventKind = "ci_failure"
	EventChangesRequested EventKind = "changes_requested"
	EventReviewThread     EventKind = "review_thread"
	EventHumanComment     EventKind = "human_comment"
)

// Event is one follow-up to deliver. Key is stable for the underlying fact
// (the red head SHA, the set of reviewers requesting changes, the review
// thread's node ID), so the journal can tell a new event from one already
// handled.
type Event struct {
	Kind   EventKind
	Key    string
	Detail string
}

// Options configures Route.
type Options struct {
	// Dir is the pointer directory (see Dir).
	Dir string
	// MaxAge is the resume window measured from the PR's pointer creation.
	// Zero or negative disables the age check.
	MaxAge time.Duration
	// Skip, when non-nil, excludes a PR outright (escalated to a human).
	Skip func(repo string, number int) bool
	// Threads holds the unresolved review-bot threads per PR, keyed by
	// ThreadsKey (from review-threads.json). Nil means none known.
	Threads map[string][]github.ReviewThread
	// Comments holds the human feedback per PR, keyed by ThreadsKey (from
	// github.FetchHumanPRComments, already filtered to human authors). Nil
	// means none known.
	Comments map[string][]github.PRComment
	// BaseSyncEnabled reports whether the owner enabled
	// review.contributor_prs.base_sync for a repo. Nil is disabled.
	BaseSyncEnabled func(repo string) bool
	// BranchUpdater is the GitHub write surface used for owner-gated fork
	// base sync and stale-head notes. Nil makes Route fall back to comments
	// and kicks only.
	BranchUpdater BranchUpdater
	// Audit, when non-nil, records every routing decision and skip.
	Audit  AuditFunc
	Logger *slog.Logger
}

// BranchUpdater is the narrow GitHub write surface Route needs.
type BranchUpdater interface {
	UpdateBranch(ctx context.Context, repo string, number int) error
	UpdateBranchExpectedHead(ctx context.Context, repo string, number int, expectedHeadSHA string, observedBaseSHA string) error
	CreateIssueComment(ctx context.Context, repo string, number int, body string) error
}

// Outcome reports what Route did for one PR that had pending follow-ups.
type Outcome struct {
	Repo   string
	Number int
	Agent  string
	Events []Event
	Route  string
	Reason string
	// Queued counts human-feedback events handed to the agent's next fresh
	// kick (HandoffSection) because the session could not be resumed.
	Queued int
}

// Route walks the open PRs, and for every PR this hive authored (has a
// pointer) with new follow-up events, either resumes the authoring session
// with them or records a fallback to the ordinary fresh-dispatch path. It
// reads signals already on the PR from the eval tick's enumeration, or
// collected by the caller (Threads, Comments). The only optional GitHub write
// is the owner-gated contributor_prs base sync for hive-authored fork PRs.
func Route(ctx context.Context, prs []github.PullRequest, r Resumer, opts Options, now time.Time) []Outcome {
	storeMu.Lock()
	defer storeMu.Unlock()
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	store := turn.FileStore{Dir: opts.Dir}
	var outcomes []Outcome
	var delta Stats
	for i := range prs {
		pr := &prs[i]
		if pr.Number <= 0 || pr.Repo == "" {
			continue
		}
		env, err := store.Load(ctx, PointerID(pr.Repo, pr.Number))
		if errors.Is(err, fs.ErrNotExist) {
			continue // not a PR this hive's agents opened
		}
		if err != nil {
			logger.Warn("prfollowup: unreadable pointer; leaving PR to the fresh-dispatch path",
				"repo", pr.Repo, "pr", pr.Number, "error", err)
			continue
		}
		if !pointerIs(env, pr.Repo, pr.Number) || env.Agent.Name == "" {
			continue
		}
		var out *Outcome
		changed := false
		if reason := skipReason(pr, opts); reason != "" {
			// Skipped PRs are left entirely alone (no events are journaled),
			// so a draft marked ready, or a PR a human hands back, routes its
			// follow-ups normally again. Only the transition is counted.
			if env.Variables[varSkip] != reason {
				env.Variables[varSkip] = reason
				changed = true
				delta.addSkipped(reason)
				audit(opts.Audit, AuditActionSkipped, env.Agent.Name, "outcome", "skipped", "reason", reason,
					"repo", pr.Repo, "pr", pr.Number)
			}
			if env.Variables[varLive] != "" {
				delete(env.Variables, varLive)
				changed = true
			}
		} else {
			if env.Variables[varSkip] != "" {
				delete(env.Variables, varSkip)
				changed = true
			}
			key := ThreadsKey(pr.Repo, pr.Number)
			var routed bool
			out, routed = routeOne(ctx, &env, pr, opts.Threads[key], opts.Comments[key], r, opts, now, &delta)
			changed = changed || routed
		}
		if changed {
			env.UpdatedAt = now
			if err := store.Persist(ctx, env); err != nil {
				logger.Warn("prfollowup: failed to persist pointer", "repo", pr.Repo, "pr", pr.Number, "error", err)
			}
		}
		if out != nil {
			level := slog.LevelInfo
			if out.Route == RouteDeferred {
				level = slog.LevelDebug // retried every tick while the agent works
			} else {
				audit(opts.Audit, AuditActionRouted, out.Agent, "outcome", out.Route, "reason", out.Reason,
					"repo", out.Repo, "pr", out.Number, "events", len(out.Events), "kinds", eventKinds(out.Events),
					"queued", out.Queued)
			}
			logger.Log(ctx, level, "prfollowup: routed PR follow-up", "repo", out.Repo, "pr", out.Number,
				"agent", out.Agent, "route", out.Route, "reason", out.Reason, "events", len(out.Events), "queued", out.Queued)
			outcomes = append(outcomes, *out)
		}
	}
	if err := addStats(opts.Dir, delta, now); err != nil {
		logger.Warn("prfollowup: failed to update follow-up counters", "error", err)
	}
	return outcomes
}

// skipReason says why a PR with a pointer is not routed at all, or "".
func skipReason(pr *github.PullRequest, opts Options) string {
	switch {
	case pr.Draft:
		return SkipDraft
	case pr.FromFork && !needsBaseSync(pr):
		return SkipFork
	case opts.Skip != nil && opts.Skip(pr.Repo, pr.Number):
		return SkipEscalated
	}
	return ""
}

func audit(fn AuditFunc, action, agent string, kv ...any) {
	if fn == nil {
		return
	}
	fields := make(map[string]any, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		k, ok := kv[i].(string)
		if !ok {
			continue
		}
		if s, isStr := kv[i+1].(string); isStr && s == "" {
			continue
		}
		fields[k] = kv[i+1]
	}
	fn(action, agent, fields)
}

func eventKinds(events []Event) string {
	seen := map[EventKind]bool{}
	var kinds []string
	for _, ev := range events {
		if !seen[ev.Kind] {
			seen[ev.Kind] = true
			kinds = append(kinds, string(ev.Kind))
		}
	}
	sort.Strings(kinds)
	return strings.Join(kinds, ",")
}

// routeOne handles one eligible PR. It returns the outcome (nil when there
// was nothing to do) and whether env changed and must be persisted.
func routeOne(ctx context.Context, env *turn.SessionEnvelope, pr *github.PullRequest, threads []github.ReviewThread, comments []github.PRComment, r Resumer, opts Options, now time.Time, delta *Stats) (result *Outcome, changed bool) {
	if out, ok := tryOwnerGatedBaseSync(ctx, env, pr, opts, now, delta); ok {
		return out, true
	}
	events := detectEvents(env, pr, threads, comments, env.CreatedAt)
	changed = rememberDirtyBaseHead(env, pr)
	changed = reconcilePending(env, r, delta) || changed
	// A PR is "live" while it still has follow-up activity: a fact that stays
	// true until the agent fixes it (red CI, changes requested, an open bot
	// thread) or human feedback still waiting for a fresh kick. A human
	// comment that was already resumed or handed off does not keep it live.
	// Only live PRs get their handoff note in the agent's kicks.
	defer func() {
		if setLive(env, hasStandingFact(events) || len(pendingHandoffs(env)) > 0) {
			changed = true
		}
	}()
	keyOf := func(ev Event) string {
		return turn.DeriveIdempotencyKey(env.SessionID, turn.OpIntent{
			Kind: turn.OpFollowUpKick, Repo: pr.Repo, Target: "#" + strconv.Itoa(pr.Number), Body: ev.Key,
		})
	}

	// Pending = detected now and not yet settled. An entry still "intended"
	// from an earlier call (busy session, or a restart mid-delivery) is
	// re-queued here; one whose fact no longer holds is settled superseded.
	var pending []Event
	live := map[string]bool{}
	for _, ev := range events {
		key := keyOf(ev)
		live[key] = true
		if e, ok := env.Journal.Lookup(key); ok && !e.Ambiguous() {
			continue
		}
		pending = append(pending, ev)
	}
	for _, e := range env.Journal.Ambiguous() {
		if e.Kind == turn.OpFollowUpKick && !live[e.IdempotencyKey] && !needsBaseSync(pr) {
			env.Journal.Settle(e.IdempotencyKey, turn.OpFailed, "", fallbackPrefix+ReasonSuperseded, now)
			changed = true
		}
	}
	if len(pending) == 0 {
		return nil, changed
	}

	out := &Outcome{Repo: pr.Repo, Number: pr.Number, Agent: env.Agent.Name, Events: pending}
	intend := func() {
		for _, ev := range pending {
			env.Journal.RecordIntent(keyOf(ev), turn.OpIntent{
				Kind: turn.OpFollowUpKick, Repo: pr.Repo, Target: "#" + strconv.Itoa(pr.Number), Body: ev.Key,
			}, now)
		}
	}
	settle := func(status turn.OpStatus, ref, errStr string) {
		for _, ev := range pending {
			env.Journal.Settle(keyOf(ev), status, ref, errStr, now)
		}
	}
	// fallback settles the (already intended) events as not resumed.
	fallback := func(reason string) (*Outcome, bool) {
		settle(turn.OpFailed, "", fallbackPrefix+reason)
		out.Route, out.Reason = RouteFallback, reason
		out.Queued = queueHandoffs(env, pending, r, now)
		delta.addFallback(reason, len(pending))
		delta.HandoffsQueued += out.Queued
		return out, true
	}

	if postStaleHeadPushNotes(ctx, env, opts.BranchUpdater, pr, pending) {
		changed = true
	}
	if reason := resumeBlocker(env, r, opts, now); reason != "" {
		intend()
		return fallback(reason)
	}
	current, _ := r.SessionID(env.Agent.Name)

	// Journal the intent BEFORE the effect: a crash between here and the
	// settle below leaves the entries intended, and the next call re-queues
	// them (against a new process, whose session ID can no longer match, so
	// they fall back rather than being lost or delivered twice).
	intend()
	msg := BuildMessage(pr, env.Variables[varURL], pending)
	if err := r.SendResumeKick(env.Agent.Name, msg, current); err != nil {
		if errors.Is(err, ErrBusy) {
			out.Route, out.Reason = RouteDeferred, err.Error()
			delta.Deferred++
			return out, true
		}
		return fallback(err.Error())
	}
	settle(turn.OpSucceeded, externalRefResumed, "")
	env.AddMessage(turn.RoleUser, msg)
	env.Messages[len(env.Messages)-1].Metadata = map[string]string{messageTriggerKey: TriggerFollowUp}
	env.TurnCount++
	// The delivered kick is itself a new LastKick, so the session ID moves;
	// follow it so the NEXT follow-up resumes the same conversation too.
	if next, ok := r.SessionID(env.Agent.Name); ok {
		env.Variables[varCLISession] = next
	}
	out.Route = RouteResumed
	delta.Resumed += len(pending)
	return out, true
}

// hasStandingFact reports whether events include a fact that stays true
// until the agent acts on it (anything but a one-off human comment).
func hasStandingFact(events []Event) bool {
	for _, ev := range events {
		if ev.Kind != EventHumanComment {
			return true
		}
	}
	return false
}

// setLive records whether the PR has live follow-up activity and reports
// whether that changed.
func setLive(env *turn.SessionEnvelope, live bool) bool {
	was := env.Variables[varLive] == liveFlag
	if live == was {
		return false
	}
	if live {
		env.Variables[varLive] = liveFlag
	} else {
		delete(env.Variables, varLive)
	}
	return true
}

// followUpsUsed is how much of MaxFollowUpsPerPR a PR has spent: follow-ups
// resumed into the authoring session plus human feedback handed to a fresh
// kick.
func followUpsUsed(env *turn.SessionEnvelope) int {
	n, _ := strconv.Atoi(env.Variables[varHandoffs])
	return env.TurnCount + n
}

// resumeBlocker returns why the authoring session cannot be resumed, or ""
// when it can.
func resumeBlocker(env *turn.SessionEnvelope, r Resumer, opts Options, now time.Time) string {
	if opts.MaxAge > 0 && now.Sub(env.CreatedAt) > opts.MaxAge {
		return ReasonExpired
	}
	if followUpsUsed(env) >= MaxFollowUpsPerPR {
		return ReasonCapReached
	}
	if r == nil {
		return ReasonNoResumer
	}
	saved := env.Variables[varCLISession]
	current, ok := r.SessionID(env.Agent.Name)
	if !ok || saved == "" {
		return ReasonSessionGone
	}
	if current != saved {
		return ReasonSessionMoved
	}
	return ""
}

// detectEvents derives the follow-up events visible on pr, its unresolved
// review-bot threads, and the human comments left since the pointer was
// created (since).
func needsBaseSync(pr *github.PullRequest) bool {
	return pr.BaseSHA != "" && (pr.MergeableState == "dirty" || pr.MergeableState == "behind")
}

func baseSyncKey(pr *github.PullRequest) string {
	return "base-sync:" + pr.BaseSHA + ":" + pr.HeadSHA
}

func baseMovedHeadVar(baseSHA string) string {
	return varBaseMovedHeadPrefix + baseSHA
}

func baseHeadKey(pr *github.PullRequest) string {
	return pr.BaseSHA + ":" + pr.HeadSHA
}

func baseSyncEnabled(opts Options, repo string) bool {
	return opts.BaseSyncEnabled != nil && opts.BaseSyncEnabled(repo)
}

func tryOwnerGatedBaseSync(ctx context.Context, env *turn.SessionEnvelope, pr *github.PullRequest, opts Options, now time.Time, delta *Stats) (*Outcome, bool) {
	if !pr.FromFork || !needsBaseSync(pr) || !baseSyncEnabled(opts, pr.Repo) || !pr.MaintainerCanModify || opts.BranchUpdater == nil {
		return nil, false
	}
	if env.Variables[varBaseSyncSuccessPrefix+baseHeadKey(pr)] != "" || env.Variables[varBaseSyncConflictPrefix+pr.BaseSHA] != "" || env.Variables[varBaseSyncStalePrefix+baseHeadKey(pr)] != "" {
		return nil, false
	}
	key := turn.DeriveIdempotencyKey(env.SessionID, turn.OpIntent{
		Kind: turn.OpFollowUpKick, Repo: pr.Repo, Target: "#" + strconv.Itoa(pr.Number), Body: baseSyncKey(pr),
	})
	if e, ok := env.Journal.Lookup(key); ok && !e.Ambiguous() {
		return nil, false
	}
	env.Journal.RecordIntent(key, turn.OpIntent{
		Kind: turn.OpFollowUpKick, Repo: pr.Repo, Target: "#" + strconv.Itoa(pr.Number), Body: baseSyncKey(pr),
	}, now)
	if err := opts.BranchUpdater.UpdateBranchExpectedHead(ctx, pr.Repo, pr.Number, pr.HeadSHA, pr.BaseSHA); err != nil {
		if updateBranchStaleHead(err) {
			env.Variables[varBaseSyncStalePrefix+baseHeadKey(pr)] = "1"
			env.Journal.Settle(key, turn.OpFailed, "", "update-branch stale head; waiting for next PR snapshot", now)
			return nil, true
		}
		if updateBranchConflict(err) {
			env.Variables[varBaseSyncConflictPrefix+pr.BaseSHA] = "1"
			env.Journal.Settle(key, turn.OpFailed, "", "update-branch conflict; routing base repair", now)
			return nil, false
		}
		env.Journal.Settle(key, turn.OpFailed, "", err.Error(), now)
		out := &Outcome{Repo: pr.Repo, Number: pr.Number, Agent: env.Agent.Name, Route: RouteDeferred, Reason: err.Error()}
		delta.Deferred++
		return out, true
	}
	env.Variables[varBaseSyncSuccessPrefix+baseHeadKey(pr)] = "1"
	env.Journal.Settle(key, turn.OpSucceeded, "update-branch", "", now)
	return &Outcome{Repo: pr.Repo, Number: pr.Number, Agent: env.Agent.Name, Route: RouteUpdated}, true
}

func updateBranchStaleHead(err error) bool {
	if !isUpdateBranch422(err) {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "expected_head_sha") ||
		strings.Contains(msg, "expected head") ||
		strings.Contains(msg, "head sha") ||
		strings.Contains(msg, "head_sha") ||
		strings.Contains(msg, "does not match")
}

func updateBranchConflict(err error) bool {
	if !isUpdateBranch422(err) {
		return false
	}
	return true
}

func isUpdateBranch422(err error) bool {
	var apiErr *gh.ErrorResponse
	if errors.As(err, &apiErr) && apiErr.Response != nil && apiErr.Response.StatusCode == http.StatusUnprocessableEntity {
		return true
	}
	return strings.Contains(err.Error(), "422") || strings.Contains(strings.ToLower(err.Error()), "validation failed")
}

func rememberDirtyBaseHead(env *turn.SessionEnvelope, pr *github.PullRequest) bool {
	if pr.BaseSHA == "" || pr.HeadSHA == "" || pr.MergeableState != "dirty" {
		return false
	}
	key := baseMovedHeadVar(pr.BaseSHA)
	if env.Variables[key] != "" {
		return false
	}
	env.Variables[key] = pr.HeadSHA
	return true
}

func staleHeadPushEvent(env turn.SessionEnvelope, pr *github.PullRequest) (Event, bool) {
	if pr.BaseSHA == "" || pr.HeadSHA == "" || pr.MergeableState != "dirty" {
		return Event{}, false
	}
	first := env.Variables[baseMovedHeadVar(pr.BaseSHA)]
	if first == "" || first == pr.HeadSHA {
		return Event{}, false
	}
	return Event{
		Kind:   EventStaleHeadPush,
		Key:    "stale-head:" + pr.BaseSHA + ":" + pr.HeadSHA,
		Detail: fmt.Sprintf("<!-- hive-stale-head-push --> New head %s was pushed while this PR is still dirty against base %s. Repair the base conflict first: fetch %s at %s and merge or rebase the PR branch before addressing any other work.", shortSHA(pr.HeadSHA), pr.BaseRef, pr.BaseRef, pr.BaseSHA),
	}, true
}

func postStaleHeadPushNotes(ctx context.Context, env *turn.SessionEnvelope, commenter BranchUpdater, pr *github.PullRequest, events []Event) bool {
	if commenter == nil {
		return false
	}
	changed := false
	for _, ev := range events {
		if ev.Kind == EventStaleHeadPush {
			key := varStaleHeadNotePrefix + pr.HeadSHA
			if env.Variables[key] != "" {
				continue
			}
			env.Variables[key] = "1"
			changed = true
			_ = commenter.CreateIssueComment(ctx, pr.Repo, pr.Number, ev.Detail)
		}
	}
	return changed
}

func detectEvents(env *turn.SessionEnvelope, pr *github.PullRequest, threads []github.ReviewThread, comments []github.PRComment, since time.Time) []Event {
	// Base repair takes precedence over review feedback. Do not settle review
	// events until the PR is mergeable again; they will be detected next pass.
	// Key by base, not head: pushing onto the stale head must not create a
	// fresh repair event on every tick. Include state so behind -> dirty wakes
	// the owner again even if an earlier update attempt did not complete.
	if needsBaseSync(pr) {
		if env.Variables[varBaseSyncSuccessPrefix+baseHeadKey(pr)] != "" {
			return nil
		}
		if env.Variables[varBaseSyncStalePrefix+baseHeadKey(pr)] != "" {
			return nil
		}
		if ev, ok := staleHeadPushEvent(*env, pr); ok {
			return []Event{ev}
		}
		return []Event{{
			Kind:   EventBaseMoved,
			Key:    "base:" + pr.BaseSHA + ":" + pr.MergeableState,
			Detail: fmt.Sprintf("<!-- hive-base-moved --> Base %s is now at %s; this PR is %s. Before addressing review threads or pushing any new work, fetch the base and merge or rebase your own PR branch onto it, resolving conflicts while preserving the PR's intent. Do not rewrite a branch you do not own. Reply on the PR naming this base commit after repair.", pr.BaseRef, pr.BaseSHA, pr.MergeableState),
		}}
	}
	var events []Event
	// A settled red CI: CIChecksRunning means other shards are still
	// reporting, and the whole failure set is not known yet.
	if pr.CIStatus == "failure" && !pr.CIChecksRunning && pr.HeadSHA != "" {
		events = append(events, Event{
			Kind:   EventCIFailure,
			Key:    "ci:" + pr.HeadSHA,
			Detail: ciDetail(pr),
		})
	}
	if pr.Protection != nil && pr.Protection.ReviewDecision == github.ReviewDecisionChangesRequested && !github.HumanReviewAddressed(*pr) {
		by := append([]string(nil), pr.Protection.ChangesRequestedBy...)
		sort.Strings(by)
		detail := "A reviewer requested changes."
		if len(by) > 0 {
			detail = "Changes requested by " + strings.Join(by, ", ") + "."
		}
		key := "review:" + strings.Join(by, ",")
		if !pr.Protection.LatestHumanReviewSubmittedAt.IsZero() {
			key = fmt.Sprintf("%s:%s", key, pr.Protection.LatestHumanReviewSubmittedAt.UTC().Format(time.RFC3339Nano))
		}
		events = append(events, Event{
			Kind:   EventChangesRequested,
			Key:    key,
			Detail: detail,
		})
	}
	for _, th := range threads {
		if th.ThreadID == "" {
			continue
		}
		events = append(events, Event{
			Kind:   EventReviewThread,
			Key:    "thread:" + th.ThreadID,
			Detail: threadDetail(th),
		})
	}
	for _, c := range comments {
		// Dedupe is per comment id (the journal key). A comment older than
		// the pointer predates the PR's follow-up window: never routed.
		if c.ID == "" || c.CreatedAt.Before(since) {
			continue
		}
		events = append(events, Event{
			Kind:   EventHumanComment,
			Key:    "comment:" + c.ID,
			Detail: commentDetail(c),
		})
	}
	return events
}

func commentDetail(c github.PRComment) string {
	what := "Comment"
	switch c.Kind {
	case github.PRCommentReview:
		what = "Review"
	case github.PRCommentInline:
		what = "Review comment"
	}
	where := ""
	if c.Path != "" {
		where = " on " + c.Path
		if c.Line > 0 {
			where = fmt.Sprintf(" on %s:%d", c.Path, c.Line)
		}
	}
	return fmt.Sprintf("%s from %s%s (%s): %s", what, c.Author, where, c.ID, truncateRunes(strings.TrimSpace(c.Body), threadBodyRunes))
}

func shortSHA(sha string) string {
	if len(sha) <= shortSHALen {
		return sha
	}
	return sha[:shortSHALen]
}

func threadDetail(th github.ReviewThread) string {
	where := th.Path
	if th.Line > 0 {
		where = fmt.Sprintf("%s:%d", th.Path, th.Line)
	}
	return fmt.Sprintf("Review comment from %s on %s (thread %s): %s", th.Author, where, th.ThreadID, truncateRunes(strings.TrimSpace(th.Body), threadBodyRunes))
}

func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "..."
}

func ciDetail(pr *github.PullRequest) string {
	var b strings.Builder
	sha := pr.HeadSHA
	if len(sha) > shortSHALen {
		sha = sha[:shortSHALen]
	}
	fmt.Fprintf(&b, "CI failed on %s", sha)
	if len(pr.FailingChecks) > 0 {
		fmt.Fprintf(&b, " (%s)", strings.Join(pr.FailingChecks, ", "))
	}
	b.WriteString(".")
	if ex := strings.TrimSpace(pr.CIFailureExcerpt); ex != "" {
		ex = truncateRunes(ex, ciExcerptRunes)
		b.WriteString("\nFailure excerpt:\n")
		b.WriteString(ex)
	}
	return b.String()
}

// BuildMessage renders the follow-up turn typed into the authoring session.
// It speaks to the agent as the continuation it is, so the agent picks up
// its own earlier reasoning rather than re-deriving it.
func BuildMessage(pr *github.PullRequest, url string, events []Event) string {
	var b strings.Builder
	fmt.Fprintf(&b, "PR FOLLOW-UP: new activity on %s#%d", pr.Repo, pr.Number)
	if url == "" {
		url = pr.URL
	}
	if url != "" {
		fmt.Fprintf(&b, " (%s)", url)
	}
	b.WriteString(", the PR you opened earlier in this session. Continue from where you left off.\n")
	for _, ev := range events {
		b.WriteString("- ")
		b.WriteString(ev.Detail)
		b.WriteString("\n")
	}
	b.WriteString("Read the new feedback on the PR, push any fix to the same branch, and reply on the PR. Do not open a new PR.")
	return b.String()
}
