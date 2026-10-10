package connector

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/knowledge"
)

type fakeWikiBackend struct {
	fakeGitBackend
	files []wikiFile
}

func (b *fakeWikiBackend) Files(context.Context) ([]wikiFile, error) { return b.files, nil }

func stubWiki(t *testing.T, backends map[string]*fakeWikiBackend) *[]knowledge.GitSourceConfig {
	t.Helper()
	origBackend, origValidate := newWikiBackend, validateGitURL
	t.Cleanup(func() { newWikiBackend, validateGitURL = origBackend, origValidate })
	var got []knowledge.GitSourceConfig
	newWikiBackend = func(cfg knowledge.GitSourceConfig, _ string, _ *slog.Logger) wikiBackend {
		got = append(got, cfg)
		return backends[cfg.URL]
	}
	validateGitURL = func(context.Context, string) error { return nil }
	return &got
}

func wikiCfg(scope map[string]string) ConnectorConfig {
	return ConnectorConfig{Name: "wiki", Type: TypeGitHubWiki, Enabled: true, Layer: "project", Scope: scope}
}

func TestGitHubWikiValidate(t *testing.T) {
	stubWiki(t, nil)
	reg := DefaultRegistry()
	tests := []struct {
		name    string
		scope   map[string]string
		wantErr string
	}{
		{"ok", map[string]string{"repos": "acme/docs"}, ""},
		{"ok multiple", map[string]string{"repos": "acme/docs, acme/app", "branch": "main"}, ""},
		{"missing", map[string]string{}, "scope.repos is required"},
		{"not owner/repo", map[string]string{"repos": "docs"}, "owner/repo"},
		{"url rejected", map[string]string{"repos": "https://evil.example/x"}, "owner/repo"},
		{"duplicate", map[string]string{"repos": "acme/docs,Acme/Docs"}, "listed twice"},
		{"bad branch", map[string]string{"repos": "acme/docs", "branch": "--upload-pack=x"}, "scope.branch"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := reg.New(wikiCfg(tt.scope), Deps{StateDir: t.TempDir()})
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

func TestGitHubWikiSyncOrdering(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	backend := &fakeWikiBackend{
		fakeGitBackend: fakeGitBackend{head: "abc123"},
		files: []wikiFile{
			{Name: "Zebra", Body: "z"},
			{Name: "Install", Body: "install", UpdatedAt: ts},
			{Name: "_Footer", Body: "footer"},
			{Name: "Alpha", Body: "a"},
			{Name: "Usage", Body: "usage"},
			{Name: "Home", Body: "welcome"},
			{Name: "_Sidebar", Body: "# Guide\n- [Usage](Usage)\n  - [[Install steps|Install]]\n- [Ext](https://example.com/x)\n- [Missing](Nope)\n"},
		},
	}
	got := stubWiki(t, map[string]*fakeWikiBackend{"https://github.com/acme/docs.wiki.git": backend})
	c, err := DefaultRegistry().New(wikiCfg(map[string]string{"repos": "acme/docs"}), Deps{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	var pages []Page
	cur, err := c.Sync(context.Background(), "", func(p Page) error { pages = append(pages, p); return nil })
	if err != nil || cur != "acme/docs@abc123" {
		t.Fatalf("Sync = %q, %v", cur, err)
	}
	if len(*got) != 1 || (*got)[0].Branch != "master" || (*got)[0].Layer != knowledge.LayerProject {
		t.Fatalf("GitSourceConfig = %+v", *got)
	}
	var ids []string
	for _, p := range pages {
		ids = append(ids, p.ID)
	}
	want := []string{"acme/docs/Home", "acme/docs/Usage", "acme/docs/Install", "acme/docs/Alpha", "acme/docs/Zebra"}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("order = %v, want %v", ids, want)
	}
	if pages[0].Attrs["root"] != "true" || pages[1].Attrs["root"] != "" {
		t.Fatalf("root attr: %+v %+v", pages[0].Attrs, pages[1].Attrs)
	}
	inst := pages[2]
	if !reflect.DeepEqual(inst.Path, []string{"acme/docs", "Guide", "Usage"}) || !inst.UpdatedAt.Equal(ts) ||
		inst.URL != "https://github.com/acme/docs/wiki/Install" || inst.Markdown != "install" {
		t.Fatalf("Install page = %+v", inst)
	}
	if backend.initCalls != 1 {
		t.Fatalf("initCalls = %d", backend.initCalls)
	}
	if _, err := c.Sync(context.Background(), cur, func(Page) error { return nil }); err != nil || backend.syncCalls != 1 {
		t.Fatalf("second sync: %v, syncCalls=%d", err, backend.syncCalls)
	}
}

func TestGitHubWikiMissingWiki(t *testing.T) {
	backend := &fakeWikiBackend{fakeGitBackend: fakeGitBackend{
		initErr: errors.New("git clone: fatal: repository 'https://github.com/acme/docs.wiki.git/' not found"),
	}}
	stubWiki(t, map[string]*fakeWikiBackend{"https://github.com/acme/docs.wiki.git": backend})
	c, err := DefaultRegistry().New(wikiCfg(map[string]string{"repos": "acme/docs"}), Deps{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	cur, err := c.Sync(context.Background(), "prev", func(Page) error { return nil })
	if err == nil || cur != "prev" || !strings.Contains(err.Error(), "acme/docs not found") || !strings.Contains(err.Error(), "no wiki") {
		t.Fatalf("Sync = %q, %v", cur, err)
	}
}

func TestParseSidebar(t *testing.T) {
	got := parseSidebar("## Intro\n* [A](A.md)\n* [B](B#frag)\n  * [[C d]]\n* [A](A)\n")
	want := []sidebarEntry{
		{page: "A", path: []string{"Intro"}},
		{page: "B", path: []string{"Intro"}},
		{page: "C-d", path: []string{"Intro", "B"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseSidebar = %+v, want %+v", got, want)
	}
}
