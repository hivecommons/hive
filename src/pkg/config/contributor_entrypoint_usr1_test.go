package config

import (
	"strings"
	"testing"
)

// hivecommons/hive#12225: `hivectl hives use` sends USR1 to the container's
// PID 1 (the entrypoint), but the relay's SIGUSR1 handler lives in the child.
// The entrypoint must trap USR1, forward it to the relay, and keep waiting
// (a trapped signal makes `wait` return early).
func TestContributorEntrypointForwardsUSR1ToRelay(t *testing.T) {
	entrypoint := readRepoFile(t, "bin", "contributor-agent.sh")
	for _, want := range []string{
		`kill -USR1 "$RELAY_PID"`,
		"trap forward_usr1 SIGUSR1",
		`kill -0 "$RELAY_PID"`,
		"trap cleanup SIGTERM SIGINT",
	} {
		if !strings.Contains(entrypoint, want) {
			t.Fatalf("bin/contributor-agent.sh missing %q — USR1 from `hivectl hives use` would be dropped by PID 1 (hivecommons/hive#12225)", want)
		}
	}
	if strings.Contains(entrypoint, "\nwait \"$RELAY_PID\"\n") {
		t.Fatal("bare `wait \"$RELAY_PID\"` returns early on a trapped signal; it must be in a re-wait loop (hivecommons/hive#12225)")
	}
}
