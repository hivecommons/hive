package inbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// routeTimeout bounds one background delivery.
const routeTimeout = 15 * time.Minute

// Settings is the operator-editable part of the Inbox (mirrors inbox.*
// YAML minus binary/agent plumbing).
type Settings struct {
	Sources  []string           `json:"sources"`
	Rules    []config.InboxRule `json:"rules"`
	SpekRepo string             `json:"spek_repo"`
	DibsURL  string             `json:"dibs_url"`
	// DibsTokenSet reports whether inbox.dibs.token_env resolves; the token
	// itself is never returned.
	DibsTokenSet bool `json:"dibs_token_set"`
	SpekReady    bool `json:"spek_ready"`
}

// Service lists items across sources, applies persisted state, runs
// auto-routing rules and executes manual routes in the background.
type Service struct {
	Store  *Store
	Logger *slog.Logger
	// Now is injectable for tests.
	Now func() time.Time

	mu      sync.RWMutex
	cfg     config.InboxConfig
	sources map[string]Source
	dests   map[string]Destination
	wg      sync.WaitGroup
	// Run executes a delivery; nil runs it in a goroutine. Tests set it to
	// run inline.
	Run func(fn func())
	// Status snapshots for the settings view.
	spekReady    func() bool
	dibsTokenSet func() bool
}

// NewService builds a service for cfg with the given sources/destinations.
func NewService(cfg config.InboxConfig, store *Store, sources []Source, dests []Destination, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Service{Store: store, Logger: logger, cfg: cfg, sources: map[string]Source{}, dests: map[string]Destination{}}
	for _, src := range sources {
		s.sources[src.Type()] = src
	}
	for _, d := range dests {
		s.dests[d.Name()] = d
	}
	return s
}

// SetReadiness installs the probes behind Settings.SpekReady / DibsTokenSet.
func (s *Service) SetReadiness(spek, dibs func() bool) {
	s.spekReady, s.dibsTokenSet = spek, dibs
}

// Config returns the current inbox config.
func (s *Service) Config() config.InboxConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// UpdateConfig replaces the routing config (after the dashboard saved it).
func (s *Service) UpdateConfig(cfg config.InboxConfig) {
	s.mu.Lock()
	s.cfg = cfg
	s.mu.Unlock()
}

// Settings returns the operator view of the config.
func (s *Service) Settings() Settings {
	cfg := s.Config()
	st := Settings{
		Sources:  cfg.EffectiveSources(),
		Rules:    cfg.Rules,
		SpekRepo: cfg.Spek.Repo,
		DibsURL:  cfg.Dibs.URL,
	}
	if st.Rules == nil {
		st.Rules = []config.InboxRule{}
	}
	if s.spekReady != nil {
		st.SpekReady = s.spekReady()
	}
	if s.dibsTokenSet != nil {
		st.DibsTokenSet = s.dibsTokenSet()
	}
	return st
}

// Items lists every item across the configured sources with state applied,
// newest first.
func (s *Service) Items(ctx context.Context) ([]Item, error) {
	cfg := s.Config()
	var out []Item
	var errs []error
	for _, typ := range cfg.EffectiveSources() {
		src := s.source(typ)
		if src == nil {
			continue
		}
		items, err := src.List(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", typ, err))
		}
		out = append(out, items...)
	}
	s.Store.Apply(out)
	sortItems(out)
	return out, errors.Join(errs...)
}

func (s *Service) source(typ string) Source {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sources[typ]
}

// Find returns one item by key.
func (s *Service) Find(ctx context.Context, source, id string) (Item, bool, error) {
	items, err := s.Items(ctx)
	for _, it := range items {
		if it.Source == source && it.ID == id {
			return it, true, err
		}
	}
	return Item{}, false, err
}

// Unprocessed counts items with no filed route and not dismissed.
func Unprocessed(items []Item) int {
	n := 0
	for _, it := range items {
		if !it.Processed {
			n++
		}
	}
	return n
}

// Dismiss marks an item knowledge-only.
func (s *Service) Dismiss(source, id string, dismissed bool) error {
	return s.Store.Dismiss(Key(source, id), dismissed)
}

// ErrUnknownDestination is returned for a destination the service lacks.
var ErrUnknownDestination = errors.New("unknown destination")

// Route queues it for dest and runs the delivery in the background. The
// returned state is the queued state; callers poll Items for progress.
func (s *Service) Route(ctx context.Context, it Item, req Request) (RouteState, error) {
	if req.Destination == DestDismiss {
		if err := s.Dismiss(it.Source, it.ID, true); err != nil {
			return RouteState{}, err
		}
		return RouteState{Status: StatusFiled, UpdatedAt: s.now()}, nil
	}
	s.mu.RLock()
	dest := s.dests[req.Destination]
	s.mu.RUnlock()
	if dest == nil {
		return RouteState{}, fmt.Errorf("%w: %q", ErrUnknownDestination, req.Destination)
	}
	if req.Destination == DestSpekRFC && strings.TrimSpace(req.Repo) == "" {
		req.Repo = s.Config().Spek.Repo
	}
	st, err := s.Store.Begin(it.Key(), req.Destination, req.Repo)
	if err != nil {
		return st, err
	}
	s.wg.Add(1)
	run := s.Run
	if run == nil {
		run = func(fn func()) { go fn() }
	}
	run(func() {
		defer s.wg.Done()
		s.deliver(it, dest, req)
	})
	return st, nil
}

func (s *Service) deliver(it Item, dest Destination, req Request) {
	key := it.Key()
	ctx, cancel := context.WithTimeout(context.Background(), routeTimeout)
	defer cancel()
	if _, err := s.Store.Set(key, req.Destination, StatusRunning, Result{}, ""); err != nil {
		s.Logger.Warn("inbox: persist running state", "item", key, "error", err)
	}
	src := s.source(it.Source)
	if src == nil {
		s.fail(key, req.Destination, fmt.Errorf("source %q is not configured", it.Source))
		return
	}
	content, err := src.Fetch(ctx, it)
	if err != nil {
		s.fail(key, req.Destination, fmt.Errorf("fetch: %w", err))
		return
	}
	res, err := dest.Deliver(ctx, it, content, req)
	if err != nil {
		s.fail(key, req.Destination, err)
		return
	}
	status := StatusFiled
	if res.URL == "" && res.Note != "" {
		status = StatusDelegated
	}
	if _, err := s.Store.Set(key, req.Destination, status, res, res.Note); err != nil {
		s.Logger.Warn("inbox: persist filed state", "item", key, "error", err)
	}
	s.Logger.Info("inbox: routed", "item", key, "destination", req.Destination, "status", status, "url", res.URL)
}

func (s *Service) fail(key, dest string, err error) {
	s.Logger.Warn("inbox: route failed", "item", key, "destination", dest, "error", err)
	if _, perr := s.Store.Set(key, dest, StatusFailed, Result{}, err.Error()); perr != nil {
		s.Logger.Warn("inbox: persist failed state", "item", key, "error", perr)
	}
}

// Sweep applies auto-routing rules to items the store has never seen and
// marks every listed item seen, so a rule added later never back-fires on
// old recordings. It is called after each connector sync.
func (s *Service) Sweep(ctx context.Context) (routed int, err error) {
	items, lerr := s.Items(ctx)
	rules := s.Config().Rules
	for _, it := range items {
		if s.Store.Seen(it.Key()) {
			continue
		}
		rule, ok := FirstRule(rules, it)
		if !ok {
			_ = s.Store.MarkSeen(it.Key())
			continue
		}
		if _, rerr := s.Route(ctx, it, Request{Destination: rule.Destination, Repo: rule.Repo}); rerr != nil {
			s.Logger.Warn("inbox: auto-route", "item", it.Key(), "error", rerr)
			_ = s.Store.MarkSeen(it.Key())
			continue
		}
		routed++
	}
	return routed, lerr
}

// Wait blocks until background deliveries finish (tests, shutdown).
func (s *Service) Wait() { s.wg.Wait() }

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func sortItems(items []Item) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j].RecordedAt.After(items[j-1].RecordedAt); j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}
