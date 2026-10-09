package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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
	syncMu   sync.Mutex
}

// Syncer runs every enabled connector on its interval and writes the pages
// it emits into the vault for the connector's layer.
type Syncer struct {
	opts    SyncerOptions
	logger  *slog.Logger
	order   []string
	entries map[string]*entry

	mu     sync.Mutex
	status map[string]Status
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
	persisted := s.loadState()
	for _, cfg := range cfgs {
		if _, dup := s.entries[cfg.Name]; dup {
			return nil, fmt.Errorf("connector: duplicate name %q", cfg.Name)
		}
		deps := opts.Deps
		if deps.StateDir != "" {
			deps.StateDir = filepath.Join(deps.StateDir, cfg.Name)
		}
		e := &entry{cfg: cfg}
		e.conn, e.buildErr = opts.Registry.New(cfg, deps)
		st := persisted[cfg.Name]
		st.Name, st.Type, st.Layer = cfg.Name, cfg.Type, cfg.Layer
		st.Enabled = cfg.Enabled
		st.Interval = cfg.EffectiveInterval().String()
		st.Running = false
		if e.buildErr != nil {
			st.LastError = e.buildErr.Error()
			s.logger.Warn("knowledge connector disabled: invalid config", "name", cfg.Name, "type", cfg.Type, "error", e.buildErr)
		}
		s.entries[cfg.Name] = e
		s.order = append(s.order, cfg.Name)
		s.status[cfg.Name] = st
	}
	return s, nil
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
	s.status[name] = st
	return st
}

// SyncNow runs one sync of the named connector immediately, whether or not
// it is enabled. It returns ErrSyncInProgress if a sync is already running.
func (s *Syncer) SyncNow(ctx context.Context, name string) (Status, error) {
	e, ok := s.entries[name]
	if !ok {
		return Status{}, fmt.Errorf("%w: %q", ErrUnknownConnector, name)
	}
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
			return
		}
		st.LastError = ""
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
		s.logger.Warn("knowledge connector sync failed", "name", name, "type", e.cfg.Type, "error", err)
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
	if err != nil {
		return pages, cur, false, err
	}
	truncated := false
	if t, ok := e.conn.(Truncator); ok && t.Truncated() {
		// A capped listing is not complete: never tombstone what it missed.
		truncated, full = true, false
	}
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
	return pages, next, truncated, nil
}

// Run syncs every enabled, valid connector immediately and then on its
// interval until ctx is cancelled. It blocks until all loops have exited.
func (s *Syncer) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, name := range s.order {
		e := s.entries[name]
		if !e.cfg.Enabled || e.buildErr != nil {
			continue
		}
		wg.Add(1)
		go func(name string, interval time.Duration) {
			defer wg.Done()
			for {
				_, _ = s.SyncNow(ctx, name)
				if sleepCtx(ctx, interval) != nil {
					return
				}
			}
		}(name, e.cfg.EffectiveInterval())
	}
	wg.Wait()
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
