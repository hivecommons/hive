package knowledge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/beads"
)

// apiWithVault builds a KnowledgeAPI with a single connected file vault so the
// document-import methods have a place to write facts.
func apiWithVault(t *testing.T) (*KnowledgeAPI, string) {
	t.Helper()
	vaultDir := t.TempDir()
	api := &KnowledgeAPI{
		config: KnowledgeConfig{Enabled: true},
		logger: covLogger(),
	}
	if err := api.ConnectVault(vaultDir, "vault"); err != nil {
		t.Fatal(err)
	}
	return api, vaultDir
}

func TestKnowledgeAPI_SetContext7APIKey(t *testing.T) {
	api := &KnowledgeAPI{logger: covLogger()}
	api.SetContext7APIKey("secret")
	if api.context7APIKey != "secret" {
		t.Errorf("key = %q", api.context7APIKey)
	}
}

func TestKnowledgeAPI_GraphStoreSetGet(t *testing.T) {
	api := &KnowledgeAPI{logger: covLogger()}
	if api.GraphStore() != nil {
		t.Error("expected nil graph store initially")
	}
	gs := newTestGraphStore(t)
	api.SetGraphStore(gs)
	if api.GraphStore() != gs {
		t.Error("expected graph store to be set")
	}
}

func TestKnowledgeAPI_WireContext7Suggester_NoKey(t *testing.T) {
	api := &KnowledgeAPI{logger: covLogger()}
	p := NewPrimer(nil, PrimerConfig{}, covLogger())
	// No API key -> returns early, suggester not wired.
	api.WireContext7Suggester(p)
	if p.context7Suggest != nil {
		t.Error("suggester should not be wired without an API key")
	}
}

func TestKnowledgeAPI_WireContext7Suggester_AlreadyImported(t *testing.T) {
	api, _ := apiWithVault(t)
	api.context7APIKey = "key"
	// Register a doc source so the suggester short-circuits on a title match.
	api.docSources = append(api.docSources, &DocumentSource{
		metadata: DocMetadata{Title: "React"},
		logger:   covLogger(),
	})
	p := NewPrimer(nil, PrimerConfig{}, covLogger())
	api.WireContext7Suggester(p)
	if p.context7Suggest == nil {
		t.Fatal("suggester should be wired when key present")
	}
	// Keyword matches an existing doc title -> empty hint, no network.
	if hint := p.context7Suggest(context.Background(), "react"); hint != "" {
		t.Errorf("expected empty hint for already-imported doc, got %q", hint)
	}
}

func TestKnowledgeAPI_ListGetReimportDeleteDocument(t *testing.T) {
	// ImportDocument itself writes an artifact under the hardcoded
	// localKnowledgeDir (/data/knowledge), which is read-only in test
	// environments, so we inject a DocumentSource whose storage lives in a
	// temp dir and exercise the list/get/reimport/delete methods directly.
	// The underlying DocumentSource.Import path is covered by docsource tests.
	api, vaultDir := apiWithVault(t)

	storageBase := t.TempDir()
	srcFile := filepath.Join(storageBase, "doc.txt")
	if err := os.WriteFile(srcFile, []byte("Para one.\n\nPara two."), 0o644); err != nil {
		t.Fatal(err)
	}

	ds := NewDocumentSource(DocSourceConfig{
		Name:     "my-import",
		FilePath: srcFile,
		Layer:    LayerProject,
	}, storageBase, vaultDir, nil, covLogger(), "")
	meta, err := ds.Import(context.Background())
	if err != nil {
		t.Fatalf("ds.Import: %v", err)
	}
	api.docSources = append(api.docSources, ds)

	// docVaultDir should resolve to the connected vault.
	if got := api.docVaultDir(); got != vaultDir {
		t.Errorf("docVaultDir = %q, want %q", got, vaultDir)
	}

	docs := api.ListDocuments()
	if len(docs) != 1 {
		t.Fatalf("ListDocuments = %d, want 1", len(docs))
	}

	got, err := api.GetDocument(meta.Slug)
	if err != nil {
		t.Fatalf("GetDocument: %v", err)
	}
	if got.Slug != meta.Slug {
		t.Errorf("GetDocument slug = %q", got.Slug)
	}
	if _, err := api.GetDocument("nonexistent"); err == nil {
		t.Error("expected error for missing document")
	}

	// Reimport re-fetches and replaces.
	if _, err := api.ReimportDocument(context.Background(), meta.Slug); err != nil {
		t.Fatalf("ReimportDocument: %v", err)
	}
	if _, err := api.ReimportDocument(context.Background(), "nope"); err == nil {
		t.Error("expected error reimporting missing document")
	}

	// Delete it.
	if err := api.DeleteDocument(meta.Slug); err != nil {
		t.Fatalf("DeleteDocument: %v", err)
	}
	if err := api.DeleteDocument(meta.Slug); err == nil {
		t.Error("expected error deleting already-deleted document")
	}
}

func TestKnowledgeAPI_DocVaultDir_Fallback(t *testing.T) {
	// No vaults connected -> fallback to the documents-vault under the data dir.
	api := &KnowledgeAPI{logger: covLogger()}
	got := api.docVaultDir()
	if !strings.HasSuffix(got, "documents-vault") {
		t.Errorf("expected documents-vault fallback, got %q", got)
	}
}

func TestKnowledgeAPI_CleanupOrphanedDocFacts(t *testing.T) {
	api, vaultDir := apiWithVault(t)

	// Create an orphaned doc fact (doc- prefix, not owned by any doc source).
	orphan := filepath.Join(vaultDir, "doc-orphan-000.md")
	if err := os.WriteFile(orphan, []byte("---\ntitle: Orphan\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A non-doc file that should be left alone.
	keep := filepath.Join(vaultDir, "regular.md")
	if err := os.WriteFile(keep, []byte("---\ntitle: Keep\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}

	removed, err := api.CleanupOrphanedDocFacts()
	if err != nil {
		t.Fatalf("CleanupOrphanedDocFacts: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Error("orphan fact should have been removed")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Error("regular fact should have been kept")
	}
}

func TestKnowledgeAPI_SearchContext7Libraries_Error(t *testing.T) {
	api := &KnowledgeAPI{logger: covLogger()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Cancelled context -> the underlying HTTP call fails.
	if _, err := api.SearchContext7Libraries(ctx, "react"); err == nil {
		t.Error("expected error from SearchContext7Libraries with cancelled ctx")
	}
}

func TestKnowledgeAPI_ObsidianSync_PathTraversal(t *testing.T) {
	api := &KnowledgeAPI{logger: covLogger()}
	_, err := api.ObsidianSync(context.Background(), ObsidianSyncRequest{
		Filename: "../escape.md",
		Content:  "x",
	})
	if err == nil || !strings.Contains(err.Error(), "path traversal") {
		t.Fatalf("expected path traversal error, got %v", err)
	}
}

// --- PREnricher via a mock GitHub server ---

func mockGHClient(t *testing.T, handler http.HandlerFunc) (*gh.Client, func()) {
	t.Helper()
	srv := httptest.NewServer(handler)
	c := gh.NewClient(srv.Client())
	base, err := c.WithEnterpriseURLs(srv.URL+"/", srv.URL+"/")
	if err != nil {
		srv.Close()
		t.Fatal(err)
	}
	return base, srv.Close
}

func TestPREnricher_EnrichBody(t *testing.T) {
	client, closeSrv := mockGHClient(t, func(w http.ResponseWriter, r *http.Request) {
		pr := map[string]any{
			"number":        7,
			"title":         "Fix the thing",
			"body":          "This PR fixes an important bug.",
			"changed_files": 3,
			"additions":     10,
			"deletions":     2,
			"labels":        []map[string]any{{"name": "bug"}, {"name": "priority"}},
			"merged_at":     "2025-01-02T00:00:00Z",
		}
		json.NewEncoder(w).Encode(pr)
	})
	defer closeSrv()

	e := NewPREnricher(client, "myorg", []string{"repo1"}, covLogger())

	// repo#num form.
	b := &beads.Bead{ID: "b1", ExternalRef: "myorg/repo1#7", Metadata: map[string]any{}}
	body := e.EnrichBody(context.Background(), b, "scanner")
	if !strings.Contains(body, "fixes an important bug") {
		t.Errorf("body missing PR description: %q", body)
	}
	if !strings.Contains(body, "Changed: 3 files") {
		t.Errorf("body missing change stats: %q", body)
	}
	if !strings.Contains(body, "Labels: bug, priority") {
		t.Errorf("body missing labels: %q", body)
	}

	// Second call hits the cache.
	_ = e.EnrichBody(context.Background(), b, "scanner")

	// gh-<n> form resolves via fetchPRFromRepos across configured repos.
	b2 := &beads.Bead{ID: "b2", ExternalRef: "gh-7", Metadata: map[string]any{}}
	if got := e.EnrichBody(context.Background(), b2, "scanner"); got == "" {
		t.Error("expected non-empty body for gh-<n> ref")
	}

	// Unparseable ref -> empty.
	b3 := &beads.Bead{ID: "b3", ExternalRef: "not-a-ref", Metadata: map[string]any{}}
	if got := e.EnrichBody(context.Background(), b3, "scanner"); got != "" {
		t.Errorf("expected empty body for bad ref, got %q", got)
	}

	e.ClearCache()
}

func TestPREnricher_EnrichBody_FetchError(t *testing.T) {
	client, closeSrv := mockGHClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	defer closeSrv()

	e := NewPREnricher(client, "myorg", []string{"repo1"}, covLogger())
	b := &beads.Bead{ID: "b1", ExternalRef: "myorg/repo1#99", Metadata: map[string]any{}}
	if got := e.EnrichBody(context.Background(), b, "scanner"); got != "" {
		t.Errorf("expected empty body on fetch error, got %q", got)
	}
}

func TestPREnricher_FetchPR_NoClient(t *testing.T) {
	e := NewPREnricher(nil, "org", nil, covLogger())
	if pr := e.fetchPR(context.Background(), "o", "r", 1); pr != nil {
		t.Error("expected nil PR with nil client")
	}
}

// --- BeadSynthesizer.CleanupVault ---

func TestBeadSynthesizer_CleanupVault(t *testing.T) {
	srv, _ := newMockWikiServer(t)
	store := newTestStore(t)
	stores := map[string]*beads.Store{"scanner": store}

	vaultDir := t.TempDir()
	api := NewKnowledgeAPI([]LayerConfig{{Type: LayerProject, URL: srv.URL}},
		KnowledgeConfig{Enabled: true}, covLogger())

	synth := NewBeadSynthesizer(stores, api, BeadSynthesizerConfig{
		VaultPath: vaultDir,
	}, covLogger(), nil)

	// A junk fact (metadata-only body) and a substantive one.
	junk := "---\ntitle: Junk\n---\n- External: x\n- Source: y\n"
	if err := os.WriteFile(filepath.Join(vaultDir, "junk.md"), []byte(junk), 0o644); err != nil {
		t.Fatal(err)
	}
	good := "---\ntitle: Good\n---\nReal substantive content here.\n"
	if err := os.WriteFile(filepath.Join(vaultDir, "good.md"), []byte(good), 0o644); err != nil {
		t.Fatal(err)
	}

	removed, err := synth.CleanupVault()
	if err != nil {
		t.Fatalf("CleanupVault: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
}

func TestBeadSynthesizer_CleanupVault_NoDir(t *testing.T) {
	synth := &BeadSynthesizer{logger: covLogger()}
	if n, err := synth.CleanupVault(); err != nil || n != 0 {
		t.Errorf("expected (0,nil) with no vault dir, got (%d,%v)", n, err)
	}
}

// --- types.factDocURL ---

func TestFactDocURL(t *testing.T) {
	f := Fact{Sources: []Source{{DocURL: "https://example.com/doc"}}}
	if got := factDocURL(f); got != "https://example.com/doc" {
		t.Errorf("factDocURL = %q", got)
	}
	if got := factDocURL(Fact{}); got != "" {
		t.Errorf("expected empty for no sources, got %q", got)
	}
}

// --- FormatForPrompt with doc URL source (exercises the source-link branch) ---

func TestFormatForPrompt_WithDocURL(t *testing.T) {
	pk := &PrimedKnowledge{
		Facts: []Fact{
			{
				Title:      "Doc-backed fact",
				Type:       FactReference,
				Body:       "body",
				Confidence: 0.5,
				Sources:    []Source{{DocURL: "https://example.com/x"}},
			},
		},
		Hints: []string{"a hint"},
	}
	out := pk.FormatForPrompt()
	if !strings.Contains(out, "source: https://example.com/x") {
		t.Errorf("expected doc source link in prompt, got:\n%s", out)
	}
	if !strings.Contains(out, "a hint") {
		t.Errorf("expected hint in prompt, got:\n%s", out)
	}
}
