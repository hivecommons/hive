package agent

// Covers the fallback machinery of claude_session_adopt.go (#4637) that the
// seeding-path tests never reach: the PRODUCTION runAsAgentUser body (exercised
// against a fake su-exec on PATH), the stage-and-rename write fallback in
// writeClaudeSessionForAgent, the post-read size-cap re-check in
// readClaudeSessionAsOwner, and the errors.Join paths that must preserve BOTH
// the direct-I/O error and the helper error for diagnoseStuckLogin.

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// withFakeSuExec prepends a directory holding a fake su-exec to PATH. The fake
// drops the "uid:gid" spec argument and execs the rest of argv directly, which
// is exactly the observable contract production relies on: spec first, command
// after, stdin/stdout/stderr passed through.
func withFakeSuExec(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nshift\nexec \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "su-exec"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// swapAgentUserRunner replaces the runAsAgentUser seam for one test.
func swapAgentUserRunner(t *testing.T, fn func(spec string, stdin []byte, argv ...string) ([]byte, error)) {
	t.Helper()
	old := runAsAgentUser
	runAsAgentUser = fn
	t.Cleanup(func() { runAsAgentUser = old })
}

// --- the production su-exec runner ---------------------------------------------

func TestRunAsAgentUser_PassesStdinAndReturnsStdout(t *testing.T) {
	withFakeSuExec(t)
	out, err := runAsAgentUser("1001:1001", []byte("session-bytes"), "cat")
	if err != nil {
		t.Fatalf("runAsAgentUser = %v, want success via fake su-exec", err)
	}
	if got := string(out); got != "session-bytes" {
		t.Errorf("stdout = %q, want stdin echoed back", got)
	}
}

func TestRunAsAgentUser_FailureCarriesStderr(t *testing.T) {
	withFakeSuExec(t)
	_, err := runAsAgentUser("1001:1001", nil, "sh", "-c", "echo boom >&2; exit 3")
	if err == nil {
		t.Fatal("a failing helper must surface an error")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error %q should carry the helper's stderr", err)
	}
	if !strings.Contains(err.Error(), "1001:1001") {
		t.Errorf("error %q should name the user spec it ran as", err)
	}
}

func TestRunAsAgentUser_SuExecAbsentFailsClosed(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // nothing on PATH at all
	_, err := runAsAgentUser("1001:1001", nil, "cat", "/dev/null")
	if err == nil || !strings.Contains(err.Error(), "su-exec unavailable") {
		t.Errorf("err = %v, want the su-exec-unavailable refusal", err)
	}
}

// --- owner spec refusals ---------------------------------------------------------

func TestClaudeSessionOwnerSpec_RefusesRootOwnedFile(t *testing.T) {
	const path = "/etc/hostname"
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		t.Skipf("no root-owned regular file to probe at %s", path)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != 0 {
		t.Skipf("%s is not uid-0 owned here", path)
	}
	if _, err := claudeSessionOwnerSpec(path); err == nil ||
		!strings.Contains(err.Error(), "uid 0") {
		t.Errorf("err = %v, want the refusal to run a helper as root", err)
	}
}

// --- owner reads -----------------------------------------------------------------

func TestReadClaudeSessionAsOwner_SpecFailureShortCircuits(t *testing.T) {
	m := interactiveHomeTestManager(t)
	called := false
	swapAgentUserRunner(t, func(string, []byte, ...string) ([]byte, error) {
		called = true
		return nil, nil
	})
	if _, err := m.readClaudeSessionAsOwner(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Error("a path with no owner spec must not be readable")
	}
	if called {
		t.Error("no helper may run for a path whose owner spec was refused")
	}
}

func TestReadClaudeSessionAsOwner_RechecksSizeCapAfterRead(t *testing.T) {
	withSharedAgentHome(t)
	m := interactiveHomeTestManager(t)
	path := writeUnreadableSession(t, interactiveHomePath("guide"), signedInClaudeSession)

	// The Lstat cap passes (file is tiny) but the helper returns a payload
	// that grew past the cap between stat and read.
	swapAgentUserRunner(t, func(string, []byte, ...string) ([]byte, error) {
		return bytes.Repeat([]byte("x"), claudeSessionAdoptMaxBytes+1), nil
	})
	_, err := m.readClaudeSessionAsOwner(path)
	if err == nil || !strings.Contains(err.Error(), "cap") {
		t.Errorf("err = %v, want the post-read adoption-cap refusal", err)
	}
}

func TestReadClaudeSessionForAdoption_JoinsDirectAndHelperErrors(t *testing.T) {
	withSharedAgentHome(t)
	m := interactiveHomeTestManager(t)
	path := writeUnreadableSession(t, interactiveHomePath("guide"), signedInClaudeSession)

	helperErr := errors.New("helper wedged")
	swapAgentUserRunner(t, func(string, []byte, ...string) ([]byte, error) {
		return nil, helperErr
	})
	_, err := m.readClaudeSessionForAdoption(path)
	if err == nil {
		t.Fatal("both reads failed; an error is required")
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Errorf("joined error %v must preserve the direct EACCES", err)
	}
	if !errors.Is(err, helperErr) {
		t.Errorf("joined error %v must preserve the helper failure", err)
	}
}

// --- the write fallback ----------------------------------------------------------

func TestWriteClaudeSessionForAgent_DirectWriteNeedsNoHelper(t *testing.T) {
	m := interactiveHomeTestManager(t)
	target := filepath.Join(t.TempDir(), ".claude.json")
	called := false
	swapAgentUserRunner(t, func(string, []byte, ...string) ([]byte, error) {
		called = true
		return nil, nil
	})
	if err := m.writeClaudeSessionForAgent(target, []byte(signedInClaudeSession), "1001:1001"); err != nil {
		t.Fatalf("direct write = %v, want success", err)
	}
	if called {
		t.Error("a writable target must never invoke the su-exec helper")
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != signedInClaudeSession {
		t.Errorf("target = %q, %v; want the seeded session", data, err)
	}
}

func TestWriteClaudeSessionForAgent_NonPermissionErrorIsNotRetried(t *testing.T) {
	m := interactiveHomeTestManager(t)
	called := false
	swapAgentUserRunner(t, func(string, []byte, ...string) ([]byte, error) {
		called = true
		return nil, nil
	})
	target := filepath.Join(t.TempDir(), "no-such-dir", ".claude.json")
	err := m.writeClaudeSessionForAgent(target, []byte("{}"), "1001:1001")
	if err == nil || errors.Is(err, fs.ErrPermission) {
		t.Fatalf("err = %v, want a non-permission direct failure", err)
	}
	if called {
		t.Error("only EACCES may fall back to the agent-identity write")
	}
}

func TestWriteClaudeSessionForAgent_EmptySpecCannotFallBack(t *testing.T) {
	withSharedAgentHome(t)
	m := interactiveHomeTestManager(t)
	target := writeUnreadableSession(t, interactiveHomePath("scanner"), skeletonClaudeSession)
	called := false
	swapAgentUserRunner(t, func(string, []byte, ...string) ([]byte, error) {
		called = true
		return nil, nil
	})
	err := m.writeClaudeSessionForAgent(target, []byte(signedInClaudeSession), "")
	if !errors.Is(err, fs.ErrPermission) {
		t.Errorf("err = %v, want the direct EACCES back unchanged", err)
	}
	if called {
		t.Error("an empty user spec must never reach the helper")
	}
}

func TestWriteClaudeSessionForAgent_FallbackStagesThroughOwnedTemp(t *testing.T) {
	withSharedAgentHome(t)
	m := interactiveHomeTestManager(t)
	target := writeUnreadableSession(t, interactiveHomePath("scanner"), skeletonClaudeSession)

	var gotSpec string
	var gotArgv []string
	var gotStdin []byte
	swapAgentUserRunner(t, func(spec string, stdin []byte, argv ...string) ([]byte, error) {
		gotSpec, gotStdin = spec, append([]byte(nil), stdin...)
		gotArgv = append([]string(nil), argv...)
		return nil, nil
	})
	if err := m.writeClaudeSessionForAgent(target, []byte(signedInClaudeSession), "1001:1001"); err != nil {
		t.Fatalf("fallback write = %v, want success", err)
	}
	if gotSpec != "1001:1001" {
		t.Errorf("helper ran as %q, want the target agent's spec", gotSpec)
	}
	if string(gotStdin) != signedInClaudeSession {
		t.Errorf("helper stdin = %q, want the session bytes", gotStdin)
	}
	// sh -c <script> <target>: the target must ride as $0 (data), never be
	// interpolated into the script (code).
	if len(gotArgv) != 4 || gotArgv[0] != "sh" || gotArgv[1] != "-c" {
		t.Fatalf("helper argv = %v, want sh -c <script> <target>", gotArgv)
	}
	if gotArgv[3] != target {
		t.Errorf("script $0 = %q, want target %q", gotArgv[3], target)
	}
	if strings.Contains(gotArgv[2], target) {
		t.Errorf("script %q interpolates the target path; it must be passed as $0", gotArgv[2])
	}
	if !strings.Contains(gotArgv[2], "mv -f") || !strings.Contains(gotArgv[2], "rm -f") {
		t.Errorf("script %q should stage-and-rename with cleanup on failure", gotArgv[2])
	}
}

func TestWriteClaudeSessionForAgent_FallbackFailureJoinsBothErrors(t *testing.T) {
	withSharedAgentHome(t)
	m := interactiveHomeTestManager(t)
	target := writeUnreadableSession(t, interactiveHomePath("scanner"), skeletonClaudeSession)

	helperErr := errors.New("helper died mid-write")
	swapAgentUserRunner(t, func(string, []byte, ...string) ([]byte, error) {
		return nil, helperErr
	})
	err := m.writeClaudeSessionForAgent(target, []byte(signedInClaudeSession), "1001:1001")
	if err == nil {
		t.Fatal("both writes failed; an error is required")
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Errorf("joined error %v must preserve the direct EACCES", err)
	}
	if !errors.Is(err, helperErr) {
		t.Errorf("joined error %v must preserve the helper failure", err)
	}
}
