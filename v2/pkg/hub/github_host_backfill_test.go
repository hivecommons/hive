package hub

import (
	"testing"

	"log/slog"
)

// reconcileGitHubHostFromSpoke must backfill an EMPTY github_host from a spoke
// whose whole github block coherently, workingly names a forge — and must NEVER
// overwrite an explicit recorded host from a spoke report (the report is
// downstream of hub delivery: adopting a contradiction launders a mis-delivery
// back into the record, the 2026-08-05 revert loop), never demote a legit
// public pin, and never fire on an unknown/blank report.
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

	t.Run("an explicit recorded host is NEVER overwritten from a spoke report", func(t *testing.T) {
		// Even a fully coherent, working-looking GHE report cannot overwrite an
		// explicitly recorded github.com host: the spoke's github block is
		// downstream of hub delivery, so a contradiction is at best a
		// mis-delivery echoing back. Adopting it is the 2026-08-05 revert loop:
		// hand-restored metas were re-stomped GHE within ~30 minutes by exactly
		// this path, and the wrong-forge repair then re-delivered the GHE App.
		// The vllmd-06 class (meta github.com, repo genuinely on GHE) is now
		// operator/forge-switch territory, deliberately.
		changed, got := run(t,
			&SaaSHive{ID: "vllmd-06", Status: "claimed", GitHubHost: "github.com"},
			&HeartbeatPayload{
				HiveID:               "vllmd-06",
				GitHubHost:           "github.ibm.com",
				GitHubAPIURL:         "https://github.ibm.com/api/v3",
				GitHubAppID:          testGHEAppID,
				GitHubInstallationID: 42,
			},
		)
		if changed || got != "github.com" {
			t.Errorf("got (changed=%v, host=%q), want (false, github.com) — the recorded host is the authority", changed, got)
		}
	})

	t.Run("an EMPTY host is backfilled from a coherent working GHE report", func(t *testing.T) {
		// base-or-api: blank base_url with a GHE api_url still recognises GHE.
		// Positive evidence complete: an App of that forge, live installation.
		changed, got := run(t,
			&SaaSHive{ID: "blank-working", Status: "claimed"},
			&HeartbeatPayload{
				HiveID:               "blank-working",
				GitHubHost:           "",
				GitHubAPIURL:         "https://github.ibm.com/api/v3",
				GitHubAppID:          testGHEAppID,
				GitHubInstallationID: 42,
			},
		)
		if !changed || got != "github.ibm.com" {
			t.Errorf("got (changed=%v, host=%q), want (true, github.ibm.com)", changed, got)
		}
	})

	t.Run("empty host: a fresh-boot report with installation 0 is not evidence", func(t *testing.T) {
		// The fresh-boot gap: a just-booted mis-delivered spoke has not failed
		// yet, so "not failing" alone must not admit it. A zeroed installation
		// is a repair in flight (ResetInstallation), never a forge election.
		changed, got := run(t,
			&SaaSHive{ID: "blank-reset", Status: "claimed"},
			&HeartbeatPayload{
				HiveID:               "blank-reset",
				GitHubAPIURL:         "https://github.ibm.com/api/v3",
				GitHubAppID:          testGHEAppID,
				GitHubInstallationID: 0,
			},
		)
		if changed || got != "" {
			t.Errorf("got (changed=%v, host=%q), want (false, \"\") — a mid-reset spoke proves nothing", changed, got)
		}
	})

	t.Run("empty host: GHE urls beside the PUBLIC App are a mis-delivery artifact", func(t *testing.T) {
		// The github block must coherently name ONE forge: leftover GHE urls
		// with the public app_id are exactly what a half-applied mis-delivery
		// (or a half-completed restore) looks like.
		changed, got := run(t,
			&SaaSHive{ID: "blank-incoherent", Status: "claimed"},
			&HeartbeatPayload{
				HiveID:               "blank-incoherent",
				GitHubAPIURL:         "https://github.ibm.com/api/v3",
				GitHubAppID:          testGitHubComAppID,
				GitHubInstallationID: 42,
			},
		)
		if changed || got != "" {
			t.Errorf("got (changed=%v, host=%q), want (false, \"\") — an incoherent block is not a forge", changed, got)
		}
	})

	t.Run("empty host: an auth-failing spoke is never adopted", func(t *testing.T) {
		changed, got := run(t,
			&SaaSHive{ID: "blank-failing", Status: "claimed"},
			&HeartbeatPayload{
				HiveID:               "blank-failing",
				GitHubAPIURL:         "https://github.ibm.com/api/v3",
				GitHubAppID:          testGHEAppID,
				GitHubInstallationID: 42,
				GitHubAppRequired:    true,
				GitHubAppState:       spokeAppStateNotInstalled,
			},
		)
		if changed || got != "" {
			t.Errorf("got (changed=%v, host=%q), want (false, \"\") — a failing spoke's forge is not its forge", changed, got)
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
