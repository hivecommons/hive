package github

import (
	"context"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"
)

const (
	terminalEnrichmentInterval = 10 * time.Second
	terminalEnrichmentBudget   = 10 * time.Second
	terminalEnrichmentBatch    = 8
)

type terminalEnrichmentTask struct {
	owner, repoName string
	observation     PRTerminalObservation
	updatedAt       time.Time
	identity        HiveIdentity
	mergedBy        string
	sequence        uint64
	revision        uint64
}

func (t terminalEnrichmentTask) key() string {
	return strings.ToLower(t.owner+"/"+t.repoName) + "#" + strconv.Itoa(t.observation.Number)
}

func (t terminalEnrichmentTask) sameObservation(other terminalEnrichmentTask) bool {
	a, b := t.observation, other.observation
	return t.updatedAt.Equal(other.updatedAt) && t.identity == other.identity && t.mergedBy == other.mergedBy &&
		a.Repo == b.Repo && a.Number == b.Number && a.State == b.State && a.ObservedAt.Equal(b.ObservedAt) &&
		a.Actor == b.Actor && a.Attribution == b.Attribution && a.CreatedAt.Equal(b.CreatedAt) &&
		a.URL == b.URL && slices.Equal(a.Issues, b.Issues)
}

// Only observational accounting is deferred. Claim listings, churn history,
// ledger persistence and fail-closed dispatch filtering never read this queue.
// Pending tasks survive failed batches and coalesce by PR, not by scan. On a
// process restart the existing terminal listing replays the settle window;
// timeline sinks must therefore remain idempotent and persist first sightings.
type terminalEnrichmentState struct {
	mu       sync.Mutex
	started  bool
	cancel   context.CancelFunc
	done     chan struct{}
	wake     chan struct{}
	pending  map[string]terminalEnrichmentTask
	sequence uint64
	retries  retryTracker
}

// StartPRTerminalEnrichment isolates historical accounting from current-work
// evaluation. It starts at most one worker per client, using the same lifecycle
// context as the client's request watchers. Unstarted clients retain the
// synchronous observation API (including standalone callers and tests).
func (c *Client) StartPRTerminalEnrichment(ctx context.Context) <-chan struct{} {
	if c == nil || ctx == nil {
		done := make(chan struct{})
		close(done)
		return done
	}
	s := &c.prTerminalEnrichment
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return s.done
	}
	s.started = true
	ctx, s.cancel = context.WithCancel(ctx)
	s.done = make(chan struct{})
	s.wake = make(chan struct{}, 1)
	s.pending = make(map[string]terminalEnrichmentTask)
	go func() {
		defer close(s.done)
		ticker := time.NewTicker(terminalEnrichmentInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			case <-s.wake:
			}
			if ctx.Err() != nil {
				return
			}
			c.enrichTerminalPRBatch(ctx, terminalEnrichmentBudget, terminalEnrichmentBatch)
		}
	}()
	return s.done
}

// StopPRTerminalEnrichment retires a replaced client's worker without waiting
// on an uninterruptible accounting sink. The replacement's settle scan replays
// recent pending history, just as on process restart.
func (c *Client) StopPRTerminalEnrichment() {
	if c == nil {
		return
	}
	s := &c.prTerminalEnrichment
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		s.cancel()
	}
}

func (c *Client) observeTerminalPR(ctx context.Context, task terminalEnrichmentTask) {
	s := &c.prTerminalEnrichment
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		c.enrichTerminalPR(ctx, task)
		return
	}
	key := task.key()
	old, exists := s.pending[key]
	if exists && task.sameObservation(old) {
		s.mu.Unlock()
		return
	}
	if !exists || !task.updatedAt.Before(old.updatedAt) {
		s.sequence++
		task.revision = s.sequence
		if exists {
			task.sequence = old.sequence
		} else {
			task.sequence = s.sequence
		}
		s.pending[key] = task
	}
	// Wake only on an empty-to-nonempty transition. A listing must not
	// repeatedly wake a failed/backed-off batch or bypass its bounded cadence.
	if !exists && len(s.pending) == 1 {
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
	s.mu.Unlock()
}

func (c *Client) enrichTerminalPR(ctx context.Context, task terminalEnrichmentTask) bool {
	if ctx.Err() != nil {
		return false
	}
	obs := task.observation
	if obs.State == PRStateMerged {
		pr := &gh.PullRequest{Number: gh.Ptr(obs.Number), MergedBy: &gh.User{Login: gh.Ptr(task.mergedBy)}}
		obs.Actor = c.prTerminalMergedBy(ctx, task.owner, task.repoName, pr)
		obs.Attribution = prTerminalMergeAttribution(obs.Actor, task.identity, c.appBotLogin)
	}
	if ctx.Err() != nil {
		return false
	}
	c.emitPRTerminalObserved(obs)
	return c.emitTerminalPRReviews(ctx, task.owner, task.repoName, obs.Repo, obs.Number, task.updatedAt, task.identity)
}

func (c *Client) enrichTerminalPRBatch(ctx context.Context, budget time.Duration, limit int) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	s := &c.prTerminalEnrichment
	s.mu.Lock()
	tasks := make([]terminalEnrichmentTask, 0, len(s.pending))
	for _, task := range s.pending {
		tasks = append(tasks, task)
	}
	s.mu.Unlock()
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].sequence < tasks[j].sequence })
	processed, failed := 0, 0
	for _, task := range tasks {
		key := task.key()
		if ctx.Err() != nil || processed+failed >= limit {
			break
		}
		if !s.retries.allows(key, time.Now()) {
			continue
		}
		ok := c.enrichTerminalPR(ctx, task)
		if ok {
			processed++
			s.retries.clear(key)
			s.mu.Lock()
			current := s.pending[key]
			// A scan may have enqueued a newer terminal state/review while
			// this request was in flight. Never acknowledge that newer work.
			if current.revision == task.revision {
				delete(s.pending, key)
			}
			s.mu.Unlock()
		} else {
			failed++
			// Accounting is not discarded at the request-file quarantine
			// horizon: a later successful batch must still record it.
			s.retries.noteFailure(key, time.Now())
		}
	}
	s.mu.Lock()
	pending := len(s.pending)
	s.mu.Unlock()
	if c.logger != nil && (processed+failed > 0) {
		c.logger.Info("historical PR enrichment batch",
			"processed", processed, "failed", failed, "deferred", pending,
			"duration", time.Since(started), "budget", budget, "limit", limit)
	}
}
