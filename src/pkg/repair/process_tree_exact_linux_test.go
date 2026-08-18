//go:build linux

package repair

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	exactLinuxSacrificialParentEnv = "HIVE_TEST_EXACT_LINUX_SACRIFICIAL_PARENT"
	exactLinuxBubblewrapEnv        = "HIVE_TEST_EXACT_LINUX_BWRAP"
	exactLinuxRootEnv              = "HIVE_TEST_EXACT_LINUX_ROOT"
	exactLinuxMarkerEnv            = "HIVE_TEST_EXACT_LINUX_MARKER"
	exactLinuxReadyEnv             = "HIVE_TEST_EXACT_LINUX_READY"
)

func TestExactLinuxProcessTreeReapsDetachedClosedHandleDescendantOnLeaderExit(t *testing.T) {
	helper := exactLinuxTestBubblewrap(t)
	root := t.TempDir()
	marker := filepath.Join(root, "normal-heartbeat")
	command := detachedLinuxHeartbeatCommand(context.Background(), root, marker, false)
	wait, err := startExactRepairProcessTree(context.Background(), command, helper, codexProviderFileIdentity{})
	if err != nil {
		t.Fatalf("start exact process tree: %v", err)
	}
	if err := wait(); err != nil {
		skipUnavailableExactLinuxBoundary(t, err, command)
		t.Fatalf("wait exact process tree: %v: %s", err, command.Stderr.(*bytes.Buffer).String())
	}
	assertLinuxHeartbeatStopped(t, marker)
}

func TestExactLinuxProcessTreeReapsDetachedClosedHandleDescendantOnCancellation(t *testing.T) {
	helper := exactLinuxTestBubblewrap(t)
	root := t.TempDir()
	marker := filepath.Join(root, "cancel-heartbeat")
	ctx, cancel := context.WithCancel(context.Background())
	command := detachedLinuxHeartbeatCommand(ctx, root, marker, true)
	wait, err := startExactRepairProcessTree(ctx, command, helper, codexProviderFileIdentity{})
	if err != nil {
		t.Fatalf("start exact process tree: %v", err)
	}
	waitForLinuxHeartbeat(t, marker)
	cancel()
	if err := wait(); err == nil {
		t.Fatal("canceled exact process-tree leader exited successfully")
	}
	assertLinuxHeartbeatStopped(t, marker)
}

func TestExactLinuxProcessTreeDiesWithSacrificialHiveParent(t *testing.T) {
	helper := exactLinuxTestBubblewrap(t)
	root := t.TempDir()
	marker := filepath.Join(root, "crash-heartbeat")
	ready := filepath.Join(root, "parent-ready")
	command := exec.Command(os.Args[0], "-test.run=^TestExactLinuxProcessTreeSacrificialParentHelper$")
	command.Env = append(os.Environ(),
		exactLinuxSacrificialParentEnv+"=1",
		exactLinuxBubblewrapEnv+"="+helper.Path,
		exactLinuxRootEnv+"="+root,
		exactLinuxMarkerEnv+"="+marker,
		exactLinuxReadyEnv+"="+ready,
	)
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	if !waitForExactLinuxFile(ready, 8*time.Second) {
		_ = command.Process.Kill()
		_ = command.Wait()
		detail := strings.ToLower(output.String())
		if strings.Contains(detail, "operation not permitted") || strings.Contains(detail, "creating new namespace failed") {
			t.Skipf("host policy correctly makes exact Linux containment unavailable: %s", strings.TrimSpace(detail))
		}
		t.Fatalf("sacrificial parent did not establish exact boundary: %s", output.String())
	}
	first := waitForLinuxHeartbeat(t, marker)
	time.Sleep(150 * time.Millisecond)
	second := waitForLinuxHeartbeat(t, marker)
	if second.Size() <= first.Size() {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatal("detached descendant was not live before sacrificial parent death")
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait()
	assertLinuxHeartbeatStopped(t, marker)
}

func TestExactLinuxProcessTreeSacrificialParentHelper(t *testing.T) {
	if os.Getenv(exactLinuxSacrificialParentEnv) != "1" {
		t.Skip("sacrificial helper only")
	}
	root := os.Getenv(exactLinuxRootEnv)
	marker := os.Getenv(exactLinuxMarkerEnv)
	ready := os.Getenv(exactLinuxReadyEnv)
	helperPath := os.Getenv(exactLinuxBubblewrapEnv)
	for _, value := range []string{root, marker, ready, helperPath} {
		if value == "" || !filepath.IsAbs(value) {
			t.Fatal("sacrificial exact-boundary input is invalid")
		}
	}
	helper, err := inspectCodexProviderFile(helperPath)
	if err != nil {
		t.Fatal(err)
	}
	command := detachedLinuxHeartbeatCommand(context.Background(), root, marker, true)
	if _, err := startExactRepairProcessTree(context.Background(), command, helper, codexProviderFileIdentity{}); err != nil {
		t.Fatal(err)
	}
	waitForLinuxHeartbeat(t, marker)
	if err := os.WriteFile(ready, []byte("ready\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {}
}

func TestExactLinuxProcessTreeRejectsChangedSealedHelperBeforePayloadLaunch(t *testing.T) {
	helper := exactLinuxTestBubblewrap(t)
	root := t.TempDir()
	copyPath := filepath.Join(root, "sealed-bwrap")
	data, err := os.ReadFile(helper.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(copyPath, data, 0o700); err != nil {
		t.Fatal(err)
	}
	sealed, err := inspectCodexProviderFile(copyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(copyPath, append(data, 0), 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(root, "payload-ran")
	command := exec.CommandContext(context.Background(), "/bin/sh", "-c", "touch -- \"$1\"", "sh", sentinel)
	command.Dir = root
	if _, err := startExactRepairProcessTree(context.Background(), command, sealed, codexProviderFileIdentity{}); err == nil ||
		(!strings.Contains(err.Error(), "changed") && !strings.Contains(err.Error(), "revalidate")) {
		t.Fatalf("changed helper did not fail closed before launch: %v", err)
	}
	if _, err := os.Stat(sentinel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("payload ran after helper identity drift: %v", err)
	}
}

func TestExactLinuxProcessTreeUsesAttestedCapabilityDropExecWhenAmbientCapabilityIsPresent(t *testing.T) {
	root := t.TempDir()
	helperPath := filepath.Join(root, "bwrap")
	dropperPath := filepath.Join(root, "setpriv")
	marker := filepath.Join(root, "payload-ran")
	if err := os.WriteFile(helperPath, []byte("#!/bin/sh\nwhile [ \"$1\" != \"--\" ]; do shift; done\nshift\nexec \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dropperPath, []byte("#!/bin/sh\n[ \"$1\" = \"--inh-caps=-all\" ] || exit 81\n[ \"$2\" = \"--ambient-caps=-all\" ] || exit 82\n[ \"$3\" = \"--\" ] || exit 83\nshift 3\nexec \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	helper, err := inspectCodexProviderFile(helperPath)
	if err != nil {
		t.Fatal(err)
	}
	dropper, err := inspectCodexProviderFile(dropperPath)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("/bin/sh", "-c", "printf pass > \"$1\"", "sh", marker)
	command.Dir = root
	if err := configureExactLinuxProcessTreeCommand(command, helper, codexProviderFileIdentity{}, true); err == nil || !strings.Contains(err.Error(), "capability-drop helper") {
		t.Fatalf("ambient launch without attested dropper did not fail closed: %v", err)
	}
	command = exec.Command("/bin/sh", "-c", "printf pass > \"$1\"", "sh", marker)
	command.Dir = root
	if err := configureExactLinuxProcessTreeCommand(command, helper, dropper, true); err != nil {
		t.Fatal(err)
	}
	if err := command.Run(); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "pass" {
		t.Fatalf("capability-drop exec did not preserve the exact payload: data=%q err=%v", data, err)
	}
}

func detachedLinuxHeartbeatCommand(ctx context.Context, root, marker string, keepLeader bool) *exec.Cmd {
	leaderTail := "exit 0"
	if keepLeader {
		leaderTail = "while :; do sleep 1; done"
	}
	script := `
marker="$1"
/usr/bin/setsid -f /bin/sh -c 'trap "" TERM; exec >/dev/null 2>&1; while :; do printf x >> "$1"; sleep 0.02; done' sh "$marker"
i=0
while [ ! -s "$marker" ]; do
  i=$((i+1))
  if [ "$i" -gt 200 ]; then exit 70; fi
  sleep 0.01
done
` + leaderTail
	command := exec.CommandContext(ctx, "/bin/sh", "-c", script, "sh", marker)
	command.Dir = root
	command.Stdout = &bytes.Buffer{}
	command.Stderr = &bytes.Buffer{}
	return command
}

func exactLinuxTestBubblewrap(t *testing.T) codexProviderFileIdentity {
	t.Helper()
	path, err := exec.LookPath("bwrap")
	if err != nil {
		t.Skip("exact Linux process-tree regression requires bubblewrap")
	}
	path, err = filepath.EvalSymlinks(path)
	if err == nil {
		path, err = filepath.Abs(path)
	}
	if err != nil {
		t.Fatal(err)
	}
	identity, err := inspectCodexProviderFile(path)
	if err != nil {
		t.Fatalf("inspect bubblewrap: %v", err)
	}
	return identity
}

func skipUnavailableExactLinuxBoundary(t *testing.T, waitErr error, command *exec.Cmd) {
	t.Helper()
	detail := strings.ToLower(waitErr.Error() + "\n" + command.Stderr.(*bytes.Buffer).String())
	if strings.Contains(detail, "operation not permitted") || strings.Contains(detail, "creating new namespace failed") ||
		strings.Contains(detail, "no permissions to creating new namespace") {
		t.Skipf("host policy correctly makes exact Linux containment unavailable: %s", strings.TrimSpace(detail))
	}
}

func waitForLinuxHeartbeat(t *testing.T, marker string) os.FileInfo {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if info, err := os.Stat(marker); err == nil && info.Size() > 0 {
			return info
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("detached heartbeat did not start: %s", marker)
	return nil
}

func waitForExactLinuxFile(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if info, err := os.Stat(path); err == nil && info.Size() > 0 {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func assertLinuxHeartbeatStopped(t *testing.T, marker string) {
	t.Helper()
	first := waitForLinuxHeartbeat(t, marker)
	time.Sleep(300 * time.Millisecond)
	second, err := os.Stat(marker)
	if err != nil {
		t.Fatal(err)
	}
	if second.Size() != first.Size() {
		t.Fatalf("detached setsid descendant survived exact PID-namespace cleanup: before=%d after=%d", first.Size(), second.Size())
	}
}
