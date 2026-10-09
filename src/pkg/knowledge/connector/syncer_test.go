package connector

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeConn is a scriptable in-memory connector.
type fakeConn struct {
	typ         string
	validateErr error

	mu      sync.Mutex
	pages   []Page
	next    Cursor
	err     error
	cursors []Cursor
	calls   atomic.Int32
	started chan struct{}
	release chan struct{}
}

func (f *fakeConn) Type() string                   { return f.typ }
func (f *fakeConn) Validate(ConnectorConfig) error { return f.validateErr }

func (f *fakeConn) set(pages []Page, next Cursor, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pages, f.next, f.err = pages, next, err
}

func (f *fakeConn) Sync(ctx context.Context, cur Cursor, emit func(Page) error) (Cursor, error) {
	f.calls.Add(1)
	if f.started != nil {
		f.started <- struct{}{}
		<-f.release
	}
	f.mu.Lock()
	f.cursors = append(f.cursors, cur)
	pages, next, err := append([]Page(nil), f.pages...), f.next, f.err
	f.mu.Unlock()
	for _, p := range pages {
		if err := emit(p); err != nil {
			return cur, err
		}
	}
	if err != nil {
		return cur, err
	}
	return next, nil
}

type fullFakeConn struct{ *fakeConn }

func (fullFakeConn) FullListing() bool { return true }

// newTestSyncer registers conns by name under type "fake" and returns a
// syncer writing into per-layer dirs under a temp root.
func newTestSyncer(t *testing.T, cfgs []ConnectorConfig, conns map[string]Connector, mutate func(*SyncerOptions)) (*Syncer, string) {
	t.Helper()
	root := t.TempDir()
	reg := NewRegistry()
	factory := func(cfg ConnectorConfig, _ Deps) (Connector, error) {
		c, ok := conns[cfg.Name]
		if !ok {
			return nil, errors.New("no fake for " + cfg.Name)
		}
		return c, nil
	}
	if err := reg.Register("fake", factory); err != nil {
		t.Fatal(err)
	}
	opts := SyncerOptions{
		Registry: reg,
		Deps:     Deps{StateDir: filepath.Join(root, "state")},
		VaultDir: func(layer string) (string, error) { return filepath.Join(root, "vault", layer), nil },
		Now:      func() time.Time { return time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC) },
	}
	if mutate != nil {
		mutate(&opts)
	}
	s, err := NewSyncer(cfgs, opts)
	if err != nil {
		t.Fatalf("NewSyncer: %v", err)
	}
	return s, root
}

func fakeCfg(name string) ConnectorConfig {
	return ConnectorConfig{Name: name, Type: "fake", Enabled: true, Layer: "project", Interval: time.Hour}
}

func readFact(t *testing.T, dir, slug string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, slug+".md"))
	if err != nil {
		t.Fatalf("reading %s: %v", slug, err)
	}
	return frontMatter(string(data))
}

func TestNewSyncerErrors(t *testing.T) {
	if _, err := NewSyncer(nil, SyncerOptions{}); err == nil || !strings.Contains(err.Error(), "VaultDir is required") {
		t.Fatalf("err = %v", err)
	}
	vd := func(string) (string, error) { return t.TempDir(), nil }
	if _, err := NewSyncer([]ConnectorConfig{fakeCfg("a"), fakeCfg("a")}, SyncerOptions{VaultDir: vd}); err == nil || !strings.Contains(err.Error(), "duplicate name") {
		t.Fatalf("duplicate err = %v", err)
	}
	// Defaults: DefaultRegistry, which does not know type "fake" — the entry
	// is kept with its build error rather than failing the whole syncer.
	s, err := NewSyncer([]ConnectorConfig{fakeCfg("a")}, SyncerOptions{VaultDir: vd})
	if err != nil {
		t.Fatal(err)
	}
	st, ok := s.Status("a")
	if !ok || !strings.Contains(st.LastError, "unknown type") || st.Interval != "1h0m0s" || !st.Enabled {
		t.Fatalf("status = %+v", st)
	}
	if _, err := s.SyncNow(context.Background(), "a"); err == nil || !strings.Contains(err.Error(), "unknown type") {
		t.Fatalf("SyncNow on broken entry = %v", err)
	}
	if _, err := s.SyncNow(context.Background(), "zzz"); !errors.Is(err, ErrUnknownConnector) {
		t.Fatalf("unknown name err = %v", err)
	}
	if _, ok := s.Status("zzz"); ok {
		t.Fatal("Status of unknown connector reported ok")
	}
}

func TestSyncerWritesFactsAndStatus(t *testing.T) {
	inc := &fakeConn{typ: "fake"}
	full := fullFakeConn{&fakeConn{typ: "fake"}}
	cfgs := []ConnectorConfig{fakeCfg("wiki"), fakeCfg("repo")}
	s, root := newTestSyncer(t, cfgs, map[string]Connector{"wiki": inc, "repo": full}, nil)
	vault := filepath.Join(root, "vault", "project")
	ctx := context.Background()

	inc.set([]Page{
		{ID: "p1", Title: "One", Markdown: "first"},
		{ID: "p2", Title: "Two", Markdown: "second", URL: "https://wiki.example/p2"},
	}, "c1", nil)
	st, err := s.SyncNow(ctx, "wiki")
	if err != nil {
		t.Fatalf("SyncNow: %v", err)
	}
	if st.Pages != 2 || st.Facts != 2 || st.Deprecated != 0 || st.Cursor != "c1" || st.LastError != "" || st.LastSync.IsZero() || st.Running {
		t.Fatalf("status after first sync = %+v", st)
	}
	if fm := readFact(t, vault, "fake-wiki-p2"); fm["status"] != StatusActive || fm["source_url"] != "https://wiki.example/p2" || fm["layer"] != "project" {
		t.Fatalf("front-matter = %v", fm)
	}

	// Incremental sync: p1 not re-emitted must NOT be tombstoned; p2 archived upstream.
	inc.set([]Page{{ID: "p2", Title: "Two", Markdown: "second", Archived: true}}, "c2", nil)
	st, err = s.SyncNow(ctx, "wiki")
	if err != nil {
		t.Fatal(err)
	}
	if st.Pages != 1 || st.Facts != 1 || st.Deprecated != 1 || st.Cursor != "c2" {
		t.Fatalf("status after incremental = %+v", st)
	}
	if got := inc.cursors; len(got) != 2 || got[0] != "" || got[1] != "c1" {
		t.Fatalf("cursors passed = %v", got)
	}
	if readFact(t, vault, "fake-wiki-p1")["status"] != StatusActive {
		t.Fatal("incremental sync tombstoned an unlisted page")
	}

	// Failed sync: error recorded, cursor and last sync kept.
	prev := st
	inc.set(nil, "c3", errors.New("rate limited"))
	st, err = s.SyncNow(ctx, "wiki")
	if err == nil || st.LastError != "rate limited" || st.Cursor != prev.Cursor || !st.LastSync.Equal(prev.LastSync) || st.Pages != prev.Pages {
		t.Fatalf("status after failure = %+v (err %v)", st, err)
	}

	// Full lister: a page that disappears from the listing is tombstoned,
	// and the other connector's facts are untouched.
	full.set([]Page{{ID: "a"}, {ID: "b"}}, "h1", nil)
	if _, err := s.SyncNow(ctx, "repo"); err != nil {
		t.Fatal(err)
	}
	full.set([]Page{{ID: "a"}}, "h2", nil)
	st, err = s.SyncNow(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if st.Facts != 1 || st.Deprecated != 1 {
		t.Fatalf("full listing status = %+v", st)
	}
	if readFact(t, vault, "fake-repo-b")["status"] != StatusDeprecated {
		t.Fatal("page missing from full listing was not tombstoned")
	}
	if readFact(t, vault, "fake-wiki-p1")["status"] != StatusActive {
		t.Fatal("another connector's fact was tombstoned")
	}
	// A page that comes back is reactivated.
	full.set([]Page{{ID: "a"}, {ID: "b"}}, "h3", nil)
	if st, _ = s.SyncNow(ctx, "repo"); st.Facts != 2 || st.Deprecated != 0 {
		t.Fatalf("reactivated status = %+v", st)
	}

	names := []string{}
	for _, st := range s.Statuses() {
		names = append(names, st.Name)
	}
	if strings.Join(names, ",") != "wiki,repo" {
		t.Fatalf("Statuses order = %v", names)
	}
}

func TestSyncerSyncErrors(t *testing.T) {
	ctx := context.Background()

	t.Run("vault resolve error", func(t *testing.T) {
		c := &fakeConn{typ: "fake"}
		s, _ := newTestSyncer(t, []ConnectorConfig{fakeCfg("x")}, map[string]Connector{"x": c}, func(o *SyncerOptions) {
			o.VaultDir = func(string) (string, error) { return "", errors.New("no layer") }
		})
		if _, err := s.SyncNow(ctx, "x"); err == nil || !strings.Contains(err.Error(), "no layer") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("empty page id", func(t *testing.T) {
		c := &fakeConn{typ: "fake"}
		c.set([]Page{{ID: " "}}, "n", nil)
		s, _ := newTestSyncer(t, []ConnectorConfig{fakeCfg("x")}, map[string]Connector{"x": c}, nil)
		st, err := s.SyncNow(ctx, "x")
		if err == nil || !strings.Contains(err.Error(), "empty ID") || st.Cursor != "" {
			t.Fatalf("err = %v status %+v", err, st)
		}
	})

	t.Run("cancelled context", func(t *testing.T) {
		c := &fakeConn{typ: "fake"}
		c.set([]Page{{ID: "a"}}, "n", nil)
		s, _ := newTestSyncer(t, []ConnectorConfig{fakeCfg("x")}, map[string]Connector{"x": c}, nil)
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := s.SyncNow(cctx, "x"); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("vault is a file", func(t *testing.T) {
		c := &fakeConn{typ: "fake"}
		file := filepath.Join(t.TempDir(), "f")
		if err := os.WriteFile(file, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		s, _ := newTestSyncer(t, []ConnectorConfig{fakeCfg("x")}, map[string]Connector{"x": c}, func(o *SyncerOptions) {
			o.VaultDir = func(string) (string, error) { return file, nil }
		})
		// No pages emitted and a full listing: the tombstone scan fails.
		if _, err := s.SyncNow(ctx, "x"); err == nil || !strings.Contains(err.Error(), "listing vault") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestSyncerInProgress(t *testing.T) {
	c := &fakeConn{typ: "fake", started: make(chan struct{}), release: make(chan struct{})}
	s, _ := newTestSyncer(t, []ConnectorConfig{fakeCfg("x")}, map[string]Connector{"x": c}, nil)
	done := make(chan error, 1)
	go func() {
		_, err := s.SyncNow(context.Background(), "x")
		done <- err
	}()
	<-c.started
	st, err := s.SyncNow(context.Background(), "x")
	if !errors.Is(err, ErrSyncInProgress) || !st.Running {
		t.Fatalf("concurrent SyncNow = %+v, %v", st, err)
	}
	close(c.release)
	if err := <-done; err != nil {
		t.Fatalf("first sync: %v", err)
	}
}

func TestSyncerStatePersistence(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "sub", "connectors.json")
	c := &fakeConn{typ: "fake"}
	c.set([]Page{{ID: "a"}}, "cur-1", nil)
	s, root := newTestSyncer(t, []ConnectorConfig{fakeCfg("x")}, map[string]Connector{"x": c}, func(o *SyncerOptions) {
		o.StatePath = statePath
	})
	if _, err := s.SyncNow(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}

	c2 := &fakeConn{typ: "fake"}
	reg := NewRegistry()
	_ = reg.Register("fake", func(ConnectorConfig, Deps) (Connector, error) { return c2, nil })
	s2, err := NewSyncer([]ConnectorConfig{fakeCfg("x")}, SyncerOptions{
		Registry:  reg,
		VaultDir:  func(layer string) (string, error) { return filepath.Join(root, "vault", layer), nil },
		StatePath: statePath,
	})
	if err != nil {
		t.Fatal(err)
	}
	st, _ := s2.Status("x")
	if st.Cursor != "cur-1" || st.Pages != 1 || st.Facts != 1 || st.LastSync.IsZero() {
		t.Fatalf("restored status = %+v", st)
	}
	if _, err := s2.SyncNow(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if len(c2.cursors) != 1 || c2.cursors[0] != "cur-1" {
		t.Fatalf("restored cursor not used: %v", c2.cursors)
	}
}

func TestSyncerStateErrors(t *testing.T) {
	dir := t.TempDir()
	corrupt := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(corrupt, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"corrupt":      corrupt,
		"directory":    dir,                                  // unreadable as a file
		"parent file":  filepath.Join(blocker, "state.json"), // save fails: parent is a file
		"missing file": filepath.Join(dir, "absent.json"),
	} {
		t.Run(name, func(t *testing.T) {
			c := &fakeConn{typ: "fake"}
			c.set([]Page{{ID: "a"}}, "n", nil)
			s, _ := newTestSyncer(t, []ConnectorConfig{fakeCfg("x")}, map[string]Connector{"x": c}, func(o *SyncerOptions) {
				o.StatePath = path
			})
			if st, _ := s.Status("x"); st.Cursor != "" {
				t.Fatalf("bad state produced cursor %q", st.Cursor)
			}
			if _, err := s.SyncNow(context.Background(), "x"); err != nil {
				t.Fatalf("sync must succeed even when state cannot be persisted: %v", err)
			}
		})
	}
}

func TestSyncerRun(t *testing.T) {
	on := &fakeConn{typ: "fake"}
	off := &fakeConn{typ: "fake"}
	cfgOn := fakeCfg("on")
	cfgOn.Interval = 5 * time.Millisecond
	cfgOff := fakeCfg("off")
	cfgOff.Enabled = false
	broken := fakeCfg("broken")
	s, _ := newTestSyncer(t, []ConnectorConfig{cfgOn, cfgOff, broken}, map[string]Connector{"on": on, "off": off}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(done)
	}()
	deadline := time.After(10 * time.Second)
	for on.calls.Load() < 2 {
		select {
		case <-deadline:
			t.Fatal("enabled connector was not re-synced on its interval")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if off.calls.Load() != 0 {
		t.Fatal("disabled connector was synced")
	}
}

type truncFakeConn struct {
	*fakeConn
	truncated bool
}

func (t *truncFakeConn) Truncated() bool { return t.truncated }

func TestSyncerTruncatedSkipsTombstonesAndOnSync(t *testing.T) {
	c := &truncFakeConn{fakeConn: &fakeConn{typ: "fake"}}
	c.set([]Page{{ID: "a"}, {ID: "b"}}, "cur-1", nil)
	var calls []Status
	var errs []error
	s, root := newTestSyncer(t, []ConnectorConfig{fakeCfg("x")}, map[string]Connector{"x": c}, func(o *SyncerOptions) {
		o.OnSync = func(st Status, err error) { calls = append(calls, st); errs = append(errs, err) }
	})
	dir := filepath.Join(root, "vault", "project")
	if _, err := s.SyncNow(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	// Reset the cursor so the next sync is a full listing, but cap it.
	s.update("x", func(st *Status) { st.Cursor = "" })
	c.set([]Page{{ID: "a"}}, "cur-2", nil)
	c.truncated = true
	st, err := s.SyncNow(context.Background(), "x")
	if err != nil || !st.Truncated || st.Cursor != "cur-2" {
		t.Fatalf("truncated sync = %+v, %v", st, err)
	}
	if got := readFact(t, dir, Slug("fake", "x", "b"))["status"]; got != StatusActive {
		t.Fatalf("truncated full listing tombstoned b: status %q", got)
	}
	// An untruncated full listing tombstones the missing page and clears the flag.
	s.update("x", func(st *Status) { st.Cursor = "" })
	c.truncated = false
	st, err = s.SyncNow(context.Background(), "x")
	if err != nil || st.Truncated {
		t.Fatalf("full sync = %+v, %v", st, err)
	}
	if got := readFact(t, dir, Slug("fake", "x", "b"))["status"]; got != StatusDeprecated {
		t.Fatalf("missing page status = %q, want deprecated", got)
	}
	c.set(nil, "", errors.New("upstream down"))
	if _, err := s.SyncNow(context.Background(), "x"); err == nil {
		t.Fatal("expected sync error")
	}
	if len(calls) != 4 || calls[0].Pages != 2 || !calls[1].Truncated || errs[3] == nil || calls[3].LastError != "upstream down" {
		t.Fatalf("OnSync calls = %+v errs = %v", calls, errs)
	}
}
