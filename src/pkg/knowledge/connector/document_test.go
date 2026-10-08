package connector

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/knowledge"
)

func docCfg(scope map[string]string) ConnectorConfig {
	return ConnectorConfig{Name: "guide", Type: TypeDocument, Enabled: true, Layer: "project", Scope: scope}
}

func TestDocumentConnectorValidate(t *testing.T) {
	reg := DefaultRegistry()
	tests := []struct {
		name    string
		scope   map[string]string
		wantErr string
	}{
		{"url", map[string]string{"url": "https://example.com/guide.pdf"}, ""},
		{"file", map[string]string{"file_path": "/data/knowledge/guide.md"}, ""},
		{"context7", map[string]string{"context7_id": "/vercel/next.js"}, ""},
		{"none", map[string]string{}, "exactly one of url, file_path or context7_id"},
		{"two", map[string]string{"url": "https://example.com/a", "file_path": "/data/knowledge/a"}, "exactly one"},
		{"bad scheme", map[string]string{"url": "file:///etc/passwd"}, "scope.url must be http(s)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := reg.New(docCfg(tt.scope), Deps{})
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

type fetchCall struct {
	cfg          knowledge.DocSourceConfig
	knowledgeDir string
	key          string
}

func stubFetch(t *testing.T, facts []knowledge.ExtractedFact, err error) *[]fetchCall {
	t.Helper()
	orig := fetchDocumentFacts
	t.Cleanup(func() { fetchDocumentFacts = orig })
	var calls []fetchCall
	fetchDocumentFacts = func(_ context.Context, cfg knowledge.DocSourceConfig, dir, key string, _ *slog.Logger) ([]knowledge.ExtractedFact, string, error) {
		calls = append(calls, fetchCall{cfg, dir, key})
		return facts, cfg.Name, err
	}
	return &calls
}

func TestDocumentConnectorSync(t *testing.T) {
	when := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	facts := []knowledge.ExtractedFact{
		{Title: "Summary: Guide", Body: "intro", SourceDate: when},
		{Title: "Guide", Body: "intro and more", SourceDate: when},
	}
	calls := stubFetch(t, facts, nil)
	c, err := DefaultRegistry().New(docCfg(map[string]string{"context7_id": "/acme/lib"}), Deps{KnowledgeDir: "/kb"})
	if err != nil {
		t.Fatal(err)
	}
	if fl, ok := c.(FullLister); !ok || !fl.FullListing() {
		t.Fatal("document connector must be a full lister")
	}
	var pages []Page
	cur, err := c.Sync(context.Background(), "", func(p Page) error { pages = append(pages, p); return nil })
	if err != nil || len(cur) != 16 {
		t.Fatalf("Sync = %q, %v", cur, err)
	}
	if len(pages) != 2 || pages[0].ID != "000" || pages[1].ID != "001" || pages[1].Title != "Guide" ||
		pages[1].Markdown != "intro and more" || pages[0].URL != "context7:///acme/lib" || !pages[0].UpdatedAt.Equal(when) {
		t.Fatalf("pages = %+v", pages)
	}
	if got := (*calls)[0]; got.cfg.Context7ID != "/acme/lib" || got.cfg.Name != "guide" || got.knowledgeDir != "/kb" || got.key != "" {
		t.Fatalf("fetch call = %+v", got)
	}
	again, _ := c.Sync(context.Background(), cur, func(Page) error { return nil })
	if again != cur {
		t.Fatalf("cursor not deterministic: %q vs %q", cur, again)
	}
	if _, err := c.Sync(context.Background(), "", func(Page) error { return errors.New("stop") }); err == nil || err.Error() != "stop" {
		t.Fatalf("emit err = %v", err)
	}
}

func TestDocumentConnectorAuthAndErrors(t *testing.T) {
	calls := stubFetch(t, nil, nil)
	t.Setenv("HIVE_TEST_CONTEXT7_KEY", "c7-key")
	cfg := docCfg(map[string]string{"context7_id": "/acme/lib"})
	cfg.Auth = Auth{Env: "HIVE_TEST_CONTEXT7_KEY"}
	c, err := DefaultRegistry().New(cfg, Deps{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Sync(context.Background(), "", func(Page) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if (*calls)[0].key != "c7-key" {
		t.Fatalf("api key not passed: %+v", (*calls)[0])
	}

	cfg.Auth = Auth{Env: "HIVE_TEST_CONTEXT7_UNSET_KEY"}
	c, _ = DefaultRegistry().New(cfg, Deps{})
	if cur, err := c.Sync(context.Background(), "prev", func(Page) error { return nil }); err == nil || cur != "prev" {
		t.Fatalf("unset auth = %q, %v", cur, err)
	}

	stubFetch(t, nil, errors.New("HTTP 500"))
	c, _ = DefaultRegistry().New(docCfg(map[string]string{"file_path": "/kb/x.md"}), Deps{})
	if cur, err := c.Sync(context.Background(), "prev", func(Page) error { return nil }); err == nil || cur != "prev" {
		t.Fatalf("fetch error = %q, %v", cur, err)
	}

	// No dashboard handler sits in front of a connector, so the pre-fetch
	// SSRF check happens in Sync with the shared guarded client.
	c, _ = DefaultRegistry().New(docCfg(map[string]string{"url": "http://127.0.0.1/admin"}), Deps{})
	if _, err := c.Sync(context.Background(), "", func(Page) error { return nil }); err == nil || !strings.Contains(err.Error(), "private/internal") {
		t.Fatalf("SSRF err = %v", err)
	}
}

// Parity: the document connector produces the same fact titles and bodies,
// in the same order, as the legacy knowledge.documents import of the same
// file — it runs the same fetch/parse/chunk code path.
func TestDocumentConnectorParityWithLegacyImport(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "guide.md")
	text := "# Contributor Guide\n\nAlways sign commits with -s.\n\n## Testing\n\nCI is the only verdict. Do not run the suite locally.\n\n" +
		strings.Repeat("Long paragraph about reviews. ", 80)
	if err := os.WriteFile(src, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}

	legacyVault := filepath.Join(base, "legacy-vault")
	ds := knowledge.NewDocumentSource(knowledge.DocSourceConfig{Name: "guide", FilePath: src, Layer: knowledge.LayerProject}, base, legacyVault, nil, slog.Default(), "")
	meta, err := ds.Import(context.Background())
	if err != nil {
		t.Fatalf("legacy import: %v", err)
	}

	connVault := filepath.Join(base, "connector-vault")
	s, err := NewSyncer([]ConnectorConfig{docCfg(map[string]string{"file_path": src})}, SyncerOptions{
		Deps:     Deps{KnowledgeDir: base},
		VaultDir: func(string) (string, error) { return connVault, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	st, err := s.SyncNow(context.Background(), "guide")
	if err != nil {
		t.Fatalf("connector sync: %v", err)
	}
	if st.Facts != meta.FactCount || len(meta.FactSlugs) < 2 {
		t.Fatalf("fact count: connector %d, legacy %d", st.Facts, meta.FactCount)
	}
	for i, legacySlug := range meta.FactSlugs {
		lt, lb := readTitleBody(t, legacyVault, legacySlug)
		ct, cb := readTitleBody(t, connVault, Slug(TypeDocument, "guide", strings.TrimPrefix(legacySlug, "doc-guide-")))
		if lt != ct || lb != cb {
			t.Errorf("fact %d differs:\nlegacy    %q / %q\nconnector %q / %q", i, lt, lb, ct, cb)
		}
	}
}

func readTitleBody(t *testing.T, dir, slug string) (string, string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, slug+".md"))
	if err != nil {
		t.Fatalf("reading %s: %v", slug, err)
	}
	content := string(data)
	end := strings.Index(content[4:], "\n---")
	if end < 0 {
		t.Fatalf("%s: no front-matter", slug)
	}
	return frontMatter(content)["title"], strings.TrimSpace(content[4+end+4:])
}
