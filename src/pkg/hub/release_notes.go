package hub

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/releasenotes"
)

const (
	hubReleaseNotesOwner       = "hivecommons"
	hubReleaseNotesRepo        = "hive"
	hubReleaseNotesCacheTTL    = time.Hour
	hubReleaseNotesFetchBudget = 20 * time.Second
	hubReleaseNotesMaxBody     = 4 << 20
	hubReleaseNotesMaxFrags    = 200
	hubReleaseNotesMaxEntries  = 64
	hubReleaseNotesBuildBurst  = 8
	hubReleaseNotesBuildWindow = 10 * time.Minute
)

var (
	hubReleaseNotesRawBaseURL = "https://raw.githubusercontent.com"
	hubReleaseNotesHTTP       = &http.Client{Timeout: 10 * time.Second}
	hubReleaseNotesSHAPattern = regexp.MustCompile(`^[0-9a-fA-F]{7,40}$`)
)

type hubReleaseNotesFrom struct {
	SHA     string `json:"sha"`
	Version string `json:"version,omitempty"`
}

type hubReleaseNotesTo struct {
	SHA     string `json:"sha"`
	Ref     string `json:"ref"`
	Version string `json:"version,omitempty"`
}

type hubReleaseNotesResponse struct {
	From       hubReleaseNotesFrom    `json:"from"`
	To         hubReleaseNotesTo      `json:"to"`
	Sections   []releasenotes.Section `json:"sections"`
	Unreleased *releasenotes.Section  `json:"unreleased,omitempty"`
	Truncated  bool                   `json:"truncated"`
	Source     string                 `json:"source"`
	Error      string                 `json:"error,omitempty"`
}

type hubReleaseNotesCacheEntry struct {
	resp hubReleaseNotesResponse
	at   time.Time
}

type hubReleaseNotesInFlight struct {
	done chan struct{}
	resp hubReleaseNotesResponse
}

var hubReleaseNotesCache = struct {
	sync.Mutex
	entries  map[string]hubReleaseNotesCacheEntry
	inFlight map[string]*hubReleaseNotesInFlight
}{entries: map[string]hubReleaseNotesCacheEntry{}, inFlight: map[string]*hubReleaseNotesInFlight{}}

var hubReleaseNotesBuilds = struct {
	sync.Mutex
	inFlight    int
	windowStart time.Time
	windowCount int
}{}

var hubReleaseNotesNow = time.Now

func (s *HubServer) handleReleaseNotes(w http.ResponseWriter, r *http.Request) {
	from := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("from")))
	to := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("to")))
	if !hubReleaseNotesSHAPattern.MatchString(from) {
		writeHubReleaseNotesError(w, "from must be a commit SHA", http.StatusBadRequest)
		return
	}
	if !hubReleaseNotesSHAPattern.MatchString(to) {
		writeHubReleaseNotesError(w, "to must be a resolved commit SHA", http.StatusBadRequest)
		return
	}
	if hiveID := strings.TrimSpace(r.URL.Query().Get("hive_id")); hiveID != "" {
		if s == nil || s.verifySpokeUpgradeProof(hiveID, r.Header.Get(proxyAuthHeader)) != spokeProofOK {
			writeHubReleaseNotesError(w, "spoke release-notes proof rejected", http.StatusUnauthorized)
			return
		}
	} else if s != nil && s.getAuthUser(r) == "" {
		writeHubReleaseNotesError(w, "not authenticated", http.StatusUnauthorized)
		return
	}
	resp := hubReleaseNotesResponse{
		From:     hubReleaseNotesFrom{SHA: from},
		To:       hubReleaseNotesTo{SHA: to, Ref: to},
		Sections: []releasenotes.Section{},
		Source:   "unavailable",
	}
	if sameHubCommit(from, to) {
		resp.Source = "changelog"
		writeHubReleaseNotes(w, resp)
		return
	}
	key := from + "|" + to
	if cached, ok := cachedHubReleaseNotes(key); ok {
		writeHubReleaseNotes(w, cached)
		return
	}
	flight, leader := beginHubReleaseNotesBuild(key)
	if !leader {
		<-flight.done
		writeHubReleaseNotes(w, flight.resp)
		return
	}
	defer func() { finishHubReleaseNotesBuild(key, flight, resp) }()

	release, reason, ok := acquireHubReleaseNotesBuild()
	if !ok {
		resp.Error = reason
		writeHubReleaseNotes(w, resp)
		return
	}
	defer release()
	ctx, cancel := context.WithTimeout(r.Context(), hubReleaseNotesFetchBudget)
	defer cancel()
	resp = buildHubReleaseNotes(ctx, resp)
	writeHubReleaseNotes(w, resp)
}

func writeHubReleaseNotesError(w http.ResponseWriter, msg string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func writeHubReleaseNotes(w http.ResponseWriter, resp hubReleaseNotesResponse) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func beginHubReleaseNotesBuild(key string) (*hubReleaseNotesInFlight, bool) {
	hubReleaseNotesCache.Lock()
	defer hubReleaseNotesCache.Unlock()
	if flight, ok := hubReleaseNotesCache.inFlight[key]; ok {
		return flight, false
	}
	flight := &hubReleaseNotesInFlight{done: make(chan struct{})}
	hubReleaseNotesCache.inFlight[key] = flight
	return flight, true
}

func finishHubReleaseNotesBuild(key string, flight *hubReleaseNotesInFlight, resp hubReleaseNotesResponse) {
	hubReleaseNotesCache.Lock()
	if resp.Source == "changelog" {
		storeHubReleaseNotesLocked(key, resp, hubReleaseNotesNow())
	}
	flight.resp = resp
	delete(hubReleaseNotesCache.inFlight, key)
	close(flight.done)
	hubReleaseNotesCache.Unlock()
}

func cachedHubReleaseNotes(key string) (hubReleaseNotesResponse, bool) {
	hubReleaseNotesCache.Lock()
	defer hubReleaseNotesCache.Unlock()
	e, ok := hubReleaseNotesCache.entries[key]
	if !ok {
		return hubReleaseNotesResponse{}, false
	}
	if hubReleaseNotesNow().Sub(e.at) > hubReleaseNotesCacheTTL {
		delete(hubReleaseNotesCache.entries, key)
		return hubReleaseNotesResponse{}, false
	}
	return e.resp, true
}

func storeHubReleaseNotes(key string, resp hubReleaseNotesResponse, now time.Time) {
	hubReleaseNotesCache.Lock()
	defer hubReleaseNotesCache.Unlock()
	storeHubReleaseNotesLocked(key, resp, now)
}

func storeHubReleaseNotesLocked(key string, resp hubReleaseNotesResponse, now time.Time) {
	for k, e := range hubReleaseNotesCache.entries {
		if now.Sub(e.at) > hubReleaseNotesCacheTTL {
			delete(hubReleaseNotesCache.entries, k)
		}
	}
	for len(hubReleaseNotesCache.entries) >= hubReleaseNotesMaxEntries {
		oldestKey, oldest := "", time.Time{}
		for k, e := range hubReleaseNotesCache.entries {
			if oldestKey == "" || e.at.Before(oldest) {
				oldestKey, oldest = k, e.at
			}
		}
		delete(hubReleaseNotesCache.entries, oldestKey)
	}
	hubReleaseNotesCache.entries[key] = hubReleaseNotesCacheEntry{resp: resp, at: now}
}

func acquireHubReleaseNotesBuild() (func(), string, bool) {
	now := hubReleaseNotesNow()
	hubReleaseNotesBuilds.Lock()
	defer hubReleaseNotesBuilds.Unlock()
	if hubReleaseNotesBuilds.inFlight >= 2 {
		return nil, "release notes are already being fetched; retry shortly", false
	}
	if now.Sub(hubReleaseNotesBuilds.windowStart) >= hubReleaseNotesBuildWindow {
		hubReleaseNotesBuilds.windowStart = now
		hubReleaseNotesBuilds.windowCount = 0
	}
	if hubReleaseNotesBuilds.windowCount >= hubReleaseNotesBuildBurst {
		return nil, "release notes fetch limit reached; retry later", false
	}
	hubReleaseNotesBuilds.windowCount++
	hubReleaseNotesBuilds.inFlight++
	return func() {
		hubReleaseNotesBuilds.Lock()
		hubReleaseNotesBuilds.inFlight--
		hubReleaseNotesBuilds.Unlock()
	}, "", true
}

func buildHubReleaseNotes(ctx context.Context, resp hubReleaseNotesResponse) hubReleaseNotesResponse {
	fromLog, err := hubReleaseNotesFile(ctx, resp.From.SHA, "CHANGELOG.md")
	if err != nil {
		resp.Error = "CHANGELOG.md at the running revision: " + err.Error()
		return resp
	}
	toLog, err := hubReleaseNotesFile(ctx, resp.To.SHA, "CHANGELOG.md")
	if err != nil {
		resp.Error = "CHANGELOG.md at the target revision: " + err.Error()
		return resp
	}
	resp.From.Version = releasenotes.Latest(releasenotes.Parse(fromLog))
	resp.To.Version = releasenotes.Latest(releasenotes.Parse(toLog))
	fromFrags, toFrags := fetchHubReleaseNotesFragments(ctx, resp.From.SHA, resp.To.SHA)
	res := releasenotes.Build(fromLog, toLog, fromFrags, toFrags, true, 0)
	resp.Source = "changelog"
	resp.Sections = res.Sections
	if resp.Sections == nil {
		resp.Sections = []releasenotes.Section{}
	}
	resp.Unreleased = res.Unreleased
	resp.Truncated = res.Truncated
	return resp
}

func fetchHubReleaseNotesFragments(ctx context.Context, fromSHA, toSHA string) ([]string, []releasenotes.Fragment) {
	toNames, err := hubReleaseNotesDir(ctx, toSHA, "changelog.d")
	if err != nil {
		return nil, nil
	}
	fromNames, err := hubReleaseNotesDir(ctx, fromSHA, "changelog.d")
	if err != nil {
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
		if len(frags) >= hubReleaseNotesMaxFrags {
			break
		}
		body, err := hubReleaseNotesFile(ctx, toSHA, "changelog.d/"+n)
		if err != nil {
			continue
		}
		frags = append(frags, releasenotes.Fragment{Name: n, Body: body})
	}
	return fromNames, frags
}

func hubReleaseNotesFile(ctx context.Context, ref, path string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(githubAPIBase, "/")+"/repos/"+hubReleaseNotesOwner+"/"+hubReleaseNotesRepo+"/contents/"+path+"?ref="+url.QueryEscape(ref), nil)
	if err == nil {
		req.Header.Set("Accept", "application/vnd.github+json")
		authGitHubRequest(req)
		if resp, err := hubReleaseNotesHTTP.Do(req); err == nil {
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode == http.StatusOK {
				var body struct {
					Content  string `json:"content"`
					Encoding string `json:"encoding"`
				}
				if err := json.NewDecoder(io.LimitReader(resp.Body, hubReleaseNotesMaxBody)).Decode(&body); err == nil && body.Encoding == "base64" {
					dec, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(body.Content, "\n", ""))
					return string(dec), err
				}
			}
		}
	}
	body, err := hubReleaseNotesGet(ctx, fmt.Sprintf("%s/%s/%s/%s/%s", strings.TrimRight(hubReleaseNotesRawBaseURL, "/"), hubReleaseNotesOwner, hubReleaseNotesRepo, url.PathEscape(ref), path))
	return string(body), err
}

func hubReleaseNotesDir(ctx context.Context, ref, dir string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(githubAPIBase, "/")+"/repos/"+hubReleaseNotesOwner+"/"+hubReleaseNotesRepo+"/contents/"+dir+"?ref="+url.QueryEscape(ref), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	authGitHubRequest(req)
	resp, err := hubReleaseNotesHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub returned HTTP %d", resp.StatusCode)
	}
	var entries []struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, hubReleaseNotesMaxBody)).Decode(&entries); err != nil {
		return nil, fmt.Errorf("decoding %s listing: %w", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name)
	}
	return names, nil
}

func hubReleaseNotesGet(ctx context.Context, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hubReleaseNotesHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub returned HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, hubReleaseNotesMaxBody))
}

func sameHubCommit(a, b string) bool {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	if a == "" || b == "" {
		return false
	}
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	return strings.EqualFold(a[:n], b[:n])
}
