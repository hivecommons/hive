package hub

import "testing"

// The disk metric exists because a node at 81% used gave the operator no
// warning before kubelet began evicting pods. These tests pin the two
// behaviours that make the metric trustworthy: real usage is reported from
// the kubelet stats endpoint, and *missing* usage stays missing rather than
// collapsing into a healthy-looking 0%.

func TestParseNodeStatsSummaryDisk(t *testing.T) {
	// Shape mirrors a real /stats/summary response (extra fields ignored).
	raw := []byte(`{"node":{"nodeName":"n1","fs":{"availableBytes":7086080,"capacityBytes":100,"usedBytes":81}}}`)
	usage, ok := parseNodeStatsSummaryDisk(raw)
	if !ok {
		t.Fatal("expected disk usage to parse")
	}
	if usage.usedBytes != 81 || usage.capacityBytes != 100 {
		t.Fatalf("got used=%d cap=%d, want 81/100", usage.usedBytes, usage.capacityBytes)
	}
}

func TestParseNodeStatsSummaryDiskRejectsUnusable(t *testing.T) {
	cases := map[string]string{
		"malformed json":   `{`,
		"no node":          `{}`,
		"missing usage":    `{"node":{"fs":{"capacityBytes":100}}}`,
		"missing capacity": `{"node":{"fs":{"usedBytes":81}}}`,
		// Zero capacity would make the percentage a divide-by-zero.
		"zero capacity": `{"node":{"fs":{"usedBytes":81,"capacityBytes":0}}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, ok := parseNodeStatsSummaryDisk([]byte(body)); ok {
				t.Fatal("expected unusable stats to be rejected, not defaulted to zero")
			}
		})
	}
}

func TestApplyNodeDiskUsageComputesPercent(t *testing.T) {
	var n ClusterHealthNode
	const gib = int64(giToBytes)
	// 81 GiB used of 100 GiB: the danger zone that triggered the incident.
	applyNodeDiskUsage(&n, nodeDiskUsage{usedBytes: 81 * gib, capacityBytes: 100 * gib})
	if n.DiskPercent == nil || *n.DiskPercent != 81 {
		t.Fatalf("DiskPercent = %v, want 81", n.DiskPercent)
	}
	if n.DiskTotalMB == nil || *n.DiskTotalMB != 100*1024 {
		t.Fatalf("DiskTotalMB = %v, want 102400", n.DiskTotalMB)
	}
	if n.DiskUsedMB == nil || *n.DiskUsedMB != 81*1024 {
		t.Fatalf("DiskUsedMB = %v, want 82944", n.DiskUsedMB)
	}
}

func TestSummarizeDiskIgnoresNodesWithoutData(t *testing.T) {
	mb := func(v int64) *int64 { return &v }
	nodes := []ClusterHealthNode{
		// 90 GiB used of 100 GiB.
		{DiskTotalMB: mb(100 * 1024), DiskUsedMB: mb(90 * 1024)},
		// Unreachable node: must not dilute the percentage toward healthy.
		{},
	}
	gb, pct := summarizeDisk(nodes)
	if gb == nil || pct == nil {
		t.Fatal("expected a summary from the one reporting node")
	}
	if *gb != 100 {
		t.Fatalf("total GB = %d, want 100 (unknown node excluded)", *gb)
	}
	if *pct != 90 {
		t.Fatalf("percent = %d, want 90 (unknown node must not dilute)", *pct)
	}
}

func TestSummarizeDiskUnknownWhenNoNodeReports(t *testing.T) {
	// The critical case: an unreachable cluster must yield nil, so the UI
	// renders a dash. Returning 0 would paint a full disk as healthy.
	gb, pct := summarizeDisk([]ClusterHealthNode{{}, {}})
	if gb != nil || pct != nil {
		t.Fatalf("got %v/%v, want nil/nil so the UI renders unknown, not 0%%", gb, pct)
	}
	if gb, pct := summarizeDisk(nil); gb != nil || pct != nil {
		t.Fatalf("nil nodes: got %v/%v, want nil/nil", gb, pct)
	}
}

func TestNodeStatsSummaryPath(t *testing.T) {
	got := nodeStatsSummaryPath("10.0.10.52")
	want := "/api/v1/nodes/10.0.10.52/proxy/stats/summary"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestDiskThresholdsMatchKubeletBehaviour(t *testing.T) {
	// Thresholds are meaningful only if they track kubelet: image GC starts
	// at 85% used and hard eviction fires at 90% used (nodefs.available<10%).
	if clusterHealthDiskWarnPct != 85 {
		t.Fatalf("warn = %d, want 85 (imageGCHighThresholdPercent)", clusterHealthDiskWarnPct)
	}
	if clusterHealthDiskDangerPct != 90 {
		t.Fatalf("danger = %d, want 90 (evictionHard nodefs.available<10%%)", clusterHealthDiskDangerPct)
	}
	if clusterHealthDiskWarnPct >= clusterHealthDiskDangerPct {
		t.Fatal("warn must fire before danger to leave time to act")
	}
}
