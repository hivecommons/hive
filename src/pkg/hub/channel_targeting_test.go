package hub

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func targetingLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func resetChannelRevisionCache(t *testing.T) {
	t.Helper()
	channelRevisionMu.Lock()
	channelRevisionCache = map[string]channelRevisionEntry{}
	channelRevisionMu.Unlock()
}

// stubChannelRevisions points ghcrTagRevision at a fixed channel→SHA table and
// clears the cache on both sides, so a resolved answer cannot leak between
// tests. Returns a counter of how many times the (uncached) resolver ran.
func stubChannelRevisions(t *testing.T, byTag map[string]string) *int32 {
	t.Helper()
	resetChannelRevisionCache(t)
	var calls int32
	orig := ghcrTagRevision
	ghcrTagRevision = func(repo, tag string, _ *slog.Logger) string {
		atomic.AddInt32(&calls, 1)
		if repo != ghcrRepoSpoke {
			t.Errorf("channel revision must read the SPOKE repo, got %q", repo)
		}
		return byTag[tag]
	}
	t.Cleanup(func() {
		ghcrTagRevision = orig
		resetChannelRevisionCache(t)
	})
	return &calls
}

// stubBranchHead seeds the image-verified latest-SHA cache for one branch.
func stubBranchHead(t *testing.T, branch, sha string) {
	t.Helper()
	latestSHAMu.Lock()
	saved := latestSHAByBranch
	latestSHAByBranch = map[string]branchSHAInfo{branch: {SHA: sha}}
	latestSHAMu.Unlock()
	t.Cleanup(func() {
		latestSHAMu.Lock()
		latestSHAByBranch = saved
		latestSHAMu.Unlock()
	})
}

func TestSpokeReleaseChannel(t *testing.T) {
	cases := []struct {
		name     string
		imageRef string
		tracked  string
		want     string
	}{
		{"channel tag", "ghcr.io/hivecommons/hive:stable", "", "stable"},
		{"candidate tag", "ghcr.io/hivecommons/hive:candidate", "", "candidate"},
		{"branch moving tag is not a channel", "ghcr.io/hivecommons/hive:v4-latest", "", ""},
		{"sha pin is not a channel", "ghcr.io/hivecommons/hive:526ef71", "", ""},
		{"digest pin is not a channel", "ghcr.io/hivecommons/hive@sha256:abc", "", ""},
		// The REPORTED tag leads. A hive whose tracked_channel says "stable"
		// while its Deployment is still on v4-latest has a switch in flight;
		// targeting stable there aims at a channel it is not on yet.
		{"reported tag wins over intent", "ghcr.io/hivecommons/hive:v4-latest", "stable", ""},
		{"reported channel wins over a different intent", "ghcr.io/hivecommons/hive:candidate", "stable", "candidate"},
		// Spokes too old to report an image ref fall back to intent.
		{"no image ref falls back to tracked channel", "", "stable", "stable"},
		{"no image ref, no channel", "", "", ""},
		{"no image ref, tracked branch is not a channel", "", "v4", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := spokeReleaseChannel(tc.imageRef, tc.tracked); got != tc.want {
				t.Errorf("spokeReleaseChannel(%q, %q) = %q, want %q",
					tc.imageRef, tc.tracked, got, tc.want)
			}
		})
	}
}

// TestReachableUpgradeTargetPrefersChannelOverBranchHead is the regression test
// for #5994: a spoke tracking :stable must be targeted at the commit :stable
// carries, never at the newer branch head the soak policy is withholding.
func TestReachableUpgradeTargetPrefersChannelOverBranchHead(t *testing.T) {
	stubBranchHead(t, "v4", "526ef71")
	stubChannelRevisions(t, map[string]string{"stable": "77ba848"})
	s := &HubServer{logger: targetingLogger()}

	got := s.reachableUpgradeTarget("v4", "ghcr.io/hivecommons/hive:stable", "stable")
	if !got.Resolved {
		t.Fatalf("channel resolved to a commit but Resolved is false: %+v", got)
	}
	if got.SHA != "77ba848" {
		t.Errorf("target = %q, want the stable channel's commit 77ba848 (branch head is 526ef71)", got.SHA)
	}
	if got.Channel != ReleaseChannelStable {
		t.Errorf("Channel = %q, want %q", got.Channel, ReleaseChannelStable)
	}
}

// A branch-tracking spoke must be unaffected: branch head is exactly what its
// moving tag delivers.
func TestReachableUpgradeTargetBranchSpokeUsesBranchHead(t *testing.T) {
	stubBranchHead(t, "v4", "526ef71")
	calls := stubChannelRevisions(t, map[string]string{"stable": "77ba848"})
	s := &HubServer{logger: targetingLogger()}

	got := s.reachableUpgradeTarget("v4", "ghcr.io/hivecommons/hive:v4-latest", "")
	if !got.Resolved || got.SHA != "526ef71" || got.Channel != "" {
		t.Errorf("reachableUpgradeTarget = %+v, want {SHA:526ef71 Channel: Resolved:true}", got)
	}
	if n := atomic.LoadInt32(calls); n != 0 {
		t.Errorf("a branch-tracking spoke must not cost a registry round-trip, got %d", n)
	}
}

// An unresolvable channel must SUPPRESS the instruction, not fall back to
// branch head — the fallback is the bug.
func TestReachableUpgradeTargetUnresolvedChannelSuppresses(t *testing.T) {
	stubBranchHead(t, "v4", "526ef71")
	stubChannelRevisions(t, map[string]string{}) // nothing resolves
	s := &HubServer{logger: targetingLogger()}

	got := s.reachableUpgradeTarget("v4", "ghcr.io/hivecommons/hive:stable", "stable")
	if got.Resolved {
		t.Errorf("unresolvable channel reported Resolved: %+v", got)
	}
	if got.SHA != "" {
		t.Errorf("SHA = %q, want empty — branch head must never stand in for a channel", got.SHA)
	}
	if got.Channel != ReleaseChannelStable {
		t.Errorf("Channel = %q, want %q so the refusal can name the channel", got.Channel, ReleaseChannelStable)
	}
}

func TestChannelRevisionSHACaches(t *testing.T) {
	calls := stubChannelRevisions(t, map[string]string{"stable": "77ba848"})
	log := targetingLogger()

	for i := 0; i < 3; i++ {
		if got := channelRevisionSHA(ReleaseChannelStable, log); got != "77ba848" {
			t.Fatalf("call %d = %q, want 77ba848", i, got)
		}
	}
	if n := atomic.LoadInt32(calls); n != 1 {
		t.Errorf("resolver ran %d times, want 1 — the answer must be cached for channelDigestTTL", n)
	}
}

// A failed refresh serves the last good answer while it is young, and stops
// serving it once it is older than the grace.
func TestChannelRevisionSHAServesStaleWithinGrace(t *testing.T) {
	stubChannelRevisions(t, map[string]string{}) // every refresh fails
	log := targetingLogger()

	channelRevisionMu.Lock()
	channelRevisionCache[ReleaseChannelStable] = channelRevisionEntry{
		sha: "77ba848",
		at:  time.Now().Add(-(channelDigestTTL + time.Minute)),
	}
	channelRevisionMu.Unlock()
	if got := channelRevisionSHA(ReleaseChannelStable, log); got != "77ba848" {
		t.Errorf("within grace: got %q, want the previous answer 77ba848", got)
	}

	channelRevisionMu.Lock()
	channelRevisionCache[ReleaseChannelStable] = channelRevisionEntry{
		sha: "77ba848",
		at:  time.Now().Add(-(channelRevisionStaleGrace + time.Minute)),
	}
	channelRevisionMu.Unlock()
	if got := channelRevisionSHA(ReleaseChannelStable, log); got != "" {
		t.Errorf("past grace: got %q, want \"\" so the caller suppresses instead of guessing", got)
	}
}

// A failed refresh must not overwrite the cache with the failure: the next call
// has to retry rather than latch "unknown" for a whole TTL.
func TestChannelRevisionSHARetriesAfterFailure(t *testing.T) {
	resetChannelRevisionCache(t)
	var calls int32
	answer := ""
	orig := ghcrTagRevision
	ghcrTagRevision = func(_, _ string, _ *slog.Logger) string {
		atomic.AddInt32(&calls, 1)
		return answer
	}
	t.Cleanup(func() {
		ghcrTagRevision = orig
		resetChannelRevisionCache(t)
	})
	log := targetingLogger()

	if got := channelRevisionSHA(ReleaseChannelStable, log); got != "" {
		t.Fatalf("cold failure = %q, want empty", got)
	}
	answer = "77ba848"
	if got := channelRevisionSHA(ReleaseChannelStable, log); got != "77ba848" {
		t.Errorf("retry after failure = %q, want 77ba848", got)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Errorf("resolver ran %d times, want 2 — a failure must not be cached", n)
	}
}

// ghcrTagRevision must walk index → linux/amd64 manifest → config blob, and
// must not mistake buildx's unknown/unknown attestation descriptor for the
// image.
func TestGhcrTagRevisionWalksIndexToRevisionLabel(t *testing.T) {
	const (
		amd64Digest = "sha256:amd64manifest"
		attDigest   = "sha256:attestation"
		cfgDigest   = "sha256:config"
		fullSHA     = "77ba848f0e1d2c3b4a596877ba848f0e1d2c3b4a"
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/token":
			_, _ = io.WriteString(w, `{"token":"t"}`)
		case r.URL.Path == "/v2/"+ghcrRepoSpoke+"/manifests/stable":
			_, _ = io.WriteString(w, `{"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[
			  {"digest":"`+attDigest+`","platform":{"architecture":"unknown","os":"unknown"}},
			  {"digest":"`+amd64Digest+`","platform":{"architecture":"amd64","os":"linux"}}]}`)
		case r.URL.Path == "/v2/"+ghcrRepoSpoke+"/manifests/"+amd64Digest:
			_, _ = io.WriteString(w, `{"config":{"digest":"`+cfgDigest+`"}}`)
		case r.URL.Path == "/v2/"+ghcrRepoSpoke+"/blobs/"+cfgDigest:
			_, _ = io.WriteString(w, `{"config":{"Labels":{"`+revisionLabel+`":"`+fullSHA+`"}}}`)
		default:
			t.Errorf("unexpected registry request: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	savedBase := ghcrBase
	ghcrBase = srv.URL
	t.Cleanup(func() { ghcrBase = savedBase })

	got := ghcrTagRevision(ghcrRepoSpoke, ReleaseChannelStable, targetingLogger())
	if got != shortSHA(fullSHA) {
		t.Errorf("ghcrTagRevision = %q, want %q", got, shortSHA(fullSHA))
	}
}

// A single-platform manifest (no index) must resolve without the extra hop.
func TestGhcrTagRevisionHandlesSinglePlatformManifest(t *testing.T) {
	const cfgDigest = "sha256:config"
	const fullSHA = "526ef71f0e1d2c3b4a5968526ef71f0e1d2c3b4a"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			_, _ = io.WriteString(w, `{"token":"t"}`)
		case strings.HasSuffix(r.URL.Path, "/manifests/candidate"):
			_, _ = io.WriteString(w, `{"config":{"digest":"`+cfgDigest+`"}}`)
		case strings.HasSuffix(r.URL.Path, "/blobs/"+cfgDigest):
			_, _ = io.WriteString(w, `{"config":{"Labels":{"`+revisionLabel+`":"`+fullSHA+`"}}}`)
		default:
			t.Errorf("unexpected registry request: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	savedBase := ghcrBase
	ghcrBase = srv.URL
	t.Cleanup(func() { ghcrBase = savedBase })

	if got := ghcrTagRevision(ghcrRepoSpoke, ReleaseChannelCandidate, targetingLogger()); got != shortSHA(fullSHA) {
		t.Errorf("ghcrTagRevision = %q, want %q", got, shortSHA(fullSHA))
	}
}

// An image published without the revision label must resolve to "", so the
// caller suppresses rather than inventing a target.
func TestGhcrTagRevisionMissingLabelResolvesEmpty(t *testing.T) {
	const cfgDigest = "sha256:config"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			_, _ = io.WriteString(w, `{"token":"t"}`)
		case strings.HasSuffix(r.URL.Path, "/manifests/stable"):
			_, _ = io.WriteString(w, `{"config":{"digest":"`+cfgDigest+`"}}`)
		default:
			_, _ = io.WriteString(w, `{"config":{"Labels":{"org.opencontainers.image.title":"hive"}}}`)
		}
	}))
	t.Cleanup(srv.Close)

	savedBase := ghcrBase
	ghcrBase = srv.URL
	t.Cleanup(func() { ghcrBase = savedBase })

	if got := ghcrTagRevision(ghcrRepoSpoke, ReleaseChannelStable, targetingLogger()); got != "" {
		t.Errorf("ghcrTagRevision = %q, want \"\" when the revision label is absent", got)
	}
}

func TestGhcrManifestBodyFailureModes(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"non-OK", http.StatusServiceUnavailable, `{"config":{"digest":"sha256:config"}}`},
		{"invalid JSON", http.StatusOK, `<html>not a manifest</html>`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v2/"+ghcrRepoSpoke+"/manifests/stable" {
					t.Errorf("unexpected registry request: %s", r.URL.Path)
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			t.Cleanup(srv.Close)

			savedBase := ghcrBase
			ghcrBase = srv.URL
			t.Cleanup(func() { ghcrBase = savedBase })

			if got := ghcrManifestBody(srv.Client(), "t", ghcrRepoSpoke, ReleaseChannelStable, targetingLogger()); got != nil {
				t.Errorf("ghcrManifestBody() = %+v, want nil", got)
			}
		})
	}
}

func TestGhcrTagRevisionBlobFailureModes(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"blob non-OK", http.StatusForbidden, `{}`},
		{"blob invalid JSON", http.StatusOK, `<html>not a config</html>`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const cfgDigest = "sha256:config"
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/token":
					_, _ = io.WriteString(w, `{"token":"t"}`)
				case strings.HasSuffix(r.URL.Path, "/manifests/stable"):
					_, _ = io.WriteString(w, `{"config":{"digest":"`+cfgDigest+`"}}`)
				case strings.HasSuffix(r.URL.Path, "/blobs/"+cfgDigest):
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, tc.body)
				default:
					t.Errorf("unexpected registry request: %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			t.Cleanup(srv.Close)

			savedBase := ghcrBase
			ghcrBase = srv.URL
			t.Cleanup(func() { ghcrBase = savedBase })

			if got := ghcrTagRevision(ghcrRepoSpoke, ReleaseChannelStable, targetingLogger()); got != "" {
				t.Errorf("ghcrTagRevision = %q, want \"\" when config blob cannot be read", got)
			}
		})
	}
}

// ============================================================
// The heartbeat path, end to end
// ============================================================

func postChannelHeartbeat(t *testing.T, srv *HubServer, payload string) HeartbeatResponse {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/heartbeat", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("heartbeat: got %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp HeartbeatResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("heartbeat: decoding response: %v", err)
	}
	return resp
}

// The live failure from #5994: a spoke-managed hive whose Deployment tracks
// :stable was answered with branch HEAD, a commit its tag withholds, on every
// beat forever. It must now be told to reach what :stable carries.
func TestHeartbeatTargetsTheSpokesChannelNotBranchHead(t *testing.T) {
	resetCommitOrderState(t)
	stubBranchHead(t, "v2", "526ef71")
	stubChannelRevisions(t, map[string]string{"stable": "77ba848"})
	srv := newHubServerForTest(t)
	srv.setHubSecret("")

	resp := postChannelHeartbeat(t, srv,
		`{"hive_id":"chan-spoke","git_hash":"old1111","git_branch":"v2","auto_upgrade":true,`+
			`"image_ref":"ghcr.io/hivecommons/hive:stable"}`)

	if resp.UpgradeTo != "77ba848" {
		t.Errorf("upgrade_to = %q, want the stable channel's commit 77ba848, not branch head 526ef71", resp.UpgradeTo)
	}
	// latest_sha still answers "how far has the branch moved" — the behind-count
	// the dashboard renders is a different question from what a spoke can reach.
	if resp.LatestSHA != "526ef71" {
		t.Errorf("latest_sha = %q, want the branch head 526ef71", resp.LatestSHA)
	}
}

// A spoke already at its channel's commit is up to date, however far the branch
// has run ahead. This is the state the 41 alerting spokes were actually in.
func TestHeartbeatNoUpgradeWhenSpokeIsAtItsChannel(t *testing.T) {
	resetCommitOrderState(t)
	stubBranchHead(t, "v2", "526ef71")
	stubChannelRevisions(t, map[string]string{"stable": "77ba848"})
	srv := newHubServerForTest(t)
	srv.setHubSecret("")

	resp := postChannelHeartbeat(t, srv,
		`{"hive_id":"at-chan","git_hash":"77ba848","git_branch":"v2","auto_upgrade":true,`+
			`"image_ref":"ghcr.io/hivecommons/hive:stable"}`)

	if resp.UpgradeTo != "" {
		t.Errorf("upgrade_to = %q, want empty — the spoke is at everything its tag offers", resp.UpgradeTo)
	}
}

// An unresolvable channel must produce silence, not a branch-head fallback.
func TestHeartbeatWithholdsWhenChannelDoesNotResolve(t *testing.T) {
	resetCommitOrderState(t)
	stubBranchHead(t, "v2", "526ef71")
	stubChannelRevisions(t, map[string]string{}) // stable resolves to nothing
	srv := newHubServerForTest(t)
	srv.setHubSecret("")

	resp := postChannelHeartbeat(t, srv,
		`{"hive_id":"unres-chan","git_hash":"old1111","git_branch":"v2","auto_upgrade":true,`+
			`"image_ref":"ghcr.io/hivecommons/hive:stable"}`)

	if resp.UpgradeTo != "" {
		t.Errorf("upgrade_to = %q, want empty — branch head must never stand in for an unresolved channel", resp.UpgradeTo)
	}
}

// A spoke AHEAD of its channel must not be handed the channel's older commit:
// that is a downgrade instruction, a failure mode the branch-head path never had.
func TestHeartbeatNoDowngradeWhenSpokeIsAheadOfItsChannel(t *testing.T) {
	resetCommitOrderState(t)
	stubBranchHead(t, "v2", "526ef71")
	stubChannelRevisions(t, map[string]string{"stable": "77ba848"})
	seedCommitOrder("ahead11", "77ba848", true)
	srv := newHubServerForTest(t)
	srv.setHubSecret("")

	resp := postChannelHeartbeat(t, srv,
		`{"hive_id":"ahead-chan","git_hash":"ahead11","git_branch":"v2","auto_upgrade":true,`+
			`"image_ref":"ghcr.io/hivecommons/hive:stable"}`)

	if resp.UpgradeTo != "" {
		t.Errorf("upgrade_to = %q, want empty — instructing the channel commit here is a downgrade", resp.UpgradeTo)
	}
}

// A branch-tracking spoke is untouched by all of the above: branch head is
// exactly what its moving tag delivers.
func TestHeartbeatBranchTrackingSpokeStillGetsBranchHead(t *testing.T) {
	resetCommitOrderState(t)
	stubBranchHead(t, "v2", "526ef71")
	stubChannelRevisions(t, map[string]string{"stable": "77ba848"})
	srv := newHubServerForTest(t)
	srv.setHubSecret("")

	resp := postChannelHeartbeat(t, srv,
		`{"hive_id":"branch-spoke","git_hash":"old1111","git_branch":"v2","auto_upgrade":true,`+
			`"image_ref":"ghcr.io/hivecommons/hive:v2-latest"}`)

	if resp.UpgradeTo != "526ef71" {
		t.Errorf("upgrade_to = %q, want the branch head 526ef71", resp.UpgradeTo)
	}
}

// A spoke that reports no image ref at all (an older build) keeps the historical
// branch-head behaviour — there is nothing to resolve a channel from.
func TestHeartbeatSpokeWithoutImageRefKeepsBranchHead(t *testing.T) {
	resetCommitOrderState(t)
	stubBranchHead(t, "v2", "526ef71")
	stubChannelRevisions(t, map[string]string{"stable": "77ba848"})
	srv := newHubServerForTest(t)
	srv.setHubSecret("")

	resp := postChannelHeartbeat(t, srv,
		`{"hive_id":"no-ref-spoke","git_hash":"old1111","git_branch":"v2","auto_upgrade":true}`)

	if resp.UpgradeTo != "526ef71" {
		t.Errorf("upgrade_to = %q, want the branch head 526ef71", resp.UpgradeTo)
	}
}

// A registry that does not carry the tag at all, and an index with no
// linux/amd64 image in it, must both resolve to "" rather than to a guess.
func TestGhcrTagRevisionFailsClosed(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"tag absent", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/token" {
				_, _ = io.WriteString(w, `{"token":"t"}`)
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}},
		{"index carries only an attestation", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/token" {
				_, _ = io.WriteString(w, `{"token":"t"}`)
				return
			}
			_, _ = io.WriteString(w, `{"manifests":[{"digest":"sha256:att","platform":{"architecture":"unknown","os":"unknown"}}]}`)
		}},
		{"manifest has no config descriptor", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/token" {
				_, _ = io.WriteString(w, `{"token":"t"}`)
				return
			}
			_, _ = io.WriteString(w, `{"config":{}}`)
		}},
		{"token response is not JSON", func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `<html>rate limited</html>`)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			t.Cleanup(srv.Close)
			savedBase := ghcrBase
			ghcrBase = srv.URL
			t.Cleanup(func() { ghcrBase = savedBase })

			if got := ghcrTagRevision(ghcrRepoSpoke, ReleaseChannelStable, targetingLogger()); got != "" {
				t.Errorf("ghcrTagRevision = %q, want \"\" — an unresolvable channel must never produce a target", got)
			}
		})
	}
}
