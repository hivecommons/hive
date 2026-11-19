package dashboard

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/knowledge"
)

// TestDocumentsImportPersistsAndDeleteUnpersists pins #18058: a document
// imported from the dashboard is recorded in knowledge.documents (as git
// sources are in knowledge.git_sources) so boot can restore it after an
// upgrade, a repeat import does not duplicate the entry, and deleting the
// document drops it from config so boot does not resurrect it.
func TestDocumentsImportPersistsAndDeleteUnpersists(t *testing.T) {
	base := t.TempDir()
	knowledge.SetBaseDirForTest(t, base)
	source := filepath.Join(base, "uploads", "guide.txt")
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("Guide\n\nAgents must cite the tech docs guide."), 0o644); err != nil {
		t.Fatal(err)
	}

	s, deps := apiServer(t)
	deps.Config.SourcePath = filepath.Join(t.TempDir(), "hive.yaml")
	deps.Knowledge = knowledge.NewKnowledgeAPI(nil, knowledge.KnowledgeConfig{Enabled: true, Engine: "file"}, deps.Logger)
	deps.Config.Knowledge.Documents = []config.DocSourceConfigYAML{{Name: "other-doc", URL: "https://example.com/other.pdf", Layer: "project"}}

	rec := doPost(s, "/api/knowledge/documents", map[string]any{"name": "Tech Docs Guide", "file_path": source})
	if rec.Code != http.StatusOK {
		t.Fatalf("import: expected 200, got %d (body %q)", rec.Code, rec.Body.String())
	}
	want := []config.DocSourceConfigYAML{
		{Name: "other-doc", URL: "https://example.com/other.pdf", Layer: "project"},
		{Name: "Tech Docs Guide", FilePath: source, Layer: "project"},
	}
	assertDocs := func(stage string, want []config.DocSourceConfigYAML) {
		t.Helper()
		got := deps.Config.Knowledge.Documents
		if len(got) != len(want) {
			t.Fatalf("%s: knowledge.documents = %+v, want %+v", stage, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s: knowledge.documents = %+v, want %+v", stage, got, want)
			}
		}
	}
	assertDocs("after import", want)
	persisted, err := os.ReadFile(deps.Config.SourcePath)
	if err != nil {
		t.Fatalf("read persisted config: %v", err)
	}
	if !strings.Contains(string(persisted), "Tech Docs Guide") {
		t.Fatalf("persisted config is missing the imported document:\n%s", persisted)
	}

	// A config entry for the same slug (e.g. a re-import after delete-from-disk)
	// must not be duplicated; the knowledge API rejects the second import, so
	// exercise the dedupe directly.
	s.persistDocumentSource(knowledge.DocSourceConfig{Name: "tech docs guide", FilePath: source, Layer: knowledge.LayerProject})
	assertDocs("after duplicate persist", want)

	rec = doDelete(s, "/api/knowledge/documents/"+knowledge.DocumentSlug("Tech Docs Guide"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: expected 200, got %d (body %q)", rec.Code, rec.Body.String())
	}
	assertDocs("after delete", want[:1])
	persisted, err = os.ReadFile(deps.Config.SourcePath)
	if err != nil {
		t.Fatalf("read persisted config: %v", err)
	}
	if text := string(persisted); strings.Contains(text, "Tech Docs Guide") || !strings.Contains(text, "other-doc") {
		t.Fatalf("persisted config did not drop only the deleted document:\n%s", text)
	}
}
