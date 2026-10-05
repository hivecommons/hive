package main

import (
	"testing"

	spoke "github.com/hivecommons/hive/pkg/hub/spoke"
)

// A spoke told its cluster through hive.yaml (hub.cluster_id) — the usual
// setup for a firewalled spoke on a pull-only cluster — must still attach its
// node health to the heartbeat. Gating on HIVE_CLUSTER_ID alone dropped it,
// leaving the hub nothing to show for the cluster (#10559).
func TestHeartbeatClusterHealth_GatedOnResolvedClusterID(t *testing.T) {
	report := &spoke.HeartbeatClusterHealthReport{Summary: spoke.HeartbeatClusterSummary{TotalNodes: 2}}
	calls := 0
	collect := func() *spoke.HeartbeatClusterHealthReport {
		calls++
		return report
	}

	// cluster id from hive.yaml only; HIVE_CLUSTER_ID unset.
	t.Setenv("HIVE_CLUSTER_ID", "")
	cfgHub := bootHeartbeatConfig().Hub
	cfgHub.ClusterID = "vllm-d"
	tgt := resolveHubTarget(cfgHub, "", "")
	if got := heartbeatClusterHealth(tgt.clusterID, collect); got != report {
		t.Fatalf("hive.yaml cluster_id: got %v, want the collected report", got)
	}

	// cluster id from the environment.
	if got := heartbeatClusterHealth(resolveHubTarget(bootHeartbeatConfig().Hub, "", "vllm-d").clusterID, collect); got != report {
		t.Fatalf("HIVE_CLUSTER_ID: got %v, want the collected report", got)
	}

	// No cluster id anywhere: never guess, and never pay for the collection.
	before := calls
	if got := heartbeatClusterHealth("", collect); got != nil {
		t.Fatalf("no cluster id: got %v, want nil", got)
	}
	if got := heartbeatClusterHealth("   ", collect); got != nil {
		t.Fatalf("blank cluster id: got %v, want nil", got)
	}
	if calls != before {
		t.Fatalf("collector ran %d time(s) with no cluster id, want 0", calls-before)
	}
}
