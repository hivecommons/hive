package hub

import (
	"context"
	cryptoRand "crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
	"time"
)

var saasHivesDir = "/data/saas/hives"

const (
	maxHivesPerUser       = 3
	maxSaaSHivesTotal     = 0 // 0 = unlimited
	provisionTimeout      = 5 * time.Minute
	cpuRequest            = "500m"
	cpuLimit              = "2000m"
	memRequest            = "2Gi"
	memLimit              = "8Gi"
	nfsStorageCapacity    = "50Gi"
	nfsMountTargetIP      = "10.0.10.30"
	nfsExportPathPrefix   = "/hive-"
	rolloutMaxSurge       = 1
	rolloutMaxUnavailable = 0

	// dynamicPVCStorage is the storage request for dynamically-provisioned PVCs (e.g. CephFS).
	dynamicPVCStorage = "50Gi"

	// publicGitHubOAuthClientID is the Hive GitHub App client ID for Device Flow login on public GitHub.
	publicGitHubOAuthClientID = "Ov23ligE2p0gjXg6xAUf"

	// dashboardPort is the port the hive dashboard listens on.
	dashboardPort = 3002

	// terminalPort is the port the hive terminal/API listens on.
	terminalPort = 3001

	// dashboardTokenBytes is the number of random bytes for dashboard auth tokens.
	dashboardTokenBytes = 32

	// saasRoleOwner / saasRoleRead are the role strings stored in each
	// SaaSUser.Hives map and injected into the spoke's authorized-users list.
	saasRoleOwner = "owner"
	saasRoleRead  = "read"

	// ingressTypeOpenShiftRoute selects OpenShift Route generation.
	ingressTypeOpenShiftRoute = "openshift-route"

	// storageTypeDynamic selects dynamic PVC provisioning (no NFS PV).
	storageTypeDynamic = "dynamic"

	// storageTypeNFS selects NFS-backed PV + PVC provisioning.
	storageTypeNFS = "nfs"

	// initContainerUID is the UID used by the init container for chown.
	initContainerUID = 1001

	// initContainerGID is the GID used by the init container for chown.
	initContainerGID = 1000

	// sccServiceAccountName is the ServiceAccount name created for SCC-requiring clusters.
	sccServiceAccountName = "hive-sa"
)

// provisionPollInterval is how often startProvisionWatcher polls provisioning
// hives for readiness. A var (not a const) so tests can drive one loop
// iteration quickly; production keeps the default.
var provisionPollInterval = 30 * time.Second

// defaultClusterID is the cluster ID assigned to hives that predate
// multi-cluster support.
const defaultClusterID = "hive-oke"

// gpuClusterID is the cluster ID of the GPU pool. It is heartbeat-only: the
// hub cannot reach it over kubectl, so all config for its hives (access lists,
// GitHub App creds, claimed project config) is delivered via the heartbeat
// response rather than pushed.
const gpuClusterID = "vllm-d"

// Placeholder-hive lifecycle status values. A pre-provisioned placeholder sits
// at statusAvailable until an admin assigns it to a requesting user, at which
// point its status is cleared (empty) and it behaves like any owned hive.
const (
	// statusAvailable marks a pre-provisioned placeholder hive that is idle and
	// waiting to be claimed. Only such a hive may be assigned.
	statusAvailable = "available"
)

// ACMM level bounds for a claimed/assigned hive. The maturity model spans
// levels 0-6; a placeholder assignment defaults to level 2 when unspecified.
const (
	minAssignACMMLevel     = 0
	maxAssignACMMLevel     = 6
	defaultAssignACMMLevel = 2
)

// clustersConfigPath is the on-disk location where the hub reads cluster
// definitions. It is a JSON array of ClusterConfig objects. A var (not a const)
// so tests can point it at a temp file; production never reassigns it.
var clustersConfigPath = "/data/saas/clusters.json"

// ClusterConfig describes a Kubernetes cluster that the hub can provision
// hive spokes onto. Each cluster has its own kubeconfig, storage backend,
// ingress style, and optional platform-specific settings (OCI, OpenShift).
type ClusterConfig struct {
	ID                          string `json:"id" yaml:"id"`
	Name                        string `json:"name" yaml:"name"`
	KubeconfigPath              string `json:"kubeconfig_path,omitempty" yaml:"kubeconfig_path,omitempty"`
	Context                     string `json:"context" yaml:"context"`
	InCluster                   bool   `json:"in_cluster" yaml:"in_cluster"`
	StorageClass                string `json:"storage_class" yaml:"storage_class"`
	StorageType                 string `json:"storage_type" yaml:"storage_type"`
	NFSMountIP                  string `json:"nfs_mount_ip,omitempty" yaml:"nfs_mount_ip,omitempty"`
	IngressType                 string `json:"ingress_type" yaml:"ingress_type"`
	IngressClass                string `json:"ingress_class,omitempty" yaml:"ingress_class,omitempty"`
	CertIssuer                  string `json:"cert_issuer,omitempty" yaml:"cert_issuer,omitempty"`
	Domain                      string `json:"domain" yaml:"domain"`
	DomainPrefix                string `json:"domain_prefix,omitempty" yaml:"domain_prefix,omitempty"`
	OCICompartment              string `json:"oci_compartment,omitempty" yaml:"oci_compartment,omitempty"`
	OCIAvailDomain              string `json:"oci_avail_domain,omitempty" yaml:"oci_avail_domain,omitempty"`
	OCIMountTarget              string `json:"oci_mount_target,omitempty" yaml:"oci_mount_target,omitempty"`
	OCIExportSet                string `json:"oci_export_set,omitempty" yaml:"oci_export_set,omitempty"`
	RequiresSCC                 bool   `json:"requires_scc" yaml:"requires_scc"`
	SCCName                     string `json:"scc_name,omitempty" yaml:"scc_name,omitempty"`
	HasGPU                      bool   `json:"has_gpu" yaml:"has_gpu"`
	Arch                        string `json:"arch" yaml:"arch"`
	ImageTag                    string `json:"image_tag" yaml:"image_tag"`
	ImagePullPolicy             string `json:"image_pull_policy,omitempty" yaml:"image_pull_policy,omitempty"`
	InferenceEndpoint           string `json:"inference_endpoint,omitempty" yaml:"inference_endpoint,omitempty"`
	GitHubBaseURL               string `json:"github_base_url,omitempty" yaml:"github_base_url,omitempty"`
	GitHubAPIURL                string `json:"github_api_url,omitempty" yaml:"github_api_url,omitempty"`
	OAuthClientID               string `json:"oauth_client_id,omitempty" yaml:"oauth_client_id,omitempty"`
	ClusterHealthTimeoutSeconds int    `json:"cluster_health_timeout_seconds,omitempty" yaml:"cluster_health_timeout_seconds,omitempty"`
}

// kubectlForCluster builds an exec.Cmd that targets a specific cluster.
// For in-cluster configs (InCluster == true) it runs plain kubectl;
// for remote clusters it injects --kubeconfig and --context flags.
func kubectlForCluster(cluster *ClusterConfig, args ...string) *exec.Cmd {
	return exec.Command("kubectl", kubectlArgsForCluster(cluster, args...)...)
}

// kubectlForClusterContext is like kubectlForCluster but lets callers bound the
// command lifetime with a context.
func kubectlForClusterContext(ctx context.Context, cluster *ClusterConfig, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "kubectl", kubectlArgsForCluster(cluster, args...)...)
}

func kubectlArgsForCluster(cluster *ClusterConfig, args ...string) []string {
	fullArgs := []string{}
	if !cluster.InCluster {
		fullArgs = append(fullArgs, "--kubeconfig", cluster.KubeconfigPath, "--context", cluster.Context)
	} else {
		// Explicitly pass in-cluster credentials so kubectl does not fall back
		// to localhost:8080 when exec.Command inherits an empty KUBECONFIG path.
		host := os.Getenv("KUBERNETES_SERVICE_HOST")
		port := os.Getenv("KUBERNETES_SERVICE_PORT")
		if host != "" && port != "" {
			fullArgs = append(fullArgs,
				"--server", fmt.Sprintf("https://%s:%s", host, port),
				"--certificate-authority", "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt",
				"--token", readSAToken(),
			)
		}
	}
	fullArgs = append(fullArgs, args...)
	return fullArgs
}

// readSAToken reads the current service account token from the projected volume.
func readSAToken() string {
	data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
	if err != nil {
		return ""
	}
	return string(data)
}

// loadClusters reads the clusters config file and returns a validated
// map of cluster ID → ClusterConfig. If the file does not exist, it
// returns a single default entry for hive-oke (backward compatibility).
func loadClusters(logger *slog.Logger) map[string]ClusterConfig {
	clusters := make(map[string]ClusterConfig)

	data, err := os.ReadFile(clustersConfigPath)
	if err != nil {
		logger.Info("no clusters config found, using default hive-oke cluster", "path", clustersConfigPath)
		clusters[defaultClusterID] = ClusterConfig{
			ID:           defaultClusterID,
			Name:         "OKE (default)",
			InCluster:    true,
			StorageType:  "nfs",
			IngressType:  "nginx",
			IngressClass: "nginx",
			CertIssuer:   "letsencrypt-prod",
			Domain:       "hive.kubestellar.io",
			Arch:         "arm64",
			ImageTag:     "v2-latest",
		}
		return clusters
	}

	var configs []ClusterConfig
	if err := json.Unmarshal(data, &configs); err != nil {
		logger.Error("failed to parse clusters config", "path", clustersConfigPath, "error", err)
		return clusters
	}

	for _, c := range configs {
		if c.ID == "" {
			logger.Warn("skipping cluster config with empty ID")
			continue
		}
		if !c.InCluster && c.KubeconfigPath == "" {
			logger.Warn("skipping remote cluster with no kubeconfig_path", "cluster", c.ID)
			continue
		}
		if c.Domain == "" {
			logger.Warn("skipping cluster with no domain", "cluster", c.ID)
			continue
		}
		clusters[c.ID] = c
	}

	logger.Info("loaded cluster configs", "count", len(clusters))
	return clusters
}

type PendingAccessRequest struct {
	Username    string `json:"username"`
	RequestedAt string `json:"requested_at"`
	// Note is the requester's justification for wanting access,
	// surfaced to owners/approvers. May be empty for legacy records.
	Note string `json:"note,omitempty"`
}

type SaaSHive struct {
	ID          string   `json:"id"`
	Owner       string   `json:"owner"`
	ProjectName string   `json:"project_name"`
	Org         string   `json:"org"`
	// GitHubHost is the GitHub instance this project lives on — empty means
	// public github.com, otherwise a GitHub Enterprise host (github.ibm.com,
	// github.cisco.com, …). Recorded at assign time from the requested org so
	// the spoke can be pointed at the right API over the heartbeat.
	GitHubHost  string   `json:"github_host,omitempty"`
	Repos       []string `json:"repos"`
	PrimaryRepo string   `json:"primary_repo"`
	ACMMLevel   int      `json:"acmm_level"`
	Status      string   `json:"status"`
	CreatedAt   string   `json:"created_at"`
	Subdomain   string   `json:"subdomain"`
	// VanityURL is the friendly dashboard URL derived from the claimed project
	// (e.g. hosted-<org>-<repo>-*.hive.kubestellar.io), set at ASSIGN time. Its
	// presence is the explicit marker that a placeholder has been claimed —
	// callers prefer it over the raw placeholder Subdomain and must NOT infer
	// "claimed" by pattern-matching the placeholder name. Empty = unclaimed
	// placeholder (or a pre-vanity hive).
	VanityURL string `json:"vanity_url,omitempty"`
	// ClaimDelivered flips true once a claimed spoke first reports back the
	// assigned org/repos, i.e. the claim payload reached it. Before that the hub
	// PUSHES org/repos to the spoke (it may still report its old placeholder
	// project); after that the spoke's dashboard becomes the source of truth and
	// the hub ADOPTS operator edits instead of pushing. (ACMM is operator-owned
	// from the start and always adopted, independent of this flag.)
	ClaimDelivered  bool                   `json:"claim_delivered,omitempty"`
	Error           string                 `json:"error,omitempty"`
	AutoUpgrade     bool                   `json:"auto_upgrade"`
	IsPublic        bool                   `json:"is_public"`
	PendingRequests []PendingAccessRequest `json:"pending_requests,omitempty"`
	OCIFileSystemID string                 `json:"oci_file_system_id,omitempty"`
	OCIExportID     string                 `json:"oci_export_id,omitempty"`
	ClusterID       string                 `json:"cluster_id,omitempty"`

	// AutoUpgradeMode gates WHEN an enabled auto-upgrade may fire:
	// AutoUpgradeModeInstant (or empty) upgrades as soon as the hive is seen
	// behind latest; AutoUpgradeModeDaily upgrades at most once per ET calendar
	// day, at or after autoUpgradeDailyHour. omitempty keeps existing meta.json
	// records byte-identical until the operator actually picks a mode, and an
	// absent field reads as instant — so the ~42 records already on the PVC keep
	// today's behaviour exactly.
	AutoUpgradeMode string `json:"auto_upgrade_mode,omitempty"`
	// AutoUpgradeLastFired is the ET calendar day (autoUpgradeDateFormat) on
	// which the daily schedule last triggered an upgrade for this hive. It lives
	// here, in the hive's own meta.json on the PVC, for two reasons: it is
	// per-hive state that belongs with the rest of the hive's upgrade
	// preferences, and persisting it means a hub restart at 17:03 cannot re-fire
	// an upgrade that already went out at 17:01. Empty for instant-mode hives,
	// which are never gated and never record a date.
	AutoUpgradeLastFired string `json:"auto_upgrade_last_fired,omitempty"`

	// GitHubBaseURL / GitHubAPIURL pin this hive's GitHub host. The GitHub
	// host is a property of the HIVE (where its org/repos live), not the
	// cluster: a cluster-level GHE default silently breaks hives for
	// public-GitHub orgs provisioned onto that cluster (empty
	// oauth_client_id → broken direct-route sign-in, and all API traffic
	// aimed at the wrong host). "public" (or an explicit github.com URL)
	// forces public GitHub; empty falls back to the cluster default for
	// backward compatibility.
	GitHubBaseURL string `json:"github_base_url,omitempty"`
	GitHubAPIURL  string `json:"github_api_url,omitempty"`

	// Migration tracking fields (Phase 7).
	MigrationStatus    string `json:"migration_status,omitempty"`     // "migrating", "completed", "failed"
	MigrationFrom      string `json:"migration_from,omitempty"`       // source cluster ID
	MigrationTo        string `json:"migration_to,omitempty"`         // target cluster ID
	MigrationStartedAt string `json:"migration_started_at,omitempty"` // RFC3339 timestamp
}

type CreateHiveRequest struct {
	Org            string `json:"org"`
	Repos          string `json:"repos"`
	PrimaryRepo    string `json:"primary_repo"`
	ProjectName    string `json:"project_name"`
	ACMMLevel      int    `json:"acmm_level"`
	ClusterID      string `json:"cluster_id"`
	GitHubToken    string `json:"github_token"`
	AuthMethod     string `json:"auth_method"`
	AppID          string `json:"app_id"`
	InstallationID string `json:"installation_id"`
	AppPrivateKey  string `json:"app_private_key"`
	// IsPublic controls registry visibility. A pointer so that "field
	// absent" (the admin create modal and older API callers never sent
	// it) defaults to public — the behavior before PR #1604 hardcoded
	// is_public: true in the provisioning template. Send false explicitly
	// to provision a private hive.
	IsPublic *bool `json:"is_public"`
	// GitHubBaseURL / GitHubAPIURL: per-hive GitHub host override (see
	// SaaSHive). Use "public" to force public github.com on a cluster
	// whose defaults point at a GHE instance.
	GitHubBaseURL string `json:"github_base_url,omitempty"`
	GitHubAPIURL  string `json:"github_api_url,omitempty"`
}

// githubHostPublic is the sentinel request value that forces public
// github.com even when the target cluster's defaults point at GHE.
const githubHostPublic = "public"

// effectiveGitHubBaseURL resolves the GitHub web base URL for a hive:
// hive-level override first (with "public"/github.com meaning public →
// empty string, the template's public-GitHub representation), then the
// cluster default.
func effectiveGitHubBaseURL(h *SaaSHive, cluster *ClusterConfig) string {
	switch h.GitHubBaseURL {
	case "":
		return cluster.GitHubBaseURL
	case githubHostPublic, "https://github.com":
		return ""
	default:
		return h.GitHubBaseURL
	}
}

// effectiveGitHubAPIURL resolves the GitHub API URL with the same
// precedence as effectiveGitHubBaseURL.
func effectiveGitHubAPIURL(h *SaaSHive, cluster *ClusterConfig) string {
	switch h.GitHubAPIURL {
	case "":
		// An explicit public base URL implies the public API too — don't
		// let a cluster GHE API URL leak under a public web host.
		if h.GitHubBaseURL == githubHostPublic || h.GitHubBaseURL == "https://github.com" {
			return ""
		}
		return cluster.GitHubAPIURL
	case githubHostPublic, "https://api.github.com":
		return ""
	default:
		return h.GitHubAPIURL
	}
}

func generateHiveID(org, repo string) string {
	short := repo
	if len(short) > 12 {
		short = short[:12]
	}
	chars := "abcdefghijklmnopqrstuvwxyz0123456789"
	suffix := make([]byte, 4)
	for i := range suffix {
		suffix[i] = chars[rand.Intn(len(chars))]
	}
	return fmt.Sprintf("hosted-%s-%s-%s", sanitize(org), sanitize(short), string(suffix))
}

func sanitize(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, c := range s {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			b.WriteRune(c)
		}
	}
	return b.String()
}

// clusterForHive returns the ClusterConfig for a given hive, falling back
// to the default cluster if the hive has no cluster_id or the ID is unknown.
func (s *HubServer) clusterForHive(h *SaaSHive) *ClusterConfig {
	clusterID := h.ClusterID
	if clusterID == "" {
		clusterID = defaultClusterID
	}
	if c, ok := s.clusters[clusterID]; ok {
		return &c
	}
	// Fallback: return the default cluster if it exists.
	if c, ok := s.clusters[defaultClusterID]; ok {
		return &c
	}
	return nil
}

// hubCluster returns the cluster config for the hub itself (always the
// default in-cluster config). Hub operations like self-upgrade always
// target this cluster.
func (s *HubServer) hubCluster() *ClusterConfig {
	if c, ok := s.clusters[defaultClusterID]; ok {
		return &c
	}
	// Fallback: synthesize an in-cluster config so hub operations still work.
	return &ClusterConfig{ID: defaultClusterID, InCluster: true}
}

func loadSaaSHive(id string) *SaaSHive {
	if strings.Contains(id, "..") || strings.Contains(id, "/") || strings.Contains(id, "\\") {
		return nil
	}
	path := filepath.Join(saasHivesDir, id, "meta.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var h SaaSHive
	if json.Unmarshal(data, &h) != nil {
		return nil
	}
	return &h
}

func saveSaaSHive(h *SaaSHive) error {
	if strings.Contains(h.ID, "..") || strings.Contains(h.ID, "/") || strings.Contains(h.ID, "\\") {
		return fmt.Errorf("invalid hive ID for save: %q", h.ID)
	}
	dir := filepath.Join(saasHivesDir, h.ID)
	os.MkdirAll(dir, 0o755)
	data, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "meta.json")
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func listSaaSHives() []SaaSHive {
	entries, err := os.ReadDir(saasHivesDir)
	if err != nil {
		return nil
	}
	var hives []SaaSHive
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		h := loadSaaSHive(e.Name())
		if h != nil {
			hives = append(hives, *h)
		}
	}
	return hives
}

func countUserHives(username string) int {
	count := 0
	for _, h := range listSaaSHives() {
		if h.Owner == username {
			count++
		}
	}
	return count
}

// authorizedUsersForHive builds the comma-separated authorized-users list a
// spoke needs to enforce per-user device-flow authorization on its direct
// (non-hub-proxied) route. Format: "owner:owner,viewer1:read,viewer2:read".
//
// The owner always comes first with role "owner" (read-write). Every OTHER
// SaaS user the hub granted this hive (any of "owner"/"read-write"/"read") is
// appended as "read" — a read-only viewer on the direct route. Granting a
// non-owner write access on the direct route is deliberately deferred (only the
// single provisioned owner is write-capable there); see the PR notes for the
// multi-user-grant follow-up. This fails safe: a grant can never widen access
// beyond read on the direct route. Usernames are sanitized to guard the env
// value, and the owner is de-duplicated so they appear exactly once.
func authorizedUsersForHive(h *SaaSHive) string {
	entries := make([]string, 0, 4)
	seen := map[string]bool{}
	owner := sanitize(h.Owner)
	if owner != "" {
		entries = append(entries, owner+":"+saasRoleOwner)
		seen[strings.ToLower(owner)] = true
	}
	for _, u := range listAllSaaSUsers() {
		if _, ok := u.Hives[h.ID]; !ok {
			continue
		}
		name := sanitize(u.GitHubUsername)
		if name == "" || seen[strings.ToLower(name)] {
			continue
		}
		entries = append(entries, name+":"+saasRoleRead)
		seen[strings.ToLower(name)] = true
	}
	return strings.Join(entries, ",")
}

func provisionHive(h *SaaSHive, req *CreateHiveRequest, cluster *ClusterConfig, logger *slog.Logger) error {
	dir := filepath.Join(saasHivesDir, h.ID, "manifests")
	os.MkdirAll(dir, 0o755)

	repos := h.Repos
	reposYAML := "[]"
	if len(repos) > 0 {
		parts := make([]string, 0, len(repos))
		for _, r := range repos {
			clean := sanitize(r)
			if clean != "" {
				parts = append(parts, fmt.Sprintf("      - %s", clean))
			}
		}
		if len(parts) > 0 {
			reposYAML = "\n" + strings.Join(parts, "\n")
		}
	}

	useApp := req.AuthMethod == "app" && req.AppID != ""
	useAppFull := useApp && req.InstallationID != "" && req.AppPrivateKey != ""

	// Determine the dashboard URL based on cluster domain.
	dashboardHost := h.ID + "." + cluster.Domain
	dashboardURL := "https://" + dashboardHost

	// Determine image pull policy: explicit config wins, otherwise Always
	// (mutable tags like v2-latest require Always to pick up upgrades).
	imagePullPolicy := cluster.ImagePullPolicy
	if imagePullPolicy == "" {
		imagePullPolicy = "Always"
	}

	// Determine image tag from cluster config, falling back to v2-latest.
	imageTag := cluster.ImageTag
	if imageTag == "" {
		imageTag = "v2-latest"
	}

	data := map[string]any{
		"ID":              h.ID,
		"Namespace":       "hive-hosted-" + h.ID,
		"Org":             sanitize(h.Org),
		"Repos":           reposYAML,
		"PrimaryRepo":     sanitize(h.PrimaryRepo),
		"AuthorizedUsers": authorizedUsersForHive(h),
		"ACMMLevel":       h.ACMMLevel,
		"Token":           req.GitHubToken,
		"UseApp":          useApp,
		"UseAppFull":      useAppFull,
		"AppID":           sanitize(req.AppID),
		"InstallationID":  sanitize(req.InstallationID),
		"AppPrivateKey": func() string {
			lines := strings.Split(strings.TrimSpace(req.AppPrivateKey), "\n")
			for i := range lines {
				lines[i] = "    " + strings.TrimSpace(lines[i])
			}
			return strings.Join(lines, "\n")
		}(),
		"CPURequest":            cpuRequest,
		"CPULimit":              cpuLimit,
		"MemRequest":            memRequest,
		"MemLimit":              memLimit,
		"RolloutMaxSurge":       rolloutMaxSurge,
		"RolloutMaxUnavailable": rolloutMaxUnavailable,
		"DashboardToken": func() string {
			b := make([]byte, dashboardTokenBytes)
			if _, err := cryptoRand.Read(b); err != nil {
				logger.Error("failed to generate dashboard token", "error", err)
				return ""
			}
			return hex.EncodeToString(b)
		}(),
		"HubSecret": func() string {
			if s := os.Getenv("HIVE_HUB_SECRET"); s != "" {
				return s
			}
			if data, err := os.ReadFile("/data/saas/hub-secret.key"); err == nil {
				return strings.TrimSpace(string(data))
			}
			return ""
		}(),
		// Cluster-aware fields.
		"DashboardHost":      dashboardHost,
		"DashboardURL":       dashboardURL,
		"DashboardPort":      dashboardPort,
		"TerminalPort":       terminalPort,
		"ImagePullPolicy":    imagePullPolicy,
		"ImageTag":           imageTag,
		"IsOpenShiftRoute":   cluster.IngressType == ingressTypeOpenShiftRoute,
		"IsNginxIngress":     cluster.IngressType != ingressTypeOpenShiftRoute,
		"IsDynamicStorage":   cluster.StorageType == storageTypeDynamic,
		"IsNFSStorage":       cluster.StorageType == storageTypeNFS || cluster.StorageType == "",
		"StorageClass":       cluster.StorageClass,
		"DynamicPVCStorage":  dynamicPVCStorage,
		"NFSStorageCapacity": nfsStorageCapacity,
		"NFSMountTargetIP":   nfsMountTargetIP,
		"NFSExportPath":      nfsExportPathPrefix + h.ID,
		"PVName":             "hive-" + h.ID + "-fss-pv",
		"RequiresSCC":        cluster.RequiresSCC,
		"SCCName": func() string {
			if cluster.SCCName != "" {
				return cluster.SCCName
			}
			return "anyuid"
		}(),
		"InitContainerUID":  initContainerUID,
		"InitContainerGID":  initContainerGID,
		"InferenceEndpoint": cluster.InferenceEndpoint,
		"HasInference":      cluster.InferenceEndpoint != "",
		"IsPublic":          h.IsPublic,
		"HiveType":          "hosted",
		"HasGHE":            effectiveGitHubBaseURL(h, cluster) != "",
		"GitHubBaseURL":     effectiveGitHubBaseURL(h, cluster),
		"GitHubAPIURL":      effectiveGitHubAPIURL(h, cluster),
		"OAuthClientID": func() string {
			// Follow the hive's EFFECTIVE GitHub host, not the cluster
			// default — a public-GitHub hive on a GHE-defaulted cluster
			// must still get the public Device Flow client ID or its
			// direct-route sign-in is broken.
			if effectiveGitHubBaseURL(h, cluster) == "" {
				return publicGitHubOAuthClientID
			}
			if cluster.OAuthClientID != "" {
				return cluster.OAuthClientID
			}
			return ""
		}(),
		"CertIssuer":   cluster.CertIssuer,
		"IngressClass": cluster.IngressClass,
		"Domain":       cluster.Domain,
		"InCluster":    cluster.InCluster,
	}

	// For NFS storage: auto-create OCI File System + NFS export.
	// Failures are non-fatal — the admin can create them manually.
	if cluster.StorageType == storageTypeNFS || cluster.StorageType == "" {
		exportPath := nfsExportPathPrefix + h.ID
		fsID, err := createOCIFileSystem("hive-"+h.ID, logger)
		if err != nil {
			logger.Warn("OCI FSS creation failed — admin must create manually", "hive", h.ID, "error", err)
		} else {
			h.OCIFileSystemID = fsID
			exportID, exportErr := createOCIExport(fsID, exportPath, logger)
			if exportErr != nil {
				logger.Warn("OCI export creation failed — admin must create manually", "hive", h.ID, "error", exportErr)
			} else {
				h.OCIExportID = exportID
			}
		}
	}

	tmpl, err := template.New("manifests").Parse(k8sManifestTemplate)
	if err != nil {
		return fmt.Errorf("template parse: %w", err)
	}

	manifestPath := filepath.Join(dir, "all.yaml")
	f, err := os.Create(manifestPath)
	if err != nil {
		return fmt.Errorf("create manifest: %w", err)
	}
	if err := tmpl.Execute(f, data); err != nil {
		f.Close()
		return fmt.Errorf("template exec: %w", err)
	}
	f.Close()

	cmd := kubectlForCluster(cluster, "apply", "-f", manifestPath)
	out, err := cmd.CombinedOutput()

	// Remove manifest immediately — it contains GitHub tokens in plaintext
	if rmErr := os.Remove(manifestPath); rmErr != nil {
		logger.Error("SECURITY: failed to remove manifest with plaintext tokens", "path", manifestPath, "error", rmErr)
	}

	if err != nil {
		logger.Warn("kubectl apply failed", "hive", h.ID, "cluster", cluster.ID, "output", string(out), "error", err)
		return fmt.Errorf("provisioning failed — check hub logs for details")
	}

	logger.Info("audit: saas hive provisioned", "hive_id", h.ID, "owner", h.Owner, "org", h.Org, "cluster", cluster.ID)
	return nil
}

// deprovisionHive performs best-effort cleanup of all resources associated
// with a hosted hive: K8s namespace (cascading to deployment, service, ingress,
// configmap, secret, PVC), cluster-scoped PV, OCI NFS export, OCI file system,
// and the on-disk SaaS hive record. It also decrements the owner's quota.
// Order matters: namespace before PV, export before file system.
// Errors are logged but do not stop the remaining cleanup steps.
func deprovisionHive(h *SaaSHive, cluster *ClusterConfig, logger *slog.Logger) {
	hiveID := h.ID

	// Step 1: Delete K8s namespace. Cascading delete removes deployment, service,
	// ingress/route, configmap, secret, PVC, and (for OpenShift) routes.
	ns := "hive-hosted-" + hiveID
	logger.Info("deprovision: deleting namespace", "namespace", ns, "cluster", cluster.ID)
	cmd := kubectlForCluster(cluster, "delete", "namespace", ns, "--ignore-not-found")
	if out, err := cmd.CombinedOutput(); err != nil {
		logger.Warn("deprovision: namespace delete failed", "namespace", ns, "output", string(out), "error", err)
	}

	// Step 2: NFS storage requires extra cleanup — PV is cluster-scoped and
	// OCI FSS resources live outside K8s. Dynamic storage (CephFS) cascades
	// with the namespace delete, so no extra steps needed.
	if cluster.StorageType == storageTypeNFS || cluster.StorageType == "" {
		// Delete cluster-scoped PV (not removed by namespace deletion).
		pvName := "hive-" + hiveID + "-fss-pv"
		logger.Info("deprovision: deleting PV", "pv", pvName)
		cmd = kubectlForCluster(cluster, "delete", "pv", pvName, "--ignore-not-found")
		if out, err := cmd.CombinedOutput(); err != nil {
			logger.Warn("deprovision: PV delete failed", "pv", pvName, "output", string(out), "error", err)
		}

		// Delete OCI NFS export (must happen before file system deletion).
		if h.OCIExportID != "" {
			logger.Info("deprovision: deleting OCI export", "exportID", h.OCIExportID)
			if err := deleteOCIExport(h.OCIExportID, logger); err != nil {
				logger.Warn("deprovision: OCI export delete failed", "exportID", h.OCIExportID, "error", err)
			}
		}

		// Delete OCI file system (must be empty of exports first).
		if h.OCIFileSystemID != "" {
			logger.Info("deprovision: deleting OCI file system", "fileSystemID", h.OCIFileSystemID)
			if err := deleteOCIFileSystem(h.OCIFileSystemID, logger); err != nil {
				logger.Warn("deprovision: OCI file system delete failed", "fileSystemID", h.OCIFileSystemID, "error", err)
			}
		}
	}

	// Step 3: For OpenShift with SCC, remove the SCC binding. The ServiceAccount
	// is already gone with the namespace, but the cluster-scoped SCC binding may linger.
	if cluster.RequiresSCC {
		sccName := cluster.SCCName
		if sccName == "" {
			sccName = "anyuid"
		}
		logger.Info("deprovision: removing SCC binding", "scc", sccName, "namespace", ns)
		cmd = kubectlForCluster(cluster, "adm", "policy", "remove-scc-from-user", sccName,
			fmt.Sprintf("system:serviceaccount:%s:%s", ns, sccServiceAccountName))
		if out, err := cmd.CombinedOutput(); err != nil {
			// oc adm may not be available via kubectl; log but don't fail.
			logger.Warn("deprovision: SCC removal failed (may need manual cleanup)", "output", string(out), "error", err)
		}
	}

	// Step 4: Remove the on-disk SaaS hive record.
	hiveDir := filepath.Join(saasHivesDir, hiveID)
	if err := os.RemoveAll(hiveDir); err != nil {
		logger.Warn("deprovision: failed to remove hive directory", "path", hiveDir, "error", err)
	}

	// Step 5: Remove hive from all user records (decrement quota).
	for _, u := range listAllSaaSUsers() {
		if _, ok := u.Hives[hiveID]; ok {
			delete(u.Hives, hiveID)
			if err := saveSaaSUser(&u); err != nil {
				logger.Warn("deprovision: failed to update user record", "user", u.GitHubUsername, "error", err)
			}
		}
	}

	logger.Info("audit: hive deprovisioned", "hive_id", hiveID, "owner", h.Owner, "cluster", cluster.ID)
}

// migrateHive moves a hive from one cluster to another. For v1 this is a
// "fresh provision on target, deprovision on source" approach — no data copy.
// The hive rebuilds its state from GitHub on the new cluster.
func (s *HubServer) migrateHive(h *SaaSHive, fromCluster, toCluster *ClusterConfig) {
	logger := s.logger.With("hive_id", h.ID, "from", fromCluster.ID, "to", toCluster.ID)
	logger.Info("audit: migration started")

	// Step 1: Build a synthetic CreateHiveRequest from the existing hive metadata
	// so we can reuse the standard provisioning path.
	req := &CreateHiveRequest{
		Org:         h.Org,
		Repos:       strings.Join(h.Repos, ","),
		PrimaryRepo: h.PrimaryRepo,
		ACMMLevel:   h.ACMMLevel,
		ClusterID:   toCluster.ID,
	}

	// Read the existing secret from the source cluster to pass credentials through.
	ns := "hive-hosted-" + h.ID
	tokenOut, err := kubectlForCluster(fromCluster, "get", "secret", "hive-secrets", "-n", ns,
		"-o", "jsonpath={.data.github-token}").Output()
	if err == nil && len(tokenOut) > 0 {
		// Token is base64-encoded in the secret; kubectl jsonpath returns raw base64.
		decoded, decErr := base64Decode(string(tokenOut))
		if decErr == nil {
			req.GitHubToken = decoded
		}
	}

	// Check for GitHub App credentials.
	appKeyOut, err := kubectlForCluster(fromCluster, "get", "secret", "hive-secrets", "-n", ns,
		"-o", "jsonpath={.data.gh-app-key\\.pem}").Output()
	if err == nil && len(appKeyOut) > 0 {
		decoded, decErr := base64Decode(string(appKeyOut))
		if decErr == nil && strings.HasPrefix(strings.TrimSpace(decoded), "-----BEGIN") {
			req.AppPrivateKey = decoded
			req.AuthMethod = "app"
			// Read app_id and installation_id from the ConfigMap.
			appIDOut, _ := kubectlForCluster(fromCluster, "get", "configmap", "hive-config", "-n", ns,
				"-o", "jsonpath={.data.hive\\.yaml}").Output()
			if len(appIDOut) > 0 {
				configStr := string(appIDOut)
				req.AppID = extractYAMLValue(configStr, "app_id")
				req.InstallationID = extractYAMLValue(configStr, "installation_id")
			}
		}
	}

	// Determine visibility from the source (is_public). The registry may
	// know the hive is public even when the SaaS record predates the
	// IsPublic field (pre-#1604 provisions), so only ever upgrade to
	// public here — never downgrade a record the owner toggled public.
	s.mu.RLock()
	for _, re := range s.registry.Hives {
		if re.ID == h.ID {
			h.IsPublic = h.IsPublic || re.IsPublic
			break
		}
	}
	s.mu.RUnlock()

	// Update the hive subdomain and dashboard URL for the new cluster.
	h.Subdomain = h.ID + "." + toCluster.Domain

	// Step 2: Provision on target cluster.
	logger.Info("migration: provisioning on target cluster")
	if provErr := provisionHive(h, req, toCluster, logger); provErr != nil {
		logger.Error("migration: provisioning on target failed", "error", provErr)
		h.MigrationStatus = "failed"
		h.Error = fmt.Sprintf("migration failed during provisioning on %s: %s", toCluster.ID, provErr.Error())
		h.Status = "running" // restore previous status
		saveSaaSHive(h)
		return
	}

	// Step 3: Deprovision on source cluster (best-effort cleanup).
	logger.Info("migration: deprovisioning source cluster")
	deprovisionHive(h, fromCluster, logger)

	// Step 4: Update hive records to point to the new cluster.
	h.ClusterID = toCluster.ID
	h.Status = "provisioning" // will flip to "running" via the provision watcher
	h.MigrationStatus = "completed"
	h.Error = ""
	if saveErr := saveSaaSHive(h); saveErr != nil {
		logger.Error("migration: failed to save updated hive record", "error", saveErr)
	}

	// Update the registry entry's ClusterID and ClusterName in-memory.
	s.mu.Lock()
	for i := range s.registry.Hives {
		if s.registry.Hives[i].ID == h.ID {
			s.registry.Hives[i].ClusterID = toCluster.ID
			s.registry.Hives[i].ClusterName = toCluster.Name
			s.registry.Hives[i].DashboardURL = "https://" + h.Subdomain
			break
		}
	}
	s.mu.Unlock()
	s.requestSave()

	logger.Info("audit: migration completed", "new_cluster", toCluster.ID, "subdomain", h.Subdomain)
}

// base64Decode decodes a standard base64 string. Returns empty string on error.
func base64Decode(s string) (string, error) {
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		// Try URL-safe encoding as fallback.
		data, err = base64.URLEncoding.DecodeString(strings.TrimSpace(s))
		if err != nil {
			return "", err
		}
	}
	return string(data), nil
}

// extractYAMLValue does a simple line-based extraction of a key: value from
// a YAML string. This avoids pulling in a YAML parser for two fields.
func extractYAMLValue(yamlStr, key string) string {
	for _, line := range strings.Split(yamlStr, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, key+":") {
			val := strings.TrimPrefix(trimmed, key+":")
			return strings.TrimSpace(val)
		}
	}
	return ""
}

// startProvisionWatcher polls provisioning hives until ctx is cancelled. It
// takes a context so the loop can be stopped for a clean shutdown (and so tests
// can stop the goroutine rather than leaking it past the test, which otherwise
// races the package-level state that per-test temp-dir setup rewrites).
func (s *HubServer) startProvisionWatcher(ctx context.Context) {
	ticker := time.NewTicker(provisionPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		hives := listSaaSHives()
		for _, h := range hives {
			if h.Status != "provisioning" {
				continue
			}
			created, _ := time.Parse(time.RFC3339, h.CreatedAt)
			if time.Since(created) > provisionTimeout {
				h.Status = "error"
				h.Error = "provisioning timed out"
				saveSaaSHive(&h)
				s.logger.Warn("saas hive provision timeout", "hive_id", h.ID)
				continue
			}

			cluster := s.clusterForHive(&h)
			if cluster == nil {
				s.logger.Warn("no cluster config for hive", "hive_id", h.ID, "cluster_id", h.ClusterID)
				continue
			}
			ns := "hive-hosted-" + h.ID
			cmd := kubectlForCluster(cluster, "get", "deployment", "hive", "-n", ns, "-o", "jsonpath={.status.availableReplicas}")
			out, err := cmd.Output()
			if err != nil {
				continue
			}
			if strings.TrimSpace(string(out)) == "1" {
				h.Status = "running"
				// Clear migration tracking once the hive is running on the new cluster.
				if h.MigrationStatus == "completed" || h.MigrationStatus == "migrating" {
					s.logger.Info("audit: post-migration hive running", "hive_id", h.ID, "cluster", cluster.ID)
					h.MigrationStatus = ""
					h.MigrationFrom = ""
					h.MigrationTo = ""
					h.MigrationStartedAt = ""
				}
				saveSaaSHive(&h)
				s.logger.Info("audit: saas hive running", "hive_id", h.ID, "cluster", cluster.ID)
			}
		}
	}
}

const k8sManifestTemplate = `apiVersion: v1
kind: Namespace
metadata:
  name: {{.Namespace}}
---
{{- if .RequiresSCC}}
apiVersion: v1
kind: ServiceAccount
metadata:
  name: hive-sa
  namespace: {{.Namespace}}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: hive-sa-scc-{{.SCCName}}
  namespace: {{.Namespace}}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: system:openshift:scc:{{.SCCName}}
subjects:
- kind: ServiceAccount
  name: hive-sa
  namespace: {{.Namespace}}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: hive-self-upgrade
  namespace: {{.Namespace}}
rules:
- apiGroups: ["apps"]
  resources: ["deployments"]
  resourceNames: ["hive"]
  verbs: ["get", "patch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: hive-self-upgrade
  namespace: {{.Namespace}}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: hive-self-upgrade
subjects:
- kind: ServiceAccount
  name: hive-sa
  namespace: {{.Namespace}}
---
{{- end}}
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: hive-secrets-writer
  namespace: {{.Namespace}}
rules:
# Least privilege: the hive pod may read and patch ONLY its own
# hive-secrets Secret, so dashboard-entered API keys (e.g. the LiteLLM
# key) are stored in the Secret instead of on the PVC.
- apiGroups: [""]
  resources: ["secrets"]
  resourceNames: ["hive-secrets"]
  verbs: ["get", "patch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: hive-secrets-writer
  namespace: {{.Namespace}}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: hive-secrets-writer
subjects:
- kind: ServiceAccount
{{- if .RequiresSCC}}
  name: hive-sa
{{- else}}
  name: default
{{- end}}
  namespace: {{.Namespace}}
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: hive-config
  namespace: {{.Namespace}}
data:
  hive.yaml: |
    project:
      org: {{.Org}}
      repos: {{.Repos}}
      primary_repo: {{.PrimaryRepo}}
    agents:
      guide:
        backend: copilot
        model: claude-sonnet-4-6
        enabled: true
      scanner:
        backend: copilot
        model: claude-sonnet-4-6
        enabled: true
    governor:
      eval_interval_s: 300
      modes:
        idle:
          threshold: 0
          guide: 4h
          scanner: 4h
        busy:
          threshold: 10
          guide: 2h
          scanner: 2h
    github:
{{- if .UseApp}}
      app_id: {{.AppID}}
      installation_id: {{.InstallationID}}
      key_file: /secrets/gh-app-key.pem
{{- else}}
      token: "${HIVE_GITHUB_TOKEN}"
{{- end}}
{{- if .HasGHE}}
      base_url: {{.GitHubBaseURL}}
      api_url: {{.GitHubAPIURL}}
{{- end}}
{{- if .OAuthClientID}}
      oauth_client_id: {{.OAuthClientID}}
{{- end}}
    knowledge:
      enabled: true
      engine: llm-wiki
      bead_synthesizer:
        schedule: hourly
        min_confidence: 0.5
        target_layer: project
        max_facts_per_cycle: 20
        vault_path: /data/vaults/bead-synth-wiki
        retention_policy:
          max_beads: 5000
          archive_after_synth_days: 7
          high_priority_retain_days: 30
          preserve_with_deps: true
      vaults:
        - name: bead-synth-wiki
          path: /data/vaults/bead-synth-wiki
          auto_index: true
          git_sync: false
    dashboard:
      port: {{.DashboardPort}}
      # hub_proxied is true ONLY on the nginx-ingress path (hive-oke), where the
      # hub's auth-proxy (see the auth-url/auth-signin/auth-response-headers
      # annotations below) authenticates every request and injects trusted
      # X-Hive-User/X-Hive-Role headers. That keeps the hive from flipping into
      # standalone direct-route mode when it carries an authorized_users list
      # (which would strip those headers and disable the shared token, breaking
      # the dashboard link and the snapshot preview).
      #
      # On the OpenShift-Route path (vllm-d) there is NO nginx auth-proxy — the
      # hive is reached directly — so it stays hub_proxied:false and correctly
      # enforces per-user device-flow authz itself from authorized_users.
      hub_proxied: {{.IsNginxIngress}}
    hub:
      enabled: true
      url: https://hive.kubestellar.io
      dashboard_url: {{.DashboardURL}}
      hive_type: {{.HiveType}}
      is_public: {{.IsPublic}}
    acmm_level: {{.ACMMLevel}}
---
apiVersion: v1
kind: Secret
metadata:
  name: hive-secrets
  namespace: {{.Namespace}}
type: Opaque
stringData:
  dashboard-token: {{.DashboardToken}}
{{- if .UseAppFull}}
  gh-app-key.pem: |
{{.AppPrivateKey}}
{{- else if not .UseApp}}
  github-token: {{.Token}}
{{- end}}
---
{{- if .IsNFSStorage}}
apiVersion: v1
kind: PersistentVolume
metadata:
  name: {{.PVName}}
spec:
  capacity:
    storage: {{.NFSStorageCapacity}}
  accessModes:
    - ReadWriteMany
  persistentVolumeReclaimPolicy: Retain
  nfs:
    server: {{.NFSMountTargetIP}}
    path: {{.NFSExportPath}}
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: hive-data
  namespace: {{.Namespace}}
spec:
  accessModes:
    - ReadWriteMany
  resources:
    requests:
      storage: {{.NFSStorageCapacity}}
  volumeName: {{.PVName}}
  storageClassName: ""
{{- end}}
{{- if .IsDynamicStorage}}
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: hive-data
  namespace: {{.Namespace}}
spec:
  accessModes:
    - ReadWriteMany
  storageClassName: {{.StorageClass}}
  resources:
    requests:
      storage: {{.DynamicPVCStorage}}
{{- end}}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: hive
  namespace: {{.Namespace}}
spec:
  replicas: 1
  # Zero-downtime rollout: maxUnavailable=0 keeps the old pod serving until the
  # surge pod passes its readinessProbe, so the OpenShift router never sees zero
  # Ready endpoints (which renders as "Application is not available"). This
  # REQUIRES the hive-data PVC to be ReadWriteMany so both pods can mount /data
  # during the surge — see the PVC definitions above (NFS and dynamic storage
  # both request ReadWriteMany). A ReadWriteOnce PVC would deadlock the surge
  # pod on volume attach across nodes; do not set maxSurge>0 with an RWO volume.
  strategy:
    type: RollingUpdate
    rollingUpdate:
      maxSurge: {{.RolloutMaxSurge}}
      maxUnavailable: {{.RolloutMaxUnavailable}}
  selector:
    matchLabels:
      app: hive
      hive-id: {{.ID}}
  template:
    metadata:
      labels:
        app: hive
        hive-id: {{.ID}}
    spec:
{{- if .RequiresSCC}}
      serviceAccountName: hive-sa
{{- end}}
      initContainers:
      - name: copy-config
        image: ghcr.io/kubestellar/hive:{{.ImageTag}}
        imagePullPolicy: {{.ImagePullPolicy}}
        command: ["sh", "-c", "cp /etc/hive-seed/hive.yaml /etc/hive/hive.yaml && echo configmap-copied; if [ -f /data/hive.yaml.bak ]; then echo backup-exists-for-recovery; fi"]
        volumeMounts:
        - name: config
          mountPath: /etc/hive-seed
          readOnly: true
        - name: config-writable
          mountPath: /etc/hive
        - name: data
          mountPath: /data
{{- if .RequiresSCC}}
      - name: init-permissions
        image: ghcr.io/kubestellar/hive:{{.ImageTag}}
        imagePullPolicy: {{.ImagePullPolicy}}
        # Best-effort ownership normalization. /data is already 1001:1000 on
        # these hives, so this recursive chown is belt-and-suspenders and must
        # never be fatal: on OpenShift the restricted SCC rejects runAsUser:0
        # and assigns an arbitrary non-root UID, so the chown runs as a
        # non-owner and fails on any file it doesn't own (e.g. a stale
        # root-owned /data/*.tmp). Suppress errors AND force exit 0 so a
        # non-ownable file can't crash-loop init and wedge the rolling update
        # (maxUnavailable=0/maxSurge=1 means a never-Ready surge pod hangs the
        # rollout indefinitely). We deliberately do NOT request runAsUser:0:
        # it is invalid under the restricted SCC and pointless here since the
        # files that matter are already correctly owned — letting the SCC
        # assign the UID makes the chown a no-op success.
        command: ["sh", "-c", "chown -R {{.InitContainerUID}}:{{.InitContainerGID}} /data 2>/dev/null || true; echo permissions-set"]
        volumeMounts:
        - name: data
          mountPath: /data
{{- end}}
      containers:
      - name: hive
        image: ghcr.io/kubestellar/hive:{{.ImageTag}}
        imagePullPolicy: {{.ImagePullPolicy}}
        securityContext:
          capabilities:
            add:
            - NET_ADMIN
        readinessProbe:
          httpGet:
            path: /api/health
            port: {{.DashboardPort}}
          initialDelaySeconds: 5
          periodSeconds: 5
          failureThreshold: 3
        startupProbe:
          httpGet:
            path: /api/health
            port: {{.DashboardPort}}
          initialDelaySeconds: 10
          periodSeconds: 5
          failureThreshold: 30
        livenessProbe:
          # /api/livez (not /api/health) so a heartbeat goroutine that dies
          # silently while the HTTP server stays up still gets caught and the
          # pod restarted. /api/health only proves the dashboard's HTTP
          # server is responsive; it does not check that the heartbeat loop is
          # still running, so a hive with a wedged loop but a live server
          # would otherwise stay 1/1 Running forever while the hub shows it
          # offline (gray dot) with nothing to recover it.
          #
          # /api/livez keys off heartbeat *attempts*, not successes, so an
          # unreachable or rejecting hub never kills the pod — a restart
          # cannot fix a network partition, and gating on it crash-looped
          # healthy spokes on firewalled clusters. Heartbeat freshness is
          # reported via /api/health/deep instead. See
          # pkg/dashboard/server.go handleLivez; hives without a hub
          # configured are exempt (guarded by hub.HeartbeatEnabled()).
          httpGet:
            path: /api/livez
            port: {{.DashboardPort}}
          periodSeconds: 30
          failureThreshold: 3
        env:
{{- if not .UseApp}}
        - name: HIVE_GITHUB_TOKEN
          valueFrom:
            secretKeyRef:
              name: hive-secrets
              key: github-token
{{- end}}
        - name: DASHBOARD_AUTH_TOKEN
          valueFrom:
            secretKeyRef:
              name: hive-secrets
              key: dashboard-token
        - name: HIVE_ID
          value: "{{.ID}}"
        - name: HIVE_LEVEL
          value: "{{.ACMMLevel}}"
        - name: HIVE_HUB_URL
          value: https://hive.kubestellar.io
        - name: HIVE_HUB_SECRET
          value: "{{.HubSecret}}"
{{- if .AuthorizedUsers}}
        - name: HIVE_AUTHORIZED_USERS
          value: "{{.AuthorizedUsers}}"
{{- end}}
{{- if .HasInference}}
        - name: HIVE_VLLM_ENDPOINT
          value: "{{.InferenceEndpoint}}"
{{- end}}
        ports:
        - name: terminal
          containerPort: {{.TerminalPort}}
        - name: dashboard
          containerPort: {{.DashboardPort}}
        resources:
          requests:
            cpu: {{.CPURequest}}
            memory: {{.MemRequest}}
          limits:
            cpu: {{.CPULimit}}
            memory: {{.MemLimit}}
        volumeMounts:
        - name: config-writable
          mountPath: /etc/hive
        - name: data
          mountPath: /data
        # hive-secrets is always whole-volume-mounted (no subPath) so keys
        # the pod patches into the Secret (e.g. litellm_api_key entered in
        # the dashboard) propagate to /secrets/<key> without a restart.
        - name: secrets
          mountPath: /secrets
          readOnly: true
      volumes:
      - name: config
        configMap:
          name: hive-config
      - name: config-writable
        emptyDir: {}
      - name: data
        persistentVolumeClaim:
          claimName: hive-data
      - name: secrets
        secret:
          secretName: hive-secrets
---
apiVersion: v1
kind: Service
metadata:
  name: hive
  namespace: {{.Namespace}}
spec:
  selector:
    app: hive
    hive-id: {{.ID}}
  ports:
  - name: terminal
    port: {{.TerminalPort}}
    targetPort: {{.TerminalPort}}
  - name: dashboard
    port: {{.DashboardPort}}
    targetPort: {{.DashboardPort}}
  type: ClusterIP
{{- if .IsNginxIngress}}
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: hive
  namespace: {{.Namespace}}
  annotations:
    cert-manager.io/cluster-issuer: {{.CertIssuer}}
    nginx.ingress.kubernetes.io/auth-url: "https://hive.kubestellar.io/api/saas/auth-check?hive={{.ID}}&uri=$request_uri"
    nginx.ingress.kubernetes.io/custom-http-errors: "502,503"
    nginx.ingress.kubernetes.io/default-backend: hive-error-pages
    nginx.ingress.kubernetes.io/auth-signin: "https://hive.kubestellar.io/login?redirect=$scheme://$http_host$request_uri"
    nginx.ingress.kubernetes.io/auth-response-headers: "X-Hive-User,X-Hive-Role"
spec:
  ingressClassName: {{.IngressClass}}
  rules:
  - host: {{.DashboardHost}}
    http:
      paths:
      - path: /
        pathType: Prefix
        backend:
          service:
            name: hive
            port:
              number: {{.DashboardPort}}
  tls:
  - hosts:
    - {{.DashboardHost}}
    secretName: hive-tls
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: hive-contribute
  namespace: {{.Namespace}}
  annotations:
    cert-manager.io/cluster-issuer: {{.CertIssuer}}
    nginx.ingress.kubernetes.io/proxy-read-timeout: "3600"
    nginx.ingress.kubernetes.io/proxy-send-timeout: "3600"
spec:
  ingressClassName: {{.IngressClass}}
  rules:
  - host: {{.DashboardHost}}
    http:
      paths:
      - path: /api/contribute
        pathType: Prefix
        backend:
          service:
            name: hive
            port:
              number: {{.DashboardPort}}
  tls:
  - hosts:
    - {{.DashboardHost}}
    secretName: hive-tls
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: hive-terminal
  namespace: {{.Namespace}}
  annotations:
    cert-manager.io/cluster-issuer: {{.CertIssuer}}
    nginx.ingress.kubernetes.io/proxy-read-timeout: "3600"
    nginx.ingress.kubernetes.io/proxy-send-timeout: "3600"
spec:
  ingressClassName: {{.IngressClass}}
  rules:
  - host: {{.DashboardHost}}
    http:
      paths:
      - path: /terminal
        pathType: Prefix
        backend:
          service:
            name: hive
            port:
              number: {{.TerminalPort}}
  tls:
  - hosts:
    - {{.DashboardHost}}
    secretName: hive-tls
{{- end}}
{{- if .IsOpenShiftRoute}}
---
apiVersion: route.openshift.io/v1
kind: Route
metadata:
  name: hive-dashboard
  namespace: {{.Namespace}}
spec:
  host: {{.DashboardHost}}
  path: /
  to:
    kind: Service
    name: hive
    weight: 100
  port:
    targetPort: dashboard
  tls:
    termination: edge
    insecureEdgeTerminationPolicy: Redirect
---
apiVersion: route.openshift.io/v1
kind: Route
metadata:
  name: hive-terminal
  namespace: {{.Namespace}}
spec:
  host: {{.DashboardHost}}
  path: /terminal
  to:
    kind: Service
    name: hive
    weight: 100
  port:
    targetPort: terminal
  tls:
    termination: edge
    insecureEdgeTerminationPolicy: Redirect
{{- end}}
`

// addVanityHostToIngress makes the spoke actually serve a second, friendlier
// hostname. Provisioning templates exactly one DashboardHost, so a vanity URL
// derived from the claimed org/repo has no backend until something creates a
// route for it — and every hub link to that host (including the /open SSO
// handoff) returns 503 while the raw placeholder host works fine.
//
// nginx clusters: patch the existing Ingresses, appending the vanity host as an
// extra rule (copied from the placeholder rule) and adding it to the TLS block
// so cert-manager issues for it.
// OpenShift clusters: a Route carries a single host, so add parallel
// "<name>-vanity" Routes instead.
//
// Returns an error when the host could not be made servable; the caller then
// leaves VanityURL empty so the hive keeps its working placeholder host.
func (s *HubServer) addVanityHostToIngress(hiveID, vanityHost string, cluster *ClusterConfig) error {
	if cluster == nil || vanityHost == "" {
		return fmt.Errorf("no cluster or vanity host")
	}
	ns := "hive-hosted-" + hiveID
	placeholderHost := hiveID + "." + cluster.Domain

	if cluster.IngressType == ingressTypeOpenShiftRoute {
		for _, base := range []string{"hive-dashboard", "hive-terminal"} {
			out, err := kubectlForCluster(cluster, "-n", ns, "get", "route", base,
				"-o", "jsonpath={.spec.port.targetPort}|{.spec.path}").Output()
			if err != nil {
				continue // route absent on this spoke — nothing to mirror
			}
			parts := strings.SplitN(string(out), "|", 2)
			targetPort := parts[0]
			path := "/"
			if len(parts) == 2 && parts[1] != "" {
				path = parts[1]
			}
			manifest := fmt.Sprintf(`apiVersion: route.openshift.io/v1
kind: Route
metadata: { name: %s-vanity, namespace: %s, labels: { app: hive } }
spec:
  host: %s
  path: %s
  port: { targetPort: %s }
  tls: { termination: edge, insecureEdgeTerminationPolicy: Redirect }
  to: { kind: Service, name: hive, weight: 100 }
  wildcardPolicy: None
`, base, ns, vanityHost, path, targetPort)
			cmd := kubectlForCluster(cluster, "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(manifest)
			if err := cmd.Run(); err != nil {
				return fmt.Errorf("applying %s-vanity route: %w", base, err)
			}
		}
		return nil
	}

	// nginx: append a rule + TLS host to each existing Ingress via a JSON patch.
	patched := 0
	for _, ing := range []string{"hive", "hive-contribute", "hive-terminal"} {
		raw, err := kubectlForCluster(cluster, "-n", ns, "get", "ingress", ing, "-o", "json").Output()
		if err != nil {
			continue // not every spoke has all three
		}
		var obj map[string]any
		if err := json.Unmarshal(raw, &obj); err != nil {
			continue
		}
		spec, _ := obj["spec"].(map[string]any)
		if spec == nil {
			continue
		}
		rules, _ := spec["rules"].([]any)
		var base map[string]any
		for _, r := range rules {
			rm, _ := r.(map[string]any)
			if rm == nil {
				continue
			}
			if h, _ := rm["host"].(string); h == vanityHost {
				base = nil // already present
				goto tls
			} else if h == placeholderHost {
				base = rm
			}
		}
		if base != nil {
			clone := map[string]any{"host": vanityHost, "http": base["http"]}
			spec["rules"] = append(rules, clone)
		}
	tls:
		if tlsList, ok := spec["tls"].([]any); ok {
			for _, t := range tlsList {
				tm, _ := t.(map[string]any)
				if tm == nil {
					continue
				}
				hosts, _ := tm["hosts"].([]any)
				found := false
				for _, h := range hosts {
					if hs, _ := h.(string); hs == vanityHost {
						found = true
					}
				}
				if !found {
					tm["hosts"] = append(hosts, vanityHost)
				}
			}
		}
		patchBody, err := json.Marshal(map[string]any{"spec": spec})
		if err != nil {
			continue
		}
		cmd := kubectlForCluster(cluster, "-n", ns, "patch", "ingress", ing, "--type", "merge", "-p", string(patchBody))
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("patching ingress %s: %w", ing, err)
		}
		patched++
	}
	if patched == 0 {
		return fmt.Errorf("no ingress found in %s to add %s to", ns, vanityHost)
	}
	return nil
}
