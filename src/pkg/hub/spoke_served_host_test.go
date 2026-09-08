package hub

import (
	"io"
	"log/slog"
	"strings"
	"testing"
)

// ============================================================
// Unreachable spoke URL: the hub linked a host no Route serves.
//
// The spoke's dashboard_url fallback synthesised "<hiveID>.<hub host>". That is
// only correct when the spoke is fronted by the hub's OWN wildcard domain
// (hive-oke, where *.hive.kubestellar.io IS the router). On the OpenShift pool
// the spoke's real Route serves
// hosted-available-vllmd-260806-5q6l.apps.fmaas-vllm-d.fmaas.res.ibm.com, but
// the hub linked hosted-available-vllmd-260806-5q6l.hive.kubestellar.io — a
// name the shared wildcard resolves to the HUB's router, which has no backend
// for it and answers 503.
//
// Measured live 2026-08-14 and the reason the fix reads the live object:
//   - dig random-nonexistent-xyz.hive.kubestellar.io -> 157.151.252.29 (the hub
//     router). The wildcard answers for ANY name, so DNS resolving proves
//     nothing about reachability.
//   - curl https://hosted-available-vllmd-260806-5q6l.hive.kubestellar.io/ -> 000
//   - curl https://hosted-available-vllmd-260806-5q6l.apps.fmaas-vllm-d...   -> 401
//
// Both directions are asserted here. A spoke whose Ingress genuinely carries a
// hub-domain host must still report it (the hive-oke positive control, 67/67
// working ingresses that must not regress), and a spoke on a foreign cluster
// must report its own router's host.
// ============================================================

const (
	// servedHostOKEDomain is the hub's own wildcard domain — the ONLY domain
	// for which the old synthesised host was ever correct.
	servedHostOKEDomain = "hive.kubestellar.io"
	// servedHostOpenShiftDomain is the OpenShift pool's wildcard apps domain.
	servedHostOpenShiftDomain = "apps.fmaas-vllm-d.fmaas.res.ibm.com"
)

// placeholderHostURL must derive the domain from the hive's OWN cluster. The
// hardcoded hive.kubestellar.io it replaced is the hub's wildcard, so using it
// for a spoke elsewhere mints a host that resolves to the hub and 503s.
func TestPlaceholderHostURL_UsesTheHivesOwnClusterDomain(t *testing.T) {
	dir := t.TempDir()
	oldDir := saasHivesDir
	saasHivesDir = dir
	defer func() { saasHivesDir = oldDir }()

	s := &HubServer{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		clusters: map[string]ClusterConfig{
			defaultClusterID: {ID: defaultClusterID, Domain: servedHostOKEDomain, IngressType: "nginx"},
			"openshift-pool": {ID: "openshift-pool", Domain: servedHostOpenShiftDomain, IngressType: ingressTypeOpenShiftRoute, PullOnly: true},
		},
	}

	// Positive control A: a hive on the hub's own cluster keeps the hub domain.
	okeHive := &SaaSHive{ID: "hosted-oke-spoke-aaaa", Status: "running", ClusterID: defaultClusterID}
	if err := saveSaaSHive(okeHive); err != nil {
		t.Fatal(err)
	}
	wantOKE := "https://hosted-oke-spoke-aaaa." + servedHostOKEDomain
	if got := s.placeholderHostURL(okeHive.ID); got != wantOKE {
		t.Errorf("hub-cluster hive: expected %q, got %q", wantOKE, got)
	}

	// Positive control B: a hive on the pull-only OpenShift pool must get that
	// pool's domain, never the hub's.
	osHive := &SaaSHive{ID: "hosted-available-vllmd-260806-5q6l", Status: "running", ClusterID: "openshift-pool"}
	if err := saveSaaSHive(osHive); err != nil {
		t.Fatal(err)
	}
	wantOS := "https://hosted-available-vllmd-260806-5q6l." + servedHostOpenShiftDomain
	got := s.placeholderHostURL(osHive.ID)
	if got != wantOS {
		t.Errorf("openshift-pool hive: expected %q, got %q", wantOS, got)
	}
	if strings.HasSuffix(strings.TrimPrefix(got, "https://"), servedHostOKEDomain) {
		t.Errorf("openshift-pool hive got a hub-wildcard host %q — this is the 503 regression", got)
	}
}

// A hive whose cluster (or cluster domain) is unknown must yield "" so the
// caller reports "no reachable dashboard URL yet" instead of inventing an
// unreachable host.
func TestPlaceholderHostURL_UnknownClusterYieldsEmpty(t *testing.T) {
	dir := t.TempDir()
	oldDir := saasHivesDir
	saasHivesDir = dir
	defer func() { saasHivesDir = oldDir }()

	s := &HubServer{
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		clusters: map[string]ClusterConfig{},
	}
	h := &SaaSHive{ID: "hosted-orphan-spoke-bbbb", Status: "running", ClusterID: "gone"}
	if err := saveSaaSHive(h); err != nil {
		t.Fatal(err)
	}
	if got := s.placeholderHostURL(h.ID); got != "" {
		t.Errorf("expected \"\" for a hive with no known cluster, got %q", got)
	}
}
