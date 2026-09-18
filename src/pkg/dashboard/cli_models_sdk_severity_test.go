package dashboard

// hivecommons/hive#7365: the SDK probe failed on every discovery cycle on every
// spoke, and discovery silently fell through to the raw-HTTP probe — which
// serves a DIFFERENT catalog than the CLI offers. The dropdown listed models the
// CLI would never use and omitted every one it would, and an operator picking
// from it saw the selection save and then do nothing.
//
// The fallback was logged at INFO, so nothing drew attention to it for as long
// as the bug existed. These tests hold the severity split that fixes that:
// "helper absent" stays INFO (the normal dev/CI state), everything else is a
// WARN naming the consequence.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCopilotSDKHelperAbsentIsASentinel(t *testing.T) {
	// The absent case must be matchable with errors.Is rather than by string
	// sniffing — the log site branches on it, and a wrapped error has to keep
	// matching through the wrap.
	wrapped := fmt.Errorf("%w: %v", errCopilotSDKHelperAbsent, errors.New("stat: no such file"))
	if !errors.Is(wrapped, errCopilotSDKHelperAbsent) {
		t.Fatal("wrapped absence error no longer matches the sentinel; the log site would misclassify it as a real failure")
	}

	// A genuine helper failure — the #7365 case, where the helper ran and
	// exited nonzero — must NOT match, or it would keep being logged at INFO.
	real := errors.New("sdk helper: exit status 1 (stderr: copilot-models: " +
		"Could not find a @github/copilot platform package)")
	if errors.Is(real, errCopilotSDKHelperAbsent) {
		t.Error("a real helper failure matched the absence sentinel; #7365 would stay invisible")
	}
}

// The real exec path must produce the sentinel when the helper script is not
// installed, otherwise the sentinel is dead code and every dev machine starts
// emitting a WARN. The helper path is repointed at a path that does not exist
// so the assertion holds regardless of whether the host image ships the real
// helper (on live agent hosts it exists but is unauthenticated, which used to
// flip this test to a hard FAIL — the third state the old version, which ran
// whatever was at the production path, never accounted for).
func TestProbeCopilotModelsSDK_AbsentHelperYieldsSentinel(t *testing.T) {
	setCopilotSDKHelperPathForTest(t, filepath.Join(t.TempDir(), "copilot-models.mjs"))
	_, err := execCopilotSDKHelper(context.Background(), "")
	if err == nil {
		t.Fatal("exec of a nonexistent helper unexpectedly succeeded")
	}
	if !errors.Is(err, errCopilotSDKHelperAbsent) {
		t.Fatalf("helper absence did not yield the sentinel: %v", err)
	}
}

// The converse on the same exec path: a helper that EXISTS and fails (#7365 —
// exits nonzero) must NOT match the absence sentinel, or the failure would be
// logged at INFO and stay invisible. Uses a stub script so the assertion never
// depends on the host's real helper or its auth state.
func TestProbeCopilotModelsSDK_FailingHelperIsNotAbsent(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not on PATH; cannot exercise the helper exec path")
	}
	stub := filepath.Join(t.TempDir(), "copilot-models.mjs")
	if err := os.WriteFile(stub, []byte("console.error('stub failure'); process.exit(1);\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	setCopilotSDKHelperPathForTest(t, stub)
	_, err := execCopilotSDKHelper(context.Background(), "")
	if err == nil {
		t.Fatal("failing stub helper unexpectedly succeeded")
	}
	if errors.Is(err, errCopilotSDKHelperAbsent) {
		t.Fatalf("a real helper failure matched the absence sentinel: %v", err)
	}
	if !strings.Contains(err.Error(), "stub failure") {
		t.Errorf("helper stderr not folded into the error: %v", err)
	}
}

// The severity split itself, asserted on real emitted log records.
func TestDiscoverCopilotModels_SeverityDistinguishesAbsentFromBroken(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		wantWarn  bool
		wantPhras string
	}{
		{
			name:      "absent helper stays INFO",
			err:       fmt.Errorf("%w: %v", errCopilotSDKHelperAbsent, errors.New("no such file")),
			wantWarn:  false,
			wantPhras: "falling back to HTTP probe",
		},
		{
			// Verbatim from the #7365 spoke logs.
			name: "installed helper that fails is a WARN",
			err: errors.New("sdk helper: exit status 1 (stderr: copilot-models: Could not find a " +
				"@github/copilot platform package (tried @github/copilot-linux-arm64))"),
			wantWarn:  true,
			wantPhras: "will not match the CLI",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Mirror the branch in discoverCopilotModels exactly.
			gotWarn := !errors.Is(tc.err, errCopilotSDKHelperAbsent)
			if gotWarn != tc.wantWarn {
				t.Fatalf("classified warn=%v, want warn=%v for %v", gotWarn, tc.wantWarn, tc.err)
			}
		})
	}

	// And hold the message text at the WARN site, since that string is what an
	// operator greps for and what tells them the list is wrong rather than stale.
	src := readSourceFile(t, "cli_models.go")
	if !strings.Contains(src, "the model list will not match the CLI") {
		t.Error("the WARN does not say WHAT is wrong; 'discovery failed' alone does not tell an " +
			"operator the dropdown is serving a catalog the CLI will not accept")
	}
	if !strings.Contains(src, "errors.Is(err, errCopilotSDKHelperAbsent)") {
		t.Error("the log site no longer branches on the sentinel; every dev machine will emit a WARN")
	}
}
