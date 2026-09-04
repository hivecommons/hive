package hub

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// ============================================================
// saas.go — GitHub/GHCR SHA-poll fetch helpers
//
// These functions build hardcoded api.github.com / ghcr.io URLs but perform
// the request through the *http.Client passed to them. We give them a client
// whose RoundTripper rewrites every request to a local httptest server, so no
// real network call is made and every branch is deterministically reachable.
// ============================================================

// rewriteTransport routes all outbound requests to target (a local test server).
type rewriteTransport struct {
	target *url.URL
}

func (rt *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = rt.target.Scheme
	req.URL.Host = rt.target.Host
	return http.DefaultTransport.RoundTrip(req)
}

// clientTo returns an *http.Client whose requests all hit srv, regardless of
// the hostname the caller encoded in the URL.
func clientTo(t *testing.T, srv *httptest.Server) *http.Client {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	return &http.Client{Transport: &rewriteTransport{target: u}}
}

func TestFetchImageBuildStatus(t *testing.T) {
	cases := []struct {
		name, respBody string
		status         int
		want           string
	}{
		{"no runs", `{"workflow_runs":[]}`, 200, imageStatusBuilding},
		{"queued", `{"workflow_runs":[{"status":"queued","html_url":"https://example.test/r/1"}]}`, 200, imageStatusBuilding},
		{"in progress", `{"workflow_runs":[{"status":"in_progress","html_url":"https://example.test/r/1"}]}`, 200, imageStatusBuilding},
		{"completed success", `{"workflow_runs":[{"status":"completed","conclusion":"success","html_url":"https://example.test/r/1"}]}`, 200, imageStatusBuilding},
		{"completed failure", `{"workflow_runs":[{"status":"completed","conclusion":"failure","html_url":"https://example.test/r/1"}]}`, 200, imageStatusFailed},
		{"completed cancelled", `{"workflow_runs":[{"status":"completed","conclusion":"cancelled","html_url":"https://example.test/r/1"}]}`, 200, imageStatusFailed},
		{"completed skipped", `{"workflow_runs":[{"status":"completed","conclusion":"skipped","html_url":"https://example.test/r/1"}]}`, 200, imageStatusFailed},
		{"completed timed out", `{"workflow_runs":[{"status":"completed","conclusion":"timed_out","html_url":"https://example.test/r/1"}]}`, 200, imageStatusFailed},
		{"completed action required", `{"workflow_runs":[{"status":"completed","conclusion":"action_required","html_url":"https://example.test/r/1"}]}`, 200, imageStatusFailed},
		{"completed neutral", `{"workflow_runs":[{"status":"completed","conclusion":"neutral","html_url":"https://example.test/r/1"}]}`, 200, imageStatusFailed},
		{"non-200", ``, 500, ""},
		{"bad json", `{not json`, 200, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.respBody))
			}))
			defer srv.Close()
			got := fetchImageBuildStatus(clientTo(t, srv), "abc123def456", slog.Default())
			if got != tc.want {
				t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

func TestFetchImageBuildStateRunURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"workflow_runs":[{"status":"completed","conclusion":"failure","html_url":"https://github.com/hivecommons/hive/actions/runs/1"}]}`))
	}))
	defer srv.Close()
	got := fetchImageBuildState(clientTo(t, srv), "abc123def456", slog.Default())
	if got.Status != imageStatusFailed {
		t.Fatalf("Status = %q, want %q", got.Status, imageStatusFailed)
	}
	if got.RunURL != "https://github.com/hivecommons/hive/actions/runs/1" {
		t.Fatalf("RunURL = %q", got.RunURL)
	}
}

func TestBuildStatusWithStaleness(t *testing.T) {
	t.Setenv(imageBuildStaleAfterEnv, "30m")
	now := time.Now()
	if got := buildStatusWithStaleness(imageStatusBuilding, now.Add(-29*time.Minute), now); got != imageStatusBuilding {
		t.Fatalf("fresh building status = %q, want %q", got, imageStatusBuilding)
	}
	if got := buildStatusWithStaleness(imageStatusBuilding, now.Add(-31*time.Minute), now); got != imageStatusStale {
		t.Fatalf("stale building status = %q, want %q", got, imageStatusStale)
	}
	if got := buildStatusWithStaleness(imageStatusFailed, now.Add(-31*time.Minute), now); got != imageStatusFailed {
		t.Fatalf("failed status changed to %q", got)
	}
}

func TestGetImageStatusesAppliesStaleCap(t *testing.T) {
	t.Setenv(imageBuildStaleAfterEnv, "1m")
	resetSHACaches(t)
	defer resetSHACaches(t)
	latestSHAMu.Lock()
	headSHAByBranch["v4"] = branchHeadInfo{SHA: "b586124", ImageStatus: imageStatusBuilding, BuildStartedAt: time.Now().Add(-2 * time.Minute)}
	latestSHAMu.Unlock()
	if got := getImageStatuses()["v4"]; got != imageStatusStale {
		t.Fatalf("getImageStatuses()[v4] = %q, want %q", got, imageStatusStale)
	}
	if _, ok := getImageBuildStartTimes()["v4"]; ok {
		t.Fatalf("stale branch should not expose an active build timer")
	}
}

func TestReadyBranchDoesNotExposeOldBuildURL(t *testing.T) {
	resetSHACaches(t)
	defer resetSHACaches(t)
	setBranchHeadDetails("v4", "b586124", "", imageStatusFailed, "https://github.com/hivecommons/hive/actions/runs/1")
	setBranchHead("v4", "b586124", "", imageStatusReady)
	if got := getImageBuildURLs()["v4"]; got != "" {
		t.Fatalf("ready branch exposed stale build URL %q", got)
	}
}

func TestFetchImageBuildStatusConnError(t *testing.T) {
	// client.Do fails -> "".
	c := &http.Client{Transport: &rewriteTransport{target: mustURL(t, "http://127.0.0.1:1")}}
	if got := fetchImageBuildStatus(c, "abc123", slog.Default()); got != "" {
		t.Errorf("conn error should return empty, got %q", got)
	}
}

func TestFetchCommitMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"commit":{"message":"first line\nsecond line"}}`))
	}))
	defer srv.Close()
	got := fetchCommitMessage(clientTo(t, srv), "abcdef1234567", slog.Default())
	if got != "first line" {
		t.Errorf("fetchCommitMessage = %q, want 'first line'", got)
	}
}

func TestFetchCommitMessageErrors(t *testing.T) {
	// non-200
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	if got := fetchCommitMessage(clientTo(t, srv), "abcdef1234567", slog.Default()); got != "" {
		t.Errorf("non-200 should return empty, got %q", got)
	}

	// conn error
	c := &http.Client{Transport: &rewriteTransport{target: mustURL(t, "http://127.0.0.1:1")}}
	if got := fetchCommitMessage(c, "abcdef1234567", slog.Default()); got != "" {
		t.Errorf("conn error should return empty, got %q", got)
	}
}

func TestGhcrTagExists(t *testing.T) {
	// token then HEAD 200 -> exists.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Write([]byte(`{"token":"anon-token"}`))
	}))
	defer srv.Close()
	if !ghcrTagExists(clientTo(t, srv), ghcrRepoHub, "deadbee", slog.Default()) {
		t.Error("expected tag to exist (HEAD 200)")
	}

	// HEAD 404 -> not exists.
	srv404 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write([]byte(`{"token":"anon-token"}`))
	}))
	defer srv404.Close()
	if ghcrTagExists(clientTo(t, srv404), ghcrRepoHub, "deadbee", slog.Default()) {
		t.Error("expected tag NOT to exist (HEAD 404)")
	}
}

func TestGhcrTagExistsErrors(t *testing.T) {
	// Token request connection error -> false.
	c := &http.Client{Transport: &rewriteTransport{target: mustURL(t, "http://127.0.0.1:1")}}
	if ghcrTagExists(c, ghcrRepoHub, "deadbee", slog.Default()) {
		t.Error("conn error should return false")
	}

	// Bad token JSON -> false.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{not json`))
	}))
	defer srv.Close()
	if ghcrTagExists(clientTo(t, srv), ghcrRepoHub, "deadbee", slog.Default()) {
		t.Error("bad token json should return false")
	}
}

func TestBackfillCommitMessage(t *testing.T) {
	resetSHACaches(t)
	// Seed a cached SHA with no message.
	latestSHAMu.Lock()
	latestSHAByBranch["dev"] = branchSHAInfo{SHA: "abc1234"}
	latestSHAMu.Unlock()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"commit":{"message":"backfilled msg"}}`))
	}))
	defer srv.Close()
	backfillCommitMessage(clientTo(t, srv), "dev", slog.Default())

	latestSHAMu.RLock()
	msg := latestSHAByBranch["dev"].Message
	latestSHAMu.RUnlock()
	if msg != "backfilled msg" {
		t.Errorf("expected backfilled message, got %q", msg)
	}
}

func TestBackfillCommitMessageSkips(t *testing.T) {
	resetSHACaches(t)
	// No cached SHA -> early return (message stays empty, no request made).
	backfillCommitMessage(&http.Client{}, "unknown-branch", slog.Default())

	// Cached SHA already has a message -> early return.
	latestSHAMu.Lock()
	latestSHAByBranch["dev"] = branchSHAInfo{SHA: "abc1234", Message: "already here"}
	latestSHAMu.Unlock()
	backfillCommitMessage(&http.Client{}, "dev", slog.Default())
	latestSHAMu.RLock()
	msg := latestSHAByBranch["dev"].Message
	latestSHAMu.RUnlock()
	if msg != "already here" {
		t.Errorf("message should be unchanged, got %q", msg)
	}
}

func TestGetBranchHead(t *testing.T) {
	resetSHACaches(t)
	setBranchHead("main", "abc1234", "msg", imageStatusReady)
	got := getBranchHead("main")
	if got.SHA != "abc1234" || got.Message != "msg" {
		t.Errorf("getBranchHead = %+v", got)
	}
	// Unknown branch -> zero value.
	if got := getBranchHead("nope"); got.SHA != "" {
		t.Errorf("unknown branch should be zero, got %+v", got)
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}

// resetSHACaches clears the package SHA caches for test isolation.
func resetSHACaches(t *testing.T) {
	t.Helper()
	latestSHAMu.Lock()
	for k := range latestSHAByBranch {
		delete(latestSHAByBranch, k)
	}
	for k := range headSHAByBranch {
		delete(headSHAByBranch, k)
	}
	// The hub-image cache is a separate map from latestSHAByBranch; leaving it
	// populated leaks state into tests that assert on latest_hub_shas.
	for k := range latestHubSHAByBranch {
		delete(latestHubSHAByBranch, k)
	}
	for k := range commitMsgBySHA {
		delete(commitMsgBySHA, k)
	}
	latestSHAMu.Unlock()
}
