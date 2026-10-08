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
)

var (
	releaseNotesRawBaseURL = "https://raw.githubusercontent.com"
	releaseNotesAPIBaseURL = "https://api.github.com"
	releaseNotesHTTP       = &http.Client{Timeout: 10 * time.Second}

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
	if s.deps != nil {
		f.client = s.deps.GHClient.GoGitHub()
	}
	return f
}

var releaseNotesCache = struct {
	sync.Mutex
	entries map[string]releaseNotesCacheEntry
}{entries: map[string]releaseNotesCacheEntry{}}

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

	ctx, cancel := context.WithTimeout(r.Context(), releaseNotesFetchBudget)
	defer cancel()
	resp = buildReleaseNotes(ctx, newReleaseNotesFetcher(s), resp)
	if resp.Source == "changelog" {
		releaseNotesCache.Lock()
		releaseNotesCache.entries[key] = releaseNotesCacheEntry{resp: resp, at: time.Now()}
		releaseNotesCache.Unlock()
	}
	writeReleaseNotes(w, resp)
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
	if time.Since(e.at) > releaseNotesCacheTTL {
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
