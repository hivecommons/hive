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
)

// withVaultsBaseDir points vaultsBaseDir at a temp dir for hermetic vault tests.
func withVaultsBaseDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	orig := vaultsBaseDir
	vaultsBaseDir = dir
	t.Cleanup(func() { vaultsBaseDir = orig })
	return dir
}

// --- CreateVault happy paths -------------------------------------------------

func TestCreateVault_CreatesAndConnects(t *testing.T) {
	base := withVaultsBaseDir(t)
	api := &KnowledgeAPI{config: KnowledgeConfig{Enabled: true}, logger: covLogger()}

	info, err := api.CreateVault("Team Notes")
	if err != nil {
		t.Fatalf("CreateVault: %v", err)
	}
	wantDir := filepath.Join(base, "team-notes")
	if info.RootDir != wantDir {
		t.Errorf("RootDir = %q, want %q", info.RootDir, wantDir)
	}
	if st, err := os.Stat(wantDir); err != nil || !st.IsDir() {
		t.Fatalf("vault dir not created: %v", err)
	}

	found := false
	for _, v := range api.Vaults() {
		if v.RootDir == wantDir {
			found = true
		}
	}
	if !found {
		t.Error("created vault not listed by Vaults()")
	}
}

func TestCreateVault_IdempotentRecreate(t *testing.T) {
	withVaultsBaseDir(t)
	api := &KnowledgeAPI{config: KnowledgeConfig{Enabled: true}, logger: covLogger()}

	first, err := api.CreateVault("ops")
	if err != nil {
		t.Fatalf("first CreateVault: %v", err)
	}
	second, err := api.CreateVault("ops")
	if err != nil {
		t.Fatalf("re-create should be treated as connect, got: %v", err)
	}
	if second.RootDir != first.RootDir {
		t.Errorf("re-create RootDir = %q, want %q", second.RootDir, first.RootDir)
	}
	if got := len(api.Vaults()); got != 1 {
		t.Errorf("Vaults() len = %d, want 1 (no duplicate connection)", got)
	}
}

func TestCreateVault_TrimsWhitespaceOnlyName(t *testing.T) {
	withVaultsBaseDir(t)
	api := &KnowledgeAPI{config: KnowledgeConfig{Enabled: true}, logger: covLogger()}
	if _, err := api.CreateVault("   "); err == nil {
		t.Fatal("whitespace-only name should be rejected")
	}
}

// --- autoImportContext7 ------------------------------------------------------

func autoImportAPI(t *testing.T, key string, items []Context7AutoImport) *KnowledgeAPI {
	t.Helper()
	withKnowledgeBaseDir(t)
	return &KnowledgeAPI{
		config: KnowledgeConfig{
			Enabled:  true,
			Context7: Context7Config{AutoImport: items},
		},
		context7APIKey: key,
		logger:         covLogger(),
	}
}

func TestAutoImportContext7_NoKeyMakesNoRequests(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
	}))
	defer srv.Close()
	useContext7TestServer(t, srv.URL)

	api := autoImportAPI(t, "", []Context7AutoImport{{ID: "/vercel/next.js"}})
	api.autoImportContext7()
	if requests != 0 {
		t.Errorf("expected no network calls without an API key, got %d", requests)
	}
	if got := api.ListDocuments(); len(got) != 0 {
		t.Errorf("expected no imports, got %+v", got)
	}
}

func TestAutoImportContext7_ImportsConfiguredLibrary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, context7LLMsTxtSuffix) {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte("Next.js Docs\n\nRouting must be file based.\n\nServer components render on the server."))
	}))
	defer srv.Close()
	useContext7TestServer(t, srv.URL)

	api := autoImportAPI(t, "key", []Context7AutoImport{
		{ID: ""}, // blank entries are skipped
		{ID: "/vercel/next.js"},
	})
	api.autoImportContext7()

	docs := api.ListDocuments()
	if len(docs) != 1 {
		t.Fatalf("ListDocuments = %+v, want exactly one import", docs)
	}
	if docs[0].Slug != "vercel-nextjs" {
		t.Errorf("slug = %q, want vercel-nextjs", docs[0].Slug)
	}
	if docs[0].FactCount == 0 {
		t.Errorf("expected extracted facts, got %+v", docs[0])
	}

	// Second run: the library is already present and must not be re-imported.
	api.autoImportContext7()
	if got := api.ListDocuments(); len(got) != 1 {
		t.Errorf("re-run imported duplicates: %+v", got)
	}
}

func TestAutoImportContext7_FetchFailureIsNonFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	useContext7TestServer(t, srv.URL)

	api := autoImportAPI(t, "key", []Context7AutoImport{{ID: "/broken/lib"}})
	api.autoImportContext7() // must not panic
	if got := api.ListDocuments(); len(got) != 0 {
		t.Errorf("failed import should not register a document, got %+v", got)
	}
}

// --- WireContext7Suggester happy/error paths ---------------------------------

func TestWireContext7Suggester_SuggestsImportCommand(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != context7SearchPath {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(context7SearchResponse{Results: []Context7SearchResult{
			{ID: "/vercel/next.js", Title: "Next.js", Snippets: 42, Trust: 0.95},
		}})
	}))
	defer srv.Close()
	useContext7TestServer(t, srv.URL)

	api := &KnowledgeAPI{config: KnowledgeConfig{Enabled: true}, context7APIKey: "key", logger: covLogger()}
	p := NewPrimer(nil, PrimerConfig{}, covLogger())
	api.WireContext7Suggester(p)
	if p.context7Suggest == nil {
		t.Fatal("suggester should be wired when key present")
	}

	hint := p.context7Suggest(context.Background(), "nextjs")
	if !strings.Contains(hint, "bd kb import-ctx7 /vercel/next.js") {
		t.Errorf("hint = %q, want import command for /vercel/next.js", hint)
	}
	if !strings.Contains(hint, "Next.js") {
		t.Errorf("hint = %q, want library title", hint)
	}
}

func TestWireContext7Suggester_SearchErrorReturnsEmptyHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	useContext7TestServer(t, srv.URL)

	api := &KnowledgeAPI{config: KnowledgeConfig{Enabled: true}, context7APIKey: "key", logger: covLogger()}
	p := NewPrimer(nil, PrimerConfig{}, covLogger())
	api.WireContext7Suggester(p)

	if hint := p.context7Suggest(context.Background(), "nextjs"); hint != "" {
		t.Errorf("hint on search error = %q, want empty", hint)
	}
}

func TestWireContext7Suggester_NoResultsReturnsEmptyHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(context7SearchResponse{})
	}))
	defer srv.Close()
	useContext7TestServer(t, srv.URL)

	api := &KnowledgeAPI{config: KnowledgeConfig{Enabled: true}, context7APIKey: "key", logger: covLogger()}
	p := NewPrimer(nil, PrimerConfig{}, covLogger())
	api.WireContext7Suggester(p)

	if hint := p.context7Suggest(context.Background(), "unknown-lib"); hint != "" {
		t.Errorf("hint with no results = %q, want empty", hint)
	}
}
