package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"
)

// DefaultSyncTimeout caps a single connector sync run.
const DefaultSyncTimeout = 10 * time.Minute

// ErrUnknownConnector is returned for a name that is not configured.
var ErrUnknownConnector = errors.New("connector: unknown connector")

// ErrSyncInProgress is returned when a sync for the connector is already running.
var ErrSyncInProgress = errors.New("connector: sync already in progress")

// Status is the per-connector state surfaced to operators.
type Status struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Layer    string `json:"layer"`
	Enabled  bool   `json:"enabled"`
	Interval string `json:"interval"`
	// LastSync is the completion time of the last successful sync.
	LastSync    time.Time `json:"last_sync,omitempty"`
	LastAttempt time.Time `json:"last_attempt,omitempty"`
	// Pages is the number of pages emitted by the last successful sync.
	Pages int `json:"pages"`
	// Facts is the number of active facts this connector owns in the vault.
	Facts      int    `json:"facts"`
	Deprecated int    `json:"deprecated"`
	LastError  string `json:"last_error,omitempty"`
	Cursor     Cursor `json:"cursor,omitempty"`
	Running    bool   `json:"running"`
	// Truncated reports that the last successful sync stopped at the
	// connector's page cap; the remainder is picked up by later syncs.
	Truncated bool `json:"truncated"`
}

// SyncerOptions configures NewSyncer.
type SyncerOptions struct {
	// Registry resolves connector types; nil uses DefaultRegistry().
	Registry *Registry
	// Deps is passed to every factory. When Deps.StateDir is set, each
	// connector gets its own StateDir/<name> subdirectory.
	Deps Deps
	// VaultDir maps a layer name to the vault directory facts are written to.
	VaultDir func(layer string) (string, error)
	// StatePath, when set, persists cursors and status across restarts.
	StatePath    string
	SyncTimeout  time.Duration
	MaxPageBytes int
	Now          func() time.Time
	// OnSync, when set, is called after every sync attempt with the updated
	// status and the sync error (nil on success), e.g. to reindex the vault.
	OnSync func(st Status, err error)
}

type entry struct {
	cfg      ConnectorConfig
	conn     Connector
	buildErr error
	// syncMu is shared by every entry built for the same name, so a
	// reconfigured connector never syncs concurrently with its predecessor.
	syncMu *sync.Mutex
	// stop cancels this entry's Run loop; nil when no loop is running.
	stop context.CancelFunc
}

// Syncer runs every enabled connector on its interval and writes the pages
// it emits into the vault for the connector's layer. Reconfigure adds,
// replaces and removes connectors while it runs (#18091).
type Syncer struct {
	opts   SyncerOptions
	logger *slog.Logger

	// mu guards order, entries, status and runCtx.
	mu      sync.Mutex
	order   []string
	entries map[string]*entry
	status  map[string]Status
	// runCtx is Run's context once Run has started; loops for connectors
	// added later by Reconfigure derive from it.
	runCtx context.Context
	loops  sync.WaitGroup
}

// NewSyncer builds connectors for cfgs. A connector that fails to build is
// kept with its error in Status.LastError so one bad entry never hides the
// others; duplicate names and a missing VaultDir are hard errors.
func NewSyncer(cfgs []ConnectorConfig, opts SyncerOptions) (*Syncer, error) {
	if opts.VaultDir == nil {
		return nil, fmt.Errorf("connector: SyncerOptions.VaultDir is required")
	}
	if opts.Registry == nil {
		opts.Registry = DefaultRegistry()
	}
	if opts.SyncTimeout <= 0 {
		opts.SyncTimeout = DefaultSyncTimeout
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	s := &Syncer{
		opts:    opts,
		logger:  opts.Deps.logger(),
		entries: map[string]*entry{},
		status:  map[string]Status{},
	}
	if err := checkDuplicateNames(cfgs); err != nil {
		return nil, err
	}
	persisted := s.loadState()
	for _, cfg := range cfgs {
		st := persisted[cfg.Name]
		st.Running = false
		e := s.newEntry(cfg, &sync.Mutex{}, &st)
		s.entries[cfg.Name] = e
		s.order = append(s.order, cfg.Name)
		s.status[cfg.Name] = st
	}
	return s, nil
}

func checkDuplicateNames(cfgs []ConnectorConfig) error {
	seen := make(map[string]bool, len(cfgs))
	for _, cfg := range cfgs {
		if seen[cfg.Name] {
			return fmt.Errorf("connector: duplicate name %q", cfg.Name)
		}
		seen[cfg.Name] = true
	}
	return nil
}

// newEntry builds the connector for cfg and refreshes st's config fields.
// A build error is recorded in st.LastError.
func (s *Syncer) newEntry(cfg ConnectorConfig, syncMu *sync.Mutex, st *Status) *entry {
	deps := s.opts.Deps
	if deps.StateDir != "" {
		deps.StateDir = filepath.Join(deps.StateDir, cfg.Name)
	}
	e := &entry{cfg: cfg, syncMu: syncMu}
	e.conn, e.buildErr = s.opts.Registry.New(cfg, deps)
	st.Name, st.Type, st.Layer = cfg.Name, cfg.Type, cfg.Layer
	st.Enabled = cfg.Enabled
	st.Interval = cfg.EffectiveInterval().String()
	if e.buildErr != nil {
		st.LastError = e.buildErr.Error()
		s.logger.Warn("knowledge connector disabled: invalid config", "name", cfg.Name, "type", cfg.Type, "error", e.buildErr)
	}
	return e
}

// Reconfigure applies cfgs to a built (and possibly running) syncer without a
// restart (#18091): new names are added, entries whose config changed are
// rebuilt, and names no longer listed are removed. Unchanged entries keep
// their connector and Run loop. Status (cursor, counts) carries over for a
// changed entry, as it does across a hive restart. Duplicate names are an
// error and leave the syncer unchanged.
func (s *Syncer) Reconfigure(cfgs []ConnectorConfig) error {
	if err := checkDuplicateNames(cfgs); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	keep := make(map[string]bool, len(cfgs))
	order := make([]string, 0, len(cfgs))
	for _, cfg := range cfgs {
		keep[cfg.Name] = true
		order = append(order, cfg.Name)
		old, ok := s.entries[cfg.Name]
		if ok && reflect.DeepEqual(old.cfg, cfg) {
			continue
		}
		syncMu := &sync.Mutex{}
		st := s.status[cfg.Name]
		if ok {
			s.stopLoopLocked(old)
			syncMu = old.syncMu
			if old.buildErr != nil && st.LastError == old.buildErr.Error() {
				st.LastError = ""
			}
		}
		e := s.newEntry(cfg, syncMu, &st)
		s.entries[cfg.Name] = e
		s.status[cfg.Name] = st
		s.startLoopLocked(cfg.Name, e)
		s.logger.Info("knowledge connector loaded", "name", cfg.Name, "type", cfg.Type, "enabled", cfg.Enabled, "replaced", ok)
	}
	for _, name := range s.order {
		if keep[name] {
			continue
		}
		s.stopLoopLocked(s.entries[name])
		delete(s.entries, name)
		delete(s.status, name)
		s.logger.Info("knowledge connector removed", "name", name)
	}
	s.order = order
	return nil
}

func (s *Syncer) lookup(name string) (*entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[name]
	return e, ok
}

// Statuses returns a snapshot of every connector's status, in config order.
func (s *Syncer) Statuses() []Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Status, 0, len(s.order))
	for _, n := range s.order {
		out = append(out, s.status[n])
	}
	return out
}

// Status returns one connector's status.
func (s *Syncer) Status(name string) (Status, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.status[name]
	return st, ok
}

func (s *Syncer) update(name string, fn func(*Status)) Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.status[name]
	fn(&st)
	// A sync that finishes after Reconfigure removed its connector must
	// not resurrect the removed status.
	if _, ok := s.entries[name]; ok {
		s.status[name] = st
	}
	return st
}

// Connector returns the built connector for name; ok is false for an
// unknown name or one whose config failed to build.
func (s *Syncer) Connector(name string) (Connector, bool) {
	e, ok := s.lookup(name)
	if !ok || e.buildErr != nil || e.conn == nil {
		return nil, false
	}
	return e.conn, true
}

// SyncNow runs one sync of the named connector immediately, whether or not
// it is enabled. It returns ErrSyncInProgress if a sync is already running.
func (s *Syncer) SyncNow(ctx context.Context, name string) (Status, error) {
	e, ok := s.lookup(name)
	if !ok {
		return Status{}, fmt.Errorf("%w: %q", ErrUnknownConnector, name)
	}
	return s.syncEntry(ctx, name, e)
}

// syncEntry runs one sync of e. A Run loop passes its own entry so a loop
// cancelled by Reconfigure never syncs the entry that replaced it.
func (s *Syncer) syncEntry(ctx context.Context, name string, e *entry) (Status, error) {
	if e.buildErr != nil {
		st, _ := s.Status(name)
		return st, e.buildErr
	}
	if !e.syncMu.TryLock() {
		st, _ := s.Status(name)
		return st, ErrSyncInProgress
	}
	defer e.syncMu.Unlock()

	var cur Cursor
	s.update(name, func(st *Status) {
		st.Running = true
		st.LastAttempt = s.opts.Now().UTC()
		cur = st.Cursor
	})

	pages, next, truncated, err := s.runSync(ctx, e, cur)

	var counts map[string]string
	var dir string
	if d, derr := s.opts.VaultDir(e.cfg.Layer); derr == nil {
		dir = d
		w := &FactWriter{Dir: dir}
		counts, _ = w.Existing(e.cfg)
	}
	st := s.update(name, func(st *Status) {
		st.Running = false
		if counts != nil {
			st.Facts, st.Deprecated = 0, 0
			for _, v := range counts {
				if v == StatusDeprecated {
					st.Deprecated++
				} else {
					st.Facts++
				}
			}
		}
		if err != nil {
			st.LastError = err.Error()
			// A partial sync still produced a complete listing: record
			// the error but advance as for a success.
			if !IsPartial(err) {
				return
			}
		} else {
			st.LastError = ""
		}
		st.LastSync = s.opts.Now().UTC()
		st.Pages = pages
		st.Cursor = next
		st.Truncated = truncated
	})
	s.saveState()
	if s.opts.OnSync != nil {
		s.opts.OnSync(st, err)
	}
	if err != nil {
		s.logger.Warn("knowledge connector sync failed", "name", name, "type", e.cfg.Type, "partial", IsPartial(err), "error", err)
	} else {
		s.logger.Info("knowledge connector synced", "name", name, "type", e.cfg.Type, "pages", pages, "facts", st.Facts, "vault", dir)
	}
	return st, err
}

func (s *Syncer) runSync(ctx context.Context, e *entry, cur Cursor) (int, Cursor, bool, error) {
	dir, err := s.opts.VaultDir(e.cfg.Layer)
	if err != nil {
		return 0, cur, false, fmt.Errorf("resolving vault for layer %q: %w", e.cfg.Layer, err)
	}
	ctx, cancel := context.WithTimeout(ctx, s.opts.SyncTimeout)
	defer cancel()

	w := &FactWriter{Dir: dir, MaxPageBytes: s.opts.MaxPageBytes, Now: s.opts.Now}
	full := isFullListing(e.conn, cur)
	seen := map[string]bool{}
	pages := 0
	emit := func(p Page) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		slug, _, err := w.Write(e.cfg, p)
		if err != nil {
			return fmt.Errorf("writing page %q: %w", p.ID, err)
		}
		seen[slug] = true
		pages++
		return nil
	}
	next, err := e.conn.Sync(ctx, cur, emit)
	if err != nil && !IsPartial(err) {
		return pages, cur, false, err
	}
	truncated := false
	if t, ok := e.conn.(Truncator); ok && t.Truncated() {
		// A capped listing is not complete: never tombstone what it missed.
		truncated, full = true, false
	}
	partial := err
	if full {
		existing, err := w.Existing(e.cfg)
		if err != nil {
			return pages, cur, truncated, err
		}
		for slug, st := range existing {
			if seen[slug] || st == StatusDeprecated {
				continue
			}
			if _, err := w.Deprecate(slug); err != nil {
				return pages, cur, truncated, err
			}
		}
	}
	return pages, next, truncated, partial
}

// Run syncs every enabled, valid connector immediately and then on its
// interval until ctx is cancelled; connectors added or changed later by
// Reconfigure start their own loop. It blocks until ctx is cancelled and
// every loop has exited.
func (s *Syncer) Run(ctx context.Context) {
	s.mu.Lock()
	s.runCtx = ctx
	for _, name := range s.order {
		s.startLoopLocked(name, s.entries[name])
	}
	s.mu.Unlock()
	<-ctx.Done()
	s.loops.Wait()
}

// startLoopLocked starts e's interval loop when Run is active and e is
// enabled and valid. s.mu must be held.
func (s *Syncer) startLoopLocked(name string, e *entry) {
	if s.runCtx == nil || s.runCtx.Err() != nil || !e.cfg.Enabled || e.buildErr != nil {
		return
	}
	ctx, cancel := context.WithCancel(s.runCtx)
	e.stop = cancel
	interval := e.cfg.EffectiveInterval()
	s.loops.Add(1)
	go func() {
		defer s.loops.Done()
		// Wait out a sync still running for a replaced entry of the same
		// name so the new config's first sync is not skipped as in progress.
		e.syncMu.Lock()
		e.syncMu.Unlock() //nolint:staticcheck // SA2001: intentional barrier
		for ctx.Err() == nil {
			_, _ = s.syncEntry(ctx, name, e)
			if sleepCtx(ctx, interval) != nil {
				return
			}
		}
	}()
}

// stopLoopLocked cancels e's interval loop, if any. s.mu must be held.
func (s *Syncer) stopLoopLocked(e *entry) {
	if e != nil && e.stop != nil {
		e.stop()
		e.stop = nil
	}
}

type persistedStatus struct {
	Cursor      Cursor    `json:"cursor,omitempty"`
	LastSync    time.Time `json:"last_sync,omitempty"`
	LastAttempt time.Time `json:"last_attempt,omitempty"`
	Pages       int       `json:"pages"`
	Facts       int       `json:"facts"`
	Deprecated  int       `json:"deprecated"`
	LastError   string    `json:"last_error,omitempty"`
	Truncated   bool      `json:"truncated,omitempty"`
}

func (s *Syncer) loadState() map[string]Status {
	out := map[string]Status{}
	if s.opts.StatePath == "" {
		return out
	}
	data, err := os.ReadFile(s.opts.StatePath)
	if err != nil {
		if !os.IsNotExist(err) {
			s.logger.Warn("knowledge connector state unreadable; starting fresh", "path", s.opts.StatePath, "error", err)
		}
		return out
	}
	var raw map[string]persistedStatus
	if err := json.Unmarshal(data, &raw); err != nil {
		s.logger.Warn("knowledge connector state corrupt; starting fresh", "path", s.opts.StatePath, "error", err)
		return out
	}
	for name, p := range raw {
		out[name] = Status{
			Cursor: p.Cursor, LastSync: p.LastSync, LastAttempt: p.LastAttempt,
			Pages: p.Pages, Facts: p.Facts, Deprecated: p.Deprecated, LastError: p.LastError,
			Truncated: p.Truncated,
		}
	}
	return out
}

func (s *Syncer) saveState() {
	if s.opts.StatePath == "" {
		return
	}
	s.mu.Lock()
	raw := make(map[string]persistedStatus, len(s.status))
	for name, st := range s.status {
		raw[name] = persistedStatus{
			Cursor: st.Cursor, LastSync: st.LastSync, LastAttempt: st.LastAttempt,
			Pages: st.Pages, Facts: st.Facts, Deprecated: st.Deprecated, LastError: st.LastError,
			Truncated: st.Truncated,
		}
	}
	s.mu.Unlock()
	data, err := json.MarshalIndent(raw, "", "  ")
	if err == nil {
		err = writeAtomic(s.opts.StatePath, string(data))
	}
	if err != nil {
		s.logger.Warn("persisting knowledge connector state failed", "path", s.opts.StatePath, "error", err)
	}
}
