package dashboard

import (
	"compress/gzip"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// csp_script_src_test.go covers the script-src half of kubestellar/hive#3848
// (filed again as #3907) — the counterpart of csp_style_src_test.go, written in
// the same shape ADR-0015 established and ADR-0016 continues.
//
// The old tripwire (TestCSPScriptSrcUnsafeInlineIsStaged) demanded inversion,
// not relaxation, when the refactor landed. The refactor landed for the ELEMENT
// half only, so the inversion here is scoped exactly to it:
//
//   - script-src-elem: CLOSED — hash allowlist, no 'unsafe-inline'. Pinned by
//     TestCSPScriptSrcElemUnsafeInlineIsClosed plus the per-document coverage
//     tests, which recompute every served inline script's hash and demand it
//     be allowlisted.
//   - script-src-attr: CLOSED — 'none' after the #3848 event-delegation
//     refactor replaced every inline on*= handler attribute with data-action
//     dispatch. TestCSPScriptSrcAttrUnsafeInlineIsAbsent pins it closed.
//   - script-src: the CSP2 fallback is 'self' only — 'unsafe-inline' dropped
//     at the same moment as script-src-attr's — and must stay HASH-FREE: a
//     hash in that directive would change semantics for hash-aware pre-CSP3
//     browsers.

// sha256SourceRe matches a CSP sha256 hash source token.
var sha256SourceRe = regexp.MustCompile(`'sha256-[A-Za-z0-9+/]+=*'`)

// TestCSPScriptSrcScopedIntoElemAndAttr is the #3848 part-1 deliverable shape:
// script-src is no longer one blanket token — both CSP3 halves are present,
// and the CSP2 fallback keeps exactly its old permissiveness.
func TestCSPScriptSrcScopedIntoElemAndAttr(t *testing.T) {
	csp := servedCSP(t)

	for _, name := range []string{"script-src", "script-src-elem", "script-src-attr"} {
		if cspDirective(csp, name) == "" {
			t.Errorf("CSP is missing %q — the two script halves must be scoped separately (#3848, ADR-0016)\n got: %s", name, csp)
		}
	}

	// The fallback keeps 'self' for pre-CSP3 browsers but no longer needs
	// 'unsafe-inline': the #3848 event-delegation refactor removed every
	// inline handler attribute, so there is nothing inline left to permit.
	fallback := cspDirective(csp, "script-src")
	if !strings.Contains(fallback, "'self'") {
		t.Errorf("script-src fallback must keep 'self' for same-origin script files, got %q", fallback)
	}

	// THE LOAD-BEARING NEGATIVE: the fallback must never carry a hash. Per
	// CSP2, a hash source makes the browser ignore 'unsafe-inline' in the same
	// directive — and keeping this directive semantically stable across
	// hash-aware and hash-unaware pre-CSP3 browsers requires the hashes to
	// live in script-src-elem and ONLY there.
	if sha256SourceRe.MatchString(fallback) {
		t.Errorf("script-src fallback carries a sha256 source (%q) — this DISABLES "+
			"'unsafe-inline' on hash-aware pre-CSP3 browsers and blanks every inline "+
			"handler; hashes may only appear in script-src-elem (ADR-0016)", fallback)
	}
}

// TestCSPScriptSrcElemUnsafeInlineIsClosed is the INVERSION the old tripwire
// demanded, scoped to the half that actually landed: inline <script> elements
// are hash-allowlisted and 'unsafe-inline' is gone from script-src-elem, so an
// injected inline <script> does not execute in any CSP3 browser.
//
// Do not relax this. If a legitimate new inline script stops matching, the fix
// is to serve it through a hashed path (startup set for byte-stable documents,
// applyDocumentScriptSrcElem for rendered ones) — never to re-admit
// 'unsafe-inline' here, which CSP3 browsers would honour for injected scripts
// too the moment the hashes were ever removed.
func TestCSPScriptSrcElemUnsafeInlineIsClosed(t *testing.T) {
	elem := cspDirective(servedCSP(t), "script-src-elem")
	if elem == "" {
		t.Fatal("CSP must declare script-src-elem explicitly (#3848, ADR-0016)")
	}
	if !strings.Contains(elem, "'self'") {
		t.Errorf("script-src-elem must allowlist 'self' for same-origin script files, got %q", elem)
	}
	if strings.Contains(elem, "'unsafe-inline'") {
		t.Errorf("script-src-elem has regressed to 'unsafe-inline' (got %q) — the element "+
			"half of #3848 is CLOSED via hashes and must stay closed (ADR-0016)", elem)
	}
	if strings.Contains(elem, "'unsafe-eval'") {
		t.Errorf("script-src-elem must never permit 'unsafe-eval', got %q", elem)
	}
	// COUNT FLOOR: the startup set covers the embedded SPA (2 inline scripts)
	// and the device-flow login page (1). Fewer hashes than documents means the
	// allowlist stopped covering something that is still being served — which
	// blanks it — or the extraction broke, which is the same failure.
	if got := len(sha256SourceRe.FindAllString(elem, -1)); got < 3 {
		t.Errorf("script-src-elem carries %d sha256 sources, want >= 3 "+
			"(2 for static/index.html + 1 for the login page); allowlist or extraction broke: %q", got, elem)
	}
}

// TestCSPScriptSrcAttrUnsafeInlineIsAbsent is the INVERSION of the old staged
// tripwire (TestCSPScriptSrcAttrUnsafeInlineIsStaged), landed with the #3848
// event-delegation refactor: every inline on*= handler attribute in
// static/index.html and in Go-generated HTML was replaced with data-action /
// data-* attributes dispatched by central document listeners, so
// script-src-attr is 'none' and an injected on*= attribute never executes.
//
// Do not relax this. If a new handler is needed, wire it through the
// data-action dispatcher — never by reintroducing an inline attribute.
func TestCSPScriptSrcAttrUnsafeInlineIsAbsent(t *testing.T) {
	attr := cspDirective(servedCSP(t), "script-src-attr")
	if attr == "" {
		t.Fatal("CSP must declare script-src-attr explicitly (#3848, ADR-0016)")
	}
	if strings.Contains(attr, "'unsafe-inline'") {
		t.Fatalf("script-src-attr must not contain 'unsafe-inline' after the #3848 "+
			"event-delegation refactor (got %q) — inline handler attributes are gone "+
			"and must never come back", attr)
	}
	// The CSP2 fallback must also not carry unsafe-inline: it became droppable
	// at the same moment the attribute half closed.
	src := cspDirective(servedCSP(t), "script-src")
	if strings.Contains(src, "'unsafe-inline'") {
		t.Fatalf("script-src fallback must not contain 'unsafe-inline' after #3848 (got %q)", src)
	}
	// 'unsafe-hashes' would mean a hash per distinct handler-attribute value,
	// regenerated on every UI edit — rejected in ADR-0016 exactly as ADR-0015
	// rejected it for style attributes.
	if strings.Contains(attr, "'unsafe-hashes'") {
		t.Errorf("script-src-attr must not use 'unsafe-hashes' (got %q) — ADR-0016 rejects "+
			"per-attribute hash enumeration as unmaintainable", attr)
	}
	if strings.Contains(attr, "'self'") {
		t.Errorf("script-src-attr carries 'self' (%q), which is meaningless for handler "+
			"attributes — they have no URL source", attr)
	}
}

// TestEmbeddedIndexScriptsSatisfyCSPHashes proves the SPA document actually
// passes the policy served with it — recomputing each inline script's hash from
// the response body and demanding it be allowlisted — AND that the #3863
// startup-pre-gzip + strong-ETag design survives, which is the entire reason
// this migration uses hashes instead of nonces.
func TestEmbeddedIndexScriptsSatisfyCSPHashes(t *testing.T) {
	raw, err := fs.ReadFile(staticFS, "static/index.html")
	if err != nil {
		t.Fatalf("reading embedded SPA document: %v", err)
	}

	s := &Server{deps: &Dependencies{Config: &config.Config{}}}
	handler := s.securityHeaders(newIndexDocument(raw, Branding{}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	// #3863 invariants: still pre-gzipped, still carrying a strong ETag.
	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip — the hash migration must not cost the #3863 compression", got)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("ETag missing — the hash migration must not cost the #3863 revalidation path")
	}

	// The served body (decompressed) is byte-identical to the embedded source,
	// so hashes computed at startup from the embed describe the served bytes.
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("body is not valid gzip: %v", err)
	}
	body, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("decompressing body: %v", err)
	}
	// The invariant is that hashes describe the SERVED bytes — not that the
	// served bytes equal the embed. Branding may legitimately rewrite the
	// document (including inline script content, e.g. the Getting Started
	// flyer builds DOM from a string literal containing the bee mark), so the
	// CSP layer hashes the served document. Assert the real property: every
	// inline script in the served body is authorised by the header.
	served := scriptSrcElemSources(body)
	for _, sc := range extractInlineScripts(body) {
		if h := cspScriptHash(sc); !strings.Contains(served, h) {
			t.Fatalf("served inline script is not covered by a CSP hash (%s) — "+
				"the browser would block it", h)
		}
	}

	// POSITIVE CONTROL + COUNT FLOOR: the document still contains its scripts.
	scripts := extractInlineScripts(body)
	if len(scripts) != 2 {
		t.Fatalf("extracted %d inline scripts from static/index.html, want 2 — "+
			"if the SPA gained or lost a script block, update this floor AND confirm the new "+
			"block is served hashed", len(scripts))
	}
	elem := cspDirective(rec.Header().Get("Content-Security-Policy"), "script-src-elem")
	for i, script := range scripts {
		if h := cspScriptHash(script); !strings.Contains(elem, h) {
			t.Errorf("inline script #%d of the served SPA is not allowlisted (%s missing) — "+
				"the dashboard would render BLANK in every CSP3 browser\n elem: %s", i, h, elem)
		}
	}

	// #3863 invariant: revalidation still yields a 304 with no body, and the
	// 304 carries the same CSP so a revalidated page is governed identically.
	req304 := httptest.NewRequest(http.MethodGet, "/", nil)
	req304.Header.Set("If-None-Match", etag)
	rec304 := httptest.NewRecorder()
	handler.ServeHTTP(rec304, req304)
	if rec304.Code != http.StatusNotModified {
		t.Fatalf("If-None-Match revalidation status = %d, want 304", rec304.Code)
	}
	if got := cspDirective(rec304.Header().Get("Content-Security-Policy"), "script-src-elem"); got != elem {
		t.Errorf("304 response CSP script-src-elem differs from the 200's\n 200: %s\n 304: %s", elem, got)
	}
}

// TestLoginPageScriptSatisfiesCSPHashes covers the other byte-stable document
// in the startup set: the device-flow login page, served with the same base
// header to every unauthenticated browser path.
func TestLoginPageScriptSatisfiesCSPHashes(t *testing.T) {
	scripts := extractInlineScripts([]byte(loginPage))
	if len(scripts) != 1 {
		t.Fatalf("extracted %d inline scripts from the login page, want 1 — "+
			"update the floor AND the startup hash set together", len(scripts))
	}
	elem := cspDirective(servedCSP(t), "script-src-elem")
	if h := cspScriptHash(scripts[0]); !strings.Contains(elem, h) {
		t.Errorf("the login page's inline script is not allowlisted (%s missing) — "+
			"sign-in would break in every CSP3 browser\n elem: %s", h, elem)
	}
}

// TestContributePageScriptsSatisfyPerResponseCSP proves the per-response lane:
// the /contribute document's inline scripts vary with the request (hubURL from
// the Host header), so its handler must stamp hashes computed from the exact
// bytes it writes. Two different Hosts produce two different documents, and
// each response's policy must cover its own body.
func TestContributePageScriptsSatisfyPerResponseCSP(t *testing.T) {
	srv := newFullServer(t)

	render := func(host string) (elem string, body []byte) {
		t.Helper()
		handler := srv.securityHeaders(http.HandlerFunc(srv.handleContributeLanding))
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/contribute", nil)
		req.Host = host
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("contribute landing status = %d, want 200", rec.Code)
		}
		return cspDirective(rec.Header().Get("Content-Security-Policy"), "script-src-elem"), rec.Body.Bytes()
	}

	for _, host := range []string{"hub.example.com", "other-hub.example.org:8443"} {
		elem, body := render(host)
		if strings.Contains(elem, "'unsafe-inline'") {
			t.Errorf("host %s: script-src-elem must not carry 'unsafe-inline', got %q", host, elem)
		}
		scripts := extractInlineScripts(body)
		// COUNT FLOOR: the page ships 4 unconditional inline script blocks
		// today; zero or few means extraction broke and the policy is vacuous.
		if len(scripts) < 4 {
			t.Fatalf("host %s: extracted %d inline scripts from /contribute, want >= 4", host, len(scripts))
		}
		for i, script := range scripts {
			if h := cspScriptHash(script); !strings.Contains(elem, h) {
				t.Errorf("host %s: /contribute inline script #%d not allowlisted (%s missing) — "+
					"the page would render dead in every CSP3 browser", host, i, h)
			}
		}
	}

	// The two documents differ (different hubURL), so at least one hash must
	// differ too — proving the header really is computed per response rather
	// than baked once.
	elemA, _ := render("hub.example.com")
	elemB, _ := render("other-hub.example.org:8443")
	if elemA == elemB {
		t.Error("script-src-elem identical across different Hosts — per-response hashing is not happening")
	}
}

// TestApplyDocumentScriptSrcElem pins the header-rewrite helper the dynamic
// handlers depend on.
func TestApplyDocumentScriptSrcElem(t *testing.T) {
	doc := []byte(`<html><head><script>alert("ours")</script></head></html>`)
	wantHash := cspScriptHash([]byte(`alert("ours")`))

	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Security-Policy",
		"default-src 'self'; script-src 'self' 'unsafe-inline'; script-src-elem 'self' 'sha256-stale='; script-src-attr 'unsafe-inline'; style-src 'self'")
	applyDocumentScriptSrcElem(rec, doc)
	csp := rec.Header().Get("Content-Security-Policy")

	elem := cspDirective(csp, "script-src-elem")
	if !strings.Contains(elem, wantHash) {
		t.Errorf("rewritten script-src-elem missing the document's hash: %q", elem)
	}
	if strings.Contains(elem, "sha256-stale=") {
		t.Errorf("rewritten script-src-elem kept a stale hash: %q", elem)
	}
	// Neighbouring directives must be untouched.
	for _, want := range []string{"default-src 'self'", "script-src 'self' 'unsafe-inline'", "script-src-attr 'unsafe-inline'", "style-src 'self'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("rewrite disturbed a neighbouring directive: %q missing from %q", want, csp)
		}
	}

	// A script-free document yields a hash-free (but still closed) directive.
	rec2 := httptest.NewRecorder()
	rec2.Header().Set("Content-Security-Policy", "script-src-elem 'self' 'sha256-stale='")
	applyDocumentScriptSrcElem(rec2, []byte("<html>no scripts</html>"))
	if got := cspDirective(rec2.Header().Get("Content-Security-Policy"), "script-src-elem"); got != "script-src-elem 'self'" {
		t.Errorf("script-free document: script-src-elem = %q, want \"script-src-elem 'self'\"", got)
	}

	// No CSP header (not routed through securityHeaders): leave untouched.
	rec3 := httptest.NewRecorder()
	applyDocumentScriptSrcElem(rec3, doc)
	if got := rec3.Header().Get("Content-Security-Policy"); got != "" {
		t.Errorf("helper invented a CSP header out of nothing: %q", got)
	}
}

// TestDynamicHTMLHandlersStampDocumentCSP asserts the gate IN SOURCE: both
// handlers that render per-response HTML documents must call
// applyDocumentScriptSrcElem before writing. The /snapshot lane reads its
// document from /data at runtime, so this source-level pin is what a unit test
// can hold it to; TestContributePageScriptsSatisfyPerResponseCSP covers the
// /contribute lane end-to-end as well.
func TestDynamicHTMLHandlersStampDocumentCSP(t *testing.T) {
	for file, wantCalls := range map[string]int{
		"api_contribute.go": 1, // handleContributeLanding
		"api.go":            1, // handleSnapshotPage
	} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("reading %s: %v", file, err)
		}
		if got := strings.Count(string(src), "applyDocumentScriptSrcElem(w"); got < wantCalls {
			t.Errorf("%s calls applyDocumentScriptSrcElem %d times, want >= %d — a rendered "+
				"document served without its per-response hashes renders BLANK in CSP3 browsers", file, got, wantCalls)
		}
	}
}

// TestTerminalPathKeepsBlanketScriptSrc pins the deliberate exclusion: ttyd's
// UI is reverse-proxied — this server never holds its document bytes — so
// /terminal keeps the blanket CSP2 policy instead of a hash allowlist computed
// from the WRONG document, which would brick the terminal.
func TestTerminalPathKeepsBlanketScriptSrc(t *testing.T) {
	s := &Server{deps: &Dependencies{Config: &config.Config{}}}
	handler := s.securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, path := range []string{"/terminal", "/terminal/ws"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		csp := rec.Header().Get("Content-Security-Policy")
		if cspDirective(csp, "script-src-elem") != "" {
			t.Errorf("%s: script-src-elem present — a hash allowlist for a document this server "+
				"never renders would brick ttyd\n got: %s", path, csp)
		}
		fallback := cspDirective(csp, "script-src")
		if !strings.Contains(fallback, "'unsafe-inline'") {
			t.Errorf("%s: terminal responses must keep the blanket script-src, got %q", path, fallback)
		}
	}
}

// TestServedDocumentsHaveNoScriptInHTMLComments pins the precondition that
// keeps the hash extraction and the browser's HTML tokenizer in agreement: a
// literal "<script" inside an HTML comment is text to the browser but a tag to
// the extraction regex, which would both hash a phantom script and miss a real
// one — i.e. serve a policy that blanks the page. No document we hash may
// contain one.
func TestServedDocumentsHaveNoScriptInHTMLComments(t *testing.T) {
	htmlCommentRe := regexp.MustCompile(`(?s)<!--.*?-->`)

	index, err := fs.ReadFile(staticFS, "static/index.html")
	if err != nil {
		t.Fatalf("reading embedded SPA document: %v", err)
	}
	docs := map[string][]byte{
		"static/index.html": index,
		"loginPage":         []byte(loginPage),
	}

	srv := newFullServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/contribute", nil)
	req.Host = "hub.example.com"
	srv.handleContributeLanding(rec, req)
	docs["/contribute"] = rec.Body.Bytes()

	for name, doc := range docs {
		for _, comment := range htmlCommentRe.FindAll(doc, -1) {
			if strings.Contains(strings.ToLower(string(comment)), "<script") {
				t.Errorf("%s: HTML comment contains \"<script\" (%.80s…) — this desynchronizes "+
					"CSP hash extraction from the browser's tokenizer; reword the comment", name, comment)
			}
		}
	}
}

// TestCSPScriptSrcRationaleIsWritten pins the deliverable's written half, as
// TestCSPStyleSrcRationaleIsWritten does for ADR-0015: the staged
// script-src-attr allowance is legitimate only while its rationale and
// residual risk are on record and discoverable.
func TestCSPScriptSrcRationaleIsWritten(t *testing.T) {
	const adr = "../../docs/adr/0016-csp-script-src-scope.md"
	body, err := os.ReadFile(adr)
	if err != nil {
		t.Fatalf("ADR-0016 is missing (%v).\nscript-src-attr 'unsafe-inline' is STAGED only because "+
			"a written rationale exists (#3848/#3907); without it the allowance is undocumented again.", err)
	}
	text := string(body)
	for _, want := range []string{
		"script-src-elem", // the closed half is named
		"script-src-attr", // and distinguished from the staged half
		"Status: Accepted",
		"Residual risk",
		"#3863", // the hashes-not-nonces constraint is recorded
	} {
		if !strings.Contains(text, want) {
			t.Errorf("ADR-0016 does not mention %q — the rationale must state what is closed, "+
				"what is staged, and why hashes", want)
		}
	}

	index, err := os.ReadFile("../../docs/adr/README.md")
	if err != nil {
		t.Fatalf("reading ADR index: %v", err)
	}
	if !strings.Contains(string(index), "0016-csp-script-src-scope.md") {
		t.Error("ADR-0016 is not linked from the ADR index — an unlinked ADR is one nobody finds")
	}
}
