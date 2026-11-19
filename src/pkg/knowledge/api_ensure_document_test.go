package knowledge

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestEnsureDocumentReimportsAfterLostDiskState pins #18058: a configured
// document whose on-disk metadata is gone after an upgrade must be imported
// again at boot rather than silently dropped, while one that was reloaded
// from disk must not be imported a second time.
func TestEnsureDocumentReimportsAfterLostDiskState(t *testing.T) {
	base := withKnowledgeBaseDir(t)
	source := filepath.Join(base, "uploads", "guide.txt")
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("Guide\n\nAgents must cite the tech docs guide."), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := DocSourceConfig{Name: "Tech Docs Guide", FilePath: source, Layer: LayerProject}

	first := NewKnowledgeAPI(nil, KnowledgeConfig{Enabled: true}, importDocTestLogger())
	meta, imported, err := first.EnsureDocument(context.Background(), cfg)
	if err != nil || !imported {
		t.Fatalf("first EnsureDocument = %+v, imported=%v, err=%v; want a fresh import", meta, imported, err)
	}
	slug := DocumentSlug(cfg.Name)
	if meta.Slug != slug {
		t.Fatalf("slug = %q, want %q", meta.Slug, slug)
	}

	// Restart with the disk state intact: the reloaded document is kept as-is.
	restarted := NewKnowledgeAPI(nil, KnowledgeConfig{Enabled: true}, importDocTestLogger())
	got, imported, err := restarted.EnsureDocument(context.Background(), cfg)
	if err != nil {
		t.Fatalf("EnsureDocument after restart: %v", err)
	}
	if imported {
		t.Fatal("EnsureDocument re-imported a document that was reloaded from disk")
	}
	if got.Slug != slug || got.FactCount != meta.FactCount {
		t.Fatalf("EnsureDocument after restart = %+v, want reloaded %+v", got, meta)
	}

	// Upgrade lost the document's on-disk record: the configured source must
	// come back instead of vanishing from the knowledge sources list.
	if err := os.RemoveAll(filepath.Join(base, docStorageDir)); err != nil {
		t.Fatal(err)
	}
	upgraded := NewKnowledgeAPI(nil, KnowledgeConfig{Enabled: true}, importDocTestLogger())
	if docs := upgraded.ListDocuments(); len(docs) != 0 {
		t.Fatalf("precondition: expected no documents reloaded, got %+v", docs)
	}
	got, imported, err = upgraded.EnsureDocument(context.Background(), cfg)
	if err != nil || !imported {
		t.Fatalf("EnsureDocument after lost disk state = %+v, imported=%v, err=%v; want a re-import", got, imported, err)
	}
	if docs := upgraded.ListDocuments(); len(docs) != 1 || docs[0].Slug != slug {
		t.Fatalf("ListDocuments after re-import = %+v, want %q", docs, slug)
	}
}
