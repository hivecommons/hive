package main

// Exit-path tests for the bd kb subcommands. Every failure arm in kb.go —
// missing-argument validation, kbGet/kbPost HTTP errors, invalid-JSON
// responses, and `kb read`'s slug-not-found — terminates with os.Exit(1), so
// none of them can run in-process. Each test re-execs the test binary through
// the TestBDHelperProcess harness (main_dispatch_test.go), pointing
// BD_DASHBOARD_URL at an httptest server in the parent: the child inherits the
// env via os.Environ(), which is the same seam the real CLI resolves through
// dashboardURL().

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// kbExitTestServer starts an httptest server and points BD_DASHBOARD_URL at it
// so the subprocess spawned by runMainExpectExit1 talks to it. Unlike
// kbTestServer (kb_test.go) the caller here is never in-process — the env var
// only matters because os.Environ() carries it into the child.
func kbExitTestServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	t.Setenv("BD_DASHBOARD_URL", srv.URL)
	return srv
}

func serve500(t *testing.T) *httptest.Server {
	t.Helper()
	return kbExitTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "backend exploded", http.StatusInternalServerError)
	})
}

func serveNotJSON(t *testing.T) *httptest.Server {
	t.Helper()
	return kbExitTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "<html>definitely not json</html>")
	})
}

// --- kb search ---

func TestKBSearchNoQueryExits1(t *testing.T) {
	out := runMainExpectExit1(t, "kb", "search")
	if !strings.Contains(out, "query required") {
		t.Errorf("bd kb search output = %q; want query-required error", out)
	}
}

func TestKBSearchHTTPErrorExits1(t *testing.T) {
	serve500(t)
	out := runMainExpectExit1(t, "kb", "search", "governor")
	if !strings.Contains(out, "bd kb search:") || !strings.Contains(out, "HTTP 500") {
		t.Errorf("bd kb search output = %q; want HTTP 500 error naming the subcommand", out)
	}
}

func TestKBSearchInvalidJSONExits1(t *testing.T) {
	serveNotJSON(t)
	out := runMainExpectExit1(t, "kb", "search", "governor")
	if !strings.Contains(out, "bd kb search: invalid response") {
		t.Errorf("bd kb search output = %q; want invalid-response error", out)
	}
}

// --- kb read ---

func TestKBReadNoSlugExits1(t *testing.T) {
	out := runMainExpectExit1(t, "kb", "read")
	if !strings.Contains(out, "slug required") {
		t.Errorf("bd kb read output = %q; want slug-required error", out)
	}
}

func TestKBReadHTTPErrorExits1(t *testing.T) {
	serve500(t)
	out := runMainExpectExit1(t, "kb", "read", "some-slug")
	if !strings.Contains(out, "bd kb read:") || !strings.Contains(out, "HTTP 500") {
		t.Errorf("bd kb read output = %q; want HTTP 500 error naming the subcommand", out)
	}
}

func TestKBReadInvalidJSONExits1(t *testing.T) {
	serveNotJSON(t)
	out := runMainExpectExit1(t, "kb", "read", "some-slug")
	if !strings.Contains(out, "bd kb read: invalid response") {
		t.Errorf("bd kb read output = %q; want invalid-response error", out)
	}
}

func TestKBReadSlugNotFoundExits1(t *testing.T) {
	kbExitTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		// A hit on a DIFFERENT slug: the search matched, but no exact slug
		// equality — the arm distinct from an empty result set.
		io.WriteString(w, `[{"slug":"other-fact","title":"Other","body":"..."}]`)
	})
	out := runMainExpectExit1(t, "kb", "read", "missing-slug")
	if !strings.Contains(out, `fact "missing-slug" not found`) {
		t.Errorf("bd kb read output = %q; want slug-not-found error", out)
	}
}

// --- kb import-url ---

func TestKBImportURLNoURLExits1(t *testing.T) {
	out := runMainExpectExit1(t, "kb", "import-url")
	if !strings.Contains(out, "URL required") {
		t.Errorf("bd kb import-url output = %q; want URL-required error", out)
	}
}

func TestKBImportURLHTTPErrorExits1(t *testing.T) {
	serve500(t)
	out := runMainExpectExit1(t, "kb", "import-url", "https://example.com/doc")
	if !strings.Contains(out, "bd kb import-url:") || !strings.Contains(out, "HTTP 500") {
		t.Errorf("bd kb import-url output = %q; want HTTP 500 error naming the subcommand", out)
	}
}

func TestKBImportURLInvalidJSONExits1(t *testing.T) {
	serveNotJSON(t)
	out := runMainExpectExit1(t, "kb", "import-url", "https://example.com/doc")
	if !strings.Contains(out, "bd kb import-url: invalid response") {
		t.Errorf("bd kb import-url output = %q; want invalid-response error", out)
	}
}

// --- kb import-file ---

func TestKBImportFileNoPathExits1(t *testing.T) {
	out := runMainExpectExit1(t, "kb", "import-file")
	if !strings.Contains(out, "file path required") {
		t.Errorf("bd kb import-file output = %q; want path-required error", out)
	}
}

func TestKBImportFileHTTPErrorExits1(t *testing.T) {
	serve500(t)
	out := runMainExpectExit1(t, "kb", "import-file", "/some/doc.md")
	if !strings.Contains(out, "bd kb import-file:") || !strings.Contains(out, "HTTP 500") {
		t.Errorf("bd kb import-file output = %q; want HTTP 500 error naming the subcommand", out)
	}
}

func TestKBImportFileInvalidJSONExits1(t *testing.T) {
	serveNotJSON(t)
	out := runMainExpectExit1(t, "kb", "import-file", "/some/doc.md")
	if !strings.Contains(out, "bd kb import-file: invalid response") {
		t.Errorf("bd kb import-file output = %q; want invalid-response error", out)
	}
}

// --- kb list-docs ---

func TestKBListDocsHTTPErrorExits1(t *testing.T) {
	serve500(t)
	out := runMainExpectExit1(t, "kb", "list-docs")
	if !strings.Contains(out, "bd kb list-docs:") || !strings.Contains(out, "HTTP 500") {
		t.Errorf("bd kb list-docs output = %q; want HTTP 500 error naming the subcommand", out)
	}
}

func TestKBListDocsInvalidJSONExits1(t *testing.T) {
	serveNotJSON(t)
	out := runMainExpectExit1(t, "kb", "list-docs")
	if !strings.Contains(out, "bd kb list-docs: invalid response") {
		t.Errorf("bd kb list-docs output = %q; want invalid-response error", out)
	}
}

// --- kb ctx7-search ---

func TestKBCtx7SearchNoQueryExits1(t *testing.T) {
	out := runMainExpectExit1(t, "kb", "ctx7-search")
	if !strings.Contains(out, "library name required") {
		t.Errorf("bd kb ctx7-search output = %q; want name-required error", out)
	}
}

func TestKBCtx7SearchHTTPErrorExits1(t *testing.T) {
	serve500(t)
	out := runMainExpectExit1(t, "kb", "ctx7-search", "vllm")
	if !strings.Contains(out, "bd kb ctx7-search:") || !strings.Contains(out, "HTTP 500") {
		t.Errorf("bd kb ctx7-search output = %q; want HTTP 500 error naming the subcommand", out)
	}
}

func TestKBCtx7SearchInvalidJSONExits1(t *testing.T) {
	serveNotJSON(t)
	out := runMainExpectExit1(t, "kb", "ctx7-search", "vllm")
	if !strings.Contains(out, "bd kb ctx7-search: invalid response") {
		t.Errorf("bd kb ctx7-search output = %q; want invalid-response error", out)
	}
}

// --- kb import-ctx7 ---

func TestKBImportCtx7NoIDExits1(t *testing.T) {
	out := runMainExpectExit1(t, "kb", "import-ctx7")
	if !strings.Contains(out, "library ID required") || !strings.Contains(out, "ctx7-search") {
		t.Errorf("bd kb import-ctx7 output = %q; want ID-required error pointing at ctx7-search", out)
	}
}

func TestKBImportCtx7HTTPErrorExits1(t *testing.T) {
	serve500(t)
	out := runMainExpectExit1(t, "kb", "import-ctx7", "/vllm-project/vllm")
	if !strings.Contains(out, "bd kb import-ctx7:") || !strings.Contains(out, "HTTP 500") {
		t.Errorf("bd kb import-ctx7 output = %q; want HTTP 500 error naming the subcommand", out)
	}
}

func TestKBImportCtx7InvalidJSONExits1(t *testing.T) {
	serveNotJSON(t)
	out := runMainExpectExit1(t, "kb", "import-ctx7", "/vllm-project/vllm")
	if !strings.Contains(out, "bd kb import-ctx7: invalid response") {
		t.Errorf("bd kb import-ctx7 output = %q; want invalid-response error", out)
	}
}
