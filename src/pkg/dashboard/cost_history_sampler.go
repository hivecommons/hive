package dashboard

import (
	"context"
	"time"
)

// costHistorySampleInterval is the server-side sampler's period: the cost
// ring's own throttle, so the ring really holds ~30 days at 5-min intervals
// whether or not a browser is polling /api/knowledge/stats.
const costHistorySampleInterval = costHistoryMinIntervalMs * time.Millisecond

// StartCostHistorySampler samples the estimated-cost and fact histories every
// costHistorySampleInterval until ctx is cancelled. The browser-triggered
// sample in handleKnowledgeStats stays; both go through the same throttled
// rings, so they never double the cadence. Call once at startup.
func (s *Server) StartCostHistorySampler(ctx context.Context) {
	go s.runCostHistorySampler(ctx, costHistorySampleInterval)
}

func (s *Server) runCostHistorySampler(ctx context.Context, interval time.Duration) {
	s.sampleHistories(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sampleHistories(ctx)
		}
	}
}

// sampleHistories records one fact and one cost snapshot. Unlike
// handleKnowledgeStats it never creates the knowledge API: the fact count is
// sampled only when knowledge is already wired, while the cost is always
// sampled.
func (s *Server) sampleHistories(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	if s.deps != nil {
		s.knowledgeMu.Lock()
		k := s.deps.Knowledge
		s.knowledgeMu.Unlock()
		if k != nil {
			if total, ok := knowledgeFactTotal(k.Stats(ctx)); ok {
				s.AppendFactHistory(total)
			}
		}
	}
	s.sampleCostHistory()
}
