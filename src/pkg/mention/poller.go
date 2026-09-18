package mention

import (
	"context"
	"log/slog"
	"sort"
	"time"
)

type Poller struct {
	gh       GitHub
	repos    func() []string
	store    *Store
	handler  *Handler
	interval time.Duration
	logger   *slog.Logger
}

func NewPoller(gh GitHub, repos func() []string, store *Store, handler *Handler, interval time.Duration, logger *slog.Logger) *Poller {
	if interval <= 0 {
		interval = time.Minute
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Poller{gh: gh, repos: repos, store: store, handler: handler, interval: interval, logger: logger}
}
func (p *Poller) Run(ctx context.Context) {
	p.Poll(ctx)
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.Poll(ctx)
		}
	}
}
func (p *Poller) Poll(ctx context.Context) {
	if p == nil || p.gh == nil || p.handler == nil || p.repos == nil {
		return
	}
	for _, repo := range p.repos() {
		since := time.Time{}
		if p.store != nil {
			since = p.store.Watermark(repo)
			if since.IsZero() {
				since = time.Now().UTC()
				_ = p.store.Advance(repo, since)
			}
		}
		events, err := p.gh.ListMentionComments(ctx, repo, since)
		if err != nil {
			p.logger.Warn("mention: poll failed", "repo", repo, "error", err)
			continue
		}
		sort.SliceStable(events, func(i, j int) bool {
			return events[i].UpdatedAt.Before(events[j].UpdatedAt)
		})
		safeWatermark := since
		for _, ev := range events {
			if ev.CreatedAt.IsZero() || (!since.IsZero() && !ev.CreatedAt.After(since)) {
				if ev.UpdatedAt.After(safeWatermark) {
					safeWatermark = ev.UpdatedAt
				}
				continue
			}
			if err := p.handler.Handle(ctx, ev); err != nil {
				p.logger.Warn("mention: handle failed", "repo", repo, "node_id", ev.NodeID, "error", err)
				break
			}
			if ev.UpdatedAt.After(safeWatermark) {
				safeWatermark = ev.UpdatedAt
			}
		}
		if p.store != nil {
			_ = p.store.Advance(repo, safeWatermark)
		}
	}
}
