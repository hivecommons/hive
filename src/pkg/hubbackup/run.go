package hubbackup

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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

// registryDoc models the parts of hub-registry.json the backup needs.
type registryDoc struct {
	Hives []struct {
		ID        string `json:"id"`
		ClusterID string `json:"clusterId"`
	} `json:"hives"`
}

// clusterDoc models the parts of clusters.json the backup needs.
type clusterDoc struct {
	ID             string `json:"id"`
	InCluster      bool   `json:"in_cluster"`
	KubeconfigPath string `json:"kubeconfig_path"`
	Context        string `json:"context"`
}

// LoadTargets reads clusters.json and hub-registry.json to determine which
// spokes exist and how to reach each hosting cluster.
func LoadTargets(dataDir string) (map[string]ClusterTarget, map[string]string, error) {
	clustersRaw, err := os.ReadFile(filepath.Join(dataDir, saasSubdir, "clusters.json"))
	if err != nil {
		return nil, nil, fmt.Errorf("read clusters.json: %w", err)
	}
	var cds []clusterDoc
	if err := json.Unmarshal(clustersRaw, &cds); err != nil {
		return nil, nil, fmt.Errorf("parse clusters.json: %w", err)
	}
	clusters := map[string]ClusterTarget{}
	for _, c := range cds {
		clusters[c.ID] = ClusterTarget(c)
	}

	regRaw, err := os.ReadFile(filepath.Join(dataDir, registryFile))
	if err != nil {
		return nil, nil, fmt.Errorf("read hub-registry.json: %w", err)
	}
	var rd registryDoc
	if err := json.Unmarshal(regRaw, &rd); err != nil {
		return nil, nil, fmt.Errorf("parse hub-registry.json: %w", err)
	}
	hives := map[string]string{}
	for _, h := range rd.Hives {
		if h.ID != "" && h.ClusterID != "" {
			hives[h.ID] = h.ClusterID
		}
	}
	return clusters, hives, nil
}

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
}

// Run performs a full backup: build, encrypt, upload, verify, prune.
func Run(opts Options, logger *slog.Logger) (*Manifest, string, error) {
	key, err := LoadKey()
	if err != nil {
		return nil, "", err
	}
	dataDir := DataDir()

	hubNS := os.Getenv(EnvHubNamespace)
	if hubNS == "" {
		hubNS = DefaultHubNamespace
	}

	var spokeCollector SpokeCollector
	var secretCollector SecretCollector

	// The hub runs in-cluster, so its own kubectl needs no flags.
	hubCluster := ClusterTarget{ID: "hub", InCluster: true}

	if !opts.SkipSecrets {
		secretCollector = KubectlSecretCollector{Cluster: hubCluster, Namespace: hubNS}
	}
	if !opts.SkipSpokes {
		clusters, hives, err := LoadTargets(dataDir)
		if err != nil {
			logger.Error("backup: cannot enumerate spokes", "err", err)
			return nil, "", fmt.Errorf("enumerate spokes: %w", err)
		}
		logger.Info("backup: enumerated fleet", "clusters", len(clusters), "hives", len(hives))
		spokeCollector = KubectlSpokeCollector{Clusters: clusters, Hives: hives}
	}

	sealed, man, err := Build(key, spokeCollector, secretCollector, logger)
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

	store, err := NewObjectStore(os.Getenv(EnvBucket))
	if err != nil {
		return nil, "", err
	}
	if err := store.Put(name, sealed); err != nil {
		return nil, "", fmt.Errorf("upload archive: %w", err)
	}
	logger.Info("backup: uploaded", "object", name, "bytes", len(sealed),
		"spokes", len(man.SpokeIDs), "spoke_errors", len(man.SpokeErrors))

	// Read back and verify what actually landed in object storage.
	fetched, err := store.Get(name)
	if err != nil {
		return nil, "", fmt.Errorf("read-back failed: %w", err)
	}
	if _, err := Verify(key, fetched); err != nil {
		return nil, "", fmt.Errorf("stored archive failed verification: %w", err)
	}
	logger.Info("backup: verified stored archive", "object", name)

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
