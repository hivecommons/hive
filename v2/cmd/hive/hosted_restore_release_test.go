package main

import (
	"strings"
	"testing"

	"github.com/kubestellar/hive/v2/pkg/integrated"
)

func TestHostedRestoreReleaseRequiresCompleteDistinctIdentity(t *testing.T) {
	t.Parallel()
	current := installedReleaseIdentity{
		Version:                  "v0.4.1-integrated.20",
		HiveCommit:               strings.Repeat("a", 40),
		VisualHiveCommit:         strings.Repeat("b", 40),
		ManifestSHA256:           strings.Repeat("c", 64),
		HostedControllerProtocol: integrated.HostedControllerProtocol,
	}

	restore, manifest, err := hostedRestoreRelease("", "", "", "", 0, current)
	if err != nil || restore != nil || manifest != nil {
		t.Fatalf("empty predecessor = (%+v, %+v, %v), want nil identity", restore, manifest, err)
	}

	version := "v0.4.1-integrated.19"
	hiveCommit := strings.Repeat("d", 40)
	visualCommit := strings.Repeat("e", 40)
	manifestDigest := strings.Repeat("f", 64)
	restore, manifest, err = hostedRestoreRelease(version, hiveCommit, visualCommit, manifestDigest, integrated.HostedControllerProtocol, current)
	if err != nil {
		t.Fatalf("complete predecessor: %v", err)
	}
	if restore == nil || restore.Version != version || restore.HiveCommit != hiveCommit || restore.VisualHiveCommit != visualCommit ||
		restore.DistributionManifestSHA256 != manifestDigest || restore.HostedControllerProtocol != integrated.HostedControllerProtocol {
		t.Fatalf("restore identity = %+v", restore)
	}
	if manifest == nil || manifest.Version != version || manifest.HiveCommit != hiveCommit || manifest.VisualHiveCommit != visualCommit || manifest.DistributionManifestSHA256 != manifestDigest {
		t.Fatalf("manifest identity = %+v", manifest)
	}
	if _, _, err := hostedRestoreRelease(version, hiveCommit, visualCommit, manifestDigest, 0, current); err == nil {
		t.Fatal("protocol-zero predecessor identity was accepted")
	}

	tests := []struct {
		name           string
		version        string
		hiveCommit     string
		visualCommit   string
		manifestDigest string
	}{
		{name: "partial", version: version},
		{name: "invalid version", version: "integrated.18", hiveCommit: hiveCommit, visualCommit: visualCommit, manifestDigest: manifestDigest},
		{name: "invalid hive commit", version: version, hiveCommit: "not-a-commit", visualCommit: visualCommit, manifestDigest: manifestDigest},
		{name: "same complete identity", version: current.Version, hiveCommit: current.HiveCommit, visualCommit: current.VisualHiveCommit, manifestDigest: current.ManifestSHA256},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, _, err := hostedRestoreRelease(test.version, test.hiveCommit, test.visualCommit, test.manifestDigest, integrated.HostedControllerProtocol, current); err == nil {
				t.Fatal("invalid predecessor identity was accepted")
			}
		})
	}
	for _, candidate := range []struct {
		name       string
		version    string
		hiveCommit string
	}{
		{name: "same version only", version: current.Version, hiveCommit: hiveCommit},
		{name: "same Hive commit only", version: version, hiveCommit: current.HiveCommit},
	} {
		candidate := candidate
		t.Run(candidate.name, func(t *testing.T) {
			t.Parallel()
			if _, _, err := hostedRestoreRelease(candidate.version, candidate.hiveCommit, visualCommit, manifestDigest, integrated.HostedControllerProtocol, current); err != nil {
				t.Fatalf("distinct complete release identity was rejected: %v", err)
			}
		})
	}
}
