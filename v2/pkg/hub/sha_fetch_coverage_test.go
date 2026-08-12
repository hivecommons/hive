package hub

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
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
		{"in progress", `{"workflow_runs":[{"status":"in_progress"}]}`, 200, imageStatusBuilding},
		{"completed success", `{"workflow_runs":[{"status":"completed","conclusion":"success"}]}`, 200, imageStatusBuilding},
		{"completed failure", `{"workflow_runs":[{"status":"completed","conclusion":"failure"}]}`, 200, imageStatusFailed},
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

// TestBuildStartedAtLifecycle pins the elapsed-build-timer backend: the start
// time is stamped when a SHA first enters "building", preserved while the same
// SHA keeps building across polls, and cleared on ready/failed or a new SHA.
func TestBuildStartedAtLifecycle(t *testing.T) {
	resetSHACaches(t)

	// First poll sees the head building → start time stamped, exposed.
	setBranchHead("v2", "sha1", "msg", imageStatusBuilding)
	h := getBranchHead("v2")
	if h.BuildStartedAt.IsZero() {
		t.Fatal("building head should have a BuildStartedAt")
	}
	first := h.BuildStartedAt
	if starts := getImageBuildStartTimes(); starts["v2"] != first.UnixMilli() {
		t.Errorf("getImageBuildStartTimes[v2] = %d, want %d", starts["v2"], first.UnixMilli())
	}

	// Second poll, SAME SHA still building → start time is preserved, not reset.
	setBranchHead("v2", "sha1", "msg", imageStatusBuilding)
	if got := getBranchHead("v2").BuildStartedAt; !got.Equal(first) {
		t.Errorf("BuildStartedAt reset on same-SHA re-poll: got %v want %v", got, first)
	}

	// Image finishes → start time cleared, dropped from the exposed map.
	setBranchHead("v2", "sha1", "msg", imageStatusReady)
	if got := getBranchHead("v2").BuildStartedAt; !got.IsZero() {
		t.Errorf("BuildStartedAt should clear when ready, got %v", got)
	}
	if _, ok := getImageBuildStartTimes()["v2"]; ok {
		t.Error("a ready branch must not appear in getImageBuildStartTimes")
	}

	// A brand-new SHA building → a fresh start time, not the old one.
	setBranchHead("v2", "sha2", "msg", imageStatusBuilding)
	if got := getBranchHead("v2").BuildStartedAt; got.IsZero() || got.Equal(first) {
		t.Errorf("new building SHA should get a fresh start time, got %v (first was %v)", got, first)
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
