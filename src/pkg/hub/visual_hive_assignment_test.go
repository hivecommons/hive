package hub

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kubestellar/hive/pkg/config"
	hivegithub "github.com/kubestellar/hive/pkg/github"
)

func TestVisualHiveAssignedBrokerActivatesWithoutRestart(t *testing.T) {
	withTempAppKeyDir(t)
	withVisualHiveWrapPath(t)
	now := time.Now().UTC()
	calls := 0
	broker := &VisualHiveTokenBroker{logger: slog.New(slog.NewTextHandler(io.Discard, nil)), now: func() time.Time { return now }, pinPath: filepath.Join(t.TempDir(), "pins.json"), pins: map[string]visualHiveWrapPin{}, mintToken: func(_ context.Context, repo string) (hivegithub.AppRuntimeIdentity, string, time.Time, error) {
		calls++
		identity := testVisualHiveIdentity(repo)
		identity.AppID = config.VizHivePublicAppID
		return identity, "test-scoped-token", now.Add(time.Hour), nil
	}}
	hive := &SaaSHive{ID: "canary", ClusterID: "cluster-one", Org: "owner", PrimaryRepo: "repo"}
	request, err := NewVisualHiveTokenRequest("owner/repo", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := broker.IssueForHive(context.Background(), hive, request); err == nil {
		t.Fatal("unassigned hive got a lease")
	}
	hive.SecondaryAppID = config.VizHivePublicAppID
	if _, err := broker.IssueForHive(context.Background(), hive, request); err == nil {
		t.Fatal("missing key was accepted")
	}
	if calls != 0 {
		t.Fatal("dormant broker minted")
	}
	primary := testAppKeyPEM(t)
	if err := storeClusterAppKey("cluster-one", primary); err != nil {
		t.Fatal(err)
	}
	if err := storeSecondaryAppKey("cluster-one", config.VizHivePublicAppID, 5686, testAppKeyPEM(t)); err != nil {
		t.Fatal(err)
	}
	lease, err := broker.IssueForHive(context.Background(), hive, request)
	if err != nil || lease == nil {
		t.Fatalf("hot activation failed: %v", err)
	}
	material, err := OpenVisualHiveTokenLease(hive.ID, "owner/repo", lease, now)
	if err != nil || material.Token != "test-scoped-token" {
		t.Fatalf("lease did not open: %v", err)
	}
	if strings.TrimSpace(loadClusterAppKey("cluster-one")) != strings.TrimSpace(primary) {
		t.Fatal("primary key changed")
	}
	request.CurrentAppID = lease.AppID
	request.CurrentInstallationID = lease.InstallationID
	request.CurrentBindingDigest = lease.BindingDigest
	request.CurrentExpiresAt = lease.ExpiresAt
	if next, err := broker.IssueForHive(context.Background(), hive, request); err != nil || next != nil || calls != 1 {
		t.Fatalf("duplicate mint: calls=%d err=%v", calls, err)
	}
	// Clearing the authoritative assignment blocks renewal despite spoke claims.
	hive.SecondaryAppID = 0
	if _, err := broker.IssueForHive(context.Background(), hive, request); err == nil {
		t.Fatal("cleared assignment accepted")
	}
	hive.SecondaryAppID = config.VizHivePublicAppID
	request.Repository = "other/repo"
	if _, err := broker.IssueForHive(context.Background(), hive, request); err == nil {
		t.Fatal("cross repository request accepted")
	}
	request.Repository = "owner/repo"
	now = now.Add(50 * time.Minute)
	if next, err := broker.IssueForHive(context.Background(), hive, request); err != nil || next == nil || calls != 2 {
		t.Fatalf("renewal failed: calls=%d err=%v", calls, err)
	}
	// A corrupt rotated key blocks even a still-valid claimed lease.
	keyPath, _ := secondaryAppKeyPath(hive.ClusterID, hive.SecondaryAppID)
	if err := os.WriteFile(keyPath, []byte("invalid rotated key"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := broker.IssueForHive(context.Background(), hive, request); err == nil || calls != 2 {
		t.Fatal("invalid rotation was accepted")
	}
}

func TestVisualHiveAppKeysNeverEnterSecondaryPEMDelivery(t *testing.T) {
	withTempAppKeyDir(t)
	withTempHivesDir(t)
	s := newSecondaryAppTestServer(t)
	s.visualHiveTokenBroker = &VisualHiveTokenBroker{appID: 71}
	for _, appID := range []int64{config.VizHivePublicAppID, config.VizHiveEnterpriseAppID, 71} {
		if err := storeSecondaryAppKey("cluster-one", appID, 5686, testAppKeyPEM(t)); err != nil {
			t.Fatal(err)
		}
		mustSaveHive(t, &SaaSHive{ID: "canary", ClusterID: "cluster-one", SecondaryAppID: appID})
		// No feature request is intentional: an old spoke cannot trigger a downgrade.
		if got := s.secondaryAppKeyForHeartbeat(&HeartbeatPayload{HiveID: "canary"}); got != nil {
			t.Fatalf("private key for Visual Hive App %d reached spoke", appID)
		}
	}
}
