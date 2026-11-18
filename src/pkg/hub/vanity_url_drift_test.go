package hub

import (
	"io"
	"log/slog"
	"reflect"
	"testing"
)

// ============================================================
// Stale vanity-URL drift (the "Application is not available" bug)
//
// The hub stored a hive's vanity host once, at claim/repair time, and every
// later pass returned early on `h.VanityURL != ""`. Because the host carries a
// random suffix (hiveNameVanityHost -> randomHostSuffix), a re-provision or a
// route recreation produces a DIFFERENT hostname, and nothing ever compared the
// stored value to the live Route. Live symptom: the dashboard linked
// hosted-katamari-ibm-aiops-or-ph31.apps... while the spoke's hive-vanity Route
// served hosted-katamari-ibm-aiops-or-qcd0.apps..., so the link hit the
// OpenShift "Application is not available" page.
// ============================================================

const (
	// driftDomain mirrors the live cluster's wildcard apps domain.
	driftDomain = "apps.fmaas-vllm-d.fmaas.res.ibm.com"
	// driftStaleHost is the host the registry had persisted (no Route serves it).
	driftStaleHost = "hosted-katamari-ibm-aiops-or-ph31." + driftDomain
	// driftLiveHost is the host the spoke's hive-vanity Route actually serves.
	driftLiveHost = "hosted-katamari-ibm-aiops-or-qcd0." + driftDomain
)

// newVanityDriftTestHub builds a hub whose single cluster is an OpenShift-route
// cluster, matching the vllm-d spoke where the drift was observed.
func newVanityDriftTestHub() *HubServer {
	return &HubServer{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		clusters: map[string]ClusterConfig{
			defaultClusterID: {
				ID:          defaultClusterID,
				Name:        "vllm-d",
				Domain:      driftDomain,
				IngressType: ingressTypeOpenShiftRoute,
			},
		},
	}
}

// installVanityRouteKubectl scripts a kubectl that answers the ONE read
// existingVanityHost makes — `get route hive-dashboard-vanity -o
// jsonpath={.spec.host}` — with the supplied host. Any other invocation exits
// non-zero, so a test cannot accidentally pass by way of some other command
// succeeding.
func installVanityRouteKubectl(t *testing.T, host string) {
	t.Helper()
	installKubectlScript(t, `#!/bin/sh
case "$*" in
  *"get route "*"-vanity"*) printf '`+host+`' ;;
  *) exit 1 ;;
esac
`)
}

// saveDriftHive persists a claimed hive carrying the stale vanity URL.
func saveDriftHive(t *testing.T, id string) {
	t.Helper()
	h := &SaaSHive{
		ID: id, Status: "running", Owner: "katamari", Org: "katamari",
		ProjectName: "katamari ibm aiops or",
		Repos:       []string{"aiops"}, PrimaryRepo: "aiops", ACMMLevel: 3,
		ClusterID: defaultClusterID, ClaimDelivered: true,
		VanityURL: "https://" + driftStaleHost,
	}
	if err := saveSaaSHive(h); err != nil {
		t.Fatal(err)
	}
}

// TestVanityURLDriftReconciledToLiveRoute is the regression test for the bug.
//
// POSITIVE CONTROL: before the fix, repairVanityURLForHive returned false at
// its `h.VanityURL != ""` guard without ever running kubectl, so the stored
// host stayed at ...-ph31 and the assertion below fails. It is not merely
// exercising the code path: the scripted kubectl reports a host that DIFFERS
// from the stored one, and the test asserts the stored value actually changes
// to the live host — so a no-op implementation cannot pass it.
func TestVanityURLDriftReconciledToLiveRoute(t *testing.T) {
	useTempHiveDir(t)
	s := newVanityDriftTestHub()
	installVanityRouteKubectl(t, driftLiveHost)

	const id = "hosted-available-vllmd-01"
	saveDriftHive(t, id)

	// Precondition: the hub links the stale host that no Route serves.
	if got := claimedVanityURL(loadSaaSHive(id)); got != "https://"+driftStaleHost {
		t.Fatalf("precondition: stored vanity URL = %q, want the stale host", got)
	}

	if !s.repairVanityURLForHive(id) {
		t.Fatal("repair reported no change, but the stored vanity host differs from the live Route " +
			"(this is the bug: a stale vanity URL was never reconciled)")
	}

	want := "https://" + driftLiveHost
	if got := loadSaaSHive(id).VanityURL; got != want {
		t.Errorf("stored vanity URL = %q, want the live route host %q", got, want)
	}
	// The link the dashboard renders must now be the live one.
	if got := claimedVanityURL(loadSaaSHive(id)); got != want {
		t.Errorf("claimedVanityURL = %q, want %q", got, want)
	}
}

// TestVanityURLReconcileNoOpWhenLiveHostMatches proves the reconcile is
// idempotent: the overwhelmingly common case (stored == live) must report no
// change, so heartbeats do not rewrite meta.json every beat.
func TestVanityURLReconcileNoOpWhenLiveHostMatches(t *testing.T) {
	useTempHiveDir(t)
	s := newVanityDriftTestHub()
	// The live Route serves exactly what is stored.
	installVanityRouteKubectl(t, driftStaleHost)

	const id = "hosted-available-vllmd-02"
	saveDriftHive(t, id)

	if s.repairVanityURLForHive(id) {
		t.Error("repair reported a change when the stored host already matches the live Route")
	}
	if got := loadSaaSHive(id).VanityURL; got != "https://"+driftStaleHost {
		t.Errorf("stored vanity URL = %q, want it left untouched", got)
	}
}

// TestVanityURLReconcileIngressAliases preserves a stored host wherever it
// appears in the ingress rules, but still adopts a live host when it is absent.
func TestVanityURLReconcileIngressAliases(t *testing.T) {
	const id = "hosted-available-nginx-01"
	placeholder := id + "." + driftDomain
	for _, tt := range []struct {
		name    string
		hosts   string
		want    string
		changed bool
	}{
		{"stored before alias", placeholder + " " + driftStaleHost + " " + driftLiveHost, driftStaleHost, false},
		{"stored after alias", placeholder + " " + driftLiveHost + " " + driftStaleHost, driftStaleHost, false},
		{"placeholder between aliases", driftLiveHost + " " + placeholder + " " + driftStaleHost, driftStaleHost, false},
		{"stored absent", placeholder + " " + driftLiveHost + " alias." + driftDomain, driftLiveHost, true},
		{"placeholder only", placeholder, driftStaleHost, false},
		{"no rules", "", driftStaleHost, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			useTempHiveDir(t)
			s := newVanityDriftTestHub()
			cluster := s.clusters[defaultClusterID]
			cluster.IngressType = "nginx"
			s.clusters[defaultClusterID] = cluster
			installKubectlScript(t, `#!/bin/sh
case "$*" in
  *"get ingress hive"*) printf '%s' '`+tt.hosts+`' ;;
  *) exit 1 ;;
esac
`)
			saveDriftHive(t, id)
			before := loadSaaSHive(id)
			if changed := s.repairVanityURLForHive(id); changed != tt.changed {
				t.Fatalf("repair changed = %v, want %v", changed, tt.changed)
			}
			after := loadSaaSHive(id)
			if got := after.VanityURL; got != "https://"+tt.want {
				t.Fatalf("stored vanity URL = %q, want %q", got, "https://"+tt.want)
			}
			// A matching alias must not rewrite the record or stamp a repair.
			if !tt.changed && tt.hosts != "" && tt.hosts != placeholder && !reflect.DeepEqual(before, after) {
				t.Error("matching live alias changed the hive record")
			}
			if tt.changed && after.LastVanityRepairAt.IsZero() {
				t.Error("adopting a live host did not record a successful repair")
			}
			if s.repairVanityURLForHive(id) {
				t.Error("repeated reconciliation was not idempotent")
			}
		})
	}
}

// TestVanityURLReconcileKeepsStoredHostWhenClusterUnreadable is the guard that
// keeps this fix from making things WORSE than the bug it closes. vllm-d is
// heartbeat-only, so kubectl fails outright; an unreadable cluster is not
// evidence of drift, and blanking or rewriting the URL there would break the
// one working link the dashboard has.
func TestVanityURLReconcileKeepsStoredHostWhenClusterUnreadable(t *testing.T) {
	useTempHiveDir(t)
	s := newVanityDriftTestHub()
	// Every kubectl invocation fails, as on an unreachable cluster.
	installKubectlScript(t, "#!/bin/sh\nexit 1\n")

	const id = "hosted-available-vllmd-03"
	saveDriftHive(t, id)

	if s.repairVanityURLForHive(id) {
		t.Error("repair reported a change though the cluster could not be read")
	}
	if got := loadSaaSHive(id).VanityURL; got != "https://"+driftStaleHost {
		t.Errorf("stored vanity URL = %q, want it preserved when the cluster is unreadable", got)
	}
}
