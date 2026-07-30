package hub

import (
	"testing"

	"log/slog"
)

// reconcileGitHubHostFromSpoke must repair a persisted host that is set but WRONG
// (public→GHE) from the forge the spoke reports, while never demoting a legit
// public pin and never firing on an unknown/blank report.
func TestReconcileGitHubHostFromSpoke(t *testing.T) {
	oldHives := saasHivesDir
	saasHivesDir = t.TempDir()
	t.Cleanup(func() { saasHivesDir = oldHives })

	s := &HubServer{logger: slog.New(slog.NewTextHandler(nopWriter{}, nil))}

	// Helper: persist a claimed hive, run the reconcile, return the resulting host.
	run := func(t *testing.T, hive *SaaSHive, payload *HeartbeatPayload) (bool, string) {
		t.Helper()
		if hive != nil {
			if err := saveSaaSHive(hive); err != nil {
				t.Fatal(err)
			}
		}
		changed := s.reconcileGitHubHostFromSpoke(payload)
		got := ""
		if hive != nil {
			if reloaded := loadSaaSHive(hive.ID); reloaded != nil {
				got = reloaded.GitHubHost
			}
		}
		return changed, got
	}

	t.Run("wrong github.com host is corrected to spoke-reported GHE (base-or-api)", func(t *testing.T) {
		// The vllmd-06 class: meta says github.com, spoke reports blank base_url but
		// a GHE api_url. base-or-api must still recognise GHE.
		changed, got := run(t,
			&SaaSHive{ID: "vllmd-06", Status: "claimed", GitHubHost: "github.com"},
			&HeartbeatPayload{HiveID: "vllmd-06", GitHubHost: "", GitHubAPIURL: "https://github.ibm.com/api/v3"},
		)
		if !changed || got != "github.ibm.com" {
			t.Errorf("got (changed=%v, host=%q), want (true, github.ibm.com)", changed, got)
		}
	})

	t.Run("already-correct GHE host is a no-op", func(t *testing.T) {
		changed, got := run(t,
			&SaaSHive{ID: "already-ghe", Status: "claimed", GitHubHost: "github.ibm.com"},
			&HeartbeatPayload{HiveID: "already-ghe", GitHubAPIURL: "https://github.ibm.com/api/v3"},
		)
		if changed || got != "github.ibm.com" {
			t.Errorf("got (changed=%v, host=%q), want (false, github.ibm.com)", changed, got)
		}
	})

	t.Run("explicit public pin is never demoted or overwritten", func(t *testing.T) {
		// A hive pinned public on a GHE cluster reports github.com by design; a
		// transient GHE api_url read must not flip it.
		changed, got := run(t,
			&SaaSHive{ID: "public-pin", Status: "claimed", GitHubBaseURL: githubHostPublic, GitHubHost: "github.com"},
			&HeartbeatPayload{HiveID: "public-pin", GitHubAPIURL: "https://github.ibm.com/api/v3"},
		)
		if changed || got != "github.com" {
			t.Errorf("got (changed=%v, host=%q), want (false, github.com)", changed, got)
		}
	})

	t.Run("blank/unknown spoke report changes nothing", func(t *testing.T) {
		changed, got := run(t,
			&SaaSHive{ID: "silent-spoke", Status: "claimed", GitHubHost: "github.com"},
			&HeartbeatPayload{HiveID: "silent-spoke"}, // no host, no api_url
		)
		if changed || got != "github.com" {
			t.Errorf("got (changed=%v, host=%q), want (false, github.com)", changed, got)
		}
	})

	t.Run("a different persisted GHE host is not auto-flipped", func(t *testing.T) {
		// Already GHE but a DIFFERENT enterprise host: an operator concern, not a
		// safe single-beat flip.
		changed, got := run(t,
			&SaaSHive{ID: "other-ghe", Status: "claimed", GitHubHost: "ghe.other.com"},
			&HeartbeatPayload{HiveID: "other-ghe", GitHubAPIURL: "https://github.ibm.com/api/v3"},
		)
		if changed || got != "ghe.other.com" {
			t.Errorf("got (changed=%v, host=%q), want (false, ghe.other.com)", changed, got)
		}
	})

	t.Run("unclaimed placeholder is skipped", func(t *testing.T) {
		changed, got := run(t,
			&SaaSHive{ID: "placeholder", Status: statusAvailable, GitHubHost: "github.com"},
			&HeartbeatPayload{HiveID: "placeholder", GitHubAPIURL: "https://github.ibm.com/api/v3"},
		)
		if changed || got != "github.com" {
			t.Errorf("got (changed=%v, host=%q), want (false, github.com)", changed, got)
		}
	})

	t.Run("an in-flight operator forge switch is not raced", func(t *testing.T) {
		// RequestedGitHubHost set = adoptSpokeForge is mid-handshake driving the
		// spoke to a new forge; the spoke may transiently report either host, so
		// this reconcile must stand down and let the switch own the host.
		changed, got := run(t,
			&SaaSHive{ID: "switching", Status: "claimed", GitHubHost: "github.com", RequestedGitHubHost: "github.ibm.com"},
			&HeartbeatPayload{HiveID: "switching", GitHubAPIURL: "https://github.ibm.com/api/v3"},
		)
		if changed || got != "github.com" {
			t.Errorf("got (changed=%v, host=%q), want (false, github.com) — must not race the forge switch", changed, got)
		}
	})

	t.Run("nil payload / nil server are safe", func(t *testing.T) {
		if s.reconcileGitHubHostFromSpoke(nil) {
			t.Error("nil payload must be a no-op")
		}
		var nilServer *HubServer
		if nilServer.reconcileGitHubHostFromSpoke(&HeartbeatPayload{HiveID: "x"}) {
			t.Error("nil server must be a no-op")
		}
	})
}

// nopWriter discards log output in tests.
type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

// The vllm-d gap: placeholders provisioned before their cluster gained GHE
// settings never inherit them, so the heartbeat pushes an empty API URL and
// the spoke keeps api.github.com plus the public app_id.
func TestBackfillGitHubHostFromCluster(t *testing.T) {
	gheCluster := &ClusterConfig{
		GitHubBaseURL: "https://github.ibm.com",
		GitHubAPIURL:  "https://github.ibm.com/api/v3",
	}
	publicCluster := &ClusterConfig{}

	tests := []struct {
		name    string
		hive    *SaaSHive
		cluster *ClusterConfig
		want    string
	}{
		{
			name:    "blank host on a GHE cluster is backfilled",
			hive:    &SaaSHive{},
			cluster: gheCluster,
			want:    "github.ibm.com",
		},
		{
			name:    "existing host is never overwritten",
			hive:    &SaaSHive{GitHubHost: "github.example.com"},
			cluster: gheCluster,
			want:    "",
		},
		{
			name:    "explicit public override stays public",
			hive:    &SaaSHive{GitHubBaseURL: githubHostPublic},
			cluster: gheCluster,
			want:    "",
		},
		{
			name:    "hive-level GHE override wins over the cluster default",
			hive:    &SaaSHive{GitHubBaseURL: "https://ghe.other.com"},
			cluster: gheCluster,
			want:    "ghe.other.com",
		},
		{
			name:    "public cluster has nothing to backfill",
			hive:    &SaaSHive{},
			cluster: publicCluster,
			want:    "",
		},
		{
			name:    "nil cluster is a no-op",
			hive:    &SaaSHive{},
			cluster: nil,
			want:    "",
		},
		{
			name:    "nil hive is a no-op",
			hive:    nil,
			cluster: gheCluster,
			want:    "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := backfillGitHubHostFromCluster(tc.hive, tc.cluster); got != tc.want {
				t.Errorf("backfillGitHubHostFromCluster = %q, want %q", got, tc.want)
			}
		})
	}
}

// The backfilled host must be what actually drives the heartbeat's API URL.
func TestBackfilledHostProducesGHEAPIURL(t *testing.T) {
	host := backfillGitHubHostFromCluster(&SaaSHive{}, &ClusterConfig{
		GitHubBaseURL: "https://github.ibm.com",
	})
	if got := gheAPIURLForHost(host); got != "https://github.ibm.com/api/v3" {
		t.Errorf("gheAPIURLForHost(%q) = %q, want https://github.ibm.com/api/v3", host, got)
	}
	// And a public hive must still push nothing.
	if got := gheAPIURLForHost(backfillGitHubHostFromCluster(&SaaSHive{}, &ClusterConfig{})); got != "" {
		t.Errorf("public hive pushed API URL %q, want empty", got)
	}
}
