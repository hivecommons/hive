package hub

import (
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestPullOnlyClusterRegisters is the point of the field: a cluster the hub
// cannot reach must still be REGISTRABLE, so everything keyed off the cluster
// registry works for its hives.
//
// Before this, the loader dropped such an entry entirely ("skipping remote
// cluster with no kubeconfig_path"), which is how five a-ks-wec2 spokes ended
// up with no resolvable App identity while the hub held their App key.
func TestPullOnlyClusterRegisters(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clusters.json")
	body := `[
	  {"id":"hive-oke","name":"OKE","in_cluster":true,"domain":"hive.kubestellar.io"},
	  {"id":"a-ks-wec2","name":"IKS wec2","pull_only":true,"domain":"wec2.example.cloud",
	   "github_app_id":5686,"github_app_slug":"kubestellar-hive-ghe",
	   "github_base_url":"https://github.ibm.com","github_api_url":"https://github.ibm.com/api/v3"}
	]`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	clusters := loadClusterConfigsFrom(t, path)

	c, ok := clusters["a-ks-wec2"]
	if !ok {
		t.Fatal("pull-only cluster was dropped at load: its hives get no identity, " +
			"and clusterForHive falls back to the DEFAULT cluster")
	}
	if !c.PullOnly {
		t.Error("PullOnly did not survive the round trip")
	}
	if c.GitHubAppID != 5686 {
		t.Errorf("GitHubAppID = %d, want 5686", c.GitHubAppID)
	}
}

// TestRemoteClusterWithoutKubeconfigStillDropped is the guard: pull_only must be
// a DELIBERATE declaration, not a way for a misconfigured entry to slip in. An
// entry that simply forgot kubeconfig_path is still an error.
func TestRemoteClusterWithoutKubeconfigStillDropped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clusters.json")
	body := `[{"id":"oops","name":"typo","domain":"x.example"}]`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, ok := loadClusterConfigsFrom(t, path)["oops"]; ok {
		t.Error("a remote cluster with no kubeconfig_path and no pull_only must still be dropped")
	}
}

// TestPullOnlyClusterIsNotKubectlReachable pins the predicate every kubectl call
// site is expected to consult.
func TestPullOnlyClusterIsNotKubectlReachable(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cluster ClusterConfig
		want    bool
	}{
		{"pull-only", ClusterConfig{ID: "a", PullOnly: true, Domain: "d"}, false},
		{"pull-only wins over a stale kubeconfig", ClusterConfig{ID: "a", PullOnly: true, KubeconfigPath: "/etc/hive/kubeconfigs/a.yaml"}, false},
		{"remote with kubeconfig", ClusterConfig{ID: "b", KubeconfigPath: "/etc/hive/kubeconfigs/b.yaml"}, true},
		{"in-cluster", ClusterConfig{ID: "c", InCluster: true}, true},
		{"remote with nothing", ClusterConfig{ID: "d"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cluster.KubectlReachable(); got != tc.want {
				t.Errorf("KubectlReachable() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestKubectlNeverFallsBackToAmbientClusterForPullOnly is the safety property
// that matters most.
//
// kubectl given an EMPTY --kubeconfig falls back to its ambient configuration,
// which inside the hub pod is the hub's OWN cluster. A command intended for a
// pull-only pool would then execute against the hub's cluster and report
// success for work that never happened there — the silent-wrong-cluster
// failure, which is far worse than an error.
func TestKubectlNeverFallsBackToAmbientClusterForPullOnly(t *testing.T) {
	c := &ClusterConfig{ID: "a-ks-wec2", PullOnly: true, Domain: "wec2.example.cloud"}

	args := kubectlArgsForCluster(c, "get", "pods")

	i := slices.Index(args, "--kubeconfig")
	if i < 0 || i+1 >= len(args) {
		t.Fatalf("no --kubeconfig in %v", args)
	}
	got := args[i+1]
	if got == "" {
		t.Fatal("empty --kubeconfig: kubectl would silently use the hub's OWN cluster")
	}
	if got != unreachableKubeconfigSentinel {
		t.Errorf("--kubeconfig = %q, want the unreachable sentinel", got)
	}
	if _, err := os.Stat(got); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the sentinel path must not exist, stat gave %v", err)
	}
}

// TestPullOnlyClusterCountsAsUnreachableWithoutDialing checks the declarative
// short-circuit of the reactive breaker: unreachability is known from config, so
// the hub never pays the dial timeout that made a pool of hives serialise into
// tens of minutes of blocking.
func TestPullOnlyClusterCountsAsUnreachableWithoutDialing(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.clusters = map[string]ClusterConfig{
		"a-ks-wec2": {ID: "a-ks-wec2", PullOnly: true, Domain: "wec2.example.cloud"},
		"hive-oke":  {ID: "hive-oke", InCluster: true, Domain: "hive.kubestellar.io"},
	}

	if !srv.clusterRecentlyUnreachable("a-ks-wec2") {
		t.Error("a pull-only cluster must count as unreachable before any dial is attempted")
	}
	if srv.clusterRecentlyUnreachable("hive-oke") {
		t.Error("a reachable cluster must not be suppressed")
	}
}

// TestPullOnlyClusterHealthUsesHeartbeat asserts the health query routes to the
// sentinel rather than a generic failure, so the caller uses the health the
// spokes report over the heartbeat instead of rendering the pool as broken.
func TestPullOnlyClusterHealthUsesHeartbeat(t *testing.T) {
	c := &ClusterConfig{ID: "a-ks-wec2", PullOnly: true, Domain: "wec2.example.cloud"}

	_, err := buildSingleClusterHealth(c, 5, slog.Default())
	if err == nil {
		t.Fatal("expected the pull-only sentinel, got nil")
	}
	if !errors.Is(err, errClusterPullOnly) {
		t.Errorf("error %v does not wrap errClusterPullOnly, so it would be logged as a query failure", err)
	}
}

// TestPullOnlyVanityHostIsNotSilentlyAttempted covers the concrete misfire seen
// live: vanity repair for an a-ks-wec2 hive ran kubectl against hive-oke and
// reported "no ingress found" for a namespace on another cluster.
func TestPullOnlyVanityHostIsNotSilentlyAttempted(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	called := false
	srv.vanityHostServable = func(hiveID, vanityHost string, cluster *ClusterConfig) error {
		called = true
		return nil
	}

	c := &ClusterConfig{ID: "a-ks-wec2", PullOnly: true, Domain: "wec2.example.cloud"}
	err := srv.makeVanityHostServable("hosted-x", "hosted-x.wec2.example.cloud", c)
	if err == nil {
		t.Error("expected an explicit error so the caller reports the host as not served")
	}
	if called {
		t.Error("the ingress path must not run for a cluster the hub cannot reach")
	}
}

// migrateHive is provision-on-target plus deprovision-of-source, and both
// halves are kubectl. A pull-only cluster cannot serve either half, so the
// handler must refuse BEFORE it flips the hive into the migrating state —
// otherwise a hive is left marked migrating for work that can never run.
func TestMigrateRejectsPullOnlyCluster(t *testing.T) {
	for _, tc := range []struct {
		name           string
		sourcePullOnly bool
		targetPullOnly bool
	}{
		{"target is pull-only", false, true},
		{"source is pull-only", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(helperSetupTempDirs(t))
			mkUser(t, "po-user")

			srv := newHandlerHub2()
			srv.clusters = map[string]ClusterConfig{
				defaultClusterID: {ID: defaultClusterID, KubeconfigPath: "/tmp/kc", PullOnly: tc.sourcePullOnly},
				"pool-b":         {ID: "pool-b", KubeconfigPath: "/tmp/kc", PullOnly: tc.targetPullOnly},
			}
			if err := saveSaaSHive(&SaaSHive{ID: "hosted-po", Owner: "po-user"}); err != nil {
				t.Fatalf("seed hive: %v", err)
			}

			w := httptest.NewRecorder()
			req := setPathValue(reqWithUser(http.MethodPost, "/api/saas/hives/hosted-po/migrate",
				`{"target_cluster_id":"pool-b"}`, "po-user"), "id", "hosted-po")
			srv.handleMigrateHive(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("migration involving a pull-only cluster must be refused with 400, got %d: %s", w.Code, w.Body.String())
			}
			// The refusal must leave no migration state behind.
			if h := loadSaaSHive("hosted-po"); h != nil && h.MigrationStatus != "" {
				t.Errorf("hive was flipped into migration state %q despite the migration being refused", h.MigrationStatus)
			}
		})
	}
}

// loadClusterConfigsFrom parses a clusters.json at an arbitrary path using the
// same loader production uses.
func loadClusterConfigsFrom(t *testing.T, path string) map[string]ClusterConfig {
	t.Helper()
	orig := clustersConfigPath
	clustersConfigPath = path
	t.Cleanup(func() { clustersConfigPath = orig })
	return loadClusters(slog.Default())
}
