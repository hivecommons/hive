package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
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
	report    customStyleSanitizeReport
	expiresAt time.Time
}

type customStyleSanitizeReport struct {
	Dropped int      `json:"dropped"`
	Reasons []string `json:"reasons,omitempty"`
}

var (
	customStyleCacheMu sync.Mutex
	customStyleCache   = map[string]customStyleCacheEntry{}
	// customStyleClient serves the unauthenticated /api/style endpoint. The
	// fetch URL is always constructed against raw.githubusercontent.com, but
	// noRedirectToPrivate is applied like every other outbound fetch so a
	// redirect (or DNS rebinding) can never walk the request to a
	// private/internal address (SSRF defence-in-depth, #3759).
	customStyleClient = &http.Client{
		Timeout:       customStyleFetchTTL,
		CheckRedirect: noRedirectToPrivate,
	}
)

var (
	githubOwnerNameRE = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)
	githubRepoNameRE  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
	githubRefNameRE   = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,100}$`)
	githubCSSPathRE   = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)
	cssURLRE          = regexp.MustCompile(`(?is)url\(\s*(['"]?)([^'")]+)['"]?\s*\)`)
)

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

func getCustomStyle(ctx context.Context, rawSrc, rawScope string) ([]byte, customStyleSource, customStyleSanitizeReport, error) {
	scope, root, err := normalizeCustomStyleScope(rawScope)
	if err != nil {
		return nil, customStyleSource{}, customStyleSanitizeReport{}, err
	}
	src, err := validateCustomStyleSource(rawSrc)
	if err != nil {
		return nil, customStyleSource{}, customStyleSanitizeReport{}, err
	}
	key := customStyleCacheKey(src, scope)
	now := time.Now()
	customStyleCacheMu.Lock()
	if entry, ok := customStyleCache[key]; ok && now.Before(entry.expiresAt) {
		css := append([]byte(nil), entry.css...)
		report := entry.report.clone()
		customStyleCacheMu.Unlock()
		return css, src, report, nil
	}
	customStyleCacheMu.Unlock()

	css, report, err := fetchAndSanitizeCustomStyle(ctx, src, root)
	if err != nil {
		return nil, src, report, err
	}
	customStyleCacheMu.Lock()
	if len(customStyleCache) >= customStyleMaxCache {
		for k := range customStyleCache {
			delete(customStyleCache, k)
			break
		}
	}
	customStyleCache[key] = customStyleCacheEntry{css: append([]byte(nil), css...), report: report.clone(), expiresAt: now.Add(customStyleCacheTTL)}
	customStyleCacheMu.Unlock()
	return css, src, report, nil
}

func fetchAndSanitizeCustomStyle(ctx context.Context, src customStyleSource, scopeRoot string) ([]byte, customStyleSanitizeReport, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, customStyleRawURL(src), nil)
	if err != nil {
		return nil, customStyleSanitizeReport{}, err
	}
	resp, err := customStyleClient.Do(req)
	if err != nil {
		return nil, customStyleSanitizeReport{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, customStyleSanitizeReport{}, errCustomStyleNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, customStyleSanitizeReport{}, fmt.Errorf("style fetch failed")
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if ct != "" && !strings.Contains(ct, "text/css") && !strings.Contains(ct, "text/plain") {
		return nil, customStyleSanitizeReport{}, fmt.Errorf("style response is not CSS")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, customStyleMaxBytes+1))
	if err != nil {
		return nil, customStyleSanitizeReport{}, err
	}
	return sanitizeCustomStyle(body, scopeRoot)
}

var errCustomStyleNotFound = fmt.Errorf("style not found")

func sanitizeCustomStyle(css []byte, scopeRoot string) ([]byte, customStyleSanitizeReport, error) {
	if len(css) > customStyleMaxBytes {
		return nil, customStyleSanitizeReport{}, fmt.Errorf("style exceeds %d byte limit", customStyleMaxBytes)
	}
	report := customStyleSanitizeReport{}
	out := parseCustomStyleRules(stripCSSComments(string(css)), scopeRoot, &report)
	return []byte(out), report, nil
}

func (r customStyleSanitizeReport) clone() customStyleSanitizeReport {
	out := customStyleSanitizeReport{Dropped: r.Dropped}
	if len(r.Reasons) > 0 {
		out.Reasons = append([]string(nil), r.Reasons...)
	}
	return out
}

func (r *customStyleSanitizeReport) drop(reason string) {
	r.Dropped++
	if len(r.Reasons) < 25 {
		r.Reasons = append(r.Reasons, reason)
	}
}

func stripCSSComments(css string) string {
	var out strings.Builder
	for i := 0; i < len(css); {
		if i+1 < len(css) && css[i] == '/' && css[i+1] == '*' {
			i += 2
			for i+1 < len(css) && !(css[i] == '*' && css[i+1] == '/') {
				if css[i] == '\n' {
					out.WriteByte('\n')
				}
				i++
			}
			if i+1 < len(css) {
				i += 2
			}
			continue
		}
		out.WriteByte(css[i])
		i++
	}
	return out.String()
}

func parseCustomStyleRules(css, scopeRoot string, report *customStyleSanitizeReport) string {
	var out strings.Builder
	for i := 0; i < len(css); {
		i = skipCSSSpace(css, i)
		if i >= len(css) {
			break
		}
		if css[i] == '@' {
			next, rule := sanitizeCustomAtRule(css, i, scopeRoot, report)
			if rule != "" {
				out.WriteString(rule)
				out.WriteByte('\n')
			}
			if next <= i {
				break
			}
			i = next
			continue
		}
		headerEnd := findCSSControl(css, i)
		if headerEnd < 0 || css[headerEnd] != '{' {
			break
		}
		selector := strings.TrimSpace(css[i:headerEnd])
		body, next, ok := readCSSBlock(css, headerEnd)
		if !ok {
			break
		}
		i = next
		if selector == "" {
			report.drop("empty selector")
			continue
		}
		if strings.Contains(selector, `\`) {
			report.drop("CSS escape sequence")
			continue
		}
		scoped := scopeCustomStyleSelectors(selector, scopeRoot)
		if scoped == "" {
			report.drop("empty selector")
			continue
		}
		body = sanitizeCSSDeclarations(body, report, sanitizeCSSOptions{})
		if strings.TrimSpace(body) == "" {
			report.drop("empty rule")
			continue
		}
		out.WriteString(scoped)
		out.WriteString("{")
		out.WriteString(body)
		out.WriteString("}\n")
	}
	return out.String()
}

func sanitizeCustomAtRule(css string, start int, scopeRoot string, report *customStyleSanitizeReport) (int, string) {
	headerEnd := findCSSControl(css, start)
	if headerEnd < 0 {
		report.drop("unterminated at-rule")
		return len(css), ""
	}
	header := strings.TrimSpace(css[start:headerEnd])
	name := strings.ToLower(strings.TrimPrefix(readAtRuleName(header), "@"))
	if strings.Contains(header, `\`) {
		report.drop("escaped at-rule")
		if css[headerEnd] == '{' {
			_, next, ok := readCSSBlock(css, headerEnd)
			if ok {
				return next, ""
			}
		}
		return headerEnd + 1, ""
	}
	if css[headerEnd] == ';' {
		report.drop("unsupported at-rule: " + name)
		return headerEnd + 1, ""
	}
	body, next, ok := readCSSBlock(css, headerEnd)
	if !ok {
		report.drop("unterminated at-rule: " + name)
		return len(css), ""
	}
	switch name {
	case "import":
		report.drop("@import")
		return next, ""
	case "media", "supports", "container":
		inner := parseCustomStyleRules(body, scopeRoot, report)
		if strings.TrimSpace(inner) == "" {
			report.drop("empty at-rule: " + name)
			return next, ""
		}
		return next, header + "{" + inner + "}"
	case "keyframes", "-webkit-keyframes":
		inner := sanitizeKeyframes(body, report)
		if strings.TrimSpace(inner) == "" {
			report.drop("empty keyframes")
			return next, ""
		}
		return next, header + "{" + inner + "}"
	case "font-face":
		if !fontFaceSrcAllowed(body) {
			report.drop("@font-face external src")
			return next, ""
		}
		decls := sanitizeCSSDeclarations(body, report, sanitizeCSSOptions{allowAnyDataURL: true})
		if strings.TrimSpace(decls) == "" {
			report.drop("empty @font-face")
			return next, ""
		}
		return next, header + "{" + decls + "}"
	default:
		report.drop("unsupported at-rule: " + name)
		return next, ""
	}
}

func readAtRuleName(header string) string {
	for i, r := range header {
		if i == 0 {
			continue
		}
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '-' {
			continue
		}
		return header[:i]
	}
	return header
}

func skipCSSSpace(css string, i int) int {
	for i < len(css) {
		switch css[i] {
		case ' ', '\n', '\r', '\t', '\f':
			i++
		default:
			return i
		}
	}
	return i
}

func findCSSControl(css string, start int) int {
	parenDepth, bracketDepth := 0, 0
	var quote byte
	for i := start; i < len(css); i++ {
		ch := css[i]
		if quote != 0 {
			if ch == quote {
				quote = 0
			}
			continue
		}
		switch ch {
		case '\'', '"':
			quote = ch
		case '(':
			parenDepth++
		case ')':
			if parenDepth > 0 {
				parenDepth--
			}
		case '[':
			bracketDepth++
		case ']':
			if bracketDepth > 0 {
				bracketDepth--
			}
		case '{', ';':
			if parenDepth == 0 && bracketDepth == 0 {
				return i
			}
		}
	}
	return -1
}

func readCSSBlock(css string, open int) (string, int, bool) {
	if open < 0 || open >= len(css) || css[open] != '{' {
		return "", open, false
	}
	depth := 1
	var quote byte
	for i := open + 1; i < len(css); i++ {
		ch := css[i]
		if quote != 0 {
			if ch == quote {
				quote = 0
			}
			continue
		}
		switch ch {
		case '\'', '"':
			quote = ch
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return css[open+1 : i], i + 1, true
			}
		}
	}
	return "", len(css), false
}

type sanitizeCSSOptions struct {
	allowAnyDataURL bool
}

func sanitizeCSSDeclarations(body string, report *customStyleSanitizeReport, opts sanitizeCSSOptions) string {
	decls := splitCSSDeclarations(body)
	kept := make([]string, 0, len(decls))
	for _, decl := range decls {
		decl = strings.TrimSpace(decl)
		if decl == "" {
			continue
		}
		colon := findDeclarationColon(decl)
		if colon <= 0 {
			report.drop("malformed declaration")
			continue
		}
		lower := strings.ToLower(decl)
		prop := strings.TrimSpace(strings.ToLower(decl[:colon]))
		if strings.Contains(decl, `\`) {
			report.drop("CSS escape sequence")
			continue
		}
		if prop == "behavior" || strings.Contains(lower, "expression(") || strings.Contains(lower, "-moz-binding") {
			report.drop("legacy executable CSS")
			continue
		}
		if strings.Contains(lower, "image-set(") {
			report.drop("image-set")
			continue
		}
		next := sanitizeCSSURLsInDeclaration(decl, report, opts)
		kept = append(kept, next)
	}
	if len(kept) == 0 {
		return ""
	}
	return strings.Join(kept, ";")
}

func splitCSSDeclarations(body string) []string {
	var out []string
	start, parenDepth := 0, 0
	var quote byte
	for i := 0; i < len(body); i++ {
		ch := body[i]
		if quote != 0 {
			if ch == quote {
				quote = 0
			}
			continue
		}
		switch ch {
		case '\'', '"':
			quote = ch
		case '(':
			parenDepth++
		case ')':
			if parenDepth > 0 {
				parenDepth--
			}
		case ';':
			if parenDepth == 0 {
				out = append(out, body[start:i])
				start = i + 1
			}
		}
	}
	if start < len(body) {
		out = append(out, body[start:])
	}
	return out
}

func findDeclarationColon(decl string) int {
	parenDepth := 0
	var quote byte
	for i := 0; i < len(decl); i++ {
		ch := decl[i]
		if quote != 0 {
			if ch == quote {
				quote = 0
			}
			continue
		}
		switch ch {
		case '\'', '"':
			quote = ch
		case '(':
			parenDepth++
		case ')':
			if parenDepth > 0 {
				parenDepth--
			}
		case ':':
			if parenDepth == 0 {
				return i
			}
		}
	}
	return -1
}

func sanitizeKeyframes(body string, report *customStyleSanitizeReport) string {
	var out strings.Builder
	for i := 0; i < len(body); {
		i = skipCSSSpace(body, i)
		if i >= len(body) {
			break
		}
		headerEnd := findCSSControl(body, i)
		if headerEnd < 0 || body[headerEnd] != '{' {
			break
		}
		step := strings.TrimSpace(body[i:headerEnd])
		declBody, next, ok := readCSSBlock(body, headerEnd)
		if !ok {
			break
		}
		i = next
		decls := sanitizeCSSDeclarations(declBody, report, sanitizeCSSOptions{})
		if strings.Contains(step, `\`) {
			report.drop("CSS escape sequence")
			continue
		}
		if step == "" || strings.TrimSpace(decls) == "" {
			report.drop("empty keyframe step")
			continue
		}
		out.WriteString(step)
		out.WriteString("{")
		out.WriteString(decls)
		out.WriteString("}")
	}
	return out.String()
}

func fontFaceSrcAllowed(body string) bool {
	for _, decl := range splitCSSDeclarations(body) {
		colon := findDeclarationColon(decl)
		if colon <= 0 {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(decl[:colon]), "src") {
			return cssURLsAllowedForFontSrc(decl[colon+1:])
		}
	}
	return true
}

func cssURLsAllowedForFontSrc(value string) bool {
	allowed := true
	cssURLRE.ReplaceAllStringFunc(value, func(token string) string {
		matches := cssURLRE.FindStringSubmatch(token)
		if len(matches) < 3 || !isAllowedCSSURL(matches[2], true) {
			allowed = false
		}
		return token
	})
	return allowed
}

func scopeCustomStyleSelectors(selector, scopeRoot string) string {
	parts := splitSelectorList(selector)
	scoped := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if scopeRootEscapesWithSiblingCombinator(part, scopeRoot) {
			continue
		}
		if part == scopeRoot || strings.HasPrefix(part, scopeRoot+" ") || strings.HasPrefix(part, scopeRoot+".") || strings.HasPrefix(part, scopeRoot+":") || strings.HasPrefix(part, scopeRoot+"[") {
			scoped = append(scoped, part)
			continue
		}
		if part == "body" || part == "html" || part == ":root" {
			scoped = append(scoped, scopeRoot)
			continue
		}
		if themed, ok := scopeThemeAncestorSelector(part, scopeRoot); ok {
			if themed != "" {
				scoped = append(scoped, themed)
			}
			continue
		}
		if scopeRoot == customStyleAllowedScopes[customStyleScopeDashboard] {
			for _, rootSelector := range []string{"body", "html", ":root"} {
				if strings.HasPrefix(part, rootSelector+".") || strings.HasPrefix(part, rootSelector+":") || strings.HasPrefix(part, rootSelector+"#") {
					part = scopeRoot + strings.TrimPrefix(part, rootSelector)
					scoped = append(scoped, part)
					goto nextSelector
				}
			}
		}
		scoped = append(scoped, scopeRoot+" "+part)
	nextSelector:
	}
	return strings.Join(scoped, ", ")
}

func scopeRootEscapesWithSiblingCombinator(selector, scopeRoot string) bool {
	if !strings.HasPrefix(selector, scopeRoot) {
		return false
	}
	i := len(scopeRoot)
	parenDepth, bracketDepth := 0, 0
	var quote byte
	for i < len(selector) {
		ch := selector[i]
		if quote != 0 {
			if ch == quote {
				quote = 0
			}
			i++
			continue
		}
		switch ch {
		case '\'', '"':
			quote = ch
		case '(':
			parenDepth++
		case ')':
			if parenDepth > 0 {
				parenDepth--
			}
		case '[':
			bracketDepth++
		case ']':
			if bracketDepth > 0 {
				bracketDepth--
			}
		case '+', '~':
			if parenDepth == 0 && bracketDepth == 0 {
				return true
			}
		case ' ', '\n', '\r', '\t', '\f', '>':
			if parenDepth == 0 && bracketDepth == 0 {
				return nextNonSpaceIsSiblingCombinator(selector, i)
			}
		}
		i++
	}
	return false
}

func nextNonSpaceIsSiblingCombinator(selector string, i int) bool {
	for i < len(selector) {
		switch selector[i] {
		case ' ', '\n', '\r', '\t', '\f':
			i++
			continue
		}
		return selector[i] == '+' || selector[i] == '~'
	}
	return false
}

func splitSelectorList(selector string) []string {
	var out []string
	start, parenDepth, bracketDepth := 0, 0, 0
	var quote byte
	for i := 0; i < len(selector); i++ {
		ch := selector[i]
		if quote != 0 {
			if ch == quote {
				quote = 0
			}
			continue
		}
		switch ch {
		case '\'', '"':
			quote = ch
		case '(':
			parenDepth++
		case ')':
			if parenDepth > 0 {
				parenDepth--
			}
		case '[':
			bracketDepth++
		case ']':
			if bracketDepth > 0 {
				bracketDepth--
			}
		case ',':
			if parenDepth == 0 && bracketDepth == 0 {
				out = append(out, selector[start:i])
				start = i + 1
			}
		}
	}
	out = append(out, selector[start:])
	return out
}

func scopeThemeAncestorSelector(selector, scopeRoot string) (string, bool) {
	for _, prefix := range []string{"[data-theme", "html[data-theme", "body[data-theme", ":root[data-theme"} {
		if strings.HasPrefix(selector, prefix) {
			if end := strings.Index(selector, "]"); end >= 0 {
				themed := strings.TrimSpace(selector[:end+1]) + " " + scopeRoot + selector[end+1:]
				if idx := strings.Index(themed, scopeRoot); idx >= 0 && scopeRootEscapesWithSiblingCombinator(strings.TrimSpace(themed[idx:]), scopeRoot) {
					return "", true
				}
				return themed, true
			}
		}
	}
	return "", false
}

func sanitizeCSSURLToken(token string) string {
	matches := cssURLRE.FindStringSubmatch(token)
	if len(matches) < 3 {
		return ""
	}
	raw := strings.TrimSpace(matches[2])
	raw = strings.Trim(raw, `"'`)
	if !isAllowedCSSURL(raw, false) {
		return `url("")`
	}
	return "url(" + raw + ")"
}

func sanitizeCSSURLsInDeclaration(decl string, report *customStyleSanitizeReport, opts sanitizeCSSOptions) string {
	return cssURLRE.ReplaceAllStringFunc(decl, func(token string) string {
		matches := cssURLRE.FindStringSubmatch(token)
		if len(matches) < 3 {
			report.drop("malformed url()")
			return `url("")`
		}
		if !isAllowedCSSURL(matches[2], opts.allowAnyDataURL) {
			report.drop("external url()")
			return `url("")`
		}
		return sanitizeCSSURLTokenWithDataOption(token, opts.allowAnyDataURL)
	})
}

func sanitizeCSSURLTokenWithDataOption(token string, allowAnyDataURL bool) string {
	matches := cssURLRE.FindStringSubmatch(token)
	if len(matches) < 3 {
		return ""
	}
	raw := strings.TrimSpace(matches[2])
	raw = strings.Trim(raw, `"'`)
	if !isAllowedCSSURL(raw, allowAnyDataURL) {
		return `url("")`
	}
	return "url(" + raw + ")"
}

func isAllowedCSSURL(raw string, allowAnyDataURL bool) bool {
	raw = strings.TrimSpace(raw)
	raw = strings.Trim(raw, `"'`)
	lower := strings.ToLower(raw)
	if strings.Contains(raw, `\`) {
		return false
	}
	if allowAnyDataURL && strings.HasPrefix(lower, "data:") {
		return true
	}
	if strings.HasPrefix(lower, "data:image/") {
		return true
	}
	if colon := strings.Index(raw, ":"); colon >= 0 {
		slash := strings.IndexAny(raw, "/?#")
		if slash == -1 || colon < slash {
			return false
		}
	}
	if strings.HasPrefix(raw, "//") || strings.Contains(lower, "://") || strings.HasPrefix(lower, "data:") {
		return false
	}
	return true
}

func (s *Server) handleStyle(w http.ResponseWriter, r *http.Request) {
	css, src, report, err := getCustomStyle(r.Context(), r.URL.Query().Get("src"), r.URL.Query().Get("scope"))
	if err != nil {
		status := http.StatusUnprocessableEntity
		if err == errCustomStyleNotFound {
			status = http.StatusNotFound
		}
		http.Error(w, err.Error(), status)
		return
	}
	if report.Dropped > 0 {
		w.Header().Set("X-Hive-Style-Dropped", fmt.Sprintf("%d", report.Dropped))
	}
	if r.URL.Query().Get("report") == "1" {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=300")
		_ = json.NewEncoder(w).Encode(struct {
			Source string                    `json:"source"`
			CSS    string                    `json:"css"`
			Report customStyleSanitizeReport `json:"report"`
		}{
			Source: customStyleSourceKey(src),
			CSS:    string(css),
			Report: report,
		})
		return
	}
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(css)
}

func (s *Server) handleLeaderboardStyle(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	q.Set("scope", customStyleScopeLeaderboard)
	r.URL.RawQuery = q.Encode()
	s.handleStyle(w, r)
}

// Back-compatible leaderboard name kept for the PR #2834 tests and callers.
type leaderboardCustomStyleSource = customStyleSource

func validateLeaderboardCustomStyleSource(src string) (leaderboardCustomStyleSource, error) {
	return validateCustomStyleSource(src)
}

func leaderboardCustomStyleCacheKey(src leaderboardCustomStyleSource) string {
	return customStyleSourceKey(src)
}

func getLeaderboardCustomStyle(ctx context.Context, rawSrc string) ([]byte, leaderboardCustomStyleSource, error) {
	css, src, _, err := getCustomStyle(ctx, rawSrc, customStyleScopeLeaderboard)
	return css, src, err
}

func sanitizeLeaderboardCustomStyle(css []byte) ([]byte, error) {
	out, _, err := sanitizeCustomStyle(css, customStyleAllowedScopes[customStyleScopeLeaderboard])
	return out, err
}
