package github

// Tests for the merge-request relay's human-merge-path gate (#11039). Every
// refusal asserts zero PUT .../merge calls, not merely a refusal reason.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

type humanPathMergeFixture struct {
	mu          sync.Mutex
	files       []string
	prStatus    int
	filesStatus int
	filesHits   int
	merges      int
	labels      []string
	comments    []string
}

func newHumanPathMergeServer(t *testing.T, f *humanPathMergeFixture) *httptest.Server {
	t.Helper()
	green := greenFixture()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		p := r.URL.Path
		switch {
		case r.Method == http.MethodGet && p == "/repos/o/r/pulls/42" && f.prStatus != 0:
			w.WriteHeader(f.prStatus)
			return
		case r.Method == http.MethodGet && p == "/repos/o/r/pulls/42/files":
			f.filesHits++
			if f.filesStatus != 0 {
				w.WriteHeader(f.filesStatus)
				return
			}
			var out []map[string]any
			for _, name := range f.files {
				out = append(out, map[string]any{"filename": name, "status": "modified"})
			}
			_ = json.NewEncoder(w).Encode(out)
			return
		case r.Method == http.MethodPost && p == "/repos/o/r/issues/42/labels":
			var labels []string
			_ = json.NewDecoder(r.Body).Decode(&labels)
			f.labels = append(f.labels, labels...)
			_ = json.NewEncoder(w).Encode([]map[string]string{{"name": "hold"}})
			return
		case r.Method == http.MethodGet && p == "/repos/o/r/issues/42/comments":
			var out []map[string]any
			for _, body := range f.comments {
				out = append(out, map[string]any{"body": body})
			}
			_ = json.NewEncoder(w).Encode(out)
			return
		case r.Method == http.MethodPost && p == "/repos/o/r/issues/42/comments":
			var body struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.comments = append(f.comments, body.Body)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": len(f.comments)})
			return
		}
		if green.serveCI(w, r) {
			return
		}
		if r.Method == http.MethodPut && strings.HasSuffix(p, "/merge") {
			f.merges++
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"sha":"deadbeef","merged":true,"message":"Pull Request successfully merged"}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
}

func runHumanPathMergeRequest(t *testing.T, f *humanPathMergeFixture, paths map[string][]string) (string, MergeResponse) {
	t.Helper()
	srv := newHumanPathMergeServer(t, f)
	t.Cleanup(srv.Close)
	c := testMergeClient(t, srv.URL)
	c.SetHumanMergePaths(paths)

	dir := t.TempDir()
	mergeRequestDirForTest = dir
	t.Cleanup(func() { mergeRequestDirForTest = "" })
	reqPath, err := WriteMergeRequest(dir, MergeRequest{Repo: "o/r", Number: 42, Agent: "scanner", ExpectSHA: "abc", UpdateBranch: true})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessMergeRequestsOnce(context.Background())
	return reqPath, readMergeResult(t, reqPath)
}

func TestMergeRequestHumanMergePathMatchRefusedTerminally(t *testing.T) {
	f := &humanPathMergeFixture{files: []string{"src/main.go", ".claude/settings.json"}}
	reqPath, resp := runHumanPathMergeRequest(t, f, map[string][]string{"O/R": {".claude/settings.json", ".claude/hooks/**"}})

	if f.merges != 0 {
		t.Fatalf("merges = %d, want 0: a human-merge path must never be merged by the relay", f.merges)
	}
	if resp.OK || !strings.Contains(resp.Error, ".claude/settings.json") || !strings.Contains(resp.Error, HumanMergePathsConfigKey) {
		t.Fatalf("result = %+v, want refusal naming the path and %s", resp, HumanMergePathsConfigKey)
	}
	if strings.Contains(resp.Error, "src/main.go") {
		t.Fatalf("refusal names a non-matching path: %q", resp.Error)
	}
	if _, err := os.Stat(reqPath + ".denied"); err != nil {
		t.Fatalf("request should be terminally refused (.denied): %v", err)
	}
	if len(f.labels) != 1 || f.labels[0] != HumanMergePathHoldLabel {
		t.Fatalf("labels = %v, want [hold]", f.labels)
	}
	if len(f.comments) != 1 || !strings.Contains(f.comments[0], HumanMergePathMarker) || !strings.Contains(f.comments[0], ".claude/settings.json") {
		t.Fatalf("comments = %q, want one marker comment naming the path", f.comments)
	}
}

func TestMergeRequestHumanMergePathExistingMarkerNotReposted(t *testing.T) {
	f := &humanPathMergeFixture{files: []string{".claude/settings.json"}, comments: []string{HumanMergePathMarker + "\nearlier"}}
	_, resp := runHumanPathMergeRequest(t, f, map[string][]string{"o/r": {".claude/settings.json"}})
	if resp.OK || f.merges != 0 {
		t.Fatalf("result = %+v, merges = %d; want refusal", resp, f.merges)
	}
	if len(f.comments) != 1 {
		t.Fatalf("comments = %d, want the existing marker comment only", len(f.comments))
	}
}

func TestMergeRequestHumanMergePathNoMatchOrUnconfiguredMerges(t *testing.T) {
	for _, tc := range []struct {
		name  string
		paths map[string][]string
		hits  int
	}{
		{name: "unconfigured", paths: nil, hits: 0},
		{name: "other repo configured", paths: map[string][]string{"o/other": {".claude/**"}}, hits: 0},
		{name: "configured without match", paths: map[string][]string{"r": {"docs/**"}}, hits: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &humanPathMergeFixture{files: []string{".claude/settings.json"}}
			reqPath, resp := runHumanPathMergeRequest(t, f, tc.paths)
			if !resp.OK || f.merges != 1 {
				t.Fatalf("result = %+v, merges = %d; want one merge", resp, f.merges)
			}
			if _, err := os.Stat(reqPath); !os.IsNotExist(err) {
				t.Fatalf("merged request should be consumed")
			}
			if f.filesHits != tc.hits || len(f.labels) != 0 || len(f.comments) != 0 {
				t.Fatalf("files hits = %d (want %d), labels = %v, comments = %d; want no hold", f.filesHits, tc.hits, f.labels, len(f.comments))
			}
		})
	}
}

func TestMergeRequestHumanMergePathFetchErrorFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    *humanPathMergeFixture
	}{
		{name: "files API error", f: &humanPathMergeFixture{filesStatus: http.StatusInternalServerError}},
		{name: "PR fetch error", f: &humanPathMergeFixture{prStatus: http.StatusInternalServerError}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reqPath, resp := runHumanPathMergeRequest(t, tc.f, map[string][]string{"o/r": {".claude/**"}})
			if resp.OK || tc.f.merges != 0 {
				t.Fatalf("result = %+v, merges = %d; want refusal without merge", resp, tc.f.merges)
			}
			if !strings.Contains(resp.Error, HumanMergePathsConfigKey) || !strings.Contains(resp.Error, "fail closed") {
				t.Fatalf("error = %q, want fail-closed reason naming %s", resp.Error, HumanMergePathsConfigKey)
			}
			if resp.Attempts != 1 {
				t.Fatalf("attempts = %d, want 1 (bounded retry path)", resp.Attempts)
			}
			if _, err := os.Stat(reqPath); err != nil {
				t.Fatalf("request should stay queued for a bounded retry: %v", err)
			}
			if len(tc.f.labels) != 0 || len(tc.f.comments) != 0 {
				t.Fatalf("labels = %v, comments = %d; an unknown file list must not hold", tc.f.labels, len(tc.f.comments))
			}
		})
	}
}

func TestSetHumanMergePathsNormalizesKeys(t *testing.T) {
	c := NewClientForTest("http://127.0.0.1:0", "o", []string{"r"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.SetHumanMergePaths(map[string][]string{"r": {"a/**"}, "O/R": {"b"}, " ": {"c"}, "o/empty": nil})
	got := c.humanMergePathsFor("o/r")
	if len(got) != 2 {
		t.Fatalf("paths for o/r = %v, want bare and owner-qualified keys merged", got)
	}
	if c.humanMergePathsFor("o/empty") != nil || c.humanMergePathsFor("o/missing") != nil {
		t.Fatalf("empty or unknown repos must have no paths")
	}
	c.SetHumanMergePaths(nil)
	if c.humanMergePathsFor("o/r") != nil {
		t.Fatalf("nil must clear paths on reload")
	}
	var nilClient *Client
	nilClient.SetHumanMergePaths(map[string][]string{"o/r": {"x"}})
	if nilClient.humanMergePathsFor("o/r") != nil {
		t.Fatalf("nil client must report no paths")
	}
}

func TestHumanMergePathHelpersGuardNilInputs(t *testing.T) {
	ctx := context.Background()
	if err := HoldForHumanMergePaths(ctx, nil, "o", "r", 1, []string{"x"}); err == nil {
		t.Fatal("HoldForHumanMergePaths with nil client: err = nil, want error")
	}
	if _, err := ListPRChangedFiles(ctx, nil, "o", "r", nil); err == nil {
		t.Fatal("ListPRChangedFiles with nil inputs: err = nil, want error")
	}
	reason := HumanMergePathReason([]string{"a", "b"})
	if !strings.Contains(reason, "`a`, `b`") || !strings.Contains(reason, HumanMergePathsConfigKey) {
		t.Fatalf("reason = %q", reason)
	}
}
