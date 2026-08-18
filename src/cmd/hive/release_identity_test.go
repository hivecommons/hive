package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kubestellar/hive/pkg/integrated"
)

func TestLoadReleaseIdentityBesideBindsManifestToBinary(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "hive")
	if err := os.WriteFile(executable, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	hiveCommit, visualCommit := strings.Repeat("a", 40), strings.Repeat("b", 40)
	manifest := integrated.DistributionManifest{
		SchemaVersion: integrated.DistributionSchema, HiveVersion: "v0.4.1-integrated.19", HiveCommit: hiveCommit,
		VisualHiveCommit: visualCommit, VisualHiveVersion: "0.4.0", NodeVersion: "v22.23.1", OS: "linux", Architecture: "amd64",
		HostedControllerProtocol: integrated.HostedControllerProtocol,
	}
	data, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(root, "distribution-manifest.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	identity, err := loadReleaseIdentityBeside(executable, manifest.HiveVersion, hiveCommit)
	if err != nil {
		t.Fatal(err)
	}
	if identity.Repository != integratedReleaseRepository || identity.Version != manifest.HiveVersion || identity.HiveCommit != hiveCommit || identity.VisualHiveCommit != visualCommit ||
		identity.HostedControllerProtocol != integrated.HostedControllerProtocol || len(identity.ManifestSHA256) != 64 {
		t.Fatalf("unexpected installed identity: %+v", identity)
	}
	if _, err := loadReleaseIdentityBeside(executable, manifest.HiveVersion, strings.Repeat("c", 40)); err == nil {
		t.Fatal("manifest with a different Hive commit was accepted")
	}
}

func TestHostedCronForInterval(t *testing.T) {
	tests := map[time.Duration]string{5 * time.Minute: "*/5 * * * *", 15 * time.Minute: "*/15 * * * *", 2 * time.Hour: "0 */2 * * *", 24 * time.Hour: "0 0 * * *"}
	for interval, expected := range tests {
		actual, err := hostedCronForInterval(interval)
		if err != nil || actual != expected {
			t.Fatalf("cron %s = %q, %v", interval, actual, err)
		}
	}
	for _, invalid := range []time.Duration{time.Minute, 7 * time.Minute, 90 * time.Minute, 25 * time.Hour} {
		if _, err := hostedCronForInterval(invalid); err == nil {
			t.Fatalf("invalid hosted cadence %s was accepted", invalid)
		}
	}
}
