package mention

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
)

type Poller struct {
	gh       GitHub
	ghFunc   func() GitHub
	repos    func() []string
	store    *Store
	handler  *Handler
	interval time.Duration
	logger   *slog.Logger
	pollMu   sync.Mutex
	inFlight map[string]bool
	pending  map[string]bool
}

func NewPoller(gh GitHub, repos func() []string, store *Store, handler *Handler, interval time.Duration, logger *slog.Logger) *Poller {
	if interval <= 0 {
		interval = time.Minute
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Poller{gh: gh, repos: repos, store: store, handler: handler, interval: interval, logger: logger, inFlight: map[string]bool{}, pending: map[string]bool{}}
}

func (p *Poller) SetGitHubGetter(fn func() GitHub) {
	p.ghFunc = fn
}

func (p *Poller) github() GitHub {
	if p.ghFunc != nil {
		return p.ghFunc()
	}
	return p.gh
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
	if p == nil || p.handler == nil || p.repos == nil {
		return
	}
	for _, repo := range p.repos() {
		p.PollRepo(ctx, repo)
	}
}

func (p *Poller) PollRepo(ctx context.Context, repo string) {
	if p == nil || p.handler == nil || repo == "" {
		return
	}
	repo, ok := p.configuredRepo(repo)
	if !ok {
		p.logger.Debug("mention: repo poll ignored for unconfigured repo", "repo", repo)
		return
	}
	if !p.claimRepo(repo) {
		p.logger.Debug("mention: repo poll coalesced", "repo", repo)
		return
	}
	for {
		p.pollRepoOnce(ctx, repo)
		if !p.releaseRepo(repo) {
			return
		}
	}
}

func (p *Poller) configuredRepo(repo string) (string, bool) {
	if p == nil || p.repos == nil {
		return "", false
	}
	want := strings.TrimSpace(repo)
	if want == "" {
		return "", false
	}
	for _, configured := range p.repos() {
		candidate := strings.TrimSpace(configured)
		if candidate != "" && strings.EqualFold(candidate, want) {
			return candidate, true
		}
	}
	return "", false
}

func (p *Poller) pollRepoOnce(ctx context.Context, repo string) {
	gh := p.github()
	if gh == nil {
		return
	}
	since := time.Time{}
	if p.store != nil {
		since = p.store.Watermark(repo)
		if since.IsZero() {
			since = time.Now().UTC()
			_ = p.store.Advance(repo, since)
		}
	}
	events, err := gh.ListMentionComments(ctx, repo, since)
	if err != nil {
		p.logger.Warn("mention: poll failed", "repo", repo, "error", err)
		return
	}
	sort.SliceStable(events, func(i, j int) bool {
		return events[i].UpdatedAt.Before(events[j].UpdatedAt)
	})
	safeWatermark := since
	for _, ev := range events {
		if ev.CreatedAt.IsZero() || (!since.IsZero() && ev.CreatedAt.Before(since)) {
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

func (p *Poller) claimRepo(repo string) bool {
	p.pollMu.Lock()
	defer p.pollMu.Unlock()
	if p.inFlight == nil {
		p.inFlight = map[string]bool{}
	}
	if p.pending == nil {
		p.pending = map[string]bool{}
	}
	if p.inFlight[repo] {
		p.pending[repo] = true
		return false
	}
	p.inFlight[repo] = true
	return true
}

func (p *Poller) releaseRepo(repo string) bool {
	p.pollMu.Lock()
	defer p.pollMu.Unlock()
	if p.pending[repo] {
		delete(p.pending, repo)
		return true
	}
	delete(p.inFlight, repo)
	return false
}
