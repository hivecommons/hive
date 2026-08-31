package dashboard

import (
	"regexp"
	"strings"
	"testing"
)

// These tests pin the structure of the kick error path in the embedded
// static/index.html (#5301). The failure they guard against is not reachable
// from a Go handler test: handleKick answers JSON on every path it controls,
// so the HTML body that breaks the UI is always injected by an intermediary —
// an auth proxy answering a session-expiry redirect, or an ingress answering a
// 502/504. What these tests honestly guarantee is that the guard and both of
// its call sites survive future edits to the file; the rendered toast text is
// browser behavior and stays manual-verify.

// TestPostJSONHelperGuardsNonJSONResponses asserts the shared helper exists and
// still sniffs the content type before parsing. Losing the sniff regresses
// #5301: res.json() throws on an HTML body and the caller reports the parser's
// "Unexpected token '<'" instead of the actual HTTP failure.
func TestPostJSONHelperGuardsNonJSONResponses(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		"async function postJSON(url, opts)",
		"const contentType = res.headers.get('content-type') || '';",
		"if (!contentType.includes('application/json')) {",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing %q — postJSON lost its non-JSON guard", snippet)
		}
	}
}

// TestPostJSONReportsStatusAndSessionExpiry asserts the two things the guard
// must tell the operator apart: a 401/403 means sign in again, and any other
// non-JSON answer must name its status. Collapsing them back into one generic
// message is the exact ambiguity #5301 was filed about.
func TestPostJSONReportsStatusAndSessionExpiry(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		"res.status === 401 || res.status === 403",
		"'session expired — reload and sign in again'",
		"`server returned ${res.status} ${res.statusText || 'error'}`",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing %q — the non-JSON path stopped reporting the real HTTP condition", snippet)
		}
	}
}

// TestKickCallSitesUsePostJSON asserts both kick paths route through the helper
// rather than parsing the response themselves: the overview-card sender
// (ocSendKick) and the agent-detail sender (kick). A site that regrows its own
// res.json() is a site that regrows the misleading toast.
func TestKickCallSitesUsePostJSON(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		"const data = await postJSON('/api/kick/' + name, opts);",
		"const data = await postJSON(`/api/kick/${agent}`, opts);",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing %q — a kick call site bypasses postJSON", snippet)
		}
	}
	// No kick SENDER may keep a raw fetch of the endpoint alongside the helper.
	//
	// The read-only outcome poll added for #5325 legitimately fetches
	// /api/kick/{agent}/status directly: it is a GET, it tolerates a non-JSON
	// answer by simply polling again (a transient proxy hiccup says nothing
	// about the kick), and throwing there would reintroduce the very thing that
	// issue is about — reporting an indeterminate kick as failed. So the guard
	// targets the POST path specifically rather than the whole prefix.
	// Match any raw fetch of /api/kick/... whose argument list continues past
	// the URL — i.e. one that passes options, which is how a POST is spelled.
	// A bare one-argument fetch is a GET and is allowed (the status poll).
	rawPost := regexp.MustCompile(`fetch\([` + "`" + `'"]/api/kick/[^)]*,`)
	if m := rawPost.FindString(html); m != "" {
		t.Errorf("a kick sender fetches /api/kick directly (%q); it must go through postJSON", m)
	}
}
