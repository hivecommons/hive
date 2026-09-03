package hub

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// The #2496 hot loop: a spoke that never reports a key fingerprint got the
// cluster key re-pushed on EVERY beat, forever. The first deliveries must stay
// immediate (a genuinely new spoke is keyed promptly), then delivery throttles
// to the back-off interval, and any sign of convergence resets the budget.
func TestAppKeyDeliveryBackoffOnPrimaryReconcile(t *testing.T) {
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
	// The broken sender's signature: no fingerprint reported, beat after beat.
	keyless := &HeartbeatPayload{HiveID: "broken", ClusterID: "vllm-d"}

	// Inside the immediate budget every beat delivers.
	for i := 0; i < appKeyDeliveryImmediateBudget; i++ {
		cfg := s.appKeySyncForHeartbeat(keyless)
		if cfg == nil || cfg.PrivateKey == "" {
			t.Fatalf("beat %d: key not delivered inside the immediate budget", i+1)
		}
	}

	// Budget exhausted: the very next beats are suppressed.
	for i := 0; i < 3; i++ {
		if cfg := s.appKeySyncForHeartbeat(keyless); cfg != nil {
			t.Fatalf("beat after budget %d: delivery not suppressed — the every-beat key hot loop is back", i+1)
		}
	}

	// After the back-off interval one delivery goes through again…
	s.appKeyDeliveryMu.Lock()
	s.appKeyDelivery["broken"].lastSent = time.Now().Add(-appKeyDeliveryBackoffInterval)
	s.appKeyDeliveryMu.Unlock()
	if cfg := s.appKeySyncForHeartbeat(keyless); cfg == nil || cfg.PrivateKey == "" {
		t.Fatal("throttled delivery did not resume after the back-off interval — a recovered spoke would never be re-keyed")
	}
	// …and the throttle immediately re-engages.
	if cfg := s.appKeySyncForHeartbeat(keyless); cfg != nil {
		t.Fatal("throttle did not re-engage after the back-off delivery")
	}

	// CONVERGENCE: the spoke reports the cluster fingerprint. No push is
	// needed, and the ledger resets so future deliveries are immediate again.
	converged := &HeartbeatPayload{HiveID: "broken", ClusterID: "vllm-d", GitHubAppKeyFingerprint: clusterFP}
	if cfg := s.appKeySyncForHeartbeat(converged); cfg != nil {
		t.Fatal("converged spoke still got a key push")
	}
	if cfg := s.appKeySyncForHeartbeat(keyless); cfg == nil || cfg.PrivateKey == "" {
		t.Fatal("delivery counter did not reset on convergence — a spoke that re-lost its key would be throttled from its first beat")
	}
}

// SECURITY (C1/N3, CWE-200/639): the former
// TestAppKeyDeliveryBackoffOnAdditionalKeys exercised the fleet-wide
// additional-key broadcast's throttle (attachMissingAppKeys). That delivery lane
// was removed — it handed every tenant's App private key to any caller — so the
// test was deleted with it. The primary-key delivery back-off above and the
// ledger contract below remain the live coverage for the throttle.

// Unit contract of the ledger itself: budget, throttle, reset.
func TestAppKeyDeliveryLedger(t *testing.T) {
	s := &HubServer{logger: appKeyTestLogger()}

	for i := 0; i < appKeyDeliveryImmediateBudget; i++ {
		if !s.allowAppKeyDelivery("h") {
			t.Fatalf("delivery %d refused inside the immediate budget", i+1)
		}
		s.noteAppKeyDelivered("h")
	}
	if s.allowAppKeyDelivery("h") {
		t.Fatal("delivery allowed immediately after the budget was exhausted")
	}
	// Another hive is unaffected — the ledger is per hive.
	if !s.allowAppKeyDelivery("other") {
		t.Fatal("an unrelated hive was throttled")
	}
	// The throttled hive is allowed again once the interval has passed.
	s.appKeyDeliveryMu.Lock()
	s.appKeyDelivery["h"].lastSent = time.Now().Add(-appKeyDeliveryBackoffInterval)
	s.appKeyDeliveryMu.Unlock()
	if !s.allowAppKeyDelivery("h") {
		t.Fatal("throttled hive not allowed after the back-off interval")
	}
	// Convergence resets in full.
	s.noteAppKeyConverged("h")
	if !s.allowAppKeyDelivery("h") {
		t.Fatal("converged hive still throttled")
	}
}
