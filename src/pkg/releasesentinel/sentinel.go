// Package releasesentinel is the opt-in release sentinel (hivecommons/hive#9585):
// a bounded repair loop scoped to ONE release. When the workflow runs on the
// commit the current v<version> tag points at conclude with a blocking
// failure, the sentinel dispatches a repair round to an agent through the
// hive's existing kick path, waits for CI to re-run, and gives up (and tells a
// human) after a hard cap on rounds.
//
// Why it exists: a failed release used to stall until a person noticed.
// #5875 (Actions lost permission to open the release PR, v4.9.0 blocked),
// #6804 (v4.29.2 stuck on a release-gate push the App token could not make)
// and #7123 (release-gate workflows bulk-failed) each sat red for hours.
//
// Scope of this package:
//   - The durable per-release state machine
//     awaiting_ci -> fixing -> green | failed | superseded, persisted to a
//     JSON file on the PVC so a restart resumes exactly where it stopped.
//   - Matching completed workflow runs to the SHA the tag points at NOW. A run
//     for any other SHA is stale (the tag moved) and is never acted on.
//   - Classifying conclusions: failure, timed_out and startup_failure block;
//     cancelled, skipped, neutral, action_required and success do not.
//   - Classifying failures an agent cannot fix (org/repo policy, missing
//     permission, missing secret, a run that never started a job). Those are
//     escalated to a human on the FIRST round, with no dispatch and no push.
//   - Hard caps: MaxRounds (default 5) and a per-round timeout.
//
// It never pushes, never moves a tag and never talks to an agent runtime of
// its own: a round is handed to a Dispatcher (in the hive: a targeted kick to
// the ci-maintainer lane), and escalation goes to an Escalator (the hive's
// notification channels). Moving the tag together with a fix push is a
// deliberately separate, later step.
package releasesentinel

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// State is one node of the per-release state machine.
type State string

const (
	// StateAwaitingCI: the tag's CI has not produced a blocking failure the
	// sentinel has acted on yet (or a round's fix is back in CI).
	StateAwaitingCI State = "awaiting_ci"
	// StateFixing: a repair round was dispatched and has not timed out.
	StateFixing State = "fixing"
	// StateGreen: every run on the tag's SHA is non-blocking AND a published
	// (non-draft) GitHub Release exists for the tag. Terminal.
	StateGreen State = "green"
	// StateFailed: the round cap was reached, or the failure is not something
	// an agent can fix. A human has been notified. Terminal.
	StateFailed State = "failed"
	// StateSuperseded: a newer v<version> tag became current before this one
	// went green or failed. Terminal.
	StateSuperseded State = "superseded"
)

// Terminal reports whether no further transition is possible from s.
func (s State) Terminal() bool {
	return s == StateGreen || s == StateFailed || s == StateSuperseded
}

const (
	// DefaultMaxRounds bounds how many repair rounds one release may consume
	// before the sentinel gives up and escalates.
	DefaultMaxRounds = 5
	// DefaultRoundTimeout bounds how long one dispatched round may run without
	// the tag's CI going green before the round counts as spent.
	DefaultRoundTimeout = 2 * time.Hour
	// DefaultPollInterval is how often the hive evaluates the sentinel. The
	// governor's eval tick runs about once a minute; release CI moves on the
	// scale of minutes, and every pass costs a few Actions API calls.
	DefaultPollInterval = 5 * time.Minute
	// DefaultAgent is the lane a repair round is dispatched to.
	DefaultAgent = "ci-maintainer"
	// maxHistory bounds the per-release transition log.
	maxHistory = 50
	// maxRecords bounds how many releases the state file remembers; the oldest
	// terminal records are dropped first.
	maxRecords = 20
)

// Tag is the release tag the sentinel is currently scoped to.
type Tag struct {
	Name string // "v5.86.1"
	SHA  string // the commit the tag points at right now
}

// Run is one workflow run observed on a commit.
type Run struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	HeadSHA    string `json:"head_sha"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion,omitempty"`
	URL        string `json:"url,omitempty"`
}

// RunDetails is the failure evidence for one blocking run, fetched only when
// a round is about to start.
type RunDetails struct {
	// JobCount is how many jobs the run's latest attempt produced. A failed run
	// with zero jobs never executed anything: an approval gate, billing or org
	// policy stopped it, and no code change can fix that (#7123).
	JobCount int `json:"job_count"`
	// FailedJobs are "job / step" labels for the jobs that failed.
	FailedJobs []string `json:"failed_jobs,omitempty"`
	// Evidence holds the failure annotations (::error lines) of failed jobs.
	Evidence []string `json:"evidence,omitempty"`
}

// ReleaseInfo describes the GitHub Release for a tag.
type ReleaseInfo struct {
	Exists bool
	Draft  bool
	URL    string
}

// Source is the read-only view of the forge the sentinel needs.
type Source interface {
	// CurrentTag returns the highest v<MAJOR>.<MINOR>.<PATCH> tag and the SHA
	// it points at. ok=false means the repo has no release tag yet.
	CurrentTag(ctx context.Context) (tag Tag, ok bool, err error)
	// Runs lists the latest workflow run per workflow for sha.
	Runs(ctx context.Context, sha string) ([]Run, error)
	// Details fetches failure evidence for one run.
	Details(ctx context.Context, runID int64) (RunDetails, error)
	// Release reports whether a GitHub Release exists for tag.
	Release(ctx context.Context, tag string) (ReleaseInfo, error)
}

// BlockingRun is a blocking run together with its evidence.
type BlockingRun struct {
	Run
	RunDetails
}

// RepairRequest is one dispatched repair round.
type RepairRequest struct {
	Repo      string
	Tag       string
	SHA       string
	Agent     string
	Round     int
	MaxRounds int
	Deadline  time.Time
	Blocking  []BlockingRun
}

// Dispatcher hands a repair round to an agent. An error means the round was
// NOT delivered: the sentinel does not count it and retries next pass.
type Dispatcher interface {
	DispatchRepair(ctx context.Context, req RepairRequest) error
}

// EscalationReason says why a release was handed to a human.
type EscalationReason string

const (
	// EscalationPolicy: the failure is an org/repo setting, permission or
	// secret - not code. Raised on the first round, nothing dispatched.
	EscalationPolicy EscalationReason = "policy"
	// EscalationRoundCap: MaxRounds rounds were spent without going green.
	EscalationRoundCap EscalationReason = "round_cap"
)

// Escalation is what a human is told.
type Escalation struct {
	Repo     string
	Tag      string
	SHA      string
	Reason   EscalationReason
	Detail   string
	Round    int
	Blocking []BlockingRun
}

// Escalator notifies a human. It must not fail the transition: the state is
// persisted as failed either way, so a lost notification is visible in the
// state file and the log rather than retried forever.
type Escalator interface {
	Escalate(ctx context.Context, e Escalation)
}

// RunRef is the persisted summary of a blocking run.
type RunRef struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Conclusion string `json:"conclusion"`
	URL        string `json:"url,omitempty"`
}

// Transition is one entry of a record's history.
type Transition struct {
	At     time.Time `json:"at"`
	From   State     `json:"from"`
	To     State     `json:"to"`
	SHA    string    `json:"sha"`
	Round  int       `json:"round"`
	Reason string    `json:"reason"`
}

// Record is the durable per-release state.
type Record struct {
	Tag   string `json:"tag"`
	Repo  string `json:"repo"`
	SHA   string `json:"sha"`
	State State  `json:"state"`
	// Round is how many repair rounds have been dispatched so far.
	Round int `json:"round"`
	// RoundSHA is the tag SHA the current round was dispatched against. A
	// failure on the same SHA while fixing is the SAME failure, not a new one.
	RoundSHA       string    `json:"round_sha,omitempty"`
	RoundStartedAt time.Time `json:"round_started_at,omitempty"`
	FirstSeen      time.Time `json:"first_seen"`
	UpdatedAt      time.Time `json:"updated_at"`
	// Escalated is set once a human has been told about this release.
	Escalated        bool             `json:"escalated,omitempty"`
	EscalationReason EscalationReason `json:"escalation_reason,omitempty"`
	Reason           string           `json:"reason,omitempty"`
	BlockingRuns     []RunRef         `json:"blocking_runs,omitempty"`
	History          []Transition     `json:"history,omitempty"`
}

func (r *Record) transition(now time.Time, to State, reason string) {
	if r.State != to {
		r.History = append(r.History, Transition{At: now, From: r.State, To: to, SHA: r.SHA, Round: r.Round, Reason: reason})
		if len(r.History) > maxHistory {
			r.History = r.History[len(r.History)-maxHistory:]
		}
	}
	r.State = to
	r.Reason = reason
	r.UpdatedAt = now
}

// Options configures a Sentinel. Zero values take the package defaults.
type Options struct {
	// Repo is "owner/name", recorded on every record and request.
	Repo string
	// Agent is the lane repair rounds are dispatched to.
	Agent string
	// MaxRounds caps repair rounds per release. <=0 means DefaultMaxRounds.
	MaxRounds int
	// RoundTimeout bounds one round. <=0 means DefaultRoundTimeout.
	RoundTimeout time.Duration
	// IgnoreWorkflows names workflows whose runs never block (advisory noise
	// such as greeting bots). Matched case-insensitively against the run name.
	IgnoreWorkflows []string
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

func (o Options) maxRounds() int {
	if o.MaxRounds <= 0 {
		return DefaultMaxRounds
	}
	return o.MaxRounds
}

func (o Options) roundTimeout() time.Duration {
	if o.RoundTimeout <= 0 {
		return DefaultRoundTimeout
	}
	return o.RoundTimeout
}

func (o Options) agent() string {
	if a := strings.TrimSpace(o.Agent); a != "" {
		return a
	}
	return DefaultAgent
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o Options) ignored(name string) bool {
	for _, w := range o.IgnoreWorkflows {
		if w = strings.TrimSpace(w); w != "" && strings.EqualFold(w, strings.TrimSpace(name)) {
			return true
		}
	}
	return false
}

// Sentinel evaluates one repository's current release.
type Sentinel struct {
	opts       Options
	source     Source
	store      Store
	dispatcher Dispatcher
	escalator  Escalator
}

// ErrMissingDependency is returned by Evaluate when the sentinel was built
// without a source, store, dispatcher or escalator.
var ErrMissingDependency = errors.New("releasesentinel: source, store, dispatcher and escalator are all required")

// New builds a Sentinel. Nothing happens until Evaluate is called, and the
// caller is responsible for only calling it when the operator opted in.
func New(opts Options, source Source, store Store, dispatcher Dispatcher, escalator Escalator) *Sentinel {
	return &Sentinel{opts: opts, source: source, store: store, dispatcher: dispatcher, escalator: escalator}
}

// Action is what one Evaluate pass did.
type Action string

const (
	ActionNone         Action = "none"          // no release tag, or nothing changed
	ActionWaiting      Action = "waiting"       // CI pending, release unpublished, or round in flight
	ActionRoundStarted Action = "round_started" // a repair round was dispatched
	ActionEscalated    Action = "escalated"     // handed to a human, state failed
	ActionGreen        Action = "green"         // release confirmed green
	ActionTerminal     Action = "terminal"      // record already terminal; nothing to do
)

// Result reports one Evaluate pass.
type Result struct {
	Action Action
	// Record is a copy of the current release's record after the pass (nil
	// when the repo has no release tag).
	Record *Record
	// Superseded lists older tags that were marked superseded this pass.
	Superseded []string
	// StaleRuns counts runs ignored because their head SHA is not the SHA the
	// tag points at now.
	StaleRuns int
}

// Evaluate runs one pass of the state machine for the current release tag
// and persists the result. It is safe to call repeatedly; every decision is
// derived from the forge's current state plus the persisted record.
func (s *Sentinel) Evaluate(ctx context.Context) (Result, error) {
	if s == nil || s.source == nil || s.store == nil || s.dispatcher == nil || s.escalator == nil {
		return Result{Action: ActionNone}, ErrMissingDependency
	}
	now := s.opts.now()
	tag, ok, err := s.source.CurrentTag(ctx)
	if err != nil {
		return Result{Action: ActionNone}, fmt.Errorf("resolve current release tag: %w", err)
	}
	if !ok || tag.Name == "" || tag.SHA == "" {
		return Result{Action: ActionNone}, nil
	}
	records, err := s.store.Load()
	if err != nil {
		return Result{Action: ActionNone}, fmt.Errorf("load release sentinel state: %w", err)
	}

	res := Result{Action: ActionNone}
	for name, r := range records {
		if name != tag.Name && !r.State.Terminal() {
			r.transition(now, StateSuperseded, "newer release tag "+tag.Name+" is current")
			res.Superseded = append(res.Superseded, name)
		}
	}
	sort.Strings(res.Superseded)

	rec, exists := records[tag.Name]
	if !exists {
		rec = &Record{Tag: tag.Name, Repo: s.opts.Repo, SHA: tag.SHA, State: StateAwaitingCI, FirstSeen: now, UpdatedAt: now}
		records[tag.Name] = rec
	}

	if err := s.step(ctx, now, tag, rec, &res); err != nil {
		// Persist whatever was decided before the failure (supersessions, a
		// tag move) so a restart does not redo it, then report the error.
		_ = s.store.Save(prune(records))
		return res, err
	}
	if err := s.store.Save(prune(records)); err != nil {
		return res, fmt.Errorf("save release sentinel state: %w", err)
	}
	cp := *rec
	res.Record = &cp
	return res, nil
}

// step advances one record. It mutates rec and res in place.
func (s *Sentinel) step(ctx context.Context, now time.Time, tag Tag, rec *Record, res *Result) error {
	if rec.State.Terminal() {
		res.Action = ActionTerminal
		return nil
	}
	if rec.SHA != tag.SHA {
		// The tag moved (a fix was pushed and the tag re-pointed). The old
		// SHA's runs are now stale; wait for the new SHA's CI.
		rec.SHA = tag.SHA
		rec.BlockingRuns = nil
		rec.transition(now, StateAwaitingCI, "tag moved to "+shortSHA(tag.SHA))
	}

	runs, err := s.source.Runs(ctx, rec.SHA)
	if err != nil {
		return fmt.Errorf("list workflow runs for %s: %w", shortSHA(rec.SHA), err)
	}
	var blocking []Run
	pending := 0
	current := 0
	for _, r := range runs {
		if r.HeadSHA != rec.SHA {
			res.StaleRuns++
			continue
		}
		if s.opts.ignored(r.Name) {
			continue
		}
		current++
		switch {
		case !IsCompleted(r.Status):
			pending++
		case IsBlockingConclusion(r.Conclusion):
			blocking = append(blocking, r)
		}
	}

	if len(blocking) == 0 && current > 0 && pending == 0 {
		rel, err := s.source.Release(ctx, rec.Tag)
		if err != nil {
			return fmt.Errorf("look up GitHub Release %s: %w", rec.Tag, err)
		}
		if rel.Exists && !rel.Draft {
			rec.BlockingRuns = nil
			rec.transition(now, StateGreen, fmt.Sprintf("all %d runs non-blocking and release published", current))
			res.Action = ActionGreen
			return nil
		}
		// CI is green but the release is not published yet: keep waiting. A
		// round in flight stays in flight, and its deadline still applies.
	}

	if rec.State == StateFixing && now.Sub(rec.RoundStartedAt) >= s.opts.roundTimeout() {
		// The round is spent whatever CI says now.
		if rec.Round >= s.opts.maxRounds() {
			s.escalate(ctx, now, rec, EscalationRoundCap,
				fmt.Sprintf("round %d/%d timed out without a green release", rec.Round, s.opts.maxRounds()), nil)
			res.Action = ActionEscalated
			return nil
		}
		rec.transition(now, StateAwaitingCI, fmt.Sprintf("round %d timed out", rec.Round))
	}

	if len(blocking) == 0 {
		rec.BlockingRuns = nil
		res.Action = ActionWaiting
		return nil
	}
	rec.BlockingRuns = refs(blocking)
	if rec.State == StateFixing && rec.RoundSHA == rec.SHA {
		// Same SHA the round was dispatched for: this is the failure the agent
		// is already working on, not a new one.
		res.Action = ActionWaiting
		return nil
	}
	return s.startRound(ctx, now, rec, blocking, res)
}

// startRound classifies the blocking runs and either escalates (policy
// failure or cap reached) or dispatches the next round.
func (s *Sentinel) startRound(ctx context.Context, now time.Time, rec *Record, blocking []Run, res *Result) error {
	detailed := make([]BlockingRun, 0, len(blocking))
	for _, r := range blocking {
		d, err := s.source.Details(ctx, r.ID)
		if err != nil {
			return fmt.Errorf("fetch evidence for run %d: %w", r.ID, err)
		}
		detailed = append(detailed, BlockingRun{Run: r, RunDetails: d})
	}

	if class, why := ClassifyFailures(detailed); class == ClassPolicy {
		s.escalate(ctx, now, rec, EscalationPolicy, why, detailed)
		res.Action = ActionEscalated
		return nil
	}
	if rec.Round >= s.opts.maxRounds() {
		s.escalate(ctx, now, rec, EscalationRoundCap,
			fmt.Sprintf("CI still failing after %d/%d repair rounds", rec.Round, s.opts.maxRounds()), detailed)
		res.Action = ActionEscalated
		return nil
	}

	req := RepairRequest{
		Repo:      rec.Repo,
		Tag:       rec.Tag,
		SHA:       rec.SHA,
		Agent:     s.opts.agent(),
		Round:     rec.Round + 1,
		MaxRounds: s.opts.maxRounds(),
		Deadline:  now.Add(s.opts.roundTimeout()),
		Blocking:  detailed,
	}
	if err := s.dispatcher.DispatchRepair(ctx, req); err != nil {
		// Not delivered, so not counted: the round cap measures rounds an
		// agent actually got, not kicks that bounced.
		return fmt.Errorf("dispatch repair round %d for %s: %w", req.Round, rec.Tag, err)
	}
	rec.Round = req.Round
	rec.RoundSHA = rec.SHA
	rec.RoundStartedAt = now
	rec.transition(now, StateFixing, fmt.Sprintf("round %d/%d dispatched to %s", req.Round, req.MaxRounds, req.Agent))
	res.Action = ActionRoundStarted
	return nil
}

func (s *Sentinel) escalate(ctx context.Context, now time.Time, rec *Record, reason EscalationReason, detail string, blocking []BlockingRun) {
	rec.Escalated = true
	rec.EscalationReason = reason
	rec.transition(now, StateFailed, string(reason)+": "+detail)
	s.escalator.Escalate(ctx, Escalation{
		Repo: rec.Repo, Tag: rec.Tag, SHA: rec.SHA, Reason: reason, Detail: detail, Round: rec.Round, Blocking: blocking,
	})
}

func refs(runs []Run) []RunRef {
	out := make([]RunRef, 0, len(runs))
	for _, r := range runs {
		out = append(out, RunRef{ID: r.ID, Name: r.Name, Conclusion: r.Conclusion, URL: r.URL})
	}
	return out
}

// prune bounds the state file: once more than maxRecords releases are
// remembered, the least recently updated terminal records are dropped.
func prune(records map[string]*Record) map[string]*Record {
	if len(records) <= maxRecords {
		return records
	}
	var terminal []string
	for name, r := range records {
		if r.State.Terminal() {
			terminal = append(terminal, name)
		}
	}
	sort.Slice(terminal, func(i, j int) bool {
		a, b := records[terminal[i]], records[terminal[j]]
		if !a.UpdatedAt.Equal(b.UpdatedAt) {
			return a.UpdatedAt.Before(b.UpdatedAt)
		}
		return terminal[i] < terminal[j]
	})
	for _, name := range terminal {
		if len(records) <= maxRecords {
			break
		}
		delete(records, name)
	}
	return records
}

// shortSHALen is how many hex digits of a SHA are shown in reasons and kicks.
const shortSHALen = 12

func shortSHA(sha string) string {
	if len(sha) > shortSHALen {
		return sha[:shortSHALen]
	}
	return sha
}
