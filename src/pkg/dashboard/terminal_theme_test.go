package dashboard

import (
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// The Sessions iframe showed ttyd's stock black document inside a themed card.
// A user-session document (first ttyd arg hive-sess-<id>) must carry the theme
// script in addition to the clipboard script.
func TestModifyTerminalResponseInjectsThemeForSessionTerminals(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/?arg=hive-sess-6772199e9bc0&arg=attach-token", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/html; charset=utf-8"}, "Etag": []string{`"ttyd"`}},
		Body:       io.NopCloser(strings.NewReader(fakeTtydDocument)),
		Request:    req,
	}
	if err := modifyTerminalResponse(resp); err != nil {
		t.Fatalf("modifyTerminalResponse: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	doc := string(body)
	for _, want := range []string{
		terminalClipboardMarker,
		terminalThemeMarker,
		"window.parent === window",
		"resolveToken('--terminal-bg')",
		"resolveToken('--terminal-fg')",
		"resolveToken('--accent')",
		"term.options.theme = theme",
		"style.backgroundColor = theme.background",
		"new MutationObserver",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("session terminal document is missing %q", want)
		}
	}
	if !strings.HasSuffix(strings.TrimSpace(doc), "</script></body></html>") {
		t.Error("theme script was not injected before the closing </body>")
	}
	if resp.Header.Get("Etag") != "" {
		t.Error("rewritten document kept the upstream Etag")
	}
}

// Agent terminals open as a top-level tab and keep ttyd's stock look: only the
// clipboard script is injected for them.
func TestTerminalProxyLeavesAgentTerminalThemeAlone(t *testing.T) {
	startFakeTtydDocument(t, fakeTtydDocument)
	h := newTerminalTestHandler(t, "")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/terminal/?arg=hive-quality", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("terminal document status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, terminalClipboardMarker) {
		t.Error("agent terminal lost the clipboard script")
	}
	if strings.Contains(body, terminalThemeMarker) {
		t.Error("agent terminal document was given the Sessions theme script")
	}
}

func TestTerminalSessionRequest(t *testing.T) {
	tests := []struct {
		url  string
		want bool
	}{
		{"/?arg=hive-sess-6772199e9bc0", true},
		{"/?arg=hive-sess-6772199e9bc0&arg=token", true},
		{"/?arg=hive-quality", false},
		{"/token?arg=hive-quality&arg=hive-sess-6772199e9bc0", false},
		{"/", false},
	}
	for _, tc := range tests {
		req := httptest.NewRequest(http.MethodGet, tc.url, nil)
		if got := terminalSessionRequest(req); got != tc.want {
			t.Errorf("terminalSessionRequest(%q) = %v, want %v", tc.url, got, tc.want)
		}
	}
	if terminalSessionRequest(nil) {
		t.Error("nil request reported as a session terminal")
	}
}

func TestInjectTerminalThemeScriptIsIdempotent(t *testing.T) {
	once := injectTerminalThemeScript([]byte(fakeTtydDocument))
	twice := injectTerminalThemeScript(once)
	if string(once) != string(twice) {
		t.Fatal("theme script was injected twice")
	}
	if n := strings.Count(string(twice), terminalThemeMarker); n != 1 {
		t.Fatalf("marker appears %d times, want 1", n)
	}
	noBody := injectTerminalThemeScript([]byte("<html>"))
	if !strings.HasSuffix(string(noBody), "</script>") {
		t.Error("document without </body> did not get the script appended")
	}
}

// Every colour the pane shows comes from the dashboard's tokens at runtime; the
// script itself must not carry a colour literal, or a theme preset could be
// overridden by whatever was baked in here.
func TestTerminalThemeScriptHasNoColorLiterals(t *testing.T) {
	hex := regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b`)
	if m := hex.FindString(terminalThemeScript); m != "" {
		t.Fatalf("theme script hard-codes colour %q; derive it from a token instead", m)
	}
	// An rgb()/rgba() literal has digits inside; the comment that names the
	// computed form and the parser regex do not.
	rgb := regexp.MustCompile(`rgba?\(\s*\d`)
	if m := rgb.FindString(terminalThemeScript); m != "" {
		t.Fatalf("theme script hard-codes an rgb() colour (%q); derive it from a token instead", m)
	}
}
