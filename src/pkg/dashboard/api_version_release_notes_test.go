package dashboard

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/hub/spoke"
)

const (
	rnFromSHA = "aaaaaaa1111111"
	rnToSHA   = "bbbbbbb2222222"
)

const rnOldLog = "## 2026-10-07 (v5.144.0)\n### Added\n- old feature\n"

const rnNewLog = "## 2026-10-08 (v5.145.0)\n### Added\n- new feature\n### Fixed\n- new fix\n" + "\n" + rnOldLog

type rnStubFetcher struct {
	files map[string]string
	dirs  map[string][]string
	calls int
}

func (f *rnStubFetcher) File(_ context.Context, ref, path string) (string, error) {
	f.calls++
	if v, ok := f.files[ref+":"+path]; ok {
		return v, nil
	}
	return "", errors.New("not found")
}

func (f *rnStubFetcher) Dir(_ context.Context, ref, dir string) ([]string, error) {
	f.calls++
	if v, ok := f.dirs[ref+":"+dir]; ok {
		return v, nil
	}
	return nil, errors.New("not found")
}

func rnSetup(t *testing.T, f releaseNotesFetcher) *Server {
	t.Helper()
	resetReleaseNotesCache()
	prev := newReleaseNotesFetcher
	newReleaseNotesFetcher = func(*Server) releaseNotesFetcher { return f }
	t.Cleanup(func() {
		newReleaseNotesFetcher = prev
		resetReleaseNotesCache()
	})
	s, _ := apiServer(t)
	return s
}

func resetReleaseNotesCache() {
	releaseNotesCache.Lock()
	releaseNotesCache.entries = map[string]releaseNotesCacheEntry{}
	releaseNotesCache.Unlock()
	releaseNotesBuilds.Lock()
	releaseNotesBuilds.inFlight = 0
	releaseNotesBuilds.windowStart = time.Time{}
	releaseNotesBuilds.windowCount = 0
	releaseNotesBuilds.Unlock()
}

func rnDecode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func TestReleaseNotesTaggedToTagged(t *testing.T) {
	f := &rnStubFetcher{files: map[string]string{
		rnFromSHA + ":CHANGELOG.md": rnOldLog,
		rnToSHA + ":CHANGELOG.md":   rnNewLog,
	}}
	s := rnSetup(t, f)
	out := rnDecode(t, doGet(s, "/api/version/release-notes?from="+rnFromSHA+"&to="+rnToSHA))
	if out["source"] != "changelog" || out["truncated"] != false {
		t.Fatalf("unexpected response %v", out)
	}
	secs := out["sections"].([]any)
	if len(secs) != 1 {
		t.Fatalf("sections = %v", secs)
	}
	sec := secs[0].(map[string]any)
	if sec["version"] != "v5.145.0" || sec["date"] != "2026-10-08" {
		t.Errorf("section = %v", sec)
	}
	cats := sec["categories"].(map[string]any)
	if cats["added"].([]any)[0] != "new feature" || cats["fixed"].([]any)[0] != "new fix" {
		t.Errorf("categories = %v", cats)
	}
	if _, ok := out["unreleased"]; ok {
		t.Error("a SHA target with no fragments must not emit unreleased")
	}
	if out["from"].(map[string]any)["version"] != "v5.144.0" || out["to"].(map[string]any)["version"] != "v5.145.0" {
		t.Errorf("versions = %v / %v", out["from"], out["to"])
	}
}

func TestReleaseNotesUntaggedIncludesFragments(t *testing.T) {
	f := &rnStubFetcher{
		files: map[string]string{
			rnFromSHA + ":CHANGELOG.md":                    rnOldLog,
			rnToSHA + ":CHANGELOG.md":                      rnOldLog,
			rnToSHA + ":changelog.d/added-1-new.md":        "- shiny\n",
			rnToSHA + ":changelog.d/fixed-2-bug.md":        "- squashed\n",
			rnToSHA + ":changelog.d/added-3-already-in.md": "- present at from\n",
		},
		dirs: map[string][]string{
			rnFromSHA + ":changelog.d": {"README.md", "added-3-already-in.md"},
			rnToSHA + ":changelog.d":   {"README.md", "added-1-new.md", "fixed-2-bug.md", "added-3-already-in.md", "notes.txt", "odd-4.md"},
		},
	}
	s := rnSetup(t, f)
	out := rnDecode(t, doGet(s, "/api/version/release-notes?from="+rnFromSHA+"&to="+rnToSHA))
	if len(out["sections"].([]any)) != 0 {
		t.Errorf("sections = %v", out["sections"])
	}
	un, ok := out["unreleased"].(map[string]any)
	if !ok {
		t.Fatalf("missing unreleased in %v", out)
	}
	if un["title"] != "Unreleased (in this build)" {
		t.Errorf("title = %v", un["title"])
	}
	cats := un["categories"].(map[string]any)
	if len(cats["added"].([]any)) != 1 || cats["added"].([]any)[0] != "shiny" || cats["fixed"].([]any)[0] != "squashed" {
		t.Errorf("categories = %v", cats)
	}
}

func TestReleaseNotesTagTargetSkipsFragments(t *testing.T) {
	f := &rnStubFetcher{files: map[string]string{
		rnFromSHA + ":CHANGELOG.md": rnOldLog,
		"v5.145.0:CHANGELOG.md":     rnNewLog,
	}}
	s := rnSetup(t, f)
	s.deps = &Dependencies{}
	// A tag name that does not resolve through any source is unavailable.
	out := rnDecode(t, doGet(s, "/api/version/release-notes?from="+rnFromSHA+"&to=v5.145.0"))
	if out["source"] != "unavailable" {
		t.Fatalf("unexpected %v", out)
	}
	// Directly exercise the tagged path: no fragment listing is requested.
	resp := buildReleaseNotes(context.Background(), f, releaseNotesResponse{
		From: releaseNotesFrom{SHA: rnFromSHA},
		To:   releaseNotesTo{SHA: "v5.145.0", Ref: "v5.145.0"},
	})
	if resp.Source != "changelog" || len(resp.Sections) != 1 || resp.Unreleased != nil {
		t.Errorf("unexpected %+v", resp)
	}
}

func TestReleaseNotesSameRevisionIsEmpty(t *testing.T) {
	f := &rnStubFetcher{}
	s := rnSetup(t, f)
	out := rnDecode(t, doGet(s, "/api/version/release-notes?from="+rnFromSHA+"&to="+rnFromSHA[:7]))
	if out["source"] != "changelog" || len(out["sections"].([]any)) != 0 || f.calls != 0 {
		t.Errorf("unexpected %v calls=%d", out, f.calls)
	}
}

func TestReleaseNotesUnavailableNeverFails(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"from missing", map[string]string{rnToSHA + ":CHANGELOG.md": rnNewLog}, "running revision"},
		{"to missing", map[string]string{rnFromSHA + ":CHANGELOG.md": rnOldLog}, "target revision"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := rnSetup(t, &rnStubFetcher{files: tt.files})
			out := rnDecode(t, doGet(s, "/api/version/release-notes?from="+rnFromSHA+"&to="+rnToSHA))
			if out["source"] != "unavailable" || !strings.Contains(out["error"].(string), tt.want) {
				t.Errorf("unexpected %v", out)
			}
			if secs, ok := out["sections"].([]any); !ok || len(secs) != 0 {
				t.Errorf("sections must be an empty array, got %v", out["sections"])
			}
		})
	}
}

func TestReleaseNotesBadRequests(t *testing.T) {
	s := rnSetup(t, &rnStubFetcher{})
	for _, q := range []string{
		"",
		"from=" + rnFromSHA,
		"to=" + rnToSHA,
		"from=nothex&to=" + rnToSHA,
		"from=" + rnFromSHA + "&to=a..b",
		"from=" + rnFromSHA + "&to=bad%20ref",
	} {
		if rec := doGet(s, "/api/version/release-notes?"+q); rec.Code != http.StatusBadRequest {
			t.Errorf("query %q: status = %d", q, rec.Code)
		}
	}
}

func TestReleaseNotesCachesPerPair(t *testing.T) {
	f := &rnStubFetcher{files: map[string]string{
		rnFromSHA + ":CHANGELOG.md": rnOldLog,
		rnToSHA + ":CHANGELOG.md":   rnNewLog,
	}}
	s := rnSetup(t, f)
	url := "/api/version/release-notes?from=" + rnFromSHA + "&to=" + rnToSHA
	doGet(s, url)
	calls := f.calls
	doGet(s, url)
	if f.calls != calls {
		t.Errorf("second request refetched: %d -> %d", calls, f.calls)
	}
	releaseNotesCache.Lock()
	for k, e := range releaseNotesCache.entries {
		e.at = time.Now().Add(-2 * releaseNotesCacheTTL)
		releaseNotesCache.entries[k] = e
	}
	releaseNotesCache.Unlock()
	doGet(s, url)
	if f.calls == calls {
		t.Error("expired entry was served from cache")
	}
}

func TestReleaseNotesFailuresNotCached(t *testing.T) {
	f := &rnStubFetcher{files: map[string]string{}}
	s := rnSetup(t, f)
	url := "/api/version/release-notes?from=" + rnFromSHA + "&to=" + rnToSHA
	doGet(s, url)
	f.files[rnFromSHA+":CHANGELOG.md"] = rnOldLog
	f.files[rnToSHA+":CHANGELOG.md"] = rnNewLog
	out := rnDecode(t, doGet(s, url))
	if out["source"] != "changelog" {
		t.Errorf("failure was cached: %v", out)
	}
}

func TestReleaseNotesTruncated(t *testing.T) {
	var b strings.Builder
	b.WriteString("## 2026-10-09 (v5.146.0)\n### Added\n")
	for i := 0; i < 450; i++ {
		fmt.Fprintf(&b, "- entry %d\n", i)
	}
	s := rnSetup(t, &rnStubFetcher{files: map[string]string{
		rnFromSHA + ":CHANGELOG.md": rnOldLog,
		rnToSHA + ":CHANGELOG.md":   b.String() + rnOldLog,
	}})
	out := rnDecode(t, doGet(s, "/api/version/release-notes?from="+rnFromSHA+"&to="+rnToSHA))
	if out["truncated"] != true {
		t.Errorf("expected truncated, got %v", out["truncated"])
	}
}

func TestReleaseNotesCacheIsBounded(t *testing.T) {
	resetReleaseNotesCache()
	t.Cleanup(resetReleaseNotesCache)
	base := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	for i := 0; i < releaseNotesMaxCacheEntries+10; i++ {
		storeReleaseNotes(fmt.Sprintf("k%03d", i), releaseNotesResponse{Source: "changelog"}, base.Add(time.Duration(i)*time.Second))
	}
	releaseNotesCache.Lock()
	n := len(releaseNotesCache.entries)
	_, oldestKept := releaseNotesCache.entries["k010"]
	_, evicted := releaseNotesCache.entries["k000"]
	releaseNotesCache.Unlock()
	if n != releaseNotesMaxCacheEntries {
		t.Errorf("cache holds %d entries, want %d", n, releaseNotesMaxCacheEntries)
	}
	if evicted || !oldestKept {
		t.Errorf("eviction order wrong: k000 present=%v k010 present=%v", evicted, oldestKept)
	}

	// Expired entries go first, even when the cache is not full.
	resetReleaseNotesCache()
	storeReleaseNotes("stale", releaseNotesResponse{}, base.Add(-2*releaseNotesCacheTTL))
	storeReleaseNotes("fresh", releaseNotesResponse{}, base)
	releaseNotesCache.Lock()
	_, staleKept := releaseNotesCache.entries["stale"]
	releaseNotesCache.Unlock()
	if staleKept {
		t.Error("expired entry survived a store")
	}
}

func TestReleaseNotesUncachedBuildsAreThrottled(t *testing.T) {
	f := &rnStubFetcher{files: map[string]string{
		rnFromSHA + ":CHANGELOG.md": rnOldLog,
		rnToSHA + ":CHANGELOG.md":   rnNewLog,
	}}
	s := rnSetup(t, f)
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	prevNow := releaseNotesNow
	releaseNotesNow = func() time.Time { return now }
	t.Cleanup(func() { releaseNotesNow = prevNow })

	// Each distinct `from` is a cache miss; the stub serves CHANGELOG.md for
	// any ref that has a fixture, so vary a SHA that is never fetched: `to`
	// resolves straight to the changelog, so vary `from` and add fixtures.
	for i := 0; i < releaseNotesBuildBurst; i++ {
		from := fmt.Sprintf("%07d%07d", i, i)
		f.files[from+":CHANGELOG.md"] = rnOldLog
		out := rnDecode(t, doGet(s, "/api/version/release-notes?from="+from+"&to="+rnToSHA))
		if out["source"] != "changelog" {
			t.Fatalf("build %d refused: %v", i, out)
		}
	}
	calls := f.calls
	extra := "cccccccc3333333"
	f.files[extra+":CHANGELOG.md"] = rnOldLog
	out := rnDecode(t, doGet(s, "/api/version/release-notes?from="+extra+"&to="+rnToSHA))
	if out["source"] != "unavailable" || !strings.Contains(out["error"].(string), "limit") {
		t.Errorf("over-burst build was not throttled: %v", out)
	}
	if f.calls != calls {
		t.Errorf("throttled request still fetched: %d -> %d", calls, f.calls)
	}

	// Cached pairs are still served while throttled.
	out = rnDecode(t, doGet(s, "/api/version/release-notes?from=00000000000000&to="+rnToSHA))
	if out["source"] != "changelog" {
		t.Errorf("cached pair refused under throttle: %v", out)
	}

	// The window rolls over.
	now = now.Add(releaseNotesBuildWindow)
	out = rnDecode(t, doGet(s, "/api/version/release-notes?from="+extra+"&to="+rnToSHA))
	if out["source"] != "changelog" {
		t.Errorf("build after window reset refused: %v", out)
	}
}

func TestReleaseNotesConcurrentBuildsAreCapped(t *testing.T) {
	resetReleaseNotesCache()
	t.Cleanup(resetReleaseNotesCache)
	var releases []func()
	for i := 0; i < releaseNotesMaxConcurrentBuilds; i++ {
		rel, _, ok := acquireReleaseNotesBuild()
		if !ok {
			t.Fatalf("build %d refused below the cap", i)
		}
		releases = append(releases, rel)
	}
	if _, reason, ok := acquireReleaseNotesBuild(); ok || !strings.Contains(reason, "already being fetched") {
		t.Errorf("build above the cap admitted: ok=%v reason=%q", ok, reason)
	}
	releases[0]()
	if _, _, ok := acquireReleaseNotesBuild(); !ok {
		t.Error("slot freed by release was not reusable")
	}
}

func TestResolveReleaseNotesRef(t *testing.T) {
	s := newTestServer()
	prev := standaloneChannelRevision
	standaloneChannelRevision = func(ch string) string {
		if ch == "edge" {
			return "EDGE0001"
		}
		return ""
	}
	t.Cleanup(func() { standaloneChannelRevision = prev })

	if got := s.resolveReleaseNotesRef("ABCDEF1234"); got != "abcdef1234" {
		t.Errorf("sha = %q", got)
	}
	if got := s.resolveReleaseNotesRef("edge"); got != "EDGE0001" {
		t.Errorf("edge = %q", got)
	}
	if got := s.resolveReleaseNotesRef("stable"); got != "" {
		t.Errorf("unresolvable channel = %q", got)
	}
	s.versionMu.Lock()
	s.hubUpgradePolicy = &spoke.HeartbeatUpgradePolicy{Channel: "stable", TargetResolved: true, TargetSHA: "cccccccc3333"}
	s.versionMu.Unlock()
	if got := s.resolveReleaseNotesRef("stable"); got != "cccccccc3333" {
		t.Errorf("policy channel = %q", got)
	}
	// Unresolved ref: the response is unavailable, never an error status.
	rec := doGet(rnSetup(t, &rnStubFetcher{}), "/api/version/release-notes?from="+rnFromSHA+"&to=nosuchbranch")
	if out := rnDecode(t, rec); out["source"] != "unavailable" {
		t.Errorf("unexpected %v", out)
	}
}

func rnFakeGitHub(t *testing.T, apiFails bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	file := func(w http.ResponseWriter, content string) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"type": "file", "encoding": "base64", "name": "f",
			"content": base64.StdEncoding.EncodeToString([]byte(content)),
		})
	}
	mux.HandleFunc("/repos/hivecommons/hive/contents/CHANGELOG.md", func(w http.ResponseWriter, r *http.Request) {
		if apiFails {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		file(w, "## 2026-10-08 (v5.145.0)\n### Added\n- via api "+r.URL.Query().Get("ref")+"\n")
	})
	mux.HandleFunc("/repos/hivecommons/hive/contents/changelog.d", func(w http.ResponseWriter, r *http.Request) {
		if apiFails && r.Header.Get("Authorization") != "" {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, `[{"name":"added-1-x.md"},{"name":"fixed-2-y.md"}]`)
	})
	mux.HandleFunc("/hivecommons/hive/deadbeef/CHANGELOG.md", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "## 2026-10-01 (v5.1.0)\n### Added\n- via raw\n")
	})
	mux.HandleFunc("/hivecommons/hive/badbad/CHANGELOG.md", func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestGitHubReleaseNotesFetcher(t *testing.T) {
	srv := rnFakeGitHub(t, false)
	prevRaw, prevAPI := releaseNotesRawBaseURL, releaseNotesAPIBaseURL
	releaseNotesRawBaseURL, releaseNotesAPIBaseURL = srv.URL, srv.URL
	t.Cleanup(func() { releaseNotesRawBaseURL, releaseNotesAPIBaseURL = prevRaw, prevAPI })
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("credentialed API read", func(t *testing.T) {
		g := &githubReleaseNotesFetcher{client: ghpkg.NewClientForTest(srv.URL, "hivecommons", nil, logger).GoGitHub()}
		got, err := g.File(ctx, "deadbeef", "CHANGELOG.md")
		if err != nil || !strings.Contains(got, "via api deadbeef") {
			t.Fatalf("File = %q, %v", got, err)
		}
		names, err := g.Dir(ctx, "deadbeef", "changelog.d")
		if err != nil || len(names) != 2 {
			t.Fatalf("Dir = %v, %v", names, err)
		}
	})
	t.Run("raw fallback without credential", func(t *testing.T) {
		g := &githubReleaseNotesFetcher{}
		got, err := g.File(ctx, "deadbeef", "CHANGELOG.md")
		if err != nil || !strings.Contains(got, "via raw") {
			t.Fatalf("File = %q, %v", got, err)
		}
		names, err := g.Dir(ctx, "deadbeef", "changelog.d")
		if err != nil || len(names) != 2 || names[0] != "added-1-x.md" {
			t.Fatalf("Dir = %v, %v", names, err)
		}
	})
	t.Run("missing content errors", func(t *testing.T) {
		g := &githubReleaseNotesFetcher{}
		if _, err := g.File(ctx, "badbad", "CHANGELOG.md"); err == nil {
			t.Error("expected an error for a 404")
		}
		if _, err := g.Dir(ctx, "deadbeef", "nope"); err == nil {
			t.Error("expected an error for a missing directory")
		}
	})
	t.Run("API failure falls back to raw", func(t *testing.T) {
		failing := rnFakeGitHub(t, true)
		releaseNotesRawBaseURL, releaseNotesAPIBaseURL = failing.URL, failing.URL
		g := &githubReleaseNotesFetcher{client: ghpkg.NewClientForTest(failing.URL, "hivecommons", nil, logger).GoGitHub()}
		got, err := g.File(ctx, "deadbeef", "CHANGELOG.md")
		if err != nil || !strings.Contains(got, "via raw") {
			t.Fatalf("File = %q, %v", got, err)
		}
		if names, err := g.Dir(ctx, "deadbeef", "changelog.d"); err != nil || len(names) != 2 {
			t.Fatalf("Dir = %v, %v", names, err)
		}
	})
	t.Run("bad listing JSON", func(t *testing.T) {
		bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "{") }))
		defer bad.Close()
		releaseNotesAPIBaseURL = bad.URL
		if _, err := (&githubReleaseNotesFetcher{}).Dir(ctx, "deadbeef", "changelog.d"); err == nil {
			t.Error("expected a decode error")
		}
	})
}

func TestNewReleaseNotesFetcherDefault(t *testing.T) {
	s := newTestServer()
	f, ok := newReleaseNotesFetcher(s).(*githubReleaseNotesFetcher)
	if !ok || f.client != nil {
		t.Fatalf("expected a credential-less fetcher, got %#v", f)
	}
	s.deps = &Dependencies{}
	if f, ok := newReleaseNotesFetcher(s).(*githubReleaseNotesFetcher); !ok || f.client != nil {
		t.Fatalf("expected a credential-less fetcher with empty deps, got %#v", f)
	}
}
