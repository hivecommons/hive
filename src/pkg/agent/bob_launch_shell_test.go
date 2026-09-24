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

	cases := []struct {
		version  string
		wantArgs string
	}{
		{"2.0.4", "chat --accept-license --auto-approve --trust"},
		{"1.0.6", "--accept-license --auth-method api-key --approval-mode yolo --trust"},
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

			line := shellEnvVar("HIVE_AGENT", "quality") + " " + bobLaunchCmd(fake)
			out, err := exec.Command(bash, "-c", line).CombinedOutput()
			if err != nil {
				t.Fatalf("launch line failed: %v\nline: %s\noutput: %s", err, line, out)
			}
			want := "ARGS=" + tc.wantArgs + " HIVE_AGENT=quality"
			if got := strings.TrimSpace(string(out)); got != want {
				t.Errorf("output = %q, want %q", got, want)
			}
		})
	}
}
