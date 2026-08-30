package hub

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/kubestellar/hive/pkg/config"
)

// Per-App key storage for a cluster's SECOND (optional) GitHub App.
//
// WHY THIS EXISTS
//
// clusterAppKeyPath keys the store by cluster ID alone — one <clusterID>.pem —
// so a cluster has exactly one slot for App key material. Uploading a second
// App's key through the operator endpoint therefore OVERWROTE the Hive App key
// for that cluster, and every spoke on it lost App auth on the next reconcile.
// That is the bug #4815 exists to prevent; the optional Visual Hive App (#4030)
// has no correct place to put its credential today.
//
// WHY THE PRIMARY PATH IS NOT TOUCHED
//
// ~55 live hives across two clusters authenticate from keys already sitting at
// /data/saas/app-keys/<clusterID>.pem. Renaming, moving or "normalising" that
// path is a fleet-wide auth outage for zero benefit: the primary App's key is
// still exactly one per cluster and the existing path already names it
// unambiguously. The second App's key therefore lands ALONGSIDE it under a new
// name, and there is no on-disk migration of any kind — a hub rolled back to a
// build without this file finds every primary key exactly where it left it, and
// simply ignores the extra files.
//
// WHY NOT A SUBDIRECTORY PER CLUSTER
//
// A sibling filename keeps the store a flat directory of PEMs, which is what
// storeClusterAppKey's atomic temp-then-rename assumes (the temp file must land
// on the same filesystem as its target, and CreateTemp is given
// clusterAppKeyDir). A per-cluster subdirectory would need its own MkdirAll,
// its own 0700 enforcement per cluster, and would make a cluster ID a directory
// name — a strictly larger traversal surface for no gain.

// secondaryAppKeyInfix separates the cluster ID from the App ID in a secondary
// key's filename: <clusterID>-app-<appID>.pem.
//
// The literal "-app-" (rather than a bare "-") is deliberate. Cluster IDs
// legitimately contain "-" and end in digits (oke-260812-8yiz), so a bare
// separator would make "<a>-<b>.pem" ambiguous between a secondary key for
// cluster <a> and a PRIMARY key for a cluster literally named "<a>-<b>". Since
// the character allowlist below rejects nothing that would let a real cluster ID
// contain "-app-<digits>" at the very end… it could in principle, so the
// ambiguity is resolved the only way that is actually safe: this store NEVER
// scans filenames to recover a cluster ID. Every read is a lookup by an
// (clusterID, appID) pair the caller already holds, and the filename is only
// ever CONSTRUCTED, never parsed. The infix exists purely to keep secondary
// files visually distinguishable from primary ones for a human reading `ls`.
const secondaryAppKeyInfix = "-app-"

// secondaryAppKeyPath returns the on-disk path of a cluster's key for an App
// that is NOT that cluster's primary App.
//
// It applies exactly the same guards as clusterAppKeyPath — the character
// allowlist, the explicit "."/".." rejection — because the cluster ID reaches
// the filesystem from the same untrusted sources. The App ID is a NEW component
// of the path and therefore a new traversal surface: it is accepted only as a
// positive int64 and rendered back with strconv, so the filename can only ever
// contain decimal digits. A string App ID concatenated straight into the name
// would let "../../etc/x" out of the directory; there is deliberately no
// string-typed entry point to this function.
//
// Returns ok=false — never an error — for the same reason clusterAppKeyPath
// does: "this names no key file" is an ordinary state that every caller already
// handles as "no key", not a fault to propagate.
func secondaryAppKeyPath(clusterID string, appID int64) (string, bool) {
	if appID <= 0 {
		return "", false
	}
	// Reuse the primary path builder for the cluster-ID half so the two can
	// never drift: whatever cluster IDs are legal for the primary key are
	// exactly the ones legal here, validated by the same code.
	primary, ok := clusterAppKeyPath(clusterID)
	if !ok {
		return "", false
	}
	id := strings.TrimSuffix(filepath.Base(primary), ".pem")
	name := id + secondaryAppKeyInfix + strconv.FormatInt(appID, 10) + ".pem"
	return filepath.Join(clusterAppKeyDir, name), true
}

// loadSecondaryAppKey returns the PEM stored for (cluster, App), or "" when
// there is none. Mirrors loadClusterAppKey exactly, including the "-----BEGIN"
// sanity check: a truncated file must never be delivered to a spoke, where it
// would replace a working key with one that cannot sign.
func loadSecondaryAppKey(clusterID string, appID int64) string {
	path, ok := secondaryAppKeyPath(clusterID, appID)
	if !ok {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	pem := strings.TrimSpace(string(data))
	if !strings.HasPrefix(pem, "-----BEGIN") {
		return ""
	}
	return pem
}

// storeSecondaryAppKey writes a cluster's key for a non-primary App.
//
// It refuses to write when appID is the cluster's PRIMARY App. That guard is
// the point of this whole change: without it, a "second App" upload naming the
// primary App by mistake would create a shadow copy of the primary key at a
// second path, and the two would silently diverge on the next primary rotation
// — one of them stale, with nothing to say which. The caller (the upload
// handler) rejects that case with a clear error before reaching here; this is
// the defence in depth for any future caller.
//
// The write itself is the same atomic temp-then-rename at the same modes as
// storeClusterAppKey — shared through storeAppKeyAtPath so the two can never
// acquire different security properties.
func storeSecondaryAppKey(clusterID string, appID int64, primaryAppID int64, pemData string) error {
	if appID <= 0 {
		return fmt.Errorf("invalid app id for secondary app key store: %d", appID)
	}
	if primaryAppID != 0 && appID == primaryAppID {
		return fmt.Errorf("app %d is cluster %s's primary app: its key belongs at the primary path, not a secondary one", appID, clusterID)
	}
	path, ok := secondaryAppKeyPath(clusterID, appID)
	if !ok {
		return fmt.Errorf("invalid cluster id for secondary app key store: %q", clusterID)
	}
	return storeAppKeyAtPath(path, pemData, fmt.Sprintf("cluster %s app %d", clusterID, appID))
}

// secondaryAppKeyFingerprint returns the non-secret fingerprint of a stored
// secondary key, or "" when there is none.
func secondaryAppKeyFingerprint(clusterID string, appID int64) string {
	pem := loadSecondaryAppKey(clusterID, appID)
	if pem == "" {
		return ""
	}
	fp, err := config.AppKeyFingerprint(pem)
	if err != nil {
		return ""
	}
	return fp
}

// secondaryAppKeysForCluster lists the App IDs a cluster holds a secondary key
// for, ascending.
//
// It scans the store directory and matches on the CONSTRUCTED prefix for this
// cluster, so the ambiguity noted at secondaryAppKeyInfix cannot mis-attribute a
// file: a name only counts when it starts with this cluster's own validated ID
// plus the infix and ends in a parseable positive integer. A cluster with no
// secondary keys — every cluster in the fleet today — returns nil and touches
// nothing else.
func secondaryAppKeysForCluster(clusterID string) []int64 {
	primary, ok := clusterAppKeyPath(clusterID)
	if !ok {
		return nil
	}
	id := strings.TrimSuffix(filepath.Base(primary), ".pem")
	prefix := id + secondaryAppKeyInfix
	entries, err := os.ReadDir(clusterAppKeyDir)
	if err != nil {
		// A missing store directory is the ordinary state on a hub that has
		// never had a key uploaded. Never an error.
		return nil
	}
	var out []int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".pem") {
			continue
		}
		digits := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".pem")
		appID, convErr := strconv.ParseInt(digits, 10, 64)
		if convErr != nil || appID <= 0 {
			continue
		}
		// Only a name this store would itself have produced counts. A file
		// named with leading zeros or padding parses as an integer but is not
		// one of ours, and reading it would attribute a key to an App whose
		// canonical path is a different file.
		if canonical, pathOK := secondaryAppKeyPath(clusterID, appID); !pathOK || filepath.Base(canonical) != name {
			continue
		}
		out = append(out, appID)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// secondaryAppKeyForHive resolves the secondary App key this HIVE is authorized
// to receive, or ("", 0) when it is authorized for none.
//
// AUTHORIZATION, NOT DISCOVERY — this is the whole security contract of #4815.
//
// The removed AdditionalKeys broadcast (heartbeat.go, CWE-200/639) selected keys
// from the FLEET key set with no binding to the caller, so any hive holding the
// fleet-shared bearer could pull every tenant's App private key by beating with
// any hive_id. This function never enumerates the fleet. It answers only from
// the hub's OWN record for one hive — the SaaSHive the caller resolved from the
// authenticated identity — and returns a key only when that record explicitly
// names the App (SecondaryAppID) AND the key is stored under that hive's own
// cluster. A hive with no SecondaryAppID (every hive in the fleet today) gets
// nothing, and there is no input a spoke can supply that changes the answer:
// the App ID comes from the hub record, never from the payload.
//
// Returning the cluster ID's key rather than a fleet-wide lookup by app_id is
// deliberate too: appKeysByAppID would happily hand back another tenant's key
// for the same App ID, which is the broadcast bug in miniature.
func secondaryAppKeyForHive(h *SaaSHive) (pem string, appID int64) {
	if h == nil || h.SecondaryAppID <= 0 {
		return "", 0
	}
	clusterID := strings.TrimSpace(h.ClusterID)
	if clusterID == "" {
		return "", 0
	}
	key := loadSecondaryAppKey(clusterID, h.SecondaryAppID)
	if key == "" {
		return "", 0
	}
	return key, h.SecondaryAppID
}

// secondaryAppAssignmentRequest is the admin body for assigning (or clearing) a
// hive's optional second App.
type secondaryAppAssignmentRequest struct {
	// AppID names the second App this hive may hold a key for. Zero CLEARS the
	// assignment — an explicit, reversible operation, so an operator can undo a
	// mistake without editing meta.json by hand.
	AppID int64 `json:"app_id"`
}

// handleSetHiveSecondaryApp assigns a hive its optional second GitHub App:
// PUT /api/saas/hives/{id}/secondary-app
//
// This is the ONLY way SecondaryAppID is ever written, and it is admin-gated for
// the same reason the key upload is: the field is an authorization decision, so
// anything that can set it can direct key material at a hive.
//
// It carries NO key material in either direction — it names an App by its
// non-secret numeric ID. The key itself reaches the hub only through
// PUT /api/saas/admin/cluster-app-keys/{clusterID} with for_app_id, and reaches
// the spoke only through the targeted heartbeat delivery. Keeping the two
// separate means assigning an App a hive has no key for is harmless: the
// delivery decision simply finds no key and does nothing.
//
// Deliberately does NOT touch githubAppRequired / githubAppState. A hive with an
// unassigned, unassigned-but-keyless, or undelivered second App is a perfectly
// ordinary healthy hive — an OPTIONAL App must never be able to red one.
func (s *HubServer) handleSetHiveSecondaryApp(w http.ResponseWriter, r *http.Request) {
	if !isHubAdmin(s.getAuthUser(r)) {
		http.Error(w, `{"error":"admin access required"}`, http.StatusForbidden)
		return
	}
	hiveID := r.PathValue("id")
	h := loadSaaSHive(hiveID)
	if h == nil {
		http.Error(w, `{"error":"hive not found"}`, http.StatusNotFound)
		return
	}
	var body secondaryAppAssignmentRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxSecondaryAppAssignmentBytes)).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid JSON body"}`, http.StatusBadRequest)
		return
	}
	if body.AppID < 0 {
		http.Error(w, `{"error":"app_id must be a positive github app id, or 0 to clear"}`, http.StatusBadRequest)
		return
	}
	// The second App must not BE the App this hive authenticates as. Allowing it
	// would make one App both primary and secondary for the hive, so two
	// independent delivery lanes would write the same /data/gh-app-key-<id>.pem
	// and the last beat would decide which key the hive signs with.
	//
	// Judged against ResolveHiveIdentity, never against the cluster record's
	// github_app_id. A cluster hosts hives of BOTH forges, so the cluster
	// default is not necessarily the App any one hive authenticates as — a
	// public-elected hive on a GHE-default cluster resolves the PUBLIC App. Read
	// from the cluster directly, this guard would refuse the wrong App ID for
	// exactly those hives and permit the one that actually collides. It is also
	// the single-resolver rule the fleet already enforces
	// (TestNoCallSiteResolvesIdentityIndependently): provisioning, the heartbeat
	// answer and this check must not be able to give three different answers.
	if body.AppID != 0 {
		var cluster *ClusterConfig
		if c, ok := s.clusters[strings.TrimSpace(h.ClusterID)]; ok {
			cluster = &c
		}
		if id := ResolveHiveIdentity(h, cluster); id.AppID != 0 && body.AppID == id.AppID {
			http.Error(w, fmt.Sprintf(
				`{"error":"app %d is the app this hive already authenticates as and cannot also be its secondary app"}`,
				body.AppID), http.StatusConflict)
			return
		}
	}
	h.SecondaryAppID = body.AppID
	if err := saveSaaSHive(h); err != nil {
		s.logger.Error("failed to set hive secondary app", "hive_id", hiveID, "error", err)
		http.Error(w, `{"error":"could not record the assignment"}`, http.StatusInternalServerError)
		return
	}
	s.logger.Info("audit: hive secondary github app assignment changed",
		"hive_id", hiveID,
		"cluster_id", h.ClusterID,
		"secondary_app_id", body.AppID,
		"by", s.getAuthUser(r),
	)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"hive_id":          hiveID,
		"secondary_app_id": h.SecondaryAppID,
		// Fingerprint only, so an operator can confirm the hub holds a key for
		// the App they just assigned without the key ever being readable.
		"secondary_key_fingerprint": secondaryAppKeyFingerprint(strings.TrimSpace(h.ClusterID), h.SecondaryAppID),
	})
}

// maxSecondaryAppAssignmentBytes caps this body. It carries one integer; 4 KiB
// is far more than any legitimate request needs and refuses to buffer an
// arbitrary payload into memory.
const maxSecondaryAppAssignmentBytes = 4 << 10

// secondaryAppKeyDeliveryDecision is the outcome of comparing what a spoke
// reports holding against the secondary key the hub believes it should have.
//
// It is a SEPARATE type from appKeySyncDecision on purpose. appKeySyncDecision
// governs the PRIMARY reconcile that ~55 live hives depend on every 2 minutes;
// adding fields or branches to it to carry a second App would put an optional,
// zero-hive feature inside the one function whose behaviour must stay
// byte-identical. Keeping them apart means decideSecondaryAppKeySync cannot
// change a single primary decision, which is a property a test can assert.
type secondaryAppKeyDeliveryDecision struct {
	// Deliver is true when the hub should attach this secondary key.
	Deliver bool
	// AppID is the App the key signs for; zero when nothing is delivered.
	AppID int64
	// Reason explains the decision for the audit log. Never contains key
	// material.
	Reason string
	// ToFingerprint is the non-secret fingerprint of the key being delivered.
	ToFingerprint string
	// HeldFingerprint is what the spoke reported holding for AppID, "" when it
	// reported nothing.
	HeldFingerprint string
}

// secondaryAppKeyForHeartbeat is the heartbeat entry point for optional
// second-App key delivery. It returns nil — attach nothing, change nothing —
// for every hive that is not assigned a second App, which is every hive in the
// fleet today.
//
// The hive record is loaded from the payload's HiveID exactly as every other
// heartbeat path does. That is the AUTHENTICATED hive identity as this handler
// knows it, and the identity the authorization is resolved against: the App ID
// comes from the loaded record, never from the payload, so a caller asserting
// another hive's ID receives that hive's assignment — which is no better than
// what the primary reconcile already hands it — and can never enumerate the
// fleet's keys the way the removed AdditionalKeys broadcast allowed. The
// heartbeat's own hive_id trust problem is a separate, pre-existing issue
// (documented on AdditionalKeys); this path deliberately does not widen it.
func (s *HubServer) secondaryAppKeyForHeartbeat(payload *HeartbeatPayload) *HeartbeatAppKey {
	if s == nil || payload == nil {
		return nil
	}
	h := loadSaaSHive(payload.HiveID)
	if h == nil || h.SecondaryAppID <= 0 {
		return nil
	}
	pem, appID := secondaryAppKeyForHive(h)
	decision := decideSecondaryAppKeySync(pem, appID, payload.GitHubAppKeysHeld)
	if !decision.Deliver {
		return nil
	}
	if s.logger != nil {
		// Fingerprints only. The key itself is never logged, at any level.
		s.logger.Info("heartbeat: delivering secondary github app key to spoke",
			"hive_id", payload.HiveID,
			"cluster_id", h.ClusterID,
			"secondary_app_id", decision.AppID,
			"reason", decision.Reason,
			"held_fingerprint", decision.HeldFingerprint,
			"to_fingerprint", decision.ToFingerprint,
		)
	}
	return &HeartbeatAppKey{
		AppID:       decision.AppID,
		PrivateKey:  pem,
		Fingerprint: decision.ToFingerprint,
	}
}

const (
	secondaryReasonNoAssignment = "hive is not assigned a secondary app"
	secondaryReasonNoKey        = "hub holds no key for the hive's secondary app"
	secondaryReasonHeld         = "spoke already holds this secondary app key"
	secondaryReasonDeliver      = "spoke is missing or holds a stale secondary app key"
)

// decideSecondaryAppKeySync mirrors decideAppKeySync's fingerprint-comparison
// shape — a key the spoke already holds is never re-pushed — so a steady-state
// hive with a second App produces no key traffic at all after the first
// delivery.
//
// heldFingerprints is the spoke's own GitHubAppKeysHeld report (app_id decimal
// string → fingerprint). It is used ONLY to suppress a redundant re-push; it can
// never cause a key to be delivered that would not otherwise be, so a spoke
// cannot influence which key it receives by lying about what it holds. That is
// the distinction that made the old broadcast unsafe and makes this safe.
func decideSecondaryAppKeySync(keyPEM string, appID int64, heldFingerprints map[string]string) secondaryAppKeyDeliveryDecision {
	if appID <= 0 {
		return secondaryAppKeyDeliveryDecision{Reason: secondaryReasonNoAssignment}
	}
	if strings.TrimSpace(keyPEM) == "" {
		return secondaryAppKeyDeliveryDecision{AppID: appID, Reason: secondaryReasonNoKey}
	}
	fp, err := config.AppKeyFingerprint(keyPEM)
	if err != nil || fp == "" {
		// Unfingerprintable means uncomparable, which means it would be pushed
		// on every single beat forever. Same refusal as storeClusterAppKey's.
		return secondaryAppKeyDeliveryDecision{AppID: appID, Reason: secondaryReasonNoKey}
	}
	held := heldFingerprints[strconv.FormatInt(appID, 10)]
	if strings.TrimSpace(held) == fp {
		return secondaryAppKeyDeliveryDecision{
			AppID:           appID,
			Reason:          secondaryReasonHeld,
			ToFingerprint:   fp,
			HeldFingerprint: held,
		}
	}
	return secondaryAppKeyDeliveryDecision{
		Deliver:         true,
		AppID:           appID,
		Reason:          secondaryReasonDeliver,
		ToFingerprint:   fp,
		HeldFingerprint: strings.TrimSpace(held),
	}
}
