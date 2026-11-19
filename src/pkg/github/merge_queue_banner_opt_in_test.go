package github

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// On a repo opted into auto_merge.native_merge_queue the lanes enqueue the PR
// themselves, so a relayed direct merge refused with 405 "Changes must be made
// through the merge queue" is expected: the request is still set aside on the
// first refusal, but no operator banner is raised. Live case: knuckle,
// projectbluefin/utah-packages#1210, #972, #1217, #1218 and #1215 on
// 2026-11-18/19, each merged by the queue while the banner stayed up.
func TestMergeRequestWatcher_MergeQueueRequiredOptedInRaisesNoBanner(t *testing.T) {
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
	defer srv.Close()
	c := testMergeClient(t, srv.URL)
	var logs strings.Builder
	c.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	var asked []string
	c.SetNativeQueueOptedIn(func(repo string) bool { asked = append(asked, repo); return repo == "o/r" })
	sink := &recordingMergeAlertSink{}
	c.SetMergeFailureAlertSink(sink)

	dir := t.TempDir()
	mergeRequestDirForTest = dir
	defer func() { mergeRequestDirForTest = "" }()

	reqPath, _ := WriteMergeRequest(dir, MergeRequest{Repo: "o/r", Number: 74, ExpectSHA: "abc", Agent: "scanner"})
	c.ProcessMergeRequestsOnce(context.Background())

	if got := puts.Load(); got != 1 {
		t.Fatalf("PUT /merge calls = %d, want 1", got)
	}
	if _, err := os.Stat(reqPath + ".exhausted"); err != nil {
		t.Fatalf("merge-queue-required refusal should still quarantine on the first attempt: %v", err)
	}
	if len(sink.adds) != 0 {
		t.Fatalf("opted-in repo raised a merge-queue-required banner: %+v", sink.adds)
	}
	if len(asked) != 1 || asked[0] != "o/r" {
		t.Fatalf("opt-in check asked for %v, want [o/r]", asked)
	}
	if !strings.Contains(logs.String(), "native_queue_opted_in=true") {
		t.Fatalf("log line should record the opt-in:\n%s", logs.String())
	}
}

// A merge-queue-required banner raised before the repo was opted in retires on
// the next revalidation once the repo is opted in, while the rule still applies.
func TestRevalidateMergeFailureAlerts_MergeQueueRequiredClearsOnceRepoOptsIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/pulls/"):
			_, _ = io.WriteString(w, `{"number":7,"state":"open","head":{"sha":"abc"},"base":{"ref":"main"}}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/rules/branches/main"):
			_, _ = io.WriteString(w, `[{"type":"merge_queue","parameters":{}}]`)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	c := testMergeClient(t, srv.URL)
	var optedIn atomic.Bool
	c.SetNativeQueueOptedIn(func(repo string) bool { return optedIn.Load() && repo == "o/r" })
	sink := &recordingMergeAlertSink{}
	c.SetMergeFailureAlertSink(sink)
	c.raiseMergeFailureAlert("o/r", 7, mergeQueueRequiredErr)
	if len(sink.adds) != 1 {
		t.Fatalf("expected 1 alert, got %d", len(sink.adds))
	}

	now := time.Date(2026, 11, 19, 6, 0, 0, 0, time.UTC)
	nowFn := func() time.Time { return now }
	c.revalidateMergeFailureAlerts(context.Background(), nowFn)
	if len(sink.clears) != 0 {
		t.Fatalf("alert cleared before the repo opted in: %v", sink.clears)
	}

	optedIn.Store(true)
	now = now.Add(mergeAlertRevalidateInterval)
	c.revalidateMergeFailureAlerts(context.Background(), nowFn)
	if len(sink.clears) != 1 || sink.clears[0] != sink.adds[0].id {
		t.Fatalf("expected merge-queue-required alert cleared once opted in, got %v", sink.clears)
	}
}

// The banner never tells the operator to remove the merge-queue rule.
func TestMergeQueueRequiredBannerDoesNotSuggestRemovingTheRule(t *testing.T) {
	alert, ok := classifyMergeFailureForOperator("o/r", mergeQueueRequiredErr)
	if !ok {
		t.Fatal("merge-queue-required refusal raised no alert")
	}
	for _, banned := range []string{"remove the merge-queue rule", "settings/rules"} {
		if strings.Contains(alert.message, banned) {
			t.Errorf("banner %q still contains %q", alert.message, banned)
		}
	}
}
