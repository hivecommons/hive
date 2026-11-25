// Package clusterconfig reads the hub's durable cluster configuration.
package clusterconfig

import (
	"encoding/json"
	"errors"
	"log/slog"
	"strings"

	"github.com/hivecommons/hive/pkg/hub/spoke"
)

// DefaultID is the built-in in-cluster target used by legacy hosted hives.
const DefaultID = "hive-oke"

// ErrNoUsableClusters reports a parsed registry with no admitted clusters.
var ErrNoUsableClusters = errors.New("cluster registry has no usable cluster")

// Decode reads and admits the cluster entries the hub can use.
func Decode(data []byte, logger *slog.Logger) (map[string]ClusterConfig, error) {
	var configs []ClusterConfig
	if err := json.Unmarshal(data, &configs); err != nil {
		return nil, err
	}
	clusters := make(map[string]ClusterConfig, len(configs))
	for _, c := range configs {
		if c.ID == "" {
			logger.Warn("skipping cluster config with empty ID")
			continue
		}
		if !c.InCluster && c.KubeconfigPath == "" && !c.PullOnly {
			logger.Warn("skipping remote cluster with no kubeconfig_path",
				"cluster", c.ID,
				"remedy", "set kubeconfig_path, or pull_only: true if the hub cannot reach this cluster and its spokes push heartbeats outbound")
			continue
		}
		if c.Domain == "" {
			logger.Warn("skipping cluster with no domain", "cluster", c.ID)
			continue
		}
		clusters[c.ID] = c
	}
	if len(clusters) == 0 {
		return nil, ErrNoUsableClusters
	}
	return clusters, nil
}

// ClusterConfig describes a Kubernetes cluster that the hub can provision
// hive spokes onto. Each cluster has its own kubeconfig, storage backend,
// ingress style, and optional platform-specific settings (OCI, OpenShift).
type ClusterConfig struct {
	ID             string `json:"id" yaml:"id"`
	Name           string `json:"name" yaml:"name"`
	KubeconfigPath string `json:"kubeconfig_path,omitempty" yaml:"kubeconfig_path,omitempty"`
	Context        string `json:"context" yaml:"context"`
	InCluster      bool   `json:"in_cluster" yaml:"in_cluster"`
	StorageClass   string `json:"storage_class" yaml:"storage_class"`
	StorageType    string `json:"storage_type" yaml:"storage_type"`
	NFSMountIP     string `json:"nfs_mount_ip,omitempty" yaml:"nfs_mount_ip,omitempty"`
	IngressType    string `json:"ingress_type" yaml:"ingress_type"`
	IngressClass   string `json:"ingress_class,omitempty" yaml:"ingress_class,omitempty"`
	CertIssuer     string `json:"cert_issuer,omitempty" yaml:"cert_issuer,omitempty"`
	Domain         string `json:"domain" yaml:"domain"`
	DomainPrefix   string `json:"domain_prefix,omitempty" yaml:"domain_prefix,omitempty"`
	OCICompartment string `json:"oci_compartment,omitempty" yaml:"oci_compartment,omitempty"`
	OCIAvailDomain string `json:"oci_avail_domain,omitempty" yaml:"oci_avail_domain,omitempty"`
	OCIMountTarget string `json:"oci_mount_target,omitempty" yaml:"oci_mount_target,omitempty"`
	OCIExportSet   string `json:"oci_export_set,omitempty" yaml:"oci_export_set,omitempty"`
	RequiresSCC    bool   `json:"requires_scc" yaml:"requires_scc"`
	SCCName        string `json:"scc_name,omitempty" yaml:"scc_name,omitempty"`
	HasGPU         bool   `json:"has_gpu" yaml:"has_gpu"`

	// WildcardTLSSecret names the wildcard certificate this cluster's
	// ingress-nginx controller already serves as its --default-ssl-certificate,
	// in "<namespace>/<secret>" form (e.g. "hive-hub/hive-wildcard-tls"). When
	// set, provisioned spoke Ingresses OMIT their per-host tls: block and the
	// cert-manager issuer annotation for every host the wildcard covers, so one
	// certificate serves the cluster instead of one per hive (#5977).
	//
	// This is an operator ASSERTION about cluster state, not something the hub
	// verifies, and setting it wrongly is not a soft failure: with no
	// --default-ssl-certificate actually configured, ingress-nginx serves its
	// built-in SELF-SIGNED certificate and every spoke dashboard on the cluster
	// fails TLS validation at once. Set it only after BOTH prerequisites hold —
	// the wildcard secret exists on the cluster, and the controller's
	// --default-ssl-certificate names it. Empty (the default) keeps the
	// historical per-host behaviour, which always works.
	//
	// See src/docs/spoke-wildcard-tls.md and wildcard_tls.go.
	WildcardTLSSecret string `json:"wildcard_tls_secret,omitempty" yaml:"wildcard_tls_secret,omitempty"`

	// MaxHives caps how many hosted hives (SaaS records, assigned or pooled)
	// may exist on this cluster. 0 = unlimited. This is the HARD per-cluster
	// gate: the bin-packed capacity estimate (hiveSlotsForNode) is advisory
	// and can be optimistic, but routes/PVCs/namespaces and image-pull
	// bandwidth all scale with hive COUNT, so operators need an explicit
	// ceiling they can set per cluster as the fleet grows.
	MaxHives int `json:"max_hives,omitempty" yaml:"max_hives,omitempty"`
	// PoolMin/PoolTarget drive the watermark pool replenisher
	// (pool_replenisher.go): when clean available placeholders on this
	// cluster drop below pool_min, the hub provisions up to pool_target.
	// pool_target 0 (the default) disables auto-replenish for the cluster;
	// pool_min 0 with a target treats the target itself as the floor.
	PoolMin           int    `json:"pool_min,omitempty" yaml:"pool_min,omitempty"`
	PoolTarget        int    `json:"pool_target,omitempty" yaml:"pool_target,omitempty"`
	Arch              string `json:"arch" yaml:"arch"`
	ImageTag          string `json:"image_tag" yaml:"image_tag"`
	ImagePullPolicy   string `json:"image_pull_policy,omitempty" yaml:"image_pull_policy,omitempty"`
	InferenceEndpoint string `json:"inference_endpoint,omitempty" yaml:"inference_endpoint,omitempty"`
	GitHubBaseURL     string `json:"github_base_url,omitempty" yaml:"github_base_url,omitempty"`
	GitHubAPIURL      string `json:"github_api_url,omitempty" yaml:"github_api_url,omitempty"`
	// UpstreamProxy names a corporate HTTP forward proxy the hives on THIS
	// cluster must chain selected hosts through (e.g. an enterprise GitHub
	// reachable only via the proxy). Injected into new pool slots as
	// HIVE_UPSTREAM_PROXY_URL / HIVE_UPSTREAM_PROXY_HOSTS / HIVE_NO_PROXY.
	UpstreamProxy *ClusterUpstreamProxy `json:"upstream_proxy,omitempty" yaml:"upstream_proxy,omitempty"`
	// HostAliases are Pod hostAliases entries rendered into every hive
	// Deployment on THIS cluster, for GitHub Enterprise hosts reachable only
	// through a TLS reverse proxy pinned to a fixed IP (no CONNECT proxy, so
	// upstream_proxy cannot express it). Validated at load by
	// normalizeClusterHostAliases; existing hives are converged by the
	// host_aliases reconcile sweep (cluster_host_aliases.go, #17801).
	HostAliases []spoke.HostAlias `json:"host_aliases,omitempty" yaml:"host_aliases,omitempty"`
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
	// PullOnly marks a push-reported cluster the hub CANNOT reach: its
	// spokes connect outbound over the heartbeat and nothing here can kubectl
	// into them. The yaml/json key remains pull_only for compatibility, but
	// operator-facing text should describe these clusters as push-reported.
	//
	// WHY THIS FIELD EXISTS. Registration used to require a kubeconfig_path,
	// so a push-reported pool could not be registered AT ALL — the loader dropped
	// the entry ("skipping remote cluster with no kubeconfig_path"). Everything
	// keyed off the cluster registry then silently misfired for its hives:
	// appIdentityForHive returned nil, so spokes sat on the placeholder app_id
	// logging "cluster names no github app — cannot repair", naming a remedy
	// for a cluster entry that could not exist; and clusterForHive fell back
	// to the DEFAULT cluster, so vanity repair ran kubectl against the hub's
	// own cluster looking for a namespace that lives elsewhere.
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
	// the heartbeat-only cluster's entry handed a GHE App to every public-GitHub hive on the cluster,
	// because there was nowhere else for a public identity to live. Those hives
	// had already ELECTED github.com in their meta.json github_host — the hub
	// knew the right answer and had no field in which to act on it.
	//
	// Per the platform owner: "the heartbeat-only cluster is used for GHE, but gh can be elected."
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

// ClusterUpstreamProxy is the per-cluster upstream forward-proxy setting the
// hosted-hive provisioner injects into pool-slot Deployments. Field semantics
// mirror the hive's network.upstream_proxy config block: URL is the proxy
// (userinfo becomes Proxy-Authorization), Hosts restricts chaining to the
// listed patterns (empty = every host not in NoProxy), NoProxy is always
// direct.
type ClusterUpstreamProxy struct {
	URL     string   `json:"url" yaml:"url"`
	Hosts   []string `json:"hosts,omitempty" yaml:"hosts,omitempty"`
	NoProxy []string `json:"no_proxy,omitempty" yaml:"no_proxy,omitempty"`
}

// Enabled reports whether the cluster configures an upstream proxy.
func (c *ClusterUpstreamProxy) Enabled() bool {
	return c != nil && strings.TrimSpace(c.URL) != ""
}

// EnvURL returns the proxy URL for the spoke environment.
func (c *ClusterUpstreamProxy) EnvURL() string {
	if !c.Enabled() {
		return ""
	}
	return strings.TrimSpace(c.URL)
}

// EnvHosts returns the comma-separated upstream proxy host patterns.
func (c *ClusterUpstreamProxy) EnvHosts() string {
	if !c.Enabled() {
		return ""
	}
	return joinPatterns(c.Hosts)
}

// EnvNoProxy returns the comma-separated proxy bypass patterns.
func (c *ClusterUpstreamProxy) EnvNoProxy() string {
	if !c.Enabled() {
		return ""
	}
	return joinPatterns(c.NoProxy)
}

func joinPatterns(in []string) string {
	out := make([]string, 0, len(in))
	for _, p := range in {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, ",")
}
