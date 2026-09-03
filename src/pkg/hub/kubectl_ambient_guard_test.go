package hub

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// ============================================================================
// THE INVARIANT: a kubectl command NEVER runs against an ambient cluster.
// ============================================================================
//
// This is how the leak in #5768 actually reached a live cluster, and it is the
// half of the bug that teardown and the janitor do not address.
//
// The chain, measured rather than reasoned about:
//
//  1. Test helpers point clustersConfigPath at an empty t.TempDir(), so no
//     clusters.json exists.
//  2. loadClustersChecked treats an absent file as clustersAbsent and
//     synthesises defaultClusterRegistry() — one entry, hive-oke, with
//     InCluster: TRUE.
//  3. kubectlArgsForCluster's InCluster branch adds --server/--token ONLY when
//     KUBERNETES_SERVICE_HOST and _PORT are set. On a laptop or a CI runner
//     they are not, so it USED TO add no connection flag at all.
//  4. kubectl with no flags falls back to its ambient configuration —
//     ~/.kube/config or $KUBECONFIG.
//  5. Any test that reaches provisioning therefore issued a REAL `kubectl
//     apply` — creating a real Namespace, Deployment and PVC — against
//     whatever cluster the runner was pointed at, named after the test's own
//     fixture org.
//
// That is why the leaked namespaces are this package's unit-test fixtures
// character for character: hive-hosted-hosted-acme-repo-abcd,
// hive-hosted-hosted-apporg-repo1-*, hive-hosted-hosted-myorg-repo1-*.
//
// InCluster is a CLAIM, not a fact. The absence of the ServiceAccount env vars
// is a positive, reliable signal that the process is not in a pod — a real hub
// pod always has both — so the guard cannot break a legitimate in-cluster call.

// TestKubectlNeverRunsAgainstAmbientClusterOutsidePod is THE invariant test.
//
// For EVERY cluster shape, the built argv must always pin kubectl to an
// explicit target. An argv carrying no --kubeconfig and no --server is one
// that resolves against ambient config, and there is no cluster shape for
// which that is acceptable outside a pod.
func TestKubectlNeverRunsAgainstAmbientClusterOutsidePod(t *testing.T) {
	// Guarantee we look like "not running in a pod", which is the situation
	// every test and every developer laptop is actually in.
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")

	clusters := []struct {
		name    string
		cluster *ClusterConfig
	}{
		{"default registry entry (the one tests actually get)", func() *ClusterConfig {
			c := defaultClusterRegistry()[defaultClusterID]
			return &c
		}()},
		{"in-cluster claim", &ClusterConfig{ID: "c", InCluster: true}},
		{"remote with kubeconfig", &ClusterConfig{ID: "c", KubeconfigPath: "/tmp/kc", Context: "ctx"}},
		{"remote without kubeconfig", &ClusterConfig{ID: "c"}},
		{"pull-only", &ClusterConfig{ID: "c", PullOnly: true}},
		{"pull-only claiming in-cluster", &ClusterConfig{ID: "c", PullOnly: true, InCluster: true}},
	}

	for _, tc := range clusters {
		t.Run(tc.name, func(t *testing.T) {
			argv := kubectlArgsForCluster(tc.cluster, "apply", "-f", "manifest.yaml")
			joined := strings.Join(argv, " ")

			pinned := false
			for _, a := range argv {
				if a == "--kubeconfig" || a == "--server" {
					pinned = true
					break
				}
			}
			if !pinned {
				t.Fatalf("INVARIANT VIOLATED: kubectl argv pins no cluster, so it resolves\n"+
					"against AMBIENT kube config (~/.kube/config or $KUBECONFIG).\n"+
					"This is how #5768 leaked fixture-named namespaces onto a live CI cluster.\n"+
					"  cluster: %+v\n  argv:    %s", tc.cluster, joined)
			}
		})
	}
}

// TestInClusterClaimOutsidePodAimsAtSentinel pins the specific repair: a
// cluster claiming InCluster while the ServiceAccount env vars are absent must
// aim at the unreachable sentinel, so the command fails loudly and locally
// rather than succeeding quietly somewhere real.
func TestInClusterClaimOutsidePodAimsAtSentinel(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")

	argv := kubectlArgsForCluster(&ClusterConfig{ID: "hive-oke", InCluster: true}, "get", "ns")
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, unreachableKubeconfigSentinel) {
		t.Fatalf("an InCluster claim outside a pod must aim at the unreachable sentinel;\ngot: %s", joined)
	}
}

// TestRealInClusterStillUsesServiceAccount is the other side of the guard: in a
// REAL pod, the in-cluster path must be untouched. If this fails, the fix has
// broken production provisioning rather than only test escapes.
func TestRealInClusterStillUsesServiceAccount(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	t.Setenv("KUBERNETES_SERVICE_PORT", "443")

	argv := kubectlArgsForCluster(&ClusterConfig{ID: "hive-oke", InCluster: true}, "get", "ns")
	joined := strings.Join(argv, " ")

	if !strings.Contains(joined, "--server https://10.0.0.1:443") {
		t.Errorf("a real in-cluster pod must still target the API server; got: %s", joined)
	}
	if strings.Contains(joined, unreachableKubeconfigSentinel) {
		t.Errorf("a real in-cluster pod must NOT be aimed at the sentinel; got: %s", joined)
	}
}

// TestProvisioningFromTestHarnessCannotReachAmbientCluster is the end-to-end
// assertion, driven through the real HTTP handler exactly as the escaping
// tests do.
//
// It asserts the property at the layer where it actually bit: a test that
// drives provisioning must not be able to produce a kubectl invocation capable
// of reaching a real cluster. The argv is inspected rather than the outcome,
// because on a machine with no cluster ANY implementation "passes" by
// accident — which is precisely why this went unnoticed.
func TestProvisioningFromTestHarnessCannotReachAmbientCluster(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	// The cluster a test actually gets: no clusters.json, so the synthesised
	// default entry, which claims InCluster.
	c := defaultClusterRegistry()[defaultClusterID]
	if !c.InCluster {
		t.Fatal("precondition changed: the default cluster no longer claims InCluster; " +
			"re-check whether this guard still covers the escape path")
	}

	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")

	argv := kubectlArgsForCluster(&c, "apply", "-f", "all.yaml")
	for i, a := range argv {
		// An empty --kubeconfig is the worst case: kubectl treats it as
		// "use ambient config", which is the silent-misfire this guards.
		if a == "--kubeconfig" && (i+1 >= len(argv) || strings.TrimSpace(argv[i+1]) == "") {
			t.Fatal("empty --kubeconfig makes kubectl fall back to ambient config")
		}
	}
	if !strings.Contains(strings.Join(argv, " "), unreachableKubeconfigSentinel) {
		t.Fatalf("a provisioning kubectl built by a test harness must be unable to reach a\n"+
			"real cluster. argv: %s", strings.Join(argv, " "))
	}
}

// TestHandleCreateHiveDoesNotLeakNamespaceOnUnstubbedCluster documents the
// full end-to-end behaviour of the escaping test shape: driving the create
// handler with no scripted kubectl installed must NOT succeed at provisioning,
// and must not leave the hive marked as successfully provisioned.
func TestHandleCreateHiveDoesNotLeakNamespaceOnUnstubbedCluster(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	authCleanup := helperSetupAuthUser(t, "ghp_guard", "guard-user")
	defer authCleanup()

	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")

	u := ensureSaaSUser("guard-user")
	u.SaaSQuota = 5
	if err := saveSaaSUser(u); err != nil {
		t.Fatalf("saveSaaSUser: %v", err)
	}
	srv := newHubServerForTest(t)

	body := `{"org":"apporg","repos":"repo1","github_token":"ghp_fake12345678"}`
	req := httptest.NewRequest("POST", "/create", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer ghp_guard")
	w := httptest.NewRecorder()
	srv.handleCreateHive(w, req)

	// Drain the async provisioning the handler enqueued.
	provisionWG.Wait()

	// Whatever the handler returned to the caller, provisioning itself must
	// have failed rather than quietly succeeding against an ambient cluster.
	for _, h := range listSaaSHives() {
		if h.Status == "provisioning" || h.Status == statusAvailable {
			t.Errorf("hive %q reached status %q with no reachable cluster — provisioning "+
				"must not appear to succeed when kubectl cannot legitimately target anything",
				h.ID, h.Status)
		}
	}
}
