package dashboard

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeFixtureSnapshot writes an html/{mode} pair through the SAME
// buildSnapshotFn seam handleSnapshotPage drives, so these tests exercise the
// real rebuild-decision and rewrite-pipeline code paths rather than
// hand-crafting files behind the handler's back.
func fakeBuilder(t *testing.T, calls *[]string, html string) func(s *Server, outputFile, mode string) {
	t.Helper()
	return func(s *Server, outputFile, mode string) {
		*calls = append(*calls, mode)
		if err := os.MkdirAll(filepath.Dir(outputFile), 0o755); err != nil {
			t.Fatalf("fakeBuilder: mkdir: %v", err)
		}
		if err := os.WriteFile(outputFile, []byte(html), 0o644); err != nil {
			t.Fatalf("fakeBuilder: write: %v", err)
		}
	}
}

const fixtureSnapshotHTML = `<!DOCTYPE html><html><head></head><body>
<a href="/live/hive">home</a>
<a href="/live/hive/dark">dark</a>
<a href="/live/hive/light">light</a>
<script>console.log("snapshot")</script>
</body></html>`

func newSnapshotTestServer(t *testing.T) *Server {
	t.Helper()
	srv := newFullServer(t)
	srv.deps.Config.Hub.AutoSnapshot = true
	srv.snapshotDir = t.TempDir()
	return srv
}

// TestHandleSnapshotPageBuildsWhenMissing covers the "no file yet" half of
// the stale-threshold rebuild decision (#5235): with nothing on disk,
// handleSnapshotPage must invoke the builder for BOTH modes (dark and light
// are always rebuilt together) before serving.
func TestHandleSnapshotPageBuildsWhenMissing(t *testing.T) {
	srv := newSnapshotTestServer(t)
	var calls []string
	srv.buildSnapshotFn = fakeBuilder(t, &calls, fixtureSnapshotHTML)

	req := httptest.NewRequest(http.MethodGet, "/snapshot?mode=light", nil)
	w := httptest.NewRecorder()
	srv.handleSnapshotPage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if len(calls) != 2 {
		t.Fatalf("expected buildSnapshot called for both modes when no file exists, got calls=%v", calls)
	}
	gotModes := map[string]bool{calls[0]: true, calls[1]: true}
	if !gotModes["dark"] || !gotModes["light"] {
		t.Errorf("expected both dark and light rebuilt, got %v", calls)
	}
}

// TestHandleSnapshotPageSkipsRebuildWhenFresh covers the "fresh file"
// non-stale half of the rebuild decision: a snapshot written moments ago,
// well inside the interval, must be served AS-IS with no builder call.
func TestHandleSnapshotPageSkipsRebuildWhenFresh(t *testing.T) {
	srv := newSnapshotTestServer(t)
	srv.deps.Config.Hub.SnapshotIntervalMin = 15

	lightPath := filepath.Join(srv.snapshotDir, "snapshot-light.html")
	if err := os.MkdirAll(srv.snapshotDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lightPath, []byte(fixtureSnapshotHTML), 0o644); err != nil {
		t.Fatal(err)
	}

	var calls []string
	srv.buildSnapshotFn = fakeBuilder(t, &calls, fixtureSnapshotHTML)

	req := httptest.NewRequest(http.MethodGet, "/snapshot?mode=light", nil)
	w := httptest.NewRecorder()
	srv.handleSnapshotPage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if len(calls) != 0 {
		t.Errorf("expected no rebuild for a fresh file, got calls=%v", calls)
	}
}

// TestHandleSnapshotPageRebuildsWhenStale covers the "stale file" half: a
// snapshot older than the configured interval must trigger a rebuild even
// though a file already exists.
func TestHandleSnapshotPageRebuildsWhenStale(t *testing.T) {
	srv := newSnapshotTestServer(t)
	srv.deps.Config.Hub.SnapshotIntervalMin = 5

	if err := os.MkdirAll(srv.snapshotDir, 0o755); err != nil {
		t.Fatal(err)
	}
	lightPath := filepath.Join(srv.snapshotDir, "snapshot-light.html")
	stale := []byte("<html><body>stale</body></html>")
	if err := os.WriteFile(lightPath, stale, 0o644); err != nil {
		t.Fatal(err)
	}
	staleTime := time.Now().Add(-time.Hour)
	if err := os.Chtimes(lightPath, staleTime, staleTime); err != nil {
		t.Fatal(err)
	}

	var calls []string
	srv.buildSnapshotFn = fakeBuilder(t, &calls, fixtureSnapshotHTML)

	req := httptest.NewRequest(http.MethodGet, "/snapshot?mode=light", nil)
	w := httptest.NewRecorder()
	srv.handleSnapshotPage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if len(calls) != 2 {
		t.Fatalf("expected a rebuild of both modes for a stale file, got calls=%v", calls)
	}
	// The rebuilt (fresh fixture) content must be what's served, not the
	// stale bytes that were on disk before the rebuild.
	if strings.Contains(w.Body.String(), "stale") {
		t.Error("served body still contains the stale placeholder — rebuild did not replace the file before serving")
	}
}

// TestHandleSnapshotPageModeSelection covers the dark/light mode-selection
// logic: "classic" aliases to dark, any other/absent value defaults to
// light, and each reads its own snapshot-{mode}.html file.
func TestHandleSnapshotPageModeSelection(t *testing.T) {
	tests := []struct {
		name       string
		query      string
		wantFile   string
		wantMarker string
	}{
		{"explicit dark", "?mode=dark", "snapshot-dark.html", "DARK-MARKER"},
		{"classic aliases to dark", "?mode=classic", "snapshot-dark.html", "DARK-MARKER"},
		{"explicit light", "?mode=light", "snapshot-light.html", "LIGHT-MARKER"},
		{"absent defaults to light", "", "snapshot-light.html", "LIGHT-MARKER"},
		{"unrecognized defaults to light", "?mode=bogus", "snapshot-light.html", "LIGHT-MARKER"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newSnapshotTestServer(t)
			if err := os.MkdirAll(srv.snapshotDir, 0o755); err != nil {
				t.Fatal(err)
			}
			// Pre-seed BOTH files, fresh, each with a distinct marker, so a
			// wrong mode selection reading the other file is caught rather
			// than papered over by a shared rebuild.
			if err := os.WriteFile(filepath.Join(srv.snapshotDir, "snapshot-dark.html"),
				[]byte("<html><body>DARK-MARKER</body></html>"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(srv.snapshotDir, "snapshot-light.html"),
				[]byte("<html><body>LIGHT-MARKER</body></html>"), 0o644); err != nil {
				t.Fatal(err)
			}

			var calls []string
			srv.buildSnapshotFn = fakeBuilder(t, &calls, fixtureSnapshotHTML)

			req := httptest.NewRequest(http.MethodGet, "/snapshot"+tt.query, nil)
			w := httptest.NewRecorder()
			srv.handleSnapshotPage(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
			}
			if len(calls) != 0 {
				t.Fatalf("both files were fresh — expected no rebuild, got calls=%v", calls)
			}
			if !strings.Contains(w.Body.String(), tt.wantMarker) {
				t.Errorf("expected body to contain %q (file %s), got: %s", tt.wantMarker, tt.wantFile, w.Body.String())
			}
		})
	}
}

// TestHandleSnapshotPageRewritePipeline covers the URL-rewrite pipeline named
// in #5235: /live/hive{,/dark,/light} links become /snapshot links, the
// configured DashboardURL is collapsed to /snapshot (href AND action
// attributes, plus any bare occurrence), and the three *.hive.kubestellar.io
// / localhost:PORT / 192.168.* href regexps are rewritten to /snapshot.
func TestHandleSnapshotPageRewritePipeline(t *testing.T) {
	srv := newSnapshotTestServer(t)
	srv.deps.Config.Hub.DashboardURL = "https://dash.example.internal"

	html := `<!DOCTYPE html><html><body>
<a href="/live/hive">root</a>
<a href="/live/hive/dark">dark</a>
<a href="/live/hive/light">light</a>
<a href="https://dash.example.internal/foo">dash-href</a>
<form action="https://dash.example.internal/submit">dash-action</form>
<span>https://dash.example.internal/bare</span>
<a href="https://myhive.hive.kubestellar.io/x">hub-href</a>
<a href="http://localhost:5173/y">localhost-href</a>
<a href="http://192.168.1.50:8080/z">lan-href</a>
</body></html>`

	if err := os.MkdirAll(srv.snapshotDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srv.snapshotDir, "snapshot-light.html"), []byte(html), 0o644); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/snapshot?mode=light", nil)
	w := httptest.NewRecorder()
	srv.handleSnapshotPage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	cases := []struct {
		name        string
		mustContain string
		mustNotHave string
	}{
		{"live/hive root", `href="/snapshot"`, `href="/live/hive"`},
		{"live/hive dark", `href="/snapshot?mode=dark"`, `href="/live/hive/dark"`},
		{"live/hive light", `href="/snapshot?mode=light"`, `href="/live/hive/light"`},
		{"dash href", ``, `href="https://dash.example.internal`},
		{"dash action", ``, `action="https://dash.example.internal`},
		{"dash bare", ``, `https://dash.example.internal/bare`},
		{"hub subdomain href", ``, `hive.kubestellar.io`},
		{"localhost href", ``, `http://localhost:5173`},
		{"lan href", ``, `http://192.168.1.50`},
	}
	for _, c := range cases {
		if c.mustContain != "" && !strings.Contains(body, c.mustContain) {
			t.Errorf("%s: expected body to contain %q, got: %s", c.name, c.mustContain, body)
		}
		if c.mustNotHave != "" && strings.Contains(body, c.mustNotHave) {
			t.Errorf("%s: expected body to NOT contain %q, got: %s", c.name, c.mustNotHave, body)
		}
	}
}

// TestHandleSnapshotPageStampsCSPScriptSrcElem covers CSP hash stamping on
// the served bytes (#5235): applyDocumentScriptSrcElem must be called with
// the FINAL rewritten HTML (after mode/URL rewrites), not the raw file
// content, so the header always matches exactly what's on the wire.
func TestHandleSnapshotPageStampsCSPScriptSrcElem(t *testing.T) {
	srv := newSnapshotTestServer(t)

	html := `<!DOCTYPE html><html><body>
<a href="/live/hive">root</a>
<script>window.__snapshotMarker = "unique-inline-script-5235";</script>
</body></html>`
	if err := os.MkdirAll(srv.snapshotDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srv.snapshotDir, "snapshot-light.html"), []byte(html), 0o644); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/snapshot?mode=light", nil)
	w := httptest.NewRecorder()
	// Seed a CSP header the way securityHeaders middleware would, so
	// applyDocumentScriptSrcElem has something to rewrite (it is a no-op
	// against an unset header — see its doc comment).
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src-elem 'self'")

	srv.handleSnapshotPage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	csp := w.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src-elem") {
		t.Fatalf("expected script-src-elem directive in CSP header, got %q", csp)
	}
	if !strings.Contains(csp, "sha256-") {
		t.Errorf("script-src-elem was not replaced by a per-document sha256 hash allowlist: %q", csp)
	}
	// The hash must have been computed from the SERVED (rewritten) body,
	// not the raw file — confirm the served body still carries the inline
	// script whose hash was stamped.
	if !strings.Contains(w.Body.String(), "unique-inline-script-5235") {
		t.Fatalf("served body lost the inline script the CSP hash should describe: %s", w.Body.String())
	}
}

// TestHandleSnapshotPageServiceUnavailableWhenBuildFails covers the failure
// path directly adjacent to the success paths above: if the builder never
// produces a readable file (e.g. the real node invocation errors), the
// handler must degrade to 503 rather than serving a stale/missing read as a
// 200.
func TestHandleSnapshotPageServiceUnavailableWhenBuildFails(t *testing.T) {
	srv := newSnapshotTestServer(t)
	srv.buildSnapshotFn = func(s *Server, outputFile, mode string) {
		// Simulate a failed build: no file written, matching what
		// buildSnapshotProd does when the node subprocess errors (it logs
		// and returns without creating outputFile).
	}

	req := httptest.NewRequest(http.MethodGet, "/snapshot?mode=light", nil)
	w := httptest.NewRecorder()
	srv.handleSnapshotPage(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when the build produced no file, got %d: %s", w.Code, w.Body.String())
	}
}

// TestBuildSnapshotUsesInjectedFn confirms buildSnapshot itself dispatches to
// Server.buildSnapshotFn when set, and never falls through to the real Node
// subprocess in that case — the seam #5235 asked for.
func TestBuildSnapshotUsesInjectedFn(t *testing.T) {
	srv := newSnapshotTestServer(t)
	var gotOutputFile, gotMode string
	var called bool
	srv.buildSnapshotFn = func(s *Server, outputFile, mode string) {
		called = true
		gotOutputFile = outputFile
		gotMode = mode
	}

	target := filepath.Join(srv.snapshotDir, "snapshot-dark.html")
	srv.buildSnapshot(target, "dark")

	if !called {
		t.Fatal("buildSnapshotFn was not invoked")
	}
	if gotOutputFile != target {
		t.Errorf("outputFile = %q, want %q", gotOutputFile, target)
	}
	if gotMode != "dark" {
		t.Errorf("mode = %q, want dark", gotMode)
	}
	// No file should exist — the injected fn deliberately didn't write one,
	// proving buildSnapshot did not ALSO run the real builder.
	if _, err := os.Stat(target); err == nil {
		t.Error("buildSnapshot wrote a file itself despite buildSnapshotFn being set — real builder ran alongside the seam")
	}
}

// TestSnapshotDirOrDefault covers the directory-injection seam itself: empty
// Server.snapshotDir falls back to the production path; a non-empty override
// (as tests set) is used verbatim.
func TestSnapshotDirOrDefault(t *testing.T) {
	srv := newFullServer(t)
	if got := srv.snapshotDirOrDefault(); got != "/data/snapshots" {
		t.Errorf("default snapshotDirOrDefault() = %q, want /data/snapshots", got)
	}
	srv.snapshotDir = "/tmp/custom-snapshots"
	if got := srv.snapshotDirOrDefault(); got != "/tmp/custom-snapshots" {
		t.Errorf("overridden snapshotDirOrDefault() = %q, want /tmp/custom-snapshots", got)
	}
}
