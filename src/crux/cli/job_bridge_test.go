package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/sandbox/substratebridge"
)

// jobBridgeTokenEnv is a variable name no host sets, so the tests control
// whether the bridge sees a token without depending on the ambient env.
const jobBridgeTokenEnv = "CRUX_TEST_JOB_BRIDGE_TOKEN"

func runJobBridge(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	e := &Env{Stdout: &stdout, Stderr: &stderr}
	code := e.Run(context.Background(), append([]string{JobBridgeCommand}, args...))
	return code, stdout.String(), stderr.String()
}

// TestJobBridgeRequiresCommand: the Job's container command must name what
// to run after "--"; without one the bridge exits with usage, before it
// reads the token or opens a listener.
func TestJobBridgeRequiresCommand(t *testing.T) {
	t.Setenv(jobBridgeTokenEnv, "dummy-token")
	code, stdout, stderr := runJobBridge(t, "-token-env", jobBridgeTokenEnv, "-listen", "127.0.0.1:0")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d (stderr %q)", code, ExitUsage, stderr)
	}
	if !strings.Contains(stderr, "usage: crux job-bridge") || stdout != "" {
		t.Fatalf("stdout = %q, stderr = %q; want the job-bridge usage line on stderr", stdout, stderr)
	}
}

// TestJobBridgeRefusesEmptyToken: the bridge must never serve the workspace
// unauthenticated, so an unset or empty token variable is a usage error that
// names the variable.
func TestJobBridgeRefusesEmptyToken(t *testing.T) {
	t.Setenv(jobBridgeTokenEnv, "")
	code, _, stderr := runJobBridge(t, "-token-env", jobBridgeTokenEnv, "-listen", "127.0.0.1:0", "--", "true")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d (stderr %q)", code, ExitUsage, stderr)
	}
	if !strings.Contains(stderr, "$"+jobBridgeTokenEnv+" is empty; refusing to serve without a token") {
		t.Fatalf("stderr = %q; want the empty-token refusal naming $%s", stderr, jobBridgeTokenEnv)
	}
}

// TestJobBridgeListenError: an address the bridge cannot bind is a plain
// error, not a usage error.
func TestJobBridgeListenError(t *testing.T) {
	t.Setenv(jobBridgeTokenEnv, "dummy-token")
	code, _, stderr := runJobBridge(t, "-token-env", jobBridgeTokenEnv, "-listen", "127.0.0.1:-1", "--", "true")
	if code != ExitError {
		t.Fatalf("exit = %d, want %d (stderr %q)", code, ExitError, stderr)
	}
	if !strings.HasPrefix(stderr, "crux: job-bridge: ") {
		t.Fatalf("stderr = %q; want the listen error prefixed with crux: job-bridge:", stderr)
	}
}

// TestJobBridgeServesUntilWaitExpires: with a token and a command the
// subcommand hands the parsed flags to the kick server, which gives up with
// ExitNoWorkspace once -wait passes without hive pushing the workspace. The
// reported duration proves -wait reached the server.
func TestJobBridgeServesUntilWaitExpires(t *testing.T) {
	t.Setenv(jobBridgeTokenEnv, "dummy-token")
	code, _, stderr := runJobBridge(t,
		"-token-env", jobBridgeTokenEnv,
		"-listen", "127.0.0.1:0",
		"-workspace", t.TempDir(),
		"-wait", "20ms",
		"--", "true")
	if code != substratebridge.ExitNoWorkspace {
		t.Fatalf("exit = %d, want %d (stderr %q)", code, substratebridge.ExitNoWorkspace, stderr)
	}
	if !strings.Contains(stderr, "did not push the workspace within 20ms") {
		t.Fatalf("stderr = %q; want the no-push timeout reporting the -wait value", stderr)
	}
}
