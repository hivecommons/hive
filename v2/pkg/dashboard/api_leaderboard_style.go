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
	leaderboardCustomStyleMaxBytes   = 128 * 1024
	leaderboardCustomStyleCacheTTL   = 5 * time.Minute
	leaderboardCustomStyleMaxCache   = 64
	leaderboardCustomStyleFetchTTL   = 10 * time.Second
	leaderboardCustomStyleDefaultRef = "HEAD"
)

var leaderboardCustomStyleRawBaseURL = "https://raw.githubusercontent.com"

type leaderboardCustomStyleSource struct {
	Owner string
	Repo  string
	Path  string
	Ref   string
}

type leaderboardCustomStyleCacheEntry struct {
	css       []byte
	expiresAt time.Time
}

var (
	leaderboardCustomStyleCacheMu sync.Mutex
	leaderboardCustomStyleCache   = map[string]leaderboardCustomStyleCacheEntry{}
	leaderboardCustomStyleClient  = &http.Client{Timeout: leaderboardCustomStyleFetchTTL}
)

var (
	githubOwnerNameRE = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)
	githubRepoNameRE  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
	githubRefNameRE   = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,100}$`)
	githubCSSPathRE   = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)
	cssImportRE       = regexp.MustCompile(`(?is)@import\b[^;]*(;|$)`)
	cssURLRE          = regexp.MustCompile(`(?is)url\(\s*(['"]?)([^'")]+)['"]?\s*\)`)
)

func validateLeaderboardCustomStyleSource(src string) (leaderboardCustomStyleSource, error) {
	src = strings.TrimSpace(src)
	if src == "" {
		return leaderboardCustomStyleSource{}, fmt.Errorf("style source is required")
	}
	if strings.Contains(src, "://") || strings.HasPrefix(src, "//") {
		return leaderboardCustomStyleSource{}, fmt.Errorf("use owner/repo/path.css, not a full URL")
	}
	ref := leaderboardCustomStyleDefaultRef
	if at := strings.LastIndex(src, "@"); at >= 0 {
		if at == len(src)-1 {
			return leaderboardCustomStyleSource{}, fmt.Errorf("ref is empty")
		}
		ref = src[at+1:]
		src = src[:at]
	}
	parts := strings.Split(src, "/")
	if len(parts) < 3 {
		return leaderboardCustomStyleSource{}, fmt.Errorf("style source must be owner/repo/path.css")
	}
	owner, repo := parts[0], parts[1]
	cssPath := strings.Join(parts[2:], "/")
	if !githubOwnerNameRE.MatchString(owner) {
		return leaderboardCustomStyleSource{}, fmt.Errorf("invalid owner")
	}
	if !githubRepoNameRE.MatchString(repo) || repo == "." || repo == ".." {
		return leaderboardCustomStyleSource{}, fmt.Errorf("invalid repo")
	}
	if err := validateLeaderboardCustomStylePath(cssPath); err != nil {
		return leaderboardCustomStyleSource{}, err
	}
	if ref != leaderboardCustomStyleDefaultRef {
		if !githubRefNameRE.MatchString(ref) || strings.Contains(ref, "..") || strings.HasPrefix(ref, "/") || strings.HasSuffix(ref, "/") {
			return leaderboardCustomStyleSource{}, fmt.Errorf("invalid ref")
		}
	}
	return leaderboardCustomStyleSource{Owner: owner, Repo: repo, Path: cssPath, Ref: ref}, nil
}

func validateLeaderboardCustomStylePath(cssPath string) error {
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

func leaderboardCustomStyleCacheKey(src leaderboardCustomStyleSource) string {
	return src.Owner + "/" + src.Repo + "/" + src.Path + "@" + src.Ref
}

func leaderboardCustomStyleRawURL(src leaderboardCustomStyleSource) string {
	parts := []string{strings.TrimRight(leaderboardCustomStyleRawBaseURL, "/"), pathEscape(src.Owner), pathEscape(src.Repo), pathEscape(src.Ref)}
	for _, segment := range strings.Split(src.Path, "/") {
		parts = append(parts, pathEscape(segment))
	}
	return strings.Join(parts, "/")
}

func pathEscape(s string) string {
	return strings.ReplaceAll(url.PathEscape(s), "%2F", "/")
}

func getLeaderboardCustomStyle(ctx context.Context, rawSrc string) ([]byte, leaderboardCustomStyleSource, error) {
	src, err := validateLeaderboardCustomStyleSource(rawSrc)
	if err != nil {
		return nil, leaderboardCustomStyleSource{}, err
	}
	key := leaderboardCustomStyleCacheKey(src)
	now := time.Now()
	leaderboardCustomStyleCacheMu.Lock()
	if entry, ok := leaderboardCustomStyleCache[key]; ok && now.Before(entry.expiresAt) {
		css := append([]byte(nil), entry.css...)
		leaderboardCustomStyleCacheMu.Unlock()
		return css, src, nil
	}
	leaderboardCustomStyleCacheMu.Unlock()

	css, err := fetchAndSanitizeLeaderboardCustomStyle(ctx, src)
	if err != nil {
		return nil, src, err
	}
	leaderboardCustomStyleCacheMu.Lock()
	if len(leaderboardCustomStyleCache) >= leaderboardCustomStyleMaxCache {
		for k := range leaderboardCustomStyleCache {
			delete(leaderboardCustomStyleCache, k)
			break
		}
	}
	leaderboardCustomStyleCache[key] = leaderboardCustomStyleCacheEntry{css: append([]byte(nil), css...), expiresAt: now.Add(leaderboardCustomStyleCacheTTL)}
	leaderboardCustomStyleCacheMu.Unlock()
	return css, src, nil
}

func fetchAndSanitizeLeaderboardCustomStyle(ctx context.Context, src leaderboardCustomStyleSource) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, leaderboardCustomStyleRawURL(src), nil)
	if err != nil {
		return nil, err
	}
	resp, err := leaderboardCustomStyleClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errLeaderboardStyleNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("style fetch failed")
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if ct != "" && !strings.Contains(ct, "text/css") && !strings.Contains(ct, "text/plain") {
		return nil, fmt.Errorf("style response is not CSS")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, leaderboardCustomStyleMaxBytes+1))
	if err != nil {
		return nil, err
	}
	return sanitizeLeaderboardCustomStyle(body)
}

var errLeaderboardStyleNotFound = fmt.Errorf("style not found")

func sanitizeLeaderboardCustomStyle(css []byte) ([]byte, error) {
	if len(css) > leaderboardCustomStyleMaxBytes {
		return nil, fmt.Errorf("style exceeds %d byte limit", leaderboardCustomStyleMaxBytes)
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
	return []byte(scopeLeaderboardCustomStyle(strings.Join(kept, "\n"))), nil
}

func scopeLeaderboardCustomStyle(css string) string {
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
		scoped := scopeLeaderboardSelectors(selector)
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

func scopeLeaderboardSelectors(selector string) string {
	parts := strings.Split(selector, ",")
	scoped := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if part == "#tab-leaderboard" {
			scoped = append(scoped, part)
			continue
		}
		scoped = append(scoped, "#tab-leaderboard "+part)
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

func (s *Server) handleLeaderboardStyle(w http.ResponseWriter, r *http.Request) {
	src := r.URL.Query().Get("src")
	css, _, err := getLeaderboardCustomStyle(r.Context(), src)
	if err != nil {
		status := http.StatusUnprocessableEntity
		if err == errLeaderboardStyleNotFound {
			status = http.StatusNotFound
		}
		http.Error(w, err.Error(), status)
		return
	}
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(css)
}
