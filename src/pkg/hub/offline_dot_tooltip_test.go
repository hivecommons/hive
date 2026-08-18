package hub

import (
	"strings"
	"testing"
)

// TestOfflineStatusDotCarriesTooltip pins the native `title` tooltip on the
// My Hives status dot in its OFFLINE state. The online dot has always carried a
// rich hover via healthBadge() (status + ns: + heartbeat age), but the grey
// offline dot rendered as a bare '<span class="online-dot off"></span>' with no
// title at all — so an operator hovering a dead row could not even see which
// Kubernetes namespace the hive runs in without cross-referencing the registry
// by hand. This asserts the offline dot now builds a defensive title from the
// same facts an operator needs to go find the hive on the cluster.
//
// String-anchored on the raw dashboardHTML, same as the other JS structure
// tests (see TestDashboardHeartbeatHeartFlashesOnReceipt).
func TestOfflineStatusDotCarriesTooltip(t *testing.T) {
	// The offline dot span must now carry a title attribute (and a help cursor),
	// no longer the bare untitled span.
	if strings.Contains(dashboardHTML, `: '<span class="online-dot off"></span>');`) {
		t.Error("offline status dot is still the bare untitled '<span class=\"online-dot off\"></span>' — it must carry a title tooltip")
	}
	if !strings.Contains(dashboardHTML, `'<span class="online-dot off" title="' + escAttr(offlineDotTitle) + '" style="cursor:help"></span>'`) {
		t.Error("offline status dot lost its escAttr(offlineDotTitle) title attribute")
	}

	// The tooltip is built defensively: a status word, then the namespace,
	// cluster and last-seen lines, each skipped when its field is empty.
	for _, part := range []string{
		`var offlineDotTitle = (function() {`,
		`var parts = ['Offline'];`,
		`var ns = hiveNamespace(h);`,
		`if (ns) parts.push('ns: ' + ns);`,
		`var cluster = h.clusterName || h.clusterId || '';`,
		`if (cluster) parts.push('cluster: ' + cluster);`,
		`if (h.lastHeartbeat) parts.push('last seen: ' + fmtUserTS(h.lastHeartbeat));`,
		`return parts.join('\n');`,
	} {
		if !strings.Contains(dashboardHTML, part) {
			t.Errorf("offline status dot tooltip is missing its %q builder line", part)
		}
	}
}
