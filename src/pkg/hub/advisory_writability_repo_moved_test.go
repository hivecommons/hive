package hub

import "testing"

// #5774. appCanWriteForAdvisory decides whether a stale advisory digest is a
// FAULT worth flagging or simply the consequence of an App that cannot write.
// The repo-moved state is the second: it is classified only when the
// installation covers NOTHING under the hive's configured owner (a required
// clause of InstallationCoverage.MovedTo), so no repository this hive is
// pointed at — the advisory repo included — is writable.
//
// Reading it as writable would flag the hive for a silent digest it had no way
// to post, which is the misattribution this whole state exists to stop.
func TestAppCanWriteForAdvisory_RepoMovedIsNotWritable(t *testing.T) {
	if appCanWriteForAdvisory(RegistryEntry{GitHubAppState: "repo-moved"}) {
		t.Error("repo-moved read as writable: the installation covers nothing under the configured owner, so the digest cannot land and a stale flag would blame the hive for our own migration")
	}
}

// TestAppCanWriteForAdvisory_RepoNotCoveredStaysWritable pins the deliberate
// asymmetry, so the omission next to it does not read as an oversight and get
// "fixed" into a false negative.
//
// repo-not-covered fires when ANY configured repo is unticked, which need not
// be the advisory repo. A hive with two repos, one of them unticked, can still
// be posting digests perfectly well to the other — so this state is not
// evidence the digest cannot land, and treating it as such would silence a real
// wedged-digest alarm.
func TestAppCanWriteForAdvisory_RepoNotCoveredStaysWritable(t *testing.T) {
	if !appCanWriteForAdvisory(RegistryEntry{GitHubAppState: "repo-not-covered"}) {
		t.Error("repo-not-covered read as non-writable: it names an unticked repo that need not be the advisory one, so it cannot prove the digest is blocked")
	}
}
