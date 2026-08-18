package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kubestellar/hive/pkg/integrated"
)

const integratedReleaseRepository = "DavidDiaz0317/hive"

type installedReleaseIdentity struct {
	Repository               string
	Version                  string
	HiveCommit               string
	VisualHiveCommit         string
	ManifestSHA256           string
	ManifestPath             string
	HostedControllerProtocol int
}

func loadInstalledReleaseIdentity() (installedReleaseIdentity, error) {
	executable, err := os.Executable()
	if err != nil {
		return installedReleaseIdentity{}, fmt.Errorf("resolve installed Hive executable: %w", err)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(executable); resolveErr == nil {
		executable = resolved
	}
	return loadReleaseIdentityBeside(executable, integratedVersion, gitHash)
}

func loadReleaseIdentityBeside(executable, buildVersion, buildCommit string) (installedReleaseIdentity, error) {
	manifestPath := filepath.Join(filepath.Dir(executable), "distribution-manifest.json")
	info, err := os.Lstat(manifestPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 || info.Size() > 32<<20 {
		return installedReleaseIdentity{}, fmt.Errorf("hosted setup requires the immutable integrated release manifest beside Hive; reinstall the release or use --runtime local")
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return installedReleaseIdentity{}, fmt.Errorf("read installed integrated release manifest: %w", err)
	}
	var manifest integrated.DistributionManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return installedReleaseIdentity{}, fmt.Errorf("parse installed integrated release manifest: %w", err)
	}
	version := strings.TrimSpace(buildVersion)
	commit := strings.ToLower(strings.TrimSpace(buildCommit))
	if manifest.SchemaVersion != integrated.DistributionSchema || manifest.HiveVersion != version || manifest.HiveCommit != commit ||
		len(manifest.VisualHiveCommit) != 40 || manifest.HostedControllerProtocol != integrated.HostedControllerProtocol || manifest.OS == "" || manifest.Architecture == "" {
		return installedReleaseIdentity{}, fmt.Errorf("installed integrated release manifest does not match this Hive binary identity")
	}
	digest := sha256.Sum256(data)
	return installedReleaseIdentity{
		Repository: integratedReleaseRepository, Version: version, HiveCommit: commit,
		VisualHiveCommit: strings.ToLower(manifest.VisualHiveCommit), ManifestSHA256: hex.EncodeToString(digest[:]),
		ManifestPath: manifestPath, HostedControllerProtocol: manifest.HostedControllerProtocol,
	}, nil
}

func hostedCronForInterval(interval time.Duration) (string, error) {
	if interval < 5*time.Minute || interval > 24*time.Hour || interval%time.Minute != 0 {
		return "", fmt.Errorf("hosted run interval must be whole minutes from five minutes through 24 hours")
	}
	minutes := int(interval / time.Minute)
	if minutes < 60 && 60%minutes == 0 {
		return fmt.Sprintf("*/%d * * * *", minutes), nil
	}
	if minutes%60 == 0 {
		hours := minutes / 60
		if hours == 24 {
			return "0 0 * * *", nil
		}
		if hours < 24 && 24%hours == 0 {
			return fmt.Sprintf("0 */%d * * *", hours), nil
		}
	}
	return "", fmt.Errorf("hosted run interval %s cannot be represented as a non-drifting GitHub schedule; use 5, 10, 15, 20, 30 minutes or a whole-hour divisor of 24", interval)
}
