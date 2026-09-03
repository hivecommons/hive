package hub

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func resetCommitBehindState(t *testing.T) {
	t.Helper()
	commitBehindMu.Lock()
	origFetch := fetchCommitBehindCount
	commitBehindCache = map[commitBehindKey]commitBehindValue{}
	commitBehindInFlight = map[commitBehindKey]bool{}
	commitBehindMu.Unlock()
	latestSHAMu.Lock()
	oldLatest, hadLatest := latestSHAByBranch[stableReleaseBranch]
	latestSHAByBranch[stableReleaseBranch] = branchSHAInfo{SHA: "head999"}
	latestSHAMu.Unlock()
	t.Cleanup(func() {
		commitBehindMu.Lock()
		fetchCommitBehindCount = origFetch
		commitBehindCache = map[commitBehindKey]commitBehindValue{}
		commitBehindInFlight = map[commitBehindKey]bool{}
		commitBehindMu.Unlock()
		latestSHAMu.Lock()
		if hadLatest {
			latestSHAByBranch[stableReleaseBranch] = oldLatest
		} else {
			delete(latestSHAByBranch, stableReleaseBranch)
		}
		latestSHAMu.Unlock()
	})
}

func TestResolveCommitBehindCachesKnownAndUnknown(t *testing.T) {
	resetCommitBehindState(t)
	key := commitBehindKey{base: "base111", head: "head999"}
	resolveCommitBehind(key, func(base, head string, logger *slog.Logger) (int, bool, error) {
		return 12, true, nil
	}, nil)
	if got, known := commitsBehindStableV4("base111", nil); !known || got != 12 {
		t.Fatalf("known cached count = %d,%v; want 12,true", got, known)
	}

	unknownKey := commitBehindKey{base: "fork123", head: "head999"}
	resolveCommitBehind(unknownKey, func(base, head string, logger *slog.Logger) (int, bool, error) {
		return 0, false, nil
	}, nil)
	if _, known := commitsBehindStableV4("fork123", nil); known {
		t.Fatal("unknown compare result must stay unknown")
	}
}

func TestResolveCommitBehindErrorNotCached(t *testing.T) {
	resetCommitBehindState(t)
	key := commitBehindKey{base: "base111", head: "head999"}
	resolveCommitBehind(key, func(base, head string, logger *slog.Logger) (int, bool, error) {
		return 0, false, errors.New("rate limited")
	}, nil)
	commitBehindMu.Lock()
	_, cached := commitBehindCache[key]
	inFlight := commitBehindInFlight[key]
	commitBehindMu.Unlock()
	if cached {
		t.Fatal("transient compare errors must not poison the cache")
	}
	if inFlight {
		t.Fatal("failed compare must clear in-flight")
	}
}

func TestFetchCommitBehindCountHTTP(t *testing.T) {
	oldBase := githubAPIBase
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/hivecommons/hive/compare/base111...head999" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"ahead_by":7}`))
	}))
	defer ts.Close()
	githubAPIBase = ts.URL
	t.Cleanup(func() { githubAPIBase = oldBase })

	got, known, err := fetchCommitBehindCount("base111", "head999", nil)
	if err != nil || !known || got != 7 {
		t.Fatalf("fetch count = %d,%v,%v; want 7,true,nil", got, known, err)
	}
}

func TestHandleMyHivesIncludesCommitsBehind(t *testing.T) {
	resetCommitBehindState(t)
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	saveSaaSUser(&SaaSUser{GitHubUsername: "alice", Hives: map[string]string{"h1": "owner"}})
	saveSaaSHive(&SaaSHive{ID: "h1", Owner: "alice", Org: "acme", Status: "running"})
	commitBehindMu.Lock()
	commitBehindCache[commitBehindKey{base: "base111", head: "head999"}] = commitBehindValue{count: 3, known: true}
	commitBehindMu.Unlock()

	s := &HubServer{logger: slog.Default(), hubSecret: testHubSecret}
	s.registry.Hives = []RegistryEntry{{ID: "h1", Owner: "alice", Org: "acme", GitHash: "base111", Online: true}}

	rec := httptest.NewRecorder()
	req := reqWithUser(http.MethodGet, "/api/saas/my-hives", "", "alice")
	s.handleMyHives(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("my-hives status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Hives []struct {
			CommitsBehindStableV4 *int `json:"commitsBehindStableV4"`
		} `json:"hives"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal my-hives: %v", err)
	}
	if len(resp.Hives) != 1 || resp.Hives[0].CommitsBehindStableV4 == nil || *resp.Hives[0].CommitsBehindStableV4 != 3 {
		t.Fatalf("commitsBehindStableV4 response = %+v, want 3", resp.Hives)
	}
}
