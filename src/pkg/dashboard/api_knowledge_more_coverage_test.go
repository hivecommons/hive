package dashboard

import (
	"net/http"
	"testing"
)

// covKServer uses covApiServer; knowledge handlers lazily create a file-based
// knowledge API via ensureKnowledge(), so the non-error branches run without an
// explicitly wired Knowledge dependency.
func covKServer(t *testing.T) *Server {
	t.Helper()
	return covApiServer(t)
}

func TestCovKM_Toggle(t *testing.T) {
	s := covKServer(t)
	if rec := doPutRaw(s, "/api/knowledge/enabled", "bad"); rec.Code != http.StatusBadRequest {
		t.Fatalf("toggle bad body: %d", rec.Code)
	}
	// Enable then disable → exercises both branches.
	if rec := doPut(s, "/api/knowledge/enabled", map[string]any{"enabled": true}); rec.Code != http.StatusOK {
		t.Fatalf("toggle enable: %d", rec.Code)
	}
	if rec := doPut(s, "/api/knowledge/enabled", map[string]any{"enabled": false}); rec.Code != http.StatusOK {
		t.Fatalf("toggle disable: %d", rec.Code)
	}
}

func TestCovKM_BeadSynthToggle(t *testing.T) {
	s := covKServer(t)
	if rec := doPutRaw(s, "/api/knowledge/bead-synthesizer/enabled", "bad"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bead toggle bad body: %d", rec.Code)
	}
	if rec := doPut(s, "/api/knowledge/bead-synthesizer/enabled", map[string]any{"enabled": true}); rec.Code != http.StatusOK {
		t.Fatalf("bead toggle enable: %d", rec.Code)
	}
	if rec := doGet(s, "/api/knowledge/bead-synthesizer"); rec.Code != http.StatusOK {
		t.Fatalf("bead status: %d", rec.Code)
	}
}

func TestCovKM_Promote(t *testing.T) {
	s := covKServer(t)
	if rec := doPostRaw(s, "/api/knowledge/promote", "bad"); rec.Code != http.StatusBadRequest {
		t.Fatalf("promote bad body: %d", rec.Code)
	}
	if rec := doPost(s, "/api/knowledge/promote", map[string]any{"slug": ""}); rec.Code != http.StatusBadRequest {
		t.Fatalf("promote missing fields: %d", rec.Code)
	}
	// All fields present but the fact doesn't exist → PromoteFact fails, VaultFact
	// lookup fails → 400.
	if rec := doPost(s, "/api/knowledge/promote", map[string]any{
		"slug": "nope", "from_layer": "project", "to_layer": "org",
	}); rec.Code != http.StatusBadRequest {
		t.Fatalf("promote nonexistent: %d", rec.Code)
	}
}

func TestCovKM_Import(t *testing.T) {
	s := covKServer(t)
	// Knowledge is nil (not lazily created here) → 503 covers the guard branch.
	if rec := doPostRaw(s, "/api/knowledge/import", "bad"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("import guard: %d", rec.Code)
	}
	// With knowledge wired, a bad body → 400.
	s2, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()
	if rec := doPostRaw(s2, "/api/knowledge/import", "bad"); rec.Code != http.StatusBadRequest {
		t.Fatalf("import bad body: %d", rec.Code)
	}
}

func TestCovKM_Create(t *testing.T) {
	s := covKServer(t)
	if rec := doPostRaw(s, "/api/knowledge/create", "bad"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("create guard: %d", rec.Code)
	}
	s2, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()
	if rec := doPostRaw(s2, "/api/knowledge/create", "bad"); rec.Code != http.StatusBadRequest {
		t.Fatalf("create bad body: %d", rec.Code)
	}
}

func TestCovKM_Graph(t *testing.T) {
	s := covKServer(t)
	// With lazily-created file knowledge, GraphStore may be nil → empty graph.
	if rec := doGet(s, "/api/knowledge/graph?root=x&depth=3"); rec.Code != http.StatusOK {
		t.Fatalf("graph: %d", rec.Code)
	}
	if rec := doGet(s, "/api/knowledge/graph?depth=notanumber"); rec.Code != http.StatusOK {
		t.Fatalf("graph bad depth: %d", rec.Code)
	}
}

func TestCovKM_Layer(t *testing.T) {
	s := covKServer(t)
	// Ordinary layer.
	if rec := doGet(s, "/api/knowledge/project"); rec.Code != http.StatusOK {
		t.Fatalf("layer: %d", rec.Code)
	}
	// git_source: prefix branch (unknown source → empty facts).
	if rec := doGet(s, "/api/knowledge/git_source:unknown"); rec.Code != http.StatusOK {
		t.Fatalf("git_source layer: %d", rec.Code)
	}
}

func TestCovKM_List(t *testing.T) {
	s := covKServer(t)
	if rec := doGet(s, "/api/knowledge?type=gotcha"); rec.Code != http.StatusOK {
		t.Fatalf("list: %d", rec.Code)
	}
}

func TestCovKM_Export(t *testing.T) {
	s := covKServer(t)
	if rec := doGet(s, "/api/knowledge/export"); rec.Code != http.StatusOK {
		t.Fatalf("export: %d", rec.Code)
	}
}

func TestCovKM_Stats(t *testing.T) {
	s := covKServer(t)
	if rec := doGet(s, "/api/knowledge/stats"); rec.Code != http.StatusOK {
		t.Fatalf("stats: %d", rec.Code)
	}
}

func TestCovKM_SearchByTag(t *testing.T) {
	s := covKServer(t)
	if rec := doGet(s, "/api/knowledge/search?tag=urgent&limit=3"); rec.Code != http.StatusOK {
		t.Fatalf("search by tag: %d", rec.Code)
	}
}

// ---- Documents ----

func TestCovKM_DocumentsList(t *testing.T) {
	s := covKServer(t)
	if rec := doGet(s, "/api/knowledge/documents"); rec.Code != http.StatusOK {
		t.Fatalf("docs list: %d", rec.Code)
	}
}

func TestCovKM_DocumentsImport(t *testing.T) {
	s := covKServer(t)
	if rec := doPostRaw(s, "/api/knowledge/documents", "bad"); rec.Code != http.StatusBadRequest {
		t.Fatalf("docs import bad body: %d", rec.Code)
	}
	if rec := doPost(s, "/api/knowledge/documents", map[string]any{}); rec.Code != http.StatusBadRequest {
		t.Fatalf("docs import missing name: %d", rec.Code)
	}
	if rec := doPost(s, "/api/knowledge/documents", map[string]any{"name": "doc1"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("docs import missing source: %d", rec.Code)
	}
}

func TestCovKM_DocumentGetDelete(t *testing.T) {
	s := covKServer(t)
	// Unknown slug → 404 for get/delete/reimport.
	if rec := doGet(s, "/api/knowledge/documents/ghostslug"); rec.Code != http.StatusNotFound {
		t.Fatalf("doc get: %d", rec.Code)
	}
	if rec := doDelete(s, "/api/knowledge/documents/ghostslug", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("doc delete: %d", rec.Code)
	}
}

func TestCovKM_Context7Search(t *testing.T) {
	s := covKServer(t)
	// Missing q → 400.
	if rec := doGet(s, "/api/knowledge/context7/search"); rec.Code != http.StatusBadRequest {
		t.Fatalf("context7 missing q: %d", rec.Code)
	}
}
