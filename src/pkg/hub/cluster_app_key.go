package hub

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// Per-cluster GitHub App private-key store.
//
// WHY THIS EXISTS
//
// Every cluster the hub provisions onto talks to its OWN GitHub instance, and
// each of those instances hosts its own GitHub App registration with its own
// app_id and its own private key. Before this store the hub only ever knew a key
// that an operator pasted into a SINGLE hive's provisioning request. A hive
// provisioned without one — or provisioned against the wrong GitHub — had no
// path back to a correct key, and the only working copy of a GitHub Enterprise
// key in the fleet lived on one spoke's PVC. Reprovisioning that spoke would
// have destroyed it.
//
// The store makes the HUB the authority: one key per cluster, from which every
// spoke on that cluster is continuously reconciled over the existing heartbeat
// channel.
//
// WHY THE KEY IS NOT IN clusters.json
//
// clusters.json is read, parsed and rendered along operator-facing paths (the
// cluster list, the create-hive modal, provisioning templates). Putting signing
// material in it would put that material one careless marshal away from an HTTP
// response. The key therefore lives in its own file, referenced only by cluster
// ID, and ClusterConfig carries nothing but the non-secret app_id.

// clusterAppKeyDir is the directory holding one PEM per cluster, named
// <clusterID>.pem. A var (not a const) so tests can redirect it at a temp dir;
// production never reassigns it. It sits beside the hub's other secrets
// (hmac.key, hub-secret.key) on the same PVC, so it survives hub restarts and
// is backed up by whatever backs up /data.
var clusterAppKeyDir = "/data/saas/app-keys"

// File modes for the key store. The directory is owner-only too: the filenames
// alone (cluster IDs) are harmless, but there is no reason for anything else on
// the pod to enumerate or traverse it.
const (
	// clusterAppKeyDirMode is rwx------ — only the hub process may traverse.
	clusterAppKeyDirMode = 0o700
	// clusterAppKeyFileMode is rw------- — the key is readable only by the hub.
	clusterAppKeyFileMode = 0o600
)

// clusterAppKeyPath returns the on-disk path of a cluster's App key.
//
// The cluster ID is sanitized before it reaches the filesystem: IDs arrive from
// clusters.json and from heartbeat payloads, and a value like "../../etc/x"
// would otherwise escape the key directory. Only characters that appear in real
// cluster IDs survive; anything else makes the path invalid and the caller
// treats the cluster as having no key.
func clusterAppKeyPath(clusterID string) (string, bool) {
	id := strings.TrimSpace(clusterID)
	if id == "" {
		return "", false
	}
	for _, r := range id {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.'
		if !ok {
			return "", false
		}
	}
	// Reject "." and ".." outright: both pass the character filter above but
	// name a directory rather than a key file.
	if id == "." || id == ".." {
		return "", false
	}
	return filepath.Join(clusterAppKeyDir, id+".pem"), true
}

// loadClusterAppKey returns the PEM private key stored for a cluster, or "" when
// the cluster has none. A missing key is an ordinary state — most clusters use
// the public GitHub App whose key is supplied per hive at provisioning time — so
// it is never an error and never logged as one.
func loadClusterAppKey(clusterID string) string {
	path, ok := clusterAppKeyPath(clusterID)
	if !ok {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	pem := strings.TrimSpace(string(data))
	if !strings.HasPrefix(pem, "-----BEGIN") {
		// A truncated or garbage file must not be pushed to spokes: it would
		// replace a working key with a broken one across the whole cluster.
		return ""
	}
	return pem
}

// storeClusterAppKey writes a cluster's App private key to the store.
//
// The write is atomic (temp file in the same directory, then rename) so a hub
// restart or a full disk can never leave a half-written PEM that would be
// distributed to every spoke on the cluster. The temp file is created with the
// final restrictive mode from the outset — never world-readable for even the
// instant between create and chmod.
func storeClusterAppKey(clusterID, pemData string) error {
	path, ok := clusterAppKeyPath(clusterID)
	if !ok {
		return fmt.Errorf("invalid cluster id for app key store: %q", clusterID)
	}
	return storeAppKeyAtPath(path, pemData, "cluster "+clusterID)
}

// storeAppKeyAtPath is storeClusterAppKey's body, extracted verbatim so the
// secondary-App store (cluster_secondary_app_key.go) writes through the SAME
// code rather than a copy.
//
// The extraction is deliberate and the copy is the thing being avoided: this
// function is where 0700/0600, the create-with-final-mode, the fsync and the
// atomic rename live. A second store with its own hand-rolled write would be
// one review away from dropping the Sync or creating world-readable and nobody
// would notice until a key leaked. subject names the owner of the key for error
// messages only — it must never be, and never is, the key material.
func storeAppKeyAtPath(path, pemData, subject string) error {
	trimmed := strings.TrimSpace(pemData)
	if !strings.HasPrefix(trimmed, "-----BEGIN") {
		return fmt.Errorf("app key for %s is not PEM", subject)
	}
	// Validate before persisting: a key we cannot fingerprint is a key we could
	// never compare against a spoke's report, so it would be pushed forever.
	if _, err := config.AppKeyFingerprint(trimmed); err != nil {
		return fmt.Errorf("app key for %s is unusable: %w", subject, err)
	}

	if err := os.MkdirAll(clusterAppKeyDir, clusterAppKeyDirMode); err != nil {
		return fmt.Errorf("create app key dir: %w", err)
	}

	tmp, err := os.CreateTemp(clusterAppKeyDir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("create temp app key file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once the rename below succeeds

	if err := tmp.Chmod(clusterAppKeyFileMode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp app key file: %w", err)
	}
	if _, err := tmp.WriteString(trimmed + "\n"); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp app key file: %w", err)
	}
	// Sync before rename so the rename cannot publish an empty file after a
	// crash — the key is distributed fleet-wide, so a partial write is worse
	// than no write.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp app key file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp app key file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename app key into place: %w", err)
	}
	return nil
}

// clusterAppKeyFingerprint returns the non-secret fingerprint of a cluster's
// stored key, or "" when the cluster has no usable key.
func clusterAppKeyFingerprint(clusterID string) string {
	pem := loadClusterAppKey(clusterID)
	if pem == "" {
		return ""
	}
	fp, err := config.AppKeyFingerprint(pem)
	if err != nil {
		return ""
	}
	return fp
}

// clusterAppIdentity is what the hub believes a given cluster's spokes should be
// authenticating as. Assembled from the cluster config (app_id, non-secret) and
// the key store (the key itself, never in any config file).
type clusterAppIdentity struct {
	AppID   int64
	AppSlug string
	// APIURL, BaseURL and Forge describe the forge THIS HIVE resolved to, which
	// is not necessarily its cluster's default.
	//
	// They are carried here because dropping them is what made the hub refuse
	// its own repair: the guard below re-read the URLs from the CLUSTER record,
	// so a hive that elected github.com had its public app_id validated against
	// the cluster's GHE api_url/base_url. That pair IS inconsistent, so
	// RejectIdentitySet correctly refused it — and the push those hives needed
	// was dropped every ~30s with the right answer already computed.
	//
	// Forge distinguishes "public, whose URLs are legitimately empty" from "no
	// forge resolved"; the URLs alone cannot.
	APIURL      string
	BaseURL     string
	Forge       string
	PrivateKey  string
	Fingerprint string
}

// HasKey reports whether this identity carries actual key material, as opposed
// to naming an App whose key the hub has never been given.
//
// Every decision that would REPLACE a spoke's key must gate on this: an
// identity with no key can still authoritatively correct an app_id, but pushing
// its empty PrivateKey would blank a spoke's working key file. The heartbeat
// contract treats an empty private_key as "leave the key alone", so the two
// concerns stay cleanly separable.
func (i *clusterAppIdentity) HasKey() bool {
	return i != nil && i.PrivateKey != "" && i.Fingerprint != ""
}

// appIdentityForCluster resolves the App identity the hub should enforce on a
// cluster. It returns nil when the cluster is unknown or has no configured
// app_id — both of which mean "this hub has nothing authoritative to say about
// this cluster's App credentials, so change nothing".
//
// A CONFIGURED app_id WITH NO STORED KEY IS STILL AN IDENTITY
//
// The key store and the cluster config are populated independently: app_id is
// non-secret and lives in clusters.json, the key is secret and lives in the
// per-cluster PEM store. Requiring BOTH before the hub would say anything meant
// a cluster that named its App but whose key had never been uploaded produced
// nil — and every downstream reconcile silently became a no-op.
//
// That is exactly how the placeholder-sentinel repair shipped in #2333 was
// prevented from ever running on the hub-reachable cluster: the operator added
// "github_app_id" to clusters.json as instructed, but the hub-reachable cluster has no
// <clusterID>.pem (only the heartbeat-only cluster does), so appIdentityForCluster returned nil and
// appKeyConfigForHeartbeat bailed before decideAppKeySync's sentinel branch was
// ever consulted. The repair was unreachable in production for the one cluster
// it was written for.
//
// The two facts are independently useful. Correcting a spoke's app_id needs
// only the non-secret app_id; replacing its KEY needs the PEM. Returning the
// identity with an empty PrivateKey/Fingerprint lets the app_id-only repair
// proceed, while every key-pushing decision still gates on HasKey() so a hub
// holding no key can never blank a spoke's working one.
func (s *HubServer) appIdentityForCluster(clusterID string) *clusterAppIdentity {
	// No hive in hand: the cluster's own default identity. Callers that DO have
	// a hive must use appIdentityForHive so the hive's election is honoured.
	return s.appIdentityForHive(nil, clusterID)
}

// appIdentityForHive resolves the App identity the hub should answer with for a
// specific hive, routing through ResolveHiveIdentity so this path cannot give a
// different answer from provisioning or the forge endpoint.
//
// A nil hive means "no election known" and yields the cluster default, which is
// exactly the previous behaviour of appIdentityForCluster.
func (s *HubServer) appIdentityForHive(h *SaaSHive, clusterID string) *clusterAppIdentity {
	if s == nil || s.clusters == nil {
		return nil
	}
	c, ok := s.clusters[clusterID]
	if !ok {
		// UNREGISTERED CLUSTER. Returning nil here meant a hive whose cluster is
		// missing from clusters.json got no answer at all, beat after beat —
		// the hub logged "cluster names no github app — cannot repair" and
		// offered a remedy (set github_app_id on the cluster) for a cluster
		// entry that does not exist. Five spoke-cluster spokes stayed on
		// config.PlaceholderAppID that way.
		//
		// A hive's ELECTED forge does not depend on the cluster registry, and
		// the two build-constant Apps are per-forge facts, so this is the one
		// answer that is still knowable. It is deliberately narrow: hive
		// intent only (an unclaimed placeholder elects nothing and still gets
		// nil), and only forges this build can name.
		return s.builtinIdentityForForge(electedForgeForHive(h, nil))
	}
	resolved := ResolveHiveIdentityInFleet(h, &c, s.forgeAppsAcrossFleet())
	// A hive that ELECTED a forge no cluster entry names an App on still has a
	// real App when that forge's App is a build constant (public github.com, the
	// known GHE instance). Without this, a public-elected hive on a GHE-only
	// fleet resolved AppID 0, this function returned nil, and the wrong-forge
	// repair could never deliver — the hub held the right answer (github_host
	// restored to github.com) and answered nothing, beat after beat, which is
	// how six restored public hives stayed on the GHE App on 2026-08-05.
	// Cluster-default identities may use the same constants, but only when the
	// cluster stated that default forge with positive evidence. This repairs a
	// GHE-default cluster that omitted github_app_id without treating an empty,
	// under-backfilled cluster record as proof that it is public github.com.
	if resolved.AppID == 0 && resolved.Forge != "" && (resolved.FromHiveIntent || clusterDefaultForgeHasPositiveEvidence(&c, resolved.Forge)) {
		if appID, slug := builtinAppOfForge(resolved.Forge); appID != 0 {
			resolved.AppID = appID
			if resolved.AppSlug == "" {
				resolved.AppSlug = slug
			}
		}
	}
	if resolved.AppID == 0 {
		return nil
	}
	identity := &clusterAppIdentity{
		AppID:   resolved.AppID,
		AppSlug: resolved.AppSlug,
		APIURL:  resolved.APIURL,
		BaseURL: resolved.BaseURL,
		Forge:   resolved.Forge,
	}
	// The KEY must follow the APP, not the cluster. A hive that elected a forge
	// its cluster does not default to needs that App's key; the cluster's key
	// belongs to a different App and would fail auth even with a correct app_id.
	if resolved.FromHiveIntent && resolved.AppID != c.GitHubAppID {
		if k, found := s.appKeysByAppID()[resolved.AppID]; found && k.PrivateKey != "" {
			identity.PrivateKey = k.PrivateKey
			identity.Fingerprint = k.Fingerprint
			if identity.AppSlug == "" {
				identity.AppSlug = k.AppSlug
			}
		}
		return identity
	}
	pem := loadClusterAppKey(clusterID)
	if pem == "" {
		// No key uploaded for this cluster. The app_id alone is still
		// authoritative and still repairs a sentinel spoke.
		return identity
	}
	fp, err := config.AppKeyFingerprint(pem)
	if err != nil {
		return identity
	}
	identity.PrivateKey = pem
	identity.Fingerprint = fp
	return identity
}

// assignTimeAppIdentity resolves the COMPLETE forge identity set an assigning
// hive should be given — app_id, app_slug, base_url and api_url, plus the key
// when this hub holds one.
//
// WHY ASSIGN NEEDS ITS OWN ENTRY POINT
//
// Assign already knows the forge: handleAssignHive records h.GitHubHost from
// the requested org before it reaches the credential step. Yet until now the
// only way a spoke received an App at assign was an admin pasting app_id,
// installation_id AND a PEM into the dialog — all three, or nothing was sent.
// Leave one blank and the hive kept config.PlaceholderAppID, which is how five
// spokes on a spoke cluster sat in dashboard-only mode carrying 999999999 while the
// hub knew their forge was github.ibm.com and held that App's key the whole
// time.
//
// installation_id is deliberately NOT part of the answer. The spoke discovers
// it itself once it can authenticate as the App ("github app installation
// auto-discovered"), so requiring an operator to supply a value the spoke
// derives is what made the automatic path unreachable. A zero installation in
// HeartbeatGitHubAppConfig means "not speaking to this field", so omitting it
// can never blank a working one.
//
// Returns nil when the forge is unknown or names no App this hub can identify —
// "say nothing" rather than guess, so assign never invents an identity.
func (s *HubServer) assignTimeAppIdentity(h *SaaSHive) *HeartbeatGitHubAppConfig {
	if s == nil || h == nil {
		return nil
	}
	identity := s.appIdentityForHive(h, clusterIDForHive(h))
	if identity == nil {
		// The hive's cluster is absent from clusters.json (or names no App on
		// the elected forge), so appIdentityForHive returned before any App
		// logic. The elected forge is still a fact, and the two build-constant
		// Apps are per-forge constants — positive knowledge, not invention.
		identity = s.builtinIdentityForForge(electedForgeForHive(h, s.clusterForHive(h)))
	}
	if identity == nil || identity.AppID == 0 {
		return nil
	}
	cfg := &HeartbeatGitHubAppConfig{
		AppID:   identity.AppID,
		AppSlug: identity.AppSlug,
		APIURL:  identity.APIURL,
		BaseURL: identity.BaseURL,
	}
	// Only ever ride a key we actually hold: an empty private_key means "leave
	// the spoke's key alone", and the spoke prefers its own per-app-id file
	// (/data/gh-app-key-<app_id>.pem) when it already has the right one.
	if identity.HasKey() {
		cfg.PrivateKey = identity.PrivateKey
	}
	return cfg
}

// builtinIdentityForForge builds the identity set for a forge whose App is a
// build constant, filling in the forge URLs that make an app_id usable. An App
// ID presented to the wrong forge returns "404 Integration not found", so the
// URLs travel with it rather than being left for a later repair.
func (s *HubServer) builtinIdentityForForge(forge string) *clusterAppIdentity {
	if s == nil || forge == "" {
		return nil
	}
	appID, slug := builtinAppOfForge(forge)
	if appID == 0 {
		return nil
	}
	identity := &clusterAppIdentity{AppID: appID, AppSlug: slug, Forge: forge}
	// Public github.com is the spoke's default forge, so its URLs stay empty —
	// writing them would pin a value the spoke already assumes. Enterprise must
	// be stated explicitly.
	if !isPublicForgeHost(forge) {
		identity.BaseURL = config.EnterpriseGitHubBaseURL
		identity.APIURL = config.EnterpriseGitHubAPIURL
	}
	if k, found := s.appKeysByAppID()[appID]; found && k.PrivateKey != "" {
		identity.PrivateKey = k.PrivateKey
		identity.Fingerprint = k.Fingerprint
		if identity.AppSlug == "" {
			identity.AppSlug = k.AppSlug
		}
	}
	return identity
}

// fleetAppKey is one App identity the fleet knows, indexed by its own app_id.
// It is assembled the same way appIdentityForCluster assembles a cluster's
// identity — the non-secret app_id/slug from cluster config, the secret key from
// the per-cluster key store — but keyed by APP rather than by cluster, so a
// spoke can be handed the key of an App that is not its own cluster's.
type fleetAppKey struct {
	AppID       int64
	AppSlug     string
	PrivateKey  string
	Fingerprint string
}

// appKeysByAppID returns the fleet's App keys de-duplicated by app_id.
//
// WHY app_id AND NOT clusterID
//
// The store on disk is keyed by cluster (one <clusterID>.pem per cluster), but
// two clusters can front the SAME GitHub host and therefore the same App — and,
// more importantly, a spoke needs to be handed the key for an App that is NOT its
// cluster's default (a github.com hive parked on a GitHub-Enterprise cluster).
// Re-indexing the existing per-cluster keys by their app_id gives exactly that
// lookup without a second on-disk store or any migration: every key already on
// disk simply becomes discoverable by the App it belongs to.
//
// It is read-only over the cluster map and the key files; it invents nothing and
// stores nothing. A cluster with no configured app_id, or no usable key on disk,
// contributes nothing. When two clusters resolve to the same app_id the first
// usable key wins deterministically (clusters are visited in sorted ID order) —
// they are the same App, so any of its valid keys is correct.
func (s *HubServer) appKeysByAppID() map[int64]fleetAppKey {
	out := make(map[int64]fleetAppKey)
	if s == nil || s.clusters == nil {
		return out
	}
	ids := make([]string, 0, len(s.clusters))
	for id := range s.clusters {
		ids = append(ids, id)
	}
	sort.Strings(ids) // deterministic winner when two clusters share an app_id
	for _, id := range ids {
		c := s.clusters[id]
		if c.GitHubAppID == 0 {
			continue
		}
		if _, seen := out[c.GitHubAppID]; seen {
			continue
		}
		pem := loadClusterAppKey(id)
		if pem == "" {
			continue
		}
		fp, err := config.AppKeyFingerprint(pem)
		if err != nil {
			continue
		}
		out[c.GitHubAppID] = fleetAppKey{
			AppID:       c.GitHubAppID,
			AppSlug:     c.GitHubAppSlug,
			PrivateKey:  pem,
			Fingerprint: fp,
		}
	}
	return out
}

// SECURITY (C1/N3, CWE-200/639): the fleet-wide "additional App keys" delivery
// lane was REMOVED. It formerly attached every OTHER fleet App's private key to
// every heartbeat response, selecting them purely from the fleet key set with no
// binding to the authenticated caller. Combined with the heartbeat trusting the
// body-supplied hive_id, that let any hive holding the fleet-shared bearer pull
// every tenant's App private key by beating with any hive_id. See
// additionalAppKeysForSpoke/attachMissingAppKeys in git history.
//
// A heartbeat now delivers ONLY the App identity/key for THAT hive:
//   - its PRIMARY (cluster) key, via appKeySyncForHeartbeat's per-cluster
//     reconcile (idempotent, gated on the hive's own cluster key), and
//   - a targeted, operator/webhook-queued identity, via
//     pendingAppIdentityForHeartbeat.
//
// A hive is never handed a key for an App it is not currently assigned. The
// "github.com hive on a GHE cluster needs the other forge's key pre-positioned
// for a future migration" rationale did not justify broadcasting private key
// material: when a hive is actually re-assigned to a different App, that App's
// key is delivered at that time through the reconcile/pending-identity lanes,
// keyed to the hive that is authoritatively assigned it — not speculatively
// pushed to every spoke. appKeysByAppID is retained: provisioning still renders
// a specific hive's own manifest from it, and nothing on the heartbeat path
// enumerates other tenants' keys any more.

// appKeySyncDecision is the outcome of comparing a spoke's reported App identity
// against its cluster's authoritative one. Returned rather than acted on inline
// so the decision is table-testable without a filesystem or an HTTP server.
type appKeySyncDecision struct {
	// Push is true when the hub should send this spoke a GitHub App config at
	// all — which may be an app_id correction, a key delivery, or both.
	Push bool
	// PushKey is true when that config should carry KEY MATERIAL.
	//
	// It is deliberately separate from Push. The placeholder-sentinel repair is
	// an app_id correction that is valid even when the hub holds no key for the
	// cluster, and the heartbeat contract reads an empty private_key as "leave
	// the spoke's key alone". Collapsing the two would either block the repair
	// on a key the hub may not have, or send an empty key that blanks a working
	// one. Every decision that replaces a key sets both.
	PushKey bool
	// Reason is a short, log-safe explanation. Never contains key material.
	Reason string
	// FromFingerprint is what the spoke reported ("" = spoke reported none).
	FromFingerprint string
	// ToFingerprint is the cluster key's fingerprint the spoke should end up at.
	ToFingerprint string
}

// Reasons a sync decision was taken, as named constants so log queries and
// tests never depend on a literal typed twice.
const (
	appKeyReasonNoClusterKey    = "cluster has no app key configured"
	appKeyReasonPerHiveOverride = "hive has an explicitly provisioned per-hive key"
	appKeyReasonSpokeHasNoKey   = "spoke reports no app key"
	appKeyReasonMismatch        = "spoke key fingerprint differs from cluster key"
	appKeyReasonMatch           = "spoke already holds the cluster key"
	// appKeyReasonPerHiveUnusable is the one case that overrides the per-hive
	// precedence: the hive claims the cluster's app_id but holds a key that is
	// not the cluster's. That pair cannot sign a valid JWT for that App, so the
	// per-hive key is not a choice being protected — it is a fault.
	appKeyReasonPerHiveUnusable = "per-hive key cannot sign for the app_id this hive claims"
	// appKeyReasonDifferentApp is the per-hive case that IS protected: the hive
	// is pinned to an App other than the cluster's, so its key is presumed
	// correct for that App and the cluster key would break it.
	appKeyReasonDifferentApp = "hive is deliberately pinned to a different app_id"
	// appKeyReasonPlaceholderAppID repairs a spoke still running the
	// config.PlaceholderAppID sentinel. The sentinel is NOT a real App and is
	// never a deliberate pin — it is what provisioning writes when no App ID was
	// known yet — so the per-hive-override protection must not shield it. Such a
	// spoke signs a JWT for a nonexistent App and can never authenticate, no
	// matter what installation_id its owner enters, so pushing the cluster's real
	// App identity is the only thing that can fix it.
	appKeyReasonPlaceholderAppID = "spoke still carries the placeholder app_id sentinel"
	// appKeyReasonPublicHiveOnGHECluster is the class fix for a github.com hive
	// parked on a GitHub-Enterprise-default cluster (the heartbeat-only cluster). The hive's meta
	// pins it to public github.com (github_base_url:"public" / "https://github.com")
	// even though its cluster's default App is the GHE App. Pushing the cluster's
	// GHE key as the hive's PRIMARY every beat overwrites its github.com app_id and
	// breaks github.com auth. The hive is github.com: its own public App stays
	// primary. (The heartbeat no longer also broadcasts the cluster GHE key as an
	// additional key — that cross-tenant lane was removed for C1/N3; the GHE key
	// is delivered only when the hive is actually re-assigned to that App.) This
	// mirrors resolveProvisionAppID, which likewise honours the public pin instead
	// of forcing the cluster GHE app_id.
	appKeyReasonPublicHiveOnGHECluster = "hive is pinned to public github.com on a GHE-default cluster"
	// appKeyReasonWrongForgeApp repairs a spoke carrying an App registered on a
	// DIFFERENT GitHub host than the one the HIVE ITSELF talks to.
	//
	// appKeyReasonDifferentApp protects a deliberate pin, and that protection is
	// right whenever both Apps live on the same forge — an operator may genuinely
	// want a hive on its own App. But an App ID is only meaningful ON the host that
	// issued it: GitHub Enterprise and github.com maintain entirely separate App
	// registries, so a github.com app_id names NOTHING on github.ibm.com. Such a
	// pairing is not a configuration choice, it is provably broken in the same way
	// the placeholder sentinel is — every JWT is signed for an App the host has
	// never heard of, and no installation_id the owner enters can rescue it.
	//
	// It is also the state a forge migration leaves behind: moving a hive's host
	// without swapping its App identity strands the old forge's app_id on the new
	// host. That is exactly how a live GHE hive ended up emitting an install link
	// for the github.com App slug and 404ing on GitHub Enterprise.
	//
	// The discriminator is narrow, evidence-based and — since the 2026-08-05
	// fleet regression — PER-HIVE, never per-cluster: the spoke's own reported
	// app_id names a forge (config.ForgeOfAppID / the fleet's forge-app map)
	// that is not the forge the HIVE's own recorded host elects. It fires in
	// BOTH directions: a GHE-host hive whose spoke runs the public App (the EPM
	// class), and a public-host hive whose spoke was wrongly flipped onto the
	// GHE App (the alchemy class, mis-flipped by the cluster-keyed guard #2713
	// shipped). Judging it on the CLUSTER's forge instead is exactly what
	// force-flipped every github.com project on the heartbeat-only cluster to app 5686.
	appKeyReasonWrongForgeApp = "hive carries an app_id registered on a different github host"
)

// decideAppKeySync decides whether the hub should push its cluster App key to a
// spoke on this heartbeat.
//
// PRECEDENCE: an explicitly per-hive-provisioned key WINS over the cluster
// default. A per-hive key is a deliberate operator act — a hive pointed at a
// different App, or a hive mid-migration — and a cluster-wide reconcile that
// overwrote it would silently undo that decision on the next beat, with no way
// for the operator to make it stick. The cluster key is a FLOOR for hives nobody
// has spoken for, not a ceiling over hives somebody has.
//
// # THE ONE EXCEPTION — A WRONG PER-HIVE KEY IS NOT A CHOICE
//
// That precedence protects a DECISION. It must not protect a FAULT, and the two
// are distinguishable without heuristics. A GitHub App JWT is signed by the
// private key and presented alongside an app_id; GitHub verifies the signature
// against the public key registered for THAT app_id. So for a hive reporting
// app_id == identity.AppID with a key fingerprint != identity.Fingerprint, the
// credential pair is provably unusable — not "probably stale", not "differently
// configured", but arithmetically incapable of producing a token. That is
// exactly the state three live GHE hives are in: they carry the PUBLIC
// github.com App's key while claiming the GHE app_id, and every request dies
// with "401 A JSON web token could not be decoded". Withholding the cluster key
// from them protects nothing and leaves them permanently dead.
//
// The discriminator is therefore the app_id, and ONLY the app_id:
//
//	per-hive key + spoke app_id == cluster app_id + fingerprint differs
//	    -> PUSH. The key cannot work for the App the hive claims.
//	per-hive key + spoke app_id != cluster app_id (and non-zero)
//	    -> DO NOT PUSH. A different App on purpose; its key is right for it.
//	per-hive key + spoke app_id == 0 (unknown / too old to report)
//	    -> DO NOT PUSH. Silence is not evidence of a fault; stay conservative.
//
// This is a proof, not an inference: it never asks whether a key LOOKS stale,
// only whether the hive's own claimed App ID makes its own key impossible.
//
// IDEMPOTENCE: the hub pushes only when the fingerprints differ. Once the spoke
// reports the cluster fingerprint the decision flips to no-push and the key stops
// travelling. Re-sending a credential every 30 seconds to a spoke that already
// has it is pure exposure for no benefit.
//
// UNKNOWN IS NOT MISMATCH — with one deliberate exception. A spoke too old to
// report a fingerprint sends "". For most reconciles that must be read as
// "unknown, do nothing" (see GitHubAPIURL on HeartbeatPayload). Here the opposite
// is correct: a hive with NO key cannot authenticate at all, and the empty
// fingerprint is exactly what four live hives report. Pushing to an old spoke
// that in fact has a good key is harmless — it rewrites the same App identity —
// whereas withholding leaves a dead hive dead. Once the spoke is new enough to
// report, the push stops on the following beat.
//
// PUBLIC-PINNED HIVE ON A GHE CLUSTER — never force the cluster GHE app.
// hivePublicPinned is true when the hive's meta pins it to public github.com
// (github_base_url:"public" / "https://github.com") while its cluster defaults to
// a GHE App. The cluster identity resolved above is that GHE App; pushing it as
// this hive's PRIMARY would overwrite its github.com app_id and break github.com
// auth on the very next beat. So the reconcile refuses to push the cluster key as
// primary for such a hive — its own public App stays primary. (The heartbeat no
// longer also broadcasts the cluster GHE key as an additional key; that
// cross-tenant delivery lane was removed for C1/N3. Such a hive is handed the
// GHE App key only when it is actually re-assigned to that App.) This is the
// app-KEY reconcile honouring the same public pin that resolveProvisionAppID and
// effectiveGitHubBaseURL already honour on the provisioning path.
//
// WRONG-FORGE APP — spokeOnWrongForge carries the one fact this function cannot
// see for itself: that the App the spoke reports was issued by a DIFFERENT forge
// than the one this HIVE's own recorded host elects (computed by the caller from
// the spoke's app_id via the app-id→forge map, against the hive's resolved
// forge). Combined with a non-zero spoke app_id that differs from the identity
// being delivered, that proves the spoke's App names nothing on the host this
// hive uses. It is judged on the HIVE's forge, never the cluster's: a cluster
// hosts hives of BOTH forges (the heartbeat-only cluster carries github.ibm.com projects beside
// github.com ones), so cluster membership alone proves nothing about any one
// hive. See appKeyReasonWrongForgeApp.
//
// THE identity PARAMETER IS THE HIVE'S RESOLVED IDENTITY, NOT THE CLUSTER'S.
// It is the App of the hive's ELECTED forge (appIdentityForHive /
// ResolveHiveIdentityInFleet), which coincides with the cluster's App only for
// hives that elected nothing or elected their cluster's own forge. The caller
// (appKeyConfigForHeartbeat) guarantees that whenever spokeOnWrongForge is
// true the identity's App genuinely belongs to the elected forge. That
// guarantee is load-bearing for the wrong-forge gate below: comparing the
// spoke's App against the CLUSTER's App instead is exactly what made the
// repair unreachable for a public-elected hive mis-flipped onto its GHE
// cluster's own App (5686 == 5686 — the gate saw the spoke's broken App on
// both sides and stood down, every beat, for six live hives on 2026-08-05).
func decideAppKeySync(spokeFingerprint string, hasPerHiveKey, hivePublicPinned, spokeOnWrongForge bool, spokeAppID int64, identity *clusterAppIdentity) appKeySyncDecision {
	if identity == nil {
		return appKeySyncDecision{Reason: appKeyReasonNoClusterKey, FromFingerprint: spokeFingerprint}
	}
	if hivePublicPinned {
		// The hive is public github.com on a GHE-default cluster. Never push the
		// cluster's GHE App as primary; the additional-keys pass still delivers the
		// GHE key so a later migration onto that App has it on disk.
		return appKeySyncDecision{
			Reason:          appKeyReasonPublicHiveOnGHECluster,
			FromFingerprint: strings.TrimSpace(spokeFingerprint),
			ToFingerprint:   identity.Fingerprint,
		}
	}
	fp := strings.TrimSpace(spokeFingerprint)
	// A spoke still running the placeholder sentinel is broken regardless of
	// which key it holds: app_id 999999999 names no App, so every JWT it signs
	// is for a nonexistent App and auth fails no matter how correct its
	// installation_id and key are. Decided BEFORE the per-hive branch because a
	// sentinel hive typically holds a provisioned per-hive key and would
	// otherwise be shielded by appKeyReasonPerHiveOverride and never repaired —
	// and before the fingerprint-match test, since holding the right key does
	// not make a nonexistent app_id work.
	//
	// This is the ONLY app_id the reconcile treats as unconditionally wrong. A
	// spoke on any other non-matching app_id is still respected as a deliberate
	// pin (appKeyReasonDifferentApp).
	// The sentinel repair deliberately does NOT require the hub to hold this
	// cluster's key. Correcting app_id 999999999 to the cluster's real App is a
	// non-secret change, and it is the whole fix for a spoke that already holds
	// a valid per-hive key provisioned for that App. Gating the repair on key
	// material — as the original identity lookup did — is precisely what made
	// this branch dead code on a cluster that had configured its app_id but
	// never uploaded a PEM. The key rides along only when the hub actually has
	// one; otherwise this is an app_id-only push.
	if spokeAppID == config.PlaceholderAppID {
		return appKeySyncDecision{
			Push:            true,
			PushKey:         identity.HasKey(),
			Reason:          appKeyReasonPlaceholderAppID,
			FromFingerprint: fp,
			ToFingerprint:   identity.Fingerprint,
		}
	}
	// A spoke holding an App from ANOTHER forge is broken for the same reason the
	// sentinel is: the app_id names no App on the host this hive actually uses.
	// Decided here — after the sentinel and the public pin, but BEFORE the
	// per-hive-key shield — because a stranded hive typically does hold a per-hive
	// key (for the other forge's App) and would otherwise be protected as
	// appKeyReasonDifferentApp and never repaired.
	//
	// Requires a POSITIVE, non-zero app_id disagreement: zero means "too old to
	// report", and silence is never evidence of a fault (same contract as the
	// per-hive exception above). spokeOnWrongForge is itself only ever true on
	// positive evidence — a spoke app_id whose issuing forge is KNOWN and differs
	// from the forge the hive's own recorded host elects — so an unrecognised
	// App ID (a third forge, a self-hosted deployment) stays protected as a
	// deliberate pin.
	//
	// Like the sentinel repair this does NOT require the hub to hold the cluster's
	// key — correcting a cross-forge app_id/app_slug is a non-secret change and is
	// the whole fix for a hive whose owner has yet to install the App at all.
	//
	// The third clause compares the spoke's App against the HIVE'S ELECTED
	// forge's App — identity is the per-hive resolution, never the raw cluster
	// record. For a coherent identity it is implied by spokeOnWrongForge (the
	// spoke's App is another forge's, the identity's is the elected forge's, and
	// no App ID belongs to two forges); it stays as a guard so an incoherent
	// caller can never make this branch push a spoke's own broken App back at it.
	if spokeOnWrongForge && spokeAppID != 0 && spokeAppID != identity.AppID {
		return appKeySyncDecision{
			Push:            true,
			PushKey:         identity.HasKey(),
			Reason:          appKeyReasonWrongForgeApp,
			FromFingerprint: fp,
			ToFingerprint:   identity.Fingerprint,
		}
	}
	// Beyond the sentinel repair every remaining decision is about REPLACING the
	// spoke's key, which the hub cannot do without holding one. Stopping here
	// keeps a keyless cluster from reporting a "mismatch" against an empty
	// fingerprint and pushing a blank key over a working one.
	if !identity.HasKey() {
		return appKeySyncDecision{
			Reason:          appKeyReasonNoClusterKey,
			FromFingerprint: fp,
		}
	}
	if hasPerHiveKey {
		// Idempotence first: if the per-hive key already IS the cluster key
		// there is nothing to decide and nothing to send. Checking this ahead of
		// the app_id logic also means a hive whose provisioned key happens to be
		// correct never enters the exception path at all.
		if fp != "" && fp == identity.Fingerprint {
			return appKeySyncDecision{
				Reason:          appKeyReasonMatch,
				FromFingerprint: fp,
				ToFingerprint:   identity.Fingerprint,
			}
		}
		// The exception: the hive claims this cluster's App but cannot sign for
		// it. Requires a POSITIVE app_id match — zero (unknown) and any other
		// App both fall through to the protected override below.
		if spokeAppID != 0 && spokeAppID == identity.AppID && fp != "" && fp != identity.Fingerprint {
			return appKeySyncDecision{
				Push:            true,
				PushKey:         true,
				Reason:          appKeyReasonPerHiveUnusable,
				FromFingerprint: fp,
				ToFingerprint:   identity.Fingerprint,
			}
		}
		reason := appKeyReasonPerHiveOverride
		if spokeAppID != 0 && spokeAppID != identity.AppID {
			reason = appKeyReasonDifferentApp
		}
		return appKeySyncDecision{
			Reason:          reason,
			FromFingerprint: fp,
			ToFingerprint:   identity.Fingerprint,
		}
	}
	if fp == "" {
		return appKeySyncDecision{
			Push:          true,
			PushKey:       true,
			Reason:        appKeyReasonSpokeHasNoKey,
			ToFingerprint: identity.Fingerprint,
		}
	}
	if fp != identity.Fingerprint {
		return appKeySyncDecision{
			Push:            true,
			PushKey:         true,
			Reason:          appKeyReasonMismatch,
			FromFingerprint: fp,
			ToFingerprint:   identity.Fingerprint,
		}
	}
	return appKeySyncDecision{
		Reason:          appKeyReasonMatch,
		FromFingerprint: fp,
		ToFingerprint:   identity.Fingerprint,
	}
}

// appKeyConfigForHeartbeat returns the GitHub App config to attach to a spoke's
// heartbeat response so it self-corrects onto its cluster's key, or nil when
// nothing needs to change.
//
// installationID is echoed back unchanged from what the hub already knows for
// this hive: this reconcile fixes the KEY, and must never disturb a correct
// installation_id. Zero is passed through as zero — the spoke's callback only
// rebuilds its client once app_id, installation_id and key file are all present,
// so a key delivered ahead of an installation ID lands on disk and waits.
func (s *HubServer) appKeyConfigForHeartbeat(hiveID, clusterID string, spokeFingerprint string, hasPerHiveKey, hivePublicPinned bool, spokeAppID int64, installationID int64, logger *slog.Logger, hiveOpt ...*SaaSHive) *HeartbeatGitHubAppConfig {
	// hiveOpt carries the hub's record for this hive, whose github_host is the
	// hive's OWN elected forge. Without it this path answered from the cluster
	// alone and overwrote correctly-repaired spokes on the next beat.
	//
	// Variadic so the existing callers/tests with no hive record keep compiling
	// and keep their exact previous behaviour (no election -> cluster default).
	var hive *SaaSHive
	if len(hiveOpt) > 0 {
		hive = hiveOpt[0]
	}
	identity := s.appIdentityForHive(hive, clusterID)
	// Is the SPOKE running an App from a forge other than the one this HIVE's
	// own recorded host elects? Judged per-hive, never per-cluster.
	//
	// The 2026-08-05 fleet regression is why this is emphatically NOT
	// "clusterIsGHE": #2713 shipped this branch keyed on clusterDefaultForge, so
	// every hive on the GHE-default heartbeat-only cluster whose spoke reported the
	// public App — including the github.com projects that cluster also hosts
	// (ibm/alchemy-logging: org "ibm" lives on public github.com) — was
	// "repaired" onto app 5686 / github.ibm.com and 404'd on every token mint.
	// A cluster hosts hives of BOTH forges; cluster membership proves nothing
	// about any one hive's forge.
	//
	// The per-hive signal needs two pieces of POSITIVE evidence:
	//   - the hive's own forge is known: it recorded a host (electedForgeForHive)
	//     or it is a CLAIMED hive, whose empty github_host means public
	//     github.com by the field's own contract. A nil record or an unclaimed
	//     placeholder yields no evidence and the repair stands down.
	//   - the spoke's reported app_id maps to a KNOWN forge (forgeOfSpokeAppID)
	//     that differs from the forge of the identity resolved FOR THIS HIVE.
	//     An unrecognised App ID is a deliberate pin, not evidence.
	//
	// This also gives the repair its REVERSE direction: a public-host hive whose
	// spoke reports the GHE App (the mis-flipped alchemy class) resolves a
	// public identity above, its spoke's app_id maps to the GHE forge, and the
	// same branch flips it back.
	spokeOnWrongForge := false
	hiveForgeKnown := false
	if hive != nil {
		var cl *ClusterConfig
		if c, ok := s.clusters[clusterID]; ok {
			cl = &c
		}
		hiveForgeKnown = electedForgeForHive(hive, cl) != "" || hiveClaimed(hive)
	}
	if hiveForgeKnown && identity != nil && identity.Forge != "" {
		if spokeForge := s.forgeOfSpokeAppID(spokeAppID); spokeForge != "" && !sameGitHubHost(spokeForge, identity.Forge) {
			spokeOnWrongForge = true
		}
	}
	// THE REPAIR DELIVERS THE ELECTED FORGE'S APP — NEVER ANOTHER FORGE'S.
	//
	// The identity resolved above is supposed to be the hive's elected forge's
	// App, but two config shapes can leak a wrong-forge App into it: a cluster
	// record whose flat github_app_id names the GHE App while its blank urls
	// default the record to public (the App lands keyed under the hive's OWN
	// forge), and any equivalent drift in a forges map. Delivering that identity
	// would push the spoke's broken App straight back at it — and the delivery
	// gate below, comparing the spoke's App against the SAME wrong App
	// (5686 == 5686), would instead stand down and deliver nothing forever.
	// That dead gate is why six public hives whose hub meta was correctly
	// restored to github_host=github.com received no re-delivery across many
	// heartbeats on 2026-08-05 and had to be hand-restored from backups.
	//
	// So when the spoke is provably on the wrong forge, verify the identity
	// being delivered is provably NOT: an App whose issuing forge is known
	// (config.ForgeOfAppID) must agree with the hive's elected forge, else it is
	// rebuilt from the build's own App for that forge — with the key looked up
	// by the App it belongs to, and the urls stated explicitly so the spoke is
	// moved off the wrong forge's urls in the same atomic set. If the build
	// cannot name an App on the elected forge either, the repair stands down
	// rather than deliver another forge's App.
	if spokeOnWrongForge {
		if f := config.ForgeOfAppID(identity.AppID); f != "" && !sameGitHubHost(f, identity.Forge) {
			if appID, slug := builtinAppOfForge(identity.Forge); appID != 0 {
				identity.AppID = appID
				identity.AppSlug = slug
				// The key must follow the App: whatever key rode in with the
				// leaked identity signs for the WRONG App. Empty means "leave
				// the spoke's key alone", which is the safe floor.
				identity.PrivateKey, identity.Fingerprint = "", ""
				if k, found := s.appKeysByAppID()[appID]; found && k.PrivateKey != "" {
					identity.PrivateKey, identity.Fingerprint = k.PrivateKey, k.Fingerprint
				}
				if isPublicForgeHost(identity.Forge) {
					identity.APIURL, identity.BaseURL = config.DefaultGitHubAPIURL, config.DefaultGitHubBaseURL
				} else {
					identity.BaseURL = "https://" + forgeHostLabel(identity.Forge)
					identity.APIURL = identity.BaseURL + gheAPIPathSuffix
				}
			} else {
				spokeOnWrongForge = false
			}
		}
	}
	// The public pin is honoured BY the per-hive identity now: a public-pinned
	// hive resolves the PUBLIC App above, so delivering that identity is the pin
	// being enforced, not violated. Suppressing delivery on the pin (the old
	// behaviour, from when this path could only answer with the cluster's GHE
	// App) would leave a public-pinned spoke that was wrongly flipped onto the
	// GHE App broken forever — the hub would hold the right answer and refuse to
	// send it. Only a GHE identity may still be suppressed by the pin, and the
	// per-hive resolver can no longer produce one for a public-pinned hive.
	if hivePublicPinned && identity != nil && isPublicForgeHost(identity.Forge) {
		hivePublicPinned = false
	}
	decision := decideAppKeySync(spokeFingerprint, hasPerHiveKey, hivePublicPinned, spokeOnWrongForge, spokeAppID, identity)
	// A spoke carrying the sentinel is broken and must be legible even when the
	// hub can do nothing about it — that silence is what made this bug cost days
	// of debugging while the dashboard blamed the installation ID. Logged BEFORE
	// the no-push return so the unrepairable case (cluster names no app_id at
	// all) is exactly the case that gets said out loud.
	if spokeAppID == config.PlaceholderAppID && logger != nil {
		if identity == nil {
			logger.Warn("heartbeat: spoke carries the placeholder app_id sentinel and its cluster names no github app — cannot repair",
				"hive_id", hiveID,
				"cluster_id", clusterID,
				"spoke_app_id", spokeAppID,
				"remedy", "set github_app_id on this cluster in clusters.json",
			)
		} else {
			logger.Info("heartbeat: repairing placeholder app_id sentinel on spoke",
				"hive_id", hiveID,
				"cluster_id", clusterID,
				"spoke_app_id", spokeAppID,
				"to_app_id", identity.AppID,
				"cluster_has_key", identity.HasKey(),
			)
		}
	}
	if !decision.Push || identity == nil {
		return nil
	}
	// An identity with no key still repairs an app_id. Sending its empty
	// PrivateKey is safe and intended — the spoke callback reads an empty
	// private_key as "leave the key file alone" — but PushKey is honoured
	// explicitly so the intent is in the code rather than in that coincidence.
	privateKey := ""
	if decision.PushKey {
		privateKey = identity.PrivateKey
	}
	if logger != nil {
		// Fingerprints only. The key itself is never logged, at any level.
		logger.Info("heartbeat: pushing cluster github app config to spoke",
			"hive_id", hiveID,
			"cluster_id", clusterID,
			"app_id", identity.AppID,
			"spoke_app_id", spokeAppID,
			"per_hive_key", hasPerHiveKey,
			"public_pinned", hivePublicPinned,
			"push_key", decision.PushKey,
			"reason", decision.Reason,
			"from_fingerprint", decision.FromFingerprint,
			"to_fingerprint", decision.ToFingerprint,
		)
	}
	// WRITE-PATH GUARD, hub side. This struct is the exact channel that carried
	// the 2026-07-31 breakage: it delivers app_id and app_slug and has no field
	// for api_url, so a cluster whose github_app_id names one forge while its
	// URLs name another pushes a half identity the spoke cannot reassemble.
	// Refuse to construct it.
	//
	// Validated against the identity the CLUSTER declares — app_id and app_slug
	// from the App identity, api_url and base_url from the cluster record — so a
	// clusters.json entry naming a GHE App beside public-GitHub URLs is caught
	// at the source. The spoke re-validates against its own current values,
	// which is the half this side cannot see.
	//
	// GATED ON THE CLUSTER DECLARING A FORGE AT ALL. A cluster record with both
	// URLs empty is UNDER-SPECIFIED, not public: several real records carry only
	// github_app_id and github_app_slug, and clusterIsGHE above already treats
	// empty github_base_url as "no forge information". Reading that silence as
	// "public github.com" here would refuse every GHE cluster that has not
	// backfilled its URLs and stop the key/app_id repair path fleet-wide — the
	// same class of outage this guard exists to prevent, in the other direction.
	// Silence is never evidence. When the cluster does declare a forge, the full
	// set is checked.
	// Validate against the forge THIS HIVE resolved to. The App and the URLs
	// must come from the SAME resolution or the check compares two forges and
	// refuses a set that is actually correct.
	//
	// Fall back to the cluster record only when the resolver named no forge at
	// all. A public election legitimately resolves to EMPTY urls — that is how
	// every healthy public spoke runs — so empty is not itself a reason to fall
	// back; only an unresolved forge is.
	clusterAPIURL := identity.APIURL
	clusterBaseURL := identity.BaseURL
	if identity.Forge == "" {
		clusterAPIURL = clusterAPIURLForIdentity(s, clusterID)
		clusterBaseURL = clusterBaseURLForIdentity(s, clusterID)
	}
	clusterDeclaresForge := clusterAPIURL != "" || clusterBaseURL != ""

	if err := config.RejectIdentitySet(config.GitHubConfig{
		AppID:   identity.AppID,
		AppSlug: identity.AppSlug,
		APIURL:  clusterAPIURL,
		BaseURL: clusterBaseURL,
	}); err != nil && clusterDeclaresForge {
		if logger != nil {
			logger.Error("REFUSING to push cluster github app config: the cluster's identity set is inconsistent and would half-apply on the spoke — nothing was sent",
				"error", err,
				"hive_id", hiveID,
				"cluster_id", clusterID,
				"app_id", identity.AppID,
				"app_slug", identity.AppSlug,
				"remedy", "correct github_app_id/github_app_slug/github_api_url/github_base_url for this cluster in clusters.json",
			)
		}
		return nil
	}

	// installation_id IS PART OF THE ATOMIC SET. An installation belongs to ONE
	// App: whenever the delivered identity CHANGES the spoke's app_id, the old
	// installation_id is the previous App's and can never mint a token under the
	// new one — reused, it 404s on POST /app/installations/<id>/access_tokens,
	// which is exactly how the 2026-08-05 delivery (app swap with the old
	// installation_id left in place) degraded 9 spokes at 23:56Z. So an
	// app-changing delivery carries an explicit ResetInstallation: the spoke
	// drops the stale id, HasUsableApp() goes false, the "Install GitHub App /
	// Set ID" banner is raised, and the 2-minute self-heal ticker's
	// RediscoverAndAdopt re-establishes the correct installation for the
	// delivered App (automatically, when the App is already installed on the
	// org — the flip-back case, where the old id becomes discoverable again).
	//
	// Conservative on silence, as everywhere: a zero spoke app_id (too old to
	// report) never resets, and the placeholder sentinel never does either —
	// a sentinel hive's installation_id was entered by its owner FOR the real
	// App being delivered, not for a previous App, and must be preserved.
	// When the app_id is UNCHANGED, installation_id is left strictly alone.
	resetInstallation := spokeAppID != 0 &&
		spokeAppID != config.PlaceholderAppID &&
		identity.AppID != spokeAppID
	deliveredInstallationID := installationID
	if resetInstallation {
		deliveredInstallationID = 0
	}
	if resetInstallation && logger != nil {
		logger.Info("heartbeat: delivered app identity changes the spoke's app_id — resetting installation_id with it (atomic set)",
			"hive_id", hiveID,
			"cluster_id", clusterID,
			"spoke_app_id", spokeAppID,
			"to_app_id", identity.AppID,
		)
	}

	// Emit the COMPLETE set. The guard above validated app_id/app_slug against
	// the forge URLs this hive resolved to; sending the App without those URLs
	// would ship a set the spoke cannot reassemble, and leave it to be completed
	// — or contradicted — by a ProjectConfig push on a different channel.
	//
	// The URLs are the RESOLVED ones (identity.APIURL/BaseURL), so what the
	// spoke receives is byte-identical to what was just validated. A public
	// election resolves to empty URLs, which the spoke reads as "unchanged" —
	// correct, because empty is also how every healthy public spoke is stored.
	return &HeartbeatGitHubAppConfig{
		AppID:             identity.AppID,
		InstallationID:    deliveredInstallationID,
		ResetInstallation: resetInstallation,
		PrivateKey:        privateKey,
		AppSlug:           identity.AppSlug,
		APIURL:            identity.APIURL,
		BaseURL:           identity.BaseURL,
	}
}

// forgeOfSpokeAppID maps a spoke-reported app_id to the forge that issued it,
// or "" when the App is unknown to this hub.
//
// Two sources, both positive evidence only: the build's own forge-identity
// table (config.ForgeOfAppID — the public and known-GHE Apps), then the fleet's
// cluster configs (forgeAppsAcrossFleet — an App any cluster names on any
// forge, forge-map shape included). Zero and the placeholder sentinel map to
// nothing: silence is never evidence.
func (s *HubServer) forgeOfSpokeAppID(appID int64) string {
	if appID == 0 || appID == config.PlaceholderAppID {
		return ""
	}
	if f := config.ForgeOfAppID(appID); f != "" {
		return forgeHostLabel(f)
	}
	if s == nil {
		return ""
	}
	for host, app := range s.forgeAppsAcrossFleet() {
		if app.AppID == appID {
			return forgeHostLabel(host)
		}
	}
	return ""
}

// clusterAPIURLForIdentity and clusterBaseURLForIdentity read the cluster's
// declared forge URLs so the hub-side guard validates the FULL identity set the
// cluster declares, not just the two fields the heartbeat channel can carry.
//
// An unknown cluster, or one that declares neither URL, yields empty strings.
// The caller reads that as "no forge declared" and skips the check entirely
// rather than assuming public github.com — see clusterDeclaresForge.
func clusterAPIURLForIdentity(s *HubServer, clusterID string) string {
	if c, ok := s.clusters[clusterID]; ok {
		return strings.TrimSpace(c.GitHubAPIURL)
	}
	return ""
}

func clusterBaseURLForIdentity(s *HubServer, clusterID string) string {
	if c, ok := s.clusters[clusterID]; ok {
		return strings.TrimSpace(c.GitHubBaseURL)
	}
	return ""
}

// --- Operator API ---

// clusterAppKeyRequest is the admin upload body for a cluster's App key.
type clusterAppKeyRequest struct {
	// AppID is the numeric GitHub App ID on that cluster's GitHub host. Optional
	// on upload: when omitted the cluster's configured github_app_id stands.
	AppID int64 `json:"app_id,omitempty"`
	// PrivateKey is the PEM private key. It is WRITE-ONLY: it is stored to the
	// 0600 key file and is never returned by any endpoint, in any form.
	PrivateKey string `json:"private_key"`
	// ForAppID scopes the upload to a SPECIFIC App, so a cluster can hold a
	// second (optional) App's key beside its primary one (#4815).
	//
	// OPTIONAL AND ABSENT BY DEFAULT. Omitted — the shape every existing caller
	// sends — means "the cluster's primary App", which writes the same
	// <clusterID>.pem the endpoint has always written, with the same result.
	// Nothing about the existing contract changes.
	//
	// WHY NOT REUSE AppID ABOVE
	//
	// AppID already means "also record this as the cluster's primary
	// github_app_id in clusters.json" and callers send it that way today. Giving
	// it a second meaning would make an existing primary-key upload that names
	// its own App ID — a completely ordinary request — suddenly write to a
	// secondary path instead, silently leaving the primary key stale. A separate
	// field cannot be misread by a caller that has never heard of it.
	//
	// Setting it to the cluster's primary App is REJECTED rather than accepted
	// as a no-op alias: an operator who believes they are uploading a second
	// App's key while actually naming the primary would otherwise overwrite the
	// key ~55 live hives authenticate with. That silent overwrite is the entire
	// bug this field exists to prevent, so it must fail loudly.
	ForAppID int64 `json:"for_app_id,omitempty"`
}

// clusterAppKeyStatus is the read side. It carries the fingerprint and NEVER the
// key — this shape is what any operator UI renders, and the omission is the
// point, not an oversight.
type clusterAppKeyStatus struct {
	ClusterID   string `json:"cluster_id"`
	AppID       int64  `json:"app_id,omitempty"`
	HasKey      bool   `json:"has_key"`
	Fingerprint string `json:"fingerprint,omitempty"`
	// SecondaryKeys lists the cluster's non-primary App keys, fingerprints only,
	// ascending by App ID. omitempty and nil for every cluster that has none —
	// which is every cluster in the fleet today — so an existing consumer of
	// this payload sees a byte-identical response.
	SecondaryKeys []secondaryAppKeyStatus `json:"secondary_keys,omitempty"`
}

// secondaryAppKeyStatus is one non-primary App key the hub holds for a cluster.
// Like clusterAppKeyStatus it carries the fingerprint and NEVER the key; the
// omission is the contract, not an oversight.
type secondaryAppKeyStatus struct {
	AppID       int64  `json:"app_id"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

// secondaryAppKeyStatusesFor builds the read-side inventory of a cluster's
// non-primary keys. Fingerprints are recomputed from disk rather than cached so
// the operator sees what is actually stored, which is the whole point of the
// endpoint (confirming an upload landed).
func secondaryAppKeyStatusesFor(clusterID string) []secondaryAppKeyStatus {
	appIDs := secondaryAppKeysForCluster(clusterID)
	if len(appIDs) == 0 {
		return nil
	}
	out := make([]secondaryAppKeyStatus, 0, len(appIDs))
	for _, id := range appIDs {
		out = append(out, secondaryAppKeyStatus{
			AppID:       id,
			Fingerprint: secondaryAppKeyFingerprint(clusterID, id),
		})
	}
	return out
}

// handleGetClusterAppKeys reports, per cluster, whether a hub-held App key
// exists and which key it is — by fingerprint only. An operator uses this to
// confirm an upload landed and that the fleet is converging, without the key
// ever being retrievable once stored.
func (s *HubServer) handleGetClusterAppKeys(w http.ResponseWriter, r *http.Request) {
	statuses := make([]clusterAppKeyStatus, 0, len(s.clusters))
	for id, c := range s.clusters {
		fp := clusterAppKeyFingerprint(id)
		statuses = append(statuses, clusterAppKeyStatus{
			ClusterID: id,
			AppID:     c.GitHubAppID,
			// HasKey stays the PRIMARY key's presence. A cluster holding only a
			// secondary key still has no primary key, and reporting otherwise
			// would tell an operator the fleet's auth is provisioned when it is
			// not.
			HasKey:        fp != "",
			Fingerprint:   fp,
			SecondaryKeys: secondaryAppKeyStatusesFor(id),
		})
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].ClusterID < statuses[j].ClusterID })
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(statuses)
}

// handlePutClusterAppKey stores (or replaces) a cluster's App private key.
//
// This is the ONLY way key material enters the hub. It is admin-gated, the key
// is validated before it is persisted, and the response echoes back only the
// resulting fingerprint so the operator can verify the right key landed without
// the endpoint ever becoming a way to read a key back out.
func (s *HubServer) handlePutClusterAppKey(w http.ResponseWriter, r *http.Request) {
	clusterID := r.PathValue("clusterID")
	if _, ok := s.clusters[clusterID]; !ok {
		http.Error(w, `{"error":"unknown cluster"}`, http.StatusNotFound)
		return
	}

	var body clusterAppKeyRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxClusterAppKeyBytes)).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid JSON body"}`, http.StatusBadRequest)
		return
	}
	key := strings.TrimSpace(body.PrivateKey)
	if !strings.HasPrefix(key, "-----BEGIN") {
		http.Error(w, `{"error":"private_key must be a PEM private key"}`, http.StatusBadRequest)
		return
	}
	fp, err := config.AppKeyFingerprint(key)
	if err != nil {
		// Report only that parsing failed. The error must not quote the input.
		http.Error(w, `{"error":"private_key is not a usable RSA/EC private key"}`, http.StatusBadRequest)
		return
	}
	primaryAppID := s.clusters[clusterID].GitHubAppID

	// SECOND-APP UPLOAD (#4815). Taken BEFORE any write, and it returns from its
	// own branch rather than falling through, so there is no path on which a
	// secondary upload can reach storeClusterAppKey and clobber the primary key.
	if body.ForAppID != 0 {
		if body.ForAppID < 0 {
			http.Error(w, `{"error":"for_app_id must be a positive github app id"}`, http.StatusBadRequest)
			return
		}
		// The guard this issue exists for. An operator naming the cluster's own
		// App here believes they are adding a second key; accepting it would
		// either overwrite the key ~55 live hives authenticate with, or fork a
		// shadow copy that silently goes stale. Refuse, and say which App the
		// cluster's primary actually is so the mistake is obvious.
		if primaryAppID != 0 && body.ForAppID == primaryAppID {
			http.Error(w, fmt.Sprintf(
				`{"error":"app %d is this cluster's primary app; omit for_app_id to replace the primary key"}`,
				body.ForAppID), http.StatusConflict)
			return
		}
		// A cluster with no primary app_id configured has no primary key to
		// protect, but it also has no way to tell a "second" App from its first.
		// Refuse rather than guess: silently creating a secondary key on such a
		// cluster would leave the real primary slot empty and the reconcile
		// delivering nothing, with the operator believing the upload landed.
		if primaryAppID == 0 {
			http.Error(w, `{"error":"cluster has no primary github_app_id configured; upload its primary key first"}`, http.StatusConflict)
			return
		}
		if body.AppID != 0 && body.AppID != body.ForAppID {
			// app_id sets the cluster's PRIMARY App. Combining it with a
			// secondary upload asks for two contradictory things in one request.
			http.Error(w, `{"error":"app_id cannot be set on a secondary (for_app_id) upload"}`, http.StatusBadRequest)
			return
		}
		if err := storeSecondaryAppKey(clusterID, body.ForAppID, primaryAppID, key); err != nil {
			s.logger.Error("failed to store secondary cluster app key",
				"cluster_id", clusterID, "for_app_id", body.ForAppID, "error", err)
			http.Error(w, `{"error":"failed to store key"}`, http.StatusInternalServerError)
			return
		}
		s.logger.Info("audit: secondary github app key stored for cluster",
			"cluster_id", clusterID,
			"for_app_id", body.ForAppID,
			"primary_app_id", primaryAppID,
			"fingerprint", fp, // fingerprint only — never the key
			// The primary's fingerprint is logged UNCHANGED so the audit trail
			// itself is the evidence that this upload did not disturb it.
			"primary_fingerprint", clusterAppKeyFingerprint(clusterID),
			"by", s.getAuthUser(r),
		)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(clusterAppKeyStatus{
			ClusterID:     clusterID,
			AppID:         primaryAppID,
			HasKey:        clusterAppKeyFingerprint(clusterID) != "",
			Fingerprint:   clusterAppKeyFingerprint(clusterID),
			SecondaryKeys: secondaryAppKeyStatusesFor(clusterID),
		})
		return
	}

	if err := storeClusterAppKey(clusterID, key); err != nil {
		s.logger.Error("failed to store cluster app key", "cluster_id", clusterID, "error", err)
		http.Error(w, `{"error":"failed to store key"}`, http.StatusInternalServerError)
		return
	}

	// Persist the app_id alongside so the cluster has a complete identity. The
	// ID is not secret and belongs in clusters.json.
	appID := primaryAppID
	if body.AppID != 0 && body.AppID != appID {
		if err := s.setClusterAppID(clusterID, body.AppID); err != nil {
			s.logger.Warn("stored cluster app key but failed to persist app_id",
				"cluster_id", clusterID, "error", err)
		} else {
			appID = body.AppID
		}
	}

	s.logger.Info("audit: cluster github app key stored",
		"cluster_id", clusterID,
		"app_id", appID,
		"fingerprint", fp, // fingerprint only — never the key
		"by", s.getAuthUser(r),
	)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(clusterAppKeyStatus{
		ClusterID:   clusterID,
		AppID:       appID,
		HasKey:      true,
		Fingerprint: fp,
		// nil — hence absent from the JSON — for every cluster without a second
		// App, so a primary upload's response stays byte-identical to today's.
		SecondaryKeys: secondaryAppKeyStatusesFor(clusterID),
	})
}

// maxClusterAppKeyBytes caps the upload body. A 4096-bit RSA PEM is well under
// 4 KiB; 16 KiB leaves generous headroom while refusing to buffer an arbitrary
// payload into memory.
const maxClusterAppKeyBytes = 16 << 10

// setClusterAppID records a cluster's non-secret App ID in both the in-memory
// map and clusters.json, so it survives a hub restart.
func (s *HubServer) setClusterAppID(clusterID string, appID int64) error {
	c, ok := s.clusters[clusterID]
	if !ok {
		return fmt.Errorf("unknown cluster %q", clusterID)
	}
	c.GitHubAppID = appID
	s.clusters[clusterID] = c

	// Rewrite clusters.json preserving every other cluster untouched. Read the
	// file fresh rather than re-serializing the in-memory map: loadClusters
	// silently drops entries that fail validation, and writing the map back
	// would delete them from disk permanently.
	data, err := os.ReadFile(clustersConfigPath)
	if err != nil {
		return fmt.Errorf("read clusters config: %w", err)
	}
	var configs []ClusterConfig
	if err := json.Unmarshal(data, &configs); err != nil {
		return fmt.Errorf("parse clusters config: %w", err)
	}
	found := false
	for i := range configs {
		if configs[i].ID == clusterID {
			configs[i].GitHubAppID = appID
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("cluster %q not present in %s", clusterID, clustersConfigPath)
	}
	out, err := json.MarshalIndent(configs, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal clusters config: %w", err)
	}
	// Atomic replace, same reasoning as the key store: a truncated clusters.json
	// would leave the hub unable to reach any cluster on its next restart.
	tmp := clustersConfigPath + ".tmp"
	if err := os.WriteFile(tmp, out, clustersConfigFileMode); err != nil {
		return fmt.Errorf("write clusters config: %w", err)
	}
	if err := os.Rename(tmp, clustersConfigPath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename clusters config: %w", err)
	}
	return nil
}

// clustersConfigFileMode is rw-r----- : clusters.json holds no secrets (that is
// the whole point of keeping the App key out of it) but it does describe
// kubeconfig locations, so it is not world-readable.
const clustersConfigFileMode = 0o640

// appKeySyncForHeartbeat is the heartbeat-handler entry point for the standing
// per-cluster App key reconcile. It resolves which cluster the beating spoke
// belongs to and defers to appKeyConfigForHeartbeat for the decision.
//
// Returns nil — meaning "attach nothing, change nothing" — for every hive whose
// cluster has no hub-managed App identity, which is the overwhelming majority.
// The reconcile is strictly opt-in per cluster.
func (s *HubServer) appKeySyncForHeartbeat(payload *HeartbeatPayload) *HeartbeatGitHubAppConfig {
	if s == nil || payload == nil {
		return nil
	}
	// Load the hive's own meta once: it carries the GitHub-host pin
	// (github_base_url) that decides whether this hive is public github.com, and
	// it is the AUTHORITATIVE source for the cluster ID — see below.
	sh := loadSaaSHive(payload.HiveID)
	// CLUSTER PRECEDENCE: the hub's own SaaS record first, the spoke's
	// self-report only as a fallback. Every other surface already resolves it
	// that way — the registry heartbeat handler (server.go) stamps
	// entry.ClusterID from sh.ClusterID first, and it is THAT ID the
	// app-creds-undelivered alert checks the PEM store under and names in its
	// PUT /api/saas/admin/cluster-app-keys/{id} remedy. This delivery path was
	// the one place that trusted the spoke first, so a spoke provisioned under
	// an old cluster ID kept reporting it forever and the two paths split:
	// the operator uploaded the key exactly where the alert said (the hub
	// record's cluster, "hive-oke"), the alert then confirmed "the hub holds a
	// key" — and every beat this function looked the key up under the spoke's
	// stale ID ("oke-260812-8yiz"), found nothing, and delivered nothing.
	// That is how kelly-headwaters stayed keyless AFTER the operator did the
	// documented remedy (#4316/#4323). The spoke's report still serves hives
	// with no SaaS record (bare/BYO spokes).
	clusterID := ""
	if sh != nil {
		clusterID = sh.ClusterID
	}
	if clusterID == "" {
		clusterID = payload.ClusterID
	}
	if clusterID == "" {
		return nil
	}
	if spokeCluster := payload.ClusterID; spokeCluster != "" && spokeCluster != clusterID && s.logger != nil {
		// Say the disagreement out loud: a spoke carrying a stale cluster ID
		// is exactly the state that made key delivery silently undeliverable.
		s.logger.Warn("heartbeat: spoke reports a different cluster than the hub records for it — app-key reconcile follows the hub record",
			"hive_id", payload.HiveID,
			"spoke_cluster_id", spokeCluster,
			"hub_cluster_id", clusterID,
		)
	}
	// Is this hive pinned to public github.com while its cluster defaults to a
	// GHE App? effectiveGitHubBaseURL honours the hive's github_base_url:"public"
	// / "https://github.com" sentinel and returns "" for public; the cluster
	// defaulting to GHE is exactly cluster.GitHubBaseURL != "". When both hold, the
	// cluster's GHE App must NOT be forced onto this hive as its primary key — the
	// same public pin resolveProvisionAppID already honours on the provisioning
	// path. The signal is computed here (where the hive meta and cluster config are
	// both in hand) and threaded into the decision so decideAppKeySync stays a pure,
	// table-testable function.
	hivePublicPinned := false
	if sh != nil {
		if c, ok := s.clusters[clusterID]; ok {
			// "cluster defaults to GHE" is judged on clusterDefaultForge, not the
			// flat github_base_url alone, so a GHE cluster written in the
			// default_forge/forges-map shape is recognised as GHE here too.
			clusterGHEDefault := !isPublicForgeHost(clusterDefaultForge(&c))
			// The hive is public-pinned only when it ELECTED public. Judge that on
			// electedForgeForHive, which reads github_host FIRST (then the
			// github_base_url "public" sentinel) — NOT on effectiveGitHubBaseURL,
			// which reads only github_base_url and returns "" for a GHE hive whose
			// forge lives in github_host with a blank base_url. That blind spot
			// wrongly flagged every GHE hive on a forge-map cluster as public-pinned
			// (its base_url is blank there), suppressing the very GHE identity
			// delivery this reconcile exists to make.
			elected := electedForgeForHive(sh, &c)
			hiveElectedPublic := elected != "" && isPublicForgeHost(elected)
			hivePublicPinned = hiveElectedPublic && clusterGHEDefault
		}
	}
	// The hub does not track installation IDs, and this reconcile is about the
	// KEY. Sending 0 tells the spoke to leave its (already correct) installation
	// ID alone — see the callback in cmd/hive.
	const installationIDUnchanged = 0
	cfg := s.appKeyConfigForHeartbeat(
		payload.HiveID,
		clusterID,
		payload.GitHubAppKeyFingerprint,
		payload.GitHubAppKeyPerHive,
		hivePublicPinned,
		payload.GitHubAppID,
		installationIDUnchanged,
		s.logger,
		sh,
	)
	// Non-convergence back-off (#2496). The reconcile above is idempotent only
	// when the spoke reflects deliveries; a broken sender that reports no
	// fingerprint forever would otherwise pull key material every beat without
	// bound. Track consecutive key deliveries and throttle past the budget —
	// while a beat that needs no key from a spoke reporting a fingerprint is
	// the convergence signal that resets the ledger.
	if cfg == nil || cfg.PrivateKey == "" {
		if strings.TrimSpace(payload.GitHubAppKeyFingerprint) != "" {
			s.noteAppKeyConverged(payload.HiveID)
		}
		// A spoke POSITIVELY reporting it never received an App key, answered
		// with no key: the hive is dead until an operator uploads one, and this
		// beat is the ONLY place that fact is knowable. It used to pass in
		// silence — no log line, no alert — which is how kelly-headwaters sat
		// degraded on key-missing for 8 days (provisioned 2026-08-12, noticed
		// 2026-08-20) while the hub answered nothing every 30 seconds. Said out
		// loud with a remedy, exactly like the placeholder-sentinel warn above
		// it in appKeyConfigForHeartbeat: an undeliverable state must never be
		// the quiet case.
		if s.logger != nil && payload.GitHubAppRequired && strings.TrimSpace(payload.GitHubAppState) == appStateKeyMissingToken {
			s.logger.Warn("heartbeat: spoke reports its github app key was never delivered and the hub holds no key to deliver — hive cannot work until an operator uploads the App key",
				"hive_id", payload.HiveID,
				"cluster_id", clusterID,
				"spoke_app_id", payload.GitHubAppID,
				"remedy", "PUT /api/saas/admin/cluster-app-keys/"+clusterID+" with the App's PEM private key",
			)
		}
		if cfg == nil {
			// Nothing else to deliver: check for the one fault a converged app
			// identity can still hide — a stale installation_id left behind by an
			// app swap that predates the atomic-set rule above (the 2026-08-05
			// deliveries swapped app_id and left each spoke's old installation_id
			// in place). See staleInstallationResetForHeartbeat.
			cfg = s.staleInstallationResetForHeartbeat(payload, sh, clusterID)
		}
		return cfg
	}
	if !s.allowAppKeyDelivery(payload.HiveID) {
		// Suppressed this beat; the WARN in allowAppKeyDelivery names the hive
		// when the throttled delivery does go out. Returning nil (rather than
		// stripping the key) keeps the wire contract simple: nothing rides.
		return nil
	}
	s.noteAppKeyDelivered(payload.HiveID)
	return cfg
}

// Spoke-reported App auth states this hub acts on. They mirror the stable wire
// tokens of github.AppAuthState.String() — declared here as named constants so
// the heartbeat consumers do not depend on a literal typed twice.
const (
	// spokeAppStateNotInstalled is what a spoke reports after a 404 from GitHub
	// on an App endpoint — including POST /app/installations/<id>/access_tokens
	// with an installation_id that does not belong to the configured App.
	spokeAppStateNotInstalled = "not-installed"
	// spokeAppStateWrongInstallation is the spoke's classification when the App
	// authenticated but the installation resolves to a different account.
	spokeAppStateWrongInstallation = "wrong-installation"
)

// staleInstallationResetForHeartbeat repairs the one fault a CONVERGED App
// identity can still hide: an installation_id that belongs to a PREVIOUS App.
//
// An installation is issued for exactly one App. A delivery that swaps a
// spoke's app_id while leaving its installation_id in place (what the
// 2026-08-05 forge deliveries did, before installation_id joined the atomic
// set) leaves a spoke whose every field looks coherent — app_id, slug, urls all
// name one forge — while every token mint dies with
// "POST .../app/installations/<id>/access_tokens: 404 Not Found". On later
// beats the app_id matches what the hub would deliver, so the ordinary
// reconcile sees nothing to do, and the spoke stays degraded forever.
//
// This is the standing, automatic form of the operator's "Reset App" button
// (handleResetApp / RequestedAppReset), gated on POSITIVE evidence only:
//
//   - the spoke reports a NON-ZERO installation_id (zero would make the reset a
//     no-op, and is also the read-back that stops re-delivery — idempotence),
//   - the spoke's own app_id MATCHES the identity the hub resolves for this
//     hive (the app is converged; the installation is the last stale piece —
//     an app-id mismatch is the wrong-forge/mismatch repair's job, which now
//     carries its own reset),
//   - the spoke POSITIVELY reports failing App auth: github_app_required is
//     raised AND its classified state is not-installed / wrong-installation —
//     the two classifications a stale installation produces (404 / wrong
//     account). A healthy spoke clears the flag on its next successful write,
//     and silence (an old spoke reporting neither field) never triggers.
//   - the hive is CLAIMED and no operator flow owns the field: an armed
//     RequestedAppReset already delivers this, and an in-flight forge switch
//     (RequestedGitHubHost) owns the whole identity.
//
// Clearing the installation makes HasUsableApp() false on the spoke, raising
// the "Install GitHub App / Set ID" banner and starting the RediscoverAndAdopt
// self-heal ticker, which adopts the correct installation automatically when
// the delivered App is already installed on the org.
func (s *HubServer) staleInstallationResetForHeartbeat(payload *HeartbeatPayload, sh *SaaSHive, clusterID string) *HeartbeatGitHubAppConfig {
	if s == nil || payload == nil {
		return nil
	}
	if payload.GitHubInstallationID == 0 || payload.GitHubAppID == 0 {
		return nil
	}
	if !hiveClaimed(sh) {
		return nil
	}
	if sh.RequestedAppReset || sh.RequestedGitHubHost != "" {
		return nil // an operator flow owns this field right now
	}
	if !payload.GitHubAppRequired {
		return nil
	}
	state := strings.TrimSpace(payload.GitHubAppState)
	if state != spokeAppStateNotInstalled && state != spokeAppStateWrongInstallation {
		return nil
	}
	identity := s.appIdentityForHive(sh, clusterID)
	if identity == nil || identity.AppID == 0 || identity.AppID != payload.GitHubAppID {
		return nil // not converged on the hub's answer — other repairs own this
	}
	if s.logger != nil {
		s.logger.Warn("heartbeat: spoke's app identity is converged but its installation_id cannot mint tokens — resetting it so the install/rediscover flow re-establishes it",
			"hive_id", payload.HiveID,
			"cluster_id", clusterID,
			"app_id", payload.GitHubAppID,
			"stale_installation_id", payload.GitHubInstallationID,
			"spoke_app_state", state,
		)
	}
	return &HeartbeatGitHubAppConfig{ResetInstallation: true}
}
