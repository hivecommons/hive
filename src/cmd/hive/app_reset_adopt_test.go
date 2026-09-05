package main

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	hub "github.com/hivecommons/hive/pkg/hub/spoke"
)

// These tests drive nextInstallationID — the REAL decision in main.go — not a
// restatement of it. An earlier version defined its own copy of the rule and
// stayed green when the production branch was disabled entirely, which is the
// same "tested the thing beside the change" flaw that let four checks pass
// while testing nothing this week.
func applyInstallation(cur config.GitHubConfig, ghCfg *hub.HeartbeatGitHubAppConfig) config.GitHubConfig {
	next := cur
	next.InstallationID, _ = nextInstallationID(cur.InstallationID, ghCfg)
	return next
}

// TestPushedZeroStillCannotBlankAnInstallation is the guard the reset feature
// must not weaken.
//
// Zero on the wire means "the hub is not speaking to this field". The
// cluster-wide key reconcile sends zero on every beat, because it repairs KEYS
// on hives whose installation the hub does not track. If zero were read as
// "clear it", that reconcile would blank a working installation on every hive
// it touches — turning a key-only fault into a total auth outage.
func TestPushedZeroStillCannotBlankAnInstallation(t *testing.T) {
	cur := config.GitHubConfig{AppID: config.PublicGitHubAppID, InstallationID: 145050760}

	got := applyInstallation(cur, &hub.HeartbeatGitHubAppConfig{AppID: config.PublicGitHubAppID})
	if got.InstallationID != 145050760 {
		t.Fatalf("a pushed zero blanked a working installation (%d) — this is the outage the guard prevents", got.InstallationID)
	}
}

// TestResetFlagClearsTheInstallation: only the explicit flag may zero it.
// Clearing makes HasUsableApp() false, which raises githubAppRequired — the
// prompt to install the App again, and the trigger for the self-heal ticker
// whose RediscoverAndAdopt finds the right installation.
func TestResetFlagClearsTheInstallation(t *testing.T) {
	cur := config.GitHubConfig{AppID: config.PublicGitHubAppID, InstallationID: 145050760}
	if !cur.HasUsableApp() {
		t.Fatal("precondition: the hive should start with a usable App")
	}

	got := applyInstallation(cur, &hub.HeartbeatGitHubAppConfig{ResetInstallation: true})
	if got.InstallationID != 0 {
		t.Fatalf("reset did not clear the installation: %d", got.InstallationID)
	}
	if got.HasUsableApp() {
		t.Fatal("App still reads as usable after the reset; the owner would never be prompted to reinstall")
	}
	// The rest of the identity must survive — a reset costs one re-install,
	// not an identity rebuild.
	if got.AppID != cur.AppID {
		t.Fatalf("reset changed app_id: %d -> %d", cur.AppID, got.AppID)
	}
}

// TestResetWins: a delivery carrying BOTH a reset and an installation must
// reset. The hub never sends both, but the spoke should not depend on that.
func TestResetWins(t *testing.T) {
	cur := config.GitHubConfig{AppID: config.PublicGitHubAppID, InstallationID: 145050760}
	got := applyInstallation(cur, &hub.HeartbeatGitHubAppConfig{
		ResetInstallation: true, InstallationID: 999999,
	})
	if got.InstallationID != 0 {
		t.Fatalf("installation %d was adopted despite a reset", got.InstallationID)
	}
}
