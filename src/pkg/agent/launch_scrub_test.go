package agent

// Tests for issue #3940 (CWE-532): an agent launched via a DIRECT launch_cmd
// override must have its stderr/stdout scrubbed of token-shaped values by the
// SAME machinery as the standard backend launch path — both in the Go sandbox
// executor (logscrub at the result boundary) and in bin/agent-launch.sh
// (--exec passthrough through the single scrub pipe).
//
// All token values below are SYNTHETIC — they match the credential regexes by
// shape only and correspond to no real credential.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/sandbox"
)

const (
	// syntheticToken matches logscrub's GitHub token pattern (ghs_ prefix,
	// >=10 token chars) and agent-launch.sh's sed pattern. Synthetic only.
	syntheticToken = "ghs_SYNTHETIC0000000000000000000000000000"
	// syntheticJWT matches the three-part eyJ… JWT shape. Synthetic only.
	syntheticJWT = "eyJhbGciOiJIUzI1NiIsInR5cCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.c3ludGhldGljc2lnbmF0dXJlMDAwMDAw"
	// plainMarker is a non-secret string that must survive scrubbing intact.
	plainMarker = "plain-diagnostic-line-must-pass-through"
)

// sandboxScrubLauncher returns a canned sandbox result and records the command
// the executor asked it to run, so tests can prove which launch path was used.
type sandboxScrubLauncher struct {
	result  sandbox.Result
	command []string
}

func (l *sandboxScrubLauncher) Run(_ context.Context, spec sandbox.LaunchSpec) (sandbox.Result, error) {
	l.command = append([]string(nil), spec.Command...)
	if err := os.WriteFile(filepath.Join(spec.Workspace, "agent-report.json"), []byte(`{"lane":"scanner","kind":"summary","findings":[],"prs_opened":[],"beads_filed":[],"summary":"ok"}`), 0o660); err != nil {
		return sandbox.Result{}, err
	}
	return l.result, nil
}

// TestSandboxExecutorScrubsLaunchOutput proves that token-shaped values
// emitted by the agent CLI never reach the kick result or the on-disk
// transcript — for the direct launch_cmd override path AND (positive control)
// the normal backend path — while non-secret output passes through unchanged.
func TestSandboxExecutorScrubsLaunchOutput(t *testing.T) {
	const directCmd = "/usr/bin/copilot --allow-all --model claude-test-model"
	tests := []struct {
		name        string
		cfg         configSnapshot
		wantCmdPart string
	}{
		{
			name:        "direct launch_cmd override",
			cfg:         configSnapshot{Backend: "copilot", LaunchCmd: directCmd},
			wantCmdPart: directCmd,
		},
		{
			name:        "normal backend path still scrubbed",
			cfg:         configSnapshot{Backend: "claude"},
			wantCmdPart: "claude",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			launcher := &sandboxScrubLauncher{result: sandbox.Result{
				Stdout: "starting\n" + syntheticToken + "\n" + plainMarker + "\n",
				Stderr: "auth failure: " + syntheticToken + " jwt=" + syntheticJWT + "\n" + plainMarker + "\n",
			}}
			execr := &SandboxExecutor{Runner: &sandboxFakeRunner{}, Launcher: launcher}
			res, err := execr.Run(context.Background(), SandboxKickSpec{
				Agent: "scanner", AgentConfig: tt.cfg, Message: "fix",
				Org: "kubestellar", Repo: "hive", WorkspaceDir: t.TempDir(), Image: "agent-image",
			})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			// The launch path under test must actually have been taken.
			if got := strings.Join(launcher.command, " "); !strings.Contains(got, tt.wantCmdPart) {
				t.Fatalf("sandbox command %q does not contain %q — wrong launch path exercised", got, tt.wantCmdPart)
			}
			transcript, err := os.ReadFile(res.TranscriptPath)
			if err != nil {
				t.Fatalf("reading transcript: %v", err)
			}
			for what, text := range map[string]string{
				"result stdout": res.Sandbox.Stdout,
				"result stderr": res.Sandbox.Stderr,
				"transcript":    string(transcript),
			} {
				if strings.Contains(text, syntheticToken) {
					t.Errorf("%s leaked the synthetic token: %q", what, text)
				}
				if strings.Contains(text, syntheticJWT) {
					t.Errorf("%s leaked the synthetic JWT: %q", what, text)
				}
				if !strings.Contains(text, "[REDACTED]") {
					t.Errorf("%s shows no redaction marker — scrub did not run: %q", what, text)
				}
				if !strings.Contains(text, plainMarker) {
					t.Errorf("%s dropped non-secret output: %q", what, text)
				}
			}
		})
	}
}

// agentLaunchScriptPath locates bin/agent-launch.sh relative to this package
// (src/pkg/agent → repo root → bin).
func agentLaunchScriptPath(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "..", "bin", "agent-launch.sh"))
	if err != nil {
		t.Fatalf("resolving script path: %v", err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Skipf("bin/agent-launch.sh not available: %v", err)
	}
	return p
}

func requireScriptDeps(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	// agent-launch.sh scrubs with `sed -u`; BSD sed on older macOS lacks it.
	if err := exec.Command("bash", "-c", "echo x | sed -u -E 's/x/y/' >/dev/null").Run(); err != nil {
		t.Skip("sed -u not supported on this platform")
	}
}

// runAgentLaunch runs bin/agent-launch.sh with the given args, returning its
// combined stderr and the path of the stderr log it wrote.
func runAgentLaunch(t *testing.T, agentID string, extraEnv []string, args ...string) (string, string) {
	t.Helper()
	script := agentLaunchScriptPath(t)
	stderrLog := "/tmp/.hive-launch-stderr-" + agentID + ".log"
	t.Cleanup(func() { _ = os.Remove(stderrLog) })
	cmd := exec.Command("bash", append([]string{script}, args...)...)
	cmd.Env = append(os.Environ(), append([]string{"HIVE_AGENT=" + agentID}, extraEnv...)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	cmd.Stdout = nil
	if err := cmd.Run(); err != nil {
		t.Fatalf("agent-launch.sh %v: %v\nstderr: %s", args, err, stderr.String())
	}
	return stderr.String(), stderrLog
}

func assertScrubbed(t *testing.T, what, text string) {
	t.Helper()
	if strings.Contains(text, syntheticToken) {
		t.Errorf("%s leaked the synthetic token: %q", what, text)
	}
	if !strings.Contains(text, "[REDACTED]") {
		t.Errorf("%s shows no redaction marker — scrub did not run: %q", what, text)
	}
	if !strings.Contains(text, plainMarker) {
		t.Errorf("%s dropped non-secret output: %q", what, text)
	}
}

// TestAgentLaunchScriptScrubsDirectExecStderr proves that a child launched via
// the direct-command passthrough (agent-launch.sh --exec …, the routing used
// for custom launch_cmd values) has token-shaped stderr scrubbed both on the
// live stderr stream and in the on-disk stderr log, while non-secret output
// passes through.
func TestAgentLaunchScriptScrubsDirectExecStderr(t *testing.T) {
	requireScriptDeps(t)
	child := "echo '" + syntheticToken + " " + plainMarker + "' >&2"
	stderr, stderrLog := runAgentLaunch(t, "scrubtest-direct", nil,
		"--exec", "bash", "-c", child)
	assertScrubbed(t, "wrapper stderr", stderr)
	logData, err := os.ReadFile(stderrLog)
	if err != nil {
		t.Fatalf("reading stderr log: %v", err)
	}
	assertScrubbed(t, "stderr log "+stderrLog, string(logData))
}

// TestAgentLaunchScriptScrubsBackendStderr is the positive control for the
// standard path: a normal --backend launch flows through the same scrub pipe.
// The backend binary is a PATH stub that emits a synthetic token on stderr.
func TestAgentLaunchScriptScrubsBackendStderr(t *testing.T) {
	requireScriptDeps(t)
	stubDir := t.TempDir()
	stub := "#!/bin/bash\necho '" + syntheticToken + " " + plainMarker + "' >&2\nexit 0\n"
	if err := os.WriteFile(filepath.Join(stubDir, "claude"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	stderr, stderrLog := runAgentLaunch(t, "scrubtest-backend",
		[]string{"PATH=" + stubDir + string(os.PathListSeparator) + os.Getenv("PATH")},
		"--backend", "claude", "--model", "claude-test-model")
	assertScrubbed(t, "wrapper stderr", stderr)
	logData, err := os.ReadFile(stderrLog)
	if err != nil {
		t.Fatalf("reading stderr log: %v", err)
	}
	assertScrubbed(t, "stderr log "+stderrLog, string(logData))
}
