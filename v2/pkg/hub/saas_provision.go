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
	"sort"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/kubestellar/hive/v2/pkg/config"
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
// point its status flips to statusAssigned and it behaves like any owned hive.
const (
	// statusAvailable marks a pre-provisioned placeholder hive that is idle and
	// waiting to be claimed. Only such a hive may be assigned.
	statusAvailable = "available"
	// statusAssigned marks a placeholder that has been CLAIMED by a user. It is
	// the authoritative "no longer available" signal.
	//
	// The claim paths (handleApproveProvision, handleAssignHive) previously set
	// Status = "" here. That empty string is indistinguishable from "unknown",
	// so every downstream availability check that keys off ProvStatus (the
	// landing-page fleet tiles, computeFleetStats) fell back to the immutable
	// "hosted-available-" ID prefix — which a claimed placeholder KEEPS forever
	// — and mis-counted 22 claimed hives as still available (38 shown vs the
	// true 17). Setting an explicit assigned status makes ProvStatus the single
	// authoritative signal: a claimed hive reads provStatus!="available" without
	// any prefix guessing. A live spoke later overwrites this with its real
	// lifecycle status ("provisioning"/"running") on the next heartbeat; until
	// then "assigned" is the correct, honest state.
	statusAssigned = "assigned"
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
// hive above L3 Quality-Gated. L4 Security-Aware and higher are reached after
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
	GitHubAppID int64 `json:"github_app_id,omitempty" yaml:"github_app_id,omitempty"`
	// PullOnly marks a cluster the hub CANNOT reach: its spokes connect
	// outbound over the heartbeat and nothing here can kubectl into them.
	//
	// WHY THIS FIELD EXISTS. Registration used to require a kubeconfig_path,
	// so a pull-only pool could not be registered AT ALL — the loader dropped
	// the entry ("skipping remote cluster with no kubeconfig_path"). Everything
	// keyed off the cluster registry then silently misfired for its hives:
	// appIdentityForHive returned nil, so five a-ks-wec2 spokes sat on the
	// placeholder app_id logging "cluster names no github app — cannot repair",
	// naming a remedy for a cluster entry that could not exist; and
	// clusterForHive fell back to the DEFAULT cluster, so vanity repair ran
	// kubectl against hive-oke looking for a namespace that lives elsewhere.
	//
	// Registering such a cluster makes its identity (App, forge URLs, domain)
	// available to the heartbeat paths that do work, while every kubectl path
	// must first ask KubectlReachable and skip rather than aim at the wrong
	// cluster.
	PullOnly      bool   `json:"pull_only,omitempty" yaml:"pull_only,omitempty"`
	OAuthClientID string `json:"oauth_client_id,omitempty" yaml:"oauth_client_id,omitempty"`
	// Forges carries App identity for EVERY forge this cluster can serve, keyed
	// by bare forge host ("github.com", "github.ibm.com").
	//
	// WHY THIS FIELD EXISTS. The flat github_app_id/github_app_slug/github_*_url
	// fields above give a cluster exactly ONE identity slot, which made a
	// cluster's forge a PIN rather than a DEFAULT. On 2026-07-31 that cost seven
	// hives their identity: adding github_app_slug: kubestellar-hive-ghe to the
	// vllm-d entry handed a GHE App to every public-GitHub hive on the cluster,
	// because there was nowhere else for a public identity to live. Those hives
	// had already ELECTED github.com in their meta.json github_host — the hub
	// knew the right answer and had no field in which to act on it.
	//
	// Per the platform owner: "vllm-d is used for GHE, but gh can be elected."
	// A cluster's forge is a DEFAULT with per-hive election. This map is what
	// makes a public election on a GHE-default cluster RESOLVABLE instead of
	// 409-ing as "incomplete".
	//
	// BACKWARD COMPATIBILITY: an entry that omits this map still works. The flat
	// fields are read as the DEFAULT forge's identity (see forgesForCluster), so
	// today's single-identity clusters.json parses and behaves unchanged. This
	// field is purely additive.
	Forges map[string]ClusterForgeIdentity `json:"forges,omitempty" yaml:"forges,omitempty"`
	// DefaultForge names which forge a hive on this cluster gets when it has not
	// elected one, as a bare host. Empty means "infer from the flat fields" —
	// github_base_url/github_api_url if set, else public github.com — which is
	// exactly today's behaviour.
	DefaultForge                string `json:"default_forge,omitempty" yaml:"default_forge,omitempty"`
	ClusterHealthTimeoutSeconds int    `json:"cluster_health_timeout_seconds,omitempty" yaml:"cluster_health_timeout_seconds,omitempty"`
}

// ClusterForgeIdentity is one forge's App registration on one cluster. It is
// the non-secret half only: as with the flat GitHubAppID field, the matching
// PRIVATE KEY lives in its own 0600 file keyed by cluster ID, never here.
//
// AppID and AppSlug are BOTH required for an identity to count as complete —
// see forgeIdentityForTarget, which refuses a switch rather than let an empty
// slug fall back to the public default and build an install link that 404s on
// GHE.
type ClusterForgeIdentity struct {
	AppID   int64  `json:"app_id,omitempty" yaml:"app_id,omitempty"`
	AppSlug string `json:"app_slug,omitempty" yaml:"app_slug,omitempty"`
	// APIURL and BaseURL are optional. For a forge in config.forgeIdentities
	// they are derivable from the host, so a cluster entry need only name the
	// App. They exist for a third GHE instance the derivation table does not
	// know, where the URLs cannot be inferred.
	APIURL  string `json:"api_url,omitempty" yaml:"api_url,omitempty"`
	BaseURL string `json:"base_url,omitempty" yaml:"base_url,omitempty"`
}

// forgeHostKey reduces any spelling of a forge — bare host, web URL, or API
// URL — to the canonical bare-host key used by ClusterConfig.Forges.
//
// githubHostLabel alone is NOT sufficient: it strips the scheme but keeps the
// path, so an api_url of https://github.ibm.com/api/v3 yields
// "github.ibm.com/api/v3", which matches no map key and no target host. Every
// lookup into the forges map must go through this.
func forgeHostKey(s string) string {
	h := strings.ToLower(githubHostLabel(s))
	h = strings.TrimSuffix(strings.TrimRight(h, "/"), "/api/v3")
	h = strings.SplitN(h, "/", 2)[0]
	if h == "" || h == "api.github.com" {
		return publicForgeHost
	}
	return h
}

// forgesForCluster returns the cluster's per-forge identity map, synthesising
// the legacy single-identity shape when `forges` is absent.
//
// This is the compatibility seam. A cluster entry written before this change
// has its flat github_app_id/github_app_slug read as the identity of the forge
// its github_base_url/github_api_url name — which is precisely what those
// fields have always meant. An entry that carries an explicit `forges` map wins
// outright, and a map entry for a given host overrides the synthesised one.
func forgesForCluster(c *ClusterConfig) map[string]ClusterForgeIdentity {
	out := map[string]ClusterForgeIdentity{}
	if c == nil {
		return out
	}
	// Legacy flat fields -> the identity of the cluster's own (default) forge.
	if c.GitHubAppID != 0 || strings.TrimSpace(c.GitHubAppSlug) != "" {
		out[clusterDefaultForge(c)] = ClusterForgeIdentity{
			AppID:   c.GitHubAppID,
			AppSlug: strings.TrimSpace(c.GitHubAppSlug),
			APIURL:  c.GitHubAPIURL,
			BaseURL: c.GitHubBaseURL,
		}
	}
	// Explicit per-forge entries override the synthesised one.
	for host, id := range c.Forges {
		out[forgeHostKey(host)] = id
	}
	return out
}

// clusterDefaultForge returns the forge a hive on this cluster gets when it has
// elected nothing: explicit default_forge, else the host named by the flat URL
// fields, else public github.com.
func clusterDefaultForge(c *ClusterConfig) string {
	if c == nil {
		return publicForgeHost
	}
	if f := strings.TrimSpace(c.DefaultForge); f != "" {
		return forgeHostKey(f)
	}
	if c.GitHubBaseURL != "" {
		return forgeHostKey(c.GitHubBaseURL)
	}
	if c.GitHubAPIURL != "" {
		return forgeHostKey(c.GitHubAPIURL)
	}
	return publicForgeHost
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

// unreachableKubeconfigSentinel is aimed at instead of a real kubeconfig when a
// caller builds a kubectl command for a cluster the hub cannot reach.
//
// It must never exist. An empty --kubeconfig makes kubectl fall back to its
// AMBIENT configuration, which inside the hub pod is the hub's OWN cluster — so
// a command meant for a pull-only pool would quietly execute against hive-oke
// and report success for work that never happened there. Naming a path that
// cannot resolve turns that silent misfire into a loud failure.
const unreachableKubeconfigSentinel = "/nonexistent/pull-only-cluster-is-not-reachable-from-the-hub"

// KubectlReachable reports whether the hub can run kubectl against this cluster.
//
// Callers that are about to touch a cluster with kubectl must check this and
// skip with a clear message: a pull-only cluster's spokes are reached ONLY by
// answering their outbound heartbeat.
func (c *ClusterConfig) KubectlReachable() bool {
	if c == nil {
		return false
	}
	if c.PullOnly {
		return false
	}
	return c.InCluster || c.KubeconfigPath != ""
}

func kubectlArgsForCluster(cluster *ClusterConfig, args ...string) []string {
	fullArgs := []string{}
	if !cluster.InCluster {
		kubeconfig := cluster.KubeconfigPath
		if !cluster.KubectlReachable() || kubeconfig == "" {
			// Defence in depth: the call sites guard on KubectlReachable, and
			// this makes a missed guard fail loudly rather than execute against
			// whatever ambient cluster kubectl would otherwise choose.
			kubeconfig = unreachableKubeconfigSentinel
		}
		fullArgs = append(fullArgs, "--kubeconfig", kubeconfig, "--context", cluster.Context)
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
	ClaimDelivered bool `json:"claim_delivered,omitempty"`
	// ACMMDelivered flips true once the spoke has reported back the ACMM level
	// the hub assigned it. It is the level's OWN half of the claim handshake,
	// deliberately separate from ClaimDelivered.
	//
	// WHY IT CANNOT REUSE ClaimDelivered
	//
	// #2333 put ACMM on the ClaimDelivered handshake, which is right for hives
	// claimed from then on. But ClaimDelivered is PERSISTED STATE, and every
	// hive claimed before that change already had it set to true under the old
	// rule, which required only org/repos to match. For those hives both halves
	// of the fix are permanently disabled on the first beat after upgrade: the
	// push is gated on !ClaimDelivered (already false, so never pushes) and the
	// adopt is gated on ClaimDelivered (already true, so the stale spoke report
	// is adopted). The requested level was overwritten in meta long ago, so hub
	// and spoke now agree on the wrong number and no drift is even detectable.
	// That is exactly the live hosted-available-oke-11-placeholder-r05x state:
	// an approved acmm_level 3 request running, and displaying, as L2.
	//
	// A separate flag defaults to false on every existing hive, so the level
	// delivery re-arms once for hives that never got it — without re-opening the
	// org/repos claim, and without a migration.
	ACMMDelivered bool `json:"acmm_delivered,omitempty"`
	// RequestedACMMLevel is the level the approved provision request asked for,
	// recorded at assign time so it survives the spoke overwriting ACMMLevel in
	// meta. It is the level the hub delivers; ACMMLevel is the level currently
	// in effect. Zero means "nothing was explicitly requested" — the ordinary
	// case for admin-created and pre-existing hives — and delivers nothing.
	RequestedACMMLevel int `json:"requested_acmm_level,omitempty"`

	// RequestedGitHubHost / ForgeDelivered are the FORGE half of the same
	// delivery handshake, added for the operator-facing forge switch
	// (handleSwitchForge). They exist for exactly the reason ACMMDelivered
	// does, and the reason is worth stating because it was verified the hard
	// way on the live fleet.
	//
	// WHY A HUB-SIDE EDIT OF GitHubHost DOES NOT STICK
	//
	// github_host was edited to "github.ibm.com" in a live hive's meta.json and
	// the hub restarted. Within one beat the registry was back to "github.com".
	// handleHeartbeat records RegistryEntry.GitHubHost from the SPOKE's
	// HeartbeatPayload.GitHubHost every beat, and the spoke derives that from
	// its own config via GitHubConfig.HostLabel(). The hub's edit changed
	// nothing the spoke reports, so the spoke simply re-reported the unchanged
	// truth and the hub adopted it. This is the same adopt-stale-value class as
	// the ACMM revert fixed in #2354.
	//
	// RequestedGitHubHost records what the operator asked for, so it survives
	// the spoke reporting the old host, and ForgeDelivered latches true the
	// moment the spoke reports the requested host back — after which the push
	// stops for good and the spoke owns the value again. Both default to their
	// zero values on every existing hive, so nothing is delivered to a hive
	// nobody switched: the push is gated on a NON-EMPTY RequestedGitHubHost.
	// RequestedAppReset asks the spoke to CLEAR its installation_id, so the
	// hive falls back to "App not installed" and prompts the owner to install
	// it again.
	//
	// WHY THIS NEEDS ITS OWN FIELD RATHER THAN JUST PUSHING ZERO
	//
	// The wire cannot express a clear. The spoke adopts a pushed installation
	// only when it is non-zero (cmd/hive: `if ghCfg.InstallationID != 0`),
	// deliberately — zero means "the hub is not speaking to this field", and
	// treating it as "blank it" once turned a key-only fault into a total auth
	// outage. So a reset has to be a distinct instruction, not a value.
	//
	// It is a REQUEST, persisted on the hive record, because the operator's
	// intent has to survive a missed beat and a hub restart. Cleared when the
	// spoke reports installation_id 0 back — read-back confirmation, the same
	// contract the forge switch uses.
	//
	// The live case: a hive provisioned with its OWN GitHub App kept that App's
	// installation_id after its identity was later moved to the fleet's public
	// App. app_id, app_slug and key_file all named the public App while the
	// installation belonged to another, so every freshly minted token returned
	// "404 Not Found" while cached ones kept working — 84 failures in three
	// hours on one hive, with no config field visibly wrong.
	RequestedAppReset bool `json:"requested_app_reset,omitempty"`

	RequestedGitHubHost string `json:"requested_github_host,omitempty"`
	// ForgeDelivered flips true once the spoke reports the requested forge host.
	ForgeDelivered bool `json:"forge_delivered,omitempty"`

	// PendingAppConfig is a GitHub App identity queued for delivery to the
	// spoke over the heartbeat.
	//
	// WHY IT IS PERSISTED HERE RATHER THAN HELD IN MEMORY
	//
	// This delivery used to live only in HubServer.pendingGitHubAppConfigs, an
	// in-memory map drained with delete() on the first beat that carried it. It
	// had two failure modes with no diagnosis path:
	//
	//   - a hub restart between arming and the next beat lost the push
	//     silently, and nothing anywhere recorded that an identity change had
	//     been requested; and
	//   - being one-shot, a spoke that missed or failed to apply that single
	//     beat never got another.
	//
	// Persisting it to meta.json makes an outstanding identity change durable
	// and inspectable — the same reason RequestedACMMLevel and
	// RequestedGitHubHost live here rather than in memory.
	//
	// nil means nothing is queued. It is CLEARED on confirmation, not on
	// delivery (see AppIdentityDelivered), so a lost beat is retried.
	PendingAppConfig *PendingAppIdentity `json:"pending_app_config,omitempty"`

	// RequestedRestartAt (RFC3339) arms a rolling restart of this hive's spoke,
	// delivered on every heartbeat inside spokeRestartDeliveryWindow and then
	// cleared. Durable on the hive record for the same reason as
	// PendingAppConfig: an in-memory flag dies with a hub restart and a missed
	// beat, and the whole point of this switch is reaching spokes the hub
	// cannot touch any other way.
	RequestedRestartAt string `json:"requested_restart_at,omitempty"`

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

	// AssignedAt (RFC3339) records WHEN this placeholder was last claimed — set by
	// both claim paths (handleApproveProvision, handleAssignHive) the moment they
	// flip Status to statusAssigned. It exists so the self-heal sweep can measure
	// how long a placeholder has been stuck at Status=statusAssigned &&
	// !ClaimDelivered and reset one that never completed its claim (a spoke frozen
	// on an old image never reports the org/repos back, so ClaimDelivered stays
	// false forever). Empty on records claimed before this field existed and on
	// unclaimed placeholders; a zero/absent stamp makes the sweep skip the hive
	// rather than reset it on an unknowable age.
	AssignedAt string `json:"assigned_at,omitempty"`
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
	// Routed through the ONE resolver so provisioning, the heartbeat answer and
	// the key lookup cannot drift apart again. The rule below is the one this
	// function already implemented — a hive pinned to public github.com on a
	// GHE-default cluster keeps its public app_id, because the cluster's App is
	// registered on the wrong host for it — now generalised to both directions
	// and shared with every other caller.
	identity := ResolveHiveIdentity(h, cluster)
	if identity.FromHiveIntent {
		// The hive elected a forge its cluster does not serve. If the hub can
		// name an App there, use it; otherwise the REQUEST's app_id stands,
		// exactly as before — the cluster's App would be the wrong forge's.
		if identity.AppID != 0 && identity.AppID != cluster.GitHubAppID {
			return strconv.FormatInt(identity.AppID, 10)
		}
		return sanitize(reqAppID)
	}
	return strconv.FormatInt(identity.AppID, 10)
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
	// A per-hive base URL is a stated DECISION and wins outright — including the
	// "public" sentinel, which effectiveGitHubBaseURL resolves to "" and which
	// must STAY public (return "") rather than being re-GHE'd from the cluster.
	// So any non-empty github_base_url short-circuits the cluster fallback: a real
	// per-hive host is recorded as-is, and the public sentinel records nothing.
	if strings.TrimSpace(h.GitHubBaseURL) != "" {
		base := effectiveGitHubBaseURL(h, cluster)
		if base == "" {
			return "" // explicit public pin — stay public
		}
		return githubHostLabel(base)
	}
	// No per-hive host at all: fall back to the cluster's DEFAULT forge, forge-map
	// shape included. The old code read only the flat github_base_url via
	// effectiveGitHubBaseURL, so a GHE cluster written as `default_forge` +
	// `forges` map (blank flat github_base_url) resolved to public here and left
	// GitHubHost blank — the spoke then kept api.github.com with the public app_id
	// even though the cluster is GHE (the EPM placeholder). clusterDefaultForge
	// honours default_forge, so a GHE default now backfills the GHE host however
	// it is written. A public default still returns "" (nothing to record).
	forge := clusterDefaultForge(cluster)
	if isPublicForgeHost(forge) {
		return "" // public github.com — nothing to record
	}
	return forge
}

// FORGE IS PER-HIVE. Two repairs used to live here that keyed a hive's forge on
// its CLUSTER's default — repairGitHubHostForHive (filled a blank github_host
// from the cluster forge on every beat) and guardGHEHiveNotOnPublicForge
// (rewrote a blank-or-public github_host to the cluster's GHE host whenever the
// spoke reported the public forge). Both are gone, on purpose.
//
// A cluster hosts hives of BOTH forges: vllm-d carries github.ibm.com projects
// (certus, EPM, …) beside github.com projects (ibm/alchemy-logging — the org
// "ibm" lives on public github.com). At 23:56Z on 2026-08-05, minutes after the
// cluster-keyed guard shipped, it rewrote github_host on every vllm-d hive whose
// spoke reported the public App — github.com projects included — and the
// delivery that followed flipped their spokes onto app 5686 / github.ibm.com,
// degrading 9 of them with "404 Not Found" on every token mint. Cluster
// membership proves nothing about any one hive's forge; only the hive's own
// recorded github_host (empty = public github.com, the field's documented
// meaning) does.
//
// What remains, all keyed on per-hive evidence:
//   - assign/approve record github_host from the pasted org URL (and
//     backfillGitHubHostFromCluster fills it ONCE, at claim time, for legacy
//     requests that named no host);
//   - reconcileGitHubHostFromSpoke backfills an EMPTY github_host from a spoke
//     whose whole github block coherently, workingly names a forge — it never
//     overwrites an explicit record (a spoke's report is downstream of hub
//     delivery, so a contradiction is a mis-delivery echo, not evidence);
//   - repairMisflippedForgeFromRequest (below) restores a github_host the
//     cluster-keyed guard stomped, from the provision request's own record;
//   - the wrong-forge identity repair (decideAppKeySync /
//     appKeyReasonWrongForgeApp) re-delivers the hive's OWN forge — in both
//     directions — when the spoke's app_id provably belongs to another forge.

// repairMisflippedForgeFromRequest restores a claimed hive's github_host after
// the 2026-08-05 cluster-keyed guard stomped it, using the hive's own provision
// request — the surviving record of which forge the org actually lives on — as
// the authority. It reports whether it changed anything.
//
// THE STATE IT REPAIRS. The removed guard rewrote github_host from blank /
// "github.com" to the cluster's GHE host, and the delivery that followed
// flipped the spoke's whole github block onto the GHE App. Hub record and spoke
// now AGREE on the wrong forge, so no drift-based repair can see the fault; the
// only remaining evidence of the hive's true forge is the provision request the
// owner filed, which records the org's host explicitly ("public" or a pasted
// GHE host — the onboarding form always sends one).
//
// Every gate is positive evidence, and ALL must hold:
//   - the hive is CLAIMED, with no operator flow owning the host: no in-flight
//     forge switch (RequestedGitHubHost) and no COMPLETED one either
//     (ForgeDelivered — a deliberate later switch outranks the original
//     request);
//   - its recorded github_host is currently a GHE host;
//   - its own provision request (matched by AssignedHive) EXPLICITLY records
//     public github.com. A blank request host is treated as unknown — requests
//     filed before the host field existed carry "", and flipping a genuine GHE
//     hive public on that silence would recreate this incident in reverse;
//   - the spoke POSITIVELY reports it is running that same GHE forge (it
//     adopted the mis-delivery) AND reports failing App auth
//     (spokeAppAuthFailing) — a healthy hive is never touched, whatever the
//     paperwork says.
//
// The write restores github_host to public github.com. The same beat's identity
// reconcile then resolves the hive's public App (per-hive election), the
// wrong-forge repair delivers the complete public set — explicit public urls,
// public app_id, and an installation reset riding with the app change — and the
// spoke's RediscoverAndAdopt re-adopts its old public installation, which is
// valid again under the restored app_id.
func (s *HubServer) repairMisflippedForgeFromRequest(payload *HeartbeatPayload) bool {
	if s == nil || payload == nil {
		return false
	}
	h := loadSaaSHive(payload.HiveID)
	if !hiveClaimed(h) {
		return false // unclaimed placeholder (or no record) — assign owns its host
	}
	if h.RequestedGitHubHost != "" || h.ForgeDelivered {
		return false // an operator forge switch owns (or deliberately set) the host
	}
	current := forgeHostLabel(h.GitHubHost)
	if h.GitHubHost == "" || isPublicForgeHost(current) {
		return false // nothing GHE recorded — nothing to restore
	}
	req := loadProvisionRequest(h.Owner)
	if req == nil || req.AssignedHive != h.ID {
		return false // no surviving request record for THIS hive — no authority
	}
	reqHost := strings.TrimSpace(req.GitHubHost)
	requestSaysPublic := strings.EqualFold(reqHost, githubHostPublic) ||
		(reqHost != "" && isPublicForgeHost(forgeHostLabel(reqHost)))
	if !requestSaysPublic {
		return false // request names GHE, or is silent — silence is not evidence
	}
	// The spoke must have adopted the mis-delivered GHE forge AND be failing on
	// it. A spoke still on public, or one that is healthy, is not this fault.
	spokeForge := config.GitHubConfig{
		BaseURL: strings.TrimSpace(payload.GitHubBaseURL),
		APIURL:  strings.TrimSpace(payload.GitHubAPIURL),
	}.HostLabel()
	if !sameGitHubHost(forgeHostLabel(spokeForge), current) {
		return false
	}
	if !spokeAppAuthFailing(payload) {
		return false
	}
	before := h.GitHubHost
	h.GitHubHost = publicForgeHost
	if err := saveSaaSHive(h); err != nil {
		s.logger.Error("github forge restore: failed to save hive",
			"hive", payload.HiveID, "error", err)
		return false
	}
	s.logger.Warn("github forge restore: a public-github.com hive was mis-flipped onto its cluster's GHE forge — restoring the host its provision request records",
		"hive", payload.HiveID,
		"org", h.Org,
		"owner", h.Owner,
		"github_host_before", before,
		"github_host_after", publicForgeHost,
		"request_github_host", reqHost,
		"spoke_app_id", payload.GitHubAppID,
		"spoke_app_state", strings.TrimSpace(payload.GitHubAppState),
	)
	return true
}

// spokeAppAuthFailing reports whether a spoke POSITIVELY reports that its
// GitHub App auth is not working: the app-required banner is raised and its own
// classification names a credential/installation fault. Silence — an old spoke
// reporting neither field — is never read as failure.
func spokeAppAuthFailing(payload *HeartbeatPayload) bool {
	if payload == nil || !payload.GitHubAppRequired {
		return false
	}
	switch strings.TrimSpace(payload.GitHubAppState) {
	case spokeAppStateNotInstalled, spokeAppStateWrongInstallation, "key-invalid", "key-missing":
		return true
	}
	return false
}

// reconcileGitHubHostFromSpoke BACKFILLS an EMPTY persisted github_host from
// the forge a demonstrably WORKING spoke reports it is running against. It
// reports whether it changed anything.
//
// THE RECORDED HOST IS THE AUTHORITY; A SPOKE'S REPORT IS DOWNSTREAM OF IT.
// A spoke's github block is whatever the hub last delivered (plus whatever a
// mis-delivery left behind), so a spoke report can NEVER be treated as
// independent evidence against an explicitly recorded host — adopting it just
// launders the hub's own mis-delivery back into the record. That laundering is
// exactly the feedback loop observed live on 2026-08-05
// (hosted-kalantar-msb-soft-reflect-jsro, threatened on all 9): an operator
// restored meta github_host to github.com and the spoke to the public App;
// this reconcile then re-adopted github.ibm.com from a freshly-booted spoke
// still carrying the mis-delivered GHE identity — a just-booted spoke has not
// FAILED yet, so the auth-failing stand-down alone did not protect it — and
// with meta back at GHE the wrong-forge repair re-delivered the GHE App with
// an installation reset on the next beat. Every hand-repair reverted within
// minutes; all six restored metas were re-stomped within ~30 minutes.
//
// So this function now writes ONLY into an EMPTY github_host. An explicit
// recorded host that contradicts the spoke is operator/request territory: the
// forge-switch flow (RequestedGitHubHost) and the request-based restore
// (repairMisflippedForgeFromRequest) are the paths allowed to change a
// recorded host, and the wrong-forge delivery repair moves the SPOKE to match
// the record — never the record to match the spoke. The former public→GHE
// correction of an explicitly recorded github.com host (the vllmd-06 hand-edit
// class) is deliberately no longer automatic: it was indistinguishable, from
// the hub's side, from the mis-delivery laundering above.
//
// POSITIVE EVIDENCE ONLY for the empty-host backfill — "not failing yet" is
// not "working" (the fresh-boot gap above), so adoption requires the spoke's
// whole github block to coherently name the observed forge AND show a live
// installation:
//   - the spoke's reported app_id maps to the SAME forge it reports running
//     (forgeOfSpokeAppID — leftover urls beside another forge's App are a
//     mis-delivery artifact, not a forge);
//   - a NON-ZERO installation_id (a spoke mid-reset/rediscovery is not
//     evidence);
//   - no positively reported auth failure (spokeAppAuthFailing).
//
// The spoke's forge is read base-or-api across the two reported fields
// (GitHubHost from base_url, GitHubAPIURL from api_url) so a GHE spoke with a
// blank base_url — the common state — is still recognised as GHE. A
// blank/unknown report changes nothing, and public is never adopted (an empty
// recorded host already means public github.com for a claimed hive). Every
// change is logged with before/after.
func (s *HubServer) reconcileGitHubHostFromSpoke(payload *HeartbeatPayload) bool {
	if s == nil || payload == nil {
		return false
	}
	// The spoke's observed forge, base-or-api. GitHubHost is the bare base_url
	// host; GitHubAPIURL is the resolved api_url. HostLabel() prefers base, falls
	// back to api — exactly the authority the rest of this package now uses.
	observed := config.GitHubConfig{
		BaseURL: strings.TrimSpace(payload.GitHubHost),
		APIURL:  strings.TrimSpace(payload.GitHubAPIURL),
	}.HostLabel()
	// A spoke that reported neither field resolves to the github.com default; that
	// is "unknown", not a real mismatch. Only a spoke that positively reports a
	// GHE host can trip this reconcile.
	if observed == "" || observed == config.DefaultGitHubBaseURL[len("https://"):] {
		return false
	}
	h := loadSaaSHive(payload.HiveID)
	if h == nil {
		return false
	}
	if h.Status == statusAvailable {
		return false // unclaimed placeholder — assign owns its host
	}
	// An operator-requested forge SWITCH is in flight — RequestedGitHubHost is set
	// and adoptSpokeForge is driving the spoke from its old forge to the new one
	// (see forge.go). During that handshake the spoke transiently reports EITHER
	// host, so an unsolicited repair here would race the switch and could latch a
	// stale value. The switch path owns the host until it completes (it clears
	// RequestedGitHubHost when ForgeDelivered flips), so stand down entirely while
	// it is pending. This is the ONLY writer coordination this reconcile needs; a
	// hive with no switch requested is exactly the drift case it exists to fix.
	if h.RequestedGitHubHost != "" {
		return false
	}
	// An explicit public pin is a DECISION, not a fault: leave it alone even if the
	// spoke momentarily reports a GHE host (its cluster default leaking through).
	if h.GitHubBaseURL == githubHostPublic || h.GitHubBaseURL == "https://github.com" {
		return false
	}
	// AN EXPLICIT RECORDED HOST IS NEVER OVERWRITTEN FROM A SPOKE REPORT.
	// The record (assign/approve, operator forge switch, request-based restore)
	// is the authority; the spoke's github block is downstream of hub delivery,
	// so a contradicting report is at best a mis-delivery echoing back — the
	// 2026-08-05 loop that re-stomped six hand-restored metas within ~30
	// minutes. If the record and the spoke disagree, the wrong-forge delivery
	// repair moves the SPOKE onto the recorded forge; nothing here moves the
	// record onto the spoke's.
	if h.GitHubHost != "" {
		return false
	}
	// EMPTY-host backfill, on positive working evidence only. "Has not failed"
	// is not "working": a freshly-booted mis-delivered spoke reports the wrong
	// forge's urls before its first token mint fails, so absence of a failure
	// report proves nothing (the fresh-boot gap in the old stand-down).
	//
	// A BROKEN spoke's forge report is not evidence of intent. A spoke whose App
	// auth is failing may be running a forge it was mis-delivered (the
	// 2026-08-05 flip left 9 spokes positively reporting github.ibm.com they
	// could not authenticate against); adopting that report would launder the
	// mis-delivery into the hive's record and fight the restore that
	// repairMisflippedForgeFromRequest performs.
	if spokeAppAuthFailing(payload) {
		return false
	}
	// The whole github block must coherently name the observed forge: an App of
	// that forge, with a live (non-zero) installation. A spoke carrying GHE urls
	// beside the public App — or beside a freshly-reset installation_id 0 — is a
	// mis-delivery artifact or a repair in flight, never a forge election.
	if payload.GitHubInstallationID == 0 {
		return false
	}
	spokeAppForge := s.forgeOfSpokeAppID(payload.GitHubAppID)
	if spokeAppForge == "" || !sameGitHubHost(spokeAppForge, observed) {
		return false
	}
	before := h.GitHubHost
	h.GitHubHost = observed
	if err := saveSaaSHive(h); err != nil {
		s.logger.Error("github host reconcile: failed to save hive",
			"hive", payload.HiveID, "error", err)
		return false
	}
	s.logger.Info("github host reconcile: backfilled empty github_host from a working spoke's coherent forge report",
		"hive", payload.HiveID,
		"org", h.Org,
		"github_host_before", before,
		"github_host_after", observed,
		"github_api_url_reported", strings.TrimSpace(payload.GitHubAPIURL),
		"spoke_app_id", payload.GitHubAppID,
		"spoke_installation_id", payload.GitHubInstallationID,
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
// handoff) treats an empty VanityURL as "no validated host, keep
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
// Kicked from each heartbeat via kickVanityURLRepairAsync — in the BACKGROUND,
// never on the request path: the kubectl calls below can hang for minutes
// against an unreachable cluster, and a heartbeat response delayed past the
// spoke's 10s client timeout is a response the spoke never sees (which is how
// claim delivery to vllm-d silently broke). The guards below still return
// before any network call or write in the overwhelmingly common no-op case.
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
	// Re-load before writing. makeVanityHostServable (and existingVanityHost
	// above) can block for minutes in kubectl against a slow/unreachable
	// cluster, and the heartbeat path keeps load-modify-writing this hive's
	// record meanwhile — adoptSpokeProjectConfig latching ClaimDelivered /
	// ACMMDelivered, the forge handshakes. Saving through the record loaded
	// before that window would silently revert every field written during it.
	h = loadSaaSHive(hiveID)
	if h == nil {
		return false
	}
	if h.VanityURL != "" {
		return false // another path minted one while we were blocked — keep it
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

// kickVanityURLRepairAsync runs repairVanityURLForHive in the background, at
// most one attempt per hive at a time, registered with provisionWG so tests can
// drain it before swapping the package-level saas*Dir vars.
//
// It exists because the repair is only cheap on its NO-OP paths. When it
// actually has work to do it shells out to kubectl against the hive's cluster
// (existingVanityHost, then addVanityHostToIngress), and against a cluster the
// hub cannot reach — vllm-d is heartbeat-only — those calls hang until their
// TCP dials time out: ~90 seconds per attempt observed live. Called
// synchronously from handleHeartbeat, that stall pushed every heartbeat
// response past the spoke's 10s client timeout (heartbeatTimeout), so the
// claimed-project config the response carried never reached the spoke and
// fresh assignments on heartbeat-only clusters could never complete (the
// vllmd-14 / vllmd-13 / EPM wedge).
//
// The same cheap guards repairVanityURLForHive starts with run inline here, so
// the overwhelmingly common no-op case (already has a vanity URL, unclaimed
// placeholder, incomplete claim, no cluster domain) spawns no goroutine at all.
func (s *HubServer) kickVanityURLRepairAsync(hiveID string) {
	h := loadSaaSHive(hiveID)
	if h == nil || h.Status == statusAvailable || h.VanityURL != "" ||
		h.Org == "" || h.PrimaryRepo == "" {
		return
	}
	if cluster := s.clusterForHive(h); cluster == nil || cluster.Domain == "" {
		return
	}
	// One in-flight attempt per hive: beats arrive every ~2 min while a failing
	// attempt can take minutes, and stacking attempts would pile goroutines up
	// against the same dead cluster for no benefit.
	if _, inFlight := s.vanityRepairInFlight.LoadOrStore(hiveID, struct{}{}); inFlight {
		return
	}
	provisionWG.Add(1)
	go func() {
		defer provisionWG.Done()
		defer s.vanityRepairInFlight.Delete(hiveID)
		s.repairVanityURLForHive(hiveID)
	}()
}

// kickClaimClusterWorkAsync runs the cluster-facing side effects of a claim —
// the namespace identity stamp and the vanity-host mint — in the BACKGROUND,
// at most one attempt per hive at a time, registered with provisionWG so tests
// can drain it before swapping the package-level saas*Dir vars (the same
// contract every other background provision goroutine in this package follows).
//
// It exists for exactly the reason kickVanityURLRepairAsync does (#2730):
// handleAssignHive and handleApproveProvision used to run this work
// SYNCHRONOUSLY between persisting the assignment and writing the HTTP
// response. Every call shells out to kubectl against the hive's cluster, and
// against a cluster the hub cannot reach — vllm-d is heartbeat-only — those
// calls hang until their TCP dials time out (~45s each, ~90s observed for the
// vanity mint alone), so the admin's assign dialog sat on a pending fetch for
// about a minute. Nothing here is needed for the response semantics: the claim
// itself is persisted before the kick and delivered to the spoke over the
// heartbeat channel (projectConfigForHiveID), the namespace stamp is
// best-effort/cosmetic by contract, and a failed vanity mint already has a
// retry path (the heartbeat-kicked repair above) plus a Warn log.
//
// Callers must kick AFTER the assignment is fully persisted: the goroutine
// re-loads meta.json rather than closing over the handler's copy, so it never
// writes through a record that predates concurrent heartbeat updates.
func (s *HubServer) kickClaimClusterWorkAsync(hiveID string) {
	// One in-flight claim work-set per hive, so an admin double-submitting the
	// dialog (or re-assigning while a slow attempt is stuck against a dead
	// cluster) does not stack goroutines against the same unreachable cluster.
	if _, inFlight := s.claimWorkInFlight.LoadOrStore(hiveID, struct{}{}); inFlight {
		return
	}
	provisionWG.Add(1)
	go func() {
		defer provisionWG.Done()
		defer s.claimWorkInFlight.Delete(hiveID)
		h := loadSaaSHive(hiveID)
		if h == nil {
			return
		}
		// Entry points 2/3 for namespace identity (see
		// hosted_namespace_identity.go): the claim/assign moment is when the
		// hive's real owner/org become known, so the namespace labels must be
		// (re)written now. Best-effort and bounded, but the bound is still
		// 2×15s of kubectl against an unreachable cluster — background-only.
		stampHostedNamespaceIdentity(s.clusterForHive(h), hostedNamespaceForHive(h), h.ProjectName, h.Org, h.ID, s.logger)
		// Mint the vanity host under the SAME per-hive guard the heartbeat
		// repair uses, so a beat-kicked repair and this claim-time mint never
		// run concurrently against the same hive (concurrent minters would
		// churn routes and race the adopt write).
		if _, inFlight := s.vanityRepairInFlight.LoadOrStore(hiveID, struct{}{}); inFlight {
			return // a repair attempt is already minting — let it win
		}
		defer s.vanityRepairInFlight.Delete(hiveID)
		s.mintClaimVanityURL(hiveID)
	}()
}

// mintClaimVanityURL is the vanity-minting half of a claim/(re)assignment —
// the work handleAssignHive used to do inline before writing its response.
// Behavior is unchanged from the synchronous version: prefer a name-bearing
// host built from the hive's display name (Option B — see
// hosted_namespace_identity.go), fall back to the org/repo-derived host, make
// it servable through the same seam as the retroactive repair, and only adopt
// a host that IS servable (the vllm-d 503 rule). On failure VanityURL stays
// empty, every read path keeps the working placeholder host, and the
// heartbeat-kicked repair retries later — exactly as before.
//
// The one addition over the inline version is the RE-LOAD before the adopt
// write: this now runs after the handler has already responded, so the slow
// kubectl window overlaps live heartbeat writes (ClaimDelivered/ACMMDelivered
// latches, forge handshakes) and saving through a pre-window record would
// silently revert them — the same hazard repairVanityURLForHive documents.
func (s *HubServer) mintClaimVanityURL(hiveID string) {
	h := loadSaaSHive(hiveID)
	if h == nil || h.Status == statusAvailable || h.VanityURL != "" {
		return // unclaimed, gone, or already has one (stable-host idempotency)
	}
	cluster := s.clusterForHive(h)
	if cluster == nil || cluster.Domain == "" {
		return // nothing to derive a host from
	}
	vanityHost := hiveNameVanityHost(h.ProjectName, cluster.Domain)
	if vanityHost == "" {
		vanityHost = generateHiveID(h.Org, h.PrimaryRepo) + "." + cluster.Domain
	}
	if err := s.makeVanityHostServable(hiveID, vanityHost, cluster); err != nil {
		// Do NOT adopt a host we could not make servable — leaving VanityURL
		// empty makes every read path fall back to the working placeholder
		// host, and the heartbeat repair re-attempts adoption once a route
		// exists (the cluster-wildcard/vllm-d case included).
		s.logger.Warn("vanity host is not served: could not add it to the spoke ingress/route — "+
			"keeping the working placeholder host; the heartbeat repair will retry",
			"hive", hiveID, "host", vanityHost, "cluster", cluster.ID, "error", err)
		return
	}
	h = loadSaaSHive(hiveID)
	if h == nil {
		return
	}
	if h.VanityURL != "" {
		return // another path minted one while we were in kubectl — keep it
	}
	h.VanityURL = "https://" + vanityHost
	if err := saveSaaSHive(h); err != nil {
		s.logger.Error("claim vanity mint: failed to save hive", "hive", hiveID, "error", err)
	}
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
	if cluster != nil && cluster.PullOnly {
		// The spoke's ingress lives on a cluster the hub cannot touch, so the
		// host cannot be added here. Returning an explicit error keeps the
		// caller's "vanity host is not served" reporting truthful instead of
		// letting kubectl aim at the hub's own cluster and report a missing
		// ingress for a namespace that was never there.
		return fmt.Errorf("cluster %s is pull-only: the hub cannot add %s to the spoke's ingress", cluster.ID, vanityHost)
	}
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

// removeHiveRecord durably purges the hub-side, on-disk record for a deleted
// hive: the per-hive directory under saasHivesDir/<id>/ (meta.json + manifests +
// requests.json) and the hive's timeline file under timelineDir/<id>.json.
//
// This is the counterpart to saveSaaSHive and closes the "resurrection" bug:
// deprovisionHive removes the hive directory only when a cluster config is
// available, and NOTHING removed the timeline file. A delete that could not run
// deprovision (no cluster config) therefore left the hive/<id>/ directory —
// with status:"available" — on disk, so the hive reappeared in the
// unassigned/available listing forever, pointing at a namespace that no longer
// existed. Removing the record here makes the delete stick across a hub restart.
//
// Best-effort by design: a delete must never be blocked by a leftover file, and
// the registry entry is the row the user actually sees. Failures are logged, not
// returned. The path-traversal guard mirrors loadSaaSHive/saveSaaSHive so a
// hostile id can never escape the record dirs.
func removeHiveRecord(id string, logger *slog.Logger) {
	if strings.Contains(id, "..") || strings.Contains(id, "/") || strings.Contains(id, "\\") {
		logger.Warn("removeHiveRecord: refusing unsafe hive id", "hive_id", id)
		return
	}
	hiveDir := filepath.Join(saasHivesDir, id)
	if err := os.RemoveAll(hiveDir); err != nil {
		logger.Warn("removeHiveRecord: failed to remove hive directory", "path", hiveDir, "error", err)
	}
	// timelinePath applies its own traversal guard and returns "" if the id is
	// not a safe filename; skip the removal in that case.
	if tp := timelinePath(id); tp != "" {
		if err := os.Remove(tp); err != nil && !os.IsNotExist(err) {
			logger.Warn("removeHiveRecord: failed to remove timeline file", "path", tp, "error", err)
		}
	}
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
// The owner always comes first with role "owner". Every OTHER SaaS user the hub
// granted this hive is appended with the exact validated role stored in the hub
// user record, so direct-route spokes enforce the same read/read-write/merger
// tiers as hub-proxied spokes. Usernames are sanitized to guard the env value,
// and the owner is de-duplicated so they appear exactly once.
func authorizedUsersForHive(h *SaaSHive) string {
	entries := make([]string, 0, 4)
	seen := map[string]bool{}
	owner := sanitize(h.Owner)
	if owner != "" {
		entries = append(entries, owner+":"+saasRoleOwner)
		seen[strings.ToLower(owner)] = true
	}
	for _, u := range listAllSaaSUsers() {
		role, ok := u.Hives[h.ID]
		if !ok {
			continue
		}
		if !config.ValidRole(role) {
			role = saasRoleRead
		}
		name := sanitize(u.GitHubUsername)
		if name == "" || seen[strings.ToLower(name)] {
			continue
		}
		entries = append(entries, name+":"+role)
		seen[strings.ToLower(name)] = true
	}
	return strings.Join(entries, ",")
}

// provisionAppKey is one additional App key rendered into the provisioning
// Secret: the app_id that names its file and the PEM body, pre-indented for the
// YAML block scalar it is written into.
type provisionAppKey struct {
	AppID int64
	PEM   string
}

// provisionAdditionalAppKeys turns the fleet's key set into the ADDITIONAL keys a
// newly provisioned spoke should hold — every fleet key except the primary the
// spoke is already getting as gh-app-key.pem.
//
// This is the provisioning-time twin of attachMissingAppKeys: the heartbeat gets
// a spoke to the both-keys state eventually, this gets it there at birth, so a
// forge switch never has to wait a beat for the target forge's key to arrive.
//
// Excluding primaryAppID is what keeps key selection identity-safe. A hive's
// GitHub identity is the atomic set (app_id, app_slug, api_url, base_url); the
// key it signs with must match the app_id it claims. Writing the primary only
// under its canonical gh-app-key.pem name — never also as a per-app-id file —
// means there is exactly one copy of it and no chance of two diverging.
//
// Results are sorted by app_id so the rendered manifest is deterministic: an
// unstable order would make every reconcile look like a change.
func provisionAdditionalAppKeys(fleetKeys map[int64]fleetAppKey, primaryAppID string) []provisionAppKey {
	if len(fleetKeys) == 0 {
		return nil
	}
	var primary int64
	if v, err := strconv.ParseInt(strings.TrimSpace(primaryAppID), 10, 64); err == nil {
		primary = v
	}
	ids := make([]int64, 0, len(fleetKeys))
	for id := range fleetKeys {
		if id == primary {
			continue // already delivered as gh-app-key.pem
		}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	out := make([]provisionAppKey, 0, len(ids))
	for _, id := range ids {
		pem := strings.TrimSpace(fleetKeys[id].PrivateKey)
		if !strings.HasPrefix(pem, "-----BEGIN") {
			// Never render a truncated or garbage key: it would shadow a good one.
			continue
		}
		lines := strings.Split(pem, "\n")
		for i := range lines {
			lines[i] = "    " + strings.TrimSpace(lines[i])
		}
		out = append(out, provisionAppKey{AppID: id, PEM: strings.Join(lines, "\n")})
	}
	return out
}

// fleetKeys carries every GitHub App private key the fleet knows, keyed by
// app_id, so a freshly provisioned spoke starts life holding ALL of them rather
// than waiting for the heartbeat's additional-key delivery to catch up. Pass nil
// to provision with only the primary key (the pre-existing behaviour).
func provisionHive(h *SaaSHive, req *CreateHiveRequest, cluster *ClusterConfig, fleetKeys map[int64]fleetAppKey, logger *slog.Logger) error {
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
		// AdditionalAppKeys delivers the fleet's OTHER App keys at provision time,
		// each in its own per-app-id Secret entry. Naming is by APP ID, not by
		// cluster: a key's identity is the App it belongs to, and the spoke selects
		// by matching its configured app_id (resolveAppKeyFile). Per-cluster naming
		// would be a lie here — the same PEM is valid on every cluster whose hives
		// authenticate as that App, and a spoke has no way to know which cluster a
		// file named for one was meant to serve. The primary key is excluded: it is
		// already written as gh-app-key.pem above, and duplicating it would let the
		// two copies drift.
		"AdditionalAppKeys": provisionAdditionalAppKeys(
			fleetKeys, resolveProvisionAppID(req.AppID, h, cluster)),
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

	// Entry point 1/3 for namespace identity: stamp what's known at creation
	// time. For a freshly-created pool placeholder this is often just the
	// hive_id (h.Owner/h.Org are still the admin/placeholder values) — the
	// claim (handleApproveProvision) and assign (handleAssignHive) paths
	// overwrite these once the real owner/org are known. See
	// hosted_namespace_identity.go for why the namespace itself is never
	// renamed and must instead be kept self-describing via labels.
	stampHostedNamespaceIdentity(cluster, data["Namespace"].(string), h.ProjectName, h.Org, h.ID, logger)

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
{{- range .AdditionalAppKeys}}
  gh-app-key-{{.AppID}}.pem: |
{{.PEM}}
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
        # SEED-ONLY variant (phase 3 of the layer collapse). The ConfigMap is
        # copied ONLY when the PVC carries no runtime config — i.e. first boot.
        #
        # It used to copy unconditionally on every boot, which meant the frozen
        # seed overwrote the config path before the entrypoint had a say. Phase
        # 2 made the entrypoint prefer the runtime config, so that copy became
        # redundant work that still had to be undone one line later; skipping it
        # is what actually makes the ConfigMap a seed rather than a per-boot
        # input.
        #
        # Both runtime names are probed: ~16 of 50 live spokes still carry only
        # the legacy hive.yaml.bak, so keying on the new name alone would treat
        # them as first-boot and re-seed a hive that has real config on its PVC.
        command: ["sh", "-c", "if [ -s /data/hive.yaml.runtime ]; then echo runtime-config-exists-for-recovery; elif [ -s /data/hive.yaml.bak ]; then echo legacy-runtime-config-exists-for-recovery; else cp /etc/hive-seed/hive.yaml /etc/hive/hive.yaml && echo configmap-copied-first-boot; fi"]
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
        # POD_NAMESPACE via the downward API — the spoke's OWN authoritative
        # source for the namespace it runs in. Provisioning already computes
        # this namespace (see .Namespace above) to create it, but the pod
        # cannot read the template's own values at runtime, and re-deriving
        # "hive-hosted-"+HIVE_ID client-side would silently break the day
        # that convention changes. The downward API can never disagree with
        # reality: it is the API server's own answer, not a guess.
        - name: POD_NAMESPACE
          valueFrom:
            fieldRef:
              fieldPath: metadata.namespace
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
