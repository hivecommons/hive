package spoke

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Per-cluster host aliases, delivered by the hub on the heartbeat (#18637).
//
// The hub's clusters.json can pin hostnames (GitHub Enterprise behind a TLS
// reverse proxy) to a fixed IP for every hive on a cluster. The provisioning
// template renders them, and the hub's reconcile sweep patches existing
// Deployments, but only on clusters the hub can reach with kubectl. On a
// push-reported (pull_only) cluster such as vllm-d or a-ks-wec2 the hub never
// touches the Deployment, so a spoke provisioned before the aliases existed
// kept resolving the GHE host directly and timed out. The heartbeat is the one
// channel that reaches every spoke, so the hub sends the desired aliases there
// and the spoke converges its own Deployment, exactly like the startup-probe
// and CPU-tier self-patches.

const (
	// hostAliasesSelfCheckInterval throttles the self-check. The heartbeat
	// beats far more often than aliases change; one GET per interval still
	// converges manual drift within the interval. A change in the desired set
	// is checked on the next beat regardless.
	hostAliasesSelfCheckInterval = 15 * time.Minute

	// podHostAliasesJSONPointer is the JSON-Patch path of the pod hostAliases.
	podHostAliasesJSONPointer = "/spec/template/spec/hostAliases"

	// jsonPatchContentType is the PATCH content type for an RFC 6902 patch.
	// A strategic merge cannot express "drop this entry", which exact
	// convergence needs.
	jsonPatchContentType = "application/json-patch+json"
)

// HostAlias is one Pod hostAliases entry. The hub's ClusterHostAlias is this
// type, so clusters.json, the heartbeat and the Deployment share one shape.
type HostAlias struct {
	IP        string   `json:"ip" yaml:"ip"`
	Hostnames []string `json:"hostnames" yaml:"hostnames"`
}

func normalizeAliasHost(h string) string { return strings.ToLower(strings.TrimSpace(h)) }

// MergeHostAliases returns live with want applied. Every desired IP carries
// exactly the desired hostnames; a desired hostname is removed from entries
// for any other IP (so it resolves only where clusters.json says), and an
// entry left with no hostnames is dropped. Entries unrelated to want, such as
// an operator's alias for another host, are kept. Output is sorted by IP so
// the patch is deterministic.
func MergeHostAliases(live, want []HostAlias) []HostAlias {
	wantIPs := map[string]bool{}
	wantHosts := map[string]bool{}
	for _, a := range want {
		wantIPs[strings.TrimSpace(a.IP)] = true
		for _, h := range a.Hostnames {
			wantHosts[normalizeAliasHost(h)] = true
		}
	}
	out := make([]HostAlias, 0, len(live)+len(want))
	for _, a := range live {
		if wantIPs[strings.TrimSpace(a.IP)] {
			continue
		}
		var keep []string
		for _, h := range a.Hostnames {
			if n := normalizeAliasHost(h); n != "" && !wantHosts[n] {
				keep = append(keep, h)
			}
		}
		if len(keep) == 0 {
			continue
		}
		out = append(out, HostAlias{IP: a.IP, Hostnames: keep})
	}
	out = append(out, want...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].IP < out[j].IP })
	return out
}

// hostAliasPairs flattens aliases to a set of "ip|hostname" pairs, ignoring
// order, case and surrounding space.
func hostAliasPairs(in []HostAlias) map[string]bool {
	set := map[string]bool{}
	for _, a := range in {
		for _, h := range a.Hostnames {
			if n := normalizeAliasHost(h); n != "" {
				set[strings.TrimSpace(a.IP)+"|"+n] = true
			}
		}
	}
	return set
}

// HostAliasesConverged reports whether live already equals MergeHostAliases
// (live, want) as a set of ip/hostname pairs, so the check and the patch can
// never disagree and a converged Deployment is never patched again. No
// desired aliases is always converged: a cluster without host_aliases is
// left alone.
func HostAliasesConverged(live, want []HostAlias) bool {
	if len(want) == 0 {
		return true
	}
	have := hostAliasPairs(live)
	need := hostAliasPairs(MergeHostAliases(live, want))
	if len(have) != len(need) {
		return false
	}
	for k := range need {
		if !have[k] {
			return false
		}
	}
	return true
}

// sanitizeHostAliases drops entries a Deployment would reject (unparseable IP,
// no hostnames) and lowercases hostnames. The hub already validates
// clusters.json; this keeps a malformed beat from producing a bad patch.
func sanitizeHostAliases(in []HostAlias) []HostAlias {
	var out []HostAlias
	for _, a := range in {
		ip := strings.TrimSpace(a.IP)
		if net.ParseIP(ip) == nil {
			continue
		}
		var names []string
		for _, h := range a.Hostnames {
			if n := normalizeAliasHost(h); n != "" {
				names = append(names, n)
			}
		}
		if len(names) > 0 {
			out = append(out, HostAlias{IP: ip, Hostnames: names})
		}
	}
	return out
}

// EnsureHostAliasesSelf converges this pod's own Deployment onto want. It
// returns patched=true when a patch was issued (the pod then rolls once).
// Off-cluster (no service-account token) or an empty want is a no-op.
func EnsureHostAliasesSelf(logger *slog.Logger, want []HostAlias) (patched bool, err error) {
	want = sanitizeHostAliases(want)
	if len(want) == 0 {
		return false, nil
	}
	if _, statErr := os.Stat(k8sTokenPath); statErr != nil {
		return false, nil
	}
	ns, err := os.ReadFile(k8sNamespacePath)
	if err != nil {
		return false, fmt.Errorf("reading namespace: %w", err)
	}
	namespace := strings.TrimSpace(string(ns))
	if namespace == "" {
		return false, fmt.Errorf("empty namespace from %s", k8sNamespacePath)
	}
	path := fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments/%s", namespace, selfUpgradeDeployName)

	body, err := k8sAPIGet(path)
	if err != nil {
		return false, fmt.Errorf("reading own deployment: %w", err)
	}
	var dep struct {
		Spec struct {
			Template struct {
				Spec struct {
					HostAliases []HostAlias `json:"hostAliases"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(body, &dep); err != nil {
		return false, fmt.Errorf("decoding own deployment: %w", err)
	}
	live := dep.Spec.Template.Spec.HostAliases
	if HostAliasesConverged(live, want) {
		return false, nil
	}
	merged := MergeHostAliases(live, want)
	patch, err := json.Marshal([]map[string]any{{
		"op": "add", "path": podHostAliasesJSONPointer, "value": merged,
	}})
	if err != nil {
		return false, fmt.Errorf("encoding host aliases patch: %w", err)
	}
	if err := k8sAPIPatchWithContentType(path, patch, jsonPatchContentType); err != nil {
		return false, fmt.Errorf("patching deployment host aliases: %w", err)
	}
	logger.Info("deployment patched to the cluster host aliases from the hub; pod will roll",
		"namespace", namespace, "aliases", len(merged))
	return true, nil
}

// hostAliasesSelfState throttles the heartbeat-driven self-check and keeps at
// most one check in flight.
var hostAliasesSelfState struct {
	mu        sync.Mutex
	running   bool
	lastKey   string
	lastCheck time.Time
}

// ensureHostAliasesSelfFn is swapped in tests.
var ensureHostAliasesSelfFn = EnsureHostAliasesSelf

// hostAliasesNow is swapped in tests.
var hostAliasesNow = time.Now

// hostAliasesKey identifies a desired alias set independent of order.
func hostAliasesKey(want []HostAlias) string {
	pairs := hostAliasPairs(want)
	keys := make([]string, 0, len(pairs))
	for k := range pairs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

// claimHostAliasesCheck reports whether a self-check should start now and, if
// so, marks it running. A new desired set is checked at once; an unchanged one
// at most every hostAliasesSelfCheckInterval.
func claimHostAliasesCheck(key string) bool {
	st := &hostAliasesSelfState
	st.mu.Lock()
	defer st.mu.Unlock()
	now := hostAliasesNow()
	if st.running || (key == st.lastKey && now.Sub(st.lastCheck) < hostAliasesSelfCheckInterval) {
		return false
	}
	st.running = true
	st.lastKey = key
	st.lastCheck = now
	return true
}

func releaseHostAliasesCheck() {
	hostAliasesSelfState.mu.Lock()
	hostAliasesSelfState.running = false
	hostAliasesSelfState.mu.Unlock()
}

// runHostAliasesCheck performs one self-check and logs a WARN when the
// aliases cannot be applied (RBAC, API unreachable), naming the namespace so
// the operator can find the spoke.
func runHostAliasesCheck(logger *slog.Logger, want []HostAlias) {
	defer releaseHostAliasesCheck()
	if _, err := ensureHostAliasesSelfFn(logger, want); err != nil {
		ns, _ := os.ReadFile(k8sNamespacePath)
		logger.Warn("could not apply the cluster host aliases from the hub to this spoke's deployment; GHE calls may time out",
			"namespace", strings.TrimSpace(string(ns)), "error", err,
			"remedy", "grant get/patch on deployments/hive in this namespace (hive-self-upgrade Role/RoleBinding), or patch spec.template.spec.hostAliases by hand")
	}
}

// applyHubHostAliases is called with HeartbeatResponse.HostAliases on every
// beat. An empty list means the hive's cluster has no host_aliases and the
// Deployment is left untouched. The check runs off the heartbeat goroutine.
func applyHubHostAliases(logger *slog.Logger, want []HostAlias) {
	if len(want) == 0 {
		return
	}
	if !claimHostAliasesCheck(hostAliasesKey(want)) {
		return
	}
	go runHostAliasesCheck(logger, want)
}
