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

	hgithub "github.com/hivecommons/hive/pkg/github"
)

const testHumanMergePattern = ".github/workflows/**"

// humanPathFront fronts a lane fixture: it serves the PR files list and the
// hold label / comment endpoints for PR 7 itself and proxies everything else
// to the fixture, so each lane's existing fake keeps driving the merge path.
type humanPathFront struct {
	mu          sync.Mutex
	files       []string
	filesStatus int
	filesHits   int
	labels      []string
	comments    []string
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
			var files []map[string]any
			for _, name := range f.files {
				files = append(files, map[string]any{"filename": name, "status": "modified", "additions": 1})
			}
			json.NewEncoder(w).Encode(files)
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
