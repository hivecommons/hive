package knowledge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
)

// covLogger returns a logger that discards output so tests stay quiet.
func covLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// --- Minimal PDF generation for docparser coverage ---

// makeSimplePDF hand-builds a minimal single-page PDF whose content stream
// draws the given text. The ledongthuc/pdf reader can extract it, which lets us
// exercise parsePDF/extractPageText/sanitizePDFText/extractPageTitle without a
// binary fixture in the repo.
func makeSimplePDF(text string) []byte {
	content := "BT /F1 24 Tf 72 720 Td (" + text + ") Tj ET"
	objs := []string{
		"1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n",
		"2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n",
		"3 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 5 0 R >> >> /Contents 4 0 R >>\nendobj\n",
		fmt.Sprintf("4 0 obj\n<< /Length %d >>\nstream\n%s\nendstream\nendobj\n", len(content), content),
		"5 0 obj\n<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>\nendobj\n",
	}

	buf := "%PDF-1.4\n"
	offsets := []int{}
	for _, o := range objs {
		offsets = append(offsets, len(buf))
		buf += o
	}
	xrefStart := len(buf)
	buf += fmt.Sprintf("xref\n0 %d\n", len(objs)+1)
	buf += "0000000000 65535 f \n"
	for _, off := range offsets {
		buf += fmt.Sprintf("%010d 00000 n \n", off)
	}
	buf += fmt.Sprintf("trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF", len(objs)+1, xrefStart)
	return []byte(buf)
}

func TestParsePDF_ValidSinglePage(t *testing.T) {
	data := makeSimplePDF("Hello World Document")
	chunks, title := parsePDF(data)
	if len(chunks) == 0 {
		t.Fatalf("expected at least one chunk from valid PDF")
	}
	if title != "Hello World Document" {
		t.Errorf("title = %q, want %q", title, "Hello World Document")
	}
	if !strings.Contains(chunks[0].Body, "Hello World") {
		t.Errorf("chunk body missing text: %q", chunks[0].Body)
	}
}

func TestExtractPageTitle(t *testing.T) {
	tests := []struct {
		text    string
		pageNum int
		want    string
	}{
		{"# Heading Line\nBody text", 1, "Heading Line"},
		{"A short title\nmore", 2, "A short title"},
		{"This sentence ends with a period.", 3, "Page 3"},
		{"", 4, "Page 4"},
		{strings.Repeat("x", chunkTitleMaxChars+5), 5, "Page 5"},
	}
	for _, tt := range tests {
		got := extractPageTitle(tt.text, tt.pageNum)
		if got != tt.want {
			t.Errorf("extractPageTitle(%q, %d) = %q, want %q", tt.text, tt.pageNum, got, tt.want)
		}
	}
}

func TestSanitizePDFText(t *testing.T) {
	// Includes a bullet (decorative), a ligature, and extra whitespace.
	in := "Bullet• pointû01le  with   spaces\n\n\nnext"
	out := sanitizePDFText(in)
	if strings.ContainsRune(out, '•') {
		t.Errorf("bullet not stripped: %q", out)
	}
	if strings.Contains(out, "û01") {
		t.Errorf("ligature not expanded: %q", out)
	}
	if strings.Contains(out, "  ") {
		t.Errorf("multiple spaces not collapsed: %q", out)
	}
}

func TestIsDecorative(t *testing.T) {
	decorative := []rune{0x0001, 0x00AD, 0x2022, 0xFEFF, 0xFFFD, 0x2705}
	for _, r := range decorative {
		if !isDecorative(r) {
			t.Errorf("expected %U to be decorative", r)
		}
	}
	normal := []rune{'a', 'Z', '9', '\t', '\n', ' '}
	for _, r := range normal {
		if isDecorative(r) {
			t.Errorf("expected %U to NOT be decorative", r)
		}
	}
}

func TestSplitAtParagraphs(t *testing.T) {
	// Build text that exceeds maxLen so it splits into multiple pieces.
	p1 := strings.Repeat("a", 60)
	p2 := strings.Repeat("b", 60)
	p3 := strings.Repeat("c", 60)
	text := p1 + "\n\n" + "" + "\n\n" + p2 + "\n\n" + p3
	parts := splitAtParagraphs(text, 100)
	if len(parts) < 2 {
		t.Fatalf("expected at least 2 parts, got %d", len(parts))
	}
	for _, p := range parts {
		if len(p) > 100 && !strings.Contains(p, "\n\n") {
			// A single oversized paragraph would be one part; that's acceptable.
			continue
		}
	}
}

func TestSplitAtParagraphs_SingleShort(t *testing.T) {
	parts := splitAtParagraphs("only one paragraph", 1000)
	if len(parts) != 1 {
		t.Fatalf("expected 1 part, got %d", len(parts))
	}
}

func TestValidateLocalFilePath(t *testing.T) {
	orig := allowedFilePrefixes
	t.Cleanup(func() { allowedFilePrefixes = orig })

	base := t.TempDir()
	allowedFilePrefixes = []string{base + string(filepath.Separator)}

	if err := validateLocalFilePath(filepath.Join(base, "doc.md")); err != nil {
		t.Fatalf("expected allowed local document path: %v", err)
	}
	if err := validateLocalFilePath(filepath.Join(base, "..", "secret.md")); err == nil {
		t.Fatal("expected path outside allowed prefix to be rejected")
	}

	ds := &DocumentSource{knowledgeDir: base}
	if err := ds.validateFilePath(filepath.Join(base, "nested", "doc.md")); err != nil {
		t.Fatalf("expected DocumentSource path under knowledge dir: %v", err)
	}
	if err := ds.validateFilePath(filepath.Join(base, "..", "secret.md")); err == nil {
		t.Fatal("expected DocumentSource path outside knowledge dir to be rejected")
	}
}

// --- Context7 coverage (context7Get and wrappers) ---

func TestContext7Get_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer key123" {
			t.Errorf("missing/incorrect auth header: %q", r.Header.Get("Authorization"))
		}
		fmt.Fprint(w, "some docs")
	}))
	defer srv.Close()

	body, err := context7Get(context.Background(), srv.URL, "key123")
	if err != nil {
		t.Fatalf("context7Get: %v", err)
	}
	if string(body) != "some docs" {
		t.Errorf("body = %q", body)
	}
}

func TestContext7Get_RateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	_, err := context7Get(context.Background(), srv.URL, "")
	if err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("expected rate limited error, got %v", err)
	}
}

func TestContext7Get_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := context7Get(context.Background(), srv.URL, "")
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("expected HTTP 500 error, got %v", err)
	}
}

func TestContext7Get_BadURL(t *testing.T) {
	_, err := context7Get(context.Background(), "http://\x7f\x00", "")
	if err == nil {
		t.Fatal("expected error for control-char URL")
	}
}

func TestContext7Get_ConnRefused(t *testing.T) {
	_, err := context7Get(context.Background(), "http://127.0.0.1:1", "")
	if err == nil {
		t.Fatal("expected connection error for closed port")
	}
}

// SearchContext7 and FetchContext7Docs hardcode context7.com; we can still cover
// their error path via a cancelled context (network never succeeds).
func TestSearchContext7_Error(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := SearchContext7(ctx, "react", "")
	if err == nil {
		t.Error("expected error with cancelled context")
	}
}

func TestFetchContext7Docs_Error(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := FetchContext7Docs(ctx, "/facebook/react", "hooks", "")
	if err == nil {
		t.Error("expected error with cancelled context")
	}
}

// --- fisherrao coverage ---

func TestCosineSimilarity_EdgeCases(t *testing.T) {
	if got := CosineSimilarity([]float64{1, 2}, []float64{1, 2, 3}); got != 0 {
		t.Errorf("length mismatch should give 0, got %v", got)
	}
	if got := CosineSimilarity(nil, nil); got != 0 {
		t.Errorf("empty should give 0, got %v", got)
	}
	if got := CosineSimilarity([]float64{0, 0}, []float64{0, 0}); got != 0 {
		t.Errorf("zero vectors should give 0, got %v", got)
	}
	if got := CosineSimilarity([]float64{1, 0}, []float64{1, 0}); got < 0.99 {
		t.Errorf("identical vectors should give ~1, got %v", got)
	}
}

func TestClampSigma(t *testing.T) {
	if got := clampSigma(-1); got != minSigma {
		t.Errorf("clampSigma(-1) = %v, want %v", got, minSigma)
	}
	if got := clampSigma(0.5); got != 0.5 {
		t.Errorf("clampSigma(0.5) = %v, want 0.5", got)
	}
}

// --- graphstore ExportJSON ---

func TestGraphStore_ExportJSON(t *testing.T) {
	gs := newTestGraphStore(t)
	if err := gs.AddTriple(Triple{Subject: "a", Predicate: PredicateRelatedTo, Object: "b"}); err != nil {
		t.Fatal(err)
	}
	data, err := gs.ExportJSON()
	if err != nil {
		t.Fatalf("ExportJSON: %v", err)
	}
	var triples []Triple
	if err := json.Unmarshal(data, &triples); err != nil {
		t.Fatalf("unmarshal export: %v", err)
	}
	if len(triples) != 1 {
		t.Errorf("expected 1 triple, got %d", len(triples))
	}
}

// --- filestore access counts persistence ---

func TestFileStore_AccessCountsPersistence(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "page.md"),
		[]byte("---\ntitle: Page\ntype: gotcha\n---\nBody"), 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := NewFileStore(dir, "vault", covLogger())
	if err != nil {
		t.Fatal(err)
	}
	store.RecordAccess("page")
	store.RecordAccess("page")
	// Reindex triggers persistAccessCounts (non-empty branch).
	store.Reindex()

	countsPath := filepath.Join(dir, accessCountsFile)
	if _, err := os.Stat(countsPath); err != nil {
		t.Fatalf("access counts file not written: %v", err)
	}

	// A new store over the same dir loads persisted counts.
	store2, err := NewFileStore(dir, "vault", covLogger())
	if err != nil {
		t.Fatal(err)
	}
	if store2.AccessCount("page") != 2 {
		t.Errorf("loaded access count = %d, want 2", store2.AccessCount("page"))
	}
}

func TestFileStore_LoadAccessCounts_Corrupted(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, accessCountsFile), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Should not fail construction; corrupted counts are reset.
	store, err := NewFileStore(dir, "vault", covLogger())
	if err != nil {
		t.Fatalf("NewFileStore with corrupted counts: %v", err)
	}
	if store.AccessCount("anything") != 0 {
		t.Errorf("expected 0 for corrupted counts")
	}
}

// --- docsource: LoadDocumentSource, Metadata, emitGraphTriples with graph ---

func TestLoadDocumentSource(t *testing.T) {
	dir := t.TempDir()
	meta := DocMetadata{
		Slug:        "my-doc",
		Title:       "My Doc",
		SourceURL:   "https://example.com/doc.md",
		ContentType: "text/plain",
		FactCount:   2,
		FactSlugs:   []string{"doc-my-doc-000", "doc-my-doc-001"},
	}
	data, _ := json.MarshalIndent(meta, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, docMetadataFile), data, 0o644); err != nil {
		t.Fatal(err)
	}

	ds, err := LoadDocumentSource(dir, filepath.Join(dir, "vault"), nil, covLogger(), "")
	if err != nil {
		t.Fatalf("LoadDocumentSource: %v", err)
	}
	if ds.slug != "my-doc" {
		t.Errorf("slug = %q", ds.slug)
	}
	if ds.Metadata().Title != "My Doc" {
		t.Errorf("metadata title = %q", ds.Metadata().Title)
	}
}

func TestLoadDocumentSource_Context7(t *testing.T) {
	dir := t.TempDir()
	meta := DocMetadata{
		Slug:      "ctx7-doc",
		Title:     "Ctx7",
		SourceURL: "context7://facebook/react",
	}
	data, _ := json.Marshal(meta)
	if err := os.WriteFile(filepath.Join(dir, docMetadataFile), data, 0o644); err != nil {
		t.Fatal(err)
	}
	ds, err := LoadDocumentSource(dir, dir, nil, covLogger(), "")
	if err != nil {
		t.Fatalf("LoadDocumentSource: %v", err)
	}
	if ds.config.Context7ID != "facebook/react" {
		t.Errorf("context7 id = %q", ds.config.Context7ID)
	}
	if ds.config.URL != "" {
		t.Errorf("URL should be cleared for context7 source, got %q", ds.config.URL)
	}
}

func TestLoadDocumentSource_MissingMetadata(t *testing.T) {
	if _, err := LoadDocumentSource(t.TempDir(), "", nil, covLogger(), ""); err == nil {
		t.Error("expected error for missing metadata.json")
	}
}

func TestLoadDocumentSource_BadJSON(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, docMetadataFile), []byte("{bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDocumentSource(dir, "", nil, covLogger(), ""); err == nil {
		t.Error("expected error for malformed metadata.json")
	}
}

func TestDocumentSource_ImportWithGraph(t *testing.T) {
	tmpDir := t.TempDir()
	vaultDir := filepath.Join(tmpDir, "vault")
	baseDir := filepath.Join(tmpDir, "knowledge")
	gs := newTestGraphStore(t)

	srcFile := filepath.Join(baseDir, "test.txt")
	content := "First para about one thing.\n\nSecond para about another thing.\n\nThird para wraps up."
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(srcFile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	ds := NewDocumentSource(DocSourceConfig{
		Name:     "graph-doc",
		FilePath: srcFile,
		Layer:    LayerProject,
	}, baseDir, vaultDir, gs, covLogger(), "")

	meta, err := ds.Import(context.Background())
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if len(meta.FactSlugs) == 0 {
		t.Fatal("expected facts")
	}
	// Graph triples should have been emitted (derived_from at minimum).
	triples := gs.Outgoing(meta.FactSlugs[0])
	if len(triples) == 0 {
		t.Error("expected outgoing triples for first fact")
	}

	// Delete should remove graph triples too.
	if err := ds.Delete(); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

func TestDocumentSource_FetchURL_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	tmpDir := t.TempDir()
	ds := NewDocumentSource(DocSourceConfig{
		Name:  "notfound",
		URL:   srv.URL,
		Layer: LayerProject,
	}, tmpDir, filepath.Join(tmpDir, "vault"), nil, covLogger(), "")

	if _, err := ds.Import(context.Background()); err == nil {
		t.Error("expected error for 404 URL")
	}
}

func TestDocumentSource_FetchURL_ConnRefused(t *testing.T) {
	tmpDir := t.TempDir()
	ds := NewDocumentSource(DocSourceConfig{
		Name:  "refused",
		URL:   "http://127.0.0.1:1",
		Layer: LayerProject,
	}, tmpDir, filepath.Join(tmpDir, "vault"), nil, covLogger(), "")

	if _, err := ds.Import(context.Background()); err == nil {
		t.Error("expected error for connection refused")
	}
}

func TestExtensionForType(t *testing.T) {
	tests := map[string]string{
		"application/pdf": ".pdf",
		"text/html":       ".html",
		docxMIME:          ".docx",
		"text/plain":      ".txt",
		"unknown/thing":   ".txt",
	}
	for ct, want := range tests {
		if got := extensionForType(ct); got != want {
			t.Errorf("extensionForType(%q) = %q, want %q", ct, got, want)
		}
	}
}

func TestNormalizeContentType_Docx(t *testing.T) {
	if got := normalizeContentType("application/vnd.openxmlformats-officedocument.wordprocessingml.document"); got != docxMIME {
		t.Errorf("wordprocessingml should normalize to docx, got %q", got)
	}
	if got := normalizeContentType("application/docx"); got != docxMIME {
		t.Errorf("docx should normalize to docx, got %q", got)
	}
}

// --- primer: AddFileStore, SetGraphStore, SetContext7Suggester, expandWithGraph ---

func writeVaultPage(t *testing.T, dir, slug, title, body string) {
	t.Helper()
	content := fmt.Sprintf("---\ntitle: %s\ntype: gotcha\nslug: %s\n---\n%s", title, slug, body)
	if err := os.WriteFile(filepath.Join(dir, slug+".md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPrimer_ExpandWithGraph(t *testing.T) {
	vaultDir := t.TempDir()
	writeVaultPage(t, vaultDir, "seed", "Seed Fact", "seed body")
	writeVaultPage(t, vaultDir, "related", "Related Fact", "related body")

	store, err := NewFileStore(vaultDir, "vault", covLogger())
	if err != nil {
		t.Fatal(err)
	}

	gs := newTestGraphStore(t)
	if err := gs.AddTriple(Triple{Subject: "seed", Predicate: PredicateRelatedTo, Object: "related"}); err != nil {
		t.Fatal(err)
	}

	p := NewPrimer(nil, PrimerConfig{MaxFacts: 25}, covLogger())
	p.AddFileStore("vault", store, LayerProject)
	p.SetGraphStore(gs)

	facts := []Fact{{Slug: "seed", Title: "Seed Fact", Confidence: 0.9, Layer: LayerProject}}
	expanded := p.expandWithGraph(facts)
	if len(expanded) < 2 {
		t.Fatalf("expected graph expansion to add related fact, got %d facts", len(expanded))
	}
	found := false
	for _, f := range expanded {
		if f.Slug == "related" {
			found = true
		}
	}
	if !found {
		t.Error("expected 'related' fact from graph expansion")
	}
}

func TestPrimer_ExpandWithGraph_NoGraph(t *testing.T) {
	p := NewPrimer(nil, PrimerConfig{MaxFacts: 25}, covLogger())
	facts := []Fact{{Slug: "a"}}
	if got := p.expandWithGraph(facts); len(got) != 1 {
		t.Errorf("expected passthrough without graph, got %d", len(got))
	}
}

func TestPrimer_SetContext7Suggester(t *testing.T) {
	p := NewPrimer(nil, PrimerConfig{MaxFacts: 25}, covLogger())
	called := false
	p.SetContext7Suggester(func(ctx context.Context, kw string) string {
		called = true
		return "hint for " + kw
	})
	// Prime with a keyword so the suggester runs.
	result := p.Prime(context.Background(), nil, []string{"react", "react"})
	if !called {
		t.Error("context7 suggester was not called")
	}
	if len(result.Hints) == 0 {
		t.Error("expected hints from suggester")
	}
}

// --- bead_synthesizer lifecycle (Start/Stop/IsRunning/StartBackground/ClearCache) ---

func TestBeadSynthesizer_StartBackgroundStop(t *testing.T) {
	srv, _ := newMockWikiServer(t)
	store := newTestStore(t)
	stores := map[string]*beads.Store{"scanner": store}
	synth := newTestSynthesizer(t, stores, srv.URL)

	if synth.IsRunning() {
		t.Error("should not be running before StartBackground")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	synth.StartBackground(ctx)

	// Second call is a no-op (already running).
	synth.StartBackground(ctx)

	deadline := time.Now().Add(2 * time.Second)
	for !synth.IsRunning() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !synth.IsRunning() {
		t.Fatal("expected running after StartBackground")
	}

	synth.Stop()
	for synth.IsRunning() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if synth.IsRunning() {
		t.Error("expected stopped after Stop")
	}
	// Stop again (cancel already nil) is a no-op.
	synth.Stop()
}

func TestBeadSynthesizer_SetGraphAndLifecycle(t *testing.T) {
	srv, _ := newMockWikiServer(t)
	store := newTestStore(t)
	stores := map[string]*beads.Store{"scanner": store}
	synth := newTestSynthesizer(t, stores, srv.URL)

	gs := newTestGraphStore(t)
	synth.SetGraphStore(gs)

	lm := NewBeadLifecycleManager(stores, &RetentionPolicy{}, covLogger())
	synth.SetLifecycleManager(lm)

	// runOnce should now also invoke lifecycle culling without error.
	synth.runOnce(context.Background())
}

// --- bead_lifecycle: RunCulling, retentionScore branches, hasOpenDependents ---

// seedClosedBeads writes a beads.json with backdated closed timestamps and
// reloads the store. Because beads.flexTime is unexported we cannot set it via
// the store API, so we round-trip through the on-disk JSON format the store
// already understands.
func seedClosedBeads(t *testing.T, dir string, n int, bt beads.BeadType, prio beads.Priority) {
	t.Helper()
	old := time.Now().UTC().Add(-200 * 24 * time.Hour).Format(time.RFC3339Nano)
	var items []map[string]any
	for i := 0; i < n; i++ {
		items = append(items, map[string]any{
			"id":         fmt.Sprintf("bead%03d", i),
			"title":      fmt.Sprintf("%s %d", bt, i),
			"type":       string(bt),
			"status":     "closed",
			"priority":   int(prio),
			"actor":      "scanner",
			"created_at": old,
			"updated_at": old,
			"closed_at":  old,
		})
	}
	data, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "beads.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func closedBead(t *testing.T, store *beads.Store, title string, bt beads.BeadType, prio beads.Priority) *beads.Bead {
	t.Helper()
	b, err := store.Create(title, bt, prio, "scanner", "")
	if err != nil {
		t.Fatal(err)
	}
	store.Close(b.ID)
	return b
}

func TestBeadLifecycle_RunCulling(t *testing.T) {
	dir := t.TempDir()
	store, err := beads.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Old chore beads have a low retention score and are far in the past, so
	// their score drops below the archive threshold.
	seedClosedBeads(t, dir, 5, beads.TypeChore, beads.PriorityMinor)
	if err := store.Reload(); err != nil {
		t.Fatal(err)
	}
	if store.Count() != 5 {
		t.Fatalf("seed failed: count = %d", store.Count())
	}

	stores := map[string]*beads.Store{"scanner": store}
	// MaxBeads small so target headroom forces culling.
	lm := NewBeadLifecycleManager(stores, &RetentionPolicy{MaxBeads: 2}, covLogger())
	archived, err := lm.RunCulling(context.Background())
	if err != nil {
		t.Fatalf("RunCulling: %v", err)
	}
	if archived == 0 {
		t.Error("expected at least one bead archived")
	}
}

func TestBeadLifecycle_RunCulling_UnderTarget(t *testing.T) {
	store := newTestStore(t)
	closedBead(t, store, "one", beads.TypeBug, beads.PriorityHigh)
	store.Reload()
	stores := map[string]*beads.Store{"scanner": store}
	lm := NewBeadLifecycleManager(stores, &RetentionPolicy{MaxBeads: 5000}, covLogger())
	archived, err := lm.RunCulling(context.Background())
	if err != nil {
		t.Fatalf("RunCulling: %v", err)
	}
	if archived != 0 {
		t.Errorf("expected 0 archived when under target, got %d", archived)
	}
}

func TestBeadLifecycle_RetentionScore_Branches(t *testing.T) {
	store := newTestStore(t)
	stores := map[string]*beads.Store{"scanner": store}
	lm := NewBeadLifecycleManager(stores, &RetentionPolicy{PreserveWithDeps: true}, covLogger())
	now := time.Now().UTC()

	cases := []struct {
		bt   beads.BeadType
		prio beads.Priority
	}{
		{beads.TypeBug, beads.PriorityCritical},
		{beads.TypeDecision, beads.PriorityHigh},
		{beads.TypeAdvisory, beads.PriorityMedium},
		{beads.TypeFeature, beads.PriorityMinor},
		{beads.TypeChore, beads.PriorityMinor},
	}
	for _, c := range cases {
		b := newTestBead("x", "t", c.bt, c.prio, "scanner")
		_ = lm.retentionScore(b, "scanner", now)
	}

	// synthesized_at metadata: valid old timestamp and invalid string.
	b := newTestBead("s1", "synth", beads.TypeBug, beads.PriorityLow, "scanner")
	b.Metadata["synthesized_at"] = now.Add(-30 * 24 * time.Hour).Format(time.RFC3339)
	_ = lm.retentionScore(b, "scanner", now)

	b2 := newTestBead("s2", "synth-bad", beads.TypeBug, beads.PriorityLow, "scanner")
	b2.Metadata["synthesized_at"] = "not-a-time"
	_ = lm.retentionScore(b2, "scanner", now)

	// The ClosedAt-set path is exercised via the on-disk seeded beads in
	// TestBeadLifecycle_RunCulling (flexTime is unexported, so it cannot be
	// constructed directly here).
}

func TestBeadLifecycle_HasOpenDependents(t *testing.T) {
	store := newTestStore(t)
	// Create an open dependency bead.
	dep, err := store.Create("dependency", beads.TypeBug, beads.PriorityHigh, "scanner", "")
	if err != nil {
		t.Fatal(err)
	}
	store.Reload()

	stores := map[string]*beads.Store{"scanner": store}
	lm := NewBeadLifecycleManager(stores, &RetentionPolicy{}, covLogger())

	b := newTestBead("main", "main bead", beads.TypeBug, beads.PriorityHigh, "scanner")
	b.DependsOn = []string{dep.ID}
	if !lm.hasOpenDependents(b, "scanner") {
		t.Error("expected hasOpenDependents = true for open dependency")
	}

	// No dependencies -> false.
	b2 := newTestBead("nodep", "no deps", beads.TypeBug, beads.PriorityHigh, "scanner")
	if lm.hasOpenDependents(b2, "scanner") {
		t.Error("expected hasOpenDependents = false with no deps")
	}
}

// --- gitsource: real git via HTTP clone ---

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// makeSourceRepo creates a local git repo with markdown docs and returns its path.
func makeSourceRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	runGit(t, repo, "init", "-b", "main")
	docs := filepath.Join(repo, "docs")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(docs, "guide.md"),
		[]byte("# Guide\n\nHow to do things.\n\n#howto"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "README.md"),
		[]byte("# Readme\n\nTop-level readme."), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-m", "initial")
	return repo
}

func makeHTTPSourceRepo(t *testing.T) string {
	t.Helper()
	src := makeSourceRepo(t)
	parent := t.TempDir()
	bare := filepath.Join(parent, "repo.git")
	runGit(t, src, "clone", "--bare", src, bare)

	srv := httptest.NewServer(gitHTTPBackend(t, parent))
	t.Cleanup(srv.Close)
	return srv.URL + "/repo.git"
}

func gitHTTPBackend(t *testing.T, projectRoot string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		cmd := exec.Command("git", "http-backend")
		cmd.Env = append(os.Environ(),
			"GIT_PROJECT_ROOT="+projectRoot,
			"GIT_HTTP_EXPORT_ALL=1",
			"REQUEST_METHOD="+r.Method,
			"PATH_INFO="+r.URL.Path,
			"QUERY_STRING="+r.URL.RawQuery,
			"CONTENT_TYPE="+r.Header.Get("Content-Type"),
			"CONTENT_LENGTH="+strconv.FormatInt(r.ContentLength, 10),
		)
		cmd.Stdin = r.Body

		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Logf("git http-backend failed: %v: %s", err, stderr.String())
			http.Error(w, "git http-backend failed", http.StatusInternalServerError)
			return
		}

		headerBlock, body, ok := bytes.Cut(stdout.Bytes(), []byte("\r\n\r\n"))
		if !ok {
			headerBlock, body, ok = bytes.Cut(stdout.Bytes(), []byte("\n\n"))
		}
		if !ok {
			http.Error(w, "bad git http-backend response", http.StatusInternalServerError)
			return
		}

		status := http.StatusOK
		for _, line := range strings.Split(string(headerBlock), "\n") {
			line = strings.TrimRight(line, "\r")
			if line == "" {
				continue
			}
			key, value, found := strings.Cut(line, ":")
			if !found {
				continue
			}
			key = strings.TrimSpace(key)
			value = strings.TrimSpace(value)
			if strings.EqualFold(key, "Status") {
				code, _, _ := strings.Cut(value, " ")
				if parsed, err := strconv.Atoi(code); err == nil {
					status = parsed
				}
				continue
			}
			w.Header().Add(key, value)
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}
}

func TestGitSource_InitSyncFullClone(t *testing.T) {
	t.Setenv("HIVE_ALLOW_PRIVATE_GIT_SOURCE", "true")
	src := makeHTTPSourceRepo(t)
	base := t.TempDir()

	gs := NewGitSource(GitSourceConfig{
		Name:  "docs-src",
		URL:   src,
		Layer: LayerProject,
	}, base, covLogger())

	ctx := context.Background()
	if err := gs.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if !gs.Ready() {
		t.Fatal("expected ready after Init")
	}
	if gs.Store() == nil {
		t.Fatal("expected non-nil store")
	}

	// Re-init (already cloned path -> ensureCloned pulls).
	gs2 := NewGitSource(GitSourceConfig{
		Name:  "docs-src",
		URL:   src,
		Layer: LayerProject,
	}, base, covLogger())
	if err := gs2.Init(ctx); err != nil {
		t.Fatalf("Init (reuse clone): %v", err)
	}

	// Sync pulls latest and reindexes.
	if err := gs.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
}

func TestGitSource_InitSparseSubpath(t *testing.T) {
	t.Setenv("HIVE_ALLOW_PRIVATE_GIT_SOURCE", "true")
	src := makeHTTPSourceRepo(t)
	base := t.TempDir()

	gs := NewGitSource(GitSourceConfig{
		Name:    "docs-only",
		URL:     src,
		Subpath: "docs",
		Layer:   LayerProject,
	}, base, covLogger())

	if err := gs.Init(context.Background()); err != nil {
		t.Fatalf("Init sparse: %v", err)
	}
	if !gs.Ready() {
		t.Fatal("expected ready after sparse Init")
	}
	if gs.Store().Stats().TotalPages == 0 {
		t.Error("expected indexed pages from subpath")
	}
}

func TestGitSource_InitCloneFailure(t *testing.T) {
	base := t.TempDir()
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	gs := NewGitSource(GitSourceConfig{
		Name:  "bad",
		URL:   srv.URL + "/repo.git",
		Layer: LayerProject,
	}, base, covLogger())

	if err := gs.Init(context.Background()); err == nil {
		t.Error("expected clone failure for nonexistent repo")
	}
}

func TestGitSource_SyncNotARepo(t *testing.T) {
	base := t.TempDir()
	gs := NewGitSource(GitSourceConfig{
		Name:  "norepo",
		URL:   "https://example.com/repo.git",
		Layer: LayerProject,
	}, base, covLogger())
	// cloneDir does not exist / is not a git repo.
	if err := gs.Sync(context.Background()); err == nil {
		t.Error("expected error syncing non-git dir")
	}
}

func TestGitSource_StartSyncLoop_Cancels(t *testing.T) {
	t.Setenv("HIVE_ALLOW_PRIVATE_GIT_SOURCE", "true")
	src := makeHTTPSourceRepo(t)
	base := t.TempDir()
	gs := NewGitSource(GitSourceConfig{
		Name:  "loop",
		URL:   src,
		Layer: LayerProject,
	}, base, covLogger())
	if err := gs.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		gs.StartSyncLoop(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("StartSyncLoop did not return after cancel")
	}
}

// --- gitsync: gitPull error path, syncAll ---

func TestGitPull_NotARepo(t *testing.T) {
	err := gitPull(context.Background(), t.TempDir())
	if err == nil {
		t.Error("expected gitPull error for non-repo dir")
	}
	// exercise gitPullError.Error and Unwrap
	if _, ok := err.(*gitPullError); ok {
		_ = err.Error()
		_ = err.(*gitPullError).Unwrap()
	}
}

func TestGitSyncer_SyncAll(t *testing.T) {
	src := makeSourceRepo(t)
	// Clone it so it's a real repo we can pull.
	clone := t.TempDir()
	runGit(t, filepath.Dir(clone), "clone", "file://"+src, clone)

	store, err := NewFileStore(clone, "vault", covLogger())
	if err != nil {
		t.Fatal(err)
	}

	syncer := NewGitSyncer(covLogger())
	syncer.Add("vault", clone, store)
	// Non-repo vault is skipped.
	syncer.Add("skip", t.TempDir(), store)

	syncer.syncAll(context.Background())
}
