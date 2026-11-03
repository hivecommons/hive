package main

import (
	"context"
	"sync"

	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/worksource"
)

// sentinelEventQueue serializes webhook-triggered sentinel evaluations
// (#13379). The dashboard's webhook handler must answer GitHub quickly, so
// the hook only enqueues; one worker drains the queue and runs the per-PR
// sweep, and a PR already waiting is not queued twice (a push storm on one
// PR costs one evaluation of its latest head, which is all the sentinel
// reads anyway).
type sentinelEventQueue struct {
	mu      sync.Mutex
	pending map[sentinelEventKey]bool
	ch      chan sentinelEventKey
	run     func(ctx context.Context, repo string, number int)
}

type sentinelEventKey struct {
	repo   string
	number int
}

// sentinelEventQueueDepth bounds how many distinct PRs may wait; past it the
// hook drops the delivery and the reconcile cadence picks the PR up.
const sentinelEventQueueDepth = 256

func newSentinelEventQueue(run func(ctx context.Context, repo string, number int)) *sentinelEventQueue {
	return &sentinelEventQueue{
		pending: map[sentinelEventKey]bool{},
		ch:      make(chan sentinelEventKey, sentinelEventQueueDepth),
		run:     run,
	}
}

// Enqueue is the dashboard.Dependencies.SentinelPREvent hook. It reports
// whether the PR was queued (false: already pending or queue full).
func (q *sentinelEventQueue) Enqueue(repo string, number int) bool {
	if q == nil || repo == "" || number <= 0 {
		return false
	}
	key := sentinelEventKey{repo: repo, number: number}
	q.mu.Lock()
	if q.pending[key] {
		q.mu.Unlock()
		return false
	}
	select {
	case q.ch <- key:
		q.pending[key] = true
		q.mu.Unlock()
		return true
	default:
		q.mu.Unlock()
		return false
	}
}

// Run drains the queue until ctx ends.
func (q *sentinelEventQueue) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case key := <-q.ch:
			q.mu.Lock()
			delete(q.pending, key)
			q.mu.Unlock()
			if q.run != nil {
				q.run(ctx, key.repo, key.number)
			}
		}
	}
}

// installSentinelEvents builds the queue the dashboard webhook receiver
// feeds and starts its worker on the boot context. Idempotent, like
// installWebhookWake.
func (b *boot) installSentinelEvents() {
	if b.sentinelEvents != nil {
		return
	}
	b.sentinelEvents = newSentinelEventQueue(func(ctx context.Context, repo string, number int) {
		runSentinelPREvent(ctx, b.ghClient, b.cfg, b.dashSrv, repo, number, b.logger)
	})
	ctx := b.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	go b.sentinelEvents.Run(ctx)
}

// sentinelPREventFunc is the dashboard.Dependencies.SentinelPREvent hook.
func (b *boot) sentinelPREventFunc() func(repo string, number int) {
	if b.sentinelEvents == nil {
		return nil
	}
	return func(repo string, number int) { b.sentinelEvents.Enqueue(repo, number) }
}

// projectsV2ItemEventFunc is the dashboard.Dependencies.ProjectsV2ItemEvent
// hook: a verified projects_v2_item delivery patches the GitHub Projects
// work source cache. The patch's one GraphQL read runs inline; it is a
// single call and the dashboard handler already answers GitHub before
// GitHub's delivery timeout.
func (b *boot) projectsV2ItemEventFunc() func(payload []byte) dashboard.WebhookPatchOutcome {
	return func(payload []byte) dashboard.WebhookPatchOutcome {
		ctx := b.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		patch, err := worksource.ApplyProjectsV2ItemEvent(ctx, payload)
		if err != nil {
			b.logger.Warn("projects_v2_item webhook patch failed", "action", patch.Action, "error", err)
			return dashboard.WebhookPatchOutcome{Detail: err.Error()}
		}
		if patch.Matched {
			b.logger.Debug("projects_v2_item webhook patch applied", "outcome", patch.String())
		}
		return dashboard.WebhookPatchOutcome{Applied: patch.Removed || patch.Upserted, Detail: patch.String()}
	}
}
