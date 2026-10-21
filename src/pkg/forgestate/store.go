package forgestate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"go.opentelemetry.io/otel/metric"
)

var (
	// ErrNotFound is what a Fetcher returns for an item GitHub no longer
	// serves (deleted or transferred); the cache drops it.
	ErrNotFound = errors.New("forgestate: item not found")
	// ErrUnknownRepo is returned for a repo the Store was not built with.
	ErrUnknownRepo = errors.New("forgestate: unknown repo")
	// ErrNotReady is returned by ReadThrough before the repo's first full
	// reconcile.
	ErrNotReady = errors.New("forgestate: repo has not completed its first reconcile")
	// ErrStale is returned by ReadThrough while the repo's snapshot is stale:
	// irreversible actions must never act on it.
	ErrStale = errors.New("forgestate: snapshot is stale")
)

// ListResult is one full listing of a repo's open issues and PRs.
type ListResult struct {
	Issues []Item
	PRs    []Item
	// ETag identifies this listing; the Store passes it back on the next
	// List so a Fetcher that supports conditional requests can answer
	// NotModified instead of re-sending the list.
	ETag        string
	NotModified bool
}

// Fetcher reads GitHub. The Store gates every call through its Gate first.
type Fetcher interface {
	// List returns every open issue and pull request in repo. It must
	// return an error rather than a partial list.
	List(ctx context.Context, repo, etag string) (ListResult, error)
	// Issue and PR return one item in whatever state it is in, or an
	// error wrapping ErrNotFound when GitHub no longer serves it.
	Issue(ctx context.Context, repo string, number int) (Item, error)
	PR(ctx context.Context, repo string, number int) (Item, error)
}

// Options configures New. Zero fields take the forgestate config defaults.
type Options struct {
	Repos   []string
	Fetcher Fetcher
	// Gate defaults to a BudgetGate with the default search budget.
	Gate    Gate
	Metrics Metrics
	Logger  *slog.Logger
	Now     func() time.Time
	// After is the timer Run waits on; time.After by default.
	After func(time.Duration) <-chan time.Time

	ReconcileInterval time.Duration
	// MinReconcileInterval is the interval used while a repo is stale or its
	// drift is above DriftThreshold. Default one minute, never longer than
	// ReconcileInterval.
	MinReconcileInterval time.Duration
	FreshnessBound       time.Duration
	DriftThreshold       float64
	// SearchBudget is the per-minute search budget of the default Gate.
	SearchBudget int
	// Critical is the default Gate's API-budget critical hook.
	Critical func() bool
	// Budget reports the rate-limit budget left per bucket; the reconcile
	// interval stretches as it drains (AdaptiveInterval). Nil uses the
	// process default from SetDefaultBudgetSource, or no stretching.
	Budget BudgetFunc
}

// OptionsFromConfig maps the `forgestate:` block onto Options. The caller
// still supplies Repos, Fetcher and (optionally) Critical.
func OptionsFromConfig(c config.ForgeStateConfig) Options {
	return Options{
		ReconcileInterval: c.ReconcileInterval(),
		FreshnessBound:    c.FreshnessBound(),
		DriftThreshold:    c.DriftThresholdOrDefault(),
		SearchBudget:      c.SearchBudgetPerMinute(),
	}
}

type itemKey struct {
	kind   Kind
	number int
}

// tombstone remembers a removal so an older read cannot resurrect the item.
type tombstone struct {
	updatedAt time.Time // the item's updated_at when it was removed
	at        time.Time // when the cache removed it
}

const maxDeliveries = 64

type repoState struct {
	name   string
	issues map[int]Item
	prs    map[int]Item
	tombs  map[itemKey]tombstone
	etag   string

	ready        bool
	stale        bool
	staleSince   time.Time
	staleReason  string
	lastGoodAt   time.Time
	lastDelivery string
	deliveries   []string
	drift        int
	driftHigh    bool
}

func (rs *repoState) items(k Kind) map[int]Item {
	if k == KindPR {
		return rs.prs
	}
	return rs.issues
}

// Store is the cache. One per process; safe for concurrent use.
type Store struct {
	opts      Options
	mu        sync.RWMutex
	repos     map[string]*repoState // fixed after New
	order     []string
	ready     chan struct{}
	readyOnce sync.Once
	staleness metric.Registration
}

// New returns a Store for o. It does no I/O; call Run (or Reconcile per
// repo) to fill it.
func New(o Options) (*Store, error) {
	if o.Fetcher == nil {
		return nil, errors.New("forgestate: Options.Fetcher is required")
	}
	if o.Metrics == nil {
		o.Metrics = DefaultCounters()
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.After == nil {
		o.After = time.After
	}
	if o.ReconcileInterval <= 0 {
		o.ReconcileInterval = config.DefaultForgeStateReconcileInterval
	}
	if o.MinReconcileInterval <= 0 {
		o.MinReconcileInterval = time.Minute
	}
	if o.MinReconcileInterval > o.ReconcileInterval {
		o.MinReconcileInterval = o.ReconcileInterval
	}
	if o.FreshnessBound <= 0 {
		o.FreshnessBound = config.DefaultForgeStateFreshnessBound
	}
	if o.DriftThreshold <= 0 {
		o.DriftThreshold = config.DefaultForgeStateDriftThreshold
	}
	if o.SearchBudget <= 0 {
		o.SearchBudget = config.DefaultForgeStateSearchBudget
	}
	if o.Gate == nil {
		o.Gate = NewBudgetGate(BudgetConfig{
			PerMinute: map[Bucket]int{BucketSearch: o.SearchBudget},
			Critical:  o.Critical,
			Now:       o.Now,
			Metrics:   o.Metrics,
		})
	}
	s := &Store{opts: o, repos: map[string]*repoState{}, ready: make(chan struct{})}
	for _, r := range o.Repos {
		key := canonicalRepo(r)
		if key == "" || s.repos[key] != nil {
			continue
		}
		s.repos[key] = &repoState{
			name:   r,
			issues: map[int]Item{},
			prs:    map[int]Item{},
			tombs:  map[itemKey]tombstone{},
		}
		s.order = append(s.order, key)
	}
	if len(s.repos) == 0 {
		s.readyOnce.Do(func() { close(s.ready) })
	}
	s.staleness = s.registerStaleness()
	return s, nil
}

// Close releases the Store's metric registration. The cache itself needs no
// teardown; Close only matters for processes that build many Stores.
func (s *Store) Close() error {
	if s.staleness != nil {
		return s.staleness.Unregister()
	}
	return nil
}

func (s *Store) lookup(repo string) *repoState { return s.repos[canonicalRepo(repo)] }

// ReconcileInterval is the steady-state interval between full reconciles.
func (s *Store) ReconcileInterval() time.Duration { return s.opts.ReconcileInterval }

// Repos returns the cached repos in configuration order.
func (s *Store) Repos() []string {
	out := make([]string, 0, len(s.order))
	for _, k := range s.order {
		out = append(out, s.repos[k].name)
	}
	return out
}

// Admit passes one call in bucket b through the Store's Gate. Readers that
// still call GitHub directly (search counts until they migrate) use it so
// their spend shares the cache's budget.
func (s *Store) Admit(ctx context.Context, b Bucket) error { return s.opts.Gate.Admit(ctx, b) }

// Snapshot returns a copy of repo's cache.
func (s *Store) Snapshot(repo string) (Snapshot, bool) {
	rs := s.lookup(repo)
	if rs == nil {
		return Snapshot{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	snap := Snapshot{
		Repo:           rs.name,
		Issues:         make(map[int]Item, len(rs.issues)),
		PRs:            make(map[int]Item, len(rs.prs)),
		Ready:          rs.ready,
		Stale:          rs.stale,
		StaleSince:     rs.staleSince,
		StaleReason:    rs.staleReason,
		LastGoodAt:     rs.lastGoodAt,
		LastDeliveryID: rs.lastDelivery,
		Drift:          rs.drift,
		DriftHigh:      rs.driftHigh,
	}
	for n, it := range rs.issues {
		snap.Issues[n] = it.clone()
	}
	for n, it := range rs.prs {
		snap.PRs[n] = it.clone()
	}
	return snap, true
}

// Issue returns the cached open issue n of repo.
func (s *Store) Issue(repo string, n int) (Item, bool) { return s.cached(repo, KindIssue, n) }

// PR returns the cached open pull request n of repo.
func (s *Store) PR(repo string, n int) (Item, bool) { return s.cached(repo, KindPR, n) }

func (s *Store) cached(repo string, k Kind, n int) (Item, bool) {
	rs := s.lookup(repo)
	if rs == nil {
		return Item{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	it, ok := rs.items(k)[n]
	return it.clone(), ok
}

// Deliveries returns the most recent webhook delivery ids applied to repo,
// oldest first (audit trail).
func (s *Store) Deliveries(repo string) []string {
	rs := s.lookup(repo)
	if rs == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), rs.deliveries...)
}

// Fresh reports whether repo's last successful full reconcile is within
// bound (Options.FreshnessBound when bound <= 0) and the snapshot is not
// stale. Item-level checks before an irreversible action use ReadThrough.
func (s *Store) Fresh(repo string, bound time.Duration) bool {
	rs := s.lookup(repo)
	if rs == nil {
		return false
	}
	if bound <= 0 {
		bound = s.opts.FreshnessBound
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return rs.ready && !rs.stale && s.opts.Now().Sub(rs.lastGoodAt) <= bound
}

// Ready reports whether every repo completed its first full reconcile.
func (s *Store) Ready() bool {
	select {
	case <-s.ready:
		return true
	default:
		return false
	}
}

// WaitReady blocks until every repo completed its first full reconcile, or
// ctx ends. It is the startup gate in front of the first governor pass.
func (s *Store) WaitReady(ctx context.Context) error {
	select {
	case <-s.ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ReadThrough returns item n for an irreversible action. The cached copy is
// used when it was fetched within bound (Options.FreshnessBound when
// bound <= 0); otherwise the item is re-fetched and upserted first. It
// refuses with ErrNotReady/ErrStale rather than act on an unproven snapshot.
// ok is false when the item is closed, merged or gone.
func (s *Store) ReadThrough(ctx context.Context, repo string, k Kind, n int, bound time.Duration) (Item, bool, error) {
	rs := s.lookup(repo)
	if rs == nil {
		return Item{}, false, fmt.Errorf("%w: %s", ErrUnknownRepo, repo)
	}
	if bound <= 0 {
		bound = s.opts.FreshnessBound
	}
	s.mu.RLock()
	ready, stale := rs.ready, rs.stale
	cur, ok := rs.items(k)[n]
	s.mu.RUnlock()
	switch {
	case !ready:
		return Item{}, false, ErrNotReady
	case stale:
		return Item{}, false, ErrStale
	}
	if ok && s.opts.Now().Sub(cur.FetchedAt) <= bound {
		s.opts.Metrics.ReadThrough(rs.name, resultCached)
		return cur.clone(), true, nil
	}
	it, err := s.fetch(ctx, rs.name, k, n)
	if errors.Is(err, ErrNotFound) {
		s.mu.Lock()
		s.removeLocked(rs, k, n, time.Time{}, s.opts.Now(), true)
		s.mu.Unlock()
		s.opts.Metrics.ReadThrough(rs.name, resultRemoved)
		return Item{}, false, nil
	}
	if err != nil {
		s.opts.Metrics.ReadThrough(rs.name, resultError)
		return Item{}, false, err
	}
	s.mu.Lock()
	res := s.upsertLocked(rs, it, SourceReadThrough, s.opts.Now())
	got, ok := rs.items(k)[n]
	got = got.clone()
	s.mu.Unlock()
	s.opts.Metrics.ReadThrough(rs.name, res)
	return got, ok, nil
}

// fetch gates and performs one single-item read.
func (s *Store) fetch(ctx context.Context, repo string, k Kind, n int) (Item, error) {
	if err := s.opts.Gate.Admit(ctx, BucketCore); err != nil {
		return Item{}, err
	}
	var (
		it  Item
		err error
	)
	if k == KindPR {
		it, err = s.opts.Fetcher.PR(ctx, repo, n)
	} else {
		it, err = s.opts.Fetcher.Issue(ctx, repo, n)
	}
	if err != nil {
		return Item{}, err
	}
	it.Kind, it.Number = k, n
	return it, nil
}

// supersedes reports whether in is a newer view of the item than cur:
// a later updated_at, or for a PR the same updated_at with a new head SHA.
func supersedes(in, cur Item) bool {
	if in.UpdatedAt.After(cur.UpdatedAt) {
		return true
	}
	return in.Kind == KindPR && in.UpdatedAt.Equal(cur.UpdatedAt) && in.HeadSHA != "" && in.HeadSHA != cur.HeadSHA
}

// upsertLocked applies a fetched item monotonically and reports the result
// label: resultOK (stored), resultRemoved (closed/merged, dropped) or
// resultIgnored (older than what the cache already knows).
func (s *Store) upsertLocked(rs *repoState, it Item, src Source, now time.Time) string {
	it = it.clone()
	it.Source, it.FetchedAt = src, now
	m := rs.items(it.Kind)
	key := itemKey{it.Kind, it.Number}
	if cur, ok := m[it.Number]; ok && !supersedes(it, cur) {
		return resultIgnored
	}
	if t, ok := rs.tombs[key]; ok && !it.UpdatedAt.After(t.updatedAt) {
		return resultIgnored
	}
	if !it.Open() {
		delete(m, it.Number)
		rs.tombs[key] = tombstone{updatedAt: it.UpdatedAt, at: now}
		return resultRemoved
	}
	delete(rs.tombs, key)
	m[it.Number] = it
	return resultOK
}

// removeLocked drops item n and leaves a tombstone. Unless force is set, a
// removal whose updatedAt is older than the cached item's is ignored (the
// item was reopened since). It reports whether anything changed.
func (s *Store) removeLocked(rs *repoState, k Kind, n int, updatedAt, now time.Time, force bool) bool {
	m := rs.items(k)
	key := itemKey{k, n}
	cur, ok := m[n]
	if ok && !force && updatedAt.Before(cur.UpdatedAt) {
		return false
	}
	if ok && cur.UpdatedAt.After(updatedAt) {
		updatedAt = cur.UpdatedAt
	}
	if t, had := rs.tombs[key]; had && t.updatedAt.After(updatedAt) {
		updatedAt = t.updatedAt
	}
	delete(m, n)
	rs.tombs[key] = tombstone{updatedAt: updatedAt, at: now}
	return true
}

func (s *Store) markReadyLocked(rs *repoState) {
	if rs.ready {
		return
	}
	rs.ready = true
	for _, other := range s.repos {
		if !other.ready {
			return
		}
	}
	s.readyOnce.Do(func() { close(s.ready) })
}
