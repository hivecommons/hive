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

	"github.com/kubestellar/hive/v2/pkg/config"
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
	trimmed := strings.TrimSpace(pemData)
	if !strings.HasPrefix(trimmed, "-----BEGIN") {
		return fmt.Errorf("app key for cluster %s is not PEM", clusterID)
	}
	// Validate before persisting: a key we cannot fingerprint is a key we could
	// never compare against a spoke's report, so it would be pushed forever.
	if _, err := config.AppKeyFingerprint(trimmed); err != nil {
		return fmt.Errorf("app key for cluster %s is unusable: %w", clusterID, err)
	}

	if err := os.MkdirAll(clusterAppKeyDir, clusterAppKeyDirMode); err != nil {
		return fmt.Errorf("create app key dir: %w", err)
	}

	tmp, err := os.CreateTemp(clusterAppKeyDir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("create temp app key file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename below succeeds

	if err := tmp.Chmod(clusterAppKeyFileMode); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp app key file: %w", err)
	}
	if _, err := tmp.WriteString(trimmed + "\n"); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp app key file: %w", err)
	}
	// Sync before rename so the rename cannot publish an empty file after a
	// crash — the key is distributed fleet-wide, so a partial write is worse
	// than no write.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
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
	AppID       int64
	AppSlug     string
	PrivateKey  string
	Fingerprint string
}

// appIdentityForCluster resolves the App identity the hub should enforce on a
// cluster. It returns nil when the cluster is unknown, has no configured app_id,
// or has no stored key — all of which mean "this hub has nothing authoritative
// to say about this cluster's App credentials, so change nothing".
func (s *HubServer) appIdentityForCluster(clusterID string) *clusterAppIdentity {
	if s == nil || s.clusters == nil {
		return nil
	}
	c, ok := s.clusters[clusterID]
	if !ok {
		return nil
	}
	if c.GitHubAppID == 0 {
		return nil
	}
	pem := loadClusterAppKey(clusterID)
	if pem == "" {
		return nil
	}
	fp, err := config.AppKeyFingerprint(pem)
	if err != nil {
		return nil
	}
	return &clusterAppIdentity{
		AppID:       c.GitHubAppID,
		AppSlug:     c.GitHubAppSlug,
		PrivateKey:  pem,
		Fingerprint: fp,
	}
}

// appKeySyncDecision is the outcome of comparing a spoke's reported App identity
// against its cluster's authoritative one. Returned rather than acted on inline
// so the decision is table-testable without a filesystem or an HTTP server.
type appKeySyncDecision struct {
	// Push is true when the hub should deliver the cluster key to this spoke.
	Push bool
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
func decideAppKeySync(spokeFingerprint string, hasPerHiveKey bool, cluster *clusterAppIdentity) appKeySyncDecision {
	if cluster == nil {
		return appKeySyncDecision{Reason: appKeyReasonNoClusterKey, FromFingerprint: spokeFingerprint}
	}
	if hasPerHiveKey {
		return appKeySyncDecision{
			Reason:          appKeyReasonPerHiveOverride,
			FromFingerprint: spokeFingerprint,
			ToFingerprint:   cluster.Fingerprint,
		}
	}
	if strings.TrimSpace(spokeFingerprint) == "" {
		return appKeySyncDecision{
			Push:          true,
			Reason:        appKeyReasonSpokeHasNoKey,
			ToFingerprint: cluster.Fingerprint,
		}
	}
	if spokeFingerprint != cluster.Fingerprint {
		return appKeySyncDecision{
			Push:            true,
			Reason:          appKeyReasonMismatch,
			FromFingerprint: spokeFingerprint,
			ToFingerprint:   cluster.Fingerprint,
		}
	}
	return appKeySyncDecision{
		Reason:          appKeyReasonMatch,
		FromFingerprint: spokeFingerprint,
		ToFingerprint:   cluster.Fingerprint,
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
func (s *HubServer) appKeyConfigForHeartbeat(hiveID, clusterID string, spokeFingerprint string, hasPerHiveKey bool, installationID int64, logger *slog.Logger) *HeartbeatGitHubAppConfig {
	identity := s.appIdentityForCluster(clusterID)
	decision := decideAppKeySync(spokeFingerprint, hasPerHiveKey, identity)
	if !decision.Push || identity == nil {
		return nil
	}
	if logger != nil {
		// Fingerprints only. The key itself is never logged, at any level.
		logger.Info("heartbeat: pushing cluster github app key to spoke",
			"hive_id", hiveID,
			"cluster_id", clusterID,
			"app_id", identity.AppID,
			"reason", decision.Reason,
			"from_fingerprint", decision.FromFingerprint,
			"to_fingerprint", decision.ToFingerprint,
		)
	}
	return &HeartbeatGitHubAppConfig{
		AppID:          identity.AppID,
		InstallationID: installationID,
		PrivateKey:     identity.PrivateKey,
		AppSlug:        identity.AppSlug,
	}
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
}

// clusterAppKeyStatus is the read side. It carries the fingerprint and NEVER the
// key — this shape is what any operator UI renders, and the omission is the
// point, not an oversight.
type clusterAppKeyStatus struct {
	ClusterID   string `json:"cluster_id"`
	AppID       int64  `json:"app_id,omitempty"`
	HasKey      bool   `json:"has_key"`
	Fingerprint string `json:"fingerprint,omitempty"`
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
			ClusterID:   id,
			AppID:       c.GitHubAppID,
			HasKey:      fp != "",
			Fingerprint: fp,
		})
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].ClusterID < statuses[j].ClusterID })
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(statuses)
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
	if err := storeClusterAppKey(clusterID, key); err != nil {
		s.logger.Error("failed to store cluster app key", "cluster_id", clusterID, "error", err)
		http.Error(w, `{"error":"failed to store key"}`, http.StatusInternalServerError)
		return
	}

	// Persist the app_id alongside so the cluster has a complete identity. The
	// ID is not secret and belongs in clusters.json.
	appID := s.clusters[clusterID].GitHubAppID
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
	json.NewEncoder(w).Encode(clusterAppKeyStatus{
		ClusterID:   clusterID,
		AppID:       appID,
		HasKey:      true,
		Fingerprint: fp,
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
		os.Remove(tmp)
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
	clusterID := payload.ClusterID
	if clusterID == "" {
		// The spoke did not report a cluster. Fall back to the hub's own record
		// for this hive rather than guessing the default cluster: guessing would
		// aim a GitHub Enterprise key at a public-GitHub hive.
		if sh := loadSaaSHive(payload.HiveID); sh != nil {
			clusterID = sh.ClusterID
		}
	}
	if clusterID == "" {
		return nil
	}
	// The hub does not track installation IDs, and this reconcile is about the
	// KEY. Sending 0 tells the spoke to leave its (already correct) installation
	// ID alone — see the callback in cmd/hive.
	const installationIDUnchanged = 0
	return s.appKeyConfigForHeartbeat(
		payload.HiveID,
		clusterID,
		payload.GitHubAppKeyFingerprint,
		payload.GitHubAppKeyPerHive,
		installationIDUnchanged,
		s.logger,
	)
}
