package dashboard

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// csp_style_src_test.go covers the style-src half of kubestellar/hive#3848.
//
// #3844 left ONE blanket `style-src 'self' 'unsafe-inline'` covering two things
// with different futures, and #3848 asked for them to be separated and for the
// remainder to be removed or accepted with a written rationale. The split and
// the rationale are ADR-0015; these tests pin both.
//
// The asymmetry that drives everything here: CSP has no nonce form and no hash
// form for an ATTRIBUTE-level style, so the 2061 inline style="" attributes
// cannot be constrained by any policy that also permits them — that allowance is
// ACCEPTED. Inline <style> ELEMENTS do take hashes and there are only seven, so
// that half is CLOSABLE and merely staged.

// servedCSP renders the security headers the way every response gets them.
func servedCSP(t *testing.T) string {
	t.Helper()
	s := &Server{deps: &Dependencies{Config: &config.Config{}}}
	handler := s.securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	csp := rec.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("Content-Security-Policy header missing")
	}
	return csp
}

// TestCSPStyleSrcScopedIntoElemAndAttr is the #3848 part-2 deliverable itself:
// style-src is no longer one blanket token. All three directives must be
// present — the two CSP3 halves plus the CSP2 fallback that older browsers use
// instead of them.
func TestCSPStyleSrcScopedIntoElemAndAttr(t *testing.T) {
	csp := servedCSP(t)

	for _, name := range []string{"style-src", "style-src-elem", "style-src-attr"} {
		if cspDirective(csp, name) == "" {
			t.Errorf("CSP is missing %q — the two style halves must be scoped separately (#3848, ADR-0015)\n got: %s", name, csp)
		}
	}

	// The fallback must keep permitting what the CSP3 halves permit, or a
	// browser without style-src-elem/-attr support enforces a STRICTER policy
	// than the one this project reasoned about — i.e. the split would silently
	// unstyle the UI on exactly the older browsers nobody tests.
	fallback := cspDirective(csp, "style-src")
	if !strings.Contains(fallback, "'self'") || !strings.Contains(fallback, "'unsafe-inline'") {
		t.Errorf("style-src fallback must stay permissive for pre-CSP3 browsers, got %q", fallback)
	}
}

// TestCSPStyleSrcAttrIsAcceptedNotStaged pins the half that is NOT going away.
//
// There is deliberately no tripwire here, unlike style-src-elem below: closing
// this would mean deleting every inline style attribute in the UI, and CSP
// offers nothing in between. If a future change removes 'unsafe-inline' from
// style-src-attr, this test fails and that is CORRECT — it means the UI's inline
// styles just stopped applying.
func TestCSPStyleSrcAttrIsAcceptedNotStaged(t *testing.T) {
	attr := cspDirective(servedCSP(t), "style-src-attr")
	if attr == "" {
		t.Fatal("CSP must declare style-src-attr explicitly (#3848, ADR-0015)")
	}
	if !strings.Contains(attr, "'unsafe-inline'") {
		t.Errorf("style-src-attr lost 'unsafe-inline' (got %q).\n"+
			"The dashboard renders ~2000 inline style=\"\" attributes and CSP has NO nonce or hash "+
			"form for attribute styles, so this is an ACCEPTED allowance (ADR-0015), not staging. "+
			"If the UI genuinely stopped using style attributes, update ADR-0015 in the same PR.", attr)
	}
	// 'unsafe-hashes' would mean enumerating a hash per distinct attribute value
	// and regenerating them on every UI edit — unreviewable, and rejected in
	// ADR-0015. Its appearance means someone tried to "tighten" this into a
	// maintenance trap.
	if strings.Contains(attr, "'unsafe-hashes'") {
		t.Errorf("style-src-attr must not use 'unsafe-hashes' (got %q) — ADR-0015 rejects "+
			"per-attribute hash enumeration as unmaintainable", attr)
	}
}

// TestCSPStyleSrcElemUnsafeInlineIsStaged is the TRIPWIRE for the half that IS
// closable, mirroring TestCSPScriptSrcUnsafeInlineIsStaged for script-src.
//
// The hive serves seven inline <style> elements with deterministic content, so
// this can be closed with hashes computed once at startup. It is staged behind
// script-src on purpose: while inline SCRIPT is permitted, tightening inline
// STYLE buys nothing, because anyone who can inject a <style> element can inject
// a <script> element, which is strictly more capable.
//
// When that ordering is satisfied and the hashes land, this test FAILS by
// design. Do not relax it — INVERT it, so the closed state is pinned in turn.
func TestCSPStyleSrcElemUnsafeInlineIsStaged(t *testing.T) {
	elem := cspDirective(servedCSP(t), "style-src-elem")
	if elem == "" {
		t.Fatal("CSP must declare style-src-elem explicitly (#3848, ADR-0015)")
	}
	if !strings.Contains(elem, "'self'") {
		t.Errorf("style-src-elem must allowlist 'self' for same-origin stylesheets, got %q", elem)
	}
	if !strings.Contains(elem, "'unsafe-inline'") {
		t.Fatalf("style-src-elem no longer has 'unsafe-inline' (got %q).\n"+
			"This is GOOD NEWS: the closable half of #3848's style-src work appears to have landed.\n"+
			"Invert this test into an assertion that 'unsafe-inline' is ABSENT (and that the "+
			"expected 'sha256-...' sources are present), so it can never come back, and update "+
			"ADR-0015's status in the same PR.", elem)
	}
}

// TestCSPStyleSrcSplitIsBehaviourNeutralToday guards the one way this change
// could have done harm: it is meant to NAME the decision, not alter enforcement.
// A browser that honours the CSP3 halves and one that only honours the fallback
// must both end up permitting exactly the same thing.
func TestCSPStyleSrcSplitIsBehaviourNeutralToday(t *testing.T) {
	csp := servedCSP(t)
	fallback := cspDirective(csp, "style-src")
	elem := cspDirective(csp, "style-src-elem")
	attr := cspDirective(csp, "style-src-attr")

	// Inline styles: permitted through the fallback, and through both halves.
	for name, directive := range map[string]string{
		"style-src": fallback, "style-src-elem": elem, "style-src-attr": attr,
	} {
		if !strings.Contains(directive, "'unsafe-inline'") {
			t.Errorf("%s does not permit inline styles (%q) while the others do — "+
				"the split must not change what any browser enforces (ADR-0015)", name, directive)
		}
	}
	// Stylesheet URLs: 'self' on the fallback and on the element half. Attribute
	// styles have no URL source, so style-src-attr correctly carries none.
	if !strings.Contains(elem, "'self'") || !strings.Contains(fallback, "'self'") {
		t.Errorf("style-src/style-src-elem must both allowlist 'self'\n fallback=%q elem=%q", fallback, elem)
	}
	if strings.Contains(attr, "'self'") {
		t.Errorf("style-src-attr carries 'self' (%q), which is meaningless for attribute "+
			"styles — they have no URL source", attr)
	}
}

// TestCSPStyleSrcRationaleIsWritten pins the DELIVERABLE, not just the header.
//
// #3848 accepts "keep 'unsafe-inline' with written rationale" as a legitimate
// outcome for the attribute half — which makes the written rationale part of the
// fix, not documentation of it. An undocumented 'unsafe-inline' is precisely the
// state the issue was filed about, so a future edit that deletes or unlinks the
// ADR while keeping the allowance must fail.
func TestCSPStyleSrcRationaleIsWritten(t *testing.T) {
	const adr = "../../docs/adr/0015-csp-style-src-scope.md"
	body, err := os.ReadFile(adr)
	if err != nil {
		t.Fatalf("ADR-0015 is missing (%v).\nstyle-src-attr 'unsafe-inline' is ACCEPTED only because "+
			"a written rationale exists (#3848); without it the allowance is undocumented again.", err)
	}
	text := string(body)
	for _, want := range []string{
		"style-src-attr",   // the accepted half is named
		"style-src-elem",   // and distinguished from the staged half
		"Status: Accepted", // it is a decision, not a proposal
		"Residual risk",    // what we are accepting is stated
	} {
		if !strings.Contains(text, want) {
			t.Errorf("ADR-0015 does not mention %q — the rationale must state what is accepted and why", want)
		}
	}

	index, err := os.ReadFile("../../docs/adr/README.md")
	if err != nil {
		t.Fatalf("reading ADR index: %v", err)
	}
	if !strings.Contains(string(index), "0015-csp-style-src-scope.md") {
		t.Error("ADR-0015 is not linked from the ADR index — an unlinked ADR is one nobody finds")
	}
}
