package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestBobLaunchCmdRunsBehindEnvPrefix executes the real tmux launch shape —
// KEY='value' assignments followed by bobLaunchCmd — through bash against a
// fake bob. Before the sh -c wrap, `A='1' case ... esac` was a bash syntax
// error and bob never launched on any hive.
func TestBobLaunchCmdRunsBehindEnvPrefix(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}

	if _, err := os.Stat("../../../config/backends.conf"); err != nil {
		t.Skip("monorepo file config/backends.conf not present (standalone crux checkout)")
	}

	cases := []struct {
		version  string
		wantArgs string
	}{
		{"2.0.6", "chat --accept-license --auto-approve --trust"},
		{"1.0.6", "--accept-license --auth-method api-key --approval-mode yolo --trust"},
		{"unknown", "chat --accept-license --auto-approve --trust"},
	}
	for _, tc := range cases {
		t.Run(tc.version, func(t *testing.T) {
			fake := filepath.Join(t.TempDir(), "bob")
			script := "#!/bin/sh\n" +
				"if [ \"$1\" = --version ]; then echo " + tc.version + "; echo commit: x; exit 0; fi\n" +
				"echo \"ARGS=$* HIVE_AGENT=$HIVE_AGENT\"\n"
			if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}

			// Exercise the contributor's actual flag helper against the same
			// fake CLI and expected argv as the hub's pod-agent launcher.
			conf, err := filepath.Abs("../../../config/backends.conf")
			if err != nil {
				t.Fatal(err)
			}
			contributor := exec.Command(bash, "-c", `source "$1"; eval "bob $(backend_perm_flag_shell bob)"`, "bash", conf)
			contributor.Env = append(os.Environ(), "PATH="+filepath.Dir(fake)+":"+os.Getenv("PATH"), "HIVE_AGENT=quality")
			out, err := contributor.CombinedOutput()
			want := "ARGS=" + tc.wantArgs + " HIVE_AGENT=quality"
			if err != nil || strings.TrimSpace(string(out)) != want {
				t.Fatalf("contributor launch: err=%v output=%q, want %q", err, out, want)
			}

			line := shellEnvVar("HIVE_AGENT", "quality") + " " + bobLaunchCmd(fake)
			out, err = exec.Command(bash, "-c", line).CombinedOutput()
			if err != nil {
				t.Fatalf("launch line failed: %v\nline: %s\noutput: %s", err, line, out)
			}
			if got := strings.TrimSpace(string(out)); got != want {
				t.Errorf("output = %q, want %q", got, want)
			}
		})
	}
}

// Run the real contributor startup key check without starting tmux or a relay.
func TestContributorBobKeyCompatibility(t *testing.T) {
	startup, err := os.ReadFile("../../../bin/contributor-agent.sh")
	if os.IsNotExist(err) {
		t.Skip("monorepo file bin/contributor-agent.sh not present (standalone crux checkout)")
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat("../../../config/backends.conf"); err != nil {
		t.Skip("monorepo file config/backends.conf not present (standalone crux checkout)")
	}
	start := strings.Index(string(startup), "# Fail before an unattended Bob")
	end := strings.Index(string(startup), "# Ensure metrics directory")
	if start < 0 || end <= start {
		t.Fatal("contributor Bob startup check not found")
	}
	for _, tc := range []struct {
		name, current, legacy, want string
		wantFailure                 bool
	}{
		{"current", "current-key", "", "current-key/current-key", false},
		{"legacy", "", "legacy-key", "legacy-key/legacy-key", false},
		{"both", "current-key", "legacy-key", "current-key/legacy-key", false},
		{"missing", "", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script := `set -eu; source ../../../config/backends.conf; AGENT_BACKEND=bob; BOB_API_KEY=$1; BOBSHELL_API_KEY=$2; ` +
				string(startup[start:end]) + `bash -c 'printf "%s/%s" "$BOB_API_KEY" "$BOBSHELL_API_KEY"'`
			out, err := exec.Command("bash", "-c", script, "bash", tc.current, tc.legacy).CombinedOutput()
			if tc.wantFailure {
				if err == nil || !strings.Contains(string(out), "requires BOB_API_KEY") {
					t.Fatalf("missing key: err=%v output=%q", err, out)
				}
				return
			}
			if err != nil || string(out) != tc.want {
				t.Fatalf("key aliases: err=%v output=%q, want %q", err, out, tc.want)
			}
		})
	}
}
