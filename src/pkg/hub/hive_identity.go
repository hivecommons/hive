package hub

import (
	"sort"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
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
// edit stamped the GHE App onto public-GitHub hives on the heartbeat-only cluster; a
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
// no network path to the heartbeat-only cluster's API server at all, so there is no "push" to
// fix. A spoke-side repair therefore cannot hold — the hub must answer
// correctly, and every spoke then converges on its own next beat with no
// per-spoke editing. That is what makes this one change repair the whole set.
//
// # WHY THIS KEYS ON app_id
//
// Every rule here is expressed in terms of app_id and the hive's recorded host.
// It deliberately does NOT key on api_url/base_url/app_slug string markers:
// those are EMPTY on ~41 of ~50 fleet spokes (the healthy console hive on
// the hub-reachable cluster runs all three empty), so a marker-based rule is unreachable in
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
	// APIURL/BaseURL are the forge endpoints, always stated EXPLICITLY — public
	// github.com resolves to https://api.github.com / https://github.com rather
	// than to empty strings.
	//
	// They used to be left empty for public, matching the fleet-wide on-disk
	// convention that ResolvedAPIURL/ResolvedBaseURL already map to those two
	// values. But a field that means something by being ABSENT is what hid the
	// 2026-07-31 incident: a GHE app_id beside an empty api_url read as *unset*
	// rather than *mismatched*. Explicit values cost nothing and remove the
	// ambiguity.
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
	//
	// The App/URLs are read for the cluster's DEFAULT forge through
	// forgesForCluster, NOT from the flat github_app_id/github_* fields directly.
	// The flat fields are only ONE of the two shapes a cluster's identity can take
	// since 2026-07-31: a GHE cluster may instead declare `default_forge` plus a
	// `forges` map and leave the flat fields blank. Reading the flat fields alone
	// then produced app_id 0 / blank urls for a GHE cluster whose real identity
	// sat in the map — the half-identity that dropped the EPM placeholder onto
	// github.com. forgesForCluster merges both shapes, so the default forge's
	// identity is found however it is written; defaultForgeIdentity falls back to
	// the flat fields when the map is absent, keeping legacy clusters unchanged.
	defID := defaultForgeIdentity(cluster, clusterForge)
	id := HiveIdentity{
		AppID:   defID.AppID,
		AppSlug: strings.TrimSpace(defID.AppSlug),
		APIURL:  defID.APIURL,
		BaseURL: defID.BaseURL,
		Forge:   clusterForge,
	}
	// PUBLIC urls are DELIBERATELY left empty here, unlike the elected-forge
	// branch below.
	//
	// A cluster that declares no forge at all resolves clusterForge to public —
	// but so does a GHE cluster that simply has not backfilled its urls, and
	// several real records are in exactly that state. Writing public urls on that
	// silence would stamp api.github.com onto GHE clusters and break the repair
	// path this whole design exists to serve. SILENCE IS NOT EVIDENCE, so
	// defaultForgeIdentity leaves the urls blank whenever clusterForge is public.
	//
	// A GHE default is the opposite of silence: clusterDefaultForge names a GHE
	// host only on POSITIVE evidence (an explicit default_forge or a flat GHE
	// url), so defaultForgeIdentity states the GHE urls with it. That is what
	// stops a GHE default from ever travelling with blank/public urls — the EPM
	// half-identity — while a public/under-specified cluster stays untouched. The
	// same unknown-vs-mismatch rule the push flags follow.
	//
	// Filling the slug IS safe: slugOfAppID keys on app_id, which is always
	// populated, and returns "" for an App this build does not recognise.
	if id.AppSlug == "" {
		id.AppSlug = config.SlugOfAppID(id.AppID)
	}

	elected := electedForgeForHive(h, cluster)
	// FORGE IS PER-HIVE, NOT PER-CLUSTER. A CLAIMED hive that records no host
	// defaults to PUBLIC github.com — the documented meaning of an empty
	// github_host ("empty means public github.com", SaaSHive) — NOT to its
	// cluster's forge. The heartbeat-only cluster hosts BOTH github.ibm.com projects
	// (certus, EPM, …) AND github.com projects (ibm/alchemy-logging: the org
	// "ibm" lives on public github.com). Inheriting the CLUSTER's GHE forge for
	// a claimed hive that never recorded a GHE host is what force-flipped every
	// github.com project on that cluster onto app 5686 / github.ibm.com at
	// 23:56Z on 2026-08-05 and degraded 9 spokes with
	// "404 Not Found on /app/installations/<id>/access_tokens".
	//
	// The cluster default (rule 2) still applies where it is legitimate: an
	// UNCLAIMED placeholder (its pool exists to serve the cluster's forge) and a
	// nil/status-less record (silence about the hive is not evidence about its
	// forge). A claimed hive's forge comes only from its own recorded host —
	// and a genuinely-GHE hive always has one: assign records it from the pasted
	// org URL, backfillGitHubHostFromCluster fills it at claim time, and
	// reconcileGitHubHostFromSpoke backfills a still-empty record from a
	// coherently working spoke's own report.
	if elected == "" && hiveClaimed(h) {
		elected = publicForgeHost
	}
	if elected == "" || sameGitHubHost(elected, clusterForge) {
		return id
	}

	// Rule 1: the hive elected a forge its cluster does not default to.
	elect := HiveIdentity{Forge: elected, FromHiveIntent: true}
	if isPublicForgeHost(elected) {
		// Public github.com: state the URLs EXPLICITLY rather than leaving them
		// empty for a resolver default to fill in later.
		//
		// Empty used to be correct-by-convention here — it matched how every
		// healthy public spoke was stored, and ResolvedAPIURL/ResolvedBaseURL
		// turn "" into exactly these two constants. But an empty field means
		// something by NOT being there, and that is what hid the 2026-07-31
		// incident: a GitHub Enterprise app_id sitting beside an empty api_url
		// read as *unset* rather than *mismatched*, so nothing flagged it until
		// a token call returned 404 Integration not found.
		//
		// These are the SAME values the resolver would have produced; the only
		// change is that the spoke is now told them instead of inferring them.
		// Measured on the fleet before this: base_url was empty on 48 of 51
		// spokes, api_url on 41, app_slug on 33 — implicit state was the normal
		// condition, not a handful of stragglers.
		elect.APIURL, elect.BaseURL = config.DefaultGitHubAPIURL, config.DefaultGitHubBaseURL
	} else {
		elect.BaseURL = "https://" + elected
		elect.APIURL = "https://" + elected + gheAPIPathSuffix
	}

	// Rule 3: only adopt the election if the hub can actually NAME an App on it.
	if app, ok := clusterAppForForge(cluster, elected); ok {
		elect.AppID, elect.AppSlug = app.AppID, strings.TrimSpace(app.AppSlug)
		// A cluster entry may name an App ID and leave the slug blank. Fill it
		// from the App ID rather than shipping an empty field for the spoke's
		// ResolvedAppSlug() to guess at: the slug builds the "Install GitHub
		// App" link, and an empty one falls back to the public default — which
		// is how ten GHE hives ended up with an install link pointing at an App
		// that does not exist on their host.
		//
		// slugOfAppID returns "" for an App ID this build does not recognise,
		// and that stays empty ON PURPOSE. daviddiaz0317-visual-hive runs
		// app_id 4240368 — a third App with its own key — and inventing a slug
		// for it would be a confident wrong answer where no answer is correct.
		if elect.AppSlug == "" {
			elect.AppSlug = config.SlugOfAppID(elect.AppID)
		}
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
// A cluster entry has ONE identity slot today, so the heartbeat-only cluster — a GHE-default
// cluster — names no public App at all. ResolveHiveIdentity therefore returns
// forge=github.com with AppID=0 for torch-spyre: correct, and not yet useful,
// because a repair needs an actual App to authenticate as.
//
// The fleet already knows that App: the hub-reachable cluster names it, and appKeysByAppID
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
		if !sameGitHubHost(forgeHostLabel(host), id.Forge) || app.AppID == 0 {
			continue
		}
		// COHERENCE, not just the map key. forgesForCluster synthesises a
		// cluster's flat github_app_id under that cluster's DEFAULT forge, and an
		// under-specified GHE record — flat app_id, no urls, no default_forge —
		// defaults to PUBLIC, so the fleet map can carry the GHE App keyed under
		// "github.com". Adopting that entry would hand a public-elected hive the
		// very wrong-forge App the repair exists to remove — and, worse, make the
		// wrong-forge delivery gate compare the spoke's broken App against ITSELF
		// (5686 == 5686) and stand down forever, which is exactly how six restored
		// public hives sat undelivered across many heartbeats on 2026-08-05.
		//
		// So an App whose issuing forge is POSITIVELY known (config.ForgeOfAppID)
		// must agree with the elected forge before it is borrowed. An App the
		// build does not recognise (a third forge, self-hosted) is still adopted
		// exactly as before: unknown is never a mismatch.
		if f := config.ForgeOfAppID(app.AppID); f != "" && !sameGitHubHost(f, id.Forge) {
			continue
		}
		id.AppID = app.AppID
		if id.AppSlug == "" {
			id.AppSlug = strings.TrimSpace(app.AppSlug)
		}
		return id
	}
	return id
}

// builtinAppOfForge returns the App this BUILD itself can name on a forge —
// the two compile-time App identities (public github.com and the known GHE
// instance) — or 0 for any other forge.
//
// A GitHub App ID is a per-forge constant: the public App is the SAME App for
// every hive on github.com, so answering with the constant is positive
// knowledge, not invention. This exists for the delivery path, where a repair
// needs an actual App to move a spoke onto even when no cluster entry in
// clusters.json happens to name one on the hive's elected forge.
func builtinAppOfForge(forge string) (int64, string) {
	switch {
	case isPublicForgeHost(forge):
		return config.PublicGitHubAppID, config.PublicGitHubAppSlug
	case sameGitHubHost(forge, forgeHostLabel(config.EnterpriseGitHubBaseURL)):
		return config.EnterpriseGitHubAppID, config.EnterpriseGitHubAppSlug
	}
	return 0, ""
}

// hiveClaimed reports whether a hive record is a CLAIMED hive — one with a real
// owner/org whose forge is a per-hive fact — as opposed to an unclaimed
// placeholder (statusAvailable) whose identity legitimately follows its
// cluster's default until assign records the real host.
//
// An empty Status is treated as NOT claimed: hives claimed by pre-#2333 code
// carry "", and "" is indistinguishable from "unknown", so the conservative
// reading (keep the previous cluster-default behaviour) is the safe one.
func hiveClaimed(h *SaaSHive) bool {
	return h != nil && h.Status != "" && h.Status != statusAvailable
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
	// GitHubHost IS the forge of this hive's repos. It is captured at request
	// time from the org URL the user pastes, and every repo is validated to be
	// on that same host ("single-host-per-spoke"), so it is the one input from
	// which app_id, app_slug, api_url, base_url and the key file all derive.
	if host := strings.TrimSpace(h.GitHubHost); host != "" {
		return forgeHostLabel(host)
	}
	// LEGACY ONLY. Hives assigned before github_host was recorded express the
	// same fact through GitHubBaseURL — including the "public" sentinel, which
	// exists solely because an empty base URL could not distinguish "public" from
	// "unset". Reading it here keeps those hives resolving while they carry no
	// host; normalizeHiveForge writes the derived host back so a hive passes
	// through this branch at most once.
	//
	// Nothing new should ever set GitHubBaseURL on a hive. Two fields encoding
	// one fact is what let a hive's own recorded forge disagree with the URLs
	// validated against it, which refused the identity push for 26 hives.
	if raw := strings.TrimSpace(h.GitHubBaseURL); raw != "" {
		if effectiveGitHubBaseURL(h, cluster) == "" {
			return publicForgeHost
		}
		return forgeHostLabel(raw)
	}
	return ""
}

// normalizeHiveForge collapses a hive's forge onto GitHubHost, the single
// stored input, and reports whether it changed anything.
//
// A hive may arrive expressing its forge three ways: GitHubHost (current),
// GitHubBaseURL holding a real URL (older), or GitHubBaseURL holding the
// "public" sentinel (older still, meaning "force public on a GHE-default
// cluster"). All three say the same thing, and any two of them can drift apart
// — which is exactly what happened on 2026-07-31.
//
// After this runs, GitHubHost carries the answer and the per-hive URL fields
// carry nothing, so there is no second copy left to disagree with.
func normalizeHiveForge(h *SaaSHive, cluster *ClusterConfig) bool {
	if h == nil {
		return false
	}
	elected := electedForgeForHive(h, cluster)
	// Store the elected host VERBATIM when the field does not already hold it.
	//
	// Deliberately NOT `!sameGitHubHost(h.GitHubHost, elected)`: that helper
	// treats "" as equivalent to github.com, so for a public hive it reports
	// "already correct" and the write is skipped — leaving GitHubHost empty
	// while the URL fields below are cleared, which destroys the only record of
	// the forge and drops the hive onto its cluster's default. That is the exact
	// failure this change exists to remove, reintroduced by the normalizer.
	changed := false
	if elected != "" && h.GitHubHost != elected {
		h.GitHubHost = elected
		changed = true
	}
	// Only now drop the per-hive URL overrides: the host is holding the fact, so
	// removing the second copy cannot lose it.
	//
	// The `elected != ""` gate is DEFENSIVE, not load-bearing, and a mutation
	// test proved it: electedForgeForHive returns non-empty for any non-empty
	// GitHubBaseURL, so whenever there is a URL to clear, elected is already
	// set. Removing the gate breaks no test today. It stays because it states
	// the invariant that matters — never erase a hive's only record of its
	// forge, which would drop it onto the cluster default and turn a public
	// hive on the heartbeat-only cluster into a GHE one — and because a future change to
	// electedForgeForHive could make it reachable. Flagged rather than left
	// looking verified.
	if elected != "" {
		if h.GitHubBaseURL != "" {
			h.GitHubBaseURL = ""
			changed = true
		}
		if h.GitHubAPIURL != "" {
			h.GitHubAPIURL = ""
			changed = true
		}
	}
	return changed
}

// clusterForgeHost is the forge a cluster defaults to.
//
// It MUST agree with clusterDefaultForge, which is the forge-aware authority
// (explicit default_forge, then the flat URL fields, then public). Reading only
// the flat github_base_url/github_api_url — as this used to — silently disagreed
// with clusterDefaultForge for a cluster that expresses its GHE-ness through the
// 2026-07-31 `default_forge` + `forges` map shape instead of the flat URL
// fields: clusterDefaultForge said github.ibm.com while this said github.com.
//
// That split is the regression this whole file exists to prevent, reintroduced
// one layer down. A heartbeat-only-cluster entry written as
//
//	{ "default_forge": "github.ibm.com",
//	  "forges": { "github.ibm.com": { "app_id": 5686, "app_slug": "..." } } }
//
// has EMPTY flat github_base_url, so the old form resolved its default forge to
// public github.com. ResolveHiveIdentity then handed every hive with no recorded
// host the PUBLIC identity (app_id 0 / blank urls) on a GHE cluster — the EPM
// placeholder that claimed github.ibm.com yet ran a blank forge against
// github.com. Routing through clusterDefaultForge closes that gap: one function
// decides "what forge does this cluster default to", forge-map shape included.
func clusterForgeHost(c *ClusterConfig) string {
	if c == nil {
		return publicForgeHost
	}
	return clusterDefaultForge(c)
}

// clusterDefaultForgeHasPositiveEvidence reports whether a cluster named its
// default forge explicitly, rather than falling through the empty-record default
// to public github.com. Public-by-silence is not evidence: those records may be
// GHE clusters that have not backfilled URLs yet.
func clusterDefaultForgeHasPositiveEvidence(c *ClusterConfig, forge string) bool {
	if c == nil || strings.TrimSpace(forge) == "" {
		return false
	}
	if f := strings.TrimSpace(c.DefaultForge); f != "" {
		return sameGitHubHost(forgeHostLabel(f), forge)
	}
	if f := strings.TrimSpace(c.GitHubBaseURL); f != "" {
		return sameGitHubHost(forgeHostLabel(f), forge)
	}
	if f := strings.TrimSpace(c.GitHubAPIURL); f != "" {
		return sameGitHubHost(forgeHostLabel(f), forge)
	}
	return false
}

// defaultForgeIdentity returns the App identity (id, slug, urls) a cluster
// declares for its DEFAULT forge, from whichever shape the cluster is written
// in.
//
// forgesForCluster already merges the legacy flat github_app_id/github_app_slug
// fields with the explicit per-forge `forges` map, so this one lookup covers a
// cluster written either way. The URLs are stated explicitly for a GHE default
// even when the cluster left them blank in the map, because the forge host is
// enough to derive them (the same derivation the elected-forge branch uses) —
// and a GHE default whose urls read as blank is precisely the state that let a
// GHE cluster resolve to github.com.
//
// A public default keeps EMPTY urls: that is the correct, coherent public shape
// (ResolvedAPIURL/ResolvedBaseURL map "" to the two public constants), and
// stating public urls on a cluster that only IMPLIED public — by declaring
// nothing — would stamp api.github.com onto GHE clusters that simply have not
// filled their urls in. Silence is not evidence, exactly as ResolveHiveIdentity
// documents for its own cluster-default branch.
func defaultForgeIdentity(c *ClusterConfig, forge string) ClusterForgeIdentity {
	if c == nil {
		return ClusterForgeIdentity{}
	}
	var out ClusterForgeIdentity
	for host, id := range forgesForCluster(c) {
		if sameGitHubHost(host, forge) {
			out = id
			break
		}
	}
	if isPublicForgeHost(forge) {
		return out // public: empty urls are the coherent shape
	}
	// GHE default: state the urls from the forge host when the cluster left them
	// blank, so a GHE default can never travel with public/blank urls.
	if strings.TrimSpace(out.BaseURL) == "" {
		out.BaseURL = "https://" + forge
	}
	if strings.TrimSpace(out.APIURL) == "" {
		out.APIURL = "https://" + forge + gheAPIPathSuffix
	}
	return out
}

// clusterAppForForge returns the App a cluster names on a given forge.
//
// Today a cluster entry has ONE identity slot, so this can only answer for the
// cluster's own forge. That is the honest state of clusters.json and the reason
// a public election on the GHE-default heartbeat-only cluster still resolves to "no App
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
	// hive that elected a forge its cluster does not default to: on the heartbeat-only cluster — a
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
// still be handed a real App: the heartbeat-only cluster names only the GHE App, but the hub-reachable cluster names
// the public one, and a github.com App is the same App everywhere.
//
// Deterministic on collision (two clusters naming different Apps on one forge):
// clusters are visited in sorted ID order, mirroring appKeysByAppID.
//
// It enumerates EVERY forge each cluster serves via forgesForCluster, not just
// the cluster's flat github_app_id. A GHE cluster written in the 2026-07-31
// `forges` map shape carries its App under forges.<host> with a blank flat
// github_app_id, so a flat-only read contributed nothing for it — leaving a
// hive that elected that forge on a DIFFERENT cluster unable to borrow the App,
// with the same app_id-0 blank identity this file exists to prevent.
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
		for host, fid := range forgesForCluster(&c) {
			if fid.AppID == 0 {
				continue
			}
			forge := forgeHostLabel(host)
			if _, seen := out[forge]; seen {
				continue // first cluster in sorted order wins
			}
			out[forge] = clusterAppIdentity{
				AppID:   fid.AppID,
				AppSlug: strings.TrimSpace(fid.AppSlug),
			}
		}
	}
	return out
}
