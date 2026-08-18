package knowledge

import (
	"context"
	"path/filepath"
	"testing"
)

// TestObsidianSync_FileFallback drives the no-HTTP-client branch of ObsidianSync,
// which routes through obsidianSyncToFile. The underlying write targets the
// hardcoded localKnowledgeDir and is swallowed on failure, so the call still
// succeeds and returns a "created" action.
func TestObsidianSync_FileFallback(t *testing.T) {
	api := &KnowledgeAPI{logger: covLogger()}
	res, err := api.ObsidianSync(context.Background(), ObsidianSyncRequest{
		Filename: "my-note.md",
		Content:  "Some note body.",
		Frontmatter: map[string]interface{}{
			"title":      "My Note",
			"type":       "gotcha",
			"tags":       []interface{}{"go", "testing"},
			"confidence": 0.8,
		},
	})
	if err != nil {
		t.Fatalf("ObsidianSync: %v", err)
	}
	if res.Slug != "my-note" {
		t.Errorf("slug = %q", res.Slug)
	}
	if res.Fact.Title != "My Note" {
		t.Errorf("title = %q", res.Fact.Title)
	}
	if res.Action == "" {
		t.Error("expected a non-empty action")
	}
}

// TestObsidianSync_DefaultLayerFromDots checks the layer-sanitization branch
// where a traversal-y layer collapses to the default "project".
func TestObsidianSync_LayerSanitized(t *testing.T) {
	api := &KnowledgeAPI{logger: covLogger()}
	res, err := api.ObsidianSync(context.Background(), ObsidianSyncRequest{
		Filename: "note2.md",
		Content:  "body",
		Frontmatter: map[string]interface{}{
			"layer": "..",
		},
	})
	if err != nil {
		t.Fatalf("ObsidianSync: %v", err)
	}
	if res.Fact.Layer != LayerType("project") {
		t.Errorf("expected sanitized layer 'project', got %q", res.Fact.Layer)
	}
}

// TestDocumentSource_ImportContext7Error covers the Context7 branch of
// DocumentSource.Import: with a bogus key and no network the fetch fails.
func TestDocumentSource_ImportContext7Error(t *testing.T) {
	tmpDir := t.TempDir()
	ds := NewDocumentSource(DocSourceConfig{
		Name:       "ctx7-fail",
		Context7ID: "/some/lib",
		Layer:      LayerProject,
	}, tmpDir, filepath.Join(tmpDir, "vault"), nil, covLogger(), "")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ds.Import(ctx); err == nil {
		t.Error("expected error importing Context7 doc with cancelled context")
	}
}
