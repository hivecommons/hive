package connector

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/knowledge"
)

type fakeGitBackend struct {
	ready     bool
	initErr   error
	syncErr   error
	pages     []knowledge.Fact
	head      string
	initCalls int
	syncCalls int
}

func (b *fakeGitBackend) Init(context.Context) error {
	b.initCalls++
	if b.initErr != nil {
		return b.initErr
	}
	b.ready = true
	return nil
}

func (b *fakeGitBackend) Sync(context.Context) error {
	b.syncCalls++
	return b.syncErr
}

func (b *fakeGitBackend) Ready() bool                 { return b.ready }
func (b *fakeGitBackend) Pages() []knowledge.Fact     { return b.pages }
func (b *fakeGitBackend) Head(context.Context) string { return b.head }

func stubGit(t *testing.T, backend *fakeGitBackend) *knowledge.GitSourceConfig {
	t.Helper()
	origBackend, origValidate := newGitBackend, validateGitURL
	t.Cleanup(func() { newGitBackend, validateGitURL = origBackend, origValidate })
	var got knowledge.GitSourceConfig
	newGitBackend = func(cfg knowledge.GitSourceConfig, _ string, _ *slog.Logger) gitBackend {
		got = cfg
		return backend
	}
	validateGitURL = func(_ context.Context, raw string) error {
		if strings.Contains(raw, "internal") {
			return errors.New("git URL host resolves to a private/internal address")
		}
		return nil
	}
	return &got
}

func gitCfg(scope map[string]string) ConnectorConfig {
	return ConnectorConfig{Name: "docs", Type: TypeGit, Enabled: true, Layer: "project", Scope: scope}
}

func TestGitConnectorValidate(t *testing.T) {
	stubGit(t, &fakeGitBackend{})
	reg := DefaultRegistry()
	deps := Deps{StateDir: t.TempDir()}
	tests := []struct {
		name    string
		scope   map[string]string
		wantErr string
	}{
		{"ok", map[string]string{"url": "https://github.com/acme/docs.git"}, ""},
		{"ok subpath branch", map[string]string{"url": "https://github.com/acme/docs.git", "branch": "v5", "subpath": "docs/kb"}, ""},
		{"missing url", map[string]string{}, "scope.url is required"},
		{"ssrf", map[string]string{"url": "https://internal.example/x.git"}, "scope.url: git URL host resolves to a private"},
		{"bad branch", map[string]string{"url": "https://github.com/acme/docs.git", "branch": "--upload-pack=x"}, "scope.branch"},
		{"absolute subpath", map[string]string{"url": "https://github.com/acme/docs.git", "subpath": "/etc"}, "scope.subpath"},
		{"traversal subpath", map[string]string{"url": "https://github.com/acme/docs.git", "subpath": "a/../../b"}, "scope.subpath"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := reg.New(gitCfg(tt.scope), deps)
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
	if _, err := reg.New(gitCfg(map[string]string{"url": "https://github.com/acme/docs.git"}), Deps{}); err == nil || !strings.Contains(err.Error(), "state or knowledge directory") {
		t.Fatalf("no dirs err = %v", err)
	}
	if _, err := reg.New(gitCfg(map[string]string{"url": "https://github.com/acme/docs.git"}), Deps{KnowledgeDir: t.TempDir()}); err != nil {
		t.Fatalf("KnowledgeDir fallback: %v", err)
	}
}

func TestGitConnectorSync(t *testing.T) {
	backend := &fakeGitBackend{
		head: "abc123",
		pages: []knowledge.Fact{
			{Slug: "zeta", Title: "Zeta", Body: "z body"},
			{Slug: "alpha", Title: "Alpha", Body: "a body", Status: "Deprecated"},
		},
	}
	got := stubGit(t, backend)
	c, err := DefaultRegistry().New(gitCfg(map[string]string{"url": " https://github.com/acme/docs.git ", "subpath": "kb"}), Deps{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if fl, ok := c.(FullLister); !ok || !fl.FullListing() {
		t.Fatal("git connector must be a full lister")
	}

	var pages []Page
	emit := func(p Page) error { pages = append(pages, p); return nil }
	cur, err := c.Sync(context.Background(), "", emit)
	if err != nil || cur != "abc123" {
		t.Fatalf("Sync = %q, %v", cur, err)
	}
	if got.URL != "https://github.com/acme/docs.git" || got.Branch != "main" || got.Subpath != "kb" || got.Layer != knowledge.LayerProject || got.Name != "docs" {
		t.Fatalf("GitSourceConfig = %+v", *got)
	}
	if len(pages) != 2 || pages[0].ID != "alpha" || !pages[0].Archived || pages[1].ID != "zeta" || pages[1].Archived ||
		pages[1].Markdown != "z body" || pages[1].Title != "Zeta" || pages[1].URL != "https://github.com/acme/docs.git" ||
		len(pages[1].Path) != 1 || pages[1].Path[0] != "kb" || pages[1].Attrs["branch"] != "main" {
		t.Fatalf("pages = %+v", pages)
	}
	if backend.initCalls != 1 || backend.syncCalls != 0 {
		t.Fatalf("first sync should Init: init=%d sync=%d", backend.initCalls, backend.syncCalls)
	}

	// Second sync pulls instead of re-cloning.
	backend.head = "def456"
	if cur, err := c.Sync(context.Background(), cur, func(Page) error { return nil }); err != nil || cur != "def456" {
		t.Fatalf("second Sync = %q, %v", cur, err)
	}
	if backend.initCalls != 1 || backend.syncCalls != 1 {
		t.Fatalf("second sync should pull: init=%d sync=%d", backend.initCalls, backend.syncCalls)
	}

	backend.syncErr = errors.New("pull failed")
	if cur, err := c.Sync(context.Background(), "def456", emit); err == nil || cur != "def456" {
		t.Fatalf("pull failure = %q, %v", cur, err)
	}
	backend.syncErr = nil
	if _, err := c.Sync(context.Background(), "x", func(Page) error { return errors.New("stop") }); err == nil || err.Error() != "stop" {
		t.Fatalf("emit error = %v", err)
	}
}

func TestGitConnectorInitError(t *testing.T) {
	backend := &fakeGitBackend{initErr: errors.New("clone failed")}
	stubGit(t, backend)
	c, err := DefaultRegistry().New(gitCfg(map[string]string{"url": "https://github.com/acme/docs.git"}), Deps{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if cur, err := c.Sync(context.Background(), "prev", func(Page) error { return nil }); err == nil || cur != "prev" {
		t.Fatalf("Sync = %q, %v", cur, err)
	}
}

// The git connector through the Syncer writes git-<name>-<slug> facts.
func TestGitConnectorThroughSyncer(t *testing.T) {
	backend := &fakeGitBackend{head: "h1", pages: []knowledge.Fact{{Slug: "setup", Title: "Setup", Body: "Install it."}}}
	stubGit(t, backend)
	vault := t.TempDir()
	s, err := NewSyncer([]ConnectorConfig{gitCfg(map[string]string{"url": "https://github.com/acme/docs.git"})}, SyncerOptions{
		Deps:     Deps{StateDir: t.TempDir()},
		VaultDir: func(string) (string, error) { return vault, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	st, err := s.SyncNow(context.Background(), "docs")
	if err != nil || st.Facts != 1 || st.Cursor != "h1" {
		t.Fatalf("SyncNow = %+v, %v", st, err)
	}
	fm := readFact(t, vault, "git-docs-setup")
	if fm["source"] != TypeGit || fm["connector"] != "docs" || fm["source_id"] != "setup" || fm["title"] != "Setup" {
		t.Fatalf("front-matter = %v", fm)
	}
	// Page removed from the repo: tombstoned on the next (full) sync.
	backend.pages = nil
	if st, _ := s.SyncNow(context.Background(), "docs"); st.Facts != 0 || st.Deprecated != 1 {
		t.Fatalf("after removal = %+v", st)
	}
}

// realGitBackend delegates to knowledge.GitSource; an invalid URL exercises
// every method without network access.
func TestRealGitBackendDelegates(t *testing.T) {
	b := newGitBackend(knowledge.GitSourceConfig{Name: "x", Branch: "main"}, t.TempDir(), slog.Default())
	if err := b.Init(context.Background()); err == nil || !strings.Contains(err.Error(), "invalid url") {
		t.Fatalf("Init = %v", err)
	}
	if b.Ready() {
		t.Fatal("not ready after failed init")
	}
	if b.Pages() != nil {
		t.Fatal("pages before init")
	}
	if b.Head(context.Background()) != "" {
		t.Fatal("head of a missing clone")
	}
	if err := b.Sync(context.Background()); err == nil {
		t.Fatal("Sync on a missing clone succeeded")
	}
}
