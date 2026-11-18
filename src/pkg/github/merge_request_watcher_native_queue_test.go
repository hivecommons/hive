package github

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

// nativeQueueMergeServer serves a green PR o/r#74 at head abc whose direct
// merge GitHub refuses with the merge-queue-required 405, counting PUT
// /merge calls.
func nativeQueueMergeServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var puts atomic.Int32
	green := greenFixture()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if green.serveCI(w, r) {
			return
		}
		if r.Method == "PUT" && strings.HasSuffix(r.URL.Path, "/merge") {
			puts.Add(1)
			w.WriteHeader(http.StatusMethodNotAllowed)
			_, _ = io.WriteString(w, `{"message":"Repository rule violations found\n\nChanges must be made through the merge queue\n\n"}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv, &puts
}

// projectbluefin/utah-packages#1199 and #1201, 2026-11-18: the relay sent
// PUT /merge on a repo opted into auto_merge.native_merge_queue, GitHub
// refused it with 405, and the hive told the operator to remove the
// merge-queue rule. An opted-in repo now goes through the lanes' native
// queue path; a repo that is not opted in keeps the quarantine, with a
// banner that no longer suggests weakening branch protection.
func TestMergeRequestWatcher_NativeQueueOptIn(t *testing.T) {
	queuedReason := "native-merge-queued: queued (position 1, AWAITING_CHECKS)"
	for _, tc := range []struct {
		name        string
		result      NativeQueueMergeResult
		err         error
		wantPuts    int32
		wantAlert   bool
		wantFile    string // suffix the request file ends with; "" = removed
		wantAttempt int
		wantError   string
	}{
		{
			name:     "opted in with a queue enqueues",
			result:   NativeQueueMergeResult{OptedIn: true, Queued: true, Reason: queuedReason},
			wantFile: "", wantAttempt: 1, wantError: mergeRequestEnqueuedPrefix + queuedReason,
		},
		{
			name:     "not opted in keeps the quarantine and banner",
			result:   NativeQueueMergeResult{},
			wantPuts: 1, wantAlert: true, wantFile: ".exhausted", wantAttempt: 1, wantError: mergeQueueRequiredReason,
		},
		{
			// The probe read the base as queue-less (stale or failed), so the
			// relay tried the direct merge. The lanes still own an opted-in
			// repo: quarantine the request, raise no banner.
			name:     "opted in but probe saw no queue raises no banner",
			result:   NativeQueueMergeResult{OptedIn: true},
			wantPuts: 1, wantFile: ".exhausted", wantAttempt: 1, wantError: mergeQueueRequiredReason,
		},
		{
			// Unchanged handling of a moved head: a failed attempt the
			// watcher retries, and no direct merge.
			name:     "head moved is a failed attempt",
			result:   NativeQueueMergeResult{OptedIn: true, Reason: "head-changed-since-eval"},
			wantFile: ".json", wantAttempt: 1, wantError: "head-changed-since-eval",
		},
		{
			name:     "queue read error is a failed attempt",
			result:   NativeQueueMergeResult{OptedIn: true, Reason: "native-merge-queue-read"},
			err:      errors.New("graphql unavailable"),
			wantFile: ".json", wantAttempt: 1, wantError: "native-merge-queue-read: graphql unavailable",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, puts := nativeQueueMergeServer(t)
			c := testMergeClient(t, srv.URL)
			sink := &recordingMergeAlertSink{}
			c.SetMergeFailureAlertSink(sink)
			var calls int
			var gotRepo, gotSHA string
			var gotNumber int
			c.SetNativeQueueMerge(func(_ context.Context, repo string, pr *gh.PullRequest, expectSHA string) (NativeQueueMergeResult, error) {
				calls++
				gotRepo, gotNumber, gotSHA = repo, pr.GetNumber(), expectSHA
				return tc.result, tc.err
			})
			dir := t.TempDir()
			mergeRequestDirForTest = dir
			defer func() { mergeRequestDirForTest = "" }()

			reqPath, _ := WriteMergeRequest(dir, MergeRequest{Repo: "o/r", Number: 74, ExpectSHA: "abc", Agent: "scanner"})
			c.ProcessMergeRequestsOnce(context.Background())

			if calls != 1 || gotRepo != "o/r" || gotNumber != 74 || gotSHA != "abc" {
				t.Fatalf("native queue hook calls=%d repo=%q number=%d sha=%q, want one call for o/r#74 at abc", calls, gotRepo, gotNumber, gotSHA)
			}
			if got := puts.Load(); got != tc.wantPuts {
				t.Fatalf("PUT /merge calls = %d, want %d", got, tc.wantPuts)
			}
			if tc.wantAlert != (len(sink.adds) == 1) || len(sink.adds) > 1 {
				t.Fatalf("operator alerts = %+v, want alert=%v", sink.adds, tc.wantAlert)
			}
			if tc.wantAlert {
				msg := sink.adds[0].message
				if !strings.Contains(msg, "auto_merge.native_merge_queue") || strings.Contains(msg, "settings/rules") || strings.Contains(strings.ToLower(msg), "remove the merge-queue rule") {
					t.Fatalf("banner = %q, want the opt-in advice and no advice to remove the rule", msg)
				}
			}
			switch tc.wantFile {
			case "":
				if _, err := os.Stat(reqPath); !os.IsNotExist(err) {
					t.Fatalf("request file should be removed once queued: %v", err)
				}
			case ".json":
				if _, err := os.Stat(reqPath); err != nil {
					t.Fatalf("request file should stay for a retry: %v", err)
				}
			default:
				if _, err := os.Stat(reqPath + tc.wantFile); err != nil {
					t.Fatalf("request file should be set aside as %s: %v", tc.wantFile, err)
				}
			}
			resp := readMergeResult(t, reqPath)
			if resp.OK || resp.Attempts != tc.wantAttempt || !strings.Contains(resp.Error, tc.wantError) {
				t.Fatalf("result = %+v, want ok=false attempts=%d error containing %q", resp, tc.wantAttempt, tc.wantError)
			}
		})
	}
}

// Without the hook the relay merges directly, as before.
func TestMergeRequestWatcher_NoNativeQueueHookMergesDirectly(t *testing.T) {
	srv, puts := nativeQueueMergeServer(t)
	c := testMergeClient(t, srv.URL)
	dir := t.TempDir()
	mergeRequestDirForTest = dir
	defer func() { mergeRequestDirForTest = "" }()
	reqPath, _ := WriteMergeRequest(dir, MergeRequest{Repo: "o/r", Number: 74, ExpectSHA: "abc", Agent: "scanner"})
	c.ProcessMergeRequestsOnce(context.Background())
	if got := puts.Load(); got != 1 {
		t.Fatalf("PUT /merge calls = %d, want 1", got)
	}
	if _, err := os.Stat(reqPath + ".exhausted"); err != nil {
		t.Fatalf("merge-queue-required refusal should quarantine: %v", err)
	}
}
