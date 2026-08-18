package hub

import (
	"context"
	"testing"
	"time"
)

func TestClusterHealthQueryTimeoutForDefault(t *testing.T) {
	t.Parallel()

	cluster := &ClusterConfig{InCluster: true}
	if got := clusterHealthQueryTimeoutFor(cluster); got != defaultClusterHealthQueryTimeout {
		t.Fatalf("default timeout = %v, want %v", got, defaultClusterHealthQueryTimeout)
	}
}

func TestClusterHealthQueryTimeoutForRemoteDefault(t *testing.T) {
	t.Parallel()

	cluster := &ClusterConfig{InCluster: false}
	if got := clusterHealthQueryTimeoutFor(cluster); got != remoteClusterHealthQueryTimeout {
		t.Fatalf("remote timeout = %v, want %v", got, remoteClusterHealthQueryTimeout)
	}
}

func TestClusterHealthQueryTimeoutForOverride(t *testing.T) {
	t.Parallel()

	cluster := &ClusterConfig{ClusterHealthTimeoutSeconds: 45}
	if got := clusterHealthQueryTimeoutFor(cluster); got != 45*time.Second {
		t.Fatalf("override timeout = %v, want %v", got, 45*time.Second)
	}
}

func TestKubectlForClusterContextAddsRemoteClusterFlags(t *testing.T) {
	t.Parallel()

	cluster := &ClusterConfig{
		InCluster:      false,
		KubeconfigPath: "/etc/hive/kubeconfigs/vllm-d.yaml",
		Context:        "vllm-d",
	}
	cmd := kubectlForClusterContext(context.Background(), cluster, "--request-timeout", "30s", "get", "nodes")
	want := []string{
		"kubectl",
		"--kubeconfig", "/etc/hive/kubeconfigs/vllm-d.yaml",
		"--context", "vllm-d",
		"--request-timeout", "30s",
		"get", "nodes",
	}
	if len(cmd.Args) != len(want) {
		t.Fatalf("args len = %d, want %d: %v", len(cmd.Args), len(want), cmd.Args)
	}
	for i := range want {
		if cmd.Args[i] != want[i] {
			t.Fatalf("args[%d] = %q, want %q (all args: %v)", i, cmd.Args[i], want[i], cmd.Args)
		}
	}
}
