//go:build windows

package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/kubestellar/hive/pkg/integrated"
)

func TestWindowsDaemonTaskIsLeastPrivilegePersistentAndCrashRecovering(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state with spaces")
	spec, err := newTestDaemonServiceSpec(stateDir, `C:\Program Files\Hive\hive.exe`, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := windowsDaemonTaskXML(spec, &user.User{Uid: "S-1-5-21-123-456-789-1001", Username: `EXAMPLE\operator`})
	if err != nil {
		t.Fatal(err)
	}
	xmlText := decodeWindowsTaskXMLForTest(t, definition)
	for _, required := range []string{
		"<LogonTrigger>", "<TimeTrigger>", "<StartBoundary>", "<LogonType>InteractiveToken</LogonType>", "<RunLevel>LeastPrivilege</RunLevel>",
		"<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>", "<RestartOnFailure>", "<Interval>PT1M</Interval>", "<Count>255</Count>",
		"<Repetition>", "<Interval>PT1M</Interval>", "<StopAtDurationEnd>false</StopAtDurationEnd>",
		"<MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>", "<StartWhenAvailable>true</StartWhenAvailable>",
		"<Command>C:\\Program Files\\Hive\\hive.exe</Command>", "--log-file",
		"--expected-hive-commit " + testDaemonHiveCommit,
		"--expected-executable-sha256 " + testDaemonDigest(),
	} {
		if !strings.Contains(xmlText, required) {
			t.Fatalf("scheduled task omitted %q:\n%s", required, xmlText)
		}
	}
	for _, forbidden := range []string{"HighestAvailable", "SYSTEM", "Password", "S4U"} {
		if strings.Contains(xmlText, forbidden) {
			t.Fatalf("scheduled task unexpectedly requests elevated/non-interactive authority %q", forbidden)
		}
	}
	if !strings.Contains(xmlText, `--state-dir &#34;`+stateDir+`&#34;`) {
		t.Fatalf("state directory was not preserved as one quoted argument:\n%s", xmlText)
	}
}

func decodeWindowsTaskXMLForTest(t *testing.T, data []byte) string {
	t.Helper()
	if len(data) < 2 || data[0] != 0xff || data[1] != 0xfe || (len(data)-2)%2 != 0 {
		t.Fatalf("task XML is not UTF-16LE with a BOM")
	}
	units := make([]uint16, (len(data)-2)/2)
	for index := range units {
		units[index] = binary.LittleEndian.Uint16(data[2+index*2:])
	}
	return string(utf16.Decode(units))
}

func TestWindowsServiceManagerIsIdempotentAndNeverRequestsAdmin(t *testing.T) {
	stateDir := t.TempDir()
	spec, err := newTestDaemonServiceSpec(stateDir, `C:\Hive\hive.exe`, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	account := &user.User{Uid: "S-1-5-21-1001", Username: `EXAMPLE\operator`}
	registeredDefinition, err := windowsDaemonTaskXML(spec, account)
	if err != nil {
		t.Fatal(err)
	}
	registered := false
	calls := [][]string{}
	runner := func(_ context.Context, arguments ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), arguments...))
		switch arguments[0] {
		case "/Create":
			registered = true
			return []byte("SUCCESS"), nil
		case "/Query":
			if !registered {
				return []byte("ERROR: The system cannot find the file specified."), errors.New("exit status 1")
			}
			return registeredDefinition, nil
		case "/Delete":
			registered = false
			return []byte("SUCCESS"), nil
		default:
			return []byte("SUCCESS"), nil
		}
	}
	manager := &windowsDaemonServiceManager{
		run: runner,
		currentUser: func() (*user.User, error) {
			return account, nil
		},
	}
	if err := manager.Install(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if err := manager.Install(context.Background(), spec); err != nil {
		t.Fatalf("repeat installation was not idempotent: %v", err)
	}
	if err := manager.Uninstall(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if err := manager.Uninstall(context.Background(), spec); err != nil {
		t.Fatalf("repeat uninstall was not idempotent: %v", err)
	}
	for _, call := range calls {
		joined := strings.Join(call, " ")
		if strings.Contains(strings.ToUpper(joined), "/RU SYSTEM") || strings.Contains(strings.ToUpper(joined), "/RL HIGHEST") {
			t.Fatalf("service command requested administrator authority: %v", call)
		}
	}
}

func TestWindowsCommandLineQuotingPreservesTrailingBackslashesAndQuotes(t *testing.T) {
	tests := map[string]string{
		"plain":               "plain",
		"":                    `""`,
		`C:\path with space\`: `"C:\path with space\\"`,
		`value"quoted`:        `"value\"quoted"`,
	}
	for input, expected := range tests {
		if got := quoteWindowsCommandLineArg(input); got != expected {
			t.Fatalf("quoteWindowsCommandLineArg(%q) = %q, want %q", input, got, expected)
		}
	}
}

func TestWindowsServiceInspectionFailsClosedOnAccessError(t *testing.T) {
	stateDir := t.TempDir()
	spec, err := newTestDaemonServiceSpec(stateDir, `C:\Hive\hive.exe`, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	manager := &windowsDaemonServiceManager{run: func(context.Context, ...string) ([]byte, error) {
		return []byte("ERROR: Access is denied."), errors.New("exit status 1")
	}}
	if _, err := manager.Inspect(context.Background(), spec); err == nil || !strings.Contains(strings.ToLower(err.Error()), "access is denied") {
		t.Fatalf("access error was treated as an absent safe task: %v", err)
	}
}

func TestWindowsServiceInspectionRejectsActionDrift(t *testing.T) {
	stateDir := t.TempDir()
	spec, err := newTestDaemonServiceSpec(stateDir, `C:\Hive\hive.exe`, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	account := &user.User{Uid: "S-1-5-21-1001", Username: `EXAMPLE\operator`}
	data, err := windowsDaemonTaskXML(spec, account)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := parseWindowsScheduledTask(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateWindowsScheduledTask(definition, spec, account); err != nil {
		t.Fatalf("generated task rejected: %v", err)
	}
	definition.Actions.Exec[0].Arguments += " --foreign"
	if err := validateWindowsScheduledTask(definition, spec, account); err == nil {
		t.Fatal("drifted scheduled task action was accepted")
	}
}

func TestWindowsTaskSchedulerAcceptsGeneratedUserTask(t *testing.T) {
	if os.Getenv("HIVE_TEST_WINDOWS_TASK_SCHEDULER") != "1" {
		t.Skip("set HIVE_TEST_WINDOWS_TASK_SCHEDULER=1 for the local Task Scheduler integration test")
	}
	stateDir := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	spec, err := newDaemonServiceSpec(stateDir, executable, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	manager := newPlatformDaemonServiceManager()
	if err := manager.Install(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := manager.Uninstall(context.Background(), spec); err != nil {
			t.Errorf("cleanup scheduled task: %v", err)
		}
	})
	status, err := manager.Inspect(context.Background(), spec)
	if err != nil || !status.Installed || !status.Enabled || !status.RestartOnFailure || status.RebootPersistent || !status.LoginPersistent || status.StartupMode != "interactive-user-logon" || status.HiveCommit != spec.HiveCommit || status.ExecutableSHA256 != spec.ExecutableSHA256 {
		t.Fatalf("registered task status = %+v, err=%v", status, err)
	}
}

func TestWindowsPersistentDaemonStartsAndStopsThroughTaskScheduler(t *testing.T) {
	if os.Getenv("HIVE_TEST_WINDOWS_TASK_SCHEDULER") != "1" {
		t.Skip("set HIVE_TEST_WINDOWS_TASK_SCHEDULER=1 for the local Task Scheduler integration test")
	}
	stateDir := t.TempDir()
	store, err := integrated.NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(integrated.Config{Repository: "invalid/example", RepositoryID: "1", DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	installDir := t.TempDir()
	executable := filepath.Join(installDir, "hive.exe")
	buildCtx, buildCancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer buildCancel()
	build := exec.CommandContext(buildCtx, "go", "build", "-ldflags", "-X main.gitHash="+testDaemonHiveCommit+" -X main.gitShort="+testDaemonHiveCommit[:12], "-o", executable, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build Hive integration executable: %v\n%s", err, output)
	}
	stop := func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		command := exec.CommandContext(stopCtx, executable, "stop", "--state-dir", stateDir, "--json")
		if output, err := command.CombinedOutput(); err != nil {
			t.Errorf("stop persistent test daemon: %v\n%s", err, output)
		}
	}
	t.Cleanup(stop)
	startCtx, startCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer startCancel()
	start := exec.CommandContext(startCtx, executable, "start", "--state-dir", stateDir, "--interval", "1m", "--json")
	output, err := start.CombinedOutput()
	if err != nil {
		t.Fatalf("start persistent test daemon: %v\n%s", err, output)
	}
	var status integratedDaemonStatus
	if err := json.Unmarshal(output, &status); err != nil {
		t.Fatalf("decode start status: %v\n%s", err, output)
	}
	if !status.Running || !status.RuntimeRunning || status.PID <= 0 || status.Service == nil || !status.Service.Installed || !status.Service.Enabled || status.Service.RebootPersistent || !status.Service.LoginPersistent || !status.Service.RestartOnFailure || !processIsAlive(status.PID) {
		t.Fatalf("persistent daemon did not become healthy: %+v", status)
	}
	if os.Getenv("HIVE_TEST_WINDOWS_TASK_RESTART") == "1" {
		const replacementCommit = "89abcdef0123456789abcdef0123456789abcdef"
		replacementRoot := t.TempDir()
		replacement := filepath.Join(replacementRoot, "hive.exe")
		replacementBuildCtx, replacementBuildCancel := context.WithTimeout(context.Background(), 90*time.Second)
		replacementBuild := exec.CommandContext(replacementBuildCtx, "go", "build", "-ldflags", "-X main.gitHash="+replacementCommit+" -X main.gitShort="+replacementCommit[:12], "-o", replacement, ".")
		if output, err := replacementBuild.CombinedOutput(); err != nil {
			replacementBuildCancel()
			t.Fatalf("build replacement Hive integration executable: %v\n%s", err, output)
		}
		replacementBuildCancel()
		oldPID := status.PID
		journal := installDir + ".installer-transition.json"
		prepareCtx, prepareCancel := context.WithTimeout(context.Background(), 45*time.Second)
		prepare := exec.CommandContext(prepareCtx, replacement, "installer-transition", "--phase", "prepare", "--journal", journal, "--owned-install-dir", installDir, "--json")
		if output, err := prepare.CombinedOutput(); err != nil {
			prepareCancel()
			t.Fatalf("prepare scheduler handoff before replacement: %v\n%s", err, output)
		}
		prepareCancel()
		backupDir := installDir + "-previous"
		if err := os.Rename(installDir, backupDir); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(backupDir) })
		if err := os.MkdirAll(installDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(replacement, executable); err != nil {
			t.Fatal(err)
		}
		commitCtx, commitCancel := context.WithTimeout(context.Background(), 45*time.Second)
		commit := exec.CommandContext(commitCtx, executable, "installer-transition", "--phase", "commit", "--journal", journal, "--runtime-executable", executable, "--json")
		if output, err := commit.CombinedOutput(); err != nil {
			commitCancel()
			t.Fatalf("commit scheduler handoff after replacement: %v\n%s", err, output)
		}
		commitCancel()
		finalizeCtx, finalizeCancel := context.WithTimeout(context.Background(), 30*time.Second)
		finalize := exec.CommandContext(finalizeCtx, executable, "installer-transition", "--phase", "finalize", "--journal", journal, "--json")
		if output, err := finalize.CombinedOutput(); err != nil {
			finalizeCancel()
			t.Fatalf("finalize scheduler handoff after replacement: %v\n%s", err, output)
		}
		finalizeCancel()
		restartCtx, restartCancel := context.WithTimeout(context.Background(), 45*time.Second)
		restart := exec.CommandContext(restartCtx, executable, "start", "--state-dir", stateDir, "--interval", "1m", "--json")
		output, err := restart.CombinedOutput()
		restartCancel()
		if err != nil {
			t.Fatalf("restart through same path with replacement identity: %v\n%s", err, output)
		}
		if err := json.Unmarshal(output, &status); err != nil {
			t.Fatalf("decode replacement start status: %v\n%s", err, output)
		}
		if status.PID == oldPID || status.HiveCommit != replacementCommit || processIsAlive(oldPID) {
			t.Fatalf("same-path release replacement retained stale scheduler identity: old=%d status=%+v", oldPID, status)
		}
	}
	if os.Getenv("HIVE_TEST_WINDOWS_TASK_RESTART") == "1" {
		killedPID := status.PID
		if err := terminateProcess(killedPID); err != nil {
			t.Fatalf("force-kill daemon process %d: %v", killedPID, err)
		}
		deadline := time.Now().Add(100 * time.Second)
		restarted := integratedDaemonStatus{}
		for time.Now().Before(deadline) {
			candidate := readIntegratedDaemonRuntimeStatus(stateDir)
			if candidate.Running && candidate.PID > 0 && candidate.PID != killedPID {
				restarted = candidate
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		if !restarted.Running {
			t.Fatalf("Task Scheduler did not recover force-killed daemon process %d within the restart bound", killedPID)
		}
		status = restarted
	}
	stop()
	if processIsAlive(status.PID) {
		t.Fatalf("daemon process %d survived stop", status.PID)
	}
	if _, exists, err := readDaemonServiceSpec(stateDir); err != nil || exists {
		t.Fatalf("service descriptor survived stop: exists=%t err=%v", exists, err)
	}
	spec, err := newDaemonServiceSpec(stateDir, executable, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	service, err := newPlatformDaemonServiceManager().Inspect(context.Background(), spec)
	if err != nil || service.Installed {
		t.Fatalf("scheduled task survived stop: status=%+v err=%v", service, err)
	}
}
