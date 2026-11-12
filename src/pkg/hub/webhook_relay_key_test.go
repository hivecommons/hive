package hub

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/hub/spoke"
)

type relayedSignedWebhook struct {
	header http.Header
	body   []byte
}

func captureRelaySpoke(t *testing.T) (*httptest.Server, chan relayedSignedWebhook) {
	t.Helper()
	got := make(chan relayedSignedWebhook, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- relayedSignedWebhook{header: r.Header.Clone(), body: body}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

func useTempSaaSHivesDir(t *testing.T) {
	t.Helper()
	old := saasHivesDir
	saasHivesDir = t.TempDir()
	t.Cleanup(func() { saasHivesDir = old })
}

// TestRelayWebhookSignsWithTargetHiveRelayKey: the relay to a hosted hive
// carries a hub signature that verifies with THAT hive's relay key and not
// with another hive's, binds the delivery ID and a fresh timestamp, and still
// forwards GitHub's own signature unchanged (#17911).
func TestRelayWebhookSignsWithTargetHiveRelayKey(t *testing.T) {
	const secret = "s3cr3t"
	t.Setenv(webhookSecretEnvVar, secret)
	allowLoopbackRelay(t)
	useTempSaaSHivesDir(t)
	for id, key := range map[string]string{"h1": "relay-key-h1", "h2": "relay-key-h2"} {
		if err := saveSaaSHive(&SaaSHive{ID: id, WebhookRelayKey: key}); err != nil {
			t.Fatal(err)
		}
	}
	spokeSrv, got := captureRelaySpoke(t)

	s := newWebhookHub()
	s.registry.Hives = []RegistryEntry{{ID: "h1", Org: "acme", Repos: []string{"widgets"}, DashboardURL: spokeSrv.URL}}

	body := []byte(`{"action":"synchronize","number":7,"repository":{"name":"widgets","full_name":"acme/widgets","owner":{"login":"acme"}}}`)
	ghSig := signWebhook(secret, body)
	req := httptest.NewRequest(http.MethodPost, "/api/github/webhook", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-GitHub-Delivery", "delivery-1")
	req.Header.Set("X-Hub-Signature-256", ghSig)
	before := time.Now().Unix()
	rec := httptest.NewRecorder()
	s.handleGitHubWebhook(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var r relayedSignedWebhook
	select {
	case r = <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("delivery was not relayed to the managing spoke")
	}
	if r.header.Get("X-Hub-Signature-256") != ghSig || !bytes.Equal(r.body, body) {
		t.Fatal("GitHub's signature and body must still be forwarded unchanged")
	}
	ts := r.header.Get(webhookRelayTimestampHeader)
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || sec < before || sec > time.Now().Unix() {
		t.Fatalf("relay timestamp = %q, want unix seconds at relay time", ts)
	}
	sig := r.header.Get(webhookRelaySignatureHeader)
	if sig == "" || sig != webhookRelaySignature("relay-key-h1", ts, "delivery-1", body) {
		t.Fatalf("relay signature %q does not verify with the target hive's relay key", sig)
	}
	if sig == webhookRelaySignature("relay-key-h2", ts, "delivery-1", body) {
		t.Fatal("relay signature verifies with another hive's relay key")
	}
	if sig == webhookRelaySignature("relay-key-h1", ts, "delivery-2", body) {
		t.Fatal("relay signature must bind the delivery ID")
	}
}

// TestRelayWebhookUnsignedForHiveWithoutRelayKey: a hive the hub did not
// provision (no record, so no relay key) gets the relay exactly as before.
func TestRelayWebhookUnsignedForHiveWithoutRelayKey(t *testing.T) {
	allowLoopbackRelay(t)
	useTempSaaSHivesDir(t)
	spokeSrv, got := captureRelaySpoke(t)

	s := newWebhookHub()
	s.registry.Hives = []RegistryEntry{{ID: "selfhosted", Org: "acme", Repos: []string{"widgets"}, DashboardURL: spokeSrv.URL}}
	hdr := http.Header{}
	hdr.Set("X-GitHub-Delivery", "d")
	s.relayWebhookToSpoke("push", hdr, []byte(`{"repository":{"name":"widgets","full_name":"acme/widgets","owner":{"login":"acme"}}}`))
	select {
	case r := <-got:
		if r.header.Get(webhookRelaySignatureHeader) != "" || r.header.Get(webhookRelayTimestampHeader) != "" {
			t.Fatalf("hive without a relay key got relay headers: %v", r.header)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("delivery was not relayed")
	}
}

// TestWebhookRelayKeyForHeartbeat: the relay key is delivered until the spoke
// reports holding it, minted for a hosted hive provisioned before relay keys
// existed, re-delivered after a rotation, and never offered to a hive the hub
// has no record for.
func TestWebhookRelayKeyForHeartbeat(t *testing.T) {
	useTempSaaSHivesDir(t)
	s := &HubServer{logger: slog.Default()}

	if got := s.webhookRelayKeyForHeartbeat(&HeartbeatPayload{HiveID: "unknown"}); got != "" {
		t.Fatalf("hive without a record got a relay key")
	}

	if err := saveSaaSHive(&SaaSHive{ID: "legacy"}); err != nil {
		t.Fatal(err)
	}
	minted := s.webhookRelayKeyForHeartbeat(&HeartbeatPayload{HiveID: "legacy"})
	if len(minted) != 64 {
		t.Fatalf("legacy hive: delivered %q, want a minted 256-bit hex key", minted)
	}
	if h := loadSaaSHive("legacy"); h == nil || h.WebhookRelayKey != minted {
		t.Fatal("minted relay key was not stored on the hive record")
	}
	if again := s.webhookRelayKeyForHeartbeat(&HeartbeatPayload{HiveID: "legacy"}); again != minted {
		t.Fatal("a spoke that has not confirmed the key must get the same key again, not a new one")
	}
	if got := s.webhookRelayKeyForHeartbeat(&HeartbeatPayload{HiveID: "legacy", WebhookRelayKeyFingerprint: webhookRelayKeyFingerprint(minted)}); got != "" {
		t.Fatal("key re-delivered to a spoke that already holds it")
	}

	// Rotation: a new key on the record no longer matches the spoke's
	// fingerprint, so the next beat delivers it.
	h := loadSaaSHive("legacy")
	h.WebhookRelayKey = "rotated-key"
	if err := saveSaaSHive(h); err != nil {
		t.Fatal(err)
	}
	if got := s.webhookRelayKeyForHeartbeat(&HeartbeatPayload{HiveID: "legacy", WebhookRelayKeyFingerprint: webhookRelayKeyFingerprint(minted)}); got != "rotated-key" {
		t.Fatalf("after rotation delivered %q, want the rotated key", got)
	}
}

// TestProvisionedHiveRecordsHaveDistinctRelayKeys: provisioning mints a
// fresh random key per hive.
func TestProvisionedHiveRecordsHaveDistinctRelayKeys(t *testing.T) {
	a, b := newWebhookRelayKey(), newWebhookRelayKey()
	if len(a) != 64 || len(b) != 64 || a == b {
		t.Fatalf("relay keys %q / %q, want two distinct 256-bit hex keys", a, b)
	}
}

// TestHiveStatusNeverExposesRelayKey: the owner-facing hive status returns
// the hive record, but never the relay key, which only the spoke's own
// heartbeat may carry.
func TestHiveStatusNeverExposesRelayKey(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := &HubServer{logger: slog.Default(), hubSecret: testHubSecret}
	mkUser(t, "alice")
	if err := saveSaaSHive(&SaaSHive{ID: "h1", Owner: "alice", WebhookRelayKey: "relay-key-h1"}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.handleHiveStatus(rec, setPathValue(reqWithUser(http.MethodGet, "/st", "", "alice"), "id", "h1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("owner hive-status = %d, want 200", rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["webhook_relay_key"]; ok || bytes.Contains(rec.Body.Bytes(), []byte("relay-key-h1")) {
		t.Fatalf("hive status leaked the relay key: %s", rec.Body.String())
	}
	if h := loadSaaSHive("h1"); h == nil || h.WebhookRelayKey != "relay-key-h1" {
		t.Fatal("redacting the response must not touch the stored record")
	}
}

// TestWebhookRelayKeyHeartbeatWire: the key the hub attaches reaches the
// spoke's decoder, and the fingerprint the spoke reports reaches the hub's,
// with the same fingerprint format on both sides.
func TestWebhookRelayKeyHeartbeatWire(t *testing.T) {
	raw, err := json.Marshal(HeartbeatResponse{GitHubAppConfig: &HeartbeatGitHubAppConfig{WebhookRelayKey: "relay-key-h1"}})
	if err != nil {
		t.Fatal(err)
	}
	var spokeResp spoke.HeartbeatResponse
	if err := json.Unmarshal(raw, &spokeResp); err != nil {
		t.Fatal(err)
	}
	if spokeResp.GitHubAppConfig == nil || spokeResp.GitHubAppConfig.WebhookRelayKey != "relay-key-h1" {
		t.Fatalf("spoke decoded %+v, want the relay key", spokeResp.GitHubAppConfig)
	}

	raw, err = json.Marshal(spoke.HeartbeatPayload{HiveID: "h1", WebhookRelayKeyFingerprint: webhookRelayKeyFingerprint("relay-key-h1")})
	if err != nil {
		t.Fatal(err)
	}
	var hubPayload HeartbeatPayload
	if err := json.Unmarshal(raw, &hubPayload); err != nil {
		t.Fatal(err)
	}
	if hubPayload.WebhookRelayKeyFingerprint != webhookRelayKeyFingerprint("relay-key-h1") {
		t.Fatalf("hub decoded fingerprint %q", hubPayload.WebhookRelayKeyFingerprint)
	}
}

// meta.json carries the relay key, so it must be unreadable to other local
// users like the hub's other secret stores. A pre-existing world-readable hive
// directory must be tightened on save too.
func TestSaveSaaSHiveKeepsRelayKeyPrivate(t *testing.T) {
	useTempSaaSHivesDir(t)

	dir := filepath.Join(saasHivesDir, "legacy")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := saveSaaSHive(&SaaSHive{ID: "legacy", WebhookRelayKey: newWebhookRelayKey()}); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Stat(filepath.Join(dir, "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("meta.json mode = %o, want 600", got)
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := di.Mode().Perm(); got != 0o700 {
		t.Fatalf("hive dir mode = %o, want 700", got)
	}
}

// Tightening must only strip group/other bits. A hive directory that has
// been made read-only must stay read-only so the save fails loudly instead
// of being silently widened back to writable (which also masked the forced
// save failures in reset_assignment_sweep_branches_test.go).
func TestSaveSaaSHiveNeverWidensReadOnlyDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; read-only dir cannot force a write failure")
	}
	useTempSaaSHivesDir(t)

	dir := filepath.Join(saasHivesDir, "frozen")
	if err := os.MkdirAll(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })

	if err := saveSaaSHive(&SaaSHive{ID: "frozen", WebhookRelayKey: newWebhookRelayKey()}); err == nil {
		t.Fatal("saveSaaSHive succeeded into a read-only hive dir; want write failure")
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := di.Mode().Perm(); got != 0o500 {
		t.Fatalf("hive dir mode = %o, want 500 (group/other stripped, owner bits untouched)", got)
	}
}
