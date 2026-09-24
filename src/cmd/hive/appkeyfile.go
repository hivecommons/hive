package main

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/hivecommons/hive/pkg/apphealth"
)

// deliveredKeyPath is where a hub-delivered private key for appID is stored.
//
// The filename NAMES the App, so a key can only ever be found under the App it
// was delivered for. The generic /data/gh-app-key.pem carries no such evidence:
// a key written there for one App silently becomes "the key" for whatever
// app_id the config later claims, which is how all 33 heartbeat-only-cluster spokes ended up
// signing as the public App with the GHE key and getting
// 404 Integration not found.
//
// Falls back to the generic path only when the delivery names no App, so a key
// is never dropped on the floor.
func deliveredKeyPath(appID int64) string {
	if p := appKeys.PerAppIDKeyPath(appID); p != "" {
		return p
	}
	return appKeys.DataKeyPath
}

// describeAppKeyFailure turns a bare wrapped os error from github.NewAppAuth
// into a message an operator can act on without reading the source: it names
// the path actually tried, the full resolution order that produced it, and the
// underlying cause.
//
// The generic "reading app key /secrets/gh-app-key.pem: no such file" that this
// replaces gave no hint that key_file, $GH_APP_KEY_FILE, the PVC path and the
// provisioning mount are all consulted in a fixed order — so the usual response
// was to put the key in the wrong one of the four.
func describeAppKeyFailure(configured, envOverride, resolved string, err error) string {
	order := []string{
		fmt.Sprintf("$GH_APP_KEY_FILE=%s", describeKeySource(envOverride)),
		fmt.Sprintf("github.key_file=%s", describeKeySource(configured)),
		fmt.Sprintf("per-app-id PVC key %s/gh-app-key-<app_id>.pem", appKeys.DataDir),
		fmt.Sprintf("per-app-id provisioning key %s/gh-app-key-<app_id>.pem", appKeys.ProvisionedDir),
		fmt.Sprintf("PVC fallback %s", appKeys.DataKeyPath),
		fmt.Sprintf("provisioning mount %s", appKeys.ProvisionedKeyPath),
	}
	return fmt.Sprintf(
		"GitHub App private key could not be loaded from %q: %v. "+
			"Resolution order (first non-empty wins): %s. "+
			"Write a PEM-encoded RSA private key to that path, or point github.key_file at one.",
		resolved, err, strings.Join(order, " → "),
	)
}

// describeKeySource renders an unset key-file source as "(unset)" so the
// resolution order in describeAppKeyFailure reads unambiguously.
func describeKeySource(v string) string {
	if strings.TrimSpace(v) == "" {
		return "(unset)"
	}
	return v
}

// appKeyPaths snapshots App key path locations for a pkg/apphealth
// call. Read at call time on purpose: tests repoint these, and capturing them
// once would silently ignore that.
func appKeyPaths(appID int64) apphealth.KeyPaths {
	paths := apphealth.KeyPaths{Spoke: appKeys.DataKeyPath, Provisioned: appKeys.ProvisionedKeyPath}
	if appID > 0 {
		if p := appKeys.PerAppIDKeyPath(appID); p != "" {
			paths.Extra = append(paths.Extra, p)
		}
		if appKeys.ProvisionedDir != "" {
			paths.Extra = append(paths.Extra, filepath.Join(appKeys.ProvisionedDir, fmt.Sprintf("gh-app-key-%d.pem", appID)))
		}
	}
	return paths
}
