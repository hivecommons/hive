package hubbackup

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/hivecommons/hive/pkg/hub/clusterconfig"
)

// Hub namespace defaults.
const (
	// DefaultHubNamespace is the namespace hosting the hub deployment.
	DefaultHubNamespace = "hive-hub"

	// EnvHubNamespace overrides the hub namespace.
	EnvHubNamespace = "HIVE_HUB_NAMESPACE"

	// EnvKubeconfigDir is where per-cluster kubeconfigs are mounted.
	EnvKubeconfigDir = "HIVE_KUBECONFIG_DIR"

	// DefaultKubeconfigDir matches the hub deployment's mount path.
	DefaultKubeconfigDir = "/etc/hive/kubeconfigs"
)

// LoadTargets reads the captured topology and returns the spoke collection plan.
func LoadTargets(dataDir string) (map[string]ClusterTarget, map[string]string, error) {
	clustersRaw, err := os.ReadFile(filepath.Join(dataDir, saasSubdir, clustersFile))
	if err != nil {
		return nil, nil, fmt.Errorf("read clusters.json: %w", err)
	}
	regRaw, err := os.ReadFile(filepath.Join(dataDir, registryFile))
	if err != nil {
		return nil, nil, fmt.Errorf("read hub-registry.json: %w", err)
	}
	gaps := make(map[string]string)
	clusters, hives := backupTargets(archivedSaaS{clusters: clustersRaw, gaps: gaps}, regRaw, slog.Default())
	if len(gaps) > 0 {
		return nil, nil, fmt.Errorf("enumerate spokes: %v", gaps)
	}
	return clusters, hives, nil
}

// backupTargets validates both registries even for a hub-only archive.
func backupTargets(saas archivedSaaS, regRaw []byte, logger *slog.Logger) (map[string]ClusterTarget, map[string]string) {
	gaps := saas.gaps
	clustersPath := filepath.Join(hubPrefix, saasSubdir, clustersFile)
	registryPath := filepath.Join(hubPrefix, registryFile)

	clusters := make(map[string]ClusterTarget)
	if saas.clusters == nil {
		clusters[clusterconfig.DefaultID] = ClusterTarget{ID: clusterconfig.DefaultID, InCluster: true}
	} else {
		configs, err := clusterconfig.Decode(saas.clusters, logger)
		if err != nil {
			gaps[clustersPath] = err.Error()
		}
		for _, cluster := range configs {
			clusters[cluster.ID] = ClusterTarget{
				ID:             cluster.ID,
				InCluster:      cluster.InCluster,
				KubeconfigPath: cluster.KubeconfigPath,
				Context:        cluster.Context,
				PullOnly:       cluster.PullOnly,
			}
		}
	}

	var registry struct {
		Hives json.RawMessage `json:"hives"`
	}
	var entries []struct {
		ID        string `json:"id"`
		ClusterID string `json:"clusterId"`
		Namespace string `json:"namespace"`
	}
	if gaps[registryPath] == "" {
		if err := json.Unmarshal(regRaw, &registry); err != nil {
			gaps[registryPath] = err.Error()
		} else if err := json.Unmarshal(registry.Hives, &entries); err != nil {
			gaps[registryPath] = "invalid fleet registry hives: " + err.Error()
		}
	}

	hives := make(map[string]string, len(entries))
	for _, hive := range entries {
		if hive.ID == "" {
			continue
		}
		metaCluster, hasHostedMeta := saas.hostedClusters[hive.ID]
		hosted := hasHostedMeta || strings.HasPrefix(hive.Namespace, spokeNamespacePrefix)
		clusterID := hive.ClusterID
		if clusterID == "" && hosted {
			clusterID = metaCluster
			if clusterID == "" {
				clusterID = clusterconfig.DefaultID
			}
		}
		if clusterID == "" {
			continue
		}
		hives[hive.ID] = clusterID
		if _, ok := clusters[clusterID]; !ok && gaps[clustersPath] == "" {
			switch {
			case saas.clusters == nil:
				gaps[clustersPath] = "cluster registry is missing from the archive"
			case hosted:
				gaps[clustersPath] = fmt.Sprintf("cluster %q for hosted hive %q is missing", clusterID, hive.ID)
			}
		}
	}
	return clusters, hives
}

type spokeCapture int

const (
	useSpokeCollector spokeCapture = iota
	collectArchivedSpokes
	skipSpokeCapture
)

// Options controls one backup run.
type Options struct {
	// LocalOnly writes the archive to LocalPath instead of object storage.
	LocalOnly bool
	LocalPath string

	// SkipSpokes disables per-spoke collection (hub-only backup).
	SkipSpokes bool

	// SkipSecrets disables Kubernetes Secret collection. Only safe in tests —
	// a real backup without Secrets is not restorable.
	SkipSecrets bool

	// ValidateFleetRegistry applies the restore schema to the captured registry.
	ValidateFleetRegistry func([]byte) error
}

// Run performs a full backup: build, encrypt, upload, verify, prune.
func Run(opts Options, logger *slog.Logger) (*Manifest, string, error) {
	key, err := LoadKey()
	if err != nil {
		return nil, "", err
	}
	hubNS := os.Getenv(EnvHubNamespace)
	if hubNS == "" {
		hubNS = DefaultHubNamespace
	}

	var secretCollector SecretCollector

	// The hub runs in-cluster, so its own kubectl needs no flags.
	hubCluster := ClusterTarget{ID: "hub", InCluster: true}

	if !opts.SkipSecrets {
		secretCollector = KubectlSecretCollector{Cluster: hubCluster, Namespace: hubNS}
	}

	capture := collectArchivedSpokes
	if opts.SkipSpokes {
		capture = skipSpokeCapture
	}
	sealed, man, err := build(key, nil, secretCollector, capture, opts.ValidateFleetRegistry, logger)
	if err != nil {
		return nil, "", err
	}

	// Verify before storing: an archive that cannot be verified must never be
	// counted as a good backup.
	if _, err := Verify(key, sealed); err != nil {
		return nil, "", fmt.Errorf("self-verify failed, refusing to store: %w", err)
	}

	name := ArchiveName(man.CreatedAt)

	if opts.LocalOnly {
		path := opts.LocalPath
		if path == "" {
			path = name
		}
		if err := os.WriteFile(path, sealed, 0o600); err != nil {
			return nil, "", fmt.Errorf("write local archive: %w", err)
		}
		logger.Info("backup: wrote local archive", "path", path,
			"bytes", len(sealed), "spokes", len(man.SpokeIDs))
		return man, path, nil
	}

	// Prune counts stored names, so a partial hub archive must not enter the
	// bucket where it could displace a usable backup on a later run.
	if len(man.HubErrors) > 0 {
		return nil, "", fmt.Errorf("refusing to upload incomplete hub backup: %v", man.HubErrors)
	}
	if opts.SkipSecrets {
		return nil, "", fmt.Errorf("refusing to upload a backup without Kubernetes Secrets")
	}
	prefix := fleetArchivePrefix
	switch {
	case man.SpokesSkipped:
		prefix = hubOnlyArchivePrefix
	case len(man.SpokeErrors) > 0:
		prefix = partialArchivePrefix
	}
	name = retainedArchiveName(prefix, man.CreatedAt, sealed)

	store, err := NewObjectStore(os.Getenv(EnvBucket))
	if err != nil {
		return nil, "", err
	}
	if err := store.publishArchive(name, sealed, logger); err != nil {
		return nil, "", err
	}
	logger.Info("backup: published verified archive", "object", name, "bytes", len(sealed),
		"spokes", len(man.SpokeIDs), "spoke_errors", len(man.SpokeErrors), "spokes_skipped", man.SpokesSkipped)

	if err := store.Prune(Retention(), logger); err != nil {
		logger.Warn("backup: prune failed", "err", err)
	}
	return man, name, nil
}

// VerifyLatest downloads the newest archive and verifies it end to end.
func VerifyLatest(logger *slog.Logger) (*Manifest, error) {
	key, err := LoadKey()
	if err != nil {
		return nil, err
	}
	store, err := NewObjectStore(os.Getenv(EnvBucket))
	if err != nil {
		return nil, err
	}
	names, err := store.List()
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("no archives found in bucket")
	}
	data, err := store.Get(names[0])
	if err != nil {
		return nil, err
	}
	man, err := Verify(key, data)
	if err != nil {
		return nil, err
	}
	logger.Info("backup: latest archive verified", "object", names[0],
		"files", len(man.Files), "spokes", len(man.SpokeIDs))
	return man, nil
}
