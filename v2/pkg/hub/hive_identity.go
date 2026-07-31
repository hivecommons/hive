package hub

import (
	"sort"
	"strconv"
	"strings"
)

// ============================================================================
// THE HIVE IDENTITY RESOLVER — ONE ANSWER TO ONE QUESTION
// ============================================================================
//
// "What GitHub App identity does this hive have?" had FOUR independent answers
// in this package, and they disagreed:
//
//	resolveProvisionAppID   (saas_provision.go) — provisioning.  CORRECT: honours
//	                                              the hive's public pin over the
//	                                              cluster default.
//	appIdentityForCluster   (cluster_app_key.go) — the heartbeat answer.  WRONG:
//	                                              keys on cluster ID alone.
//	loadAppPrivateKey       (webhook.go)         — the key lookup.     WRONG:
//	                                              receives the hive and never
//	                                              reads hive.GitHubHost.
//	forgeIdentityForTarget  (forge.go)           — the forge endpoint.
//
// Four answers to one question is the bug class. On 2026-07-31 a clusters.json
// edit stamped the GHE App onto public-GitHub hives on the vllm-d cluster; a
// hand-applied repair to torch-spyre was overwritten six seconds later:
//
//	16:44:11  using GitHub App authentication   app_id=3568013   (correct)
//	16:44:17  hub delivered github app config   app_id=5686      (clobbered)
//
// The hive's own recorded intent — meta.json "github_host": "github.com" — was
// structurally unreachable from the path that decided its identity.
//
// # THE SPOKE PULLS; THE HUB ANSWERS
//
// The spoke initiates the heartbeat and converges on the response. The hub has
// no network path to the vllm-d API server at all, so there is no "push" to
// fix. A spoke-side repair therefore cannot hold — the hub must answer
// correctly, and every spoke then converges on its own next beat with no
// per-spoke editing. That is what makes this one change repair the whole set.
//
// # WHY THIS KEYS ON app_id
//
// Every rule here is expressed in terms of app_id and the hive's recorded host.
// It deliberately does NOT key on api_url/base_url/app_slug string markers:
// those are EMPTY on ~41 of ~50 fleet spokes (the healthy console hive on
// hive-oke runs all three empty), so a marker-based rule is unreachable in
// production while passing happily against hand-built fixtures. app_id is
// always populated.

// HiveIdentity is the complete App identity the hub believes a hive should
// have. It is produced only by ResolveHiveIdentity, so no caller has to
// re-derive any part of it.
//
// The four fields move together. Callers that need only one still take the
// whole set, because a half-identity — the right app_id beside the wrong slug,
// or an App ID from one forge beside another forge's URLs — is exactly the
// state that caused the incident.
type HiveIdentity struct {
	// AppID is the GitHub App to authenticate as. Never zero on a resolved
	// identity unless the cluster names no App at all.
	AppID int64
	// AppSlug is the App's URL slug, which the "Install GitHub App" link is
	// built from. An empty slug falls back to the public default and 404s on a
	// GHE host, so it travels with the ID.
	AppSlug string
	// APIURL/BaseURL are the forge endpoints. Empty means public github.com —
	// the fleet-wide on-disk convention, which ResolvedAPIURL/ResolvedBaseURL
	// already map to api.github.com/github.com. They are left empty rather than
	// spelled out so a resolved identity is byte-identical to what every healthy
	// public spoke already carries.
	APIURL  string
	BaseURL string
	// Forge is the bare host label this identity belongs to ("github.com",
	// "github.ibm.com"). It is the anchor the other fields are consistent with.
	Forge string
	// FromHiveIntent records that the hive's OWN recorded host decided this,
	// rather than the cluster default. Purely diagnostic — it is what makes the
	// six-second-overwrite class of bug legible in logs instead of silent.
	FromHiveIntent bool
}

// ResolveHiveIdentity is the ONE answer to "what identity does this hive have".
//
// Precedence, generalising the rule resolveProvisionAppID already implements:
//
//  1. THE HIVE'S OWN INTENT WINS. A hive that records a GitHub host — meta.json
//     github_host, or the "public" pin in github_base_url — gets that forge's
//     App, even when its cluster defaults to the other forge. This is the fix
//     for torch-spyre (hub says public, cluster stamps GHE) and, in the other
//     direction, for vllmd-13 (hub says GHE, spoke resolved public).
//  2. THE CLUSTER IS A DEFAULT, NOT A PIN. A hive that records nothing inherits
//     its cluster's forge — unchanged behaviour, which matters because 15 of 50
//     hub records carry no github_host at all (hosted-available-vllmd-01 in org
//     "katamari" is the documented case, see backfillGitHubHostFromCluster).
//  3. NEVER INVENT AN APP. If the elected forge has no App the hub can name,
//     this falls back to the cluster's own identity rather than fabricating
//     one. Answering with an App that does not exist on the target forge is
//     precisely the 404-Integration-not-found failure being fixed.
//
// A nil cluster yields the zero identity: the hub knows nothing and must say
// nothing, rather than guessing a default that could aim a GHE App at a public
// hive.
func ResolveHiveIdentity(h *SaaSHive, cluster *ClusterConfig) HiveIdentity {
	if cluster == nil {
		return HiveIdentity{}
	}

	clusterForge := clusterForgeHost(cluster)
	// Start from the cluster default — rule 2.
	id := HiveIdentity{
		AppID:   cluster.GitHubAppID,
		AppSlug: strings.TrimSpace(cluster.GitHubAppSlug),
		APIURL:  cluster.GitHubAPIURL,
		BaseURL: cluster.GitHubBaseURL,
		Forge:   clusterForge,
	}

	elected := electedForgeForHive(h, cluster)
	if elected == "" || sameGitHubHost(elected, clusterForge) {
		return id
	}

	// Rule 1: the hive elected a forge its cluster does not default to.
	elect := HiveIdentity{Forge: elected, FromHiveIntent: true}
	if isPublicForgeHost(elected) {
		// Public github.com: empty URLs, matching every healthy public spoke.
		elect.APIURL, elect.BaseURL = "", ""
	} else {
		elect.BaseURL = "https://" + elected
		elect.APIURL = "https://" + elected + gheAPIPathSuffix
	}

	// Rule 3: only adopt the election if the hub can actually NAME an App on it.
	if app, ok := clusterAppForForge(cluster, elected); ok {
		elect.AppID, elect.AppSlug = app.AppID, strings.TrimSpace(app.AppSlug)
		return elect
	}
	// The cluster names no App on the elected forge. Keep the hive's forge —
	// its recorded intent is still the truth about where its repos live — but
	// carry no App rather than the other forge's, which would 404.
	return elect
}

// ResolveHiveIdentityInFleet is ResolveHiveIdentity with a fleet-wide fallback
// for the elected forge's App.
//
// A cluster entry has ONE identity slot today, so vllm-d — a GHE-default
// cluster — names no public App at all. ResolveHiveIdentity therefore returns
// forge=github.com with AppID=0 for torch-spyre: correct, and not yet useful,
// because a repair needs an actual App to authenticate as.
//
// The fleet already knows that App: hive-oke names it, and appKeysByAppID
// indexes every App the hub holds a key for. A GitHub App ID is a per-forge
// constant, not per-cluster state, so borrowing the public App for a hive that
// elected public is correct by construction — it is the SAME App every other
// public hive uses.
//
// forgeApps maps a forge host to the App registered on it, as assembled by the
// caller from cluster config. Passing it in keeps this function pure and
// table-testable rather than reaching for HubServer state.
func ResolveHiveIdentityInFleet(h *SaaSHive, cluster *ClusterConfig, forgeApps map[string]clusterAppIdentity) HiveIdentity {
	id := ResolveHiveIdentity(h, cluster)
	// The fallback applies ONLY to a hive that actually elected a forge its
	// cluster does not serve. A cluster that simply names no App has no identity
	// to enforce, and borrowing one from another cluster would invent an
	// identity for every hive on it — the opposite of this design's intent.
	if !id.FromHiveIntent || id.AppID != 0 || id.Forge == "" || len(forgeApps) == 0 {
		return id
	}
	for host, app := range forgeApps {
		if sameGitHubHost(forgeHostLabel(host), id.Forge) && app.AppID != 0 {
			id.AppID = app.AppID
			if id.AppSlug == "" {
				id.AppSlug = strings.TrimSpace(app.AppSlug)
			}
			return id
		}
	}
	return id
}

// electedForgeForHive returns the forge a hive has RECORDED for itself, or ""
// when it has recorded nothing.
//
// It reads the same signals the rest of the hub already treats as hive intent,
// in the order of how explicit they are:
//
//	github_host          — the authoritative record, set at assign/forge-switch
//	github_base_url      — including the "public" sentinel, which effective-
//	                       GitHubBaseURL resolves to "" (explicitly public)
//
// Deliberately NOT inferred from app_id: an App ID is an opaque number that
// carries no forge information, and inferring intent from the very field the
// incident corrupted would make the damage self-justifying.
func electedForgeForHive(h *SaaSHive, cluster *ClusterConfig) string {
	if h == nil {
		return ""
	}
	if host := strings.TrimSpace(h.GitHubHost); host != "" {
		return forgeHostLabel(host)
	}
	// No recorded host, but an explicit per-hive base URL still expresses
	// intent — notably the "public" sentinel, which exists precisely to force
	// public github.com on a cluster whose default is GHE.
	if raw := strings.TrimSpace(h.GitHubBaseURL); raw != "" {
		if effectiveGitHubBaseURL(h, cluster) == "" {
			return publicForgeHost
		}
		return forgeHostLabel(raw)
	}
	return ""
}

// clusterForgeHost is the forge a cluster defaults to: the host named by its
// URL fields, else public github.com.
func clusterForgeHost(c *ClusterConfig) string {
	if c == nil {
		return publicForgeHost
	}
	if c.GitHubBaseURL != "" {
		return forgeHostLabel(c.GitHubBaseURL)
	}
	if c.GitHubAPIURL != "" {
		return forgeHostLabel(c.GitHubAPIURL)
	}
	return publicForgeHost
}

// clusterAppForForge returns the App a cluster names on a given forge.
//
// Today a cluster entry has ONE identity slot, so this can only answer for the
// cluster's own forge. That is the honest state of clusters.json and the reason
// a public election on the GHE-default vllm-d cluster still resolves to "no App
// named": the hub genuinely does not know one. Per-forge cluster entries are a
// follow-up; isolating the lookup here means that change lands in one function
// rather than across every caller.
func clusterAppForForge(c *ClusterConfig, forge string) (clusterAppIdentity, bool) {
	if c == nil {
		return clusterAppIdentity{}, false
	}
	// forgesForCluster merges the legacy flat github_app_id/github_app_slug
	// fields (the cluster's own default forge) with the explicit per-forge
	// `forges` map, so ONE lookup covers both an old single-forge entry and a
	// dual-forge one.
	//
	// Reading only the flat fields is what left this unable to answer for a
	// hive that elected a forge its cluster does not default to: on vllm-d — a
	// GHE-default cluster hosting hives that elected github.com — it returned
	// "no App", so the resolver produced app_id 0 and 26 hives stayed broken
	// even with a public App named in `forges`.
	for host, id := range forgesForCluster(c) {
		if !sameGitHubHost(host, forge) {
			continue
		}
		if id.AppID == 0 {
			// A named forge with no App is not an answer — same as absent.
			// Returning it would hand the caller a half-identity.
			return clusterAppIdentity{}, false
		}
		return clusterAppIdentity{
			AppID:   id.AppID,
			AppSlug: strings.TrimSpace(id.AppSlug),
		}, true
	}
	return clusterAppIdentity{}, false
}

// forgeHostLabel normalises any spelling of a forge — bare host, web URL, or
// API URL — to a bare lower-case host.
//
// githubHostLabel alone is not enough: it strips the scheme but keeps the path,
// so an api_url of https://github.ibm.com/api/v3 yields "github.ibm.com/api/v3",
// which compares equal to nothing.
func forgeHostLabel(s string) string {
	h := strings.ToLower(strings.TrimSpace(githubHostLabel(s)))
	h = strings.TrimSuffix(strings.TrimRight(h, "/"), gheAPIPathSuffix)
	h = strings.SplitN(h, "/", 2)[0]
	if h == "" || h == "api.github.com" {
		return publicForgeHost
	}
	return h
}

// isPublicForgeHost reports whether a host label names public github.com.
func isPublicForgeHost(host string) bool {
	return sameGitHubHost(forgeHostLabel(host), publicForgeHost)
}

// AppIDString renders the resolved app_id for the provisioning template, which
// carries it as a string. Zero renders as "" so an unknown App stays unset
// rather than becoming a literal "0" that would fail every auth attempt.
func (i HiveIdentity) AppIDString() string {
	if i.AppID == 0 {
		return ""
	}
	return strconv.FormatInt(i.AppID, 10)
}

// forgeAppsAcrossFleet maps each forge host to the App the hub knows on it,
// assembled from every cluster's config.
//
// This is what lets a hive elect a forge its OWN cluster does not serve and
// still be handed a real App: vllm-d names only the GHE App, but hive-oke names
// the public one, and a github.com App is the same App everywhere.
//
// Deterministic on collision (two clusters naming different Apps on one forge):
// clusters are visited in sorted ID order, mirroring appKeysByAppID.
func (s *HubServer) forgeAppsAcrossFleet() map[string]clusterAppIdentity {
	out := map[string]clusterAppIdentity{}
	if s == nil || s.clusters == nil {
		return out
	}
	ids := make([]string, 0, len(s.clusters))
	for id := range s.clusters {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		c := s.clusters[id]
		if c.GitHubAppID == 0 {
			continue
		}
		forge := clusterForgeHost(&c)
		if _, seen := out[forge]; seen {
			continue // first cluster in sorted order wins
		}
		out[forge] = clusterAppIdentity{
			AppID:   c.GitHubAppID,
			AppSlug: strings.TrimSpace(c.GitHubAppSlug),
		}
	}
	return out
}
