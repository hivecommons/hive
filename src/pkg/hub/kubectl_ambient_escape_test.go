package hub

import (
	"strings"
	"testing"
)

// The tests below pin the fix for issue #5768.
//
// A hosted-spoke provisioning call builds its kubectl argv through the single
// chokepoint kubectlArgsForCluster. For a cluster whose config CLAIMS
// InCluster, that function used to append the in-cluster connection flags only
// when KUBERNETES_SERVICE_HOST and KUBERNETES_SERVICE_PORT were both set, and
// nothing at all otherwise. "Nothing at all" is not a safe default: kubectl
// with no --server and no --kubeconfig resolves against its ambient
// configuration, so the command executes against whatever cluster the machine's
// current context names.
//
// That is how 76 hive-hosted-hosted-<fixture-org>-* namespaces reached a live
// CI cluster with no corresponding hub bookkeeping — the applies were real
// while the store writes went to a per-test t.TempDir().

// hasFlag reports whether argv contains flag.
func hasFlag(argv []string, flag string) bool {
	for _, a := range argv {
		if a == flag {
			return true
		}
	}
	return false
}

// flagValue returns the value following flag, or "" when absent.
func flagValue(argv []string, flag string) string {
	for i, a := range argv {
		if a == flag && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}

// TestInClusterClaimWithoutServiceAccountEnvCannotUseAmbientConfig is the
// regression test for #5768. It is the test that would have caught the leak:
// with the ServiceAccount environment absent, an InCluster claim must NOT
// produce an argv that lets kubectl choose a cluster for itself.
//
// This test FAILS without the fix in kubectlArgsForCluster — the pre-fix argv
// for these inputs is exactly ["apply","-f","-"], which carries no connection
// flag and therefore inherits $KUBECONFIG / ~/.kube/config.
func TestInClusterClaimWithoutServiceAccountEnvCannotUseAmbientConfig(t *testing.T) {
	// TestMain already clears these; set them empty explicitly so this test
	// states its own precondition rather than depending on suite-wide setup.
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")

	cluster := &ClusterConfig{ID: "hive-oke", InCluster: true}
	argv := kubectlArgsForCluster(cluster, "apply", "-f", "-")

	// The core invariant: SOME explicit target must be named. An argv with
	// neither --server nor --kubeconfig is the ambient-config escape.
	if !hasFlag(argv, "--server") && !hasFlag(argv, "--kubeconfig") {
		t.Fatalf("ambient-config escape: InCluster claim with no ServiceAccount env produced "+
			"no --server and no --kubeconfig, so kubectl would target the machine's "+
			"current context. argv=%q", argv)
	}

	// And specifically it must be aimed at the unreachable sentinel, so the
	// command fails loudly instead of mutating some other cluster.
	if got := flagValue(argv, "--kubeconfig"); got != unreachableKubeconfigSentinel {
		t.Errorf("--kubeconfig = %q, want the unreachable sentinel %q (argv=%q)",
			got, unreachableKubeconfigSentinel, argv)
	}
}

// TestDefaultClusterRegistryEntryCannotEscapeToAmbientConfig covers the exact
// path the leak took. A test that points clustersConfigPath at an empty
// t.TempDir() gets clustersAbsent, so loadClustersChecked synthesises
// defaultClusterRegistry() — whose single entry claims InCluster: true. That
// synthesised entry is what provisioning then used, so it must be safe too.
func TestDefaultClusterRegistryEntryCannotEscapeToAmbientConfig(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")

	reg := defaultClusterRegistry()
	cluster, ok := reg[defaultClusterID]
	if !ok {
		t.Fatalf("defaultClusterRegistry() has no %q entry", defaultClusterID)
	}
	if !cluster.InCluster {
		t.Skip("default registry entry no longer claims InCluster; this escape is moot")
	}

	argv := kubectlArgsForCluster(&cluster, "create", "namespace", "hive-hosted-hosted-acme-repo-abcd")
	if !hasFlag(argv, "--server") && !hasFlag(argv, "--kubeconfig") {
		t.Fatalf("synthesised default cluster would provision against the ambient cluster: argv=%q", argv)
	}
	if got := flagValue(argv, "--kubeconfig"); got != unreachableKubeconfigSentinel {
		t.Errorf("--kubeconfig = %q, want %q (argv=%q)", got, unreachableKubeconfigSentinel, argv)
	}
}

// TestRealInClusterStillUsesServiceAccount pins the behaviour the guard must
// NOT change: inside a genuine hub pod both env vars are present, and kubectl
// must still be handed the ServiceAccount credentials rather than the sentinel.
// Without this, a fix could "close" the escape by breaking real provisioning.
func TestRealInClusterStillUsesServiceAccount(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.96.0.1")
	t.Setenv("KUBERNETES_SERVICE_PORT", "443")

	cluster := &ClusterConfig{ID: "hive-oke", InCluster: true}
	argv := kubectlArgsForCluster(cluster, "get", "pods")

	if got := flagValue(argv, "--server"); got != "https://10.96.0.1:443" {
		t.Errorf("--server = %q, want https://10.96.0.1:443 (argv=%q)", got, argv)
	}
	if hasFlag(argv, "--kubeconfig") {
		t.Errorf("a real in-cluster hub must use its ServiceAccount, not --kubeconfig: argv=%q", argv)
	}
	if !hasFlag(argv, "--certificate-authority") || !hasFlag(argv, "--token") {
		t.Errorf("real in-cluster argv missing ServiceAccount credentials: argv=%q", argv)
	}
	// The caller's own args must survive at the tail.
	if !strings.HasSuffix(strings.Join(argv, " "), "get pods") {
		t.Errorf("caller args were not preserved: argv=%q", argv)
	}
}

// TestUnreachableRemoteClusterUnchanged pins the pre-existing pull-only
// behaviour so this change is provably additive: a non-InCluster cluster the
// hub cannot reach already aimed at the sentinel, and still must.
func TestUnreachableRemoteClusterUnchanged(t *testing.T) {
	pullOnly := &ClusterConfig{ID: "vllm-d", PullOnly: true, KubeconfigPath: "/etc/hive/kubeconfigs/vllm-d"}
	argv := kubectlArgsForCluster(pullOnly, "get", "ns")
	if got := flagValue(argv, "--kubeconfig"); got != unreachableKubeconfigSentinel {
		t.Errorf("pull-only cluster --kubeconfig = %q, want sentinel %q", got, unreachableKubeconfigSentinel)
	}

	reachable := &ClusterConfig{ID: "a-ks-wec2", KubeconfigPath: "/etc/hive/kubeconfigs/a-ks-wec2", Context: "wec2"}
	argv2 := kubectlArgsForCluster(reachable, "get", "ns")
	if got := flagValue(argv2, "--kubeconfig"); got != "/etc/hive/kubeconfigs/a-ks-wec2" {
		t.Errorf("reachable remote cluster --kubeconfig = %q, want its real path", got)
	}
}
