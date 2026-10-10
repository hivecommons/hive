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

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/releasenotes"
)

const (
	releaseNotesOwner       = "hivecommons"
	releaseNotesRepo        = "hive"
	releaseNotesCacheTTL    = time.Hour
	releaseNotesFetchBudget = 20 * time.Second
	releaseNotesMaxBody     = 4 << 20
	releaseNotesMaxFrags    = 200
	// releaseNotesMaxCacheEntries bounds the per-(from, to) response cache.
	// The key is caller-supplied, so without a bound every distinct pair a
	// session asks for stays resident until its TTL lookup — which never
	// happens for a pair nobody asks for twice.
	releaseNotesMaxCacheEntries = 64
	// releaseNotesMaxConcurrentBuilds caps uncached builds in flight at once.
	// One build is up to 4 + releaseNotesMaxFrags GitHub content reads against
	// the hive's own credential.
	releaseNotesMaxConcurrentBuilds = 2
	// releaseNotesBuildBurst / releaseNotesBuildWindow throttle uncached
	// builds process-wide: a cache miss is cheap to request and expensive to
	// serve, and the dashboard's own use is one build per version-chip open.
	releaseNotesBuildBurst  = 8
	releaseNotesBuildWindow = 10 * time.Minute
)

var (
	releaseNotesRawBaseURL = "https://raw.githubusercontent.com"
	releaseNotesAPIBaseURL = "https://api.github.com"
	releaseNotesHTTP       = &http.Client{Timeout: 10 * time.Second}
	releaseNotesHubHTTP    = &http.Client{Timeout: 10 * time.Second}

	releaseNotesSHAPattern = regexp.MustCompile(`^[0-9a-fA-F]{7,40}$`)
	releaseNotesRefPattern = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,100}$`)
	releaseNotesTagPattern = regexp.MustCompile(`^v\d+\.\d+\.\d+`)
)

// releaseNotesFetcher reads repo content at a revision.
type releaseNotesFetcher interface {
	File(ctx context.Context, ref, path string) (string, error)
	Dir(ctx context.Context, ref, dir string) ([]string, error)
}

// newReleaseNotesFetcher is a seam so tests can serve fixture content.
var newReleaseNotesFetcher = func(s *Server) releaseNotesFetcher {
	f := &githubReleaseNotesFetcher{}
	if s.deps != nil && s.deps.GHClient != nil {
		f.client = s.deps.GHClient.GoGitHub()
	}
	return f
}

var releaseNotesCache = struct {
	sync.Mutex
	entries map[string]releaseNotesCacheEntry
}{entries: map[string]releaseNotesCacheEntry{}}

// releaseNotesBuilds is the process-wide admission state for uncached builds:
// a fixed-window counter plus an in-flight count. Guarded by its own mutex so
// a slow build never holds the cache lock.
var releaseNotesBuilds = struct {
	sync.Mutex
	inFlight    int
	windowStart time.Time
	windowCount int
}{}

// releaseNotesNow is a seam for tests.
var releaseNotesNow = time.Now

// acquireReleaseNotesBuild admits one uncached build, returning a release
// func, or ok=false with the reason when the concurrency cap or the
// fixed-window throttle is exhausted.
func acquireReleaseNotesBuild() (release func(), reason string, ok bool) {
	now := releaseNotesNow()
	releaseNotesBuilds.Lock()
	defer releaseNotesBuilds.Unlock()
	if releaseNotesBuilds.inFlight >= releaseNotesMaxConcurrentBuilds {
		return nil, "release notes are already being fetched; retry shortly", false
	}
	if now.Sub(releaseNotesBuilds.windowStart) >= releaseNotesBuildWindow {
		releaseNotesBuilds.windowStart = now
		releaseNotesBuilds.windowCount = 0
	}
	if releaseNotesBuilds.windowCount >= releaseNotesBuildBurst {
		return nil, "release notes fetch limit reached; retry later", false
	}
	releaseNotesBuilds.windowCount++
	releaseNotesBuilds.inFlight++
	return func() {
		releaseNotesBuilds.Lock()
		releaseNotesBuilds.inFlight--
		releaseNotesBuilds.Unlock()
	}, "", true
}

// storeReleaseNotes caches resp under key, dropping expired entries and then
// the oldest entries until the cache fits releaseNotesMaxCacheEntries.
func storeReleaseNotes(key string, resp releaseNotesResponse, now time.Time) {
	releaseNotesCache.Lock()
	defer releaseNotesCache.Unlock()
	for k, e := range releaseNotesCache.entries {
		if now.Sub(e.at) > releaseNotesCacheTTL {
			delete(releaseNotesCache.entries, k)
		}
	}
	for len(releaseNotesCache.entries) >= releaseNotesMaxCacheEntries {
		oldestKey, oldest := "", time.Time{}
		for k, e := range releaseNotesCache.entries {
			if oldestKey == "" || e.at.Before(oldest) {
				oldestKey, oldest = k, e.at
			}
		}
		delete(releaseNotesCache.entries, oldestKey)
	}
	releaseNotesCache.entries[key] = releaseNotesCacheEntry{resp: resp, at: now}
}

type releaseNotesCacheEntry struct {
	resp releaseNotesResponse
	at   time.Time
}

type releaseNotesFrom struct {
	SHA     string `json:"sha"`
	Version string `json:"version,omitempty"`
}

type releaseNotesTo struct {
	SHA     string `json:"sha"`
	Ref     string `json:"ref"`
	Version string `json:"version,omitempty"`
}

type releaseNotesResponse struct {
	From       releaseNotesFrom       `json:"from"`
	To         releaseNotesTo         `json:"to"`
	Sections   []releasenotes.Section `json:"sections"`
	Unreleased *releasenotes.Section  `json:"unreleased,omitempty"`
	Truncated  bool                   `json:"truncated"`
	Source     string                 `json:"source"`
	Error      string                 `json:"error,omitempty"`
}

// handleVersionReleaseNotes answers GET /api/version/release-notes?from=<sha>&to=<ref-or-sha>
// with the changelog sections present at `to` and absent at `from`. Fetch
// failures are reported in-band (source "unavailable") so the UI can still
// offer Upgrade; only a malformed request is an error status.
func (s *Server) handleVersionReleaseNotes(w http.ResponseWriter, r *http.Request) {
	from := strings.TrimSpace(r.URL.Query().Get("from"))
	to := strings.TrimSpace(r.URL.Query().Get("to"))
	if !releaseNotesSHAPattern.MatchString(from) {
		jsonError(w, "from must be a commit SHA", http.StatusBadRequest)
		return
	}
	if !releaseNotesRefPattern.MatchString(to) || strings.Contains(to, "..") {
		jsonError(w, "to must be a channel, branch, tag or commit SHA", http.StatusBadRequest)
		return
	}
	from = strings.ToLower(from)

	resp := releaseNotesResponse{
		From:     releaseNotesFrom{SHA: from},
		To:       releaseNotesTo{SHA: to, Ref: to},
		Sections: []releasenotes.Section{},
		Source:   "unavailable",
	}
	toSHA := s.resolveReleaseNotesRef(to)
	if toSHA == "" {
		resp.Error = fmt.Sprintf("could not resolve %q to a commit", to)
		writeReleaseNotes(w, resp)
		return
	}
	resp.To.SHA = toSHA

	if sameCommitDashboard(from, toSHA) {
		resp.Source = "changelog"
		writeReleaseNotes(w, resp)
		return
	}

	key := from + "|" + toSHA
	if cached, ok := cachedReleaseNotes(key); ok {
		writeReleaseNotes(w, cached)
		return
	}
	if hubResp, attempted := s.fetchHubReleaseNotes(r.Context(), from, toSHA); attempted {
		if hubResp.Source == "changelog" {
			storeReleaseNotes(key, hubResp, releaseNotesNow())
		}
		writeReleaseNotes(w, hubResp)
		return
	}

	release, reason, ok := acquireReleaseNotesBuild()
	if !ok {
		resp.Error = reason
		writeReleaseNotes(w, resp)
		return
	}
	defer release()

	ctx, cancel := context.WithTimeout(r.Context(), releaseNotesFetchBudget)
	defer cancel()
	resp = buildReleaseNotes(ctx, newReleaseNotesFetcher(s), resp)
	if resp.Source == "changelog" {
		storeReleaseNotes(key, resp, releaseNotesNow())
	}
	writeReleaseNotes(w, resp)
}

func (s *Server) fetchHubReleaseNotes(ctx context.Context, from, toSHA string) (releaseNotesResponse, bool) {
	resp := releaseNotesResponse{
		From:     releaseNotesFrom{SHA: from},
		To:       releaseNotesTo{SHA: toSHA, Ref: toSHA},
		Sections: []releasenotes.Section{},
		Source:   "unavailable",
	}
	if s == nil || s.deps == nil || s.deps.Config == nil || strings.TrimSpace(s.deps.Config.Hub.URL) == "" {
		return resp, false
	}
	hubURL := strings.TrimRight(strings.TrimSpace(s.deps.Config.Hub.URL), "/") + "/api/saas/release-notes"
	u, err := url.Parse(hubURL)
	if err != nil {
		resp.Error = "hub release notes URL invalid: " + err.Error()
		return resp, true
	}
	q := u.Query()
	q.Set("from", from)
	q.Set("to", toSHA)
	if hiveID := strings.TrimSpace(s.deps.Config.HiveID); hiveID != "" {
		q.Set("hive_id", hiveID)
	}
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		resp.Error = "hub release notes request invalid: " + err.Error()
		return resp, true
	}
	if proof := s.authToken; proof != "" {
		req.Header.Set(proxyAuthHeader, proof)
	} else if s.deps.Config.Dashboard.AuthToken != "" {
		req.Header.Set(proxyAuthHeader, s.deps.Config.Dashboard.AuthToken)
	}
	httpResp, err := releaseNotesHubHTTP.Do(req)
	if err != nil {
		resp.Error = "hub release notes unavailable: " + err.Error()
		return resp, true
	}
	defer closeHTTPBody(httpResp.Body)
	if httpResp.StatusCode != http.StatusOK {
		resp.Error = fmt.Sprintf("hub release notes returned HTTP %d", httpResp.StatusCode)
		return resp, true
	}
	if err := json.NewDecoder(io.LimitReader(httpResp.Body, releaseNotesMaxBody)).Decode(&resp); err != nil {
		resp.Source = "unavailable"
		resp.Error = "hub release notes response invalid: " + err.Error()
		resp.Sections = []releasenotes.Section{}
		return resp, true
	}
	if resp.Sections == nil {
		resp.Sections = []releasenotes.Section{}
	}
	return resp, true
}

func writeReleaseNotes(w http.ResponseWriter, resp releaseNotesResponse) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func cachedReleaseNotes(key string) (releaseNotesResponse, bool) {
	releaseNotesCache.Lock()
	defer releaseNotesCache.Unlock()
	e, ok := releaseNotesCache.entries[key]
	if !ok {
		return releaseNotesResponse{}, false
	}
	if releaseNotesNow().Sub(e.at) > releaseNotesCacheTTL {
		delete(releaseNotesCache.entries, key)
		return releaseNotesResponse{}, false
	}
	return e.resp, true
}

// resolveReleaseNotesRef maps a SHA, release channel, branch or tag to a commit
// SHA through the same sources /api/version measures against. It returns ""
// when the ref cannot be resolved.
func (s *Server) resolveReleaseNotesRef(ref string) string {
	if releaseNotesSHAPattern.MatchString(ref) {
		return strings.ToLower(ref)
	}
	s.versionMu.RLock()
	policy := s.hubUpgradePolicy
	s.versionMu.RUnlock()
	if policy != nil && policy.Channel == ref && policy.TargetResolved && policy.TargetSHA != "" {
		return policy.TargetSHA
	}
	if isStandaloneReleaseChannel(ref) {
		if sha := standaloneChannelRevision(ref); sha != "" {
			return sha
		}
	}
	if sha, err := s.fetchRemoteHashForBranch(ref); err == nil {
		return sha
	}
	return ""
}

// buildReleaseNotes fetches the changelogs (and, for an untagged target, the
// changelog.d fragments) and fills in the response. Any changelog fetch failure
// yields source "unavailable".
func buildReleaseNotes(ctx context.Context, f releaseNotesFetcher, resp releaseNotesResponse) releaseNotesResponse {
	fromLog, err := f.File(ctx, resp.From.SHA, "CHANGELOG.md")
	if err != nil {
		resp.Error = "CHANGELOG.md at the running revision: " + err.Error()
		return resp
	}
	toLog, err := f.File(ctx, resp.To.SHA, "CHANGELOG.md")
	if err != nil {
		resp.Error = "CHANGELOG.md at the target revision: " + err.Error()
		return resp
	}
	resp.From.Version = releasenotes.Latest(releasenotes.Parse(fromLog))
	resp.To.Version = releasenotes.Latest(releasenotes.Parse(toLog))

	untagged := !releaseNotesTagPattern.MatchString(resp.To.Ref)
	var fromFrags []string
	var toFrags []releasenotes.Fragment
	if untagged {
		fromFrags, toFrags = fetchReleaseNotesFragments(ctx, f, resp.From.SHA, resp.To.SHA)
	}
	res := releasenotes.Build(fromLog, toLog, fromFrags, toFrags, untagged, 0)
	resp.Source = "changelog"
	resp.Sections = res.Sections
	if resp.Sections == nil {
		resp.Sections = []releasenotes.Section{}
	}
	resp.Unreleased = res.Unreleased
	resp.Truncated = res.Truncated
	return resp
}

// fetchReleaseNotesFragments is best-effort: a missing changelog.d directory
// or an unreadable fragment only shrinks the unreleased section.
func fetchReleaseNotesFragments(ctx context.Context, f releaseNotesFetcher, fromSHA, toSHA string) ([]string, []releasenotes.Fragment) {
	toNames, err := f.Dir(ctx, toSHA, "changelog.d")
	if err != nil {
		return nil, nil
	}
	fromNames, err := f.Dir(ctx, fromSHA, "changelog.d")
	if err != nil {
		// Without the from listing every fragment would look new.
		return nil, nil
	}
	have := make(map[string]bool, len(fromNames))
	for _, n := range fromNames {
		have[n] = true
	}
	var frags []releasenotes.Fragment
	for _, n := range toNames {
		if have[n] || !strings.HasSuffix(n, ".md") || releasenotes.FragmentCategory(n) == "" {
			continue
		}
		if len(frags) >= releaseNotesMaxFrags {
			break
		}
		body, err := f.File(ctx, toSHA, "changelog.d/"+n)
		if err != nil {
			continue
		}
		frags = append(frags, releasenotes.Fragment{Name: n, Body: body})
	}
	return fromNames, frags
}

// githubReleaseNotesFetcher reads through the configured GitHub credential and
// falls back to unauthenticated public endpoints when that fails or no
// credential is configured.
type githubReleaseNotesFetcher struct {
	client *gh.Client
}

func (g *githubReleaseNotesFetcher) File(ctx context.Context, ref, path string) (string, error) {
	if g.client != nil {
		fc, _, _, err := g.client.Repositories.GetContents(ctx, releaseNotesOwner, releaseNotesRepo, path, &gh.RepositoryContentGetOptions{Ref: ref})
		if err == nil && fc != nil {
			if content, cerr := fc.GetContent(); cerr == nil {
				return content, nil
			}
		}
	}
	body, err := releaseNotesGet(ctx, fmt.Sprintf("%s/%s/%s/%s/%s", strings.TrimRight(releaseNotesRawBaseURL, "/"), releaseNotesOwner, releaseNotesRepo, url.PathEscape(ref), path))
	return string(body), err
}

func (g *githubReleaseNotesFetcher) Dir(ctx context.Context, ref, dir string) ([]string, error) {
	if g.client != nil {
		_, entries, _, err := g.client.Repositories.GetContents(ctx, releaseNotesOwner, releaseNotesRepo, dir, &gh.RepositoryContentGetOptions{Ref: ref})
		if err == nil && entries != nil {
			names := make([]string, 0, len(entries))
			for _, e := range entries {
				names = append(names, e.GetName())
			}
			return names, nil
		}
	}
	body, err := releaseNotesGet(ctx, fmt.Sprintf("%s/repos/%s/%s/contents/%s?ref=%s", strings.TrimRight(releaseNotesAPIBaseURL, "/"), releaseNotesOwner, releaseNotesRepo, dir, url.QueryEscape(ref)))
	if err != nil {
		return nil, err
	}
	var entries []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil, fmt.Errorf("decoding %s listing: %w", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name)
	}
	return names, nil
}

func releaseNotesGet(ctx context.Context, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := releaseNotesHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer closeHTTPBody(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub returned HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, releaseNotesMaxBody))
}
