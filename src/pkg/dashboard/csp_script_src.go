package dashboard

// csp_script_src.go — the script-src half of kubestellar/hive#3848 (#3907).
//
// #3844 staged the removal of script-src 'unsafe-inline'; #3933 (ADR-0015)
// then scoped style-src into its element/attribute halves because the two have
// different futures. This file applies the SAME decomposition to script-src,
// because the same asymmetry decides it:
//
//   - Inline <script> ELEMENTS take sha256 hashes. Every inline script this
//     server sends is either embedded in the binary (static/index.html, the
//     device-flow login page) or rendered by a handler that holds the complete
//     document before writing it — so the exact allowlist is computable, at
//     startup for the static documents and per response for the rendered ones.
//     That half is CLOSED here: script-src-elem carries hashes and no
//     'unsafe-inline'. Hashes — not nonces — deliberately: the SPA document is
//     pre-gzipped once at startup with a strong ETag (#3863, static_index.go),
//     and a per-response nonce would force a re-render + re-compress per
//     request and defeat the ETag entirely. Hashes are a pure function of the
//     bytes already being served, so the #3863 caching design is untouched.
//
//   - Inline on*= EVENT-HANDLER attributes have no nonce and no hash form
//     under script-src-elem semantics; only script-src-attr 'unsafe-hashes'
//     could pin them, at a hash per distinct attribute value — rejected for
//     the same reason ADR-0015 rejected it for styles. That half is now
//     CLOSED: the #3848 event-delegation refactor replaced every inline
//     handler attribute with data-action dispatch, so script-src-attr is
//     'none'. See ADR-0016 and TestCSPScriptSrcAttrUnsafeInlineIsAbsent.
//
//   - script-src itself is 'self' as the CSP2 fallback ('unsafe-inline'
//     dropped with #3848), and
//     MUST NOT carry the hashes: per CSP2 §7.15, the presence of a hash makes
//     a browser ignore 'unsafe-inline' in the same directive, so a browser
//     that understands hashes but not script-src-elem/-attr (Firefox < 108)
//     would block every inline handler and blank the dashboard. Browsers with
//     CSP3 use the two explicit halves and never consult the fallback.
//
// Net effect: in every current browser an injected inline <script> no longer
// executes (it cannot match a hash), while the served UI keeps working
// everywhere. Injected on*= attributes remain executable until the delegation
// refactor lands — stated, not hidden; see ADR-0016 "Residual risk".

import (
	"crypto/sha256"
	"encoding/base64"
	"io/fs"
	"net/http"
	"regexp"
	"strings"
	"sync"
)

// inlineScriptRe matches a <script ...>…</script> element and captures its
// attribute list and its raw text content. (?s) lets the content span lines;
// (?i) matches the tag case-insensitively, as the HTML tokenizer does. The
// non-greedy body stops at the first </script>, which is exactly where the
// HTML tokenizer ends the element regardless of JS syntax — jsStringLiteral
// guarantees no served script carries a raw "</script" inside its content
// (TestJSStringLiteralEscapesScriptContext), so regex and browser agree on
// every document this server produces.
var inlineScriptRe = regexp.MustCompile(`(?is)<script\b([^>]*)>(.*?)</script>`)

// scriptSrcAttrRe detects a src= attribute on a <script> tag: external scripts
// are covered by URL sources ('self', pinned CDNs), not hashes.
var scriptSrcAttrRe = regexp.MustCompile(`(?i)(^|\s)src\s*=`)

// extractInlineScripts returns the raw text content of every inline <script>
// element in doc, in document order. Scripts with a src= attribute are
// skipped; everything else — including non-executable data blocks — is
// included, because hashing our own known content is harmless and keeps the
// extraction rule simple enough to audit.
func extractInlineScripts(doc []byte) [][]byte {
	var scripts [][]byte
	for _, m := range inlineScriptRe.FindAllSubmatch(doc, -1) {
		if scriptSrcAttrRe.Match(m[1]) {
			continue
		}
		scripts = append(scripts, m[2])
	}
	return scripts
}

// cspScriptHash renders content as a CSP hash-source token. The digest is over
// the element's exact text bytes (WHATWG CSP §8.2 "is element's inline
// behavior allowed"), base64 standard encoding with padding.
func cspScriptHash(content []byte) string {
	sum := sha256.Sum256(content)
	return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
}

// scriptSrcElemSources builds the source list for script-src-elem covering
// every inline script in doc: 'self' plus one hash per distinct script.
func scriptSrcElemSources(doc []byte) string {
	sources := []string{"'self'"}
	seen := map[string]bool{}
	for _, script := range extractInlineScripts(doc) {
		h := cspScriptHash(script)
		if !seen[h] {
			seen[h] = true
			sources = append(sources, h)
		}
	}
	return strings.Join(sources, " ")
}

var (
	baseScriptSrcElemOnce    sync.Once
	baseScriptSrcElemSources string
)

// baseScriptSrcElem returns the startup-computed script-src-elem source list
// covering the two documents whose bytes are fixed for the life of the
// process: the embedded SPA (static/index.html, served verbatim by both
// static_index.go and the plain file server) and the device-flow login page
// (a const, served to any unauthenticated browser path). Computed once, like
// the #3863 gzip/ETag precomputation it must stay compatible with.
func baseScriptSrcElem() string {
	baseScriptSrcElemOnce.Do(func() {
		var docs []byte
		if raw, err := fs.ReadFile(staticFS, "static/index.html"); err == nil {
			docs = append(docs, raw...)
		}
		docs = append(docs, []byte(loginPage)...)
		baseScriptSrcElemSources = scriptSrcElemSources(docs)
	})
	return baseScriptSrcElemSources
}

// applyDocumentScriptSrcElem replaces the script-src-elem directive of the
// already-set CSP header with the hash allowlist for doc — the complete
// rendered document the handler is about to write. Handlers whose HTML varies
// per response (the /contribute landing, the /snapshot document) call this
// after rendering and before the first Write, so the policy always describes
// exactly the bytes being served. A response with no CSP header (not routed
// through securityHeaders, e.g. a bare test recorder) is left untouched.
func applyDocumentScriptSrcElem(w http.ResponseWriter, doc []byte) {
	csp := w.Header().Get("Content-Security-Policy")
	if csp == "" {
		return
	}
	parts := strings.Split(csp, ";")
	for i, part := range parts {
		if strings.HasPrefix(strings.TrimSpace(part), "script-src-elem") {
			parts[i] = " script-src-elem " + scriptSrcElemSources(doc)
			w.Header().Set("Content-Security-Policy", strings.TrimSpace(strings.Join(parts, ";")))
			return
		}
	}
}
