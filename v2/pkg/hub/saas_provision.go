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
	"strconv"
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

	// routeBaseDashboard and routeBaseTerminal name the Routes that provisioning
	// creates on an OpenShift spoke. addVanityHostToIngress mirrors each one into
	// a parallel "<name>-vanity" Route, because a Route carries a single host and
	// so cannot simply gain the vanity hostname the way an nginx Ingress rule can.
	routeBaseDashboard = "hive-dashboard"
	routeBaseTerminal  = "hive-terminal"

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

// ACMM level bounds for a SELF-SERVICE provision request (the get-started
// wizard). Narrower than the assign bounds above on purpose: nobody starts a
// hive above L3 Measured. L4 Adaptive and higher are reached after
// provisioning, from the hive's own dashboard, once the project's test
// coverage and CI history have earned the extra automation. The wizard renders
// levels up to maxRequestACMMLevel as radios and lists the rest as "available
// after provisioning"; this pair is the enforcement behind that copy, since the
// wizard is only one possible client of the endpoint.
const (
	minRequestACMMLevel = 1
	maxRequestACMMLevel = 3
)

// clustersConfigPath is the on-disk location where the hub reads cluster
// definitions. It is a JSON array of ClusterConfig objects. A var (not a const)
// so tests can point it at a temp file; production never reassigns it.
var clustersConfigPath = "/data/saas/clusters.json"

// ClusterConfig describes a Kubernetes cluster that the hub can provision
// hive spokes onto. Each cluster has its own kubeconfig, storage backend,
// ingress style, and optional platform-specific settings (OCI, OpenShift).
type ClusterConfig struct {
	ID                string `json:"id" yaml:"id"`
	Name              string `json:"name" yaml:"name"`
	KubeconfigPath    string `json:"kubeconfig_path,omitempty" yaml:"kubeconfig_path,omitempty"`
	Context           string `json:"context" yaml:"context"`
	InCluster         bool   `json:"in_cluster" yaml:"in_cluster"`
	StorageClass      string `json:"storage_class" yaml:"storage_class"`
	StorageType       string `json:"storage_type" yaml:"storage_type"`
	NFSMountIP        string `json:"nfs_mount_ip,omitempty" yaml:"nfs_mount_ip,omitempty"`
	IngressType       string `json:"ingress_type" yaml:"ingress_type"`
	IngressClass      string `json:"ingress_class,omitempty" yaml:"ingress_class,omitempty"`
	CertIssuer        string `json:"cert_issuer,omitempty" yaml:"cert_issuer,omitempty"`
	Domain            string `json:"domain" yaml:"domain"`
	DomainPrefix      string `json:"domain_prefix,omitempty" yaml:"domain_prefix,omitempty"`
	OCICompartment    string `json:"oci_compartment,omitempty" yaml:"oci_compartment,omitempty"`
	OCIAvailDomain    string `json:"oci_avail_domain,omitempty" yaml:"oci_avail_domain,omitempty"`
	OCIMountTarget    string `json:"oci_mount_target,omitempty" yaml:"oci_mount_target,omitempty"`
	OCIExportSet      string `json:"oci_export_set,omitempty" yaml:"oci_export_set,omitempty"`
	RequiresSCC       bool   `json:"requires_scc" yaml:"requires_scc"`
	SCCName           string `json:"scc_name,omitempty" yaml:"scc_name,omitempty"`
	HasGPU            bool   `json:"has_gpu" yaml:"has_gpu"`
	Arch              string `json:"arch" yaml:"arch"`
	ImageTag          string `json:"image_tag" yaml:"image_tag"`
	ImagePullPolicy   string `json:"image_pull_policy,omitempty" yaml:"image_pull_policy,omitempty"`
	InferenceEndpoint string `json:"inference_endpoint,omitempty" yaml:"inference_endpoint,omitempty"`
	GitHubBaseURL     string `json:"github_base_url,omitempty" yaml:"github_base_url,omitempty"`
	GitHubAPIURL      string `json:"github_api_url,omitempty" yaml:"github_api_url,omitempty"`
	// GitHubAppSlug is the GitHub App's URL slug on THIS cluster's GitHub
	// host. A GitHub Enterprise instance hosts its own App registration,
	// which is rarely named "kubestellar-hive" — without this the spoke's
	// install link points at an app that does not exist on that GHE host.
	// Empty falls back to config.DefaultGitHubAppSlug.
	GitHubAppSlug string `json:"github_app_slug,omitempty" yaml:"github_app_slug,omitempty"`
	// GitHubAppID is the numeric App ID registered on THIS cluster's GitHub
	// host. It is the non-secret half of the cluster's App identity — the
	// matching PRIVATE KEY is deliberately NOT a field here, because this struct
	// is parsed from clusters.json and flows along operator-facing paths. The
	// key lives in its own 0600 file keyed by cluster ID (see
	// cluster_app_key.go); this field is what tells the hub such a key is
	// expected and which App it belongs to.
	//
	// Zero means "this cluster has no hub-managed App identity" — the hub then
	// changes nothing about its spokes' credentials.
	GitHubAppID                 int64  `json:"github_app_id,omitempty" yaml:"github_app_id,omitempty"`
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
	ID          string `json:"id"`
	Owner       string `json:"owner"`
	ProjectName string `json:"project_name"`
	Org         string `json:"org"`
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

// resolveProvisionAppID chooses the GitHub App ID to provision a hive with,
// following the hive's GitHub HOST. The App may live on github.com or GitHub
// Enterprise depending on the repos; the App ID must match, because the public
// github.com App (config.Default... / the 3568013 default carried in req.AppID)
// cannot mint installation tokens against a github.ibm.com repo.
//
// Rule:
//   - The hive is explicitly pinned to PUBLIC github.com on a cluster whose App
//     is a GitHub Enterprise one → keep the request's app_id. The cluster's App
//     belongs to the wrong host for this hive; forcing it would break exactly
//     the case the public pin exists to protect.
//   - Otherwise the cluster names a GitHubAppID → use it. This now covers a
//     public-github.com CLUSTER as well as a GHE one: the App ID is a
//     per-GitHub-HOST constant, not per-hive state, and this field is the one
//     place the fleet records it.
//   - Otherwise → the request's app_id verbatim, so explicit overrides and
//     clusters with no hub-managed App identity are unchanged.
//
// The public-github.com case was previously excluded (the rule required
// effectiveGitHubBaseURL != ""), which is why every hive on the public cluster
// provisioned with the config.PlaceholderAppID sentinel and NOTHING ever
// replaced it: there was no configured App ID to look up, and assignment does
// not mint one. Such a hive builds a JWT for a nonexistent App, so App auth can
// never succeed no matter which installation_id the owner enters.
//
// Widening the rule rather than adding a github.com-specific branch is what
// generalizes: a third forge (gitlab/gitea) needs only its own cluster entry
// with a github_app_id, not another special case here.
//
// This does NOT hardcode an App ID. A cluster that names none still falls
// through to the request value exactly as before.
//
// Returns a string (the template field is a string); sanitize() is applied to
// any request-sourced value exactly as before.
func resolveProvisionAppID(reqAppID string, h *SaaSHive, cluster *ClusterConfig) string {
	if cluster == nil || cluster.GitHubAppID == 0 {
		return sanitize(reqAppID)
	}
	// A hive pinned to public github.com on a cluster whose own App is a GHE
	// App: the cluster App is registered on the wrong host for this hive, so the
	// request's public app_id stands. Detected as "the hive resolves to no GHE
	// base URL while the cluster has one" — the same public-pin semantics
	// effectiveGitHubBaseURL already implements.
	if cluster.GitHubBaseURL != "" && effectiveGitHubBaseURL(h, cluster) == "" {
		return sanitize(reqAppID)
	}
	return strconv.FormatInt(cluster.GitHubAppID, 10)
}

// backfillGitHubHostFromCluster returns the GitHub host a hive should inherit
// from its cluster, or "" when there is nothing to fill in.
//
// A hive's GitHubHost is otherwise only ever set from an explicitly pasted
// org URL at assign time. Placeholders provisioned BEFORE their cluster gained
// github_base_url/github_api_url therefore keep GitHubHost == "" forever, and
// projectConfigForHiveID pushes gheAPIURLForHost("") == "" on every heartbeat
// — which the spoke reads as "leave mine alone". The result is a hive on a GHE
// cluster still pointing at api.github.com with the public app_id (observed on
// vllm-d: hosted-available-vllmd-01 in org "katamari" has base_url: "",
// api_url: "", app_id: 3568013 against a github.ibm.com cluster).
//
// This only ever fills a blank: a hive that already carries a host — including
// an explicit "public" override, which effectiveGitHubBaseURL resolves to ""
// — is returned unchanged.
func backfillGitHubHostFromCluster(h *SaaSHive, cluster *ClusterConfig) string {
	if h == nil || h.GitHubHost != "" || cluster == nil {
		return ""
	}
	base := effectiveGitHubBaseURL(h, cluster)
	if base == "" {
		return "" // public github.com — nothing to record
	}
	return githubHostLabel(base)
}

// repairGitHubHostsFromClusters retroactively fills in a blank GitHubHost on
// hives that were ALREADY assigned, using their cluster's GHE defaults.
//
// backfillGitHubHostFromCluster only runs at assign time, so every hive claimed
// before that fix keeps GitHubHost == "" forever — and with it an empty
// github_api_url pushed on every heartbeat, which the spoke reads as "leave
// mine alone". Those hives sit on a github.ibm.com cluster while talking to
// api.github.com (hosted-available-vllmd-01 in org "katamari" is the live
// example). Nothing else ever repairs them.
//
// It is deliberately conservative and idempotent:
//   - It only ever fills a BLANK GitHubHost. An explicitly set host is never
//     overwritten, and neither is an explicit "public" override — that resolves
//     to an empty effective base URL, which backfillGitHubHostFromCluster
//     already refuses to record.
//   - It changes nothing when the hive's cluster has no GHE host configured.
//   - Unclaimed placeholders are skipped: assign owns those, and it now
//     backfills the host itself.
//   - Every change is logged with before/after.
//
// Returns the number of hives changed.
//
// It runs LAZILY, per hive, from the heartbeat path (repairGitHubHostForHive)
// rather than as a startup sweep. A goroutine at hub start would walk the hive
// directory concurrently with the first heartbeats already writing to it, for
// no benefit: a hive that never beats does not need repairing, and one that
// does gets repaired on its very next beat.
//
// NOTE: this repairs the HOST only. A hive that also carries the public
// app_id/installation_id is NOT fixed by this — the GitHub App still has to be
// installed on the target org before installation auto-discovery can self-heal
// it. See gitHubAppStillRequiredNote.
func (s *HubServer) repairGitHubHostsFromClusters() int {
	repaired := 0
	for _, h := range listSaaSHives() {
		if s.repairGitHubHostForHive(h.ID) {
			repaired++
		}
	}
	if repaired > 0 {
		s.logger.Info("github host repair: complete", "hives_repaired", repaired,
			"note", gitHubAppStillRequiredNote)
	}
	return repaired
}

// repairGitHubHostForHive applies the retroactive host repair to a SINGLE hive,
// reporting whether it changed anything. Called on each heartbeat, so it must
// be cheap and silent in the overwhelmingly common no-op case — every guard
// below returns before any write.
func (s *HubServer) repairGitHubHostForHive(hiveID string) bool {
	h := loadSaaSHive(hiveID)
	if h == nil {
		return false
	}
	if h.Status == statusAvailable {
		return false // unclaimed placeholder — assign backfills it at claim time
	}
	if h.GitHubHost != "" {
		return false // explicitly set — never clobber
	}
	// backfillGitHubHostFromCluster also returns "" for an explicit "public"
	// override (effectiveGitHubBaseURL resolves the sentinel to public) and for
	// a cluster with no GHE host, so both are covered by this one check.
	host := backfillGitHubHostFromCluster(h, s.clusterForHive(h))
	if host == "" {
		return false
	}
	h.GitHubHost = host
	if err := saveSaaSHive(h); err != nil {
		s.logger.Error("github host repair: failed to save hive",
			"hive", hiveID, "error", err)
		return false
	}
	s.logger.Info("github host repair: filled blank github_host from cluster defaults",
		"hive", hiveID,
		"cluster", clusterIDForHive(h),
		"org", h.Org,
		"github_host_before", "",
		"github_host_after", host,
		"github_api_url_after", gheAPIURLForHost(host),
		"note", gitHubAppStillRequiredNote)
	return true
}

// gitHubAppStillRequiredNote is the operator-facing caveat attached to a host
// repair. Filling in the host points the spoke at the right GitHub API, but it
// does NOT change the hive's app_id/installation_id: a hive provisioned with
// the PUBLIC App credentials still needs the GitHub Enterprise App installed on
// its org before installation auto-discovery can find an installation to adopt.
// Until a human does that, the hive authenticates as nothing on its GHE host.
const gitHubAppStillRequiredNote = "github_host repaired; a hive still carrying the public app_id also needs the GitHub Enterprise App installed on its org before its installation can be auto-discovered"

// repairVanityURLForHive retroactively mints the friendly vanity host for a hive
// that was CLAIMED BEFORE the vanity feature existed, reporting whether it
// changed anything.
//
// h.VanityURL is written in exactly ONE place — handleAssignHive, at claim time.
// Every read path (claimedVanityURL, and through it My Hives, the SSO /open
// handoff, and migrateHive) treats an empty VanityURL as "no validated host, keep
// the placeholder". So a hive claimed before that code shipped keeps
// VanityURL == "" forever and displays/opens its raw placeholder host no matter
// how many times it heartbeats — nothing else ever fills it in. That is the
// 14-claimed-hives-still-on-placeholder-IDs report: those records have a real org
// but a placeholder id, and a placeholder id is NOT evidence the fix is missing,
// only that the vanity URL was never minted for them.
//
// This deliberately does NOT touch the hive's id. The id is a k8s namespace
// suffix (hive-hosted-<id>), an ingress hostname, a per-agent socket path and the
// SSO token's "h" claim; renaming it is a different and far larger migration. The
// goal is only that the DISPLAYED name and the OPENED URL are the vanity ones.
//
// It is conservative and idempotent, mirroring repairGitHubHostForHive:
//   - Unclaimed placeholders are skipped: assign owns those and mints the vanity
//     URL itself at claim time.
//   - A hive that already has a VanityURL is never touched, so re-running is a
//     no-op and a hive keeps one stable vanity host (the id's random suffix is
//     regenerated per call, so re-minting would churn the host every beat).
//   - A hive with no resolvable cluster domain, or an incomplete claim (no org or
//     no primary repo), is skipped — there is nothing to derive a host from.
//   - The host is adopted ONLY once it is actually servable. Unlike the claim
//     path, which adopts optimistically because provisioning is creating the
//     spoke's routes anyway, a retroactive repair has no such guarantee: the
//     vanity host is a NEW name that resolves to no backend until an
//     ingress/route exists for it. Adopting it before then would replace an
//     ugly-but-working placeholder link with a 503, so addVanityHostToIngress
//     must succeed first. On a heartbeat-only cluster the hub cannot kubectl to
//     the spoke, so that call fails and the hive correctly keeps its working
//     placeholder host.
//
// Called on each heartbeat, so it must be cheap and silent in the overwhelmingly
// common no-op case — every guard below returns before any network call or write.
func (s *HubServer) repairVanityURLForHive(hiveID string) bool {
	h := loadSaaSHive(hiveID)
	if h == nil {
		return false
	}
	if h.Status == statusAvailable {
		return false // unclaimed placeholder — assign mints it at claim time
	}
	if h.VanityURL != "" {
		return false // already has one — never re-mint (idempotency + stable host)
	}
	// An incomplete claim has nothing to derive a friendly host from.
	if h.Org == "" || h.PrimaryRepo == "" {
		return false
	}
	cluster := s.clusterForHive(h)
	if cluster == nil || cluster.Domain == "" {
		return false
	}
	// Reuse the host an EXISTING vanity route already serves before minting a
	// new one. generateHiveID appends a fresh random suffix on every call, so a
	// hive whose repair keeps failing used to burn a brand-new hostname every
	// heartbeat (observed churning four hosts in eight minutes). Worse, when a
	// vanity_url had been cleared by hand to repair a mismatch, the next beat
	// minted a DIFFERENT suffix instead of re-adopting the host the spoke's
	// route was already serving — leaving the registry pointing at a hostname
	// nothing serves while the working route sat unused. Adopting the existing
	// route's host first makes this repair converge instead of oscillate.
	vanityHost := s.existingVanityHost(hiveID, cluster)
	if vanityHost == "" {
		vanityHost = generateHiveID(h.Org, h.PrimaryRepo) + "." + cluster.Domain
	}
	// Make it servable BEFORE adopting it; on failure leave VanityURL empty so
	// every read path keeps falling back to the working placeholder host.
	if err := s.makeVanityHostServable(hiveID, vanityHost, cluster); err != nil {
		s.logger.Info("vanity url repair: host is not servable yet, keeping the placeholder host",
			"hive", hiveID, "host", vanityHost, "cluster", cluster.ID, "error", err)
		return false
	}
	h.VanityURL = "https://" + vanityHost
	if err := saveSaaSHive(h); err != nil {
		s.logger.Error("vanity url repair: failed to save hive", "hive", hiveID, "error", err)
		return false
	}
	s.logger.Info("vanity url repair: minted a vanity host for a hive claimed before the vanity feature",
		"hive", hiveID, "org", h.Org, "primary_repo", h.PrimaryRepo,
		"cluster", cluster.ID, "vanity_url", h.VanityURL)
	return true
}

// existingVanityHost returns the host an already-created vanity route/ingress
// on the spoke is serving, or "" when there is none (or the cluster cannot be
// reached).
//
// This exists so the retroactive repair is CONVERGENT. The vanity host carries
// a random suffix, so re-minting is not idempotent: any path that mints a host
// without first looking for the one already in place will invent a new name,
// and the hub can end up advertising a hostname that no route serves while the
// real route answers on a different one. Reading the live object first makes
// the repair adopt reality instead of overwriting it.
//
// Best-effort by design: on any error it returns "" and the caller mints a
// fresh host exactly as before, so an unreachable cluster is no worse off.
func (s *HubServer) existingVanityHost(hiveID string, cluster *ClusterConfig) string {
	if cluster == nil {
		return ""
	}
	ns := "hive-hosted-" + hiveID
	placeholderHost := hiveID + "." + cluster.Domain

	if cluster.IngressType == ingressTypeOpenShiftRoute {
		out, err := kubectlForCluster(cluster, "-n", ns, "get", "route",
			routeBaseDashboard+"-vanity", "-o", "jsonpath={.spec.host}").Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}

	// nginx: the vanity host is the rule whose host is not the placeholder.
	out, err := kubectlForCluster(cluster, "-n", ns, "get", "ingress", "hive",
		"-o", "jsonpath={.spec.rules[*].host}").Output()
	if err != nil {
		return ""
	}
	for _, host := range strings.Fields(string(out)) {
		if host != placeholderHost && host != "" {
			return host
		}
	}
	return ""
}

// makeVanityHostServable creates the ingress/route that makes vanityHost resolve,
// returning an error when it could not be made servable. It is the one seam the
// retroactive repair goes through: production uses addVanityHostToIngress, while
// tests substitute it to exercise the adopt path without a live cluster (kubectl
// is unavailable under test, so the real call always fails there).
func (s *HubServer) makeVanityHostServable(hiveID, vanityHost string, cluster *ClusterConfig) error {
	if s.vanityHostServable != nil {
		return s.vanityHostServable(hiveID, vanityHost, cluster)
	}
	return s.addVanityHostToIngress(hiveID, vanityHost, cluster)
}

// repairVanityURLsForClaimedHives applies the retroactive vanity-URL repair to
// every hive, returning the number changed. Safe to run repeatedly: each hive is
// gated by repairVanityURLForHive, which no-ops once a vanity URL exists.
//
// DELIBERATELY NOT CALLED IN PRODUCTION, for the same reason as its sibling
// repairGitHubHostsFromClusters: the repair runs LAZILY, per hive, from the
// heartbeat path, and a startup sweep would walk the hive directory
// concurrently with the first heartbeats already writing to it for no benefit —
// a hive that never beats needs no repair, and one that does gets repaired on
// its very next beat. It is retained as the fleet-level entry point (and is
// covered by a test asserting sweep-level idempotency) so an operator-triggered
// backfill has one correct implementation to call rather than an ad-hoc loop.
// This is NOT dead code left behind by accident; do not read its absence from
// the heartbeat path as missing coverage.
func (s *HubServer) repairVanityURLsForClaimedHives() int {
	repaired := 0
	for _, h := range listSaaSHives() {
		if s.repairVanityURLForHive(h.ID) {
			repaired++
		}
	}
	if repaired > 0 {
		s.logger.Info("vanity url repair: complete", "hives_repaired", repaired)
	}
	return repaired
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
		// AppID / AppSlug follow the hive's GitHub HOST (the App may be on
		// github.com OR github.ibm.com, per the repos). A GHE hive must get the
		// cluster's GitHub Enterprise App — not the public github.com App
		// (3568013), which cannot mint tokens against github.ibm.com. When the
		// request didn't carry an App id and the hive is GHE, fall back to the
		// cluster's GitHubAppID/GitHubAppSlug. Public-GitHub hives are unchanged
		// (req value, else config's public default downstream). OAuth is a SEPARATE
		// concern and is always github.com — see OAuthClientID below.
		"AppID":          resolveProvisionAppID(req.AppID, h, cluster),
		"InstallationID": sanitize(req.InstallationID),
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
		// app_slug follows the hive's GitHub host: a GHE hive gets the cluster's
		// GHE App slug (e.g. "kubestellar-hive-ghe"); a public-GitHub hive emits
		// none, letting config.ResolvedAppSlug() supply the public default. Only
		// pair the cluster slug with a GHE hive so a public hive on a GHE-defaulted
		// cluster is never mislabeled with the GHE app.
		"GitHubAppSlug": func() string {
			if effectiveGitHubBaseURL(h, cluster) != "" {
				return cluster.GitHubAppSlug
			}
			return ""
		}(),
		"OAuthClientID": func() string {
			// OAuth / device-flow LOGIN is ALWAYS github.com, decoupled from the
			// App/repo host — every user signs in with a github.com identity even
			// on a GHE hive. Always emit the public github.com Device Flow client
			// ID; never the cluster's (possibly GHE) client. Mirrors the spoke-side
			// GitHubConfig.OAuthClientIDResolved() / OAuthBaseURL() split.
			return publicGitHubOAuthClientID
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
	// A migration re-provisions the hive under a fresh placeholder Subdomain on
	// the target cluster, but a CLAIMED hive must keep showing its vanity host —
	// resetting DashboardURL to the placeholder subdomain here is exactly what
	// left claimed, migrated hives back on the placeholder URL in the hub. Prefer
	// the validated meta vanity_url for a claimed hive; only an unclaimed
	// placeholder falls back to its new placeholder subdomain.
	migratedURL := "https://" + h.Subdomain
	if v := claimedVanityURL(h); v != "" {
		migratedURL = v
	}
	s.mu.Lock()
	for i := range s.registry.Hives {
		if s.registry.Hives[i].ID == h.ID {
			s.registry.Hives[i].ClusterID = toCluster.ID
			s.registry.Hives[i].ClusterName = toCluster.Name
			s.registry.Hives[i].DashboardURL = migratedURL
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
{{- end}}
# hive-self-upgrade must NOT be gated on RequiresSCC. The spoke patches its own
# Deployment (UpgradeSelfToSHA / SwitchImageSelf in pkg/hub/self_upgrade.go) on
# EVERY cluster, not just OpenShift ones. While this Role was inside the
# RequiresSCC conditional block, non-OpenShift spokes ran as the "default"
# ServiceAccount with no patch permission, so every hub-instructed upgrade hit
# HTTP 403, fell through to os.Exit(0), and the pod restarted onto the SAME
# image — a crash-loop that re-ran on each heartbeat and never advanced. It also
# froze the init containers on whatever tag they were provisioned with, since
# the only code path that rewrites init-container images is the very patch that
# was being denied.
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
{{- if .RequiresSCC}}
  name: hive-sa
{{- else}}
  name: default
{{- end}}
  namespace: {{.Namespace}}
---
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
{{- if .GitHubAppSlug}}
      app_slug: {{.GitHubAppSlug}}
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
    nginx.ingress.kubernetes.io/auth-response-headers: "X-Hive-User,X-Hive-Role,X-Hive-Proxy-Auth"
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
		// Mirror the nginx branch's accounting: count what was actually applied,
		// so "every source Route was unreachable" reports failure instead of
		// success. Without this the loop `continue`s past every missing/
		// unreachable Route and still returns nil, telling the caller the host is
		// servable when no Route was created — the retroactive repair then adopts
		// an unservable vanity host and replaces a working placeholder link with
		// a 503.
		applied := 0
		for _, base := range []string{routeBaseDashboard, routeBaseTerminal} {
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
			applied++
		}
		if applied == 0 {
			return fmt.Errorf("no route found in %s to mirror %s onto (cluster unreachable or spoke has no %s/%s route)",
				ns, vanityHost, routeBaseDashboard, routeBaseTerminal)
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
