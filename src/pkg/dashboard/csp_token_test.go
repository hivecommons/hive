package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// cspDirective returns the named directive from a CSP header value, or "".
func cspDirective(csp, name string) string {
	for _, part := range strings.Split(csp, ";") {
		part = strings.TrimSpace(part)
		if part == name || strings.HasPrefix(part, name+" ") {
			return part
		}
	}
	return ""
}

// TestCSPDirectivesPinned locks the shape of the dashboard CSP (#3315).
//
// The finding paired script-src 'unsafe-inline' with a DASHBOARD_TOKEN written
// into an inline <script>. The token half is fixed (the token is never rendered
// into any response -- see TestDashboardTokenNeverRenderedIntoHTML below), so
// what remains here is pinning the directives that DO NOT depend on inline
// scripts, and making sure the rest cannot silently loosen further.
func TestCSPDirectivesPinned(t *testing.T) {
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

	for _, want := range []string{
		"default-src 'self'",
		"object-src 'none'",
		"base-uri 'self'",
		// form-action was absent before #3315: without it an injected <form>
		// could post operator input to an attacker origin. It does NOT depend
		// on inline scripts, so it is enforced now rather than staged.
		"form-action 'self'",
		"frame-ancestors 'none'",
	} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP missing %q\n got: %s", want, csp)
		}
	}

	// 'unsafe-eval' has never been needed and must never appear.
	if strings.Contains(csp, "'unsafe-eval'") {
		t.Errorf("CSP must never permit 'unsafe-eval'\n got: %s", csp)
	}
}

// The tripwire that lived here — TestCSPScriptSrcUnsafeInlineIsStaged — has
// been INVERTED, exactly as its own instructions and #3848 required, for the
// half that landed: script-src is now scoped into its element and attribute
// halves (#3848 part 1 / #3907, ADR-0016), and the closed element half is
// pinned by TestCSPScriptSrcElemUnsafeInlineIsClosed in csp_script_src_test.go.
// The attribute half is still staged behind the event-delegation refactor and
// carries its own tripwire there (TestCSPScriptSrcAttrUnsafeInlineIsStaged),
// and the CSP2 fallback's deliberate 'unsafe-inline' is pinned by
// TestCSPScriptSrcScopedIntoElemAndAttr.

// TestJSStringLiteralEscapesScriptContext covers the encode-at-the-sink helper
// used for the values injected into the /contribute inline <script> (#3315),
// in BOTH directions.
func TestJSStringLiteralEscapesScriptContext(t *testing.T) {
	// --- NEGATIVE: hostile inputs must not escape the script/string context ---
	hostile := []struct {
		name string
		in   string
	}{
		{"closing script tag", `</script><script>alert(1)</script>`},
		{"uppercase closing tag", `</SCRIPT><SCRIPT>alert(1)</SCRIPT>`},
		{"mixed case closing tag", `</ScRiPt>`},
		{"single quote", `it's`},
		{"double quote", `say "hi"`},
		{"backslash", `back\slash`},
		{"backslash before quote", `trailing\`},
		{"newline", "line1\nline2"},
		{"carriage return", "line1\r\nline2"},
		{"html comment opener", `<!--<script>`},
		{"line separator", "a b"},
		{"null byte", "a\x00b"},
	}
	for _, tc := range hostile {
		t.Run(tc.name, func(t *testing.T) {
			got := jsStringLiteral(tc.in)

			// Must be a quoted literal.
			if len(got) < 2 || got[0] != '"' || got[len(got)-1] != '"' {
				t.Fatalf("jsStringLiteral(%q) = %s, want a quoted JS string literal", tc.in, got)
			}
			// Must not contain a raw script end tag in ANY case, which would
			// close the enclosing <script> element regardless of JS quoting.
			if strings.Contains(strings.ToLower(got), "</script") {
				t.Errorf("jsStringLiteral(%q) = %s, leaks a raw </script", tc.in, got)
			}
			if strings.Contains(got, "<!--") {
				t.Errorf("jsStringLiteral(%q) = %s, leaks a raw <!-- comment opener", tc.in, got)
			}
			// Must not contain a literal newline/CR, which would terminate a JS
			// string literal outright.
			if strings.ContainsAny(got, "\n\r") {
				t.Errorf("jsStringLiteral(%q) = %s, contains a raw newline", tc.in, got)
			}
			// The literal must round-trip back to EXACTLY the input: escaping
			// must not silently corrupt or drop characters. This is what stops
			// "mangle everything" from passing.
			var back string
			unescaped := strings.ReplaceAll(got, `<\/`, `</`)
			unescaped = strings.ReplaceAll(unescaped, `<\!--`, `<!--`)
			if err := json.Unmarshal([]byte(unescaped), &back); err != nil {
				t.Fatalf("jsStringLiteral(%q) = %s, not valid JSON: %v", tc.in, got, err)
			}
			if back != tc.in {
				t.Errorf("round-trip mismatch: got %q, want %q", back, tc.in)
			}
		})
	}

	// --- POSITIVE CONTROL: ordinary values survive intact and readable ---
	benign := []struct {
		in   string
		want string
	}{
		{"kubestellar", `"kubestellar"`},
		{"https://hub.example.com:8443", `"https://hub.example.com:8443"`},
		{"", `""`},
		{"my-project_1", `"my-project_1"`},
	}
	for _, tc := range benign {
		got := jsStringLiteral(tc.in)
		if got != tc.want {
			t.Errorf("jsStringLiteral(%q) = %s, want %s (benign values must not be mangled)", tc.in, got, tc.want)
		}
	}
}

// TestContributeLandingEscapesInjectedJSValues is the end-to-end control: a
// hostile Host header must not break out of the inline <script>, while a normal
// request must still produce a working page.
func TestContributeLandingEscapesInjectedJSValues(t *testing.T) {
	srv := newFullServer(t)

	// POSITIVE CONTROL FIRST: a normal request renders a usable page.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/contribute", nil)
	req.Host = "hub.example.com"
	srv.handleContributeLanding(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("contribute landing status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "var hubURL=") {
		t.Error("contribute page must still declare hubURL")
	}
	if !strings.Contains(body, "var ccProjectName=") {
		t.Error("contribute page must still declare ccProjectName")
	}
	if !strings.Contains(body, "hub.example.com") {
		t.Error("contribute page must still carry the real hub URL (positive control)")
	}
	// Balanced script elements: the page must not have a stray unclosed block.
	if opens, closes := countScriptElements(body); opens != closes {
		t.Errorf("unbalanced script tags: %d open, %d close", opens, closes)
	}

	// The two JS-context values must be emitted as ENCODED literals (produced by
	// jsStringLiteral, hence double-quoted), never dropped into hand-written
	// single quotes. Both are additionally defended upstream -- hubURL by a
	// charset filter (handleContributeLanding) and ccProjectName by
	// html.EscapeString -- so a break-out is not reachable through them TODAY.
	// That is exactly why this asserts on the encoding at the sink rather than
	// on a payload: it is the invariant that survives someone relaxing either
	// remote sanitizer, which is how this class of bug comes back.
	if strings.Contains(body, `var hubURL='`) {
		t.Error("hubURL must be JSON-encoded at the sink, not wrapped in hand-written quotes")
	}
	if strings.Contains(body, `var ccProjectName='`) {
		t.Error("ccProjectName must be JSON-encoded at the sink, not wrapped in hand-written quotes")
	}
	if !strings.Contains(body, `var hubURL="`) {
		t.Error("hubURL must be emitted as a JSON-encoded string literal")
	}
	if !strings.Contains(body, `var ccProjectName="`) {
		t.Error("ccProjectName must be emitted as a JSON-encoded string literal")
	}

	// NEGATIVE: a hostile Host must not inject a script or break the literal.
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/contribute", nil)
	req2.Host = `evil.com'</script><script>alert(1)</script>`
	req2.Header.Set("X-Forwarded-Host", `evil.com'</script><script>alert(1)</script>`)
	srv.handleContributeLanding(rec2, req2)

	body2 := rec2.Body.String()
	if strings.Contains(body2, "alert(1)") {
		t.Error("hostile Host header injected an executable payload into the page")
	}
	if strings.Contains(body2, `'</script><script>`) {
		t.Error("hostile Host header broke out of the inline script context")
	}
	if opens, closes := countScriptElements(body2); opens != closes {
		t.Errorf("hostile Host unbalanced script tags: %d open, %d close", opens, closes)
	}

	// The hostile page must have exactly as many script elements as the benign
	// one: no more (nothing injected), no fewer (nothing swallowed).
	benignOpens, _ := countScriptElements(body)
	hostileOpens, _ := countScriptElements(body2)
	if hostileOpens != benignOpens {
		t.Errorf("hostile Host changed the script-element count: %d vs benign %d",
			hostileOpens, benignOpens)
	}
}

// countScriptElements counts real <script> start and end tags in HTML, ignoring
// the sequence when it appears inside a JS line comment (the page's own source
// comments mention "<script>" in prose, which is not a tag).
func countScriptElements(body string) (opens, closes int) {
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "<!--") {
			continue
		}
		opens += strings.Count(line, "<script>") + strings.Count(line, "<script ")
		closes += strings.Count(line, "</script>")
	}
	return opens, closes
}

// TestDashboardTokenNeverRenderedIntoHTML is the Go-side half of the #3315
// regression guard, with both-direction positive controls.
//
// NEGATIVE: a token full of script-breaking characters must not appear in a
// response body in any form. POSITIVE CONTROL: the handler must still produce
// a real, useful response -- otherwise "return nothing" would pass.
func TestDashboardTokenNeverRenderedIntoHTML(t *testing.T) {
	// SYNTHETIC token designed to escape an inline <script> string context.
	// Never the real HIVE_DASHBOARD_TOKEN.
	const hostile = "abc</script><script>alert('xss')</script>'\"\\\ndef"

	t.Setenv("HIVE_DASHBOARD_TOKEN", hostile)

	srv := newFullServer(t)
	srv.authToken = hostile

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/auth/token", nil)
	srv.handleAuthToken(rec, req)

	body := rec.Body.String()

	// POSITIVE CONTROL: the endpoint must still answer usefully. On a plain
	// self-hosted hive it reports WHETHER a token is configured. If it 404s
	// (direct-route/hub-proxied), that is also a valid non-leaking answer --
	// but it must do one of the two, not return an empty 200.
	switch rec.Code {
	case http.StatusOK:
		if !strings.Contains(body, "configured") {
			t.Errorf("auth/token must still report configured-ness, got %q", body)
		}
		if !strings.Contains(body, "true") {
			t.Errorf("auth/token must report configured=true when a token is set, got %q", body)
		}
	case http.StatusNotFound:
		// Fine: the token is not exposed on this deployment shape at all.
	default:
		t.Fatalf("unexpected status %d body %q", rec.Code, body)
	}

	// NEGATIVE: the token value must never come back, in any encoding.
	if strings.Contains(body, hostile) {
		t.Error("auth/token leaked the token verbatim")
	}
	if strings.Contains(body, "alert('xss')") {
		t.Error("auth/token leaked attacker-controlled payload from the token")
	}
	// The distinctive core must not survive in any partial/escaped rendering.
	if strings.Contains(body, "abc") && strings.Contains(body, "def") {
		t.Errorf("auth/token appears to leak token material: %q", body)
	}
	// A leak via header would be just as bad.
	for name, vals := range rec.Header() {
		for _, v := range vals {
			if strings.Contains(v, "abc") && strings.Contains(v, "def") {
				t.Errorf("token material leaked via header %s: %q", name, v)
			}
		}
	}
}

// TestDashboardTokenEndpointPositiveControl is the other direction: with an
// ordinary token the endpoint must still work, so that a fix which simply
// disabled the endpoint cannot pass the leak tests above.
func TestDashboardTokenEndpointPositiveControl(t *testing.T) {
	const normal = "plain-operator-token-1234"

	t.Setenv("HIVE_DASHBOARD_TOKEN", normal)
	srv := newFullServer(t)
	srv.authToken = normal

	rec := httptest.NewRecorder()
	srv.handleAuthToken(rec, httptest.NewRequest(http.MethodGet, "/api/auth/token", nil))
	body := rec.Body.String()

	if rec.Code == http.StatusOK {
		if !strings.Contains(body, "configured") {
			t.Errorf("endpoint must report configured-ness, got %q", body)
		}
	} else if rec.Code != http.StatusNotFound {
		t.Fatalf("unexpected status %d body %q", rec.Code, body)
	}
	// Still must not hand back the value.
	if strings.Contains(body, normal) {
		t.Errorf("endpoint must never return the token VALUE, got %q", body)
	}

	// And the unset case must report configured=false rather than erroring,
	// proving the endpoint really is reading the token, not hard-coding a reply.
	os.Unsetenv("HIVE_DASHBOARD_TOKEN")
	srv2 := newFullServer(t)
	srv2.authToken = ""
	rec2 := httptest.NewRecorder()
	srv2.handleAuthToken(rec2, httptest.NewRequest(http.MethodGet, "/api/auth/token", nil))
	if rec2.Code == http.StatusOK && !strings.Contains(rec2.Body.String(), "false") {
		t.Errorf("with no token configured the endpoint must report false, got %q", rec2.Body.String())
	}
}
