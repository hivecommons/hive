package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// kbTestServer starts an httptest server and points BD_DASHBOARD_URL at it for
// the duration of the test. Every cmdKB* function resolves the dashboard
// through dashboardURL(), so this is the same seam the real CLI uses.
func kbTestServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	t.Setenv("BD_DASHBOARD_URL", srv.URL)
	return srv
}

func jsonResponse(t *testing.T, w http.ResponseWriter, v interface{}) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("encoding test response: %v", err)
	}
}

// --- kbGet / kbPost ---

func TestKBGetSuccess(t *testing.T) {
	srv := kbTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s; want GET", r.Method)
		}
		io.WriteString(w, `{"ok":true}`)
	})
	body, err := kbGet(srv.URL + "/anything")
	if err != nil {
		t.Fatalf("kbGet: %v", err)
	}
	if string(body) != `{"ok":true}` {
		t.Errorf("body = %q; want {\"ok\":true}", body)
	}
}

func TestKBGetNon200ReturnsErrorWithBody(t *testing.T) {
	srv := kbTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no such fact", http.StatusNotFound)
	})
	_, err := kbGet(srv.URL + "/missing")
	if err == nil {
		t.Fatal("kbGet on 404 returned nil error")
	}
	if !strings.Contains(err.Error(), "HTTP 404") || !strings.Contains(err.Error(), "no such fact") {
		t.Errorf("error = %q; want it to name HTTP 404 and include the body", err)
	}
}

func TestKBGetConnectionError(t *testing.T) {
	// A server that is already closed refuses connections deterministically.
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	_, err := kbGet(srv.URL)
	if err == nil {
		t.Fatal("kbGet against closed server returned nil error")
	}
	if !strings.Contains(err.Error(), "HTTP GET") {
		t.Errorf("error = %q; want the HTTP GET wrap prefix", err)
	}
}

func TestKBPostSendsJSONAndReturnsBody(t *testing.T) {
	var gotContentType, gotBody string
	srv := kbTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s; want POST", r.Method)
		}
		gotContentType = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		io.WriteString(w, `{"done":1}`)
	})
	body, err := kbPost(srv.URL+"/api", `{"a":1}`)
	if err != nil {
		t.Fatalf("kbPost: %v", err)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q; want application/json", gotContentType)
	}
	if gotBody != `{"a":1}` {
		t.Errorf("request body = %q; want {\"a\":1}", gotBody)
	}
	if string(body) != `{"done":1}` {
		t.Errorf("response body = %q; want {\"done\":1}", body)
	}
}

func TestKBPostNon200ReturnsError(t *testing.T) {
	srv := kbTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	_, err := kbPost(srv.URL, "{}")
	if err == nil {
		t.Fatal("kbPost on 500 returned nil error")
	}
	if !strings.Contains(err.Error(), "HTTP 500") || !strings.Contains(err.Error(), "boom") {
		t.Errorf("error = %q; want it to name HTTP 500 and include the body", err)
	}
}

// --- bd kb search ---

func TestCmdKBSearchPrintsResults(t *testing.T) {
	longBody := strings.Repeat("x", 250)
	kbTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/knowledge/search" {
			t.Errorf("path = %s; want /api/knowledge/search", r.URL.Path)
		}
		if q := r.URL.Query().Get("q"); q != "auth flow" {
			t.Errorf("q = %q; want %q (multi-arg queries join with spaces)", q, "auth flow")
		}
		jsonResponse(t, w, []map[string]interface{}{
			{"slug": "auth-1", "title": "Auth flow", "confidence": 0.9, "type": "pattern", "body": longBody},
		})
	})

	out := captureStdout(t, func() { cmdKBSearch([]string{"auth", "flow"}) })

	if !strings.Contains(out, "## Auth flow") {
		t.Errorf("output missing title heading:\n%s", out)
	}
	if !strings.Contains(out, "slug: auth-1") || !strings.Contains(out, "confidence: 90%") {
		t.Errorf("output missing slug/confidence line:\n%s", out)
	}
	if !strings.Contains(out, "...") {
		t.Errorf("250-char body should be truncated with ellipsis:\n%s", out)
	}
	if strings.Contains(out, longBody) {
		t.Error("output contains the full untruncated body")
	}
}

func TestCmdKBSearchNoResults(t *testing.T) {
	kbTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(t, w, []map[string]interface{}{})
	})
	out := captureStdout(t, func() { cmdKBSearch([]string{"nothing"}) })
	if !strings.Contains(out, "No results found.") {
		t.Errorf("output = %q; want no-results message", out)
	}
}

// --- bd kb read ---

func TestCmdKBReadPrintsMatchingFact(t *testing.T) {
	kbTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(t, w, []map[string]interface{}{
			{"slug": "other", "title": "Other", "body": "not this one"},
			{"slug": "target", "title": "Target fact", "body": "full body text"},
		})
	})
	out := captureStdout(t, func() { cmdKBRead([]string{"target"}) })
	if !strings.Contains(out, "# Target fact") || !strings.Contains(out, "full body text") {
		t.Errorf("output = %q; want title heading and full body of the matching slug", out)
	}
	if strings.Contains(out, "not this one") {
		t.Error("output includes a non-matching fact's body")
	}
}

// --- bd kb list-docs ---

func TestCmdKBListDocsPrintsDocuments(t *testing.T) {
	kbTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/knowledge/documents" {
			t.Errorf("path = %s; want /api/knowledge/documents", r.URL.Path)
		}
		jsonResponse(t, w, []map[string]interface{}{
			{"slug": "vllm-docs", "content_type": "markdown", "fact_count": 12.0, "source_url": "https://example.com/doc"},
			{"slug": "local-notes", "content_type": "text", "fact_count": 3.0, "source_file": "/tmp/notes.md"},
		})
	})
	out := captureStdout(t, func() { cmdKBListDocs() })
	if !strings.Contains(out, "vllm-docs") || !strings.Contains(out, "(12 facts)") || !strings.Contains(out, "https://example.com/doc") {
		t.Errorf("output missing URL-sourced doc line:\n%s", out)
	}
	if !strings.Contains(out, "local-notes") || !strings.Contains(out, "/tmp/notes.md") {
		t.Errorf("output should fall back to source_file when source_url is empty:\n%s", out)
	}
}

func TestCmdKBListDocsEmpty(t *testing.T) {
	kbTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(t, w, []map[string]interface{}{})
	})
	out := captureStdout(t, func() { cmdKBListDocs() })
	if !strings.Contains(out, "No documents imported.") {
		t.Errorf("output = %q; want empty-state message", out)
	}
}

// --- bd kb ctx7-search ---

func TestCmdKBCtx7SearchPrintsLibraries(t *testing.T) {
	kbTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/knowledge/context7/search" {
			t.Errorf("path = %s; want /api/knowledge/context7/search", r.URL.Path)
		}
		jsonResponse(t, w, []map[string]interface{}{
			{"id": "/vllm-project/vllm", "title": "vLLM", "description": strings.Repeat("d", 150), "trustScore": 9.5, "totalSnippets": 40.0, "stars": 100.0},
		})
	})
	out := captureStdout(t, func() { cmdKBCtx7Search([]string{"vllm"}) })
	if !strings.Contains(out, "/vllm-project/vllm") || !strings.Contains(out, "trust: 9.5") {
		t.Errorf("output missing library id or trust line:\n%s", out)
	}
	if !strings.Contains(out, "...") {
		t.Errorf("150-char description should be truncated:\n%s", out)
	}
	if !strings.Contains(out, "bd kb import-ctx7") {
		t.Errorf("output should end with the import hint:\n%s", out)
	}
}

func TestCmdKBCtx7SearchNoResults(t *testing.T) {
	kbTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(t, w, []map[string]interface{}{})
	})
	out := captureStdout(t, func() { cmdKBCtx7Search([]string{"nope"}) })
	if !strings.Contains(out, "No libraries found.") {
		t.Errorf("output = %q; want empty-state message", out)
	}
}

// --- bd kb import-url ---

func TestCmdKBImportURLDefaultsNameFromURL(t *testing.T) {
	var got map[string]interface{}
	kbTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/knowledge/documents" {
			t.Errorf("path = %s; want /api/knowledge/documents", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decoding import payload: %v", err)
		}
		jsonResponse(t, w, map[string]interface{}{"fact_count": 7.0, "slug": "getting-started"})
	})

	out := captureStdout(t, func() {
		cmdKBImportURL([]string{"https://example.com/docs/getting-started.html"})
	})

	if got["name"] != "getting-started" {
		t.Errorf("payload name = %v; want URL basename without extension", got["name"])
	}
	if got["url"] != "https://example.com/docs/getting-started.html" {
		t.Errorf("payload url = %v", got["url"])
	}
	if got["layer"] != "project" {
		t.Errorf("payload layer = %v; want default \"project\"", got["layer"])
	}
	if !strings.Contains(out, `Imported "getting-started"`) || !strings.Contains(out, "7 facts") {
		t.Errorf("output = %q; want import confirmation with fact count", out)
	}
}

func TestCmdKBImportURLExplicitNameAndLayer(t *testing.T) {
	var got map[string]interface{}
	kbTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		jsonResponse(t, w, map[string]interface{}{"fact_count": 1.0, "slug": "s"})
	})
	captureStdout(t, func() {
		cmdKBImportURL([]string{"--name", "my doc", "--layer", "community", "https://example.com/x"})
	})
	if got["name"] != "my doc" {
		t.Errorf("payload name = %v; want explicit --name", got["name"])
	}
	if got["layer"] != "community" {
		t.Errorf("payload layer = %v; want --layer value", got["layer"])
	}
}

// --- bd kb import-file ---

func TestCmdKBImportFileDefaultsNameFromFilename(t *testing.T) {
	var got map[string]interface{}
	kbTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		jsonResponse(t, w, map[string]interface{}{"fact_count": 2.0, "slug": "runbook"})
	})

	path := filepath.Join(t.TempDir(), "runbook.md")
	if err := os.WriteFile(path, []byte("# runbook"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() { cmdKBImportFile([]string{path}) })

	if got["name"] != "runbook" {
		t.Errorf("payload name = %v; want filename without extension", got["name"])
	}
	if got["file_path"] != path {
		t.Errorf("payload file_path = %v; want %s", got["file_path"], path)
	}
	if !strings.Contains(out, `Imported "runbook"`) || !strings.Contains(out, "2 facts") {
		t.Errorf("output = %q; want import confirmation", out)
	}
}

// --- bd kb import-ctx7 ---

func TestCmdKBImportCtx7SendsContext7ID(t *testing.T) {
	var got map[string]interface{}
	kbTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		jsonResponse(t, w, map[string]interface{}{"fact_count": 5.0, "slug": "vllm"})
	})

	out := captureStdout(t, func() { cmdKBImportCtx7([]string{"/vllm-project/vllm"}) })

	if got["context7_id"] != "/vllm-project/vllm" {
		t.Errorf("payload context7_id = %v; want the library ID", got["context7_id"])
	}
	if got["name"] != "vllm-project-vllm" {
		t.Errorf("payload name = %v; want slashes collapsed to dashes", got["name"])
	}
	if got["layer"] != "community" {
		t.Errorf("payload layer = %v; want default \"community\"", got["layer"])
	}
	if !strings.Contains(out, "5 facts") {
		t.Errorf("output = %q; want fact count", out)
	}
}

func TestCmdKBImportCtx7QuerySuffixesName(t *testing.T) {
	var got map[string]interface{}
	kbTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		jsonResponse(t, w, map[string]interface{}{"fact_count": 1.0, "slug": "s"})
	})
	captureStdout(t, func() {
		cmdKBImportCtx7([]string{"--query", "scheduling", "/vllm-project/vllm"})
	})
	if got["name"] != "vllm-project-vllm (scheduling)" {
		t.Errorf("payload name = %v; want query suffixed in parentheses", got["name"])
	}
}
