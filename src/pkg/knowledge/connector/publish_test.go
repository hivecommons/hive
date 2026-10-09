package connector

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakePublisher struct {
	mu    sync.Mutex
	calls [][]Page
	roots []string
	err   error
}

func (f *fakePublisher) Publish(_ context.Context, root string, pages []Page) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, pages)
	f.roots = append(f.roots, root)
	return f.err
}

func (f *fakePublisher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func pubLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func pubFact(t *testing.T, dir, rel, front, body string) {
	t.Helper()
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("---\n"+front+"\n---\n\n"+body+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

const promotedFront = "title: Retry budgets\ntype: decision\nsource: promoted from project by curator: auto"

func pubMirror(t *testing.T, vault string, pub Publisher, mutate func(*MirrorOptions)) *Mirror {
	t.Helper()
	opts := MirrorOptions{
		Config:    PublishConfig{Connector: "docs", Layers: []string{"org"}, Root: "Hive/Knowledge", ProposeVia: "https://github.com/acme/kb"},
		Publisher: pub,
		VaultDir:  func(string) (string, error) { return vault, nil },
		StatePath: filepath.Join(t.TempDir(), "publish.json"),
		Logger:    pubLogger(),
		Now:       func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) },
	}
	if mutate != nil {
		mutate(&opts)
	}
	m, err := NewMirror(opts)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestPublishKey(t *testing.T) {
	a, b := PublishKey("org/retry-budgets"), PublishKey("project/retry-budgets")
	if !strings.HasPrefix(a, "hive-org-retry-budgets") || a == b {
		t.Fatalf("keys %q %q", a, b)
	}
	if PublishKey("org/sub/x") == PublishKey("org/sub-x") {
		t.Fatal("lossy slug must not collide")
	}
}

func TestRenderPublishPage(t *testing.T) {
	tests := []struct {
		name       string
		title      string
		body       string
		deprecated bool
		banner     string
		propose    string
		want       []string
		notWant    []string
	}{
		{"active", "T", "body text", false, "", "https://github.com/acme/kb",
			[]string{"<!-- hive_fact_id: org/a-b -->", "# T\n\nbody text", "Maintained by Hive — edits here are overwritten; propose changes via https://github.com/acme/kb."},
			[]string{"Deprecated"}},
		{"deprecated default banner", "T", "# Own heading\nx", true, "", "",
			[]string{"> **Deprecated:** this fact is deprecated", "# Own heading", "propose changes via the Hive knowledge vault."},
			[]string{"# T\n"}},
		{"custom banner empty body", "T", "", true, "gone.", "",
			[]string{"> **Deprecated:** gone.", "# T\n\n---"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RenderPublishPage("org/a--b", tt.title, tt.body, tt.deprecated, tt.banner, tt.propose)
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("missing %q in:\n%s", w, got)
				}
			}
			for _, w := range tt.notWant {
				if strings.Contains(got, w) {
					t.Errorf("unexpected %q in:\n%s", w, got)
				}
			}
		})
	}
}

func TestNewPublisher(t *testing.T) {
	sp := spCfg(map[string]string{"drive_ids": "d1"}, Auth{Env: "SP_TOKEN"})
	tests := []struct {
		name    string
		reg     *Registry
		cfg     ConnectorConfig
		wantErr string
	}{
		{"sharepoint publishes", nil, sp, ""},
		{"document cannot publish", DefaultRegistry(), ConnectorConfig{Name: "doc", Type: TypeDocument, Layer: "org", Scope: map[string]string{"url": "https://example.com/x.md"}}, "does not support publishing"},
		{"invalid config", nil, spCfg(nil, Auth{}), "auth.env or auth.file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := NewPublisher(tt.reg, tt.cfg, spDeps())
			if tt.wantErr == "" {
				if err != nil || p == nil {
					t.Fatalf("NewPublisher = %v, %v", p, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestNewMirrorErrors(t *testing.T) {
	vd := func(string) (string, error) { return "", nil }
	cfg := PublishConfig{Connector: "docs", Layers: []string{"org"}, Root: "R"}
	tests := []struct {
		name    string
		opts    MirrorOptions
		wantErr string
	}{
		{"no publisher", MirrorOptions{Config: cfg, VaultDir: vd}, "Publisher is required"},
		{"no vault", MirrorOptions{Config: cfg, Publisher: &fakePublisher{}}, "VaultDir is required"},
		{"no layers", MirrorOptions{Config: PublishConfig{Root: "R"}, Publisher: &fakePublisher{}, VaultDir: vd}, "layers are required"},
		{"personal layer", MirrorOptions{Config: PublishConfig{Layers: []string{"personal"}, Root: "R"}, Publisher: &fakePublisher{}, VaultDir: vd}, "\"personal\""},
		{"unknown layer", MirrorOptions{Config: PublishConfig{Layers: []string{"team"}, Root: "R"}, Publisher: &fakePublisher{}, VaultDir: vd}, "\"team\""},
		{"no root", MirrorOptions{Config: PublishConfig{Layers: []string{"org"}, Root: " / "}, Publisher: &fakePublisher{}, VaultDir: vd}, "root is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewMirror(tt.opts)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
	m, err := NewMirror(MirrorOptions{Config: cfg, Publisher: &fakePublisher{}, VaultDir: vd})
	if err != nil || m.opts.Interval != DefaultPublishInterval || m.logger == nil || m.opts.Now == nil {
		t.Fatalf("defaults not applied: %v", err)
	}
}

// TestMirrorLifecycle walks one fact through create → unchanged → update →
// deprecate → removed, asserting what reaches the publisher at each step.
func TestMirrorLifecycle(t *testing.T) {
	vault := t.TempDir()
	pub := &fakePublisher{}
	var audits []PublishReport
	m := pubMirror(t, vault, pub, func(o *MirrorOptions) { o.Audit = func(r PublishReport) { audits = append(audits, r) } })
	pubFact(t, vault, "retry-budgets.md", promotedFront, "Cap retries at 3.")

	steps := []struct {
		name        string
		mutate      func()
		wantCreated int
		wantUpdated int
		wantDepr    int
		wantSame    int
		wantSent    int
		wantInPage  string
	}{
		{"create", func() {}, 1, 0, 0, 0, 1, "Cap retries at 3."},
		{"unchanged skipped", func() {}, 0, 0, 0, 1, 0, ""},
		{"update", func() { pubFact(t, vault, "retry-budgets.md", promotedFront, "Cap retries at 5.") }, 0, 1, 0, 0, 1, "Cap retries at 5."},
		{"deprecate banner", func() {
			pubFact(t, vault, "retry-budgets.md", promotedFront+"\nstatus: deprecated", "Cap retries at 5.")
		}, 0, 0, 1, 0, 1, "> **Deprecated:** this fact is deprecated"},
		{"deprecated unchanged", func() {}, 0, 0, 0, 1, 0, ""},
		{"removed tombstone", func() { _ = os.Remove(filepath.Join(vault, "retry-budgets.md")) }, 0, 1, 0, 0, 1, "removed from the Hive knowledge vault"},
		{"tombstone unchanged", func() {}, 0, 0, 0, 1, 0, ""},
	}
	for _, s := range steps {
		s.mutate()
		before := pub.count()
		r, err := m.Run(context.Background())
		if err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		if len(r.Created) != s.wantCreated || len(r.Updated) != s.wantUpdated || len(r.Deprecated) != s.wantDepr || r.Unchanged != s.wantSame {
			t.Fatalf("%s: report %+v", s.name, r)
		}
		sent := pub.count() - before
		if (s.wantSent == 0) != (sent == 0) {
			t.Fatalf("%s: publisher calls = %d, want %d pages", s.name, sent, s.wantSent)
		}
		if s.wantSent > 0 {
			pages := pub.calls[len(pub.calls)-1]
			if len(pages) != s.wantSent || !strings.Contains(pages[0].Markdown, s.wantInPage) {
				t.Fatalf("%s: pages %+v", s.name, pages)
			}
			p := pages[0]
			if p.ID != "org/retry-budgets" || p.Attrs[PublishMarker] != p.ID || p.Attrs[PublishKeyAttr] != PublishKey(p.ID) ||
				!strings.Contains(p.Markdown, "<!-- hive_fact_id: org/retry-budgets -->") || pub.roots[len(pub.roots)-1] != "Hive/Knowledge" {
				t.Fatalf("%s: page %+v", s.name, p)
			}
		}
	}
	if len(audits) != len(steps) {
		t.Fatalf("audits = %d, want one per batch", len(audits))
	}
	st := m.Status()
	if st.Pages != 1 || st.LastReport == nil || st.LastError != "" || st.LastSuccess.IsZero() || st.Running || st.Connector != "docs" {
		t.Fatalf("status %+v", st)
	}
}

func TestMirrorSelectsPromotedFacts(t *testing.T) {
	org, project := t.TempDir(), t.TempDir()
	pubFact(t, org, "a.md", promotedFront, "A")
	pubFact(t, org, "sub/b.md", "type: decision\nsource: promoted from project by x: y", "B")
	pubFact(t, org, "manual.md", "title: Manual\ntype: decision\nsource: manual", "no")
	pubFact(t, org, "gotcha.md", "title: G\ntype: gotcha\nsource: promoted from project by x: y", "no")
	pubFact(t, org, "personal.md", "title: P\ntype: decision\nlayer: personal\nsource: promoted from personal by x: y", "no")
	pubFact(t, org, ".obsidian/hidden.md", promotedFront, "no")
	pubFact(t, org, "superseded.md", "title: S\ntype: decision\nstate: superseded\nsource: promoted from project by x: y", "S")
	if err := os.WriteFile(filepath.Join(org, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	pubFact(t, project, "c.md", promotedFront, "C")
	dirs := map[string]string{"org": org, "project": project, "community": org}
	pub := &fakePublisher{}
	m := pubMirror(t, "", pub, func(o *MirrorOptions) {
		o.Config.Layers = []string{"org", "project", "community"}
		o.Config.IncludeTypes = []string{"decision"}
		o.VaultDir = func(l string) (string, error) { return dirs[l], nil }
	})
	r, err := m.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(r.Created, ",")
	if got != "org/a,org/sub/b,org/superseded,project/c" {
		t.Fatalf("created = %s", got)
	}
	for _, p := range pub.calls[0] {
		switch p.ID {
		case "org/sub/b":
			if p.Title != "sub/b" {
				t.Fatalf("title fallback = %q", p.Title)
			}
		case "org/superseded":
			if !p.Archived || !strings.Contains(p.Markdown, "Deprecated") {
				t.Fatalf("superseded page %+v", p)
			}
		}
	}
}

func TestMirrorMarkerCollision(t *testing.T) {
	vault := t.TempDir()
	pubFact(t, vault, "a.md", promotedFront, "A")
	pubFact(t, vault, "b.md", promotedFront, "B")
	pub := &fakePublisher{}
	m := pubMirror(t, vault, pub, nil)
	m.pageKey = func(string) string { return "same-page" }
	r, err := m.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Created) != 1 || r.Created[0] != "org/a" || len(r.Collisions) != 1 || !strings.Contains(r.Collisions[0], "org/b (page same-page owned by org/a)") {
		t.Fatalf("report %+v", r)
	}
	// The owner keeps its page on later runs; the colliding fact stays skipped.
	r, err = m.Run(context.Background())
	if err != nil || r.Unchanged != 1 || len(r.Collisions) != 1 || pub.count() != 1 {
		t.Fatalf("second run %+v, %v", r, err)
	}
}

func TestMirrorDryRunAndErrors(t *testing.T) {
	vault := t.TempDir()
	pubFact(t, vault, "a.md", promotedFront, "A")

	t.Run("dry run publishes nothing and keeps state", func(t *testing.T) {
		pub := &fakePublisher{}
		var audited bool
		m := pubMirror(t, vault, pub, func(o *MirrorOptions) {
			o.Config.DryRun = true
			o.Audit = func(r PublishReport) { audited = r.DryRun && r.Changed() == 1 }
		})
		for i := 0; i < 2; i++ {
			r, err := m.Run(context.Background())
			if err != nil || len(r.Created) != 1 || !r.DryRun {
				t.Fatalf("run %d: %+v %v", i, r, err)
			}
		}
		if pub.count() != 0 || !audited || m.Status().Pages != 0 || !m.Status().DryRun {
			t.Fatalf("dry run leaked: calls=%d audited=%v status=%+v", pub.count(), audited, m.Status())
		}
	})

	t.Run("publish error keeps state", func(t *testing.T) {
		pub := &fakePublisher{err: errors.New("upstream down")}
		m := pubMirror(t, vault, pub, nil)
		r, err := m.Run(context.Background())
		if err == nil || r.Error != "upstream down" || m.Status().LastError != "upstream down" || m.Status().Pages != 0 || !m.Status().LastSuccess.IsZero() {
			t.Fatalf("run %+v %v status %+v", r, err, m.Status())
		}
		pub.err = nil
		if r, err := m.Run(context.Background()); err != nil || len(r.Created) != 1 {
			t.Fatalf("retry %+v %v", r, err)
		}
	})

	t.Run("vault dir error", func(t *testing.T) {
		m := pubMirror(t, vault, &fakePublisher{}, func(o *MirrorOptions) {
			o.VaultDir = func(string) (string, error) { return "", errors.New("no vault") }
		})
		if _, err := m.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "vault for layer org: no vault") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("missing and empty vault dirs", func(t *testing.T) {
		m := pubMirror(t, filepath.Join(vault, "absent"), &fakePublisher{}, func(o *MirrorOptions) {
			o.Config.Layers = []string{"org", "project"}
			o.VaultDir = func(l string) (string, error) {
				if l == "project" {
					return "", nil
				}
				return filepath.Join(vault, "absent"), nil
			}
		})
		if r, err := m.Run(context.Background()); err != nil || r.Changed() != 0 {
			t.Fatalf("run %+v %v", r, err)
		}
	})

	t.Run("unreadable fact", func(t *testing.T) {
		bad := t.TempDir()
		if err := os.Symlink(filepath.Join(bad, "nowhere"), filepath.Join(bad, "broken.md")); err != nil {
			t.Skip(err)
		}
		m := pubMirror(t, bad, &fakePublisher{}, nil)
		if _, err := m.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "reading org vault") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("walk error below root", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores directory permissions")
		}
		bad := t.TempDir()
		locked := filepath.Join(bad, "locked")
		if err := os.Mkdir(locked, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
		m := pubMirror(t, bad, &fakePublisher{}, nil)
		if _, err := m.Run(context.Background()); err == nil {
			t.Fatal("expected walk error")
		}
	})

	t.Run("in progress", func(t *testing.T) {
		m := pubMirror(t, vault, &fakePublisher{}, nil)
		m.runMu.Lock()
		defer m.runMu.Unlock()
		if _, err := m.Run(context.Background()); !errors.Is(err, ErrPublishInProgress) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestMirrorStatePersistence(t *testing.T) {
	vault := t.TempDir()
	pubFact(t, vault, "a.md", promotedFront, "A")
	statePath := filepath.Join(t.TempDir(), "state", "publish.json")
	setPath := func(o *MirrorOptions) { o.StatePath = statePath }

	pub := &fakePublisher{}
	if _, err := pubMirror(t, vault, pub, setPath).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	m := pubMirror(t, vault, pub, setPath)
	if st := m.Status(); st.Pages != 1 || st.LastReport == nil || st.Running {
		t.Fatalf("reloaded status %+v", st)
	}
	if r, err := m.Run(context.Background()); err != nil || r.Unchanged != 1 || pub.count() != 1 {
		t.Fatalf("restart re-sent unchanged page: %+v %v", r, err)
	}

	tests := []struct {
		name    string
		content string
	}{
		{"corrupt", "{not json"},
		{"no pages", `{"status":{"connector":"old"}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "publish.json")
			if err := os.WriteFile(p, []byte(tt.content), 0o644); err != nil {
				t.Fatal(err)
			}
			m := pubMirror(t, vault, &fakePublisher{}, func(o *MirrorOptions) { o.StatePath = p })
			if st := m.Status(); st.Pages != 0 || st.Connector != "docs" {
				t.Fatalf("status %+v", st)
			}
			if r, err := m.Run(context.Background()); err != nil || len(r.Created) != 1 {
				t.Fatalf("run %+v %v", r, err)
			}
		})
	}

	t.Run("unreadable and unwritable state", func(t *testing.T) {
		dir := t.TempDir() // a directory: reading fails, and writing below a file fails
		file := filepath.Join(t.TempDir(), "f")
		if err := os.WriteFile(file, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{dir, filepath.Join(file, "publish.json")} {
			m := pubMirror(t, vault, &fakePublisher{}, func(o *MirrorOptions) { o.StatePath = p })
			if r, err := m.Run(context.Background()); err != nil || len(r.Created) != 1 {
				t.Fatalf("%s: run %+v %v", p, r, err)
			}
		}
	})

	t.Run("no state path", func(t *testing.T) {
		m := pubMirror(t, vault, &fakePublisher{}, func(o *MirrorOptions) { o.StatePath = "" })
		if r, err := m.Run(context.Background()); err != nil || len(r.Created) != 1 {
			t.Fatalf("run %+v %v", r, err)
		}
	})
}

func TestMirrorStartAndTrigger(t *testing.T) {
	vault := t.TempDir()
	pubFact(t, vault, "a.md", promotedFront, "A")
	runs := make(chan PublishReport, 16)
	send := func(r PublishReport) {
		select {
		case runs <- r:
		default:
		}
	}
	pub := &fakePublisher{err: errors.New("down")}
	m := pubMirror(t, vault, pub, func(o *MirrorOptions) {
		o.Interval = time.Hour
		o.Audit = func(r PublishReport) { send(r) }
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Start(ctx); close(done) }()
	wait := func(what string) PublishReport {
		select {
		case r := <-runs:
			return r
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for %s", what)
		}
		return PublishReport{}
	}
	if r := wait("initial run"); r.Error != "down" {
		t.Fatalf("initial run %+v", r)
	}
	m.Trigger()
	m.Trigger() // coalesces with the pending request
	wait("triggered run")
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not stop on cancel")
	}

	tick := pubMirror(t, vault, &fakePublisher{}, func(o *MirrorOptions) {
		o.Interval = 10 * time.Millisecond
		o.Audit = func(r PublishReport) { send(r) }
	})
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	go tick.Start(ctx)
	wait("first tick run")
	wait("interval run")
}

func TestSharePointPublish(t *testing.T) {
	t.Setenv("SP_TOKEN", "tok")
	type put struct{ path, ctype, auth, body string }
	var mu sync.Mutex
	var puts []put
	var fail atomic.Bool
	stubGraph(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/sites/site1/drive":
			_, _ = w.Write([]byte(`{"id":"drv"}`))
		case r.Method == http.MethodPut && !fail.Load():
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			puts = append(puts, put{r.URL.Path, r.Header.Get("Content-Type"), r.Header.Get("Authorization"), string(b)})
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
		default:
			http.Error(w, "nope", http.StatusInternalServerError)
		}
	}))
	pages := []Page{
		{ID: "org/a", Markdown: "# A", Attrs: map[string]string{PublishKeyAttr: "hive-org-a"}},
		{ID: "org/b", Markdown: "# B"},
	}
	c, err := NewPublisher(nil, spCfg(map[string]string{"site_ids": "site1"}, Auth{Env: "SP_TOKEN"}), spDeps())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Publish(context.Background(), "/Hive/Know ledge/", pages); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := append([]put(nil), puts...)
	mu.Unlock()
	if len(got) != 2 || got[0].path != "/drives/drv/root:/Hive/Know ledge/hive-org-a.md:/content" ||
		got[1].path != "/drives/drv/root:/Hive/Know ledge/"+PublishKey("org/b")+".md:/content" ||
		got[0].body != "# A" || got[0].auth != "Bearer tok" || !strings.HasPrefix(got[0].ctype, "text/markdown") {
		t.Fatalf("puts %+v", got)
	}

	tests := []struct {
		name    string
		conn    *sharePointConnector
		root    string
		pages   []Page
		fail    bool
		wantErr string
	}{
		{"no pages is a no-op", c.(*sharePointConnector), "", nil, false, ""},
		{"bad root", c.(*sharePointConnector), "a/../b", pages, false, "must be a plain folder path"},
		{"empty root", c.(*sharePointConnector), "/", pages, false, "must be a plain folder path"},
		{"missing secret", &sharePointConnector{cfg: spCfg(map[string]string{"drive_ids": "d"}, Auth{Env: "SP_UNSET_X"}), http: spDeps().HTTP}, "R", pages, false, "SP_UNSET_X"},
		{"site lookup fails", &sharePointConnector{cfg: spCfg(map[string]string{"site_ids": "nosite"}, Auth{Env: "SP_TOKEN"}), http: spDeps().HTTP}, "R", pages, false, "site nosite"},
		{"no drives", &sharePointConnector{cfg: spCfg(nil, Auth{Env: "SP_TOKEN"}), http: spDeps().HTTP}, "R", pages, false, "no drive configured"},
		{"upload fails", c.(*sharePointConnector), "R", pages, true, "publishing org/a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fail.Store(tt.fail)
			defer fail.Store(false)
			err := tt.conn.Publish(context.Background(), tt.root, tt.pages)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}
