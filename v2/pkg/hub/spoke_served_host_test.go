package hub

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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

// fakeKubeAPI stands in for the in-cluster Kubernetes API. It serves the two
// list endpoints the served-host discovery reads and 404s everything else, so a
// test cannot pass by way of a request the real code would never make.
type fakeKubeAPI struct {
	ingressHosts []string
	routes       []fakeRoute
	// ingressStatus/routeStatus override the response code, used to simulate a
	// cluster with no Route API (404) or an RBAC denial (403).
	ingressStatus int
	routeStatus   int
}

type fakeRoute struct {
	name string
	host string
}

func (f *fakeKubeAPI) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/ingresses"):
			if f.ingressStatus != 0 {
				w.WriteHeader(f.ingressStatus)
				return
			}
			type rule struct {
				Host string `json:"host"`
			}
			out := map[string]any{"items": []any{}}
			items := []any{}
			for _, h := range f.ingressHosts {
				items = append(items, map[string]any{
					"spec": map[string]any{"rules": []rule{{Host: h}}},
				})
			}
			out["items"] = items
			_ = json.NewEncoder(w).Encode(out)
		case strings.HasSuffix(r.URL.Path, "/routes"):
			if f.routeStatus != 0 {
				w.WriteHeader(f.routeStatus)
				return
			}
			items := []any{}
			for _, rt := range f.routes {
				items = append(items, map[string]any{
					"metadata": map[string]any{"name": rt.name},
					"spec":     map[string]any{"host": rt.host},
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// newFakeKubeConfig points the discovery helpers at a test server.
func newFakeKubeConfig(t *testing.T, api *fakeKubeAPI) (*http.Client, *inClusterConfig) {
	t.Helper()
	srv := httptest.NewServer(api.handler())
	t.Cleanup(srv.Close)
	return srv.Client(), &inClusterConfig{
		BaseURL:   srv.URL,
		Token:     "test-token",
		Namespace: "hive-hosted-test",
	}
}

// POSITIVE CONTROL A (must keep working): a spoke on the hub's own cluster
// carries a hub-domain host on its Ingress, and that is exactly what it must
// report. This is the 67/67 hive-oke case; if this test ever fails, the fix has
// broken the working fleet.
func TestSpokeServedHost_HubDomainIngressIsReported(t *testing.T) {
	wantHost := "hosted-aslom-hive-agent-ua5i." + servedHostOKEDomain
	client, cfg := newFakeKubeConfig(t, &fakeKubeAPI{
		ingressHosts: []string{wantHost},
	})
	got := ingressServedHost(t.Context(), client, cfg)
	if got != wantHost {
		t.Errorf("hive-oke spoke must report its own Ingress host %q, got %q", wantHost, got)
	}
	if !strings.HasSuffix(got, servedHostOKEDomain) {
		t.Errorf("expected a host on the hub domain %q, got %q", servedHostOKEDomain, got)
	}
}

// POSITIVE CONTROL B (the regression): a spoke on a foreign OpenShift cluster
// must report the host its own Route serves, NOT a hub-domain name.
func TestSpokeServedHost_OpenShiftRouteHostIsReported(t *testing.T) {
	wantHost := "hosted-available-vllmd-260806-5q6l." + servedHostOpenShiftDomain
	client, cfg := newFakeKubeConfig(t, &fakeKubeAPI{
		// No Ingress on this cluster; the dashboard Route is the source.
		routes: []fakeRoute{
			{name: routeBaseDashboard, host: wantHost},
			{name: "hive-terminal", host: wantHost},
		},
	})
	got := routeServedHost(t.Context(), client, cfg)
	if got != wantHost {
		t.Errorf("OpenShift spoke must report its Route host %q, got %q", wantHost, got)
	}
	// The heart of the bug: the reported host must NOT be on the hub's wildcard,
	// which would resolve to the hub's router and 503.
	if strings.HasSuffix(got, servedHostOKEDomain) {
		t.Errorf("reported host %q is on the hub wildcard %q — this is the 503 regression",
			got, servedHostOKEDomain)
	}
}

// The dashboard Route must be selected BY NAME. A namespace carries the
// terminal Route and "-vanity" mirrors too, so picking the first item would
// make the advertised URL depend on list order and could advertise the terminal
// host as the dashboard.
func TestSpokeServedHost_PrefersDashboardRouteByName(t *testing.T) {
	dashboard := "hosted-available-vllmd-01." + servedHostOpenShiftDomain
	client, cfg := newFakeKubeConfig(t, &fakeKubeAPI{
		routes: []fakeRoute{
			// Deliberately ordered so a naive "first item" read picks wrong.
			{name: "hive-terminal", host: "terminal-host." + servedHostOpenShiftDomain},
			{name: "hive-dashboard-vanity", host: "vanity-host." + servedHostOpenShiftDomain},
			{name: routeBaseDashboard, host: dashboard},
		},
	})
	if got := routeServedHost(t.Context(), client, cfg); got != dashboard {
		t.Errorf("expected the %s Route host %q, got %q", routeBaseDashboard, dashboard, got)
	}
}

// An RBAC denial or a cluster with no Route API must yield "" — "no answer" —
// so the caller falls back, rather than an assertion that no route exists.
func TestSpokeServedHost_UnknownAPIYieldsEmpty(t *testing.T) {
	client, cfg := newFakeKubeConfig(t, &fakeKubeAPI{
		ingressStatus: http.StatusForbidden,
		routeStatus:   http.StatusNotFound,
	})
	if got := ingressServedHost(t.Context(), client, cfg); got != "" {
		t.Errorf("an RBAC denial must yield \"\", got %q", got)
	}
	if got := routeServedHost(t.Context(), client, cfg); got != "" {
		t.Errorf("a missing Route API must yield \"\", got %q", got)
	}
}

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
