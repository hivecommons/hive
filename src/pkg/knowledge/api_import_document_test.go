package knowledge

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func withKnowledgeBaseDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	oldBase := knowledgeBaseDir
	oldPrefixes := allowedFilePrefixes
	knowledgeBaseDir = dir
	allowedFilePrefixes = []string{dir + string(filepath.Separator)}
	t.Cleanup(func() {
		knowledgeBaseDir = oldBase
		allowedFilePrefixes = oldPrefixes
	})
	return dir
}

func importDocTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestKnowledgeAPIImportDocumentFileIngestion(t *testing.T) {
	base := withKnowledgeBaseDir(t)
	source := filepath.Join(base, "uploads", "runbook.txt")
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "Runbook Title\n\nThe system must retry failed jobs.\n\nOperators need clear rollback steps."
	if err := os.WriteFile(source, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	api := NewKnowledgeAPI(nil, KnowledgeConfig{Enabled: true}, importDocTestLogger())
	meta, err := api.ImportDocument(context.Background(), DocSourceConfig{
		Name:     "Runbook Import",
		FilePath: source,
		Layer:    LayerProject,
	})
	if err != nil {
		t.Fatalf("ImportDocument: %v", err)
	}
	if meta.Slug != "runbook-import" {
		t.Fatalf("slug = %q, want runbook-import", meta.Slug)
	}
	if meta.FactCount == 0 || len(meta.FactSlugs) != meta.FactCount {
		t.Fatalf("expected imported facts, got meta %+v", meta)
	}
	if got := api.ListDocuments(); len(got) != 1 || got[0].Slug != meta.Slug {
		t.Fatalf("ListDocuments = %+v, want imported metadata", got)
	}
	if got, err := api.GetDocument(meta.Slug); err != nil || got.FactCount != meta.FactCount {
		t.Fatalf("GetDocument = %+v, %v; want fact count %d", got, err, meta.FactCount)
	}

	for _, slug := range meta.FactSlugs {
		path := filepath.Join(base, "documents-vault", slug+".md")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("expected fact %s written to vault: %v", slug, err)
		}
		if len(data) == 0 {
			t.Fatalf("fact %s is empty", slug)
		}
	}

	if _, err := api.ImportDocument(context.Background(), DocSourceConfig{Name: "Runbook Import", FilePath: source}); err == nil {
		t.Fatal("duplicate import should be rejected")
	}
}

func TestKnowledgeAPIReloadDocumentsSkipsCorruptMetadata(t *testing.T) {
	base := withKnowledgeBaseDir(t)
	source := filepath.Join(base, "uploads", "guide.txt")
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("Guide Title\n\nA persistent fact from disk."), 0o644); err != nil {
		t.Fatal(err)
	}

	api := NewKnowledgeAPI(nil, KnowledgeConfig{Enabled: true}, importDocTestLogger())
	if _, err := api.ImportDocument(context.Background(), DocSourceConfig{
		Name:     "Reloaded Guide",
		FilePath: source,
	}); err != nil {
		t.Fatalf("ImportDocument: %v", err)
	}

	badDir := filepath.Join(base, docStorageDir, "corrupt")
	if err := os.MkdirAll(badDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badDir, docMetadataFile), []byte("{not-json"), 0o644); err != nil {
		t.Fatal(err)
	}

	reloaded := NewKnowledgeAPI(nil, KnowledgeConfig{Enabled: true}, importDocTestLogger())
	docs := reloaded.ListDocuments()
	if len(docs) != 1 {
		t.Fatalf("reloaded docs = %+v, want exactly the valid document", docs)
	}
	if docs[0].Slug != "reloaded-guide" {
		t.Fatalf("reloaded slug = %q, want reloaded-guide", docs[0].Slug)
	}
}

func TestKnowledgeAPIImportDocumentRejectsInvalidSources(t *testing.T) {
	base := withKnowledgeBaseDir(t)
	api := NewKnowledgeAPI(nil, KnowledgeConfig{Enabled: true}, importDocTestLogger())

	outside := filepath.Join(filepath.Dir(base), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := api.ImportDocument(context.Background(), DocSourceConfig{Name: "Outside", FilePath: outside}); err == nil {
		t.Fatal("outside file should be rejected")
	}

	if _, err := api.ImportDocument(context.Background(), DocSourceConfig{Name: "No Source"}); err == nil {
		t.Fatal("missing url/file/context7 should be rejected")
	}
}
