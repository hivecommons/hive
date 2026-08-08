package dashboard

import (
	"context"
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
	expiresAt time.Time
}

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

func getCustomStyle(ctx context.Context, rawSrc, rawScope string) ([]byte, customStyleSource, error) {
	scope, root, err := normalizeCustomStyleScope(rawScope)
	if err != nil {
		return nil, customStyleSource{}, err
	}
	src, err := validateCustomStyleSource(rawSrc)
	if err != nil {
		return nil, customStyleSource{}, err
	}
	key := customStyleCacheKey(src, scope)
	now := time.Now()
	customStyleCacheMu.Lock()
	if entry, ok := customStyleCache[key]; ok && now.Before(entry.expiresAt) {
		css := append([]byte(nil), entry.css...)
		customStyleCacheMu.Unlock()
		return css, src, nil
	}
	customStyleCacheMu.Unlock()

	css, err := fetchAndSanitizeCustomStyle(ctx, src, root)
	if err != nil {
		return nil, src, err
	}
	customStyleCacheMu.Lock()
	if len(customStyleCache) >= customStyleMaxCache {
		for k := range customStyleCache {
			delete(customStyleCache, k)
			break
		}
	}
	customStyleCache[key] = customStyleCacheEntry{css: append([]byte(nil), css...), expiresAt: now.Add(customStyleCacheTTL)}
	customStyleCacheMu.Unlock()
	return css, src, nil
}

func fetchAndSanitizeCustomStyle(ctx context.Context, src customStyleSource, scopeRoot string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, customStyleRawURL(src), nil)
	if err != nil {
		return nil, err
	}
	resp, err := customStyleClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errCustomStyleNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("style fetch failed")
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if ct != "" && !strings.Contains(ct, "text/css") && !strings.Contains(ct, "text/plain") {
		return nil, fmt.Errorf("style response is not CSS")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, customStyleMaxBytes+1))
	if err != nil {
		return nil, err
	}
	return sanitizeCustomStyle(body, scopeRoot)
}

var errCustomStyleNotFound = fmt.Errorf("style not found")

func sanitizeCustomStyle(css []byte, scopeRoot string) ([]byte, error) {
	if len(css) > customStyleMaxBytes {
		return nil, fmt.Errorf("style exceeds %d byte limit", customStyleMaxBytes)
	}
	s := cssImportRE.ReplaceAllString(string(css), "")
	s = cssURLRE.ReplaceAllStringFunc(s, sanitizeCSSURLToken)
	var kept []string
	for _, line := range strings.Split(s, "\n") {
		lower := strings.ToLower(line)
		if strings.Contains(line, `\`) {
			continue
		}
		if strings.Contains(lower, "://") || strings.Contains(lower, "image-set(") {
			continue
		}
		if strings.Contains(lower, "expression(") || strings.Contains(lower, "-moz-binding") || strings.Contains(lower, "behavior:") {
			continue
		}
		kept = append(kept, line)
	}
	return []byte(scopeCustomStyle(strings.Join(kept, "\n"), scopeRoot)), nil
}

func scopeCustomStyle(css, scopeRoot string) string {
	var out strings.Builder
	remaining := css
	for {
		open := strings.Index(remaining, "{")
		if open < 0 {
			break
		}
		selector := strings.TrimSpace(remaining[:open])
		remaining = remaining[open+1:]
		close := strings.Index(remaining, "}")
		if close < 0 {
			break
		}
		body := remaining[:close]
		remaining = remaining[close+1:]
		if selector == "" || strings.Contains(selector, "@") {
			continue
		}
		scoped := scopeCustomStyleSelectors(selector, scopeRoot)
		if scoped == "" {
			continue
		}
		out.WriteString(scoped)
		out.WriteString("{")
		out.WriteString(body)
		out.WriteString("}\n")
	}
	return out.String()
}

func scopeCustomStyleSelectors(selector, scopeRoot string) string {
	parts := strings.Split(selector, ",")
	scoped := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if part == scopeRoot || (scopeRoot == customStyleAllowedScopes[customStyleScopeDashboard] && (strings.HasPrefix(part, scopeRoot+" ") || strings.HasPrefix(part, scopeRoot+".") || strings.HasPrefix(part, scopeRoot+":"))) {
			scoped = append(scoped, part)
			continue
		}
		if scopeRoot == customStyleAllowedScopes[customStyleScopeDashboard] {
			if part == "body" || part == "html" || part == ":root" {
				scoped = append(scoped, scopeRoot)
				continue
			}
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
	css, _, err := getCustomStyle(r.Context(), r.URL.Query().Get("src"), r.URL.Query().Get("scope"))
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
	_, _ = w.Write(css)
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

func sanitizeLeaderboardCustomStyle(css []byte) ([]byte, error) {
	return sanitizeCustomStyle(css, customStyleAllowedScopes[customStyleScopeLeaderboard])
}
