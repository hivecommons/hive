package connector

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/knowledge"
)

type fakeRepoWikiBackend struct {
	fakeGitBackend
	files    []repoWikiFile
	filesErr error
	gotDir   string
}

func (b *fakeRepoWikiBackend) Files(_ context.Context, dir string) ([]repoWikiFile, error) {
	b.gotDir = dir
	return b.files, b.filesErr
}

func stubRepoWiki(t *testing.T, backends map[string]*fakeRepoWikiBackend) *[]knowledge.GitSourceConfig {
	t.Helper()
	origBackend, origValidate := newRepoWikiBackend, validateGitURL
	t.Cleanup(func() { newRepoWikiBackend, validateGitURL = origBackend, origValidate })
	var got []knowledge.GitSourceConfig
	newRepoWikiBackend = func(cfg knowledge.GitSourceConfig, _ string, _ *slog.Logger) repoWikiBackend {
		got = append(got, cfg)
		return backends[cfg.URL]
	}
	validateGitURL = func(_ context.Context, raw string) error {
		if strings.Contains(raw, "internal") {
			return errors.New("private address")
		}
		return nil
	}
	return &got
}

func repoWikiCfg(scope map[string]string) ConnectorConfig {
	return ConnectorConfig{Name: "carried", Type: TypeRepoWiki, Enabled: true, Layer: "project", Scope: scope}
}

func TestRepoWikiValidate(t *testing.T) {
	stubRepoWiki(t, nil)
	tests := []struct {
		name    string
		scope   map[string]string
		wantErr string
	}{
		{"ok", map[string]string{"repos": "acme/app, acme/docs", "dir": "docs/hive"}, ""},
		{"no repos", map[string]string{}, "scope.repos is required"},
		{"bad repo", map[string]string{"repos": "acme"}, "owner/repo"},
		{"dup", map[string]string{"repos": "acme/app,ACME/app"}, "listed twice"},
		{"internal", map[string]string{"repos": "internal/app"}, "private address"},
		{"bad branch", map[string]string{"repos": "acme/app", "branch": "-x"}, "scope.branch"},
		{"abs dir", map[string]string{"repos": "acme/app", "dir": "../x"}, "scope.dir"},
		{"rooted dir", map[string]string{"repos": "acme/app", "dir": "/x"}, "scope.dir"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DefaultRegistry().New(repoWikiCfg(tt.scope), Deps{StateDir: t.TempDir()})
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

func TestRepoWikiNeedsDir(t *testing.T) {
	if _, err := newRepoWikiConnector(repoWikiCfg(nil), Deps{}); err == nil {
		t.Fatal("expected error without state dir")
	}
	c, err := newRepoWikiConnector(repoWikiCfg(nil), Deps{KnowledgeDir: t.TempDir()})
	if err != nil || c.Type() != TypeRepoWiki || !c.(FullLister).FullListing() {
		t.Fatalf("c = %v, err = %v", c, err)
	}
}

func TestRepoWikiSyncPages(t *testing.T) {
	appBackend := &fakeRepoWikiBackend{
		fakeGitBackend: fakeGitBackend{head: "abc123"},
		files: []repoWikiFile{
			{Name: "ops/runbook", Raw: "---\ntitle: \"Deploy runbook\"\ntags: ops, deploy\n---\n\nRun it.\n"},
			{Name: "arch", Raw: "# Architecture\n"},
			{Name: "old", Raw: "---\r\nstatus: deprecated\r\n---\r\nGone\r\n"},
			{Name: "wip", Raw: "---\n# comment\n\nstatus: draft\n---\nmaybe"},
		},
	}
	docsBackend := &fakeRepoWikiBackend{fakeGitBackend: fakeGitBackend{head: "def456"}}
	got := stubRepoWiki(t, map[string]*fakeRepoWikiBackend{
		"https://github.com/acme/app.git":  appBackend,
		"https://github.com/acme/docs.git": docsBackend,
	})
	c, err := DefaultRegistry().New(repoWikiCfg(map[string]string{"repos": "acme/app,acme/docs", "branch": "v5"}), Deps{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	run := func() ([]Page, Cursor, error) {
		var pages []Page
		cur, err := c.Sync(context.Background(), "", func(p Page) error { pages = append(pages, p); return nil })
		return pages, cur, err
	}
	pages, cur, err := run()
	if err != nil || cur != "acme/app@abc123,acme/docs@def456" {
		t.Fatalf("Sync = %q, %v", cur, err)
	}
	if len(*got) != 2 || (*got)[0].Subpath != ".hive/wiki" || (*got)[0].Branch != "v5" || (*got)[0].Layer != knowledge.LayerProject {
		t.Fatalf("GitSourceConfig = %+v", *got)
	}
	if appBackend.gotDir != ".hive/wiki" {
		t.Fatalf("dir = %q", appBackend.gotDir)
	}
	var ids []string
	for _, p := range pages {
		ids = append(ids, p.ID)
	}
	if strings.Join(ids, ",") != "acme/app/arch,acme/app/old,acme/app/ops/runbook,acme/app/wip" {
		t.Fatalf("ids = %v", ids)
	}
	arch, old, rb, wip := pages[0], pages[1], pages[2], pages[3]
	if arch.Title != "arch" || arch.Attrs["state"] != "approved" || arch.Archived || arch.Attrs["commit"] != "abc123" ||
		arch.Attrs["repo"] != "acme/app" || arch.Attrs["path"] != ".hive/wiki/arch.md" || arch.Attrs["branch"] != "v5" {
		t.Fatalf("arch = %+v", arch)
	}
	if arch.URL != "https://github.com/acme/app/blob/v5/.hive/wiki/arch.md" {
		t.Fatalf("url = %q", arch.URL)
	}
	if !old.Archived || old.Attrs["state"] != "deprecated" || old.Markdown != "Gone\n" {
		t.Fatalf("old = %+v", old)
	}
	if rb.Title != "Deploy runbook" || rb.Attrs["tags"] != "ops, deploy" || rb.Markdown != "Run it.\n" {
		t.Fatalf("runbook = %+v", rb)
	}
	if wip.Attrs["state"] != "draft" || wip.Archived {
		t.Fatalf("wip = %+v", wip)
	}

	// Second sync takes the Sync (not Init) path.
	if _, _, err := run(); err != nil || appBackend.syncCalls != 1 || appBackend.initCalls != 1 {
		t.Fatalf("err=%v init=%d sync=%d", err, appBackend.initCalls, appBackend.syncCalls)
	}
}

func TestRepoWikiSyncReportsInvalidFiles(t *testing.T) {
	b := &fakeRepoWikiBackend{
		fakeGitBackend: fakeGitBackend{head: "abc"},
		files: []repoWikiFile{
			{Name: "good", Raw: "ok"},
			{Name: "unclosed", Raw: "---\ntitle: x\nbody"},
			{Name: "badline", Raw: "---\nnot a pair\n---\n"},
			{Name: "badstatus", Raw: "---\nstatus: bogus\n---\n"},
		},
	}
	stubRepoWiki(t, map[string]*fakeRepoWikiBackend{"https://github.com/acme/app.git": b})
	c, _ := DefaultRegistry().New(repoWikiCfg(map[string]string{"repos": "acme/app", "dir": "kb/"}), Deps{StateDir: t.TempDir()})
	var ids []string
	cur, err := c.Sync(context.Background(), "", func(p Page) error { ids = append(ids, p.ID); return nil })
	if cur != "acme/app@abc" || len(ids) != 1 || ids[0] != "acme/app/good" {
		t.Fatalf("cur=%q ids=%v", cur, ids)
	}
	for _, want := range []string{"3 invalid", "kb/unclosed.md", "not closed", "kb/badline.md", "key: value", "kb/badstatus.md", `"bogus"`} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v, want %q", err, want)
		}
	}
}

func TestRepoWikiSyncErrors(t *testing.T) {
	cases := map[string]*fakeRepoWikiBackend{
		"init":  {fakeGitBackend: fakeGitBackend{initErr: errors.New("clone failed")}},
		"files": {filesErr: errors.New("walk failed")},
	}
	for name, b := range cases {
		t.Run(name, func(t *testing.T) {
			stubRepoWiki(t, map[string]*fakeRepoWikiBackend{"https://github.com/acme/app.git": b})
			c, _ := DefaultRegistry().New(repoWikiCfg(map[string]string{"repos": "acme/app"}), Deps{StateDir: t.TempDir()})
			cur, err := c.Sync(context.Background(), "keep", func(Page) error { return nil })
			if err == nil || cur != "keep" || !strings.Contains(err.Error(), "repo-wiki acme/app") {
				t.Fatalf("cur=%q err=%v", cur, err)
			}
		})
	}
}

func TestRepoWikiEmitError(t *testing.T) {
	b := &fakeRepoWikiBackend{files: []repoWikiFile{{Name: "a", Raw: "x"}}}
	stubRepoWiki(t, map[string]*fakeRepoWikiBackend{"https://github.com/acme/app.git": b})
	c, _ := DefaultRegistry().New(repoWikiCfg(map[string]string{"repos": "acme/app"}), Deps{StateDir: t.TempDir()})
	if _, err := c.Sync(context.Background(), "", func(Page) error { return errors.New("boom") }); err == nil || err.Error() != "boom" {
		t.Fatalf("err = %v", err)
	}
}

func TestParseRepoWikiFrontMatter(t *testing.T) {
	fm, body, err := parseRepoWikiFrontMatter("\ufeffplain")
	if err != nil || len(fm) != 0 || body != "plain" {
		t.Fatalf("%v %q %v", fm, body, err)
	}
	if _, _, err := parseRepoWikiFrontMatter("---\n: v\n---\n"); err == nil {
		t.Fatal("expected error for empty key")
	}
	if _, _, err := parseRepoWikiFrontMatter("---\nbad key: v\n---\n"); err == nil {
		t.Fatal("expected error for spaced key")
	}
}

func TestRealRepoWikiBackendFiles(t *testing.T) {
	base := t.TempDir()
	src := knowledge.NewGitSource(knowledge.GitSourceConfig{Name: "x", URL: "https://github.com/acme/app.git", Branch: "main"}, base, slog.Default())
	b := realRepoWikiBackend{realGitBackend{src: src}}
	// Missing directory: no files, no error.
	files, err := b.Files(context.Background(), ".hive/wiki")
	if err != nil || len(files) != 0 {
		t.Fatalf("files=%v err=%v", files, err)
	}
	root := filepath.Join(src.CloneDir(), ".hive", "wiki")
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"a.md": "A", "sub/b.MD": "B", "c.txt": "C"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	files, err = b.Files(context.Background(), ".hive/wiki")
	if err != nil || len(files) != 2 {
		t.Fatalf("files=%v err=%v", files, err)
	}
	names := map[string]string{}
	for _, f := range files {
		names[f.Name] = f.Raw
	}
	if names["a"] != "A" || names["sub/b"] != "B" {
		t.Fatalf("names = %v", names)
	}
	// A path component that is a file makes the walk fail.
	if _, err := b.Files(context.Background(), ".hive/wiki/a.md/x"); err == nil {
		t.Fatal("expected walk error")
	}
}
