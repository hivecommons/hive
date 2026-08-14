package hub

import (
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

// namespaceRegistryEntry drives handleHeartbeat exactly as the spoke does (via
// the shared postHeartbeat helper), asserts a 200, and returns the freshly-
// stored registry entry for the hive.
func namespaceRegistryEntry(t *testing.T, srv *HubServer, hiveID string) RegistryEntry {
	t.Helper()
	rec := postHeartbeat(t, srv, `{"hive_id":"`+hiveID+`","org":"testorg","git_hash":"sha1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat returned %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	for _, h := range srv.registry.Hives {
		if h.ID == hiveID {
			return h
		}
	}
	t.Fatalf("no registry entry stored for hive %q after heartbeat", hiveID)
	return RegistryEntry{}
}

// TestHeartbeatOverlaysHostedNamespace covers the RegistryEntry.Namespace
// overlay on the authoritative heartbeat path: when a SaaSHive record exists the
// entry is stamped with the derived "hive-hosted-<id>" namespace (the value an
// operator needs for `kubectl -n <ns> exec` without re-grepping the cluster),
// and when no SaaSHive record exists the namespace is left empty — never guessed
// for a self-hosted/BYO spoke.
func TestHeartbeatOverlaysHostedNamespace(t *testing.T) {
	useTempHiveDir(t) // restored via t.Cleanup — no leaked global for -race to catch

	t.Run("hosted hive with a SaaSHive record gets hive-hosted-<id>", func(t *testing.T) {
		srv := NewHubServer(0, slog.Default(), "test", "v2")
		srv.setHubSecret("")

		const hiveID = "hosted-available-oke-07-placeholder-a1b2"
		if err := saveSaaSHive(&SaaSHive{
			ID:        hiveID,
			Owner:     hubAdminUsername,
			Status:    statusAvailable,
			ClusterID: defaultClusterID,
		}); err != nil {
			t.Fatal(err)
		}

		entry := namespaceRegistryEntry(t, srv, hiveID)

		want := hiveHostedNamespacePrefix + hiveID
		if entry.Namespace != want {
			t.Errorf("Namespace = %q, want %q", entry.Namespace, want)
		}
		// The ID never changes on claim, so the derived namespace stays correct
		// across reassignment — this is the whole reason it is safe to persist.
		if entry.Namespace != "hive-hosted-"+hiveID {
			t.Errorf("Namespace not the stable hive-hosted-<id> form: %q", entry.Namespace)
		}
	})

	t.Run("hive with no SaaSHive record keeps Namespace empty", func(t *testing.T) {
		srv := NewHubServer(0, slog.Default(), "test", "v2")
		srv.setHubSecret("")

		// No saveSaaSHive: this is a self-hosted/BYO spoke the hub did not
		// provision, so it does not run in a hive-hosted-<id> namespace and the
		// hub must not fabricate one.
		entry := namespaceRegistryEntry(t, srv, "byo-selfhosted-hive")

		if entry.Namespace != "" {
			t.Errorf("Namespace = %q for a hive with no SaaSHive record, want empty (must not be guessed)", entry.Namespace)
		}
	})
}

// TestDashboardRendersHiveNamespace guards that the hive-row namespace stays
// surfaced in the STATUS HOVER (not the location column): the hover's lines[]
// builder derives the namespace via hiveNamespace(h) and pushes it as an "ns:"
// line. A regression that dropped this would silently send operators back to
// `kubectl get ns`. The namespace is deliberately NOT a permanent location-
// column value — it is low-frequency reference metadata for a kubectl -n exec.
func TestDashboardRendersHiveNamespace(t *testing.T) {
	snippets := []string{
		// The hover derives the namespace client-side from the row.
		"hiveNamespace(h)",
		// It is pushed into the hover as an "ns:" line.
		"lines.push('ns: ' + hns)",
	}
	for _, s := range snippets {
		if !strings.Contains(dashboardHTML, s) {
			t.Errorf("dashboardHTML is missing the hive-namespace hover snippet %q", s)
		}
	}
	// And it must NOT reappear as a permanent location-column render.
	if strings.Contains(dashboardHTML, "var nsLine =") {
		t.Errorf("namespace should live in the status hover, not the location column (found nsLine render)")
	}
}
