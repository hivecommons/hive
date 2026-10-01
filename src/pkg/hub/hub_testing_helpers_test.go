package hub

// hub_testing_helpers_test.go — build a REAL HubServer for a test without
// touching the host's /data and without leaking its save loop.
//
// WHY THIS EXISTS. NewHubServer is not a pure constructor. On every call it:
//
//   - reads hubSecretPath and, when the file is absent and HIVE_HUB_SECRET is
//     unset, GENERATES a master secret and WRITES it there (creating the
//     parent directory);
//   - reads registryPath, hubBannersPath, hubGenerationsPath,
//     clustersConfigPath, reachHistoryPath, timelineDir, journeyStatePath,
//     alertAcksPath and revokedSessionsPath;
//   - starts the debounced saveLoop goroutine, which writes registryPath.
//
// On CI /data does not exist, so every one of those writes fails silently —
// and that silent failure is the only thing that has kept ~30 test files'
// servers from sharing one registry file and one secret. On a hive host (the
// agents run this suite inside the pod) the writes SUCCEED against the real
// /data, and a save loop nobody stopped wakes up registrySaveDelay after its
// test returned and recreates files inside a t.TempDir the framework is
// already removing — the intermittent "TempDir RemoveAll cleanup: directory
// not empty" failure fixed for one file by #4774 and reintroduced by every
// file that never adopted StopSaveLoop.
//
// WHY PER-TEST, NOT ONE SHARED TempDir IN TestMain. Pointing the package path
// vars at a single suite-wide directory would make the writes succeed
// everywhere, which is strictly WORSE than today: test A's server would
// persist its registry, test B's NewHubServer would load it, and B would
// inherit A's hives, banners and (via hubSecretPath) A's master secret. The
// failing /data writes are currently doing the isolation work by accident; a
// shared directory removes the accident without replacing the isolation. The
// unit of isolation has to be the test, so this helper gives each caller its
// own t.TempDir, redirects the path vars only for that test's lifetime, and
// joins the save loop before the directory is removed.

import (
	"log/slog"
	"path/filepath"
	"testing"
)

// hubTestIsolationEnv is a marker variable set through t.Setenv purely so the
// testing package refuses t.Parallel for us: t.Setenv panics if the test is
// already parallel, and t.Parallel panics if t.Setenv was called first. Nothing
// reads the variable. The package path vars are process-global, so two tests
// swapping them concurrently would each construct against the other's
// directory — there is no correct parallel behaviour, only a loud refusal.
const hubTestIsolationEnv = "HIVE_HUB_TEST_ISOLATED"

// Defaults NewHubServer callers in this package have used since the first
// handler test; kept as the helper's defaults so a mechanical migration from
// `NewHubServer(0, slog.Default(), "test", "v2")` changes no assertion.
const (
	hubTestDefaultGitHash   = "test"
	hubTestDefaultGitBranch = "v2"
)

// hubTestPathVar describes one package-level path var that NewHubServer reads
// on construction, and where under the per-test directory it should point.
type hubTestPathVar struct {
	ptr *string
	// prod is the production default, captured at package init before any
	// test can have moved the var.
	prod string
	// rel is the location under the per-test directory, mirroring the /data
	// layout so a test that inspects the directory sees familiar names.
	rel string
	// always redirects the var even when a test has already moved it off its
	// production default. True only for the two paths NewHubServer WRITES on
	// construction (the secret) or from its save loop (the registry): those
	// must be per-test no matter what, and a test that wants a specific
	// registry file says so through withRegistryPath. The read-only ones stay
	// wherever the test put them, because seeding a file at a redirected path
	// BEFORE constructing the server is exactly how tests exercise the load
	// paths (see pull_only_cluster_test.go's clustersConfigPath).
	always bool
}

// hubTestPathVars is every package path var a plain NewHubServer touches.
// Order is irrelevant; the save/restore below is keyed by index.
var hubTestPathVars = []hubTestPathVar{
	{ptr: &registryPath, prod: registryPath, rel: "hub-registry.json", always: true},
	{ptr: &hubSecretPath, prod: hubSecretPath, rel: filepath.Join("saas", "hub-secret.key"), always: true},
	{ptr: &hubBannersPath, prod: hubBannersPath, rel: filepath.Join("saas", "hub-banners.json")},
	{ptr: &stablePromotionPath, prod: stablePromotionPath, rel: filepath.Join("saas", "stable-promotion.json")},
	{ptr: &hubGenerationsPath, prod: hubGenerationsPath, rel: filepath.Join("saas", "hub-generations.json")},
	{ptr: &clustersConfigPath, prod: clustersConfigPath, rel: filepath.Join("saas", "clusters.json")},
	{ptr: &reachHistoryPath, prod: reachHistoryPath, rel: "reach-history.json"},
	{ptr: &timelineDir, prod: timelineDir, rel: filepath.Join("saas", "timeline")},
	{ptr: &journeyStatePath, prod: journeyStatePath, rel: filepath.Join("saas", "journey-state.json")},
	{ptr: &alertAcksPath, prod: alertAcksPath, rel: filepath.Join("saas", "hub-alert-acks.json")},
	{ptr: &revokedSessionsPath, prod: revokedSessionsPath, rel: filepath.Join("saas", "hub-revoked-sessions.json")},
}

// hubTestConfig is the resolved set of NewHubServer arguments plus the one
// path a test may legitimately want to pin.
type hubTestConfig struct {
	port         int
	logger       *slog.Logger
	gitHash      string
	gitBranch    string
	registryPath string
}

// hubTestOption customises newHubServerForTest. Every option corresponds to
// something a pre-existing NewHubServer call site varied, so no migration had
// to change what a test asserts.
type hubTestOption func(*hubTestConfig)

// withHubIdentity sets the gitHash / gitBranch passed to NewHubServer. Note
// NewHubServer itself rewrites an empty or "unknown" branch to "v2".
func withHubIdentity(gitHash, gitBranch string) hubTestOption {
	return func(c *hubTestConfig) {
		c.gitHash = gitHash
		c.gitBranch = gitBranch
	}
}

// withHubLogger replaces the default slog.Default() logger.
func withHubLogger(logger *slog.Logger) hubTestOption {
	return func(c *hubTestConfig) { c.logger = logger }
}

// withHubPort sets the port passed to NewHubServer (the helper never listens).
func withHubPort(port int) hubTestOption {
	return func(c *hubTestConfig) { c.port = port }
}

// withRegistryPath pins registryPath for the duration of the test instead of
// the helper's per-test file. For tests that seed or inspect the registry
// file at a path they chose.
func withRegistryPath(path string) hubTestOption {
	return func(c *hubTestConfig) { c.registryPath = path }
}

// newHubServerForTest constructs a HubServer whose on-disk footprint lives
// entirely under a fresh t.TempDir and whose save loop is joined before that
// directory is removed.
//
// It must not be used from a parallel test (see hubTestIsolationEnv). It may
// be called more than once per test — each call gets its own directory — and
// from a subtest, because the restore is last-in-first-out and the subtest's
// cleanup runs before its parent's.
//
// Cleanup ORDER is the whole point and testing.T runs cleanups in reverse
// registration order, so read the body bottom-up:
//
//  1. t.TempDir registers RemoveAll first, so it runs LAST;
//  2. the path-var restore is registered second;
//  3. s.StopSaveLoop is registered last, so it runs FIRST — the loop has
//     flushed and exited before the vars go back and before the directory
//     disappears (#4774).
func newHubServerForTest(t *testing.T, opts ...hubTestOption) *HubServer {
	t.Helper()
	t.Setenv(hubTestIsolationEnv, "1")

	dir := t.TempDir()

	cfg := hubTestConfig{
		logger:    slog.Default(),
		gitHash:   hubTestDefaultGitHash,
		gitBranch: hubTestDefaultGitBranch,
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	// Restore ONLY what this call changed. A var the test moved itself is the
	// test's to restore, and it may do so with a plain defer — which runs
	// BEFORE t.Cleanup — so re-writing the value we found here would leak the
	// test's redirect into every test that follows.
	saved := make([]string, len(hubTestPathVars))
	changed := make([]bool, len(hubTestPathVars))
	for i, v := range hubTestPathVars {
		if !v.always && *v.ptr != v.prod {
			// The test moved this one deliberately; leave its seeded file
			// reachable.
			continue
		}
		saved[i] = *v.ptr
		changed[i] = true
		*v.ptr = filepath.Join(dir, v.rel)
	}
	if cfg.registryPath != "" {
		registryPath = cfg.registryPath
	}
	t.Cleanup(func() {
		for i, v := range hubTestPathVars {
			if changed[i] {
				*v.ptr = saved[i]
			}
		}
	})

	s := NewHubServer(cfg.port, cfg.logger, cfg.gitHash, cfg.gitBranch)
	t.Cleanup(s.StopSaveLoop)
	return s
}
