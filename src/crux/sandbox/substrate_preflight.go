package sandbox

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/crux/sandbox/substrateapi"
)

// Substrate cluster-prerequisite preflight (ADR-0026 phase 2; the sibling
// of the OpenShell preflight in openshell_preflight.go). The Control API
// answers whether it is up and which workers — and so which sandbox classes
// — exist; that is what decides whether a kick can land. The cluster and
// router halves in substrate_preflight_cluster.go (#17698) add the
// Kubernetes facts behind that answer when hive runs in the cluster.

const substratePreflightTTL = 5 * time.Minute

type substratePreflightEntry struct {
	at       time.Time
	warnings []string
}

// SubstratePreflighter checks a control plane and remembers the answer for a
// TTL, so the dashboard polling the same warnings does not hit it per call.
type SubstratePreflighter struct {
	mu    sync.Mutex
	cache map[string]substratePreflightEntry

	newClient func(SubstrateOptions) (substrateClient, error)
	now       func() time.Time
	// kube and routerDial let tests point the cluster and router halves at
	// fakes; nil means the in-cluster API and a plain TCP dial.
	kube       *substrateKube
	routerDial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// DefaultSubstratePreflighter backs the warnings AgentSandboxGateWarnings emits.
var DefaultSubstratePreflighter = &SubstratePreflighter{}

// Check returns the warnings for one control plane. Never an error: an
// unreachable control plane is itself a warning.
func (p *SubstratePreflighter) Check(ctx context.Context, opts SubstrateOptions) []string {
	now := time.Now
	if p.now != nil {
		now = p.now
	}
	key := opts.endpoint() + "\x00" + opts.atespace() + "\x00" + opts.Token
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.cache[key]; ok && now().Sub(e.at) < substratePreflightTTL {
		return append([]string(nil), e.warnings...)
	}
	warnings := p.probe(ctx, opts)
	if p.cache == nil {
		p.cache = map[string]substratePreflightEntry{}
	}
	p.cache[key] = substratePreflightEntry{at: now(), warnings: warnings}
	return append([]string(nil), warnings...)
}

// probe runs the three halves — Control API, cluster facts, router — and
// concatenates their warnings; a down control plane does not hide the
// cluster facts that explain it.
func (p *SubstratePreflighter) probe(ctx context.Context, opts SubstrateOptions) []string {
	out := p.probeControl(ctx, opts)
	out = append(out, p.probeCluster(ctx, opts)...)
	return append(out, p.probeRouter(ctx, opts)...)
}

func (p *SubstratePreflighter) probeControl(ctx context.Context, opts SubstrateOptions) []string {
	ep := opts.endpoint()
	ctx, cancel := context.WithTimeout(ctx, substrateRequestWait)
	defer cancel()

	newClient := newSubstrateGRPCClient
	if p.newClient != nil {
		newClient = p.newClient
	}
	client, err := newClient(opts)
	if err != nil {
		return []string{fmt.Sprintf("substrate control api %s is unreachable (%v); every kick on the substrate runtime will fail", ep, err)}
	}
	defer func() { _ = client.Close() }()

	workers, err := client.Control().ListWorkers(ctx)
	if err != nil {
		return []string{fmt.Sprintf("substrate control api %s is unreachable (%v); every kick on the substrate runtime will fail", ep, err)}
	}
	active, classes := substrateActiveWorkers(workers, opts.WorkerSelector)
	if active == 0 {
		if len(opts.WorkerSelector) > 0 {
			return []string{fmt.Sprintf("substrate control api %s has no active worker matching worker_selector %v; actors will never be scheduled — check the WorkerPool labels and that its pods are running", ep, opts.WorkerSelector)}
		}
		return []string{fmt.Sprintf("substrate control api %s reports no active workers; actors will never be scheduled — install a WorkerPool (see deploy/substrate.md) and check its pods are running", ep)}
	}
	var out []string
	if len(classes) == 0 {
		out = append(out, fmt.Sprintf("substrate control api %s has %d active worker(s) but none reports a sandbox class; the WorkerPool's SandboxConfig may not have resolved (kubectl get workerpool -A)", ep, active))
	}
	return out
}

// substrateActiveWorkers counts the ACTIVE workers matching the selector and
// returns the sorted set of sandbox classes they advertise.
func substrateActiveWorkers(workers []*substrateapi.Worker, selector map[string]string) (int, []string) {
	active := 0
	classSet := map[string]bool{}
	for _, w := range workers {
		if w.GetStatus().GetState() != substrateapi.WorkerState_WORKER_STATE_ACTIVE || !substrateLabelsMatch(w.GetLabels(), selector) {
			continue
		}
		active++
		for _, c := range w.GetStatus().GetSandboxClasses() {
			if c.GetSandboxClass() != "" {
				classSet[strings.ToLower(c.GetSandboxClass())] = true
			}
		}
	}
	classes := make([]string, 0, len(classSet))
	for c := range classSet {
		classes = append(classes, c)
	}
	sort.Strings(classes)
	return active, classes
}

func substrateLabelsMatch(labels, selector map[string]string) bool {
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}
