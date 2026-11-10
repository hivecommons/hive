package dashboard

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/knowledge"
)

// deadLoopbackURL returns an http URL on 127.0.0.1 pointing at a port that was
// just listened on and closed, so any connection attempt fails immediately
// with connection-refused instead of hanging. This lets a test drive
// ConnectGitSource through URL validation into a fast, deterministic clone
// failure without any network access.
func deadLoopbackURL(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve loopback port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("release loopback port: %v", err)
	}
	return "http://" + addr + "/repo.git"
}

// TestGitSourcesConnect_OwnerGate pins that POST, PUT and DELETE on
// /api/knowledge/git-sources are refused outright without a verified owner
// role: these endpoints mutate persisted config and trigger outbound git
// clones, so the gate must fire before any body parsing or validation.
func TestGitSourcesConnect_OwnerGate(t *testing.T) {
	s := covApiServer(t)

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, "/api/knowledge/git-sources",
			strings.NewReader(`{"name":"docs","url":"https://example.com/r.git"}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s without owner role: expected 403, got %d", method, rec.Code)
		}
	}
}

// TestGitSourcesConnect_DefaultLayerCloneFailure drives handleGitSourcesConnect
// past URL validation (HIVE_ALLOW_PRIVATE_GIT_SOURCE permits the loopback
// literal) into ConnectGitSource against a dead port. It pins the arms that sit
// between validation and success:
//   - an omitted layer defaults to "project" (the request must not be rejected
//     for a missing layer),
//   - a ConnectGitSource failure surfaces as HTTP 500 with the error text, and
//   - a failed connect must NOT append the source to persisted config —
//     otherwise a bad URL would be retried by every future startup.
func TestGitSourcesConnect_DefaultLayerCloneFailure(t *testing.T) {
	t.Setenv("HIVE_ALLOW_PRIVATE_GIT_SOURCE", "true")

	s, deps := apiServer(t)
	url := deadLoopbackURL(t)

	rec := doPost(s, "/api/knowledge/git-sources", map[string]any{
		"name": "dead-docs",
		"url":  url,
	})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("connect to dead port: expected 500, got %d (body %q)", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "dead-docs") {
		t.Errorf("expected error body to identify the failing source, got %q", body)
	}
	if n := len(deps.Config.Knowledge.GitSources); n != 0 {
		t.Errorf("failed connect must not persist config entry, got %d entries", n)
	}
}

// TestGitSourcesConnect_ExplicitLayerCloneFailure repeats the dead-port drive
// with an explicit layer so the non-defaulted arm is pinned too: the supplied
// layer must be accepted verbatim (no rejection, no silent rewrite to
// "project") all the way into the connect attempt.
func TestGitSourcesConnect_ExplicitLayerCloneFailure(t *testing.T) {
	t.Setenv("HIVE_ALLOW_PRIVATE_GIT_SOURCE", "true")

	s, deps := apiServer(t)
	url := deadLoopbackURL(t)

	rec := doPost(s, "/api/knowledge/git-sources", map[string]any{
		"name":  "dead-org-docs",
		"url":   url,
		"layer": "org",
	})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("connect to dead port: expected 500, got %d (body %q)", rec.Code, rec.Body.String())
	}
	if n := len(deps.Config.Knowledge.GitSources); n != 0 {
		t.Errorf("failed connect must not persist config entry, got %d entries", n)
	}
}

func installHermeticGit(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "git.log")
	script := `#!/bin/sh
if [ -n "${HIVE_FAKE_GIT_LOG:-}" ]; then
  printf '%s\n' "$*" >> "$HIVE_FAKE_GIT_LOG"
fi
case "$*" in
  *clone*)
    for arg do dest="$arg"; done
    mkdir -p "$dest/.git"
    mkdir -p "$dest/docs"
    printf '# Docs\n' > "$dest/README.md"
    printf '# Nested docs\n' > "$dest/docs/guide.md"
    exit "${HIVE_FAKE_GIT_CLONE_EXIT:-0}"
    ;;
  *sparse-checkout\ init*)
    exit "${HIVE_FAKE_GIT_SPARSE_INIT_EXIT:-0}"
    ;;
  *sparse-checkout\ set*)
    exit "${HIVE_FAKE_GIT_SPARSE_SET_EXIT:-0}"
    ;;
  *pull*)
    exit "${HIVE_FAKE_GIT_PULL_EXIT:-0}"
    ;;
  *ls-remote*)
    printf 'ref: refs/heads/main\tHEAD\n'
    ;;
esac
exit 0
`
	path := filepath.Join(dir, "git")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HIVE_FAKE_GIT_LOG", logPath)
	return logPath
}

func gitSourceTestServer(t *testing.T) (*Server, *Dependencies, string) {
	t.Helper()
	t.Setenv("HIVE_ALLOW_PRIVATE_GIT_SOURCE", "true")
	knowledge.SetBaseDirForTest(t, t.TempDir())

	s, deps := apiServer(t)
	deps.Config.SourcePath = filepath.Join(t.TempDir(), "hive.yaml")
	deps.Knowledge = knowledge.NewKnowledgeAPI(nil, knowledge.KnowledgeConfig{Enabled: true, Engine: "file"}, deps.Logger)
	return s, deps, "http://127.0.0.1/repo.git"
}

func doDeleteJSON(s *Server, path string, body interface{}) *httptest.ResponseRecorder {
	var b strings.Builder
	if body != nil {
		if err := json.NewEncoder(&b).Encode(body); err != nil {
			panic(err)
		}
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, path, strings.NewReader(b.String()))
	req.Header.Set("Content-Type", "application/json")
	markOwnerRequest(req)
	s.mux.ServeHTTP(rec, req)
	return rec
}

func TestGitSourcesConnectSuccessPersistsAndDedupes(t *testing.T) {
	tests := []struct {
		name        string
		body        map[string]any
		preexisting []config.GitSourceConfigYAML
		want        []config.GitSourceConfigYAML
	}{
		{
			name: "initial connect persists default layer",
			body: map[string]any{
				"name": "docs",
			},
			want: []config.GitSourceConfigYAML{
				{Name: "docs", Branch: "main", Layer: "project"},
			},
		},
		{
			name: "same url and subpath already in config is deduped",
			body: map[string]any{
				"name": "docs-again",
			},
			preexisting: []config.GitSourceConfigYAML{
				{Name: "existing-docs", Layer: "project"},
			},
			want: []config.GitSourceConfigYAML{
				{Name: "existing-docs", Layer: "project"},
			},
		},
		{
			name: "different subpath persists explicit branch and layer",
			body: map[string]any{
				"name":    "org-docs",
				"branch":  "release/v4",
				"subpath": "docs",
				"layer":   "org",
			},
			want: []config.GitSourceConfigYAML{
				{Name: "org-docs", Branch: "release/v4", Subpath: "docs", Layer: "org"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logPath := installHermeticGit(t)
			s, deps, url := gitSourceTestServer(t)
			body := make(map[string]any, len(tc.body)+1)
			for k, v := range tc.body {
				body[k] = v
			}
			body["url"] = url
			for i := range tc.preexisting {
				tc.preexisting[i].URL = url
			}
			for i := range tc.want {
				tc.want[i].URL = url
			}
			deps.Config.Knowledge.GitSources = append(deps.Config.Knowledge.GitSources, tc.preexisting...)

			rec := doPost(s, "/api/knowledge/git-sources", body)
			if rec.Code != http.StatusOK {
				t.Fatalf("connect: expected 200, got %d (body %q)", rec.Code, rec.Body.String())
			}
			if got := deps.Config.Knowledge.GitSources; len(got) != len(tc.want) {
				t.Fatalf("persisted git sources = %+v, want %+v", got, tc.want)
			} else {
				for i := range tc.want {
					if got[i] != tc.want[i] {
						t.Fatalf("persisted git sources = %+v, want %+v", got, tc.want)
					}
				}
			}
			persisted, err := os.ReadFile(deps.Config.SourcePath)
			if err != nil {
				t.Fatalf("read persisted config: %v", err)
			}
			if !strings.Contains(string(persisted), url) {
				t.Fatalf("persisted config %s does not contain connected git URL", deps.Config.SourcePath)
			}
			logged, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(logged), " clone ") {
				t.Fatalf("fake git was not invoked for clone; log:\n%s", logged)
			}
		})
	}
}

func TestGitSourcesConnectPartialFailureDoesNotPersist(t *testing.T) {
	installHermeticGit(t)
	t.Setenv("HIVE_FAKE_GIT_SPARSE_SET_EXIT", "7")
	s, deps, url := gitSourceTestServer(t)

	rec := doPost(s, "/api/knowledge/git-sources", map[string]any{
		"name":    "docs",
		"url":     url,
		"subpath": "docs",
	})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("connect with sparse-checkout failure: expected 500, got %d (body %q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "sparse-checkout set") {
		t.Fatalf("error body should report sparse-checkout failure, got %q", rec.Body.String())
	}
	if got := len(deps.Config.Knowledge.GitSources); got != 0 {
		t.Fatalf("partial clone failure persisted %d git sources, want 0", got)
	}
	if _, err := os.Stat(deps.Config.SourcePath); !os.IsNotExist(err) {
		t.Fatalf("partial clone failure should not save config, stat error = %v", err)
	}
}

func TestGitSourcesDisconnectSuccessFiltersAndPersists(t *testing.T) {
	installHermeticGit(t)
	s, deps, url := gitSourceTestServer(t)
	otherURL := "http://127.0.0.1/other.git"

	for _, body := range []map[string]any{
		{"name": "root-docs", "url": url},
		{"name": "nested-docs", "url": url, "subpath": "docs"},
		{"name": "other-docs", "url": otherURL},
	} {
		rec := doPost(s, "/api/knowledge/git-sources", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("seed connect %v: got %d (body %q)", body, rec.Code, rec.Body.String())
		}
	}

	rec := doDeleteJSON(s, "/api/knowledge/git-sources", map[string]any{
		"url":     url,
		"subpath": "docs",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("disconnect: expected 200, got %d (body %q)", rec.Code, rec.Body.String())
	}

	want := []config.GitSourceConfigYAML{
		{Name: "root-docs", URL: url, Branch: "main", Layer: "project"},
		{Name: "other-docs", URL: otherURL, Branch: "main", Layer: "project"},
	}
	if got := deps.Config.Knowledge.GitSources; len(got) != len(want) {
		t.Fatalf("remaining git sources = %+v, want %+v", got, want)
	} else {
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("remaining git sources = %+v, want %+v", got, want)
			}
		}
	}
	persisted, err := os.ReadFile(deps.Config.SourcePath)
	if err != nil {
		t.Fatalf("read persisted config: %v", err)
	}
	if text := string(persisted); strings.Contains(text, "nested-docs") || !strings.Contains(text, "root-docs") || !strings.Contains(text, "other-docs") {
		t.Fatalf("persisted config did not filter only the requested git source:\n%s", text)
	}
}

func TestGitSourcesDisconnectNotFoundDoesNotPersistSuccess(t *testing.T) {
	installHermeticGit(t)
	s, deps, url := gitSourceTestServer(t)
	deps.Config.Knowledge.GitSources = []config.GitSourceConfigYAML{
		{Name: "stale-docs", URL: url, Layer: "project"},
	}

	rec := doDeleteJSON(s, "/api/knowledge/git-sources", map[string]any{
		"url": url,
	})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("disconnect missing live source: expected 404, got %d (body %q)", rec.Code, rec.Body.String())
	}
	if got := deps.Config.Knowledge.GitSources; len(got) != 1 || got[0].Name != "stale-docs" {
		t.Fatalf("failed disconnect changed persisted config: %+v", got)
	}
	if _, err := os.Stat(deps.Config.SourcePath); !os.IsNotExist(err) {
		t.Fatalf("failed disconnect should not save config, stat error = %v", err)
	}
}

// TestGitSourcesUpdateMetadataVsReclone pins the two PUT paths: a name/layer
// edit rewrites the live source and its config entry without cloning again,
// while a url/branch/subpath edit swaps in a fresh clone in the same request.
// Either way exactly one knowledge_git_source_update audit row is written.
func TestGitSourcesUpdateMetadataVsReclone(t *testing.T) {
	const otherURL = "http://127.0.0.1/other.git"
	tests := []struct {
		name         string
		body         map[string]any
		wantRecloned bool
		wantLive     knowledge.GitSourceInfo
		wantConfig   config.GitSourceConfigYAML
		wantCloneArg string
	}{
		{
			name:         "name and layer only update in place",
			body:         map[string]any{"name": "renamed-docs", "layer": "org"},
			wantLive:     knowledge.GitSourceInfo{Name: "renamed-docs", Branch: "main", Layer: "org"},
			wantConfig:   config.GitSourceConfigYAML{Name: "renamed-docs", Branch: "main", Layer: "org"},
			wantRecloned: false,
		},
		{
			name:         "url change re-clones",
			body:         map[string]any{"new_url": otherURL, "name": "moved-docs"},
			wantLive:     knowledge.GitSourceInfo{Name: "moved-docs", URL: otherURL, Branch: "main", Layer: "project"},
			wantConfig:   config.GitSourceConfigYAML{Name: "moved-docs", URL: otherURL, Branch: "main", Layer: "project"},
			wantRecloned: true,
			wantCloneArg: otherURL,
		},
		{
			name:         "branch change re-clones in the same directory",
			body:         map[string]any{"branch": "release"},
			wantLive:     knowledge.GitSourceInfo{Name: "docs", Branch: "release", Layer: "project"},
			wantConfig:   config.GitSourceConfigYAML{Name: "docs", Branch: "release", Layer: "project"},
			wantRecloned: true,
			wantCloneArg: "--branch release",
		},
		{
			name:         "subpath change re-clones",
			body:         map[string]any{"new_subpath": "docs"},
			wantLive:     knowledge.GitSourceInfo{Name: "docs", Branch: "main", Subpath: "docs", Layer: "project"},
			wantConfig:   config.GitSourceConfigYAML{Name: "docs", Branch: "main", Subpath: "docs", Layer: "project"},
			wantRecloned: true,
			wantCloneArg: "--sparse",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logPath := installHermeticGit(t)
			s, deps, url := gitSourceTestServer(t)
			if rec := doPost(s, "/api/knowledge/git-sources", map[string]any{"name": "docs", "url": url, "branch": "main"}); rec.Code != http.StatusOK {
				t.Fatalf("seed connect: got %d (body %q)", rec.Code, rec.Body.String())
			}
			before, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}

			body := map[string]any{"url": url}
			for k, v := range tc.body {
				body[k] = v
			}
			rec := doPut(s, "/api/knowledge/git-sources", body)
			if rec.Code != http.StatusOK {
				t.Fatalf("update: expected 200, got %d (body %q)", rec.Code, rec.Body.String())
			}
			var resp struct {
				Recloned bool `json:"recloned"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if resp.Recloned != tc.wantRecloned {
				t.Fatalf("recloned = %v, want %v", resp.Recloned, tc.wantRecloned)
			}

			after, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			newCalls := strings.TrimPrefix(string(after), string(before))
			if cloned := strings.Contains(newCalls, " clone "); cloned != tc.wantRecloned {
				t.Fatalf("clone during update = %v, want %v; git calls:\n%s", cloned, tc.wantRecloned, newCalls)
			}
			if tc.wantCloneArg != "" && !strings.Contains(newCalls, tc.wantCloneArg) {
				t.Fatalf("re-clone did not use %q; git calls:\n%s", tc.wantCloneArg, newCalls)
			}

			wantLive := tc.wantLive
			if wantLive.URL == "" {
				wantLive.URL = url
			}
			live := deps.Knowledge.GitSources()
			if len(live) != 1 {
				t.Fatalf("live git sources = %+v, want exactly one", live)
			}
			got := live[0]
			if got.Name != wantLive.Name || got.URL != wantLive.URL || got.Branch != wantLive.Branch ||
				got.Subpath != wantLive.Subpath || got.Layer != wantLive.Layer || !got.Ready {
				t.Fatalf("live git source = %+v, want %+v (ready)", got, wantLive)
			}

			wantConfig := tc.wantConfig
			if wantConfig.URL == "" {
				wantConfig.URL = url
			}
			if cfg := deps.Config.Knowledge.GitSources; len(cfg) != 1 || cfg[0] != wantConfig {
				t.Fatalf("persisted git sources = %+v, want [%+v]", cfg, wantConfig)
			}
			persisted, err := os.ReadFile(deps.Config.SourcePath)
			if err != nil {
				t.Fatalf("read persisted config: %v", err)
			}
			if !strings.Contains(string(persisted), wantConfig.Name) {
				t.Fatalf("persisted config does not contain %q:\n%s", wantConfig.Name, persisted)
			}

			if n := len(s.audit.RecentWithPrefixSince(time.Now().Add(-time.Minute), "knowledge_git_source_update")); n != 1 {
				t.Fatalf("knowledge_git_source_update audit rows = %d, want 1", n)
			}
		})
	}
}

// TestGitSourcesUpdateRecloneFailureKeepsOriginal pins that a failed re-clone
// leaves the original source live and its config entry untouched, so a bad
// edit never costs the operator the source they already had.
func TestGitSourcesUpdateRecloneFailureKeepsOriginal(t *testing.T) {
	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{"new url", map[string]any{"new_url": "http://127.0.0.1/broken.git"}},
		{"same directory branch change", map[string]any{"branch": "broken"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installHermeticGit(t)
			s, deps, url := gitSourceTestServer(t)
			if rec := doPost(s, "/api/knowledge/git-sources", map[string]any{"name": "docs", "url": url}); rec.Code != http.StatusOK {
				t.Fatalf("seed connect: got %d (body %q)", rec.Code, rec.Body.String())
			}
			before := append([]config.GitSourceConfigYAML(nil), deps.Config.Knowledge.GitSources...)

			t.Setenv("HIVE_FAKE_GIT_CLONE_EXIT", "1")
			body := map[string]any{"url": url}
			for k, v := range tc.body {
				body[k] = v
			}
			rec := doPut(s, "/api/knowledge/git-sources", body)
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("failed re-clone: expected 500, got %d (body %q)", rec.Code, rec.Body.String())
			}
			live := deps.Knowledge.GitSources()
			if len(live) != 1 || live[0].URL != url || live[0].Name != "docs" || live[0].Branch != "main" {
				t.Fatalf("failed re-clone changed live sources: %+v", live)
			}
			if got := deps.Config.Knowledge.GitSources; len(got) != len(before) || got[0] != before[0] {
				t.Fatalf("failed re-clone changed config: %+v, want %+v", got, before)
			}
			if n := len(s.audit.RecentWithPrefixSince(time.Now().Add(-time.Minute), "knowledge_git_source_update")); n != 0 {
				t.Fatalf("failed update wrote %d audit rows, want 0", n)
			}
		})
	}
}

func TestGitSourcesUpdateRejectsUnknownAndConflicting(t *testing.T) {
	installHermeticGit(t)
	s, _, url := gitSourceTestServer(t)
	for _, body := range []map[string]any{
		{"name": "root-docs", "url": url},
		{"name": "nested-docs", "url": url, "subpath": "docs"},
	} {
		if rec := doPost(s, "/api/knowledge/git-sources", body); rec.Code != http.StatusOK {
			t.Fatalf("seed connect %v: got %d (body %q)", body, rec.Code, rec.Body.String())
		}
	}

	tests := []struct {
		name string
		body map[string]any
		want int
	}{
		{"missing url", map[string]any{"name": "x"}, http.StatusBadRequest},
		{"unknown source", map[string]any{"url": "http://127.0.0.1/none.git", "name": "x"}, http.StatusNotFound},
		{"invalid name", map[string]any{"url": url, "name": "a/b"}, http.StatusBadRequest},
		{"invalid branch", map[string]any{"url": url, "branch": "-evil"}, http.StatusBadRequest},
		{"invalid subpath", map[string]any{"url": url, "new_subpath": "../etc"}, http.StatusBadRequest},
		{"identity taken by another source", map[string]any{"url": url, "new_subpath": "docs"}, http.StatusConflict},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if rec := doPut(s, "/api/knowledge/git-sources", tc.body); rec.Code != tc.want {
				t.Fatalf("expected %d, got %d (body %q)", tc.want, rec.Code, rec.Body.String())
			}
		})
	}
}
