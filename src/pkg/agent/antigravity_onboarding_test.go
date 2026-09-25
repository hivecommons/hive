package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEntrypointRepairsAgyOnboardingFastPath(t *testing.T) {
	body, err := os.ReadFile("../../deploy/entrypoint.sh")
	if err != nil {
		t.Fatalf("read entrypoint: %v", err)
	}
	script := string(body)
	for _, want := range []string{
		"/data/home/.gemini/antigravity-cli/cache",
		"hive_fix_shared_credential /data/home/.gemini/antigravity-cli/cache/onboarding.json",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("entrypoint must include agy onboarding in fast permission repair; missing %q", want)
		}
	}
}

func TestPermissionsWatcher_AntigravityOnboardingState(t *testing.T) {
	resetPermWarnDedupe()
	dir := t.TempDir()
	geminiRoot := filepath.Join(dir, ".gemini")
	cacheDir := filepath.Join(geminiRoot, "antigravity-cli", "cache")
	onboarding := filepath.Join(cacheDir, agyOnboardingStateBase)

	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(onboarding, []byte(`{"theme":"dark"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	origW, origG := WatchedHomeDirs, GooseLogsDir
	WatchedHomeDirs = []string{geminiRoot}
	GooseLogsDir = filepath.Join(dir, "goose", "logs")
	t.Cleanup(func() { WatchedHomeDirs = origW; GooseLogsDir = origG })

	ensureWatchedDirs(discardLogger())
	fixPermissions(discardLogger())

	fi, err := os.Stat(onboarding)
	if err != nil {
		t.Fatalf("stat onboarding: %v", err)
	}
	if fi.Mode().Perm()&0o040 == 0 {
		t.Fatalf("agy onboarding state left at %v — the next agent UID will see first-run onboarding", fi.Mode().Perm())
	}
	if fi.Mode().Perm()&0o020 != 0 {
		t.Errorf("agy onboarding state granted group write (%v); read is all the fleet needs", fi.Mode().Perm())
	}
}
