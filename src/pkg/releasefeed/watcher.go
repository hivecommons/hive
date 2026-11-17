package releasefeed

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
)

// Release is one new upstream version waiting to be (or just) delivered.
type Release struct {
	// Source is the feed's Source.Key.
	Source  string `json:"source"`
	Package string `json:"package"`
	Old     string `json:"old"`
	New     string `json:"new"`
	URL     string `json:"url,omitempty"`
}

// Kicker delivers a release-feed kick. Returning an error wrapping ErrGated
// means a gate (agent paused, governor budget, repo pause) refused the kick:
// the releases stay pending and go out on a later poll once the gate opens.
// Any other error is a delivery failure and is retried the same way.
type Kicker interface {
	Kick(ctx context.Context, agent string, releases []Release) error
}

// ErrGated marks a kick a gate refused.
var ErrGated = errors.New("release-feed kick gated")

// Options wires a Watcher. Every func is read on each poll, so a config
// reload takes effect without restarting the watcher.
type Options struct {
	// Enabled reports whether the watcher should poll at all.
	Enabled func() bool
	// Subscriptions maps agent name to the feeds it subscribes to.
	Subscriptions func() map[string][]Source
	// Interval is the poll interval.
	Interval func() time.Duration
	// Cooldown is the minimum gap between two release-feed kicks of agent.
	Cooldown func(agent string) time.Duration
	// Token resolves a source's auth variable for agent; "" when unset.
	Token   func(agent, varName string) string
	Fetcher *Fetcher
	Store   Store
	Kicker  Kicker
	Logger  *slog.Logger
	Now     func() time.Time
}

// PollResult summarizes one poll, for logs and tests.
type PollResult struct {
	Fetched     int
	NotModified int
	Errors      int
	Detected    int
	Kicked      []string
}

// Watcher polls feeds and kicks subscribed agents on new versions.
type Watcher struct {
	opts Options

	mu     sync.Mutex
	state  *State
	loaded bool
}

// New returns a Watcher.
func New(opts Options) *Watcher {
	if opts.Fetcher == nil {
		opts.Fetcher = &Fetcher{Client: NewEgressClient()}
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Watcher{opts: opts}
}

// fallbackPollInterval is Run's spacing when Options.Interval is unset or
// returns a non-positive value.
const fallbackPollInterval = time.Minute

// Run polls at once and then every Interval until ctx ends.
func (w *Watcher) Run(ctx context.Context) {
	for {
		w.Poll(ctx)
		d := fallbackPollInterval
		if w.opts.Interval != nil {
			if v := w.opts.Interval(); v > 0 {
				d = v
			}
		}
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// Block renders the ${NEW_RELEASES} block for agent from its pending
// releases; "" when there are none.
func (w *Watcher) Block(agent string) string {
	if w == nil {
		return ""
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.state == nil {
		return ""
	}
	a := w.state.Agents[agent]
	if a == nil {
		return ""
	}
	return FormatBlock(a.Pending)
}

// BlockHeader opens every ${NEW_RELEASES} block.
const BlockHeader = "NEW UPSTREAM RELEASES (release-feed watcher):"

// FormatBlock renders releases as the ${NEW_RELEASES} block.
func FormatBlock(releases []Release) string {
	if len(releases) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(BlockHeader)
	b.WriteByte('\n')
	for _, r := range releases {
		fmt.Fprintf(&b, "- %s: %s -> %s", r.Package, r.Old, r.New)
		if r.URL != "" {
			fmt.Fprintf(&b, " (%s)", r.URL)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// maxReasonReleases is how many releases a kick-history reason names before
// it summarizes the rest as a count.
const maxReasonReleases = 3

// Reason is the kick-history reason for a release-feed kick.
func Reason(releases []Release) string {
	parts := make([]string, 0, min(len(releases), maxReasonReleases))
	for i, r := range releases {
		if i == maxReasonReleases {
			break
		}
		parts = append(parts, fmt.Sprintf("%s %s -> %s", r.Package, r.Old, r.New))
	}
	reason := "new release: " + strings.Join(parts, ", ")
	if extra := len(releases) - maxReasonReleases; extra > 0 {
		reason += fmt.Sprintf(" (+%d more)", extra)
	}
	return reason
}

type fetchJob struct {
	src   Source
	agent string
}

// Poll runs one pass: fetch every subscribed feed once, record new versions
// as pending for each subscriber, and kick every agent whose cooldown has
// passed.
func (w *Watcher) Poll(ctx context.Context) PollResult {
	var res PollResult
	if w.opts.Enabled != nil && !w.opts.Enabled() {
		return res
	}
	subs := map[string][]Source{}
	if w.opts.Subscriptions != nil {
		subs = w.opts.Subscriptions()
	}
	w.ensureLoaded()

	agents := make([]string, 0, len(subs))
	for a := range subs {
		agents = append(agents, a)
	}
	sort.Strings(agents)
	jobs := map[string]fetchJob{}
	for _, a := range agents {
		for _, src := range subs[a] {
			if _, ok := jobs[src.Key()]; !ok {
				jobs[src.Key()] = fetchJob{src: src, agent: a}
			}
		}
	}
	keys := make([]string, 0, len(jobs))
	for k := range jobs {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, key := range keys {
		if ctx.Err() != nil {
			return res
		}
		job := jobs[key]
		token := ""
		if job.src.AuthVar != "" {
			if w.opts.Token != nil {
				token = w.opts.Token(job.agent, job.src.AuthVar)
			}
			if token == "" {
				res.Errors++
				w.opts.Logger.Warn("release feed: auth variable is not set; feed skipped",
					"feed", key, "agent", job.agent, "auth_var", job.src.AuthVar)
				continue
			}
		}
		w.mu.Lock()
		prev := w.state.Sources[key]
		w.mu.Unlock()
		got, err := w.opts.Fetcher.Fetch(ctx, job.src, prev, token)
		if err != nil {
			res.Errors++
			w.opts.Logger.Warn("release feed poll failed", "feed", key, "error", err)
			continue
		}
		next := SourceState{ETag: got.ETag, LastModified: got.LastModified, Version: prev.Version, URL: prev.URL}
		if got.NotModified {
			res.NotModified++
		} else {
			res.Fetched++
			next.Version, next.URL = got.Version, got.URL
		}
		w.mu.Lock()
		w.state.Sources[key] = next
		w.mu.Unlock()
	}

	now := w.opts.Now()
	due, detected := w.diff(subs, now)
	res.Detected = detected
	for _, d := range due {
		if ctx.Err() != nil || w.opts.Kicker == nil {
			break
		}
		err := w.opts.Kicker.Kick(ctx, d.agent, d.releases)
		if err != nil {
			if errors.Is(err, ErrGated) {
				w.opts.Logger.Info("release feed kick held", "agent", d.agent, "reason", err.Error(), "pending", len(d.releases))
			} else {
				w.opts.Logger.Warn("release feed kick failed", "agent", d.agent, "error", err, "pending", len(d.releases))
			}
			continue
		}
		w.delivered(d.agent, d.releases, now)
		res.Kicked = append(res.Kicked, d.agent)
		w.opts.Logger.Info("release feed kicked agent", "agent", d.agent, "reason", Reason(d.releases))
	}
	w.save()
	return res
}

type dueKick struct {
	agent    string
	releases []Release
}

// diff records new versions as pending and returns the agents due a kick.
func (w *Watcher) diff(subs map[string][]Source, now time.Time) ([]dueKick, int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	detected := 0
	live := map[string]bool{}
	for agent := range w.state.Agents {
		if _, ok := subs[agent]; !ok {
			delete(w.state.Agents, agent)
		}
	}
	agents := make([]string, 0, len(subs))
	for agent, srcs := range subs {
		agents = append(agents, agent)
		a := w.state.agent(agent)
		subscribed := map[string]bool{}
		for _, src := range srcs {
			key := src.Key()
			live[key] = true
			subscribed[key] = true
			cur := w.state.Sources[key]
			if cur.Version == "" {
				continue
			}
			seen := a.Seen[key]
			if seen == "" {
				// First answer: a baseline, not a release.
				a.Seen[key] = cur.Version
				continue
			}
			if seen == cur.Version {
				continue
			}
			addPending(a, Release{Source: key, Package: src.Label(), Old: seen, New: cur.Version, URL: cur.URL})
			a.Seen[key] = cur.Version
			detected++
		}
		for key := range a.Seen {
			if !subscribed[key] {
				delete(a.Seen, key)
			}
		}
		kept := a.Pending[:0]
		for _, r := range a.Pending {
			if subscribed[r.Source] {
				kept = append(kept, r)
			}
		}
		a.Pending = kept
	}
	for key := range w.state.Sources {
		if !live[key] {
			delete(w.state.Sources, key)
		}
	}
	sort.Strings(agents)
	var due []dueKick
	for _, agent := range agents {
		a := w.state.Agents[agent]
		if len(a.Pending) == 0 {
			continue
		}
		cooldown := time.Duration(0)
		if w.opts.Cooldown != nil {
			cooldown = w.opts.Cooldown(agent)
		}
		if !a.LastKick.IsZero() && now.Sub(a.LastKick) < cooldown {
			continue
		}
		due = append(due, dueKick{agent: agent, releases: append([]Release(nil), a.Pending...)})
	}
	return due, detected
}

// addPending folds r into the agent's pending list: a feed that moved again
// before its kick keeps the original old version and takes the newest one.
func addPending(a *AgentState, r Release) {
	for i := range a.Pending {
		if a.Pending[i].Source != r.Source {
			continue
		}
		a.Pending[i].New, a.Pending[i].URL, a.Pending[i].Package = r.New, r.URL, r.Package
		if a.Pending[i].Old == a.Pending[i].New {
			a.Pending = append(a.Pending[:i], a.Pending[i+1:]...)
		}
		return
	}
	a.Pending = append(a.Pending, r)
}

// delivered drops the releases a kick carried and starts the cooldown. A
// release that moved again while the kick was in flight stays pending.
func (w *Watcher) delivered(agent string, sent []Release, now time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	a := w.state.Agents[agent]
	if a == nil {
		return
	}
	done := map[string]string{}
	for _, r := range sent {
		done[r.Source] = r.New
	}
	kept := a.Pending[:0]
	for _, r := range a.Pending {
		if v, ok := done[r.Source]; ok && v == r.New {
			continue
		}
		if v, ok := done[r.Source]; ok {
			r.Old = v
		}
		kept = append(kept, r)
	}
	a.Pending = kept
	a.LastKick = now
}

func (w *Watcher) ensureLoaded() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.loaded {
		return
	}
	w.loaded = true
	w.state = newState()
	if w.opts.Store == nil {
		return
	}
	st, err := w.opts.Store.Load()
	if err != nil {
		w.opts.Logger.Warn("release feed state unreadable; starting fresh (first answers are baselines)", "error", err)
		return
	}
	w.state = st
}

func (w *Watcher) save() {
	if w.opts.Store == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.opts.Store.Save(w.state); err != nil {
		w.opts.Logger.Warn("release feed state not saved", "error", err)
	}
}
