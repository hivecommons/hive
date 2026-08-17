package knowledge

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDocumentSource_ImportFile(t *testing.T) {
	tmpDir := t.TempDir()
	vaultDir := filepath.Join(tmpDir, "vault")
	baseDir := filepath.Join(tmpDir, "knowledge")

	srcFile := filepath.Join(baseDir, "test.txt")
	content := "First paragraph about transformers.\n\nSecond paragraph about attention mechanisms.\n\nThird paragraph about KV cache management."
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(srcFile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	ds := NewDocumentSource(DocSourceConfig{
		Name:     "test-doc",
		FilePath: srcFile,
		Layer:    LayerProject,
	}, baseDir, vaultDir, nil, logger, "")

	meta, err := ds.Import(context.Background())
	if err != nil {
		t.Fatalf("Import failed: %v", err)
	}

	if meta.Slug != "test-doc" {
		t.Errorf("slug = %q, want test-doc", meta.Slug)
	}
	if meta.ContentType != "text/plain" {
		t.Errorf("content_type = %q, want text/plain", meta.ContentType)
	}
	if meta.FactCount < 1 {
		t.Errorf("fact_count = %d, want >= 1", meta.FactCount)
	}

	for _, slug := range meta.FactSlugs {
		path := filepath.Join(vaultDir, slug+".md")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("fact file %s not found: %v", slug, err)
			continue
		}
		if !strings.Contains(string(data), "type: reference") {
			t.Errorf("fact %s missing type: reference", slug)
		}
		if !strings.Contains(string(data), "layer: project") {
			t.Errorf("fact %s missing layer: project", slug)
		}
	}

	mdPath := filepath.Join(baseDir, docStorageDir, "test-doc", docMetadataFile)
	if _, err := os.Stat(mdPath); os.IsNotExist(err) {
		t.Error("metadata.json not created")
	}
}

// TestDocumentSource_ImportFile_OutsideKnowledgeDirRejected is the positive
// control for the file_path confinement guard: it points a doc source at a
// real, readable file OUTSIDE the knowledge directory (simulating an
// authenticated caller using file_path to exfiltrate e.g. /data/hive.yaml or
// /secrets/*). If the validateFilePath check at the read site is removed,
// Import succeeds reading the file and this test FAILS.
func TestDocumentSource_ImportFile_OutsideKnowledgeDirRejected(t *testing.T) {
	tmpDir := t.TempDir()
	vaultDir := filepath.Join(tmpDir, "vault")
	baseDir := filepath.Join(tmpDir, "knowledge")
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Readable file outside the knowledge dir (sibling of baseDir).
	secretFile := filepath.Join(tmpDir, "secret.txt")
	if err := os.WriteFile(secretFile, []byte("super secret credentials"), 0o644); err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// Absolute path outside the knowledge dir must be rejected.
	ds := NewDocumentSource(DocSourceConfig{
		Name:     "evil-abs",
		FilePath: secretFile,
		Layer:    LayerProject,
	}, baseDir, vaultDir, nil, logger, "")
	if _, err := ds.Import(context.Background()); err == nil {
		t.Fatal("expected Import to reject absolute file_path outside knowledge dir")
	} else if !strings.Contains(err.Error(), "outside the allowed knowledge directory") {
		t.Fatalf("expected confinement error, got: %v", err)
	}

	// ../ traversal escaping the knowledge dir must be rejected.
	ds = NewDocumentSource(DocSourceConfig{
		Name:     "evil-traversal",
		FilePath: filepath.Join(baseDir, "..", "secret.txt"),
		Layer:    LayerProject,
	}, baseDir, vaultDir, nil, logger, "")
	if _, err := ds.Import(context.Background()); err == nil {
		t.Fatal("expected Import to reject ../ traversal escaping knowledge dir")
	} else if !strings.Contains(err.Error(), "outside the allowed knowledge directory") {
		t.Fatalf("expected confinement error, got: %v", err)
	}

	// No artifact may have been written for the rejected sources.
	for _, slug := range []string{"evil-abs", "evil-traversal"} {
		if _, err := os.Stat(filepath.Join(baseDir, docStorageDir, slug, docSourceFile+".txt")); !os.IsNotExist(err) {
			t.Errorf("artifact for %s should not exist after rejected import", slug)
		}
	}

	// A file inside the knowledge dir imports fine (guard is not over-broad).
	okFile := filepath.Join(baseDir, "doc.txt")
	if err := os.WriteFile(okFile, []byte("Legitimate knowledge content."), 0o644); err != nil {
		t.Fatal(err)
	}
	ds = NewDocumentSource(DocSourceConfig{
		Name:     "ok-doc",
		FilePath: okFile,
		Layer:    LayerProject,
	}, baseDir, vaultDir, nil, logger, "")
	if _, err := ds.Import(context.Background()); err != nil {
		t.Fatalf("expected in-dir import to succeed: %v", err)
	}
}

func TestDocumentSource_ImportURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><head><title>Test Paper</title></head>
<body><nav>Menu</nav><article>
<h2>Introduction</h2><p>This is the introduction to the paper.</p>
<h2>Methods</h2><p>We used several methods.</p>
</article><footer>Copyright</footer></body></html>`)
	}))
	defer server.Close()

	tmpDir := t.TempDir()
	vaultDir := filepath.Join(tmpDir, "vault")
	baseDir := filepath.Join(tmpDir, "knowledge")

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	ds := NewDocumentSource(DocSourceConfig{
		Name:  "test-paper",
		URL:   server.URL,
		Layer: LayerCommunity,
	}, baseDir, vaultDir, nil, logger, "")

	meta, err := ds.Import(context.Background())
	if err != nil {
		t.Fatalf("Import failed: %v", err)
	}

	// The configured Name wins over the HTML-extracted title; extraction is
	// only the fallback when Name is empty.
	if meta.Title != "test-paper" {
		t.Errorf("title = %q, want test-paper (config name)", meta.Title)
	}
	if meta.ContentType != "text/html" {
		t.Errorf("content_type = %q, want text/html", meta.ContentType)
	}
	if meta.FactCount < 1 {
		t.Errorf("fact_count = %d, want >= 1", meta.FactCount)
	}

	artifactPath := filepath.Join(baseDir, docStorageDir, "test-paper", docSourceFile+".html")
	if _, err := os.Stat(artifactPath); os.IsNotExist(err) {
		t.Error("source artifact not saved")
	}
}

func TestDocumentSource_Delete(t *testing.T) {
	tmpDir := t.TempDir()
	vaultDir := filepath.Join(tmpDir, "vault")
	baseDir := filepath.Join(tmpDir, "knowledge")

	srcFile := filepath.Join(baseDir, "test.txt")
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(srcFile, []byte("Some content to import."), 0o644); err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	ds := NewDocumentSource(DocSourceConfig{
		Name:     "deleteme",
		FilePath: srcFile,
		Layer:    LayerProject,
	}, baseDir, vaultDir, nil, logger, "")

	meta, err := ds.Import(context.Background())
	if err != nil {
		t.Fatalf("Import failed: %v", err)
	}

	if len(meta.FactSlugs) == 0 {
		t.Fatal("no facts created")
	}

	if err := ds.Delete(); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	for _, slug := range meta.FactSlugs {
		path := filepath.Join(vaultDir, slug+".md")
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("fact file %s still exists after delete", slug)
		}
	}

	storageDir := filepath.Join(baseDir, docStorageDir, "deleteme")
	if _, err := os.Stat(storageDir); !os.IsNotExist(err) {
		t.Error("storage dir still exists after delete")
	}
}

func TestDocumentSource_Reimport(t *testing.T) {
	tmpDir := t.TempDir()
	vaultDir := filepath.Join(tmpDir, "vault")
	baseDir := filepath.Join(tmpDir, "knowledge")

	srcFile := filepath.Join(baseDir, "test.txt")
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(srcFile, []byte("Version 1 content."), 0o644); err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	ds := NewDocumentSource(DocSourceConfig{
		Name:     "reimportme",
		FilePath: srcFile,
		Layer:    LayerProject,
	}, baseDir, vaultDir, nil, logger, "")

	meta1, err := ds.Import(context.Background())
	if err != nil {
		t.Fatalf("Import failed: %v", err)
	}

	if err := os.WriteFile(srcFile, []byte("Version 2 content with more text.\n\nAnd a second paragraph."), 0o644); err != nil {
		t.Fatal(err)
	}

	meta2, err := ds.Reimport(context.Background())
	if err != nil {
		t.Fatalf("Reimport failed: %v", err)
	}

	for _, slug := range meta1.FactSlugs {
		path := filepath.Join(vaultDir, slug+".md")
		if _, err := os.Stat(path); os.IsNotExist(err) {
			continue
		}
	}

	if meta2.FactCount < 1 {
		t.Error("reimport produced no facts")
	}
}

func TestDocumentSource_NoSourceError(t *testing.T) {
	tmpDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	ds := NewDocumentSource(DocSourceConfig{
		Name:  "no-source",
		Layer: LayerProject,
	}, tmpDir, filepath.Join(tmpDir, "vault"), nil, logger, "")

	_, err := ds.Import(context.Background())
	if err == nil {
		t.Fatal("expected error for document with no URL or file path")
	}
	if !strings.Contains(err.Error(), "has no url, file_path, or context7_id") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestDetectContentType(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"paper.pdf", "application/pdf"},
		{"page.html", "text/html"},
		{"page.htm", "text/html"},
		{"notes.md", "text/plain"},
		{"notes.txt", "text/plain"},
		{"unknown", "text/plain"},
		{"PAPER.PDF", "application/pdf"},
	}
	for _, tt := range tests {
		got := detectContentType(tt.input)
		if got != tt.want {
			t.Errorf("detectContentType(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestNormalizeContentType(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"application/pdf", "application/pdf"},
		{"application/pdf; charset=utf-8", "application/pdf"},
		{"text/html; charset=utf-8", "text/html"},
		{"text/plain", "text/plain"},
		{"application/octet-stream", "text/plain"},
	}
	for _, tt := range tests {
		got := normalizeContentType(tt.input)
		if got != tt.want {
			t.Errorf("normalizeContentType(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestDocNoRedirectToPrivate_BlocksInternalRedirects(t *testing.T) {
// Serve a redirect to a private address from a "public" host.
privateTarget := "http://127.0.0.1:1/secret"
redirectSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
http.Redirect(w, r, privateTarget, http.StatusFound)
}))
defer redirectSrv.Close()

ds := &DocumentSource{
config: DocSourceConfig{URL: redirectSrv.URL},
logger: slog.Default(),
}
_, _, err := ds.fetchURL(context.Background(), redirectSrv.URL)
if err == nil {
t.Fatal("expected error when redirect leads to private host, got nil")
}
if !strings.Contains(err.Error(), "private/internal host blocked") &&
!strings.Contains(err.Error(), "connection refused") {
// "connection refused" is also acceptable — the redirect was followed but
// nothing listens on 127.0.0.1:1, so the HTTP client fails at connection.
// The important thing is that the fetch did not succeed.
t.Errorf("unexpected error: %v", err)
}
}

func TestDocNoRedirectToPrivate_AllowsPublicHosts(t *testing.T) {
// Simulate a redirect to a routable public host by calling docNoRedirectToPrivate
// directly — httptest servers use 127.0.0.1 so we cannot use them here.
publicURL := "https://example.com/page"
req, _ := http.NewRequest(http.MethodGet, publicURL, nil)
via := []*http.Request{{}}
if err := docNoRedirectToPrivate(req, via); err != nil {
t.Errorf("expected nil for public host redirect, got: %v", err)
}
}

func TestDocNoRedirectToPrivate_BlocksAfterMaxRedirects(t *testing.T) {
req, _ := http.NewRequest(http.MethodGet, "https://example.com/", nil)
via := make([]*http.Request, 3) // exactly at limit
if err := docNoRedirectToPrivate(req, via); err == nil {
t.Error("expected error when redirect chain exceeds limit, got nil")
}
}
