package sandbox

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/sandbox/substrateapi"
)

// Cluster/router half of the Substrate preflight (#17698): a fake Kubernetes
// API serves the handful of read-only endpoints the prover walks, and a TLS
// listener plays atenet-router.

// fakeKubeAPI is a path → JSON (or status code) table.
type fakeKubeAPI struct {
	bodies map[string]any
	codes  map[string]int
	calls  []string
}

func (f *fakeKubeAPI) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls = append(f.calls, r.URL.Path)
		if code, ok := f.codes[r.URL.Path]; ok {
			w.WriteHeader(code)
			return
		}
		body, ok := f.bodies[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if raw, isRaw := body.(string); isRaw {
			_, _ = w.Write([]byte(raw))
			return
		}
		_ = json.NewEncoder(w).Encode(body)
	})
}

func kubeList(items ...any) map[string]any { return map[string]any{"items": items} }

func kubeDeploy(name string, ready int) map[string]any {
	return map[string]any{"metadata": map[string]any{"name": name}, "status": map[string]any{"readyReplicas": ready}}
}

func kubePool(name string, ready int, classes ...string) map[string]any {
	var sc []map[string]any
	for _, c := range classes {
		sc = append(sc, map[string]any{"name": c})
	}
	return map[string]any{"metadata": map[string]any{"name": name}, "spec": map[string]any{"sandboxClasses": sc}, "status": map[string]any{"readyReplicas": ready}}
}

func kubeNode(name, kernel string, kvm bool) map[string]any {
	alloc := map[string]string{"cpu": "4"}
	if kvm {
		alloc[substrateKVMResource] = "1"
	}
	return map[string]any{"metadata": map[string]any{"name": name}, "status": map[string]any{"allocatable": alloc, "nodeInfo": map[string]any{"kernelVersion": kernel}}}
}

func kubeResources(names ...string) map[string]any {
	var rs []map[string]any
	for _, n := range names {
		rs = append(rs, map[string]any{"name": n})
	}
	return map[string]any{"resources": rs}
}

const (
	pathCRDs      = "/apis/apiextensions.k8s.io/v1/customresourcedefinitions/"
	pathDeploys   = "/apis/apps/v1/namespaces/ate-system/deployments"
	pathAtelet    = "/apis/apps/v1/namespaces/ate-system/daemonsets/atelet"
	pathPools     = "/apis/ate.dev/v1/workerpools"
	pathSandboxes = "/apis/ate.dev/v1/sandboxconfigs"
	pathRuntimes  = "/apis/node.k8s.io/v1/runtimeclasses"
	pathNodes     = "/api/v1/nodes"
)

// healthyKube is a cluster where every check passes.
func healthyKube() *fakeKubeAPI {
	f := &fakeKubeAPI{bodies: map[string]any{}, codes: map[string]int{}}
	for _, crd := range substrateCRDs {
		f.bodies[pathCRDs+crd] = map[string]any{"metadata": map[string]any{"name": crd}}
	}
	var deps []any
	for _, d := range substrateOperatorDeployments {
		deps = append(deps, kubeDeploy(d, 1))
	}
	f.bodies[pathDeploys] = kubeList(deps...)
	f.bodies[pathAtelet] = map[string]any{"status": map[string]any{"numberReady": 3}}
	f.bodies[pathPools] = kubeList(kubePool("gv", 2, "gvisor"))
	f.bodies[pathSandboxes] = kubeList(map[string]any{"spec": map[string]any{"sandboxClass": "gvisor"}})
	f.bodies[pathRuntimes] = kubeList()
	f.bodies[substrateCertsGroupPath+"v1beta1"] = kubeResources("podcertificaterequests", "clustertrustbundles")
	f.bodies[pathNodes] = kubeList(kubeNode("n1", "6.12.0-207.el9uek.aarch64", false))
	return f
}

// clusterPreflighter returns a preflighter whose Control API half is healthy
// and whose cluster half talks to the fake, with the router half stubbed to
// a refused dial unless the test overrides it.
func clusterPreflighter(t *testing.T, f *fakeKubeAPI) (*SubstratePreflighter, SubstrateOptions) {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	bundle, ca, _ := selfSignedBundle(t, dir)
	token := filepath.Join(dir, "token")
	if err := os.WriteFile(token, []byte("sa-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := &fakeSubstrate{workers: []*substrateapi.Worker{worker("w", "active", nil, "gvisor")}}
	p := &SubstratePreflighter{
		newClient: fakeSubstrateFactory{fake: fake}.new,
		kube:      &substrateKube{server: srv.URL, tokenPath: token, caPath: ca, client: srv.Client()},
		routerDial: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("dial stubbed")
		},
	}
	opts := SubstrateOptions{Endpoint: "api.ate-system.svc:443", Token: "t", RouterClientBundle: bundle, RouterCAFile: ca}
	return p, opts
}

func TestSubstratePreflightClusterHealthyOnlyWarnsAboutRouter(t *testing.T) {
	f := healthyKube()
	p, opts := clusterPreflighter(t, f)
	got := p.Check(context.Background(), opts)
	if len(got) != 1 || !strings.Contains(got[0], "atenet-router") || !strings.Contains(got[0], "dial stubbed") {
		t.Fatalf("healthy cluster warnings = %q", got)
	}
	if !strings.Contains(strings.Join(f.calls, " "), pathNodes) {
		t.Errorf("node survey not reached: %v", f.calls)
	}
}

func TestSubstratePreflightClusterMissingCRDsStopsEarly(t *testing.T) {
	f := healthyKube()
	delete(f.bodies, pathCRDs+"workerpools.ate.dev")
	p, opts := clusterPreflighter(t, f)
	got := p.Check(context.Background(), opts)
	var crd int
	for _, w := range got {
		if strings.Contains(w, "CRD workerpools.ate.dev is not installed") {
			crd++
		}
		if strings.Contains(w, "deployment") || strings.Contains(w, "WorkerPool") {
			t.Errorf("operator checks ran after missing CRDs: %q", w)
		}
	}
	if crd != 1 {
		t.Errorf("CRD warnings = %d in %q", crd, got)
	}
}

func TestSubstratePreflightClusterForbiddenIsOneWarning(t *testing.T) {
	f := healthyKube()
	f.codes[pathDeploys] = http.StatusForbidden
	p, opts := clusterPreflighter(t, f)
	got := p.Check(context.Background(), opts)
	n := 0
	for _, w := range got {
		if strings.Contains(w, "403") && strings.Contains(w, "sandbox-substrate-preflight-rbac.yaml") {
			n++
		}
	}
	if n != 1 || len(got) != 2 {
		t.Errorf("forbidden warnings = %q", got)
	}
}

func TestSubstratePreflightClusterReadErrorIsAWarning(t *testing.T) {
	f := healthyKube()
	f.codes[pathPools] = http.StatusInternalServerError
	p, opts := clusterPreflighter(t, f)
	got := p.Check(context.Background(), opts)
	if len(got) != 2 || !strings.Contains(got[0], "could not be read") || !strings.Contains(got[0], "HTTP 500") {
		t.Errorf("read error warnings = %q", got)
	}

	f = healthyKube()
	f.bodies[pathPools] = "not json"
	p, opts = clusterPreflighter(t, f)
	if got := p.Check(context.Background(), opts); len(got) != 2 || !strings.Contains(got[0], "could not be read") {
		t.Errorf("bad json warnings = %q", got)
	}

	f = healthyKube()
	f.bodies[pathPools] = kubeList("not an object")
	p, opts = clusterPreflighter(t, f)
	if got := p.Check(context.Background(), opts); len(got) != 2 || !strings.Contains(got[0], "could not be read") {
		t.Errorf("bad item warnings = %q", got)
	}

	f = healthyKube()
	f.bodies[pathAtelet] = "not json"
	p, opts = clusterPreflighter(t, f)
	if got := p.Check(context.Background(), opts); len(got) != 2 || !strings.Contains(got[0], "daemonset atelet") {
		t.Errorf("bad daemonset warnings = %q", got)
	}

	f = healthyKube()
	f.bodies[substrateCertsGroupPath+"v1beta1"] = "not json"
	p, opts = clusterPreflighter(t, f)
	if got := p.Check(context.Background(), opts); len(got) != 2 || !strings.Contains(got[0], "certificates.k8s.io/v1beta1") {
		t.Errorf("bad discovery warnings = %q", got)
	}
}

func TestSubstratePreflightClusterOperatorAndPoolWarnings(t *testing.T) {
	f := healthyKube()
	f.bodies[pathDeploys] = kubeList(kubeDeploy("ate-api-server", 0), kubeDeploy("ate-controller", 1), kubeDeploy("atenet-router", 1))
	f.bodies[pathAtelet] = map[string]any{"status": map[string]any{"numberReady": 0}}
	f.bodies[pathPools] = kubeList(kubePool("gv", 0, "gvisor"))
	p, opts := clusterPreflighter(t, f)
	got := strings.Join(p.Check(context.Background(), opts), "\n")
	for _, want := range []string{
		"deployment ate-system/ate-api-server has no ready replica",
		"deployment ate-system/atenet-egress is not installed",
		"daemonset ate-system/atelet has no ready pod",
		"none of the 1 substrate WorkerPool(s) has a ready replica",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "ate-controller") || strings.Contains(got, "atenet-router is") {
		t.Errorf("healthy deployments warned:\n%s", got)
	}

	f = healthyKube()
	delete(f.bodies, pathAtelet)
	f.bodies[pathPools] = kubeList()
	p, opts = clusterPreflighter(t, f)
	got = strings.Join(p.Check(context.Background(), opts), "\n")
	if !strings.Contains(got, "daemonset ate-system/atelet is not installed") || !strings.Contains(got, "no substrate WorkerPool exists") {
		t.Errorf("missing atelet/pool warnings:\n%s", got)
	}
}

func TestSubstratePreflightClusterSandboxClassWarnings(t *testing.T) {
	// No gvisor SandboxConfig and no runsc RuntimeClass → warning.
	f := healthyKube()
	f.bodies[pathSandboxes] = kubeList()
	delete(f.bodies, pathRuntimes)
	p, opts := clusterPreflighter(t, f)
	got := strings.Join(p.Check(context.Background(), opts), "\n")
	if !strings.Contains(got, "cluster has no gVisor") {
		t.Errorf("missing gVisor warning:\n%s", got)
	}

	// A runsc RuntimeClass alone satisfies gVisor.
	f = healthyKube()
	f.bodies[pathSandboxes] = kubeList()
	f.bodies[pathRuntimes] = kubeList(map[string]any{"handler": "runsc"}, map[string]any{"handler": "kata"})
	p, opts = clusterPreflighter(t, f)
	if got := strings.Join(p.Check(context.Background(), opts), "\n"); strings.Contains(got, "gVisor") {
		t.Errorf("runsc RuntimeClass not honoured:\n%s", got)
	}

	// A microvm pool with no KVM node warns; a gvisor-less pool set does not
	// demand gVisor.
	f = healthyKube()
	f.bodies[pathPools] = kubeList(kubePool("vm", 1, "microvm"))
	f.bodies[pathSandboxes] = kubeList()
	p, opts = clusterPreflighter(t, f)
	got = strings.Join(p.Check(context.Background(), opts), "\n")
	if !strings.Contains(got, "no node advertises ate.dev/kvm") || strings.Contains(got, "no gVisor") {
		t.Errorf("microvm warnings:\n%s", got)
	}
	f.bodies[pathNodes] = kubeList(kubeNode("n1", "6.12.0", true))
	p, opts = clusterPreflighter(t, f)
	if got := strings.Join(p.Check(context.Background(), opts), "\n"); strings.Contains(got, "kvm") {
		t.Errorf("KVM node not honoured:\n%s", got)
	}
}

func TestSubstratePreflightClusterFeatureGatesAndKernels(t *testing.T) {
	f := healthyKube()
	delete(f.bodies, substrateCertsGroupPath+"v1beta1")
	f.bodies[pathNodes] = kubeList(kubeNode("old", "4.14.0-1", false), kubeNode("new", "5.15.0", false), kubeNode("odd", "weird", false))
	p, opts := clusterPreflighter(t, f)
	got := strings.Join(p.Check(context.Background(), opts), "\n")
	for _, want := range []string{"PodCertificateRequest API is not served", "ClusterTrustBundle API is not served", "1 node(s) run a kernel older", "old (4.14.0-1)"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}

	// GA v1 serving both resources is enough.
	f = healthyKube()
	delete(f.bodies, substrateCertsGroupPath+"v1beta1")
	f.bodies[substrateCertsGroupPath+"v1"] = kubeResources("PodCertificateRequests", "ClusterTrustBundles")
	p, opts = clusterPreflighter(t, f)
	if got := strings.Join(p.Check(context.Background(), opts), "\n"); strings.Contains(got, "not served") {
		t.Errorf("v1 discovery not honoured:\n%s", got)
	}
}

// init pins the Kubernetes token path to a file that does not exist so the
// Control-API-only tests behave the same on a laptop and in a CI Pod (where
// the runner has a real ServiceAccount token). Tests that want the
// in-cluster halves inject kube/routerDial or call inClusterToken.
func init() {
	substrateKubeTokenPath = filepath.Join(os.TempDir(), "hive-substrate-preflight-no-token-"+strconv.Itoa(os.Getpid()))
}

// inClusterToken makes the prover believe it runs in a Pod wired for
// Substrate: a Kubernetes token and a projected Substrate token both exist.
// It returns the Substrate token path to set as opts.TokenFile.
func inClusterToken(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	prev := substrateKubeTokenPath
	substrateKubeTokenPath = filepath.Join(dir, "k8s-token")
	ate := filepath.Join(dir, "ate-token")
	for _, f := range []string{substrateKubeTokenPath, ate} {
		if err := os.WriteFile(f, []byte("t"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { substrateKubeTokenPath = prev })
	return ate
}

func TestSubstratePreflightClusterSkippedOutsideCluster(t *testing.T) {
	fake := &fakeSubstrate{workers: []*substrateapi.Worker{worker("w", "active", nil, "gvisor")}}
	p := &SubstratePreflighter{newClient: fakeSubstrateFactory{fake: fake}.new}
	if got := p.Check(context.Background(), SubstrateOptions{Endpoint: "api:443"}); len(got) != 0 {
		t.Errorf("outside a cluster the kube/router halves must be silent: %q", got)
	}

	// In a Pod with no reachable API server the kube half reports once and
	// the router half reports unusable credentials, instead of staying silent.
	ate := inClusterToken(t)
	// Only one of the two markers present is still "outside".
	if substrateInCluster(SubstrateOptions{}) {
		t.Error("a Kubernetes token alone must not count as in-cluster")
	}
	opts := SubstrateOptions{Endpoint: "api:443", TokenFile: ate, RouterClientBundle: filepath.Join(t.TempDir(), "missing.pem")}
	got := p.probeCluster(context.Background(), opts)
	if len(got) != 1 || !strings.Contains(got[0], "cluster facts could not be read") {
		t.Errorf("in-cluster without an API server: %q", got)
	}
	if got := p.probeRouter(context.Background(), opts); len(got) != 1 || !strings.Contains(got[0], "not usable") {
		t.Errorf("in-cluster without a bundle: %q", got)
	}
}

func TestSubstratePreflightKubeClientErrors(t *testing.T) {
	k := &substrateKube{server: "https://127.0.0.1:1", tokenPath: "/nonexistent", caPath: "/nonexistent"}
	if _, err := k.get(context.Background(), "/x"); err == nil || !strings.Contains(err.Error(), "in-cluster CA") {
		t.Errorf("missing CA: %v", err)
	}
	dir := t.TempDir()
	bad := filepath.Join(dir, "ca.crt")
	_ = os.WriteFile(bad, []byte("not pem"), 0o600)
	k = &substrateKube{server: "https://127.0.0.1:1", tokenPath: "/nonexistent", caPath: bad}
	if _, err := k.get(context.Background(), "/x"); err == nil || !strings.Contains(err.Error(), "no certificates") {
		t.Errorf("bad CA: %v", err)
	}
	_, ca, _ := selfSignedBundle(t, dir)
	k = &substrateKube{server: "https://127.0.0.1:1", tokenPath: "/nonexistent", caPath: ca}
	if _, err := k.get(context.Background(), "/x"); err == nil {
		t.Error("refused dial did not error")
	}
	if k.client == nil {
		t.Error("client not cached")
	}
	k = &substrateKube{server: "http://[::1]:namedport", client: http.DefaultClient}
	if _, err := k.get(context.Background(), "/x"); err == nil {
		t.Error("bad URL did not error")
	}
}

func TestSubstrateOperatorNamespace(t *testing.T) {
	for in, want := range map[string]string{
		"api.ate-system.svc:443":                       "ate-system",
		"https://api.substrate-prod.svc.cluster.local": "substrate-prod",
		"grpc://api.hive-sub.svc:8080":                 "hive-sub",
		"":                                             "ate-system",
		"control.example.com:443":                      "ate-system",
		"api..svc:443":                                 "ate-system",
		"not a url":                                    "ate-system",
	} {
		if got := substrateOperatorNamespace(in); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
}

func TestSubstrateKernelLess(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"4.14.0-1", "4.14.77", true},
		{"4.14.77", "4.14.77", false},
		{"5.15.0-1.el8uek", "4.14.77", false},
		{"6.12.0-207.el9uek.aarch64", "4.14.77", false},
		{"4.9", "4.14.77", true},
		{"weird", "4.14.77", false},
		{"4.x.1", "4.14.77", false},
		{"5.15", "x", false},
	}
	for _, c := range cases {
		if got := substrateKernelLess(c.a, c.b); got != c.want {
			t.Errorf("substrateKernelLess(%q, %q) = %v", c.a, c.b, got)
		}
	}
}

// fakeRouterListener accepts TLS connections requiring a client certificate
// signed by the test CA and answers any CONNECT with 404, so the probe sees
// exactly what atenet-router would show an accepted or rejected client.
func fakeRouterListener(t *testing.T, cert tls.Certificate) net.Listener {
	t.Helper()
	pool := x509.NewCertPool()
	leaf, _ := x509.ParseCertificate(cert.Certificate[0])
	pool.AddCert(leaf)
	cfg := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				if _, err := http.ReadRequest(bufio.NewReader(c)); err != nil {
					return
				}
				_, _ = io.WriteString(c, "HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\n\r\n")
			}()
		}
	}()
	return ln
}

func TestSubstratePreflightRouter(t *testing.T) {
	dir := t.TempDir()
	bundle, ca, cert := selfSignedBundle(t, dir)
	token := filepath.Join(dir, "token")
	_ = os.WriteFile(token, []byte("t"), 0o600)
	ln := fakeRouterListener(t, cert)
	kube := &substrateKube{server: "http://127.0.0.1:1", tokenPath: token, caPath: ca, client: http.DefaultClient}
	p := &SubstratePreflighter{kube: kube}
	ctx := context.Background()

	// Good bundle, router verifying it → quiet.
	opts := SubstrateOptions{RouterURL: "https://" + ln.Addr().String(), RouterClientBundle: bundle, RouterCAFile: ca}
	if got := p.probeRouter(ctx, opts); len(got) != 0 {
		t.Errorf("healthy router warned: %q", got)
	}

	// Unusable bundle → credentials warning.
	bad := opts
	bad.RouterClientBundle = filepath.Join(dir, "missing.pem")
	if got := p.probeRouter(ctx, bad); len(got) != 1 || !strings.Contains(got[0], "client credentials are not usable") {
		t.Errorf("missing bundle: %q", got)
	}

	// Router rejects the client certificate: a second CA the router does
	// not trust. TLS 1.3 surfaces that on the first read, not the handshake.
	otherDir := t.TempDir()
	otherBundle, _, _ := selfSignedBundle(t, otherDir)
	rejected := opts
	rejected.RouterClientBundle = otherBundle
	if got := p.probeRouter(ctx, rejected); len(got) != 1 || !strings.Contains(got[0], "rejected hive's client certificate") || !strings.Contains(got[0], otherBundle) {
		t.Errorf("rejected cert: %q", got)
	}

	// Unreachable router.
	down := opts
	down.RouterURL = "https://127.0.0.1:1"
	if got := p.probeRouter(ctx, down); len(got) != 1 || !strings.Contains(got[0], "is unreachable") {
		t.Errorf("unreachable: %q", got)
	}

	// Injected dial wins over the real one; default bundle path is named
	// when none is configured.
	stub := &SubstratePreflighter{kube: kube, routerDial: func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("stubbed") }}
	if got := stub.probeRouter(ctx, opts); len(got) != 1 || !strings.Contains(got[0], "stubbed") {
		t.Errorf("stubbed dial: %q", got)
	}
	if substrateRouterBundlePath(SubstrateOptions{}) != SubstrateDefaultRouterClientBundle {
		t.Error("default bundle path")
	}

	// Outside a cluster the router half is silent.
	if got := (&SubstratePreflighter{}).probeRouter(ctx, opts); len(got) != 0 {
		t.Errorf("outside cluster: %q", got)
	}
}
