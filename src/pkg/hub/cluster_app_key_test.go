package hub

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// testRSAKeyBits matches the smallest size GitHub Apps actually use. These keys
// never sign anything real — they only exercise fingerprint comparison.
const testRSAKeyBits = 2048

func testAppKeyPEM(t *testing.T) string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, testRSAKeyBits)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(k),
	}))
}

// withTempAppKeyDir redirects the cluster key store at a temp dir for the
// duration of a test and restores it afterwards.
func withTempAppKeyDir(t *testing.T) string {
	t.Helper()
	orig := clusterAppKeyDir
	dir := t.TempDir()
	clusterAppKeyDir = dir
	t.Cleanup(func() { clusterAppKeyDir = orig })
	return dir
}

func appKeyTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// --- decideAppKeySync: the core state machine ---

// TestDecideAppKeySync is the table the incident maps onto directly:
// vllmd-01..03 report a WRONG fingerprint, vllmd-06..09 report NONE, and osscar
// reports the MATCHING one. Only the first two groups may receive a push.
func TestDecideAppKeySync(t *testing.T) {
	const (
		clusterFP = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		wrongFP   = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	identity := &clusterAppIdentity{
		AppID:       5686,
		PrivateKey:  "-----BEGIN RSA PRIVATE KEY-----\nx\n-----END RSA PRIVATE KEY-----",
		Fingerprint: clusterFP,
	}

	tests := []struct {
		name         string
		spokeFP      string
		perHive      bool
		publicPinned bool
		clusterIsGHE bool
		spokeAppID   int64
		cluster      *clusterAppIdentity
		wantPush     bool
		wantReason   string
		wantFromFP   string
		wantToFP     string
		description  string
	}{
		{
			name:        "fingerprint matches - no push",
			spokeFP:     clusterFP,
			cluster:     identity,
			wantPush:    false,
			wantReason:  appKeyReasonMatch,
			wantFromFP:  clusterFP,
			wantToFP:    clusterFP,
			description: "osscar: already correct, must not re-send the credential",
		},
		{
			name:        "fingerprint differs - push",
			spokeFP:     wrongFP,
			cluster:     identity,
			wantPush:    true,
			wantReason:  appKeyReasonMismatch,
			wantFromFP:  wrongFP,
			wantToFP:    clusterFP,
			description: "vllmd-01..03: carrying the public github.com key",
		},
		{
			name:        "spoke reports no key - push",
			spokeFP:     "",
			cluster:     identity,
			wantPush:    true,
			wantReason:  appKeyReasonSpokeHasNoKey,
			wantToFP:    clusterFP,
			description: "vllmd-06..09: provisioned with no key at all",
		},
		{
			name:        "whitespace-only fingerprint counts as absent",
			spokeFP:     "   ",
			cluster:     identity,
			wantPush:    true,
			wantReason:  appKeyReasonSpokeHasNoKey,
			wantToFP:    clusterFP,
			description: "a blank report must never be read as a real fingerprint",
		},
		{
			name:        "cluster has no key - no-op",
			spokeFP:     wrongFP,
			cluster:     nil,
			wantPush:    false,
			wantReason:  appKeyReasonNoClusterKey,
			wantFromFP:  wrongFP,
			description: "a cluster the hub holds no key for must be left entirely alone",
		},
		{
			name:        "cluster has no key and spoke has none either - still no-op",
			spokeFP:     "",
			cluster:     nil,
			wantPush:    false,
			wantReason:  appKeyReasonNoClusterKey,
			description: "nothing to push means nothing happens, not an empty push",
		},
		{
			name:        "per-hive key, spoke app_id unknown - no push",
			spokeFP:     wrongFP,
			perHive:     true,
			spokeAppID:  0,
			cluster:     identity,
			wantPush:    false,
			wantReason:  appKeyReasonPerHiveOverride,
			wantFromFP:  wrongFP,
			wantToFP:    clusterFP,
			description: "a spoke too old to report app_id must not be assumed broken",
		},
		{
			name:        "per-hive key with no key reported still wins",
			spokeFP:     "",
			perHive:     true,
			cluster:     identity,
			wantPush:    false,
			wantReason:  appKeyReasonPerHiveOverride,
			wantToFP:    clusterFP,
			description: "no fingerprint is no proof of a wrong key; stay conservative",
		},

		// --- THE FIX: a per-hive key that provably cannot work ---
		{
			name:        "per-hive key wrong for the app_id the hive claims - PUSH",
			spokeFP:     wrongFP,
			perHive:     true,
			spokeAppID:  5686,
			cluster:     identity,
			wantPush:    true,
			wantReason:  appKeyReasonPerHiveUnusable,
			wantFromFP:  wrongFP,
			wantToFP:    clusterFP,
			description: "vllmd-01..03: claims GHE app 5686 but holds the public github.com key — cannot sign a valid JWT, so this is a fault and not a choice",
		},
		{
			name:        "per-hive key for a deliberately DIFFERENT app - no push",
			spokeFP:     wrongFP,
			perHive:     true,
			spokeAppID:  99999,
			cluster:     identity,
			wantPush:    false,
			wantReason:  appKeyReasonDifferentApp,
			wantFromFP:  wrongFP,
			wantToFP:    clusterFP,
			description: "a hive pinned to its own App keeps its own key; the cluster key would break it",
		},
		// --- WRONG-FORGE APP: a cross-forge app_id is a fault, not a pin ---
		{
			name:         "GHE cluster, spoke carries the github.com app - PUSH",
			spokeFP:      wrongFP,
			perHive:      true,
			clusterIsGHE: true,
			spokeAppID:   3568013, // the public github.com App
			cluster:      identity,
			wantPush:     true,
			wantReason:   appKeyReasonWrongForgeApp,
			wantFromFP:   wrongFP,
			wantToFP:     clusterFP,
			description:  "vllmd-03 (Enrico): app_id 3568013 names no App on github.ibm.com, so the per-hive shield must not protect it",
		},
		{
			name:         "GHE cluster, spoke app_id unknown (0) - no push",
			spokeFP:      wrongFP,
			perHive:      true,
			clusterIsGHE: true,
			spokeAppID:   0,
			cluster:      identity,
			wantPush:     false,
			wantReason:   appKeyReasonPerHiveOverride,
			wantFromFP:   wrongFP,
			wantToFP:     clusterFP,
			description:  "a spoke too old to report its app_id is silent, and silence is never evidence of a cross-forge fault",
		},
		{
			name:         "GHE cluster, spoke already on the cluster app - no wrong-forge push",
			spokeFP:      clusterFP,
			perHive:      true,
			clusterIsGHE: true,
			spokeAppID:   5686,
			cluster:      identity,
			wantPush:     false,
			wantReason:   appKeyReasonMatch,
			wantFromFP:   clusterFP,
			wantToFP:     clusterFP,
			description:  "idempotence: once the spoke carries the GHE App the repair stops firing",
		},
		{
			name:         "public cluster keeps protecting a deliberate different app",
			spokeFP:      wrongFP,
			perHive:      true,
			clusterIsGHE: false,
			spokeAppID:   99999,
			cluster:      identity,
			wantPush:     false,
			wantReason:   appKeyReasonDifferentApp,
			wantFromFP:   wrongFP,
			wantToFP:     clusterFP,
			description:  "same-forge pins stay protected; the repair is scoped to cross-forge clusters only",
		},
		{
			name:         "public-pinned hive on a GHE cluster is never wrong-forge repaired",
			spokeFP:      wrongFP,
			perHive:      true,
			publicPinned: true,
			clusterIsGHE: true,
			spokeAppID:   3568013,
			cluster:      identity,
			wantPush:     false,
			wantReason:   appKeyReasonPublicHiveOnGHECluster,
			wantFromFP:   wrongFP,
			wantToFP:     clusterFP,
			description:  "a hive deliberately pinned to public github.com must keep its github.com App even on a GHE cluster",
		},
		{
			name:        "per-hive key already matches the cluster key - no push",
			spokeFP:     clusterFP,
			perHive:     true,
			spokeAppID:  5686,
			cluster:     identity,
			wantPush:    false,
			wantReason:  appKeyReasonMatch,
			wantFromFP:  clusterFP,
			wantToFP:    clusterFP,
			description: "idempotence: once repaired the push must stop, per-hive flag or not",
		},
		{
			name:        "per-hive, matching app_id, no fingerprint reported - no push",
			spokeFP:     "",
			perHive:     true,
			spokeAppID:  5686,
			cluster:     identity,
			wantPush:    false,
			wantReason:  appKeyReasonPerHiveOverride,
			wantToFP:    clusterFP,
			description: "the exception needs a fingerprint that provably differs, not an absent one",
		},
		{
			name:       "cluster app_id unset cannot make a positive match",
			spokeFP:    wrongFP,
			perHive:    true,
			spokeAppID: 5686,
			// Carries real key material (as any cluster with a stored PEM does)
			// so the case under test is the missing app_id alone, not a
			// keyless identity — those are now distinct states.
			cluster:     &clusterAppIdentity{AppID: 0, PrivateKey: identity.PrivateKey, Fingerprint: clusterFP},
			wantPush:    false,
			wantReason:  appKeyReasonDifferentApp,
			wantFromFP:  wrongFP,
			wantToFP:    clusterFP,
			description: "no cluster app_id means no proof of a conflict",
		},
		{
			name:        "non-per-hive mismatch on a matching app_id still pushes",
			spokeFP:     wrongFP,
			spokeAppID:  5686,
			cluster:     identity,
			wantPush:    true,
			wantReason:  appKeyReasonMismatch,
			wantFromFP:  wrongFP,
			wantToFP:    clusterFP,
			description: "reporting app_id must not change the pre-existing non-per-hive path",
		},

		// --- THE CLASS FIX: a public github.com hive parked on a GHE cluster ---
		{
			name:         "public-pinned hive on GHE cluster - never push the cluster GHE key as primary",
			spokeFP:      "",
			publicPinned: true,
			spokeAppID:   3568013, // the hive's own public github.com App
			cluster:      identity,
			wantPush:     false,
			wantReason:   appKeyReasonPublicHiveOnGHECluster,
			wantToFP:     clusterFP,
			description:  "kalantar/vllmd-10 etc: github.com hive on vllm-d — its github.com App stays primary, the GHE key rides only as additional",
		},
		{
			name:         "public-pinned hive with the wrong/empty key is still not forced onto the GHE app",
			spokeFP:      wrongFP,
			publicPinned: true,
			spokeAppID:   3568013,
			cluster:      identity,
			wantPush:     false,
			wantReason:   appKeyReasonPublicHiveOnGHECluster,
			wantFromFP:   wrongFP,
			wantToFP:     clusterFP,
			description:  "the public pin wins even before any per-hive/mismatch reasoning: the cluster GHE app is never this hive's primary",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := decideAppKeySync(tc.spokeFP, tc.perHive, tc.publicPinned, tc.clusterIsGHE, tc.spokeAppID, tc.cluster)
			if got.Push != tc.wantPush {
				t.Errorf("Push = %v, want %v (%s)", got.Push, tc.wantPush, tc.description)
			}
			if got.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q", got.Reason, tc.wantReason)
			}
			if got.FromFingerprint != tc.wantFromFP {
				t.Errorf("FromFingerprint = %q, want %q", got.FromFingerprint, tc.wantFromFP)
			}
			if got.ToFingerprint != tc.wantToFP {
				t.Errorf("ToFingerprint = %q, want %q", got.ToFingerprint, tc.wantToFP)
			}
			// No decision may ever carry key material, regardless of outcome.
			for _, field := range []string{got.Reason, got.FromFingerprint, got.ToFingerprint} {
				if strings.Contains(field, "BEGIN") || strings.Contains(field, "PRIVATE KEY") {
					t.Errorf("decision field %q contains key material", field)
				}
			}
		})
	}
}

// TestDecideAppKeySyncIsIdempotent asserts convergence: once the spoke reports
// the fingerprint it was pushed, the next decision is no-push. Without this the
// hub would put a private key on the wire on every single heartbeat.
func TestDecideAppKeySyncIsIdempotent(t *testing.T) {
	keyPEM := testAppKeyPEM(t)
	fp, err := config.AppKeyFingerprint(keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	identity := &clusterAppIdentity{AppID: 5686, PrivateKey: keyPEM, Fingerprint: fp}

	// Beat 1: spoke has nothing → push.
	first := decideAppKeySync("", false, false, false, 0, identity)
	if !first.Push {
		t.Fatal("first beat should push to a spoke with no key")
	}
	// The spoke writes the key; its next report is the fingerprint of what it
	// was given. Compute it the way the spoke would, from the delivered PEM.
	delivered, err := config.AppKeyFingerprint(first.ToFingerprintKeySource(identity))
	if err != nil {
		t.Fatal(err)
	}
	// Beat 2: spoke now reports the delivered key → no push, forever after.
	for beat := 2; beat <= 5; beat++ {
		d := decideAppKeySync(delivered, false, false, false, 0, identity)
		if d.Push {
			t.Fatalf("beat %d re-pushed a key the spoke already holds", beat)
		}
	}
}

// ToFingerprintKeySource returns the PEM the decision would deliver. Test-only
// helper kept next to its single use so production code carries no accessor
// that hands back key material.
func (d appKeySyncDecision) ToFingerprintKeySource(c *clusterAppIdentity) string {
	return c.PrivateKey
}

// --- Key store on disk ---

func TestStoreAndLoadClusterAppKey(t *testing.T) {
	withTempAppKeyDir(t)
	keyPEM := testAppKeyPEM(t)

	if err := storeClusterAppKey("vllm-d", keyPEM); err != nil {
		t.Fatalf("storeClusterAppKey: %v", err)
	}
	got := loadClusterAppKey("vllm-d")
	if strings.TrimSpace(got) != strings.TrimSpace(keyPEM) {
		t.Error("loaded key does not round-trip the stored key")
	}

	wantFP, err := config.AppKeyFingerprint(keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	if gotFP := clusterAppKeyFingerprint("vllm-d"); gotFP != wantFP {
		t.Errorf("clusterAppKeyFingerprint = %q, want %q", gotFP, wantFP)
	}
}

// TestClusterAppKeyFileMode is a hard security requirement: signing material on
// the hub PVC must be readable by nothing but the hub.
func TestClusterAppKeyFileMode(t *testing.T) {
	dir := withTempAppKeyDir(t)
	if err := storeClusterAppKey("hive-oke", testAppKeyPEM(t)); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "hive-oke.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != fs.FileMode(clusterAppKeyFileMode) {
		t.Errorf("key file mode = %#o, want %#o", perm, clusterAppKeyFileMode)
	}
}

func TestStoreClusterAppKeyRejectsBadInput(t *testing.T) {
	withTempAppKeyDir(t)
	tests := []struct {
		name      string
		clusterID string
		key       string
	}{
		{name: "empty cluster id", clusterID: "", key: testAppKeyPEM(t)},
		{name: "path traversal cluster id", clusterID: "../../etc/shadow", key: testAppKeyPEM(t)},
		{name: "dot cluster id", clusterID: ".", key: testAppKeyPEM(t)},
		{name: "dotdot cluster id", clusterID: "..", key: testAppKeyPEM(t)},
		{name: "slash in cluster id", clusterID: "a/b", key: testAppKeyPEM(t)},
		{name: "not a pem key", clusterID: "ok", key: "just a string"},
		{name: "pem envelope with garbage", clusterID: "ok", key: "-----BEGIN RSA PRIVATE KEY-----\nZZZ\n-----END RSA PRIVATE KEY-----"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := storeClusterAppKey(tc.clusterID, tc.key); err == nil {
				t.Fatal("expected storeClusterAppKey to reject this input")
			}
		})
	}
}

func TestLoadClusterAppKeyMissingOrCorrupt(t *testing.T) {
	dir := withTempAppKeyDir(t)

	if got := loadClusterAppKey("never-stored"); got != "" {
		t.Errorf("missing key returned %q, want empty", got)
	}
	if got := loadClusterAppKey("../escape"); got != "" {
		t.Errorf("traversal id returned %q, want empty", got)
	}
	// A truncated file must not be distributed: it would replace working keys
	// across an entire cluster with a broken one.
	if err := os.WriteFile(filepath.Join(dir, "trunc.pem"), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := loadClusterAppKey("trunc"); got != "" {
		t.Errorf("corrupt key returned %q, want empty", got)
	}
	if got := clusterAppKeyFingerprint("trunc"); got != "" {
		t.Errorf("corrupt key fingerprint = %q, want empty", got)
	}
}

// --- appIdentityForCluster wiring ---

func TestAppIdentityForCluster(t *testing.T) {
	withTempAppKeyDir(t)
	keyPEM := testAppKeyPEM(t)
	if err := storeClusterAppKey("vllm-d", keyPEM); err != nil {
		t.Fatal(err)
	}
	// A cluster with a key on disk but no app_id configured, to prove both
	// halves are required.
	if err := storeClusterAppKey("keyonly", keyPEM); err != nil {
		t.Fatal(err)
	}

	s := &HubServer{clusters: map[string]ClusterConfig{
		"vllm-d":   {ID: "vllm-d", GitHubAppID: 5686, GitHubAppSlug: "kubestellar-hive-ghe"},
		"keyonly":  {ID: "keyonly"},
		"hive-oke": {ID: "hive-oke", GitHubAppID: 12345},
	}}

	tests := []struct {
		name      string
		clusterID string
		wantNil   bool
		wantAppID int64
		wantSlug  string
		// wantNoKey asserts the identity names an App but carries NO key
		// material, so HasKey() is false and no key-pushing decision may fire.
		wantNoKey bool
		reason    string
	}{
		{name: "app id and key present", clusterID: "vllm-d", wantAppID: 5686, wantSlug: "kubestellar-hive-ghe"},
		{name: "key but no app id", clusterID: "keyonly", wantNil: true, reason: "an app_id-less cluster has no identity to enforce"},
		// A configured app_id with no stored key IS an identity. Returning nil
		// here is the bug that made #2333's sentinel repair unreachable in
		// production: hive-oke had github_app_id set but no PEM, so every
		// reconcile short-circuited before the repair was consulted. The app_id
		// alone still corrects a sentinel spoke; HasKey() is what gates the
		// separate question of whether key material may be pushed.
		{name: "app id but no key still yields an identity", clusterID: "hive-oke", wantAppID: 12345, wantNoKey: true,
			reason: "app_id alone is authoritative enough to repair a placeholder sentinel"},
		{name: "unknown cluster", clusterID: "nope", wantNil: true, reason: "never invent an identity for an unknown cluster"},
		{name: "empty cluster id", clusterID: "", wantNil: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := s.appIdentityForCluster(tc.clusterID)
			if tc.wantNil {
				if got != nil {
					t.Fatalf("got identity %+v, want nil (%s)", got.AppID, tc.reason)
				}
				return
			}
			if got == nil {
				t.Fatal("got nil identity, want one")
			}
			if got.AppID != tc.wantAppID {
				t.Errorf("AppID = %d, want %d", got.AppID, tc.wantAppID)
			}
			if got.HasKey() == tc.wantNoKey {
				t.Errorf("HasKey() = %v, want %v (%s)", got.HasKey(), !tc.wantNoKey, tc.reason)
			}
			if got.AppSlug != tc.wantSlug {
				t.Errorf("AppSlug = %q, want %q", got.AppSlug, tc.wantSlug)
			}
			// A key-bearing identity must always carry a fingerprint — that pair is
			// what every key-push comparison depends on. A keyless identity
			// legitimately has neither, and asserting one there would re-impose
			// the very "both or nothing" rule that made the sentinel repair
			// unreachable.
			if !tc.wantNoKey && got.Fingerprint == "" {
				t.Error("identity has key material but no fingerprint")
			}
			if tc.wantNoKey && (got.Fingerprint != "" || got.PrivateKey != "") {
				t.Errorf("keyless identity carries key material: fp=%q key_len=%d", got.Fingerprint, len(got.PrivateKey))
			}
		})
	}
}

func TestAppIdentityForClusterNilSafe(t *testing.T) {
	var nilServer *HubServer
	if got := nilServer.appIdentityForCluster("x"); got != nil {
		t.Error("nil server should yield nil identity")
	}
	empty := &HubServer{}
	if got := empty.appIdentityForCluster("x"); got != nil {
		t.Error("server with nil cluster map should yield nil identity")
	}
}

// --- Heartbeat integration ---

func TestAppKeySyncForHeartbeat(t *testing.T) {
	withTempAppKeyDir(t)
	keyPEM := testAppKeyPEM(t)
	if err := storeClusterAppKey("vllm-d", keyPEM); err != nil {
		t.Fatal(err)
	}
	clusterFP, err := config.AppKeyFingerprint(keyPEM)
	if err != nil {
		t.Fatal(err)
	}

	s := &HubServer{
		logger:   appKeyTestLogger(),
		clusters: map[string]ClusterConfig{"vllm-d": {ID: "vllm-d", GitHubAppID: 5686, GitHubAppSlug: "kubestellar-hive-ghe"}},
	}

	tests := []struct {
		name     string
		payload  *HeartbeatPayload
		wantCfg  bool
		wantSlug string
		reason   string
	}{
		{
			name:     "spoke with no key gets the cluster key",
			payload:  &HeartbeatPayload{HiveID: "vllmd-06", ClusterID: "vllm-d"},
			wantCfg:  true,
			wantSlug: "kubestellar-hive-ghe",
			reason:   "the four keyless hives",
		},
		{
			name:     "spoke with the wrong key gets corrected",
			payload:  &HeartbeatPayload{HiveID: "vllmd-01", ClusterID: "vllm-d", GitHubAppKeyFingerprint: "sha256:deadbeefdeadbeefdeadbeefdeadbeef"},
			wantCfg:  true,
			wantSlug: "kubestellar-hive-ghe",
			reason:   "the three hives carrying the public github.com key",
		},
		{
			name:    "spoke already correct is left alone",
			payload: &HeartbeatPayload{HiveID: "osscar", ClusterID: "vllm-d", GitHubAppKeyFingerprint: clusterFP},
			wantCfg: false,
			reason:  "no credential on the wire once it matches",
		},
		{
			name:    "per-hive key is not overwritten",
			payload: &HeartbeatPayload{HiveID: "special", ClusterID: "vllm-d", GitHubAppKeyPerHive: true},
			wantCfg: false,
			reason:  "per-hive beats cluster default",
		},
		{
			name:    "cluster the hub holds no key for",
			payload: &HeartbeatPayload{HiveID: "h", ClusterID: "hive-oke"},
			wantCfg: false,
			reason:  "unknown cluster: change nothing",
		},
		{
			name:    "no cluster reported and no hub record",
			payload: &HeartbeatPayload{HiveID: "orphan"},
			wantCfg: false,
			reason:  "never guess a cluster; guessing aims a GHE key at a public hive",
		},
		{
			name:    "nil payload",
			payload: nil,
			wantCfg: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := s.appKeySyncForHeartbeat(tc.payload)
			if !tc.wantCfg {
				if got != nil {
					t.Fatalf("got a config, want nil (%s)", tc.reason)
				}
				return
			}
			if got == nil {
				t.Fatalf("got nil, want a config (%s)", tc.reason)
			}
			if got.AppID != 5686 {
				t.Errorf("AppID = %d, want 5686", got.AppID)
			}
			// The reconcile must never disturb a correct installation_id, which
			// the hub does not track. Zero is the "leave it alone" signal.
			if got.InstallationID != 0 {
				t.Errorf("InstallationID = %d, want 0 (unchanged)", got.InstallationID)
			}
			if strings.TrimSpace(got.PrivateKey) != strings.TrimSpace(keyPEM) {
				t.Error("delivered key is not the cluster key")
			}
			if got.AppSlug != tc.wantSlug {
				t.Errorf("AppSlug = %q, want %q", got.AppSlug, tc.wantSlug)
			}
		})
	}
}

func TestAppKeySyncForHeartbeatNilServer(t *testing.T) {
	var s *HubServer
	if got := s.appKeySyncForHeartbeat(&HeartbeatPayload{HiveID: "x"}); got != nil {
		t.Error("nil server must yield no config")
	}
}

// TestAppKeySyncForHeartbeatPublicPinnedHive is the end-to-end class fix: a
// github.com hive (github_base_url:"public") parked on the vllm-d GHE cluster
// must NEVER be handed the cluster's GHE App (5686) as its primary key, while a
// genuine GHE hive on the same cluster still is. (Post-C1/N3 the heartbeat no
// longer also broadcasts the GHE key to the public hive as an additional key;
// that cross-tenant delivery lane was removed — see both_app_keys_test.go.)
func TestAppKeySyncForHeartbeatPublicPinnedHive(t *testing.T) {
	withTempAppKeyDir(t)
	oldHives := saasHivesDir
	saasHivesDir = t.TempDir()
	t.Cleanup(func() { saasHivesDir = oldHives })

	keyPEM := testAppKeyPEM(t)
	if err := storeClusterAppKey("vllm-d", keyPEM); err != nil {
		t.Fatal(err)
	}

	// vllm-d defaults to a GHE App: GitHubBaseURL is a GHE host, app_id 5686.
	s := &HubServer{
		logger: appKeyTestLogger(),
		clusters: map[string]ClusterConfig{
			"vllm-d": {
				ID:            "vllm-d",
				GitHubAppID:   5686,
				GitHubAppSlug: "kubestellar-hive-ghe",
				GitHubBaseURL: "https://github.ibm.com",
			},
		},
	}

	// A github.com hive pinned public, parked on the GHE cluster (kalantar's case).
	if err := saveSaaSHive(&SaaSHive{
		ID:            "kalantar-vllmd-10",
		ClusterID:     "vllm-d",
		GitHubBaseURL: githubHostPublic,
	}); err != nil {
		t.Fatal(err)
	}
	// A genuine GHE hive on the same cluster: no public pin, inherits the default.
	if err := saveSaaSHive(&SaaSHive{
		ID:        "genuine-ghe-hive",
		ClusterID: "vllm-d",
	}); err != nil {
		t.Fatal(err)
	}

	t.Run("public-pinned hive is NOT forced onto the cluster GHE app", func(t *testing.T) {
		// Reports no key and app_id 3568013 (its own github.com App) — the exact
		// state that previously got 5686 pushed as primary every beat.
		got := s.appKeySyncForHeartbeat(&HeartbeatPayload{
			HiveID:      "kalantar-vllmd-10",
			ClusterID:   "vllm-d",
			GitHubAppID: 3568013,
		})
		if got != nil {
			t.Fatalf("public-pinned hive got a primary key config (app_id %d); the cluster GHE app must never be forced as primary", got.AppID)
		}
	})

	t.Run("genuine GHE hive on the same cluster still gets the GHE key", func(t *testing.T) {
		got := s.appKeySyncForHeartbeat(&HeartbeatPayload{
			HiveID:    "genuine-ghe-hive",
			ClusterID: "vllm-d",
		})
		if got == nil {
			t.Fatal("a genuine GHE hive with no key must still receive the cluster GHE key")
		}
		if got.AppID != 5686 {
			t.Errorf("GHE hive primary app_id = %d, want 5686", got.AppID)
		}
	})

	t.Run("public pin honoured even when the spoke does not report a cluster", func(t *testing.T) {
		// Cluster ID falls back to the hive's own meta record; the public pin must
		// still be read from that same record.
		got := s.appKeySyncForHeartbeat(&HeartbeatPayload{
			HiveID:      "kalantar-vllmd-10",
			GitHubAppID: 3568013,
		})
		if got != nil {
			t.Fatalf("public-pinned hive (cluster from meta) got app_id %d as primary, want none", got.AppID)
		}
	})
}

// --- The key must never leave the hub through an API ---

// TestClusterKeyNeverSerializedIntoAPIPayload is the standing guarantee that a
// cluster's App private key cannot reach an operator UI or any HTTP client. It
// marshals every cluster-shaped payload the hub hands out and asserts the key
// appears in none of them.
func TestClusterKeyNeverSerializedIntoAPIPayload(t *testing.T) {
	withTempAppKeyDir(t)
	keyPEM := testAppKeyPEM(t)
	if err := storeClusterAppKey("vllm-d", keyPEM); err != nil {
		t.Fatal(err)
	}
	// A distinctive slice of the key body to search for. Using the middle of the
	// PEM (not the BEGIN header) makes an accidental substring match implausible.
	bodyLines := []string{}
	for _, line := range strings.Split(strings.TrimSpace(keyPEM), "\n") {
		if !strings.HasPrefix(line, "-----") && len(strings.TrimSpace(line)) > 20 {
			bodyLines = append(bodyLines, strings.TrimSpace(line))
		}
	}
	if len(bodyLines) == 0 {
		t.Fatal("generated key had no body lines to search for")
	}

	cluster := ClusterConfig{
		ID:            "vllm-d",
		Name:          "GPU pool",
		Domain:        "hive.example.com",
		GitHubAppID:   5686,
		GitHubAppSlug: "kubestellar-hive-ghe",
	}
	identity := (&HubServer{clusters: map[string]ClusterConfig{"vllm-d": cluster}}).appIdentityForCluster("vllm-d")
	if identity == nil {
		t.Fatal("expected an identity for the seeded cluster")
	}

	gh := clusterGitHubConfig(&cluster)
	payloads := map[string]any{
		// The struct parsed from and written back to clusters.json.
		"ClusterConfig": cluster,
		// What the clusters list endpoint returns to the dashboard.
		"ClusterListEntry": ClusterListEntry{
			ID:            cluster.ID,
			Name:          cluster.Name,
			HasGPU:        cluster.HasGPU,
			Arch:          cluster.Arch,
			GitHubHost:    githubHostLabel(cluster.GitHubBaseURL),
			AppInstallURL: gh.AppInstallURL(),
		},
		// The key-store status endpoint: fingerprint only, by construction.
		"clusterAppKeyStatus": clusterAppKeyStatus{
			ClusterID:   cluster.ID,
			AppID:       cluster.GitHubAppID,
			HasKey:      true,
			Fingerprint: identity.Fingerprint,
		},
		// The decision record that feeds every log line.
		"appKeySyncDecision": decideAppKeySync("sha256:00000000000000000000000000000000", false, false, false, 0, identity),
	}

	for name, payload := range payloads {
		t.Run(name, func(t *testing.T) {
			data, err := json.Marshal(payload)
			if err != nil {
				t.Fatalf("marshal %s: %v", name, err)
			}
			s := string(data)
			if strings.Contains(s, "BEGIN RSA PRIVATE KEY") || strings.Contains(s, "PRIVATE KEY") {
				t.Fatalf("%s serialization contains a PEM header:\n%s", name, s)
			}
			for _, line := range bodyLines {
				if strings.Contains(s, line) {
					t.Fatalf("%s serialization contains private key material", name)
				}
			}
		})
	}
}

// TestClusterConfigHasNoPrivateKeyField locks the design decision in: if someone
// later adds a key field to ClusterConfig it lands in clusters.json and one
// marshal away from an HTTP response. Fail loudly rather than silently.
func TestClusterConfigHasNoPrivateKeyField(t *testing.T) {
	data, err := json.Marshal(ClusterConfig{ID: "x", GitHubAppID: 1})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	for name := range fields {
		lower := strings.ToLower(name)
		if strings.Contains(lower, "private") || strings.Contains(lower, "app_key") || lower == "key" {
			t.Errorf("ClusterConfig exposes a key-shaped JSON field %q — the private key must stay out of clusters.json", name)
		}
	}
}

// TestHeartbeatPayloadCarriesNoPrivateKey asserts the upward direction: a spoke
// reports only a fingerprint, never key material. The hub is the only holder.
func TestHeartbeatPayloadCarriesNoPrivateKey(t *testing.T) {
	data, err := json.Marshal(HeartbeatPayload{
		HiveID:                  "vllmd-01",
		GitHubAppKeyFingerprint: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		GitHubAppKeyPerHive:     true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	for name := range fields {
		lower := strings.ToLower(name)
		if strings.Contains(lower, "private_key") || lower == "app_key" {
			t.Errorf("HeartbeatPayload has upward key field %q — spoke must never send the key to the hub", name)
		}
	}
	if _, ok := fields["github_app_key_fingerprint"]; !ok {
		t.Error("heartbeat payload is missing the fingerprint field the hub compares against")
	}
}

// --- HTTP handlers ---

func TestHandleGetClusterAppKeysFingerprintsOnly(t *testing.T) {
	withTempAppKeyDir(t)
	keyPEM := testAppKeyPEM(t)
	if err := storeClusterAppKey("vllm-d", keyPEM); err != nil {
		t.Fatal(err)
	}
	wantFP, err := config.AppKeyFingerprint(keyPEM)
	if err != nil {
		t.Fatal(err)
	}

	s := &HubServer{logger: appKeyTestLogger(), clusters: map[string]ClusterConfig{
		"vllm-d":   {ID: "vllm-d", GitHubAppID: 5686},
		"hive-oke": {ID: "hive-oke"},
	}}

	rec := httptest.NewRecorder()
	s.handleGetClusterAppKeys(rec, httptest.NewRequest(http.MethodGet, "/api/saas/admin/cluster-app-keys", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "PRIVATE KEY") {
		t.Fatal("status endpoint leaked key material")
	}

	var got []clusterAppKeyStatus
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d statuses, want 2", len(got))
	}
	// Sorted by cluster ID: hive-oke then vllm-d.
	if got[0].ClusterID != "hive-oke" || got[0].HasKey {
		t.Errorf("hive-oke status wrong: %+v", got[0])
	}
	if got[1].ClusterID != "vllm-d" || !got[1].HasKey || got[1].Fingerprint != wantFP {
		t.Errorf("vllm-d status wrong: %+v", got[1])
	}
}

func TestHandlePutClusterAppKey(t *testing.T) {
	keyPEM := testAppKeyPEM(t)
	wantFP, err := config.AppKeyFingerprint(keyPEM)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		clusterID  string
		body       string
		wantStatus int
		wantStored bool
	}{
		{
			name:       "valid key stored",
			clusterID:  "vllm-d",
			body:       mustJSON(t, clusterAppKeyRequest{AppID: 5686, PrivateKey: keyPEM}),
			wantStatus: http.StatusOK,
			wantStored: true,
		},
		{
			name:       "unknown cluster rejected",
			clusterID:  "nope",
			body:       mustJSON(t, clusterAppKeyRequest{PrivateKey: keyPEM}),
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "non-pem body rejected",
			clusterID:  "vllm-d",
			body:       mustJSON(t, clusterAppKeyRequest{PrivateKey: "not a key"}),
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "pem envelope with garbage rejected",
			clusterID:  "vllm-d",
			body:       mustJSON(t, clusterAppKeyRequest{PrivateKey: "-----BEGIN RSA PRIVATE KEY-----\nZZZ\n-----END RSA PRIVATE KEY-----"}),
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "malformed json rejected",
			clusterID:  "vllm-d",
			body:       "{not json",
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			withTempAppKeyDir(t)
			s := &HubServer{logger: appKeyTestLogger(), clusters: map[string]ClusterConfig{
				"vllm-d": {ID: "vllm-d", GitHubAppID: 5686},
			}}

			req := httptest.NewRequest(http.MethodPut,
				"/api/saas/admin/cluster-app-keys/"+tc.clusterID, strings.NewReader(tc.body))
			req.SetPathValue("clusterID", tc.clusterID)
			rec := httptest.NewRecorder()
			s.handlePutClusterAppKey(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			// No response, success or error, may echo key material back.
			if strings.Contains(rec.Body.String(), "PRIVATE KEY") {
				t.Fatal("response echoed key material")
			}
			stored := loadClusterAppKey(tc.clusterID) != ""
			if stored != tc.wantStored {
				t.Errorf("stored = %v, want %v", stored, tc.wantStored)
			}
			if tc.wantStored {
				var got clusterAppKeyStatus
				if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
					t.Fatalf("unmarshal response: %v", err)
				}
				if got.Fingerprint != wantFP {
					t.Errorf("response fingerprint = %q, want %q", got.Fingerprint, wantFP)
				}
			}
		})
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
