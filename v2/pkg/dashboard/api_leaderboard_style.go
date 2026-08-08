package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	customStyleMaxBytes   = 128 * 1024
	customStyleCacheTTL   = 5 * time.Minute
	customStyleMaxCache   = 64
	customStyleFetchTTL   = 10 * time.Second
	customStyleDefaultRef = "HEAD"

	customStyleScopeDashboard   = "dashboard"
	customStyleScopeLeaderboard = "leaderboard"
)

var customStyleRawBaseURL = "https://raw.githubusercontent.com"

var customStyleAllowedScopes = map[string]string{
	customStyleScopeDashboard:   "#hive-dashboard-root",
	customStyleScopeLeaderboard: "#tab-leaderboard",
}

type customStyleSource struct {
	Owner string
	Repo  string
	Path  string
	Ref   string
}

type customStyleCacheEntry struct {
	css       []byte
	dropped   []droppedCSSRule
	expiresAt time.Time
}

// droppedCSSRule records something the sanitizer removed from an author's
// stylesheet, together with a human-readable reason. Surfacing these is the
// whole point of issue #2972: before, a rejected rule was simply absent and the
// theme silently rendered as a no-op with no indication why.
type droppedCSSRule struct {
	// Rule is a short excerpt (selector, declaration, or at-rule prelude) that
	// was removed, trimmed to droppedRuleExcerptMax runes so a hostile or huge
	// stylesheet cannot blow up the response header / notice.
	Rule string `json:"rule"`
	// Reason is a stable, human-readable explanation of why it was removed.
	Reason string `json:"reason"`
}

const (
	// droppedRuleExcerptMax caps how much of a rejected rule we echo back so a
	// large or malicious stylesheet cannot bloat the response header or notice.
	droppedRuleExcerptMax = 120
	// maxDroppedRulesReported caps how many dropped entries we retain so a
	// stylesheet full of rejects cannot produce an unbounded list.
	maxDroppedRulesReported = 64
	// maxAtRuleNestDepth bounds how deep we recurse into nested at-rules so a
	// pathological stylesheet cannot exhaust the stack.
	maxAtRuleNestDepth = 8
)

var (
	customStyleCacheMu sync.Mutex
	customStyleCache   = map[string]customStyleCacheEntry{}
	customStyleClient  = &http.Client{Timeout: customStyleFetchTTL}
)

var (
	githubOwnerNameRE = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)
	githubRepoNameRE  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
	githubRefNameRE   = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,100}$`)
	githubCSSPathRE   = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)
	cssImportRE       = regexp.MustCompile(`(?is)@import\b[^;]*(;|$)`)
	cssURLRE          = regexp.MustCompile(`(?is)url\(\s*(['"]?)([^'")]+)['"]?\s*\)`)
	// cssAtRuleNameRE captures the at-rule identifier from a prelude, e.g.
	// "@media (min-width: 600px)" -> "media", "@-webkit-keyframes spin" ->
	// "-webkit-keyframes".
	cssAtRuleNameRE = regexp.MustCompile(`(?i)^@(-[a-z]+-)?([a-z-]+)`)
)

// safeNestedAtRules are the at-rules whose *inner* content is a normal list of
// scoped style rules (selector { declarations }). We recurse the sanitizer into
// their bodies so nested declarations get the same value checks and scoping.
//
// Deliberately excluded (see PR body for the security reasoning):
//   - @import      — network fetch of arbitrary CSS: an exfil / injection vector.
//     Stripped earlier by cssImportRE; also rejected here for defence in depth.
//   - @font-face   — its src: descriptor fetches a font over the network, which
//     is a request-based exfil channel we cannot constrain to "no network" while
//     still being useful, so we reject it rather than ship a half-safe allow.
//   - @charset / @namespace / @page / @layer etc. — not needed by themes and
//     easier to reason about when rejected than when partially allowed.
var safeNestedAtRules = map[string]bool{
	"media":    true,
	"supports": true,
}

// safeKeyframesAtRules are at-rules whose inner blocks are keyframe selectors
// (from/to/percentages) rather than scoped selectors, so their bodies are kept
// verbatim after value-sanitization but are NOT scope-prefixed.
var safeKeyframesAtRules = map[string]bool{
	"keyframes": true,
}

func validateCustomStyleSource(src string) (customStyleSource, error) {
	src = strings.TrimSpace(src)
	if src == "" {
		return customStyleSource{}, fmt.Errorf("style source is required")
	}
	if strings.Contains(src, "://") || strings.HasPrefix(src, "//") {
		return customStyleSource{}, fmt.Errorf("use owner/repo/path.css, not a full URL")
	}
	ref := customStyleDefaultRef
	if at := strings.LastIndex(src, "@"); at >= 0 {
		if at == len(src)-1 {
			return customStyleSource{}, fmt.Errorf("ref is empty")
		}
		ref = src[at+1:]
		src = src[:at]
	}
	parts := strings.Split(src, "/")
	if len(parts) < 3 {
		return customStyleSource{}, fmt.Errorf("style source must be owner/repo/path.css")
	}
	owner, repo := parts[0], parts[1]
	cssPath := strings.Join(parts[2:], "/")
	if !githubOwnerNameRE.MatchString(owner) {
		return customStyleSource{}, fmt.Errorf("invalid owner")
	}
	if !githubRepoNameRE.MatchString(repo) || repo == "." || repo == ".." {
		return customStyleSource{}, fmt.Errorf("invalid repo")
	}
	if err := validateCustomStylePath(cssPath); err != nil {
		return customStyleSource{}, err
	}
	if ref != customStyleDefaultRef {
		if !githubRefNameRE.MatchString(ref) || strings.Contains(ref, "..") || strings.HasPrefix(ref, "/") || strings.HasSuffix(ref, "/") {
			return customStyleSource{}, fmt.Errorf("invalid ref")
		}
	}
	return customStyleSource{Owner: owner, Repo: repo, Path: cssPath, Ref: ref}, nil
}

func validateCustomStylePath(cssPath string) error {
	if cssPath == "" || strings.HasPrefix(cssPath, "/") || strings.Contains(cssPath, "\\") || strings.Contains(cssPath, "://") {
		return fmt.Errorf("invalid path")
	}
	if !githubCSSPathRE.MatchString(cssPath) {
		return fmt.Errorf("path contains unsupported characters")
	}
	if !strings.HasSuffix(strings.ToLower(cssPath), ".css") {
		return fmt.Errorf("path must end in .css")
	}
	for _, segment := range strings.Split(cssPath, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return fmt.Errorf("path traversal is not allowed")
		}
	}
	return nil
}

func customStyleCacheKey(src customStyleSource, scope string) string {
	return src.Owner + "/" + src.Repo + "/" + src.Path + "@" + src.Ref + "|" + scope
}

func customStyleSourceKey(src customStyleSource) string {
	return src.Owner + "/" + src.Repo + "/" + src.Path + "@" + src.Ref
}

func customStyleRawURL(src customStyleSource) string {
	parts := []string{strings.TrimRight(customStyleRawBaseURL, "/"), pathEscape(src.Owner), pathEscape(src.Repo), pathEscape(src.Ref)}
	for _, segment := range strings.Split(src.Path, "/") {
		parts = append(parts, pathEscape(segment))
	}
	return strings.Join(parts, "/")
}

func pathEscape(s string) string {
	return strings.ReplaceAll(url.PathEscape(s), "%2F", "/")
}

func normalizeCustomStyleScope(scope string) (string, string, error) {
	scope = strings.TrimSpace(scope)
	if scope == "" {
		scope = customStyleScopeDashboard
	}
	root, ok := customStyleAllowedScopes[scope]
	if !ok {
		return "", "", fmt.Errorf("invalid style scope")
	}
	return scope, root, nil
}

func getCustomStyle(ctx context.Context, rawSrc, rawScope string) ([]byte, customStyleSource, error) {
	css, src, _, err := getCustomStyleWithDropped(ctx, rawSrc, rawScope)
	return css, src, err
}

// getCustomStyleWithDropped is getCustomStyle plus the list of rules the
// sanitizer removed, so callers (the /api/.../style handler and the /contribute
// landing) can report them instead of silently dropping — see #2972.
func getCustomStyleWithDropped(ctx context.Context, rawSrc, rawScope string) ([]byte, customStyleSource, []droppedCSSRule, error) {
	scope, root, err := normalizeCustomStyleScope(rawScope)
	if err != nil {
		return nil, customStyleSource{}, nil, err
	}
	src, err := validateCustomStyleSource(rawSrc)
	if err != nil {
		return nil, customStyleSource{}, nil, err
	}
	key := customStyleCacheKey(src, scope)
	now := time.Now()
	customStyleCacheMu.Lock()
	if entry, ok := customStyleCache[key]; ok && now.Before(entry.expiresAt) {
		css := append([]byte(nil), entry.css...)
		dropped := append([]droppedCSSRule(nil), entry.dropped...)
		customStyleCacheMu.Unlock()
		return css, src, dropped, nil
	}
	customStyleCacheMu.Unlock()

	css, dropped, err := fetchAndSanitizeCustomStyle(ctx, src, root)
	if err != nil {
		return nil, src, nil, err
	}
	customStyleCacheMu.Lock()
	if len(customStyleCache) >= customStyleMaxCache {
		for k := range customStyleCache {
			delete(customStyleCache, k)
			break
		}
	}
	customStyleCache[key] = customStyleCacheEntry{
		css:       append([]byte(nil), css...),
		dropped:   append([]droppedCSSRule(nil), dropped...),
		expiresAt: now.Add(customStyleCacheTTL),
	}
	customStyleCacheMu.Unlock()
	return css, src, dropped, nil
}

func fetchAndSanitizeCustomStyle(ctx context.Context, src customStyleSource, scopeRoot string) ([]byte, []droppedCSSRule, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, customStyleRawURL(src), nil)
	if err != nil {
		return nil, nil, err
	}
	resp, err := customStyleClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil, errCustomStyleNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, nil, fmt.Errorf("style fetch failed")
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if ct != "" && !strings.Contains(ct, "text/css") && !strings.Contains(ct, "text/plain") {
		return nil, nil, fmt.Errorf("style response is not CSS")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, customStyleMaxBytes+1))
	if err != nil {
		return nil, nil, err
	}
	return sanitizeCustomStyleWithDropped(body, scopeRoot)
}

var errCustomStyleNotFound = fmt.Errorf("style not found")

// sanitizeCustomStyle is the back-compatible entry point kept for callers/tests
// that do not need the dropped-rule report.
func sanitizeCustomStyle(css []byte, scopeRoot string) ([]byte, error) {
	out, _, err := sanitizeCustomStyleWithDropped(css, scopeRoot)
	return out, err
}

// sanitizeCustomStyleWithDropped rewrites an author's stylesheet into the safe,
// scoped subset the leaderboard/dashboard will accept, and returns the list of
// rules it removed (with reasons) so the caller can surface them (#2972).
//
// The parser walks balanced { } blocks so it can recurse into at-rules; the old
// implementation was string/line based and (a) discarded every at-rule and
// (b) desynced on the first nested brace, corrupting everything after an
// @media block.
func sanitizeCustomStyleWithDropped(css []byte, scopeRoot string) ([]byte, []droppedCSSRule, error) {
	if len(css) > customStyleMaxBytes {
		return nil, nil, fmt.Errorf("style exceeds %d byte limit", customStyleMaxBytes)
	}
	// Strip comments first so the block parser never sees braces or "@import"
	// hidden inside them, and so scoping is never applied to comment text (the
	// old string-based prefixer injected the scope inside comments — #2972).
	s := stripCSSComments(string(css))
	// @import is a network fetch / injection vector; remove any occurrence up
	// front (belt and braces — the at-rule allowlist below also rejects it).
	s = cssImportRE.ReplaceAllString(s, "")
	// Neutralise unsafe url()/image-set() targets inside declaration values.
	s = cssURLRE.ReplaceAllStringFunc(s, sanitizeCSSURLToken)

	drops := &droppedCSSCollector{}
	out := sanitizeCustomStyleBlocks(s, scopeRoot, false, 0, drops)
	return []byte(out), drops.rules, nil
}

// droppedCSSCollector accumulates removed rules, de-duplicated and capped.
type droppedCSSCollector struct {
	rules []droppedCSSRule
	seen  map[string]bool
}

func (d *droppedCSSCollector) add(rule, reason string) {
	if len(d.rules) >= maxDroppedRulesReported {
		return
	}
	rule = truncateForReport(collapseWhitespace(rule))
	if rule == "" {
		return
	}
	key := rule + "\x00" + reason
	if d.seen == nil {
		d.seen = map[string]bool{}
	}
	if d.seen[key] {
		return
	}
	d.seen[key] = true
	d.rules = append(d.rules, droppedCSSRule{Rule: rule, Reason: reason})
}

// sanitizeCustomStyleBlocks walks css as a sequence of "prelude { body }" rules
// plus any trailing declaration text. inKeyframes selects keyframe-selector
// handling (from/to/percent, no scoping) instead of normal scoped selectors.
func sanitizeCustomStyleBlocks(css, scopeRoot string, inKeyframes bool, depth int, drops *droppedCSSCollector) string {
	var out strings.Builder
	remaining := css
	for {
		open := strings.Index(remaining, "{")
		if open < 0 {
			break
		}
		prelude := strings.TrimSpace(remaining[:open])
		afterOpen := remaining[open+1:]
		bodyEnd := matchingBrace(afterOpen)
		if bodyEnd < 0 {
			// Unbalanced — drop the rest rather than emit a truncated block.
			if prelude != "" {
				drops.add(prelude, "unbalanced braces")
			}
			break
		}
		body := afterOpen[:bodyEnd]
		remaining = afterOpen[bodyEnd+1:]

		if prelude == "" {
			drops.add(body, "block without a selector")
			continue
		}

		if strings.HasPrefix(prelude, "@") {
			out.WriteString(sanitizeAtRule(prelude, body, scopeRoot, depth, drops))
			continue
		}

		if inKeyframes {
			// Keyframe stop: selector is from/to/percentage; keep verbatim but
			// still value-sanitize declarations. Not scope-prefixed.
			sel := sanitizeKeyframeSelector(prelude)
			if sel == "" {
				drops.add(prelude, "invalid keyframe selector")
				continue
			}
			decls := sanitizeDeclarations(body, prelude, drops)
			if strings.TrimSpace(decls) == "" {
				continue
			}
			out.WriteString(sel)
			out.WriteString("{")
			out.WriteString(decls)
			out.WriteString("}\n")
			continue
		}

		scoped := scopeCustomStyleSelectors(prelude, scopeRoot, drops)
		if scoped == "" {
			continue
		}
		decls := sanitizeDeclarations(body, prelude, drops)
		if strings.TrimSpace(decls) == "" {
			continue
		}
		out.WriteString(scoped)
		out.WriteString("{")
		out.WriteString(decls)
		out.WriteString("}\n")
	}
	return out.String()
}

// sanitizeAtRule handles a single "@name prelude { body }" rule. Only the safe
// allowlist survives; everything else is dropped with a reason.
func sanitizeAtRule(prelude, body, scopeRoot string, depth int, drops *droppedCSSCollector) string {
	name := ""
	if m := cssAtRuleNameRE.FindStringSubmatch(prelude); m != nil {
		name = strings.ToLower(m[2])
	}
	if depth >= maxAtRuleNestDepth {
		drops.add("@"+name, "at-rules nested too deeply")
		return ""
	}
	switch {
	case safeNestedAtRules[name]:
		// @media / @supports: recurse into the inner scoped rules.
		inner := sanitizeCustomStyleBlocks(body, scopeRoot, false, depth+1, drops)
		if strings.TrimSpace(inner) == "" {
			return ""
		}
		return prelude + "{\n" + inner + "}\n"
	case safeKeyframesAtRules[name]:
		// @keyframes / @-webkit-keyframes: recurse with keyframe handling.
		inner := sanitizeCustomStyleBlocks(body, scopeRoot, true, depth+1, drops)
		if strings.TrimSpace(inner) == "" {
			return ""
		}
		return prelude + "{\n" + inner + "}\n"
	default:
		drops.add(prelude, "at-rule not allowed (only @media, @supports, @keyframes)")
		return ""
	}
}

// sanitizeDeclarations validates each "prop: value" declaration in a block. A
// single bad declaration is dropped (with a reason referencing its rule) but the
// rest of the block survives — the old code dropped the whole rule if any line
// was bad, which is what made themes silently inert (#2972).
func sanitizeDeclarations(body, ruleContext string, drops *droppedCSSCollector) string {
	var kept []string
	droppedAny := false
	for _, decl := range strings.Split(body, ";") {
		trimmed := strings.TrimSpace(decl)
		if trimmed == "" {
			continue
		}
		if reason := declarationDropReason(trimmed); reason != "" {
			drops.add(shortRule(ruleContext)+" { "+trimmed+" }", reason)
			droppedAny = true
			continue
		}
		kept = append(kept, decl)
	}
	if len(kept) == 0 {
		return ""
	}
	// If we dropped nothing, emit the body verbatim so output stays byte-faithful
	// to the author's source (whitespace, trailing-semicolon style, and so on).
	// Only when a declaration was removed do we re-serialize the survivors.
	if !droppedAny {
		return body
	}
	out := make([]string, 0, len(kept))
	for _, d := range kept {
		out = append(out, strings.TrimSpace(d))
	}
	return strings.Join(out, ";") + ";"
}

// declarationDropReason returns a non-empty reason if a single declaration must
// be removed for safety, or "" if it is allowed.
func declarationDropReason(trimmed string) string {
	lower := strings.ToLower(trimmed)
	switch {
	case strings.Contains(lower, "expression("):
		return "expression() can execute script"
	case strings.Contains(lower, "-moz-binding"):
		return "-moz-binding can bind script"
	case strings.HasPrefix(lower, "behavior:") || strings.Contains(lower, " behavior:"):
		return "behavior: can execute an HTC script"
	case strings.Contains(lower, "image-set("):
		return "image-set() can fetch a remote resource (exfil)"
	case strings.Contains(lower, "://"):
		return "remote URL not allowed (exfil)"
	case strings.Contains(trimmed, `\`):
		return "escape sequences not allowed (can smuggle blocked tokens)"
	}
	return ""
}

// scopeCustomStyleSelectors prefixes each comma-separated selector with the
// scope root. Selectors that would let a theme escape its scope (e.g. an
// ancestor selector reaching <html>) are dropped with a reason so the author
// learns why, rather than getting a silent no-op (#2972).
func scopeCustomStyleSelectors(selector, scopeRoot string, drops *droppedCSSCollector) string {
	parts := strings.Split(selector, ",")
	scoped := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		isDashboard := scopeRoot == customStyleAllowedScopes[customStyleScopeDashboard]
		// An author selector that already qualifies the scope root as a compound
		// (#root.foo / #root:hover / #root[attr]) is kept as-is. We deliberately
		// do NOT treat "#root <combinator>…" (e.g. "#root ~ footer") as already
		// scoped for the leaderboard scope: a sibling/child combinator right off
		// the root can reach elements OUTSIDE the widget, so it must be prefixed
		// again to stay contained. For the dashboard scope the historical
		// behaviour also honoured a descendant space, so preserve that.
		if part == scopeRoot || strings.HasPrefix(part, scopeRoot+".") || strings.HasPrefix(part, scopeRoot+":") || strings.HasPrefix(part, scopeRoot+"[") ||
			(isDashboard && strings.HasPrefix(part, scopeRoot+" ")) {
			scoped = append(scoped, part)
			continue
		}
		if scopeRoot == customStyleAllowedScopes[customStyleScopeDashboard] {
			if part == "body" || part == "html" || part == ":root" {
				scoped = append(scoped, scopeRoot)
				continue
			}
			matchedRoot := false
			for _, rootSelector := range []string{"body", "html", ":root"} {
				if strings.HasPrefix(part, rootSelector+".") || strings.HasPrefix(part, rootSelector+":") || strings.HasPrefix(part, rootSelector+"#") {
					part = scopeRoot + strings.TrimPrefix(part, rootSelector)
					scoped = append(scoped, part)
					matchedRoot = true
					break
				}
			}
			if matchedRoot {
				continue
			}
		}
		scoped = append(scoped, scopeRoot+" "+part)
	}
	return strings.Join(scoped, ", ")
}

// sanitizeKeyframeSelector accepts only keyframe stop selectors: from, to, or a
// comma list of percentages. Anything else is rejected.
func sanitizeKeyframeSelector(sel string) string {
	parts := strings.Split(sel, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(strings.ToLower(p))
		if p == "from" || p == "to" {
			out = append(out, p)
			continue
		}
		if strings.HasSuffix(p, "%") && keyframePercentRE.MatchString(p) {
			out = append(out, p)
			continue
		}
		return ""
	}
	return strings.Join(out, ",")
}

var keyframePercentRE = regexp.MustCompile(`^\d{1,3}(\.\d+)?%$`)

// matchingBrace returns the index of the "}" that closes the block whose "{"
// has already been consumed, accounting for nesting. -1 if unbalanced.
func matchingBrace(s string) int {
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			if depth == 0 {
				return i
			}
			depth--
		}
	}
	return -1
}

// stripCSSComments removes /* ... */ comments. Unterminated comments consume to
// end-of-input (matching browser behaviour).
func stripCSSComments(s string) string {
	var out strings.Builder
	for {
		start := strings.Index(s, "/*")
		if start < 0 {
			out.WriteString(s)
			break
		}
		out.WriteString(s[:start])
		end := strings.Index(s[start+2:], "*/")
		if end < 0 {
			break
		}
		s = s[start+2+end+2:]
	}
	return out.String()
}

func collapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func truncateForReport(s string) string {
	r := []rune(s)
	if len(r) <= droppedRuleExcerptMax {
		return s
	}
	return string(r[:droppedRuleExcerptMax-1]) + "…"
}

// shortRule returns a compact selector excerpt for use inside a dropped-reason.
func shortRule(s string) string {
	s = collapseWhitespace(s)
	if len([]rune(s)) > 48 {
		return string([]rune(s)[:47]) + "…"
	}
	return s
}

func sanitizeCSSURLToken(token string) string {
	matches := cssURLRE.FindStringSubmatch(token)
	if len(matches) < 3 {
		return ""
	}
	raw := strings.TrimSpace(matches[2])
	raw = strings.Trim(raw, `"'`)
	lower := strings.ToLower(raw)
	if strings.HasPrefix(lower, "data:image/") {
		return "url(" + raw + ")"
	}
	if colon := strings.Index(raw, ":"); colon >= 0 {
		slash := strings.IndexAny(raw, "/?#")
		if slash == -1 || colon < slash {
			return "url(\"\")"
		}
	}
	if strings.HasPrefix(raw, "//") || strings.Contains(lower, "://") || strings.HasPrefix(lower, "data:") {
		return "url(\"\")"
	}
	return "url(" + raw + ")"
}

func (s *Server) handleStyle(w http.ResponseWriter, r *http.Request) {
	css, _, dropped, err := getCustomStyleWithDropped(r.Context(), r.URL.Query().Get("src"), r.URL.Query().Get("scope"))
	if err != nil {
		status := http.StatusUnprocessableEntity
		if err == errCustomStyleNotFound {
			status = http.StatusNotFound
		}
		http.Error(w, err.Error(), status)
		return
	}
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	// Anti-silent-drop (#2972): tell the caller what was removed and why.
	// X-Style-Rules-Dropped is a count so a curl author sees "loaded, N removed"
	// at a glance; X-Style-Rules-Dropped-Detail carries the JSON list.
	setDroppedStyleHeaders(w.Header(), dropped)
	_, _ = w.Write(css)
}

// setDroppedStyleHeaders writes the dropped-rule report onto response headers.
// The detail header is JSON so tooling can parse it; header values must stay on
// one line, so the JSON is compact and already length-capped upstream.
func setDroppedStyleHeaders(h http.Header, dropped []droppedCSSRule) {
	h.Set("X-Style-Rules-Dropped", fmt.Sprintf("%d", len(dropped)))
	if len(dropped) == 0 {
		return
	}
	if detail, err := json.Marshal(dropped); err == nil {
		// Strip any stray CR/LF defensively so the excerpt cannot inject a header.
		clean := strings.NewReplacer("\r", " ", "\n", " ").Replace(string(detail))
		h.Set("X-Style-Rules-Dropped-Detail", clean)
	}
}

func (s *Server) handleLeaderboardStyle(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	q.Set("scope", customStyleScopeLeaderboard)
	r.URL.RawQuery = q.Encode()
	s.handleStyle(w, r)
}

// Back-compatible leaderboard names kept for the PR #2834 tests and callers.
type leaderboardCustomStyleSource = customStyleSource
type leaderboardCustomStyleCacheEntry = customStyleCacheEntry

const (
	leaderboardCustomStyleMaxBytes   = customStyleMaxBytes
	leaderboardCustomStyleDefaultRef = customStyleDefaultRef
)

func validateLeaderboardCustomStyleSource(src string) (leaderboardCustomStyleSource, error) {
	return validateCustomStyleSource(src)
}

func leaderboardCustomStyleCacheKey(src leaderboardCustomStyleSource) string {
	return customStyleSourceKey(src)
}

func getLeaderboardCustomStyle(ctx context.Context, rawSrc string) ([]byte, leaderboardCustomStyleSource, error) {
	return getCustomStyle(ctx, rawSrc, customStyleScopeLeaderboard)
}

func getLeaderboardCustomStyleWithDropped(ctx context.Context, rawSrc string) ([]byte, leaderboardCustomStyleSource, []droppedCSSRule, error) {
	return getCustomStyleWithDropped(ctx, rawSrc, customStyleScopeLeaderboard)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// droppedRulesHTML renders the dropped-rule report as an escaped <ul> for the
// contribute notice, capped so a stylesheet full of rejects cannot bloat the
// page. Both the rule excerpt and reason are HTML-escaped.
func droppedRulesHTML(dropped []droppedCSSRule) string {
	const maxShown = 12
	var b strings.Builder
	b.WriteString(`<ul class="lb-custom-style-dropped">`)
	shown := dropped
	if len(shown) > maxShown {
		shown = shown[:maxShown]
	}
	for _, d := range shown {
		b.WriteString("<li><code>")
		b.WriteString(html.EscapeString(d.Rule))
		b.WriteString("</code> — ")
		b.WriteString(html.EscapeString(d.Reason))
		b.WriteString("</li>")
	}
	if len(dropped) > maxShown {
		b.WriteString(fmt.Sprintf("<li>…and %d more</li>", len(dropped)-maxShown))
	}
	b.WriteString("</ul>")
	return b.String()
}

func sanitizeLeaderboardCustomStyle(css []byte) ([]byte, error) {
	return sanitizeCustomStyle(css, customStyleAllowedScopes[customStyleScopeLeaderboard])
}

func sanitizeLeaderboardCustomStyleWithDropped(css []byte) ([]byte, []droppedCSSRule, error) {
	return sanitizeCustomStyleWithDropped(css, customStyleAllowedScopes[customStyleScopeLeaderboard])
}
