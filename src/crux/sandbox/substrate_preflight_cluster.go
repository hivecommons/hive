package sandbox

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/crux/sandbox/substrateapi"
)

// Cluster-level half of the Substrate preflight (hivecommons/hive#17698).
// The Control API half in substrate_preflight.go answers "is the control
// plane up and does it have workers"; this half reads the Kubernetes facts
// that explain *why* it is not, or why a kick would fail even though it is:
// the operator's CRDs and Deployments, a ready WorkerPool for the class the
// operator selected, gVisor (a SandboxConfig of class gvisor, or a runsc
// RuntimeClass) or KVM for microVMs, the certificates.k8s.io APIs behind the
// PodCertificateRequest and ClusterTrustBundle feature gates Substrate's
// SPIFFE identities need, the node kernels, and whether atenet-router
// accepts hive's client certificate. Everything is read-only, bounded by one
// request timeout per call, and skipped entirely when hive is not running in
// a cluster (no projected ServiceAccount token), where the Control API probe
// is all there is to check.

const (
	// SubstrateDefaultOperatorNamespace is where ate-setup installs the
	// control plane when the operator does not rename it.
	SubstrateDefaultOperatorNamespace = "ate-system"

	substrateKubeAPIServer = "https://kubernetes.default.svc"
	substrateKubeCAPath    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	substrateKubeMaxBody   = 8 << 20
	substrateKubeNodeLimit = 500

	substrateClassGVisor   = "gvisor"
	substrateClassMicroVM  = "microvm"
	substrateGVisorHandler = "runsc"
	// substrateKVMResource is the extended resource atelet's device plugin
	// advertises on nodes with /dev/kvm; the microVM class schedules on it.
	substrateKVMResource = "ate.dev/kvm"
	// substrateGVisorMinKernel is gVisor's documented host kernel floor.
	substrateGVisorMinKernel = "4.14.77"

	substrateAteletDaemonSet = "atelet"
	substrateCertsGroupPath  = "/apis/certificates.k8s.io/"
	substrateResourcePCR     = "podcertificaterequests"
	substrateResourceCTB     = "clustertrustbundles"
)

// substrateCRDs are the cluster-scoped CRDs ate-setup installs; Actors and
// ActorTemplates are Control API resources, not CRDs, so they are not here.
var substrateCRDs = []string{"workerpools.ate.dev", "sandboxconfigs.ate.dev"}

// substrateOperatorDeployments must each have a ready replica for a kick to
// create an actor, schedule it, reach its bridge, and egress to GitHub.
var substrateOperatorDeployments = []string{"ate-api-server", "ate-controller", "atenet-router", "atenet-egress"}

// substrateCertsVersions are the certificates.k8s.io versions that may serve
// PodCertificateRequest and ClusterTrustBundle (beta in 1.34, GA later).
var substrateCertsVersions = []string{"v1", "v1beta1"}

// substrateKubeTokenPath is the projected Kubernetes ServiceAccount token
// the kube client authenticates with. A variable so tests pin the
// environment instead of inheriting the runner's (CI itself runs in Pods).
var substrateKubeTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"

// substrateInCluster reports whether hive runs in a Pod wired for Substrate:
// both the Kubernetes ServiceAccount token and hive's projected Substrate
// token exist. The kube and router halves of the preflight only run then;
// a laptop, or a CI Pod without the Substrate projection, gets the
// Control-API probe alone.
func substrateInCluster(opts SubstrateOptions) bool {
	if _, err := os.Stat(substrateKubeTokenPath); err != nil {
		return false
	}
	_, err := os.Stat(opts.tokenFile())
	return err == nil
}

// substrateKube is the minimal read-only in-cluster API client the prover
// needs; kubejob has its own because importing it here would be a cycle.
type substrateKube struct {
	server    string
	tokenPath string
	caPath    string
	client    *http.Client
}

// errSubstrateKubeForbidden marks a 403: the probe stops after one warning
// rather than repeating the same RBAC gap per resource.
var errSubstrateKubeForbidden = errors.New("forbidden")

// errSubstrateKubeNotFound marks a 404.
var errSubstrateKubeNotFound = errors.New("not found")

func (k *substrateKube) get(ctx context.Context, path string) ([]byte, error) {
	client, err := k.httpClient()
	if err != nil {
		return nil, err
	}
	reqCtx, cancel := context.WithTimeout(ctx, substrateRequestWait)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, k.server+path, nil)
	if err != nil {
		return nil, err
	}
	if raw, err := os.ReadFile(k.tokenPath); err == nil {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(raw)))
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, substrateKubeMaxBody))
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusForbidden:
		return nil, errSubstrateKubeForbidden
	case resp.StatusCode == http.StatusNotFound:
		return nil, errSubstrateKubeNotFound
	case resp.StatusCode/100 != 2:
		return nil, fmt.Errorf("GET %s: HTTP %d", path, resp.StatusCode)
	}
	return body, nil
}

func (k *substrateKube) httpClient() (*http.Client, error) {
	if k.client != nil {
		return k.client, nil
	}
	pem, err := os.ReadFile(k.caPath)
	if err != nil {
		return nil, fmt.Errorf("reading in-cluster CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("in-cluster CA bundle contained no certificates")
	}
	k.client = &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	return k.client, nil
}

// substrateOperatorNamespace derives the operator namespace from the Control
// API endpoint (`api.<ns>.svc[:port]`, with or without a scheme); any other
// shape means the default install.
func substrateOperatorNamespace(endpoint string) string {
	addr, _, err := substrateAddress(endpoint)
	if err != nil {
		return SubstrateDefaultOperatorNamespace
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	parts := strings.Split(host, ".")
	if len(parts) >= 3 && parts[0] == "api" && parts[2] == "svc" && parts[1] != "" {
		return parts[1]
	}
	return SubstrateDefaultOperatorNamespace
}

// Minimal projections of the API objects the prover reads.

type substrateKubeList struct {
	Items []json.RawMessage `json:"items"`
}

type substrateKubeWorkload struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		SandboxClass   string `json:"sandboxClass"`
		SandboxClasses []struct {
			Name string `json:"name"`
		} `json:"sandboxClasses"`
	} `json:"spec"`
	Status struct {
		ReadyReplicas int `json:"readyReplicas"`
		NumberReady   int `json:"numberReady"`
	} `json:"status"`
	Handler string `json:"handler"`
}

type substrateKubeNode struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Status struct {
		Allocatable map[string]string `json:"allocatable"`
		NodeInfo    struct {
			KernelVersion string `json:"kernelVersion"`
		} `json:"nodeInfo"`
	} `json:"status"`
}

type substrateKubeResourceList struct {
	Resources []struct {
		Name string `json:"name"`
	} `json:"resources"`
}

// clusterProbe holds one probe's state so the step functions stay short.
type substrateClusterProbe struct {
	kube *substrateKube
	ns   string
	sel  map[string]string
	out  []string
	// classes are the sandbox classes the ready WorkerPools offer; gvisor and
	// kvm record whether the cluster can run each class.
	classes map[string]bool
	gvisor  bool
	kvm     bool
}

// substrateKubeItems GETs a collection and decodes each item into T.
func substrateKubeItems[T any](ctx context.Context, k *substrateKube, path string) ([]T, error) {
	body, err := k.get(ctx, path)
	if err != nil {
		return nil, err
	}
	var list substrateKubeList
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	items := make([]T, 0, len(list.Items))
	for _, raw := range list.Items {
		var item T
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, fmt.Errorf("GET %s: %w", path, err)
		}
		items = append(items, item)
	}
	return items, nil
}

// warnf appends one warning.
func (c *substrateClusterProbe) warnf(format string, args ...any) {
	c.out = append(c.out, fmt.Sprintf(format, args...))
}

// probeCluster runs the Kubernetes checks and returns their warnings. A 403
// anywhere ends the probe with one warning naming the RBAC the prover needs.
func (p *SubstratePreflighter) probeCluster(ctx context.Context, opts SubstrateOptions) []string {
	kube := p.kube
	if kube == nil {
		if !substrateInCluster(opts) {
			return nil
		}
		kube = &substrateKube{server: substrateKubeAPIServer, tokenPath: substrateKubeTokenPath, caPath: substrateKubeCAPath}
	}
	c := &substrateClusterProbe{kube: kube, ns: substrateOperatorNamespace(opts.endpoint()), sel: opts.WorkerSelector, classes: map[string]bool{}}
	steps := []func(context.Context) error{c.crds, c.operator, c.pools, c.sandboxClasses, c.featureGates, c.nodes}
	for _, step := range steps {
		if err := step(ctx); err != nil {
			if errors.Is(err, errSubstrateCRDsMissing) {
				return c.out
			}
			if errors.Is(err, errSubstrateKubeForbidden) {
				return append(c.out, "hive's ServiceAccount may not read the Substrate cluster facts (403); the operator, WorkerPool, gVisor and feature-gate checks were skipped — apply deploy/k8s/sandbox-substrate-preflight-rbac.yaml")
			}
			c.warnf("substrate cluster facts could not be read (%v); the operator, WorkerPool, gVisor and feature-gate checks were skipped", err)
			return c.out
		}
	}
	return c.out
}

// crds checks the operator's CRDs are installed; missing ones end the probe
// early, because every later check would just restate "not installed".
func (c *substrateClusterProbe) crds(ctx context.Context) error {
	missing := 0
	for _, name := range substrateCRDs {
		_, err := c.kube.get(ctx, "/apis/apiextensions.k8s.io/v1/customresourcedefinitions/"+name)
		switch {
		case err == nil:
		case errors.Is(err, errSubstrateKubeNotFound):
			missing++
			c.warnf("substrate CRD %s is not installed; the Substrate operator is absent and every kick on the substrate runtime will fail — run `ate-setup deploy ate-system` (see deploy/substrate.md)", name)
		default:
			return err
		}
	}
	if missing > 0 {
		return errSubstrateCRDsMissing
	}
	return nil
}

// errSubstrateCRDsMissing stops the probe after the CRD warnings.
var errSubstrateCRDsMissing = errors.New("crds missing")

// operator checks each control-plane Deployment and the atelet DaemonSet
// have a ready replica in the operator namespace.
func (c *substrateClusterProbe) operator(ctx context.Context) error {
	deps, err := substrateKubeItems[substrateKubeWorkload](ctx, c.kube, "/apis/apps/v1/namespaces/"+c.ns+"/deployments")
	if err != nil {
		return err
	}
	ready := map[string]int{}
	for _, d := range deps {
		ready[d.Metadata.Name] = d.Status.ReadyReplicas
	}
	for _, name := range substrateOperatorDeployments {
		n, ok := ready[name]
		switch {
		case !ok:
			c.warnf("substrate operator deployment %s/%s is not installed; run `ate-setup deploy ate-system` (see deploy/substrate.md)", c.ns, name)
		case n == 0:
			c.warnf("substrate operator deployment %s/%s has no ready replica; kicks on the substrate runtime will fail until it is up (kubectl -n %s describe deploy %s)", c.ns, name, c.ns, name)
		}
	}
	body, err := c.kube.get(ctx, "/apis/apps/v1/namespaces/"+c.ns+"/daemonsets/"+substrateAteletDaemonSet)
	switch {
	case errors.Is(err, errSubstrateKubeNotFound):
		c.warnf("substrate node agent daemonset %s/%s is not installed; no node can host a worker — run `ate-setup deploy ate-system`", c.ns, substrateAteletDaemonSet)
		return nil
	case err != nil:
		return err
	}
	var ds substrateKubeWorkload
	if err := json.Unmarshal(body, &ds); err != nil {
		return fmt.Errorf("daemonset %s: %w", substrateAteletDaemonSet, err)
	}
	if ds.Status.NumberReady == 0 {
		c.warnf("substrate node agent daemonset %s/%s has no ready pod; workers cannot start (kubectl -n %s describe ds %s)", c.ns, substrateAteletDaemonSet, c.ns, substrateAteletDaemonSet)
	}
	return nil
}

// pools checks at least one WorkerPool has a ready replica and records the
// sandbox classes those pools offer.
func (c *substrateClusterProbe) pools(ctx context.Context) error {
	pools, err := substrateKubeItems[substrateKubeWorkload](ctx, c.kube, "/apis/ate.dev/v1/workerpools")
	if err != nil {
		return err
	}
	ready := 0
	for _, p := range pools {
		if p.Status.ReadyReplicas == 0 {
			continue
		}
		ready++
		for _, sc := range p.Spec.SandboxClasses {
			c.classes[strings.ToLower(sc.Name)] = true
		}
	}
	if len(pools) == 0 {
		c.warnf("no substrate WorkerPool exists; actors have nowhere to run — create one (see deploy/substrate.md)")
	} else if ready == 0 {
		c.warnf("none of the %d substrate WorkerPool(s) has a ready replica; actors will never be scheduled (kubectl get workerpools -A)", len(pools))
	}
	return nil
}

// sandboxClasses checks the cluster can actually run the classes the pools
// offer: gVisor via a SandboxConfig of class gvisor (atelet fetches runsc
// itself) or a runsc RuntimeClass; microVMs via atelet's ate.dev/kvm
// resource on at least one node (checked in nodes).
func (c *substrateClusterProbe) sandboxClasses(ctx context.Context) error {
	cfgs, err := substrateKubeItems[substrateKubeWorkload](ctx, c.kube, "/apis/ate.dev/v1/sandboxconfigs")
	if err != nil {
		return err
	}
	for _, cfg := range cfgs {
		if strings.EqualFold(cfg.Spec.SandboxClass, substrateClassGVisor) {
			c.gvisor = true
		}
	}
	rcs, err := substrateKubeItems[substrateKubeWorkload](ctx, c.kube, "/apis/node.k8s.io/v1/runtimeclasses")
	if err != nil && !errors.Is(err, errSubstrateKubeNotFound) {
		return err
	}
	for _, rc := range rcs {
		if rc.Handler == substrateGVisorHandler {
			c.gvisor = true
		}
	}
	wantGVisor := c.classes[substrateClassGVisor] || len(c.classes) == 0
	if wantGVisor && !c.gvisor {
		c.warnf("cluster has no gVisor: no SandboxConfig with sandboxClass %s and no RuntimeClass with handler %s; Substrate refuses plain containers, so a gvisor WorkerPool cannot start workers — `ate-setup deploy ate-system` installs the gvisor-default SandboxConfig", substrateClassGVisor, substrateGVisorHandler)
	}
	return nil
}

// featureGates checks the certificates.k8s.io APIs that only exist when the
// PodCertificateRequest and ClusterTrustBundle feature gates are on; without
// them atelet, atenet-router and hive's own router client get no SPIFFE
// certificates and every CONNECT to an actor fails.
func (c *substrateClusterProbe) featureGates(ctx context.Context) error {
	served := map[string]bool{}
	for _, v := range substrateCertsVersions {
		body, err := c.kube.get(ctx, substrateCertsGroupPath+v)
		if errors.Is(err, errSubstrateKubeNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		var rl substrateKubeResourceList
		if err := json.Unmarshal(body, &rl); err != nil {
			return fmt.Errorf("certificates.k8s.io/%s: %w", v, err)
		}
		for _, r := range rl.Resources {
			served[strings.ToLower(r.Name)] = true
		}
	}
	if !served[substrateResourcePCR] {
		c.warnf("the PodCertificateRequest API is not served; Substrate's SPIFFE identities (atelet, atenet-router, hive's router client) cannot be issued — enable the PodCertificateRequest feature gate and certificates.k8s.io/v1beta1 on the API server and kubelets (Kubernetes ≥ 1.34)")
	}
	if !served[substrateResourceCTB] {
		c.warnf("the ClusterTrustBundle API is not served; the Control API and router CA bundles cannot be projected into hive — enable the ClusterTrustBundle feature gate and certificates.k8s.io/v1beta1 on the API server and kubelets (Kubernetes ≥ 1.34)")
	}
	return nil
}

// nodes surveys the node kernels against gVisor's floor and records whether
// any node advertises KVM for the microVM class.
func (c *substrateClusterProbe) nodes(ctx context.Context) error {
	nodes, err := substrateKubeItems[substrateKubeNode](ctx, c.kube, "/api/v1/nodes?limit="+strconv.Itoa(substrateKubeNodeLimit))
	if err != nil {
		return err
	}
	var old []string
	for _, n := range nodes {
		if _, ok := n.Status.Allocatable[substrateKVMResource]; ok {
			c.kvm = true
		}
		if kv := n.Status.NodeInfo.KernelVersion; kv != "" && substrateKernelLess(kv, substrateGVisorMinKernel) {
			old = append(old, n.Metadata.Name+" ("+kv+")")
		}
	}
	if len(old) > 0 {
		sort.Strings(old)
		c.warnf("%d node(s) run a kernel older than gVisor's %s floor and cannot host a gvisor worker: %s", len(old), substrateGVisorMinKernel, strings.Join(old, ", "))
	}
	if c.classes[substrateClassMicroVM] && !c.kvm {
		c.warnf("a WorkerPool offers the %s class but no node advertises %s; microVM workers need /dev/kvm (KVM-capable shapes) and atelet's device plugin", substrateClassMicroVM, substrateKVMResource)
	}
	return nil
}

// substrateKernelLess reports whether kernel release a (e.g. 5.15.0-1 or
// 6.12.0-207.el9uek.aarch64) is older than the dotted floor b. A release the
// prover cannot parse is never reported as old.
func substrateKernelLess(a, b string) bool {
	pa, oka := substrateKernelParts(a)
	pb, okb := substrateKernelParts(b)
	if !oka || !okb {
		return false
	}
	for i := range pa {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return false
}

func substrateKernelParts(s string) ([3]int, bool) {
	var out [3]int
	s, _, _ = strings.Cut(s, "-")
	parts := strings.Split(s, ".")
	if len(parts) < 2 {
		return out, false
	}
	for i := 0; i < 3 && i < len(parts); i++ {
		n, err := strconv.Atoi(parts[i])
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// substrateRouterProbeActor is the (nonexistent) actor the preflight names
// in its CONNECT: the router answers with an HTTP status once it has
// accepted hive's client certificate, and with a TLS alert when it has not.
const substrateRouterProbeActor = "hive-preflight"

// probeRouter checks hive can reach atenet-router and that the router
// accepts its projected client bundle: the same mTLS CONNECT the kick's
// bridge tunnel makes, aimed at an actor that does not exist. Any HTTP
// answer means the certificate was accepted (TLS 1.3 reports a rejected
// client certificate only after the handshake, so the handshake alone is
// not proof). Skipped outside a cluster, where neither the bundle nor the
// router exist.
func (p *SubstratePreflighter) probeRouter(ctx context.Context, opts SubstrateOptions) []string {
	if p.kube == nil && !substrateInCluster(opts) {
		return nil
	}
	bridge, err := newRouterBridge(opts)
	if err != nil {
		return []string{fmt.Sprintf("atenet-router client credentials are not usable (%v); every kick on the substrate runtime will fail at the bridge tunnel", err)}
	}
	if p.routerDial != nil {
		bridge.dial = p.routerDial
	}
	ctx, cancel := context.WithTimeout(ctx, substrateRequestWait)
	defer cancel()
	ref := &substrateapi.ObjectRef{Atespace: opts.atespace(), Name: substrateRouterProbeActor}
	conn, err := bridge.tunnel(ctx, ref)
	if conn != nil {
		_ = conn.Close()
	}
	switch {
	case err == nil, strings.Contains(err.Error(), "router refused connect"):
		return nil
	case strings.Contains(err.Error(), "router tls handshake") || strings.Contains(err.Error(), "router connect"):
		return []string{fmt.Sprintf("atenet-router %s rejected hive's client certificate (%v); check the podCertificate projection at %s and --ingress-allowed-spiffe-ids on the router", bridge.routerAddr, err, substrateRouterBundlePath(opts))}
	}
	return []string{fmt.Sprintf("atenet-router %s is unreachable (%v); every kick on the substrate runtime will fail at the bridge tunnel — check the atenet-router Service and NetworkPolicies", bridge.routerAddr, err)}
}

func substrateRouterBundlePath(opts SubstrateOptions) string {
	if opts.RouterClientBundle != "" {
		return opts.RouterClientBundle
	}
	return SubstrateDefaultRouterClientBundle
}
