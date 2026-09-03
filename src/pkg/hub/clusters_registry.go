package hub

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"
)

// Validate-on-load for the hub's cluster registry, /data/saas/clusters.json
// (audit 8, §6 item 11).
//
// WHAT THE AUDIT FOUND. The live file is owned 501:wheel — a macOS uid, not the
// container's — with a clusters.json.bak-akswec2 sibling beside it: the
// signature of an out-of-band `kubectl cp` from a workstation. Nothing
// in-cluster re-asserts the file, so the hub's model of its own fleet topology
// is drift-prone with nothing to correct it.
//
// WHY THIS FIX IS A LOADER AND NOT A RECONCILER. The registry is not derivable
// from the fleet: two of its three clusters (the heartbeat-only cluster, a spoke cluster) are pull_only
// BY DESIGN and the hub cannot reach them to enumerate anything. There is no
// in-cluster source that could re-generate this file, so a reconciler would
// have to invent one — and a new source of truth for the file that gates all
// fleet routing is precisely the change most likely to cause the outage it is
// meant to prevent. What IS available is the ability to refuse to run on bytes
// the hub cannot vouch for, which is the same answer audit F20 reached for
// hub-generations.json.
//
// THE FAILURE MODE THIS CLOSES. loadClusters previously returned an EMPTY map
// on a JSON parse error. An empty registry is not a degraded registry, it is an
// inverted one: clusterForHive falls through both its lookups and returns nil
// for EVERY hive, so a truncated write silently disables the hub's writes to
// the hub-reachable cluster — the one cluster it can reach — while logging a single line and
// coming up otherwise healthy. A half-written file produced by an interrupted
// `kubectl cp` (exactly the mechanism that left the .bak sibling) lands
// squarely in this case.
//
// PULL_ONLY IS PRESERVED EXACTLY. Nothing here reinterprets, defaults, or
// repairs PullOnly. The flag is what gates whether the hub attempts to write to
// a cluster (KubectlReachable tests it first, unconditionally), so this code
// round-trips it verbatim and the tests assert routing decisions are identical
// before and after.

// clustersQuarantineSuffix preserves an unparseable registry for inspection
// instead of overwriting or discarding it.
//
// The recovery differs from the alert-acks precedent deliberately, for the same
// reason hub-generations.json differs: a corrupt acks file can be discarded
// because losing an ack merely un-silences an alert — the safe direction. A
// corrupt REGISTRY must not be discarded and replaced with a synthesized
// default, because the default names only the hub-reachable cluster and would silently strand
// every hive on the two pull-only clusters, re-routing their App and host
// resolution to the wrong cluster. The bad bytes are left on disk under this
// suffix so an operator can see what arrived.
const clustersQuarantineSuffix = ".corrupt"

// clustersBackupPrefix is the sibling-file pattern an out-of-band edit leaves
// behind (clusters.json.bak-akswec2 on the live hub).
//
// It exists as a named constant so the "a sibling is not the live file"
// property is asserted rather than assumed. The loader reads exactly
// clustersConfigPath and never globs, so a .bak-* file can never be promoted
// into the live registry by this code — but that is a property worth pinning,
// because the failure it would cause (routing from a stale hand-edited
// snapshot) is silent.
const clustersBackupPrefix = ".bak-"

// isClustersSidecarPath reports whether a path is one of the registry's
// non-authoritative siblings — a hand-edit backup (clusters.json.bak-akswec2),
// a quarantined corrupt file (.corrupt), or a partial write (.tmp).
//
// The loader reads exactly clustersConfigPath and never globs a directory, so
// no sibling can be promoted into the live registry today. This predicate
// exists so that property is ASSERTED rather than assumed: the failure it
// guards against — routing the fleet from a stale hand-edited snapshot — is
// completely silent, and a future convenience like "load the newest
// clusters.json*" would introduce it without looking wrong.
func isClustersSidecarPath(path string) bool {
	base := clustersConfigPath
	if path == base {
		return false
	}
	if !strings.HasPrefix(path, base) {
		return false
	}
	suffix := strings.TrimPrefix(path, base)
	return strings.HasPrefix(suffix, clustersBackupPrefix) ||
		suffix == clustersQuarantineSuffix ||
		suffix == ".tmp"
}

// clustersReadAttempts is how many times a non-ENOENT read is retried before
// the loader gives up and fails closed.
//
// The hazard is a TRANSIENT fault — a PVC remount, an NFS blip, an EIO — so the
// first thing to do about it is try again. Failing closed is the correct end
// state but not a cheap one, so it is reached only after the transient
// explanation is ruled out. Mirrors generationsReadAttempts.
const clustersReadAttempts = 3

// clustersReadRetryDelay is the pause between read attempts. Deliberately
// short: this runs on the hub's startup path.
const clustersReadRetryDelay = 100 * time.Millisecond

// clustersLoadOutcome names what the loader established about the registry. It
// exists because the two degraded cases the old code collapsed into "empty map"
// are opposites.
type clustersLoadOutcome int

const (
	// clustersLoaded: the file was read, parsed, and yielded a usable registry.
	clustersLoaded clustersLoadOutcome = iota
	// clustersAbsent: the file is ENOENT. A POSITIVE fact, not an absence of
	// information — a hub with no registry has never been given one, so the
	// synthesized hub-reachable-cluster default is exactly right. This is the documented
	// backward-compatibility path for a fresh or Compose deployment and it is
	// preserved verbatim.
	clustersAbsent
	// clustersUntrusted: the file EXISTS but its contents could not be
	// established — an unretryable read error, bytes that do not parse, or a
	// parsed array that yielded no usable cluster at all.
	//
	// THIS IS THE CASE THAT MUST NOT YIELD AN EMPTY MAP, and must not fall back
	// to the default either. The hub knows a registry was placed here and
	// cannot tell what it says. Returning empty disables writes to the hub-reachable cluster;
	// returning the default silently re-routes every pull-only hive. Both are
	// widenings of the blast radius of a corrupt file, so the hub refuses.
	clustersUntrusted
)

// clustersFatal aborts the process when the registry cannot be established.
//
// A var so tests can substitute a recorder: the behaviour under test is "the
// hub refuses", and a test that genuinely called os.Exit would take the test
// binary down with it. Production never reassigns it.
//
// os.Exit(1) rather than panic: this runs inside NewHubServer on the startup
// path, and a panic could be recovered by a caller and turned back into the
// silent degraded start this fix removes. Exiting is not recoverable, which is
// the point — Kubernetes restarts the pod, the readiness gate stays closed, and
// the failure is visible as a CrashLoopBackOff rather than as mis-routed
// provisioning on a hub that looks healthy.
var clustersFatal = func(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}

// readClustersFile reads the registry, retrying transient faults.
//
// Returns (data, outcome). ENOENT is reported as clustersAbsent on the FIRST
// attempt without retrying: a missing file is a stable answer, and retrying it
// would add 300ms to every fresh hub's startup for no information.
func readClustersFile(path string, logger *slog.Logger) ([]byte, clustersLoadOutcome) {
	var lastErr error
	for attempt := 1; attempt <= clustersReadAttempts; attempt++ {
		data, err := os.ReadFile(path)
		if err == nil {
			return data, clustersLoaded
		}
		if os.IsNotExist(err) {
			return nil, clustersAbsent
		}
		lastErr = err
		if attempt < clustersReadAttempts {
			logger.Warn("cluster registry read failed, retrying",
				"path", path, "attempt", attempt, "error", err)
			time.Sleep(clustersReadRetryDelay)
		}
	}
	logger.Error("cluster registry is unreadable after retries — refusing to run on an unknown fleet topology",
		"path", path,
		"attempts", clustersReadAttempts,
		"error", lastErr,
		"remedy", "restore /data/saas/clusters.json on the hub PVC and restart the hub")
	return nil, clustersUntrusted
}

// quarantineClustersFile renames unparseable bytes aside so an operator can
// inspect what arrived. Best-effort: a failure to rename is logged and does not
// change the load outcome, because the outcome is already "refuse".
func quarantineClustersFile(path string, logger *slog.Logger) {
	dst := path + clustersQuarantineSuffix
	if err := os.Rename(path, dst); err != nil {
		logger.Error("could not quarantine unparseable cluster registry",
			"path", path, "quarantine", dst, "error", err)
		return
	}
	logger.Error("quarantined unparseable cluster registry for inspection",
		"path", path, "quarantine", dst)
}

// defaultClusterRegistry is the single-entry registry synthesized when no
// registry file exists.
//
// Unchanged from the original loadClusters ENOENT branch, kept in one place so
// the "absent" path is provably identical to what shipped before this fix.
func defaultClusterRegistry() map[string]ClusterConfig {
	return map[string]ClusterConfig{
		defaultClusterID: {
			ID:           defaultClusterID,
			Name:         "OKE (default)",
			InCluster:    true,
			StorageType:  "nfs",
			IngressType:  "nginx",
			IngressClass: "nginx",
			CertIssuer:   "letsencrypt-prod",
			Domain:       hubSpokeDomain(),
			Arch:         "arm64",
			// v4 is the ONLY image a hosted spoke can run: audit F2 deleted the
			// fleet-wide heartbeat lane, and a v2 spoke has no SpokeHeartbeatKey
			// self-derive path, so it cannot authenticate to this hub at all.
			ImageTag: "v4-latest",
		},
	}
}

// validateClusterEntries filters a parsed array into the registry map, applying
// exactly the same three admission rules the original loader applied, in the
// same order, with the same log lines.
//
// The rules are UNCHANGED on purpose. This fix is about what happens when the
// file cannot be parsed at all, not about which well-formed entries are
// admitted — tightening admission here would change fleet routing, which is the
// outcome the brief rules out.
func validateClusterEntries(configs []ClusterConfig, logger *slog.Logger) map[string]ClusterConfig {
	clusters := make(map[string]ClusterConfig, len(configs))
	for _, c := range configs {
		if c.ID == "" {
			logger.Warn("skipping cluster config with empty ID")
			continue
		}
		if !c.InCluster && c.KubeconfigPath == "" && !c.PullOnly {
			logger.Warn("skipping remote cluster with no kubeconfig_path",
				"cluster", c.ID,
				"remedy", "set kubeconfig_path, or pull_only: true if the hub cannot reach this cluster and its spokes connect outbound over the heartbeat")
			continue
		}
		if c.Domain == "" {
			logger.Warn("skipping cluster with no domain", "cluster", c.ID)
			continue
		}
		clusters[c.ID] = c
	}
	return clusters
}

// loadClustersChecked reads and validates the registry.
//
// Returns the registry and an outcome. The three outcomes are the whole point
// of the function; see clustersLoadOutcome.
func loadClustersChecked(logger *slog.Logger) (map[string]ClusterConfig, clustersLoadOutcome) {
	data, outcome := readClustersFile(clustersConfigPath, logger)
	switch outcome {
	case clustersAbsent:
		logger.Info("no clusters config found, using default hive-oke cluster", "path", clustersConfigPath)
		return defaultClusterRegistry(), clustersAbsent
	case clustersUntrusted:
		return nil, clustersUntrusted
	}

	var configs []ClusterConfig
	if err := json.Unmarshal(data, &configs); err != nil {
		// A truncated or hand-mangled file lands here. Do NOT return an empty
		// map: see clustersUntrusted.
		logger.Error("cluster registry does not parse — refusing to run on an unknown fleet topology",
			"path", clustersConfigPath,
			"error", err,
			"bytes", len(data),
			"remedy", "restore a valid /data/saas/clusters.json on the hub PVC and restart the hub")
		quarantineClustersFile(clustersConfigPath, logger)
		return nil, clustersUntrusted
	}

	clusters := validateClusterEntries(configs, logger)
	if len(clusters) == 0 {
		// The file exists and parsed, but nothing in it is usable. Same
		// reasoning as a parse failure: the operator placed a registry here and
		// the hub cannot act on it. An empty registry would nil out
		// clusterForHive for every hive.
		logger.Error("cluster registry parsed but yielded no usable cluster — refusing to run on an unknown fleet topology",
			"path", clustersConfigPath,
			"entries_in_file", len(configs),
			"remedy", "every entry needs a non-empty id, a domain, and either in_cluster, kubeconfig_path, or pull_only")
		return nil, clustersUntrusted
	}

	logger.Info("loaded cluster configs", "count", len(clusters))
	return clusters, clustersLoaded
}
