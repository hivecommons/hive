// Package research is the research agent pool the meetings feature kicks
// (#18288, parent #18284; Quorum contract section 3).
//
// A research request from a meeting becomes a queued kick to the least busy
// of N read-only research agents (research-1..N). The agent reads the web,
// prints "[FINDING] <one line>" per result and "[COMPLETE] <summary>" when
// done; those output signals come back through the runtime's kick observers
// tagged with the kick ID, and Pool.Observe turns them into Events.
//
// The agents are ADVISORY with no repo checkouts: the hive proxy refuses
// every GitHub write (anything but GET/HEAD/OPTIONS and git fetch) for an
// ADVISORY agent and its token tier is read-only, so "no repo writes, no
// GitHub mutations" holds even if the prompt is ignored.
package research

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/pkg/config"
)

const (
	// Role is the research agents' role.
	Role = "research"
	// NamePrefix is the research agents' name prefix: research-1..N.
	NamePrefix = "research-"
	// DefaultPoolSize is the pool size when meetings.research_pool_size is
	// unset or not positive.
	DefaultPoolSize = 2
	// MaxPoolSize caps meetings.research_pool_size.
	MaxPoolSize = 8
	// Mode is the research agents' GitHub interaction tier: read-only.
	Mode = "ADVISORY"

	// maxQueryLen bounds a research query typed into an agent, in bytes.
	maxQueryLen = 2000
	// maxTrackedKicks bounds the kicks Observe can attribute at once; the
	// oldest is forgotten first.
	maxTrackedKicks = 256
	// kickIDPrefix and kickIDRandomBytes shape the pool's kick IDs:
	// "rsch-" + 16 hex characters.
	kickIDPrefix      = "rsch-"
	kickIDRandomBytes = 8

	// coldStartSettle is how long a just-started research agent's CLI is
	// given to boot before a kick is typed into it: a kick delivered to a
	// pane still launching sees no CLI marker, the runtime restarts the
	// agent as crashed, and the kick fails with "CLI did not reach input
	// prompt" (live Discord test).
	coldStartSettle = 20 * time.Second
	// coldStartWait bounds the whole wait for a cold agent to settle.
	coldStartWait = 3 * time.Minute
	// readyPoll is how often a cold agent's status is read while waiting.
	readyPoll = time.Second
	// maxPromptRetries is how many times a kick that failed because the
	// agent's CLI never reached its input prompt is queued again, after
	// the agent settles, before the failure is reported.
	maxPromptRetries = 2
	// promptNotReady is the runtime's failure text for that case.
	promptNotReady = "did not reach input prompt"

	displayName = "research"
	emoji       = "🔎"
	description = "Read-only research agent for live meetings. Reads public web sources and reports findings; never writes to a repository or GitHub."
)

// Event kinds Observe delivers. They map onto the contract's research_update
// statuses: queued, running, done, failed; finding carries one interim
// result.
const (
	EventQueued  = "queued"
	EventRunning = "running"
	EventFinding = "finding"
	EventDone    = "done"
	EventFailed  = "failed"
)

// ErrUnsupportedRuntime means the runtime does not offer queued kicks and
// fan-out observers (the sidecar runtime today).
var ErrUnsupportedRuntime = errors.New("research pool: runtime does not support queued kicks")

// ErrEmptyQuery means Dispatch was given no query.
var ErrEmptyQuery = errors.New("research pool: empty query")

// ErrNoAgents means every research agent is unavailable. Its Reason is
// the sayable cause a meeting is told.
var ErrNoAgents error = &reasonError{
	msg:    "research pool: no research agent available",
	reason: "no research agent is available on this hive",
}

// reasonError is an error with a short reason fit to say in a meeting.
type reasonError struct{ msg, reason string }

func (e *reasonError) Error() string { return e.msg }

// Reason is the sayable cause.
func (e *reasonError) Reason() string { return e.reason }

// Runtime is what the pool needs from the agent runtime. *agent.Manager
// satisfies it; NewPool type-asserts a crux.Runtime to it.
type Runtime interface {
	crux.KickQueuer
	crux.KickObserverRegistry
	AddAgent(name string, cfg config.AgentConfig)
	GetStatusFast(name string) (*crux.Process, error)
	Start(ctx context.Context, name string) error
}

// Event is one research progress update for a kick the pool dispatched.
type Event struct {
	// KickID is what Dispatch returned.
	KickID string
	// Agent is the research agent running it.
	Agent string
	// Kind is one of the Event* constants.
	Kind string
	// Text is the finding line (EventFinding), the summary (EventDone) or
	// the reason (EventFailed). It may be empty for EventDone when the
	// agent's turn ended without a [COMPLETE] line.
	Text string
	// Meta is the map passed to Dispatch.
	Meta map[string]string
}

// PoolSize resolves meetings.research_pool_size: DefaultPoolSize when not
// positive, MaxPoolSize at most.
func PoolSize(configured int) int {
	switch {
	case configured <= 0:
		return DefaultPoolSize
	case configured > MaxPoolSize:
		return MaxPoolSize
	default:
		return configured
	}
}

// AgentNames returns research-1..n.
func AgentNames(n int) []string {
	names := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		names = append(names, NamePrefix+strconv.Itoa(i))
	}
	return names
}

// IsResearchAgent reports whether name is a research pool agent.
func IsResearchAgent(name string) bool {
	n, err := strconv.Atoi(strings.TrimPrefix(name, NamePrefix))
	return strings.HasPrefix(name, NamePrefix) && err == nil && n > 0
}

// AgentConfig is the research agent definition. base supplies the backend
// and model (and anything else the caller wants kept); the read-only fields
// are always forced, so a base cannot widen the agent's access.
func AgentConfig(base config.AgentConfig) config.AgentConfig {
	cfg := base
	noRepos := false
	cfg.Role = Role
	cfg.Mode = Mode
	cfg.IncludeRepos = &noRepos
	cfg.OnDemand = true
	cfg.Converse = nil
	if cfg.DisplayName == "" {
		cfg.DisplayName = displayName
	}
	if cfg.Emoji == "" {
		cfg.Emoji = emoji
	}
	if cfg.Description == "" {
		cfg.Description = description
	}
	return cfg
}

// Prompt is the kick a research agent receives for one query. The markers
// are spelled out in words rather than written literally: the runtime only
// treats a marker that starts a line as a signal, and a pane that wraps an
// echoed prompt could otherwise start a line with one.
func Prompt(query string) string {
	q := strings.TrimSpace(query)
	if len(q) > maxQueryLen {
		q = strings.ToValidUTF8(q[:maxQueryLen], "")
	}
	return strings.Join([]string{
		"You are a research assistant for a live meeting. Research the question below using public web sources only.",
		"Rules: read web pages and documentation; do not clone, edit or push to any repository; do not create, comment on, label or close GitHub issues or pull requests; do not run anything that changes a remote system.",
		"Report as you go. For each useful result print one line that starts with the word FINDING in square brackets, then one sentence and its source URL.",
		"When you are done print one line that starts with the word COMPLETE in square brackets, then a summary of at most three sentences.",
		"If you cannot answer, print one line that starts with the word ERROR in square brackets, then the reason.",
		"Keep it short: the meeting is waiting.",
		"",
		"Question: " + q,
	}, "\n")
}

// Pool dispatches research queries to the research agents.
type Pool struct {
	rt    Runtime
	names []string

	// base is what Ensure registered the agents with; Dispatch re-registers
	// with it when every agent has gone missing from the runtime.
	baseMu  sync.Mutex
	base    config.AgentConfig
	ensured bool

	// randRead fills kick ID randomness; crypto/rand.Read unless a test
	// swaps it.
	randRead func([]byte) (int, error)
	// async runs a delayed enqueue or warm start (a goroutine unless a
	// test swaps it); wait sleeps d unless ctx ends first; now is the
	// clock.
	async func(func())
	wait  func(ctx context.Context, d time.Duration) bool
	now   func() time.Time

	mu      sync.Mutex
	tracked map[string]*trackedKick
	order   []string
	// settling counts, per agent, the kicks waiting for a cold agent to
	// boot before they are queued: the runtime does not know them yet, so
	// pick adds them to the agent's load. Without it two questions asked
	// close together both landed on research-1 while research-2 sat idle.
	settling map[string]int
	// observers are Observe's callbacks, for failures the pool itself
	// raises.
	observers []func(Event)
}

type trackedKick struct {
	// id is the kick ID Dispatch returned; a retry is queued under a new
	// runtime ID that maps back to it.
	id     string
	agent  string
	meta   map[string]string
	prompt string
	// retries counts re-queues after a prompt-not-ready failure.
	retries int
	done    bool
}

// NewPool wraps rt (normally the hive's crux.Runtime) for a pool of size
// agents. It returns ErrUnsupportedRuntime when rt lacks queued kicks.
func NewPool(rt any, size int) (*Pool, error) {
	r, ok := rt.(Runtime)
	if !ok {
		return nil, ErrUnsupportedRuntime
	}
	return &Pool{
		rt: r, names: AgentNames(PoolSize(size)), randRead: rand.Read, tracked: make(map[string]*trackedKick),
		settling: make(map[string]int),
		async:    func(f func()) { go f() }, wait: sleepCtx, now: time.Now,
	}, nil
}

// sleepCtx sleeps d, reporting false when ctx ends first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// settled reports whether agent's CLI has had time to boot: running, not
// starting, and started at least coldStartSettle ago.
func (p *Pool) settled(agent string) bool {
	st, err := p.rt.GetStatusFast(agent)
	if err != nil || st == nil || st.State != crux.StateRunning || st.Starting {
		return false
	}
	return st.StartedAt == nil || p.now().Sub(*st.StartedAt) >= coldStartSettle
}

// awaitSettled waits, bounded by coldStartWait, for agent to settle.
func (p *Pool) awaitSettled(ctx context.Context, agent string) bool {
	deadline := p.now().Add(coldStartWait)
	for !p.settled(agent) {
		if p.now().After(deadline) || !p.wait(ctx, readyPoll) {
			return false
		}
	}
	return true
}

// Warm starts every stopped research agent so the first research request
// of a live meeting finds a booted CLI instead of a cold start. A meeting
// calls it when it starts and again on its research tick, so an agent
// stopped mid-meeting comes back. Paused or operator-stopped agents are
// left alone. It returns at once; the starts run in the background.
func (p *Pool) Warm(ctx context.Context) {
	for _, name := range p.names {
		st, err := p.rt.GetStatusFast(name)
		if err != nil || st == nil || st.Paused || st.OperatorStopped || st.Starting ||
			st.State == crux.StateRunning || st.State == crux.StatePaused {
			continue
		}
		p.async(func() {
			if err := p.rt.Start(context.WithoutCancel(ctx), name); err != nil {
				return
			}
			p.awaitSettled(context.WithoutCancel(ctx), name)
		})
	}
}

// Agents returns the pool's agent names.
func (p *Pool) Agents() []string { return append([]string(nil), p.names...) }

// Ensure registers the research agents with the runtime (AddAgent is a no-op
// for one that already exists). Call it only when meetings are enabled: the
// pool's agents exist for nothing else.
func (p *Pool) Ensure(base config.AgentConfig) {
	p.baseMu.Lock()
	p.base, p.ensured = base, true
	p.baseMu.Unlock()
	cfg := AgentConfig(base)
	for _, name := range p.names {
		p.rt.AddAgent(name, cfg)
	}
}

// Dispatch queues query on the least busy research agent and returns the
// kick ID. A stopped agent is started first. meta rides along on every Event
// for this kick (meeting ID, research ID, ...).
func (p *Pool) Dispatch(ctx context.Context, query string, meta map[string]string) (string, error) {
	if strings.TrimSpace(query) == "" {
		return "", ErrEmptyQuery
	}
	agent, err := p.pick()
	if errors.Is(err, ErrNoAgents) && p.reEnsure() {
		// The agents are runtime-only (not in hive.yaml), so anything that
		// rebuilt the runtime's agent table from the config dropped them:
		// register them again and retry once.
		agent, err = p.pick()
	}
	if err != nil {
		return "", err
	}
	if st, err := p.rt.GetStatusFast(agent); err == nil && st != nil && st.State != crux.StateRunning {
		if err := p.rt.Start(ctx, agent); err != nil {
			return "", fmt.Errorf("research pool: start %s: %w", agent, err)
		}
	}
	// Track under our own ID BEFORE enqueueing: the runtime emits
	// kick-queued from inside EnqueueKick, and an untracked ID would drop it.
	id, err := p.newID()
	if err != nil {
		return "", err
	}
	prompt := Prompt(query)
	p.track(id, agent, meta, prompt)
	if !p.settled(agent) {
		// A cold (just started) agent: queue once its CLI has booted, off
		// the caller's goroutine, so the kick is not typed into a pane
		// still launching. A failure is reported as the kick's failed
		// event, like any delivery failure.
		p.addSettling(agent, 1)
		p.async(func() {
			defer p.addSettling(agent, -1)
			p.enqueueWhenSettled(context.WithoutCancel(ctx), id, agent, id, prompt)
		})
		return id, nil
	}
	if _, err := p.rt.EnqueueKick(agent, id, prompt); err != nil {
		p.untrack(id)
		return "", fmt.Errorf("research pool: queue on %s: %w", agent, err)
	}
	return id, nil
}

// enqueueWhenSettled waits for agent to settle and queues prompt under
// runtimeID (which maps to the kick id); when it cannot, the kick fails
// through the observers.
func (p *Pool) enqueueWhenSettled(ctx context.Context, id, agent, runtimeID, prompt string) {
	if !p.awaitSettled(ctx, agent) {
		p.fail(id, "research agent "+agent+" did not finish starting")
		return
	}
	if _, err := p.rt.EnqueueKick(agent, runtimeID, prompt); err != nil {
		p.fail(id, "could not queue on "+agent+": "+err.Error())
	}
}

// fail reports a pool-side failure (a delayed enqueue that could not run)
// as the kick's failed event.
func (p *Pool) fail(id, reason string) {
	p.mu.Lock()
	tk, ok := p.tracked[id]
	if !ok || tk.done {
		p.mu.Unlock()
		return
	}
	tk.done = true
	out := Event{KickID: tk.id, Agent: tk.agent, Meta: tk.meta, Kind: EventFailed, Text: reason}
	obs := slices.Clone(p.observers)
	p.mu.Unlock()
	for _, fn := range obs {
		fn(out)
	}
}

// reEnsure registers the agents again with the base Ensure was given. It
// reports false when Ensure never ran.
func (p *Pool) reEnsure() bool {
	p.baseMu.Lock()
	base, ok := p.base, p.ensured
	p.baseMu.Unlock()
	if ok {
		p.Ensure(base)
	}
	return ok
}

// Status describes the pool for the settings page: how many agents it has
// and how many are registered and not paused right now.
type Status struct {
	Agents    []string `json:"agents"`
	Available int      `json:"available"`
}

// Status reports the pool's agents and how many can take a kick now.
func (p *Pool) Status() Status {
	st := Status{Agents: p.Agents()}
	for _, name := range p.names {
		if p.available(name) {
			st.Available++
		}
	}
	return st
}

// available reports whether name is registered and not paused.
func (p *Pool) available(name string) bool {
	st, err := p.rt.GetStatusFast(name)
	return err == nil && st != nil && !st.Paused && st.State != crux.StatePaused
}

// newID returns a fresh kick ID for a research kick.
func (p *Pool) newID() (string, error) {
	b := make([]byte, kickIDRandomBytes)
	if _, err := p.randRead(b); err != nil {
		return "", fmt.Errorf("research pool: kick id: %w", err)
	}
	return kickIDPrefix + hex.EncodeToString(b), nil
}

func (p *Pool) untrack(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.tracked, id)
	for i, v := range p.order {
		if v == id {
			p.order = append(p.order[:i], p.order[i+1:]...)
			break
		}
	}
}

// addSettling adjusts the count of kicks waiting for agent to boot.
func (p *Pool) addSettling(agent string, n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.settling[agent] += n
	if p.settling[agent] <= 0 {
		delete(p.settling, agent)
	}
}

// load is the work agent already has: kicks waiting in the runtime's
// queue, the one it is running, and those waiting for it to boot.
func (p *Pool) load(agent string) int {
	n := p.rt.KickQueueDepth(agent)
	if p.rt.InFlightKickID(agent) != "" {
		n++
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return n + p.settling[agent]
}

// Busy reports that every available research agent already has work: a
// query dispatched now is queued behind another one. A pool with no
// available agent is not busy; Dispatch fails there with ErrNoAgents.
func (p *Pool) Busy() bool {
	found := false
	for _, name := range p.names {
		if !p.available(name) {
			continue
		}
		if p.load(name) == 0 {
			return false
		}
		found = true
	}
	return found
}

// pick returns the available agent with the fewest waiting plus running
// kicks; ties go to the lowest-numbered agent.
func (p *Pool) pick() (string, error) {
	best, bestLoad := "", 0
	for _, name := range p.names {
		if !p.available(name) {
			continue
		}
		load := p.load(name)
		if best == "" || load < bestLoad {
			best, bestLoad = name, load
		}
	}
	if best == "" {
		return "", ErrNoAgents
	}
	return best, nil
}

func (p *Pool) track(id, agent string, meta map[string]string, prompt string) {
	cp := make(map[string]string, len(meta))
	for k, v := range meta {
		cp[k] = v
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.trackLocked(id, &trackedKick{id: id, agent: agent, meta: cp, prompt: prompt})
}

// trackLocked records tk under the runtime ID runtimeID.
func (p *Pool) trackLocked(runtimeID string, tk *trackedKick) {
	p.tracked[runtimeID] = tk
	p.order = append(p.order, runtimeID)
	for len(p.order) > maxTrackedKicks {
		delete(p.tracked, p.order[0])
		p.order = p.order[1:]
	}
}

// Observe registers fn for progress on every kick this pool dispatched and
// returns its remover. Each kick yields queued, running, any number of
// finding, then exactly one of done or failed.
func (p *Pool) Observe(fn func(Event)) (remove func()) {
	p.mu.Lock()
	p.observers = append(p.observers, fn)
	idx := len(p.observers) - 1
	p.mu.Unlock()
	rm := p.rt.AddKickObserver(func(ev crux.KickEvent) {
		out, ok, retry := p.translate(ev)
		if retry != nil {
			p.async(retry)
		}
		if ok {
			fn(out)
		}
	})
	return func() {
		rm()
		p.mu.Lock()
		if idx < len(p.observers) {
			p.observers[idx] = func(Event) {}
		}
		p.mu.Unlock()
	}
}

// translate maps a runtime kick event for a tracked kick to a pool Event.
// A failure because the agent's CLI never reached its input prompt (a
// cold start the runtime restarted) is not reported while retries remain:
// it returns a retry to run off the observer instead, which queues the
// kick again once the agent settles.
func (p *Pool) translate(ev crux.KickEvent) (Event, bool, func()) {
	if ev.KickID == "" {
		return Event{}, false, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	tk, ok := p.tracked[ev.KickID]
	if !ok || tk.done {
		return Event{}, false, nil
	}
	if ev.Event == crux.KickEventFailed && strings.Contains(ev.Detail, promptNotReady) && tk.retries < maxPromptRetries && tk.prompt != "" {
		if retry := p.retryLocked(tk); retry != nil {
			return Event{}, false, retry
		}
	}
	out := Event{KickID: tk.id, Agent: tk.agent, Meta: tk.meta}
	switch ev.Event {
	case crux.KickEventQueued:
		out.Kind = EventQueued
	case crux.KickEventDelivered:
		out.Kind = EventRunning
	case crux.KickEventOutputSignal:
		switch ev.Signal {
		case crux.OutputSignalFinding:
			out.Kind, out.Text = EventFinding, ev.Text
		case crux.OutputSignalComplete:
			out.Kind, out.Text = EventDone, ev.Text
		case crux.OutputSignalError:
			out.Kind, out.Text = EventFailed, ev.Text
		default:
			return Event{}, false, nil
		}
	case crux.KickEventCompleted:
		out.Kind = EventDone
	case crux.KickEventFailed:
		out.Kind, out.Text = EventFailed, ev.Detail
	default:
		return Event{}, false, nil
	}
	if out.Kind == EventDone || out.Kind == EventFailed {
		tk.done = true
	}
	if out.Kind == EventQueued && tk.retries > 0 {
		// The kick was queued already; its retry's queued event is noise.
		return Event{}, false, nil
	}
	return out, true, nil
}

// retryLocked tracks a fresh runtime ID for tk and returns the delayed
// re-queue, or nil when no ID can be made.
func (p *Pool) retryLocked(tk *trackedKick) func() {
	b := make([]byte, kickIDRandomBytes)
	if _, err := p.randRead(b); err != nil {
		return nil
	}
	runtimeID := kickIDPrefix + hex.EncodeToString(b)
	tk.retries++
	p.trackLocked(runtimeID, tk)
	return func() {
		p.enqueueWhenSettled(context.Background(), runtimeID, tk.agent, runtimeID, tk.prompt)
	}
}
