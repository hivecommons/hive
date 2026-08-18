package commands

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestKnowledgeHealthAndStats cover the health/stats generated subcommands.
func TestKnowledgeHealthAndStats(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
	}{
		{name: "health", path: "/api/knowledge/health"},
		{name: "stats", path: "/api/knowledge/stats"},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != tc.path {
				t.Fatalf("%s: path = %q", tc.name, r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{}`)
		}))
		if _, _, err := execute(t, server, "", "knowledge", tc.name); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		server.Close()
	}
}

// TestKnowledgeListWithLayerAndType covers knowledgeListCommand's layer +
// type filter branches.
func TestKnowledgeListWithLayerAndType(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/knowledge/project" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if r.URL.Query().Get("type") != "regression" {
			t.Fatalf("query = %v", r.URL.Query())
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `[]`)
	}))
	defer server.Close()

	if _, _, err := execute(t, server, "", "knowledge", "list", "--layer", "project", "--type", "regression"); err != nil {
		t.Fatal(err)
	}
}

// TestKnowledgeListWithoutLayer covers knowledgeListCommand's no-layer
// (all-layers) path.
func TestKnowledgeListWithoutLayer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/knowledge" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `[]`)
	}))
	defer server.Close()

	if _, _, err := execute(t, server, "", "knowledge", "list"); err != nil {
		t.Fatal(err)
	}
}

// TestKnowledgeSearchRequiresQueryOrTag covers the "requires a query or
// --tag" validation branch.
func TestKnowledgeSearchRequiresQueryOrTag(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	_, _, err := execute(t, server, "", "knowledge", "search")
	if err == nil || ExitCode(err) != ExitUsage {
		t.Fatalf("err = %v", err)
	}
}

// TestKnowledgeSearchRejectsNegativeLimit covers the --limit validation
// branch.
func TestKnowledgeSearchRejectsNegativeLimit(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	_, _, err := execute(t, server, "", "knowledge", "search", "--tag", "ux", "--limit", "-1")
	if err == nil || ExitCode(err) != ExitUsage {
		t.Fatalf("err = %v", err)
	}
}

// TestKnowledgeSearchByTagOnly covers the branch where only --tag is set
// (no positional query arg), so the "q" query param is omitted.
func TestKnowledgeSearchByTagOnly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") != "" || r.URL.Query().Get("tag") != "ux" {
			t.Fatalf("query = %v", r.URL.Query())
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"results":[]}`)
	}))
	defer server.Close()

	if _, _, err := execute(t, server, "", "knowledge", "search", "--tag", "ux"); err != nil {
		t.Fatal(err)
	}
}

// TestKnowledgeGetSuccess covers knowledgeGetCommand.
func TestKnowledgeGetSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/knowledge/project/create-project" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"slug":"create-project"}`)
	}))
	defer server.Close()

	if _, _, err := execute(t, server, "", "knowledge", "get", "project", "create-project"); err != nil {
		t.Fatal(err)
	}
}

// TestKnowledgeCreateSuccess covers knowledgeWriteCommand's "create" branch
// (operation != "update").
func TestKnowledgeCreateSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/knowledge/create" || r.Method != http.MethodPost {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"status":"created"}`)
	}))
	defer server.Close()

	if _, _, err := execute(t, server, `{"slug":"x"}`, "knowledge", "create", "--stdin"); err != nil {
		t.Fatal(err)
	}
}

// TestKnowledgeUpdateSuccess covers knowledgeWriteCommand's "update" branch.
func TestKnowledgeUpdateSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/knowledge/project/create-project" || r.Method != http.MethodPut {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"status":"updated"}`)
	}))
	defer server.Close()

	if _, _, err := execute(t, server, `{"slug":"x"}`, "knowledge", "update", "project", "create-project", "--stdin"); err != nil {
		t.Fatal(err)
	}
}

// TestKnowledgeDeleteSuccess covers knowledgeDeleteCommand's success path.
func TestKnowledgeDeleteSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/knowledge/project/create-project" || r.Method != http.MethodDelete {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"status":"deleted"}`)
	}))
	defer server.Close()

	if _, _, err := execute(t, server, "", "knowledge", "delete", "project", "create-project", "--yes"); err != nil {
		t.Fatal(err)
	}
}

// TestKnowledgeImportSuccess covers knowledgeImportCommand.
func TestKnowledgeImportSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/knowledge/import" || r.Method != http.MethodPost {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["format"] != "markdown" || body["layer"] != "project" || !strings.Contains(body["content"].(string), "# Doc") {
			t.Fatalf("body = %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"status":"imported"}`)
	}))
	defer server.Close()

	if _, _, err := execute(t, server, "# Doc\n", "knowledge", "import", "--stdin"); err != nil {
		t.Fatal(err)
	}
}

// TestKnowledgeExportTableOutputWritesRaw covers knowledgeExportCommand's
// table-output branch, writing the raw bytes verbatim.
func TestKnowledgeExportTableOutputWritesRaw(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "# Exported Knowledge\n")
	}))
	defer server.Close()

	stdout, _, err := execute(t, server, "", "knowledge", "export")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "# Exported Knowledge") {
		t.Fatalf("stdout = %q", stdout)
	}
}

// TestKnowledgeExportJSONOutputPrintsString covers knowledgeExportCommand's
// non-table branch, where the exported text is JSON-encoded as a string.
func TestKnowledgeExportJSONOutputPrintsString(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "# Exported Knowledge\n")
	}))
	defer server.Close()

	stdout, _, err := execute(t, server, "", "--output", "json", "knowledge", "export")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "Exported Knowledge") {
		t.Fatalf("stdout = %q", stdout)
	}
}

// TestKnowledgeGraphSuccess covers knowledgeGraphCommand with --root set.
func TestKnowledgeGraphSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/knowledge/graph" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if r.URL.Query().Get("root") != "create-project" || r.URL.Query().Get("depth") != "3" {
			t.Fatalf("query = %v", r.URL.Query())
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"nodes":[]}`)
	}))
	defer server.Close()

	if _, _, err := execute(t, server, "", "knowledge", "graph", "--root", "create-project", "--depth", "3"); err != nil {
		t.Fatal(err)
	}
}

// TestKnowledgeGraphRejectsNonPositiveDepth covers the --depth validation
// branch.
func TestKnowledgeGraphRejectsNonPositiveDepth(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	_, _, err := execute(t, server, "", "knowledge", "graph", "--depth", "0")
	if err == nil || ExitCode(err) != ExitUsage {
		t.Fatalf("err = %v", err)
	}
}

// TestKnowledgeGraphWithoutRoot covers the branch where --root is empty.
func TestKnowledgeGraphWithoutRoot(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("root") != "" {
			t.Fatalf("query = %v", r.URL.Query())
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"nodes":[]}`)
	}))
	defer server.Close()

	if _, _, err := execute(t, server, "", "knowledge", "graph"); err != nil {
		t.Fatal(err)
	}
}
