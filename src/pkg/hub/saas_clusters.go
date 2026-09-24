package hub

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// clusterIDForSaaSHive returns the cluster ID for a SaaS hive, defaulting to
// the default cluster when the field is empty (backward compatibility).
func clusterIDForSaaSHive(sh SaaSHive) string {
	if sh.ClusterID != "" {
		return sh.ClusterID
	}
	return defaultClusterID
}

// ensureClusterIDForClaim stamps a non-blank cluster_id onto a hive that is
// about to be persisted by a claim/assign. This is the data-integrity guard
// for the observed bug where CLAIMED hives lost cluster_id in their meta.json
// and then fell back to defaultClusterID (the hub-reachable cluster) in clusterForHive — which
// mis-routed App/host resolution for hives that actually run on the heartbeat-only cluster.
//
// Precedence, most-trusted first:
//  1. The hive's OWN non-blank ClusterID (the placeholder already belongs to a
//     cluster — always the most authoritative source; never override it).
//  2. poolFallback — the pool the claim was drawn from, when the caller knows
//     it (e.g. handleApproveProvision picks a pool by auth_method), but only
//     when it names a cluster the hub actually has.
//  3. defaultClusterID — last resort, matching clusterForHive's own fallback.
//
// The result is always non-blank, so with omitempty on the json tag it still
// serializes to a concrete "cluster_id" value rather than vanishing.
func (s *HubServer) ensureClusterIDForClaim(h *SaaSHive, poolFallback string) {
	if h.ClusterID != "" {
		return
	}
	if poolFallback != "" {
		if _, ok := s.clusters[poolFallback]; ok {
			h.ClusterID = poolFallback
			return
		}
	}
	h.ClusterID = defaultClusterID
}

// clusterNameForID returns the human-readable name for a cluster ID.
// Returns empty string when the cluster is not found.
func (s *HubServer) clusterNameForID(clusterID string) string {
	if c, ok := s.clusters[clusterID]; ok {
		return c.Name
	}
	return ""
}

// ClusterListEntry is the JSON response for the clusters list endpoint.
type ClusterListEntry struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	HasGPU bool   `json:"has_gpu"`
	Arch   string `json:"arch"`
	// GitHubHost is the bare hostname of the GitHub instance hives on this
	// cluster default to ("github.com" for public GitHub). Shown in the
	// create-hive modal so an admin can see which GitHub a hive will target.
	GitHubHost string `json:"github_host,omitempty"`
	// AppInstallURL is the GitHub App install link for THIS cluster's GitHub
	// host and app slug. A GitHub Enterprise cluster must never be handed a
	// public github.com link: the install request would land on the wrong
	// GitHub and the GHE org admin would never see it.
	AppInstallURL string `json:"app_install_url,omitempty"`
}

// clusterGitHubConfig projects a cluster's GitHub settings onto the
// config.GitHubConfig that owns URL construction, so the hub and the spoke
// build install links from exactly one implementation.
func clusterGitHubConfig(c *ClusterConfig) config.GitHubConfig {
	if c == nil {
		return config.GitHubConfig{}
	}
	base := c.GitHubBaseURL
	// The cluster stores "" for public GitHub; config.GitHubConfig uses the
	// same convention, so pass it through untouched. Carry the api_url too so the
	// derived config's HostLabel()/IsGHE()/AppInstallURL() resolve the forge
	// base-or-api: a GHE cluster that records only an api_url (blank base_url —
	// the common state) is still recognised as GHE, not mislabelled github.com.
	gh := config.GitHubConfig{BaseURL: base, APIURL: c.GitHubAPIURL, AppSlug: c.GitHubAppSlug}
	gh.AppSlug = gh.NormalizedAppSlug()
	return gh
}

// githubHostLabel renders a GitHub base URL as a bare hostname for display.
// Empty (public GitHub) becomes "github.com" rather than an empty chip.
func githubHostLabel(baseURL string) string {
	h := strings.TrimSpace(baseURL)
	if h == "" {
		return "github.com"
	}
	h = strings.TrimPrefix(strings.TrimPrefix(h, "https://"), "http://")
	return strings.TrimRight(h, "/")
}

// GitHubHostLabel is the exported form of githubHostLabel, for the spoke to
// normalize its own configured GitHub base URL before reporting it over the
// heartbeat. Sharing one implementation keeps the value the spoke sends and
// the value the hub renders from drifting into two different spellings.
func GitHubHostLabel(baseURL string) string { return githubHostLabel(baseURL) }

func (s *HubServer) handleListClusters(w http.ResponseWriter, r *http.Request) {
	var entries []ClusterListEntry
	for _, c := range s.clusters {
		gh := clusterGitHubConfig(&c)
		entries = append(entries, ClusterListEntry{
			ID:            c.ID,
			Name:          c.Name,
			HasGPU:        c.HasGPU,
			Arch:          c.Arch,
			GitHubHost:    gh.HostLabel(),
			AppInstallURL: gh.AppInstallURL(),
		})
	}
	// Sort for deterministic API output.
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].ID < entries[j].ID
	})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(entries)
}
