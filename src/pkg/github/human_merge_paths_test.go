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
	fileDetails []map[string]any
	prPayload   map[string]any
	movedHead   string
	// commits is served for GET pulls/42/commits; nil serves one verified
	// web-flow commit by alice at the head "abc" (what the pin-only exception
	// needs). upstream maps "/repos/<owner>/<repo><suffix>" to a payload (or
	// an int status) for the action-pin anchoring reads.
	commits     []map[string]any
	upstream    map[string]any
	ci          *ciFixture
	prStatus    int
	filesStatus int
	filesHits   int
	merges      int
	labels      []string
	comments    []string
}

func newHumanPathMergeServer(t *testing.T, f *humanPathMergeFixture) *httptest.Server {
	t.Helper()
	green := f.ci
	if green == nil {
		green = greenFixture()
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		p := r.URL.Path
		switch {
		case r.Method == http.MethodGet && p == "/repos/o/r/pulls/42" && f.prStatus != 0:
			w.WriteHeader(f.prStatus)
			return
		case r.Method == http.MethodGet && p == "/repos/o/r/pulls/42" && f.prPayload != nil:
			if f.movedHead != "" && f.filesHits > 0 {
				f.prPayload["head"] = map[string]any{"sha": f.movedHead}
			}
			_ = json.NewEncoder(w).Encode(f.prPayload)
			return
		case r.Method == http.MethodGet && p == "/repos/o/r/pulls/42/commits":
			commits := f.commits
			if commits == nil {
				commits = []map[string]any{pinOnlyTestCommit("abc", "alice", "web-flow", true)}
			}
			_ = json.NewEncoder(w).Encode(commits)
			return
		case r.Method == http.MethodGet && p == "/repos/o/r/pulls/42/files":
			f.filesHits++
			if f.filesStatus != 0 {
				w.WriteHeader(f.filesStatus)
				return
			}
			out := f.fileDetails
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
		if r.Method == http.MethodGet && f.upstream != nil {
			for suffix, payload := range f.upstream {
				if p != "/repos/upstream/action"+suffix {
					continue
				}
				if status, ok := payload.(int); ok {
					w.WriteHeader(status)
					return
				}
				_ = json.NewEncoder(w).Encode(payload)
				return
			}
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

func runHumanPathMergeRequest(t *testing.T, f *humanPathMergeFixture, paths map[string][]string, pinOnly ...map[string]PinOnlySpec) (string, MergeResponse) {
	t.Helper()
	srv := newHumanPathMergeServer(t, f)
	t.Cleanup(srv.Close)
	c := testMergeClient(t, srv.URL)
	c.SetHumanMergePaths(paths)
	if len(pinOnly) > 0 {
		c.SetHumanMergePinOnly(pinOnly[0])
	}

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

// pinOnlyTestCommit builds a pulls/N/commits entry with the author login,
// committer login and GitHub signature verification the exception inspects.
func pinOnlyTestCommit(sha, author, committer string, verified bool) map[string]any {
	return map[string]any{
		"sha":       sha,
		"author":    map[string]any{"login": author},
		"committer": map[string]any{"login": committer},
		"commit":    map[string]any{"verification": map[string]any{"verified": verified}},
	}
}

const (
	pinOnlyTestOldSHA = "1111111111111111111111111111111111111111"
	pinOnlyTestNewSHA = "2222222222222222222222222222222222222222"
)

// pinOnlyActionPatch is a single-line uses: SHA bump of upstream/action with a
// # v5 version comment.
func pinOnlyActionPatch(version string) string {
	suffix := ""
	if version != "" {
		suffix = " # " + version
	}
	return "@@ -1 +1 @@\n-      - uses: upstream/action@" + pinOnlyTestOldSHA + suffix + "\n+      - uses: upstream/action@" + pinOnlyTestNewSHA + suffix
}

func pinOnlyRelayFixture() *humanPathMergeFixture {
	return &humanPathMergeFixture{
		prPayload:   map[string]any{"number": 42, "state": "open", "user": map[string]any{"login": "alice", "type": "User"}, "head": map[string]any{"sha": "abc", "repo": map[string]any{"full_name": "o/r"}}, "base": map[string]any{"ref": "main", "repo": map[string]any{"full_name": "o/r"}}, "changed_files": 1},
		fileDetails: []map[string]any{{"filename": "Containerfile", "status": "modified", "additions": 1, "deletions": 1, "patch": "@@ -1 +1 @@\n-FROM image@sha256:" + strings.Repeat("a", 64) + "\n+FROM image@sha256:" + strings.Repeat("b", 64)}},
	}
}

func TestMergeRequestPinOnlyException(t *testing.T) {
	for _, tc := range []struct {
		name        string
		change      func(*humanPathMergeFixture, *PinOnlySpec)
		wantPass    bool
		wantUnknown bool
	}{
		{name: "trusted pin", wantPass: true},
		{name: "disabled", change: func(f *humanPathMergeFixture, ex *PinOnlySpec) { ex.Enabled = false }},
		{name: "untrusted", change: func(f *humanPathMergeFixture, ex *PinOnlySpec) { ex.TrustedAuthors = []string{"bob"} }},
		{name: "empty trust list", change: func(f *humanPathMergeFixture, ex *PinOnlySpec) { ex.TrustedAuthors = nil }},
		{name: "mixed diff", change: func(f *humanPathMergeFixture, ex *PinOnlySpec) {
			f.fileDetails[0]["patch"] = strings.Replace(f.fileDetails[0]["patch"].(string), "@@ -1 +1 @@", "@@ -1 +1,2 @@", 1) + "\n+RUN echo unsafe"
			f.fileDetails[0]["additions"] = 2
		}},
		{name: "unguarded extra file", change: func(f *humanPathMergeFixture, ex *PinOnlySpec) {
			f.prPayload["changed_files"] = 2
			f.files = []string{"README.md"}
		}},
		{name: "outside exception paths", change: func(f *humanPathMergeFixture, ex *PinOnlySpec) { ex.Paths = []string{".github/workflows/*.yml"} }},
		{name: "missing patch", change: func(f *humanPathMergeFixture, ex *PinOnlySpec) { delete(f.fileDetails[0], "patch") }},
		{name: "missing count", change: func(f *humanPathMergeFixture, ex *PinOnlySpec) { delete(f.prPayload, "changed_files") }},
		{name: "incomplete list", wantUnknown: true, change: func(f *humanPathMergeFixture, ex *PinOnlySpec) { f.prPayload["changed_files"] = 2 }},
		{name: "wrong head", change: func(f *humanPathMergeFixture, ex *PinOnlySpec) { f.prPayload["head"] = map[string]any{"sha": "other"} }},
		{name: "head moves during file fetch", change: func(f *humanPathMergeFixture, ex *PinOnlySpec) { f.movedHead = "other" }},
		{name: "commit by untrusted author", change: func(f *humanPathMergeFixture, ex *PinOnlySpec) {
			f.commits = []map[string]any{pinOnlyTestCommit("abc", "mallory", "web-flow", true)}
		}},
		{name: "commit by untrusted committer", change: func(f *humanPathMergeFixture, ex *PinOnlySpec) {
			f.commits = []map[string]any{pinOnlyTestCommit("abc", "alice", "mallory", true)}
		}},
		{name: "unverified commit", change: func(f *humanPathMergeFixture, ex *PinOnlySpec) {
			f.commits = []map[string]any{pinOnlyTestCommit("abc", "alice", "web-flow", false)}
		}},
		{name: "later untrusted commit", change: func(f *humanPathMergeFixture, ex *PinOnlySpec) {
			f.commits = []map[string]any{pinOnlyTestCommit("aaa", "alice", "web-flow", true), pinOnlyTestCommit("abc", "mallory", "mallory", true)}
		}},
		{name: "commit list not at head", change: func(f *humanPathMergeFixture, ex *PinOnlySpec) {
			f.commits = []map[string]any{pinOnlyTestCommit("stale", "alice", "web-flow", true)}
		}},
		{name: "empty commit list", change: func(f *humanPathMergeFixture, ex *PinOnlySpec) {
			f.commits = []map[string]any{}
		}},
		{name: "trusted author signing locally", wantPass: true, change: func(f *humanPathMergeFixture, ex *PinOnlySpec) {
			f.commits = []map[string]any{pinOnlyTestCommit("abc", "alice", "alice", true)}
		}},
		{name: "action pin anchored by upstream tag", wantPass: true, change: func(f *humanPathMergeFixture, ex *PinOnlySpec) {
			pinOnlyActionCase(f, ex, "v5", map[string]any{"/git/ref/tags/v5": map[string]any{"object": map[string]any{"type": "commit", "sha": pinOnlyTestNewSHA}}})
		}},
		{name: "action pin tag points elsewhere", change: func(f *humanPathMergeFixture, ex *PinOnlySpec) {
			pinOnlyActionCase(f, ex, "v5", map[string]any{"/git/ref/tags/v5": map[string]any{"object": map[string]any{"type": "commit", "sha": pinOnlyTestOldSHA}}})
		}},
		{name: "action pin tag missing upstream", change: func(f *humanPathMergeFixture, ex *PinOnlySpec) {
			pinOnlyActionCase(f, ex, "v5", map[string]any{"/git/ref/tags/v5": http.StatusNotFound})
		}},
		{name: "action pin on upstream default branch", wantPass: true, change: func(f *humanPathMergeFixture, ex *PinOnlySpec) {
			pinOnlyActionCase(f, ex, "", map[string]any{"": map[string]any{"default_branch": "main"}, "/compare/" + pinOnlyTestNewSHA + "...main": map[string]any{"status": "ahead"}})
		}},
		{name: "action pin off upstream default branch", change: func(f *humanPathMergeFixture, ex *PinOnlySpec) {
			pinOnlyActionCase(f, ex, "", map[string]any{"": map[string]any{"default_branch": "main"}, "/compare/" + pinOnlyTestNewSHA + "...main": map[string]any{"status": "diverged"}})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := pinOnlyRelayFixture()
			ex := PinOnlySpec{Enabled: true, Paths: []string{"Containerfile"}, Kinds: []string{"image-digest"}, TrustedAuthors: []string{"ALICE"}}
			if tc.change != nil {
				tc.change(f, &ex)
			}
			_, resp := runHumanPathMergeRequest(t, f, map[string][]string{"o/r": {"Containerfile", ".github/workflows/*.yml"}}, map[string]PinOnlySpec{"O/R": ex})
			if tc.wantPass {
				if !resp.OK || f.merges != 1 || len(f.labels) != 0 || len(f.comments) != 0 {
					t.Fatalf("result=%+v merges=%d labels=%v comments=%v; want merge without hold", resp, f.merges, f.labels, f.comments)
				}
			} else {
				if resp.OK || f.merges != 0 {
					t.Fatalf("result=%+v merges=%d; want refusal", resp, f.merges)
				}
				if tc.wantUnknown {
					if !strings.Contains(resp.Error, "fail closed") || len(f.labels) != 0 || len(f.comments) != 0 {
						t.Fatalf("unknown list must fail closed without a hold: %+v", resp)
					}
				} else if len(f.labels) != 1 || len(f.comments) != 1 || !strings.Contains(f.comments[0], HumanMergePathMarker) {
					t.Fatalf("labels=%v comments=%v; want hold and marker", f.labels, f.comments)
				}
			}
		})
	}
}

// pinOnlyActionCase turns the fixture into a workflow action-sha bump of
// upstream/action and serves upstream for the anchoring reads.
func pinOnlyActionCase(f *humanPathMergeFixture, ex *PinOnlySpec, version string, upstream map[string]any) {
	f.fileDetails = []map[string]any{{"filename": ".github/workflows/ci.yml", "status": "modified", "additions": 1, "deletions": 1, "patch": pinOnlyActionPatch(version)}}
	f.upstream = upstream
	ex.Paths = []string{".github/workflows/*.yml"}
	ex.Kinds = []string{"action-sha"}
}

func TestMergeRequestPinOnlyStillRequiresGreenCI(t *testing.T) {
	f := pinOnlyRelayFixture()
	f.ci = greenFixture()
	f.ci.checks[0].conclusion = "failure"
	_, resp := runHumanPathMergeRequest(t, f, map[string][]string{"o/r": {"Containerfile"}}, map[string]PinOnlySpec{"r": {Enabled: true, Paths: []string{"Containerfile"}, Kinds: []string{"image-digest"}, TrustedAuthors: []string{"alice"}}})
	if resp.OK || f.merges != 0 || len(f.labels) != 0 || len(f.comments) != 0 {
		t.Fatalf("result=%+v merges=%d labels=%v comments=%v; pin exception must still fail CI", resp, f.merges, f.labels, f.comments)
	}
}

func TestSetHumanMergePinOnlyReload(t *testing.T) {
	c := NewClientForTest("http://127.0.0.1:0", "o", []string{"r"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ex := PinOnlySpec{Enabled: true, Paths: []string{"Containerfile"}, Kinds: []string{"image-digest"}, TrustedAuthors: []string{"alice"}}
	c.SetHumanMergePinOnly(map[string]PinOnlySpec{" O/R ": ex})
	ex.Paths[0] = "unsafe"
	got := c.humanMergePinOnlyFor("r")
	if !got.Enabled || got.Paths[0] != "Containerfile" {
		t.Fatalf("policy not normalized or copied: %+v", got)
	}
	got.Paths[0] = "unsafe"
	if c.humanMergePinOnlyFor("r").Paths[0] != "Containerfile" {
		t.Fatal("lookup must return a copy")
	}
	c.SetHumanMergePinOnly(map[string]PinOnlySpec{"r": ex, "O/R": ex})
	if c.humanMergePinOnlyFor("r").Enabled {
		t.Fatal("duplicate normalized keys must fail closed")
	}
	c.SetHumanMergePinOnly(nil)
	if c.humanMergePinOnlyFor("r").Enabled {
		t.Fatal("nil must clear policy")
	}
	var absent *Client
	absent.SetHumanMergePinOnly(nil)
	if absent.humanMergePinOnlyFor("r").Enabled {
		t.Fatal("nil client must have no policy")
	}
}

func TestMergeRequestPinOnlyRequiresExpectedHead(t *testing.T) {
	f := pinOnlyRelayFixture()
	srv := newHumanPathMergeServer(t, f)
	defer srv.Close()
	c := testMergeClient(t, srv.URL)
	c.SetHumanMergePaths(map[string][]string{"o/r": {"Containerfile"}})
	c.SetHumanMergePinOnly(map[string]PinOnlySpec{"o/r": {Enabled: true, Paths: []string{"Containerfile"}, Kinds: []string{"image-digest"}, TrustedAuthors: []string{"alice"}}})
	refusal, err := c.mergeRequestHumanMergePathRefusal(context.Background(), MergeRequest{Repo: "o/r", Number: 42})
	if err != nil || refusal == "" || len(f.labels) != 1 || len(f.comments) != 1 {
		t.Fatalf("refusal=%q err=%v labels=%v comments=%v; missing requested head must hold", refusal, err, f.labels, f.comments)
	}
}
