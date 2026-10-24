package agent

import (
	"os"
	"strings"
	"testing"
)

func TestEntrypointGeminiGuardIsBoundedToAntigravityCLI(t *testing.T) {
	body, err := os.ReadFile("../../deploy/entrypoint.sh")
	if err != nil {
		t.Fatalf("read entrypoint: %v", err)
	}
	script := string(body)
	want := "hive_guard_forever gemini /data/home/.gemini/antigravity-cli/ close_write,moved_to,create hive_fix_gemini_instant &"
	if !strings.Contains(script, want) {
		t.Fatalf("gemini guard must watch agy's bounded state directory non-recursively; missing %q", want)
	}
	bad := "hive_guard_forever gemini /data/home/.gemini/ close_write,moved_to,create hive_fix_gemini_instant -r"
	if strings.Contains(script, bad) {
		t.Fatalf("gemini guard still recursively watches .gemini, so agy's brain/ churn can crash-loop it")
	}
	if !strings.Contains(script, "HIVE_WATCH_ERROR=\"$(inotifywait") || !strings.Contains(script, "watcher exited on $_dir${_why}") {
		t.Fatalf("inotify guard failures must preserve stderr in the retry warning")
	}
}
