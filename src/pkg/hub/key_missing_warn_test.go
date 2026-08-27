package hub

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// These tests pin the "say it out loud" rule for the undeliverable key state:
// a spoke POSITIVELY reporting key-missing on a required App, answered by a
// hub that holds no key for it, must produce a WARN naming the hive and the
// remedy. Before this, that beat returned nil in complete silence — no log,
// no alert — which is how a hosted hive (kelly-headwaters) sat degraded on
// key-missing for 8 days (2026-08-12 → 2026-08-20) with nothing anywhere
// telling the operator a key upload was owed.

func keyMissingWarnServer(t *testing.T, buf *bytes.Buffer) *HubServer {
	t.Helper()
	withTempAppKeyDir(t) // deliberately EMPTY: the hub holds no key to deliver
	return &HubServer{
		clusters: map[string]ClusterConfig{
			"oke-test": {ID: "oke-test"},
		},
		logger: slog.New(slog.NewTextHandler(buf, nil)),
	}
}

func TestAppKeySync_WarnsWhenSpokeReportsKeyMissingAndHubHoldsNoKey(t *testing.T) {
	var buf bytes.Buffer
	s := keyMissingWarnServer(t, &buf)

	cfg := s.appKeySyncForHeartbeat(&HeartbeatPayload{
		HiveID:            "kelly-headwaters",
		ClusterID:         "oke-test",
		GitHubAppRequired: true,
		GitHubAppState:    "key-missing",
	})
	if cfg != nil && cfg.PrivateKey != "" {
		t.Fatalf("a keyless hub delivered a key: %+v", cfg)
	}

	logged := buf.String()
	if !strings.Contains(logged, "never delivered") {
		t.Errorf("no WARN for the undeliverable key-missing state; log = %q", logged)
	}
	if !strings.Contains(logged, "kelly-headwaters") {
		t.Errorf("WARN does not name the hive; log = %q", logged)
	}
	if !strings.Contains(logged, "/api/saas/admin/cluster-app-keys/oke-test") {
		t.Errorf("WARN does not name the remedy endpoint; log = %q", logged)
	}
}

// The quiet cases stay quiet: a spoke not reporting key-missing (an ordinary
// keyless-cluster beat, the overwhelming majority) must not produce the WARN.
func TestAppKeySync_NoWarnWithoutPositiveKeyMissingReport(t *testing.T) {
	for name, payload := range map[string]*HeartbeatPayload{
		"no app state reported": {
			HiveID: "quiet-1", ClusterID: "oke-test", GitHubAppRequired: true,
		},
		"app not required": {
			HiveID: "quiet-2", ClusterID: "oke-test", GitHubAppState: "key-missing",
		},
		"user-side state": {
			HiveID: "quiet-3", ClusterID: "oke-test", GitHubAppRequired: true,
			GitHubAppState: "not-installed",
		},
	} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			s := keyMissingWarnServer(t, &buf)
			s.appKeySyncForHeartbeat(payload)
			if logged := buf.String(); strings.Contains(logged, "never delivered") {
				t.Errorf("unexpected key-missing WARN; log = %q", logged)
			}
		})
	}
}
