package dashboard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeOpencodeOnPath installs an `opencode` whose `run --help` prints help.
func fakeOpencodeOnPath(t *testing.T, help string) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\ncat <<'EOF_HELP'\n" + help + "\nEOF_HELP\n"
	if err := os.WriteFile(filepath.Join(dir, "opencode"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return "PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH")
}

// An opencode that rejects --auto ("Unrecognized flag: --auto in command
// opencode run") must not be launched with it.
func TestOpencodeAutoFlagOmittedWhenUnsupported(t *testing.T) {
	env := fakeOpencodeOnPath(t, "opencode run [message..]\n  --model  model to use")
	out, code := runBackendsConfFunc(t, "backend_perm_flag", []string{"opencode"}, env)
	if code != 0 || strings.Contains(out, "--auto") {
		t.Fatalf("unsupported opencode got --auto: code=%d out=%q", code, out)
	}
	out, code = runBackendsConfFunc(t, "opencode_local_perm_flag_shell", nil, env)
	if code != 0 || strings.Contains(out, "--auto") || !strings.Contains(out, "OPENCODE_PERMISSION=") {
		t.Fatalf("local mode must keep the deny-list but drop --auto: code=%d out=%q", code, out)
	}
}

func TestOpencodeAutoFlagKeptWhenSupported(t *testing.T) {
	env := fakeOpencodeOnPath(t, "opencode run [message..]\n  --auto  Automatically accept all permissions")
	out, code := runBackendsConfFunc(t, "backend_perm_flag", []string{"opencode"}, env)
	if code != 0 || !strings.Contains(out, "--auto") {
		t.Fatalf("supported opencode lost --auto: code=%d out=%q", code, out)
	}
}

// A failed run's feed entry carries a scrubbed, bounded reason.
func TestActivityFailureReasonOnFailedEntry(t *testing.T) {
	_, s := covK2Hub(t)
	long := "headless CLI exited with error: code 1: Unrecognized flag: --auto " + strings.Repeat("x", 400)
	s.contributeHub.addActivityWithReason(long, "alice", "failed", "", "opencode", "", "", "issue org/repo#1: t")
	s.contributeHub.addActivity("alice", "picked up", "", "opencode", "", "", "issue org/repo#1: t")
	got, _, _ := activityRequest(t, s, "")
	var failed, picked *ActivityEntry
	for i := range got {
		switch got[i].Action {
		case "failed":
			failed = &got[i]
		case "picked up":
			picked = &got[i]
		}
	}
	if failed == nil || picked == nil {
		t.Fatalf("missing entries: %+v", got)
	}
	if !strings.Contains(failed.Reason, "Unrecognized flag: --auto") {
		t.Fatalf("reason lost: %q", failed.Reason)
	}
	if n := len([]rune(failed.Reason)); n > 121 {
		t.Fatalf("reason not bounded: %d runes", n)
	}
	if picked.Reason != "" {
		t.Fatalf("non-failed entry carries a reason: %q", picked.Reason)
	}
}
