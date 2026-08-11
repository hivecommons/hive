package knowledge

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func coverageTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func coverageWikiServer() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/search", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(searchResponse{
			Results: []searchResult{
				{Slug: "fact-1", Title: "Fact One", Type: "gotcha", Confidence: 0.9},
			},
			Total: 1,
		})
	})
	mux.HandleFunc("/api/pages", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusOK)
			return
		}
		json.NewEncoder(w).Encode(searchResponse{
			Results: []searchResult{
				{Slug: "fact-1", Title: "Fact One"},
			},
			Total: 1,
		})
	})
	mux.HandleFunc("/api/pages/fact-1", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case http.MethodPost:
			w.WriteHeader(http.StatusOK)
		default:
			json.NewEncoder(w).Encode(pageResponse{
				Slug: "fact-1", Title: "Fact One", Body: "Body", Type: "gotcha", Confidence: 0.9,
			})
		}
	})
	mux.HandleFunc("/api/stats", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(statsResponse{TotalPages: 10, ByType: map[string]int{"gotcha": 5}})
	})
	mux.HandleFunc("/api/ingest", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return httptest.NewServer(mux)
}

func coverageKnowledgeAPI(serverURL string) *KnowledgeAPI {
	logger := coverageTestLogger()
	layers := []LayerConfig{
		{Type: LayerProject, URL: serverURL},
	}
	return NewKnowledgeAPI(layers, KnowledgeConfig{Enabled: true, Engine: "llm-wiki"}, logger)
}

func TestUpdateFact(t *testing.T) {
	server := coverageWikiServer()
	defer server.Close()

	api := coverageKnowledgeAPI(server.URL)
	err := api.UpdateFact(context.Background(), LayerProject, "fact-1", UpdateFactRequest{
		Title: "Updated", Body: "New body", Type: "pattern",
	})
	if err != nil {
		t.Fatalf("UpdateFact: %v", err)
	}
}

func TestUpdateFact_UnknownLayer(t *testing.T) {
	server := coverageWikiServer()
	defer server.Close()

	api := coverageKnowledgeAPI(server.URL)
	err := api.UpdateFact(context.Background(), "nonexistent", "fact-1", UpdateFactRequest{})
	if err == nil {
		t.Error("expected error for unknown layer")
	}
}

func TestPromoteFact(t *testing.T) {
	server := coverageWikiServer()
	defer server.Close()

	// Create API with two layers for promotion
	logger := coverageTestLogger()
	layers := []LayerConfig{
		{Type: LayerProject, URL: server.URL},
		{Type: LayerOrg, URL: server.URL},
	}
	api := NewKnowledgeAPI(layers, KnowledgeConfig{Enabled: true}, logger)

	result := api.PromoteFact(context.Background(), PromoteRequest{
		Slug: "fact-1", FromLayer: LayerProject, ToLayer: LayerOrg,
		Reason: "good fact", Promoter: "test",
	})
	if !result.Success {
		t.Errorf("PromoteFact failed: %s", result.Error)
	}
}

func TestPromoteFact_DownwardFails(t *testing.T) {
	server := coverageWikiServer()
	defer server.Close()

	logger := coverageTestLogger()
	layers := []LayerConfig{
		{Type: LayerProject, URL: server.URL},
		{Type: LayerOrg, URL: server.URL},
	}
	api := NewKnowledgeAPI(layers, KnowledgeConfig{Enabled: true}, logger)

	result := api.PromoteFact(context.Background(), PromoteRequest{
		Slug: "fact-1", FromLayer: LayerOrg, ToLayer: LayerProject,
	})
	if result.Success {
		t.Error("downward promotion should fail")
	}
}

func TestImportFacts_JSON(t *testing.T) {
	server := coverageWikiServer()
	defer server.Close()

	api := coverageKnowledgeAPI(server.URL)
	content := `[{"title":"JSON Fact","body":"Body","type":"pattern","confidence":0.8}]`
	count, err := api.ImportFacts(context.Background(), LayerProject, content, "json")
	if err != nil {
		t.Fatalf("ImportFacts: %v", err)
	}
	if count != 1 {
		t.Errorf("count = %d, want 1", count)
	}
}

func TestImportFacts_MarkdownHeaders(t *testing.T) {
	server := coverageWikiServer()
	defer server.Close()

	api := coverageKnowledgeAPI(server.URL)
	content := "# My Fact\nThis is the body of the fact.\n\n## Another Fact\nAnother body."
	count, err := api.ImportFacts(context.Background(), LayerProject, content, "markdown")
	if err != nil {
		t.Fatalf("ImportFacts: %v", err)
	}
	if count < 1 {
		t.Errorf("count = %d, want >= 1", count)
	}
}

func TestImportFacts_MarkdownBullets(t *testing.T) {
	server := coverageWikiServer()
	defer server.Close()

	api := coverageKnowledgeAPI(server.URL)
	content := "- **Bold title** some body text\n- **Another** more text"
	count, err := api.ImportFacts(context.Background(), LayerProject, content, "md")
	if err != nil {
		t.Fatalf("ImportFacts: %v", err)
	}
	if count < 1 {
		t.Errorf("count = %d, want >= 1", count)
	}
}

func TestImportFacts_Empty(t *testing.T) {
	server := coverageWikiServer()
	defer server.Close()

	api := coverageKnowledgeAPI(server.URL)
	count, err := api.ImportFacts(context.Background(), LayerProject, "no headings here", "markdown")
	if err != nil {
		t.Fatalf("ImportFacts: %v", err)
	}
	if count != 0 {
		t.Errorf("count = %d, want 0", count)
	}
}

func TestImportFacts_UnknownLayer(t *testing.T) {
	server := coverageWikiServer()
	defer server.Close()

	api := coverageKnowledgeAPI(server.URL)
	_, err := api.ImportFacts(context.Background(), "nonexistent", "# Test\nBody", "markdown")
	if err == nil {
		t.Error("expected error for unknown layer")
	}
}

func TestImportFacts_InvalidJSON(t *testing.T) {
	server := coverageWikiServer()
	defer server.Close()

	api := coverageKnowledgeAPI(server.URL)
	_, err := api.ImportFacts(context.Background(), LayerProject, "not json", "json")
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestImportFacts_DefaultFormat(t *testing.T) {
	server := coverageWikiServer()
	defer server.Close()

	api := coverageKnowledgeAPI(server.URL)
	content := "## Heading\nSome body text here."
	count, err := api.ImportFacts(context.Background(), LayerProject, content, "unknown-format")
	if err != nil {
		t.Fatalf("ImportFacts: %v", err)
	}
	// Default format is markdown
	if count < 1 {
		t.Errorf("count = %d", count)
	}
}

func TestCreateFact_UnknownLayer(t *testing.T) {
	server := coverageWikiServer()
	defer server.Close()

	api := coverageKnowledgeAPI(server.URL)
	err := api.CreateFact(context.Background(), CreateFactRequest{
		Title: "Test", Body: "Body", Layer: "nonexistent",
	})
	if err == nil {
		t.Error("expected error for unknown layer")
	}
}

func TestLayers_Single(t *testing.T) {
	server := coverageWikiServer()
	defer server.Close()

	api := coverageKnowledgeAPI(server.URL)
	layers := api.Layers()
	if len(layers) != 1 {
		t.Errorf("layers = %d, want 1", len(layers))
	}
	if layers[0] != LayerProject {
		t.Errorf("layer = %q", layers[0])
	}
}

func TestParseMarkdownFacts_Headers(t *testing.T) {
	content := "# First Fact\nBody of first fact.\nMore body.\n\n## Second Fact\nBody of second."
	facts := parseMarkdownFacts(content)
	if len(facts) != 2 {
		t.Fatalf("facts = %d, want 2", len(facts))
	}
	if facts[0].Title != "First Fact" {
		t.Errorf("title[0] = %q", facts[0].Title)
	}
	if facts[1].Title != "Second Fact" {
		t.Errorf("title[1] = %q", facts[1].Title)
	}
}

func TestParseMarkdownFacts_BoldBullets(t *testing.T) {
	content := "- **Bold Title** trailing body text\n  continuation line\n- **Another Title** body two"
	facts := parseMarkdownFacts(content)
	if len(facts) != 2 {
		t.Fatalf("facts = %d, want 2", len(facts))
	}
	if facts[0].Title != "Bold Title" {
		t.Errorf("title = %q", facts[0].Title)
	}
}

func TestParseMarkdownFacts_NoHeadings(t *testing.T) {
	facts := parseMarkdownFacts("no headings or bold bullets")
	if len(facts) != 0 {
		t.Errorf("facts = %d, want 0", len(facts))
	}
}

func TestParseJSONFacts_Valid(t *testing.T) {
	content := `[{"title":"Test","body":"Body","type":"gotcha"}]`
	var facts []ExtractedFact
	err := parseJSONFacts(content, &facts)
	if err != nil {
		t.Fatalf("parseJSONFacts: %v", err)
	}
	if len(facts) != 1 {
		t.Errorf("facts = %d", len(facts))
	}
}

// ---- Types coverage ----

func TestLayerTypePrecedence_Community(t *testing.T) {
	if LayerCommunity.Precedence() != 4 {
		t.Errorf("community precedence = %d, want 4", LayerCommunity.Precedence())
	}
}

func TestLayerTypePrecedence_Unknown(t *testing.T) {
	unknown := LayerType("unknown")
	if unknown.Precedence() != 99 {
		t.Errorf("unknown precedence = %d, want 99", unknown.Precedence())
	}
}

func TestFormatConfidence(t *testing.T) {
	tests := []struct {
		input float64
		want  string
	}{
		{0.95, "high"},
		{0.75, "medium"},
		{0.5, "low"},
		{0.0, "low"},
	}
	for _, tt := range tests {
		got := formatConfidence(tt.input)
		if got != tt.want {
			t.Errorf("formatConfidence(%f) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestPrimedKnowledge_FormatForPrompt_Empty(t *testing.T) {
	pk := &PrimedKnowledge{}
	result := pk.FormatForPrompt()
	if result != "" {
		t.Errorf("expected empty string, got %q", result)
	}
}

func TestPrimedKnowledge_FormatForPrompt_WithFacts(t *testing.T) {
	pk := &PrimedKnowledge{
		Facts: []Fact{
			{Title: "Test Fact", Body: "Body here", Type: FactGotcha, Confidence: 0.8},
			{Title: "Full Confidence", Body: "Full", Type: FactGotcha, Confidence: 1.0},
		},
	}
	result := pk.FormatForPrompt()
	if result == "" {
		t.Error("expected non-empty prompt")
	}
}

func TestLayerConfigEndpoint(t *testing.T) {
	tests := []struct {
		config LayerConfig
		want   string
	}{
		{LayerConfig{URL: "http://wiki.test"}, "http://wiki.test"},
		{LayerConfig{Path: "/local/path"}, ""},
		{LayerConfig{}, ""},
	}
	for _, tt := range tests {
		got := tt.config.Endpoint()
		if got != tt.want {
			t.Errorf("Endpoint(%v) = %q, want %q", tt.config, got, tt.want)
		}
	}
}

// ---- Vault tests ----

func createVaultDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(dir+"/note1.md", []byte("# Note One\nSome content about testing."), 0o644)
	os.WriteFile(dir+"/note2.md", []byte("# Note Two\nTags: #kubernetes #deployment\nDeployment guide."), 0o644)
	os.WriteFile(dir+"/readme.md", []byte("# Readme\nProject readme."), 0o644)
	return dir
}

func TestConnectVault(t *testing.T) {
	logger := coverageTestLogger()
	api := &KnowledgeAPI{
		config: KnowledgeConfig{Enabled: true},
		logger: logger,
	}

	dir := createVaultDir(t)
	err := api.ConnectVault(dir, "test-vault")
	if err != nil {
		t.Fatalf("ConnectVault: %v", err)
	}

	vaults := api.Vaults()
	if len(vaults) != 1 {
		t.Fatalf("expected 1 vault, got %d", len(vaults))
	}
	if vaults[0].Name != "test-vault" {
		t.Errorf("vault name = %q", vaults[0].Name)
	}
	if vaults[0].Pages < 3 {
		t.Errorf("expected at least 3 pages, got %d", vaults[0].Pages)
	}
}

func TestConnectVault_Duplicate(t *testing.T) {
	logger := coverageTestLogger()
	api := &KnowledgeAPI{
		config: KnowledgeConfig{Enabled: true},
		logger: logger,
	}

	dir := createVaultDir(t)
	err := api.ConnectVault(dir, "v1")
	if err != nil {
		t.Fatalf("first connect: %v", err)
	}
	err = api.ConnectVault(dir, "v2")
	if err == nil {
		t.Error("expected error for duplicate vault")
	}
}

func TestConnectVault_InvalidDir(t *testing.T) {
	logger := coverageTestLogger()
	api := &KnowledgeAPI{
		config: KnowledgeConfig{Enabled: true},
		logger: logger,
	}

	err := api.ConnectVault("/nonexistent/path", "bad")
	if err == nil {
		t.Error("expected error for invalid dir")
	}
}

func TestDisconnectVault(t *testing.T) {
	logger := coverageTestLogger()
	api := &KnowledgeAPI{
		config: KnowledgeConfig{Enabled: true},
		logger: logger,
	}

	dir := createVaultDir(t)
	api.ConnectVault(dir, "test-vault")

	err := api.DisconnectVault(dir)
	if err != nil {
		t.Fatalf("DisconnectVault: %v", err)
	}
	if len(api.Vaults()) != 0 {
		t.Error("expected 0 vaults after disconnect")
	}
}

func TestDisconnectVault_NotFound(t *testing.T) {
	logger := coverageTestLogger()
	api := &KnowledgeAPI{
		config: KnowledgeConfig{Enabled: true},
		logger: logger,
	}

	err := api.DisconnectVault("/nonexistent")
	if err == nil {
		t.Error("expected error for disconnecting unknown vault")
	}
}

func TestReindexVault(t *testing.T) {
	logger := coverageTestLogger()
	api := &KnowledgeAPI{
		config: KnowledgeConfig{Enabled: true},
		logger: logger,
	}

	dir := createVaultDir(t)
	api.ConnectVault(dir, "test-vault")

	err := api.ReindexVault(dir)
	if err != nil {
		t.Fatalf("ReindexVault: %v", err)
	}
}

func TestReindexVault_NotFound(t *testing.T) {
	logger := coverageTestLogger()
	api := &KnowledgeAPI{
		config: KnowledgeConfig{Enabled: true},
		logger: logger,
	}

	err := api.ReindexVault("/nonexistent")
	if err == nil {
		t.Error("expected error for reindexing unknown vault")
	}
}

func TestSearchAllWithVaults(t *testing.T) {
	logger := coverageTestLogger()
	server := coverageWikiServer()
	defer server.Close()

	layers := []layerClient{
		{layerType: LayerProject, client: NewClient(server.URL, logger)},
	}
	api := &KnowledgeAPI{
		layers: layers,
		config: KnowledgeConfig{Enabled: true},
		logger: logger,
	}

	dir := createVaultDir(t)
	api.ConnectVault(dir, "test-vault")

	const searchLimit = 10
	results := api.SearchAllWithVaults(context.Background(), "testing", "", searchLimit)
	// Should include results from both wiki and vault
	if len(results) == 0 {
		t.Error("expected at least some results")
	}
}

func TestVaultFacts(t *testing.T) {
	logger := coverageTestLogger()
	api := &KnowledgeAPI{
		config: KnowledgeConfig{Enabled: true},
		logger: logger,
	}

	dir := createVaultDir(t)
	api.ConnectVault(dir, "test-vault")

	facts := api.VaultFacts("test-vault")
	if len(facts) < 3 {
		t.Errorf("expected at least 3 facts, got %d", len(facts))
	}
}

func TestVaultFacts_NotFound(t *testing.T) {
	logger := coverageTestLogger()
	api := &KnowledgeAPI{
		config: KnowledgeConfig{Enabled: true},
		logger: logger,
	}

	facts := api.VaultFacts("nonexistent")
	if facts != nil {
		t.Error("expected nil for unknown vault")
	}
}

func TestVaultFact(t *testing.T) {
	logger := coverageTestLogger()
	api := &KnowledgeAPI{
		config: KnowledgeConfig{Enabled: true},
		logger: logger,
	}

	dir := createVaultDir(t)
	api.ConnectVault(dir, "test-vault")

	fact, err := api.VaultFact("note1")
	if err != nil {
		t.Fatalf("VaultFact: %v", err)
	}
	if fact == nil {
		t.Fatal("expected non-nil fact")
	}
}

func TestVaultFact_NotFound(t *testing.T) {
	logger := coverageTestLogger()
	api := &KnowledgeAPI{
		config: KnowledgeConfig{Enabled: true},
		logger: logger,
	}

	_, err := api.VaultFact("nonexistent")
	if err == nil {
		t.Error("expected error for unknown fact")
	}
}

// ---- FileStore direct tests ----

func TestFileStore_Search_EmptyQuery(t *testing.T) {
	dir := createVaultDir(t)
	logger := coverageTestLogger()
	store, err := NewFileStore(dir, "test", logger)
	if err != nil {
		t.Fatal(err)
	}
	const searchLimit = 10
	results := store.Search("", searchLimit)
	if len(results) != 0 {
		t.Errorf("expected 0 results for empty query, got %d", len(results))
	}
}

func TestFileStore_Search_WithResults(t *testing.T) {
	dir := createVaultDir(t)
	logger := coverageTestLogger()
	store, err := NewFileStore(dir, "test", logger)
	if err != nil {
		t.Fatal(err)
	}
	const searchLimit = 10
	results := store.Search("testing", searchLimit)
	if len(results) == 0 {
		t.Error("expected results for 'testing'")
	}
}

func TestFileStore_Search_TagMatch(t *testing.T) {
	dir := createVaultDir(t)
	logger := coverageTestLogger()
	store, err := NewFileStore(dir, "test", logger)
	if err != nil {
		t.Fatal(err)
	}
	const searchLimit = 10
	results := store.Search("kubernetes", searchLimit)
	if len(results) == 0 {
		t.Error("expected results for 'kubernetes' tag match")
	}
}

func TestFileStore_Search_LimitExceeded(t *testing.T) {
	dir := createVaultDir(t)
	logger := coverageTestLogger()
	store, err := NewFileStore(dir, "test", logger)
	if err != nil {
		t.Fatal(err)
	}
	// Search for very common term with limit 1
	const limitOne = 1
	results := store.Search("note", limitOne)
	if len(results) > limitOne {
		t.Errorf("expected at most 1 result, got %d", len(results))
	}
}

func TestFileStore_Search_DefaultLimit(t *testing.T) {
	dir := createVaultDir(t)
	logger := coverageTestLogger()
	store, err := NewFileStore(dir, "test", logger)
	if err != nil {
		t.Fatal(err)
	}
	results := store.Search("note", 0)
	// Should use default limit and not crash
	_ = results
}

func TestFileStore_ReadPage(t *testing.T) {
	dir := createVaultDir(t)
	logger := coverageTestLogger()
	store, err := NewFileStore(dir, "test", logger)
	if err != nil {
		t.Fatal(err)
	}
	fact, err := store.ReadPage("note1")
	if err != nil {
		t.Fatalf("ReadPage: %v", err)
	}
	if fact.Title == "" {
		t.Error("expected non-empty title")
	}
}

func TestFileStore_ReadPage_NotFound(t *testing.T) {
	dir := createVaultDir(t)
	logger := coverageTestLogger()
	store, err := NewFileStore(dir, "test", logger)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.ReadPage("nonexistent")
	if err == nil {
		t.Error("expected error for unknown page")
	}
}

func TestFileStore_ListPages_NoFilter(t *testing.T) {
	dir := createVaultDir(t)
	logger := coverageTestLogger()
	store, err := NewFileStore(dir, "test", logger)
	if err != nil {
		t.Fatal(err)
	}
	facts := store.ListPages("")
	if len(facts) < 3 {
		t.Errorf("expected at least 3 pages, got %d", len(facts))
	}
}

func TestFileStore_ListPages_WithTagFilter(t *testing.T) {
	dir := createVaultDir(t)
	logger := coverageTestLogger()
	store, err := NewFileStore(dir, "test", logger)
	if err != nil {
		t.Fatal(err)
	}
	facts := store.ListPages("kubernetes")
	// note2.md has #kubernetes tag
	if len(facts) == 0 {
		t.Error("expected results for kubernetes tag filter")
	}
}

func TestFileStore_ListPages_NoMatchingTag(t *testing.T) {
	dir := createVaultDir(t)
	logger := coverageTestLogger()
	store, err := NewFileStore(dir, "test", logger)
	if err != nil {
		t.Fatal(err)
	}
	facts := store.ListPages("nonexistent-tag")
	if len(facts) != 0 {
		t.Errorf("expected 0 results for unknown tag, got %d", len(facts))
	}
}

func TestFileStore_Stats(t *testing.T) {
	dir := createVaultDir(t)
	logger := coverageTestLogger()
	store, err := NewFileStore(dir, "test", logger)
	if err != nil {
		t.Fatal(err)
	}
	stats := store.Stats()
	if stats.Name != "test" {
		t.Errorf("name = %q", stats.Name)
	}
	if stats.TotalPages < 3 {
		t.Errorf("pages = %d", stats.TotalPages)
	}
}

func TestFileStore_Reindex(t *testing.T) {
	dir := createVaultDir(t)
	logger := coverageTestLogger()
	store, err := NewFileStore(dir, "test", logger)
	if err != nil {
		t.Fatal(err)
	}
	// Add a new file
	os.WriteFile(dir+"/new.md", []byte("# New Page\nNew content"), 0o644)
	store.Reindex()
	facts := store.ListPages("")
	if len(facts) < 4 {
		t.Errorf("expected at least 4 pages after reindex, got %d", len(facts))
	}
}

func TestFileStore_NotADirectory(t *testing.T) {
	// Create a file instead of a directory
	f, err := os.CreateTemp(t.TempDir(), "notadir")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	logger := coverageTestLogger()
	_, err = NewFileStore(f.Name(), "bad", logger)
	if err == nil {
		t.Error("expected error for non-directory path")
	}
}

func TestFileStore_HiddenDirSkipped(t *testing.T) {
	dir := createVaultDir(t)
	// Create a hidden directory with a markdown file
	hiddenDir := dir + "/.obsidian"
	os.MkdirAll(hiddenDir, 0o755)
	os.WriteFile(hiddenDir+"/config.md", []byte("# Hidden\nShould be skipped"), 0o644)

	logger := coverageTestLogger()
	store, err := NewFileStore(dir, "test", logger)
	if err != nil {
		t.Fatal(err)
	}
	// Hidden dir files should not appear
	facts := store.ListPages("")
	for _, f := range facts {
		if strings.Contains(f.Slug, ".obsidian") {
			t.Errorf("hidden dir page included: %s", f.Slug)
		}
	}
}

func TestFileStore_SymlinkedMarkdownOutsideRootSkipped(t *testing.T) {
	dir := t.TempDir()
	outsideDir := t.TempDir()
	outsidePath := filepath.Join(outsideDir, "secret.md")
	if err := os.WriteFile(outsidePath, []byte("# Outside Secret\nshould not be indexed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsidePath, filepath.Join(dir, "linked-secret.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "inside.md"), []byte("# Inside\nindexed"), 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := NewFileStore(dir, "test", coverageTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	if stats := store.Stats(); stats.TotalPages != 1 {
		t.Fatalf("TotalPages = %d, want only the in-root markdown page", stats.TotalPages)
	}
	if _, err := store.ReadPage("linked-secret"); err == nil {
		t.Fatal("expected symlinked markdown outside root not to be readable")
	}
	if results := store.Search("Outside Secret", 10); len(results) != 0 {
		t.Fatalf("search returned symlinked outside content: %#v", results)
	}
	if _, err := store.ReadPage("../secret"); err == nil {
		t.Fatal("expected path-traversal-looking slug not to be readable")
	}
}

func TestFileStore_LongBodyTruncated(t *testing.T) {
	dir := t.TempDir()
	// Create a markdown file with a very long body
	longBody := strings.Repeat("A very long line of text. ", 100)
	os.WriteFile(dir+"/long.md", []byte("# Long Note\n"+longBody), 0o644)

	logger := coverageTestLogger()
	store, err := NewFileStore(dir, "test", logger)
	if err != nil {
		t.Fatal(err)
	}

	// Search should return truncated snippets
	const searchLimit = 10
	results := store.Search("long", searchLimit)
	for _, r := range results {
		const maxSnippetPlusEllipsis = 210
		if len(r.Body) > maxSnippetPlusEllipsis {
			t.Errorf("body length %d exceeds max", len(r.Body))
		}
	}

	// ListPages should also truncate
	facts := store.ListPages("")
	for _, f := range facts {
		const maxSnippetPlusEllipsis = 210
		if len(f.Body) > maxSnippetPlusEllipsis {
			t.Errorf("body length %d exceeds max", len(f.Body))
		}
	}
}

// --- parseObsidianFile tests ---

func TestParseObsidianFile_PlainText(t *testing.T) {
	title, body, tags, _, _ := parseObsidianFile("Just plain text content", "fallback")
	if title != "fallback" {
		t.Errorf("title = %q, want fallback", title)
	}
	if body != "Just plain text content" {
		t.Errorf("body = %q", body)
	}
	if len(tags) != 0 {
		t.Errorf("tags = %v, want empty", tags)
	}
}

func TestParseObsidianFile_HeadingOnly(t *testing.T) {
	content := "# My Title\nSome body text here"
	title, body, tags, _, _ := parseObsidianFile(content, "fallback")
	if title != "My Title" {
		t.Errorf("title = %q, want My Title", title)
	}
	if body != "Some body text here" {
		t.Errorf("body = %q", body)
	}
	if len(tags) != 0 {
		t.Errorf("tags = %v", tags)
	}
}

func TestParseObsidianFile_FrontmatterWithTitle(t *testing.T) {
	content := "---\ntitle: \"Front Title\"\n---\nBody after frontmatter"
	title, body, tags, _, _ := parseObsidianFile(content, "fallback")
	if title != "Front Title" {
		t.Errorf("title = %q, want Front Title", title)
	}
	if body != "Body after frontmatter" {
		t.Errorf("body = %q", body)
	}
	if len(tags) != 0 {
		t.Errorf("tags = %v", tags)
	}
}

func TestParseObsidianFile_FrontmatterWithTags(t *testing.T) {
	content := "---\ntitle: Tagged\ntags: [\"go\", \"testing\"]\n---\nContent here"
	title, body, tags, _, _ := parseObsidianFile(content, "fallback")
	if title != "Tagged" {
		t.Errorf("title = %q", title)
	}
	if len(tags) != 2 {
		t.Fatalf("tags len = %d, want 2", len(tags))
	}
	if tags[0] != "go" || tags[1] != "testing" {
		t.Errorf("tags = %v", tags)
	}
	if body != "Content here" {
		t.Errorf("body = %q", body)
	}
}

func TestParseObsidianFile_FrontmatterListTags(t *testing.T) {
	content := "---\ntags: [initial]\n- extra-tag\n---\nBody"
	title, body, tags, _, _ := parseObsidianFile(content, "fallback")
	if title != "fallback" {
		t.Errorf("title = %q", title)
	}
	if len(tags) != 2 {
		t.Fatalf("tags len = %d, want 2: %v", len(tags), tags)
	}
	if tags[0] != "initial" || tags[1] != "extra-tag" {
		t.Errorf("tags = %v", tags)
	}
	if body != "Body" {
		t.Errorf("body = %q", body)
	}
}

func TestParseObsidianFile_InlineTags(t *testing.T) {
	content := "Some text #mytag and #another here"
	_, _, tags, _, _ := parseObsidianFile(content, "fallback")
	if len(tags) != 2 {
		t.Fatalf("tags len = %d, want 2: %v", len(tags), tags)
	}
	if tags[0] != "mytag" || tags[1] != "another" {
		t.Errorf("tags = %v", tags)
	}
}

func TestParseObsidianFile_InlineTagDedup(t *testing.T) {
	content := "---\ntags: [\"go\"]\n---\nBody with #go inline"
	_, _, tags, _, _ := parseObsidianFile(content, "fallback")
	// "go" should not be duplicated
	count := 0
	for _, t2 := range tags {
		if t2 == "go" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected 1 'go' tag, got %d in %v", count, tags)
	}
}

func TestParseObsidianFile_HeadingAfterFrontmatter(t *testing.T) {
	content := "---\ntitle: FM Title\n---\n# Heading Title\nActual body"
	title, body, tags, _, _ := parseObsidianFile(content, "fallback")
	// Heading should override frontmatter title
	if title != "Heading Title" {
		t.Errorf("title = %q, want Heading Title", title)
	}
	if body != "Actual body" {
		t.Errorf("body = %q", body)
	}
	if len(tags) != 0 {
		t.Errorf("tags = %v", tags)
	}
}

func TestParseObsidianFile_UnclosedFrontmatter(t *testing.T) {
	// No closing --- means no frontmatter parsing
	content := "---\ntitle: Orphaned\nstill going on"
	title, body, _, _, _ := parseObsidianFile(content, "fallback")
	if title != "fallback" {
		t.Errorf("title = %q, want fallback (unclosed frontmatter)", title)
	}
	if body != content {
		t.Errorf("body should be full content")
	}
}

func TestParseObsidianFile_SingleQuotedTitle(t *testing.T) {
	content := "---\ntitle: 'Quoted Title'\n---\nBody"
	title, _, _, _, _ := parseObsidianFile(content, "fallback")
	if title != "Quoted Title" {
		t.Errorf("title = %q, want Quoted Title", title)
	}
}

func TestParseObsidianFile_MarkdownHeadingIgnored(t *testing.T) {
	// ## should not be treated as inline tags
	content := "Some text ##heading not a tag"
	_, _, tags, _, _ := parseObsidianFile(content, "fallback")
	for _, tag := range tags {
		if tag == "heading" || tag == "#heading" {
			t.Errorf("## should not produce tags, got %v", tags)
		}
	}
}

func TestParseObsidianFile_EmptyTagArray(t *testing.T) {
	content := "---\ntags: []\n---\nBody"
	_, _, tags, _, _ := parseObsidianFile(content, "fallback")
	if len(tags) != 0 {
		t.Errorf("tags = %v, want empty", tags)
	}
}

func TestContainsTag_CaseInsensitive(t *testing.T) {
	tags := []string{"Go", "Testing"}
	if !containsTag(tags, "go") {
		t.Error("expected case-insensitive match for 'go'")
	}
	if !containsTag(tags, "TESTING") {
		t.Error("expected case-insensitive match for 'TESTING'")
	}
	if containsTag(tags, "rust") {
		t.Error("should not match 'rust'")
	}
}

func TestContainsTag_EmptySlice(t *testing.T) {
	if containsTag(nil, "anything") {
		t.Error("nil slice should not contain anything")
	}
}

// --- SearchAll error path tests ---

func TestSearchAll_LayerError(t *testing.T) {
	// Server that returns 500 for search
	mux := http.NewServeMux()
	mux.HandleFunc("/api/search", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	logger := coverageTestLogger()
	client := NewClient(server.URL, logger)
	api := &KnowledgeAPI{
		config: KnowledgeConfig{Enabled: true},
		layers: []layerClient{
			{layerType: "personal", client: client},
		},
		logger: logger,
	}

	results := api.SearchAll(context.Background(), "query", "", 10)
	// Should return empty results when layer errors, not panic
	if results == nil {
		results = []Fact{}
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results on error, got %d", len(results))
	}
}

func TestLayerFacts_LayerNotFound(t *testing.T) {
	logger := coverageTestLogger()
	api := &KnowledgeAPI{
		config: KnowledgeConfig{Enabled: true},
		layers: []layerClient{},
		logger: logger,
	}

	facts := api.LayerFacts(context.Background(), "personal", "")
	if facts != nil {
		t.Errorf("expected nil for missing layer, got %d facts", len(facts))
	}
}

func TestLayerFacts_Error(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/pages", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	logger := coverageTestLogger()
	client := NewClient(server.URL, logger)
	api := &KnowledgeAPI{
		config: KnowledgeConfig{Enabled: true},
		layers: []layerClient{
			{layerType: "personal", client: client},
		},
		logger: logger,
	}

	facts := api.LayerFacts(context.Background(), "personal", "")
	if facts != nil {
		t.Errorf("expected nil on error, got %d facts", len(facts))
	}
}

func TestDeleteFact_LayerNotFound(t *testing.T) {
	logger := coverageTestLogger()
	api := &KnowledgeAPI{
		config: KnowledgeConfig{Enabled: true},
		layers: []layerClient{},
		logger: logger,
	}

	err := api.DeleteFact(context.Background(), "some-slug", "nonexistent")
	if err == nil {
		t.Error("expected error for unknown layer")
	}
}

func TestUpdateFact_LayerNotFound(t *testing.T) {
	logger := coverageTestLogger()
	api := &KnowledgeAPI{
		config: KnowledgeConfig{Enabled: true},
		layers: []layerClient{},
		logger: logger,
	}

	err := api.UpdateFact(context.Background(), "nonexistent", "test-slug", UpdateFactRequest{
		Title: "test",
	})
	if err == nil {
		t.Error("expected error for unknown layer")
	}
}

func TestCreateFact_LayerNotFound(t *testing.T) {
	logger := coverageTestLogger()
	api := &KnowledgeAPI{
		config: KnowledgeConfig{Enabled: true},
		layers: []layerClient{},
		logger: logger,
	}

	err := api.CreateFact(context.Background(), CreateFactRequest{
		Title: "test",
		Body:  "body",
		Layer: "nonexistent",
	})
	if err == nil {
		t.Error("expected error for unknown layer")
	}
}

// --- Primer tests ---

func TestNewPrimer_EmptyLayers(t *testing.T) {
	logger := coverageTestLogger()
	primer := NewPrimer(nil, PrimerConfig{}, logger)
	if primer == nil {
		t.Error("expected non-nil primer even with empty layers")
	}
}

func TestNewPrimer_DefaultMaxFacts(t *testing.T) {
	logger := coverageTestLogger()
	primer := NewPrimer(nil, PrimerConfig{MaxFacts: 0}, logger)
	if primer == nil {
		t.Error("expected non-nil primer")
	}
}

func TestNewPrimer_SkipLocalOnlyLayer(t *testing.T) {
	logger := coverageTestLogger()
	layers := []LayerConfig{
		{Type: "personal"},
	}
	primer := NewPrimer(layers, PrimerConfig{}, logger)
	if primer == nil {
		t.Error("expected non-nil primer")
	}
}

// makeReadyGitSource creates a GitSource with a pre-initialized FileStore.
func makeReadyGitSource(t *testing.T, layer LayerType) *GitSource {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(dir+"/guide.md", []byte("# Guide\nSome guide content about setup."), 0o644)

	logger := coverageTestLogger()
	store, err := NewFileStore(dir, "git-test", logger)
	if err != nil {
		t.Fatal(err)
	}

	gs := &GitSource{
		config: GitSourceConfig{
			Name:  "test-git",
			URL:   "https://example.com/repo.git",
			Layer: layer,
		},
		store:  store,
		ready:  true,
		logger: logger,
	}
	return gs
}

func TestSearchAllWithVaults_WithGitSources(t *testing.T) {
	logger := coverageTestLogger()
	server := coverageWikiServer()
	defer server.Close()

	layers := []layerClient{
		{layerType: LayerProject, client: NewClient(server.URL, logger)},
	}

	gs := makeReadyGitSource(t, LayerOrg)

	api := &KnowledgeAPI{
		layers:     layers,
		config:     KnowledgeConfig{Enabled: true},
		gitSources: []*GitSource{gs},
		logger:     logger,
	}

	const searchLimit = 10
	results := api.SearchAllWithVaults(context.Background(), "guide", "", searchLimit)
	if len(results) == 0 {
		t.Error("expected results from git source search")
	}
	// Check that at least one result has the git source layer
	foundGitLayer := false
	for _, r := range results {
		if r.Layer == LayerOrg {
			foundGitLayer = true
			break
		}
	}
	if !foundGitLayer {
		t.Error("expected at least one result with git source layer")
	}
}

func TestSearchAllWithVaults_EmptyQuery_GitSources(t *testing.T) {
	logger := coverageTestLogger()

	gs := makeReadyGitSource(t, LayerProject)

	api := &KnowledgeAPI{
		config:     KnowledgeConfig{Enabled: true},
		gitSources: []*GitSource{gs},
		logger:     logger,
	}

	const searchLimit = 10
	results := api.SearchAllWithVaults(context.Background(), "", "", searchLimit)
	if len(results) == 0 {
		t.Error("expected results from git source list pages")
	}
}

func TestSearchAllWithVaults_NotReadyGitSource(t *testing.T) {
	logger := coverageTestLogger()

	gs := &GitSource{
		config: GitSourceConfig{Name: "not-ready", Layer: LayerProject},
		ready:  false,
		logger: logger,
	}

	api := &KnowledgeAPI{
		config:     KnowledgeConfig{Enabled: true},
		gitSources: []*GitSource{gs},
		logger:     logger,
	}

	results := api.SearchAllWithVaults(context.Background(), "query", "", 10)
	if len(results) != 0 {
		t.Errorf("expected 0 results from not-ready git source, got %d", len(results))
	}
}

func TestStats_WithVaultsAndGitSources(t *testing.T) {
	server := coverageWikiServer()
	defer server.Close()

	logger := coverageTestLogger()
	layers := []layerClient{
		{layerType: LayerProject, client: NewClient(server.URL, logger)},
	}

	vaultDir := createVaultDir(t)
	store, _ := NewFileStore(vaultDir, "test-vault", logger)

	gs := makeReadyGitSource(t, LayerOrg)

	api := &KnowledgeAPI{
		layers:     layers,
		config:     KnowledgeConfig{Enabled: true, Engine: "llm-wiki"},
		vaults:     []*FileStore{store},
		gitSources: []*GitSource{gs},
		logger:     logger,
	}

	stats := api.Stats(context.Background())
	if stats == nil {
		t.Fatal("expected non-nil stats")
	}
	if stats["enabled"] != true {
		t.Error("expected enabled=true")
	}
	layerStats, ok := stats["layers"].([]map[string]interface{})
	if !ok {
		t.Fatal("layers not found or wrong type")
	}
	// Should have wiki layer + vault + git source
	if len(layerStats) < 3 {
		t.Errorf("expected at least 3 layer stats, got %d", len(layerStats))
	}
}

func TestTriggerVaultReindex_ExistingVault(t *testing.T) {
	logger := coverageTestLogger()
	dir := createVaultDir(t)
	store, _ := NewFileStore(dir, "test-vault", logger)

	api := &KnowledgeAPI{
		config: KnowledgeConfig{Enabled: true},
		vaults: []*FileStore{store},
		logger: logger,
	}

	// Add a new file and trigger reindex
	os.WriteFile(dir+"/new-fact.md", []byte("# New Fact\nNew content"), 0o644)
	api.triggerVaultReindex(dir)

	// The vault should have been reindexed — new file should appear
	facts := store.ListPages("")
	if len(facts) < 4 {
		t.Errorf("expected at least 4 pages after reindex, got %d", len(facts))
	}
}

func TestTriggerVaultReindex_NewVault(t *testing.T) {
	logger := coverageTestLogger()

	api := &KnowledgeAPI{
		config: KnowledgeConfig{Enabled: true},
		logger: logger,
	}

	// Create a new vault dir
	newDir := t.TempDir()
	os.WriteFile(newDir+"/fact.md", []byte("# Fact\nContent"), 0o644)

	// Should create a new vault since dir doesn't match any existing
	api.triggerVaultReindex(newDir)

	if len(api.vaults) != 1 {
		t.Errorf("expected 1 vault after trigger, got %d", len(api.vaults))
	}
}

func TestListIdeationFacts_WithIdeation(t *testing.T) {
	// Create a server that returns ideation-type facts
	mux := http.NewServeMux()
	mux.HandleFunc("/api/pages", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(searchResponse{
			Results: []searchResult{
				{Slug: "idea-1", Title: "Big Idea", Type: "idea"},
				{Slug: "vision-1", Title: "Project Vision", Type: "vision"},
				{Slug: "pattern-1", Title: "A Pattern", Type: "pattern"},
				{Slug: "const-1", Title: "Constitution", Type: "constitution"},
			},
			Total: 4,
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	logger := coverageTestLogger()
	client := NewClient(server.URL, logger)
	api := &KnowledgeAPI{
		config: KnowledgeConfig{Enabled: true},
		layers: []layerClient{
			{layerType: LayerProject, client: client},
		},
		logger: logger,
	}

	facts := api.ListIdeationFacts(context.Background())
	// idea, vision, constitution are ideation types; pattern is not
	if len(facts) != 3 {
		t.Errorf("expected 3 ideation facts, got %d", len(facts))
		for _, f := range facts {
			t.Logf("  %s (%s)", f.Title, f.Type)
		}
	}
}

func TestListIdeationFacts_Empty(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/pages", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(searchResponse{
			Results: []searchResult{
				{Slug: "p1", Title: "Pattern", Type: "pattern"},
			},
			Total: 1,
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	logger := coverageTestLogger()
	client := NewClient(server.URL, logger)
	api := &KnowledgeAPI{
		config: KnowledgeConfig{Enabled: true},
		layers: []layerClient{
			{layerType: LayerProject, client: client},
		},
		logger: logger,
	}

	facts := api.ListIdeationFacts(context.Background())
	if len(facts) != 0 {
		t.Errorf("expected 0 ideation facts, got %d", len(facts))
	}
}

func TestGetConstitution_Found(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/pages", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(searchResponse{
			Results: []searchResult{
				{Slug: "const-1", Title: "Project Constitution", Type: "constitution"},
			},
			Total: 1,
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	logger := coverageTestLogger()
	client := NewClient(server.URL, logger)
	api := &KnowledgeAPI{
		config: KnowledgeConfig{Enabled: true},
		layers: []layerClient{
			{layerType: LayerProject, client: client},
		},
		logger: logger,
	}

	fact := api.GetConstitution(context.Background())
	if fact == nil {
		t.Fatal("expected non-nil constitution fact")
	}
	if fact.Title != "Project Constitution" {
		t.Errorf("title = %q", fact.Title)
	}
}

func TestGetConstitution_NotFound(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/pages", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(searchResponse{Results: []searchResult{}, Total: 0})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	logger := coverageTestLogger()
	client := NewClient(server.URL, logger)
	api := &KnowledgeAPI{
		config: KnowledgeConfig{Enabled: true},
		layers: []layerClient{
			{layerType: LayerProject, client: client},
		},
		logger: logger,
	}

	fact := api.GetConstitution(context.Background())
	if fact != nil {
		t.Error("expected nil when no constitution exists")
	}
}

func TestGitSources_Info(t *testing.T) {
	logger := coverageTestLogger()
	gs := makeReadyGitSource(t, LayerOrg)

	api := &KnowledgeAPI{
		config:     KnowledgeConfig{Enabled: true},
		gitSources: []*GitSource{gs},
		logger:     logger,
	}

	infos := api.GitSources()
	if len(infos) != 1 {
		t.Fatalf("expected 1 git source info, got %d", len(infos))
	}
	if infos[0].Name != "test-git" {
		t.Errorf("name = %q", infos[0].Name)
	}
	if !infos[0].Ready {
		t.Error("expected Ready=true")
	}
}

func TestGetGitSourceStore_Found(t *testing.T) {
	logger := coverageTestLogger()
	gs := makeReadyGitSource(t, LayerOrg)

	api := &KnowledgeAPI{
		config:     KnowledgeConfig{Enabled: true},
		gitSources: []*GitSource{gs},
		logger:     logger,
	}

	store := api.GetGitSourceStore("test-git")
	if store == nil {
		t.Error("expected non-nil store")
	}
}

func TestGetGitSourceStore_NotFound(t *testing.T) {
	logger := coverageTestLogger()
	api := &KnowledgeAPI{
		config: KnowledgeConfig{Enabled: true},
		logger: logger,
	}

	store := api.GetGitSourceStore("nonexistent")
	if store != nil {
		t.Error("expected nil for unknown source")
	}
}

func TestDisconnectGitSource_Found(t *testing.T) {
	logger := coverageTestLogger()
	gs := makeReadyGitSource(t, LayerOrg)

	api := &KnowledgeAPI{
		config:     KnowledgeConfig{Enabled: true},
		gitSources: []*GitSource{gs},
		logger:     logger,
	}

	err := api.DisconnectGitSource("https://example.com/repo.git", "")
	if err != nil {
		t.Fatalf("DisconnectGitSource: %v", err)
	}
	if len(api.gitSources) != 0 {
		t.Error("expected 0 git sources after disconnect")
	}
}

func TestDisconnectGitSource_NotFound(t *testing.T) {
	logger := coverageTestLogger()
	api := &KnowledgeAPI{
		config: KnowledgeConfig{Enabled: true},
		logger: logger,
	}

	err := api.DisconnectGitSource("https://example.com/nope.git", "")
	if err == nil {
		t.Error("expected error for unknown git source")
	}
}

func TestCreateIdeationFact_Valid(t *testing.T) {
	server := coverageWikiServer()
	defer server.Close()

	api := coverageKnowledgeAPI(server.URL)
	err := api.CreateIdeationFact(context.Background(), CreateFactRequest{
		Title: "My Idea",
		Body:  "Idea description",
		Type:  "idea",
	})
	if err != nil {
		t.Fatalf("CreateIdeationFact: %v", err)
	}
}

func TestCreateIdeationFact_NonIdeationType(t *testing.T) {
	server := coverageWikiServer()
	defer server.Close()

	api := coverageKnowledgeAPI(server.URL)
	err := api.CreateIdeationFact(context.Background(), CreateFactRequest{
		Title: "A Pattern",
		Body:  "Not ideation",
		Type:  "pattern",
	})
	if err == nil {
		t.Error("expected error for non-ideation type")
	}
}

func TestCreateIdeationFact_DefaultLayer(t *testing.T) {
	server := coverageWikiServer()
	defer server.Close()

	api := coverageKnowledgeAPI(server.URL)
	err := api.CreateIdeationFact(context.Background(), CreateFactRequest{
		Title: "Vision",
		Body:  "Project vision",
		Type:  "vision",
		// Layer intentionally empty — should default to project
	})
	if err != nil {
		t.Fatalf("CreateIdeationFact with default layer: %v", err)
	}
}

func TestSearchAllWithVaults_NoVaults(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/search", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(searchResponse{
			Results: []searchResult{{Slug: "f1", Title: "Layer Result"}},
			Total:   1,
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	logger := coverageTestLogger()
	client := NewClient(server.URL, logger)
	api := &KnowledgeAPI{
		config: KnowledgeConfig{Enabled: true},
		layers: []layerClient{
			{layerType: "personal", client: client},
		},
		logger: logger,
	}

	results := api.SearchAllWithVaults(context.Background(), "query", "", 10)
	if len(results) != 1 {
		t.Errorf("expected 1 result, got %d", len(results))
	}
}

func TestPrimer_Prime_EmptyQuery(t *testing.T) {
	logger := coverageTestLogger()
	cfg := PrimerConfig{MaxFacts: 10}
	p := NewPrimer(nil, cfg, logger)
	// Both filePaths and keywords empty — should return empty PrimedKnowledge
	result := p.Prime(context.Background(), nil, nil)
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if len(result.Facts) != 0 {
		t.Errorf("expected 0 facts, got %d", len(result.Facts))
	}
}

func TestFileStore_NonMarkdownSkipped(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(dir+"/readme.txt", []byte("not markdown"), 0o644)
	os.WriteFile(dir+"/notes.md", []byte("# Notes\nActual markdown"), 0o644)

	logger := coverageTestLogger()
	store, err := NewFileStore(dir, "test", logger)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	// Only the .md file should be indexed — use ListPages instead of Search
	facts := store.ListPages("")
	if len(facts) != 1 {
		t.Errorf("expected 1 fact (only .md), got %d", len(facts))
	}
}

func TestFileStore_UnreadableFileSkipped(t *testing.T) {
	dir := t.TempDir()
	// Create a valid .md file
	os.WriteFile(dir+"/good.md", []byte("# Good\nContent"), 0o644)
	// Create an unreadable .md file
	path := dir + "/bad.md"
	os.WriteFile(path, []byte("# Bad\nContent"), 0o000)

	logger := coverageTestLogger()
	store, err := NewFileStore(dir, "test", logger)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	// Use ListPages — at least the readable file should be indexed
	facts := store.ListPages("")
	if len(facts) < 1 {
		t.Errorf("expected at least 1 fact, got %d", len(facts))
	}
}

func TestFileStore_RefreshIfStale(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(dir+"/page.md", []byte("# Page\nContent"), 0o644)

	logger := coverageTestLogger()
	store, err := NewFileStore(dir, "test", logger)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	// Force the index to be stale by setting lastIndexed to the past
	store.mu.Lock()
	store.lastIndexed = time.Now().Add(-2 * indexRefreshInterval)
	store.mu.Unlock()

	// Add a new file
	os.WriteFile(dir+"/new.md", []byte("# New\nNew content"), 0o644)

	// Search should trigger refreshIfStale
	facts := store.Search("New", 10)
	if len(facts) != 1 {
		t.Errorf("expected 1 result after stale refresh, got %d", len(facts))
	}
}
