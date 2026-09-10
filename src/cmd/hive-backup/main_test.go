package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/hubbackup"
	"github.com/hivecommons/hive/pkg/spokebackup"
)

// The hive-backup CLI is the disaster-recovery entry point: it is what an
// operator runs from a CronJob (run) and, critically, what they reach for
// during an incident (verify/extract). These tests exercise the hermetic
// paths directly (usage, verify -file, extract) and drive every os.Exit path
// through a re-exec of the test binary so exit codes — which is what a
// CronJob's alerting actually observes — are asserted, not assumed.

// testKey returns a fresh hex-encoded AES-256 key and its raw bytes.
func testKey(t *testing.T) (string, []byte) {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(raw), raw
}

// quietLogger discards log output so test output stays readable.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// failingSpokeCollector simulates a fleet whose spoke collection fails, which
// populates Manifest.SpokeErrors — the branch cmdVerify warns on.
type failingSpokeCollector struct{}

func (failingSpokeCollector) Collect(*slog.Logger) ([]hubbackup.SpokeConfig, error) {
	return nil, errors.New("simulated spoke collection failure")
}

// buildLocalArchive seals a real archive from a fixture data dir containing
// one SaaS state file, returning the archive path and the file's content.
// withSpokeErrors optionally records a failed spoke collection in the manifest.
func buildLocalArchive(t *testing.T, withSpokeErrors bool) (archive string, content []byte) {
	t.Helper()
	hexKey, key := testKey(t)
	t.Setenv(hubbackup.EnvBackupKey, hexKey)

	dataDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dataDir, "saas"), 0o755); err != nil {
		t.Fatal(err)
	}

	content = []byte(`{"users":["alice"]}`)
	if err := os.WriteFile(filepath.Join(dataDir, "saas", "users.json"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(hubbackup.EnvDataDir, dataDir)

	var collector hubbackup.SpokeCollector
	if withSpokeErrors {
		collector = failingSpokeCollector{}
	}
	sealed, man, err := hubbackup.Build(key, collector, nil, quietLogger())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(man.Files) == 0 {
		t.Fatal("fixture archive captured no files")
	}

	archive = filepath.Join(t.TempDir(), "backup.enc")
	if err := os.WriteFile(archive, sealed, 0o600); err != nil {
		t.Fatal(err)
	}
	return archive, content
}

func buildSpokeArchive(t *testing.T) (archive, hexKey string) {
	t.Helper()
	hexKey, key := testKey(t)

	dataDir := t.TempDir()
	mustWriteCLI(t, filepath.Join(dataDir, "hive-id"), "spoke-hive")
	mustWriteCLI(t, filepath.Join(dataDir, "hive.yaml.dashboard"), "owner: config\n")
	mustWriteCLI(t, filepath.Join(dataDir, "hive-state.json"), `{"agents":[]}`)
	mustWriteCLI(t, filepath.Join(dataDir, "beads", "agentA", "beads.json"), `{"agent":"agentA"}`)
	t.Setenv(spokebackup.EnvDataDir, dataDir)

	res, err := spokebackup.Build(key, "spoke-hive", quietLogger())
	if err != nil {
		t.Fatalf("spokebackup.Build: %v", err)
	}
	archive = filepath.Join(t.TempDir(), "spoke-backup.enc")
	if err := os.WriteFile(archive, res.Sealed, 0o600); err != nil {
		t.Fatal(err)
	}
	return archive, hexKey
}

func mustWriteCLI(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// captureOutput runs fn while capturing the given *os.File slot (os.Stdout or
// os.Stderr), returning everything written to it.
func captureOutput(t *testing.T, slot **os.File, fn func()) string {
	t.Helper()
	old := *slot
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	*slot = w
	defer func() { *slot = old }()
	fn()
	w.Close()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// --- usage ---

func TestUsageDocumentsEveryCommandAndEnvVar(t *testing.T) {
	out := captureOutput(t, &os.Stderr, usage)

	for _, want := range []string{
		"run", "verify", "extract", "restore", "list",
		hubbackup.EnvBackupKey, hubbackup.EnvBucket,
		hubbackup.EnvDataDir, hubbackup.EnvRetention,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("usage() output missing %q:\n%s", want, out)
		}
	}
}

// --- verify -file (hermetic happy paths) ---

func TestCmdVerifyLocalArchiveReportsOK(t *testing.T) {
	archive, _ := buildLocalArchive(t, false)

	out := captureOutput(t, &os.Stdout, func() {
		cmdVerify([]string{"-file", archive}, quietLogger())
	})

	if !strings.Contains(out, "archive OK: 1 files") {
		t.Errorf("cmdVerify output = %q; want it to report 'archive OK: 1 files'", out)
	}
	if strings.Contains(out, "WARNING") {
		t.Errorf("cmdVerify warned on a clean archive:\n%s", out)
	}
}

func TestCmdVerifyLocalArchiveWarnsOnSpokeGaps(t *testing.T) {
	archive, _ := buildLocalArchive(t, true)

	out := captureOutput(t, &os.Stdout, func() {
		cmdVerify([]string{"-file", archive}, quietLogger())
	})

	if !strings.Contains(out, "archive OK") {
		t.Errorf("cmdVerify output = %q; want archive OK", out)
	}
	if !strings.Contains(out, "WARNING") || !strings.Contains(out, "_collector") {
		t.Errorf("cmdVerify did not surface the spoke gap:\n%s", out)
	}
}

// --- extract (hermetic happy path) ---

func TestCmdExtractRestoresArchivedFile(t *testing.T) {
	archive, content := buildLocalArchive(t, false)
	dest := t.TempDir()

	out := captureOutput(t, &os.Stdout, func() {
		cmdExtract([]string{"-file", archive, "-dest", dest}, quietLogger())
	})

	if !strings.Contains(out, "extracted 1 files to "+dest) {
		t.Errorf("cmdExtract output = %q", out)
	}
	restored, err := os.ReadFile(filepath.Join(dest, "hub", "saas", "users.json"))
	if err != nil {
		t.Fatalf("restored file missing: %v", err)
	}
	if !bytes.Equal(restored, content) {
		t.Errorf("restored content = %q; want %q", restored, content)
	}
}

// --- exit-code contract (re-exec the test binary so os.Exit is observable) ---

// testArgsSep separates argv words in the helper env var. The unit separator
// is used because env values cannot contain NUL and argv words never contain
// control characters in these tests.
const testArgsSep = "\x1f"

// TestHelperRunMain is not a real test: it becomes the hive-backup process
// when re-exec'd by runMainHelper. os.Exit inside main() terminates the
// subprocess, and the parent asserts on the exit code — the same signal a
// CronJob's failure alerting keys off.
func TestHelperRunMain(t *testing.T) {
	if os.Getenv("HIVE_BACKUP_TEST_RUN_MAIN") != "1" {
		t.Skip("helper process for exit-code tests")
	}
	args := []string{"hive-backup"}
	if raw := os.Getenv("HIVE_BACKUP_TEST_ARGS"); raw != "" {
		args = append(args, strings.Split(raw, testArgsSep)...)
	}
	os.Args = args
	main()
}

// runMainHelper re-execs this test binary as `hive-backup args...`, returning
// the exit code and combined output.
func runMainHelper(t *testing.T, env map[string]string, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run", "TestHelperRunMain")
	cmd.Env = append(os.Environ(),
		"HIVE_BACKUP_TEST_RUN_MAIN=1",
		"HIVE_BACKUP_TEST_ARGS="+strings.Join(args, testArgsSep),
		// Ensure no ambient key leaks in; tests that need one set it in env.
		hubbackup.EnvBackupKey+"=",
	)
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), string(out)
	}
	t.Fatalf("re-exec failed to run: %v\n%s", err, out)
	return -1, ""
}

func TestMainNoArgsExitsNonzeroWithUsage(t *testing.T) {
	code, out := runMainHelper(t, nil)
	if code != exitCodeError {
		t.Errorf("exit code = %d; want %d", code, exitCodeError)
	}
	if !strings.Contains(out, "hive-backup") || !strings.Contains(out, "verify") {
		t.Errorf("no-args run did not print usage:\n%s", out)
	}
}

func TestMainUnknownCommandExitsNonzero(t *testing.T) {
	code, out := runMainHelper(t, nil, "definitely-unknown")
	if code != exitCodeError {
		t.Errorf("exit code = %d; want %d", code, exitCodeError)
	}
	if !strings.Contains(out, "Commands:") {
		t.Errorf("unknown command did not print usage:\n%s", out)
	}
}

func TestMainVerifyWithoutKeyExitsNonzero(t *testing.T) {
	// The key env is explicitly blanked by runMainHelper: verifying anything
	// without a key must fail closed rather than proceed.
	code, out := runMainHelper(t, nil, "verify", "-file", "irrelevant.enc")
	if code != exitCodeError {
		t.Errorf("exit code = %d; want %d", code, exitCodeError)
	}
	if !strings.Contains(out, "verify failed") {
		t.Errorf("missing-key verify did not report failure:\n%s", out)
	}
}

func TestMainVerifyMissingFileExitsNonzero(t *testing.T) {
	hexKey, _ := testKey(t)
	code, out := runMainHelper(t, map[string]string{hubbackup.EnvBackupKey: hexKey},
		"verify", "-file", filepath.Join(t.TempDir(), "no-such.enc"))
	if code != exitCodeError {
		t.Errorf("exit code = %d; want %d", code, exitCodeError)
	}
	if !strings.Contains(out, "verify failed") {
		t.Errorf("missing-file verify did not report failure:\n%s", out)
	}
}

func TestMainVerifyCorruptArchiveExitsNonzero(t *testing.T) {
	hexKey, _ := testKey(t)
	bad := filepath.Join(t.TempDir(), "corrupt.enc")
	if err := os.WriteFile(bad, []byte("not an archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out := runMainHelper(t, map[string]string{hubbackup.EnvBackupKey: hexKey},
		"verify", "-file", bad)
	if code != exitCodeError {
		t.Errorf("exit code = %d; want %d", code, exitCodeError)
	}
	if !strings.Contains(out, "verify failed") {
		t.Errorf("corrupt-archive verify did not report failure:\n%s", out)
	}
}

func TestMainExtractRequiresFileAndDest(t *testing.T) {
	for _, args := range [][]string{
		{"extract"},
		{"extract", "-file", "a.enc"},
		{"extract", "-dest", "out"},
	} {
		code, out := runMainHelper(t, nil, args...)
		if code != exitCodeError {
			t.Errorf("%v: exit code = %d; want %d", args, code, exitCodeError)
		}
		if !strings.Contains(out, "extract requires -file and -dest") {
			t.Errorf("%v: missing flag guard not reported:\n%s", args, out)
		}
	}
}

func TestMainExtractWithoutKeyExitsNonzero(t *testing.T) {
	code, out := runMainHelper(t, nil, "extract", "-file", "a.enc", "-dest", t.TempDir())
	if code != exitCodeError {
		t.Errorf("exit code = %d; want %d", code, exitCodeError)
	}
	if !strings.Contains(out, "extract failed") {
		t.Errorf("missing-key extract did not report failure:\n%s", out)
	}
}

func TestMainRestoreDefaultsToDryRun(t *testing.T) {
	archive, hexKey := buildSpokeArchive(t)
	dest := t.TempDir()
	code, out := runMainHelper(t, map[string]string{hubbackup.EnvBackupKey: hexKey},
		"restore", "-file", archive, "-dest", dest)
	if code != 0 {
		t.Fatalf("exit code = %d; want 0\n%s", code, out)
	}
	if !strings.Contains(out, "restore dry-run OK") || !strings.Contains(out, "rerun with -confirm") {
		t.Fatalf("restore did not default to a dry run:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dest, "hive-id")); !os.IsNotExist(err) {
		t.Fatalf("dry-run restore wrote hive-id or hit unexpected error: %v", err)
	}
}

func TestMainRestoreConfirmWritesMappedFiles(t *testing.T) {
	archive, hexKey := buildSpokeArchive(t)
	dest := t.TempDir()
	code, out := runMainHelper(t, map[string]string{hubbackup.EnvBackupKey: hexKey},
		"restore", "-file", archive, "-dest", dest, "-confirm")
	if code != 0 {
		t.Fatalf("exit code = %d; want 0\n%s", code, out)
	}
	if !strings.Contains(out, "restored") {
		t.Fatalf("confirmed restore did not report success:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dest, "beads", "agentA", "beads.json")); err != nil {
		t.Fatalf("confirmed restore did not map beads/ into destination: %v", err)
	}
}

func TestMainRestoreRefusesDifferentHiveIDWithoutForce(t *testing.T) {
	archive, hexKey := buildSpokeArchive(t)
	dest := t.TempDir()
	mustWriteCLI(t, filepath.Join(dest, "hive-id"), "other-hive")
	code, out := runMainHelper(t, map[string]string{hubbackup.EnvBackupKey: hexKey},
		"restore", "-file", archive, "-dest", dest, "-confirm")
	if code != exitCodeError {
		t.Fatalf("exit code = %d; want %d\n%s", code, exitCodeError, out)
	}
	if !strings.Contains(out, "without -force") {
		t.Fatalf("restore did not surface the live hive-id guard:\n%s", out)
	}
}

func TestMainRunWithoutKeyExitsNonzero(t *testing.T) {
	code, out := runMainHelper(t, nil, "run", "-local", filepath.Join(t.TempDir(), "out.enc"))
	if code != exitCodeError {
		t.Errorf("exit code = %d; want %d", code, exitCodeError)
	}
	if !strings.Contains(out, "backup failed") {
		t.Errorf("missing-key run did not report failure:\n%s", out)
	}
}

func TestMainVerifyDispatchHappyPathExitsZero(t *testing.T) {
	archive, _ := buildLocalArchive(t, false)
	code, out := runMainHelper(t,
		map[string]string{hubbackup.EnvBackupKey: os.Getenv(hubbackup.EnvBackupKey)},
		"verify", "-file", archive)
	if code != 0 {
		t.Errorf("exit code = %d; want 0\n%s", code, out)
	}
	if !strings.Contains(out, "archive OK: 1 files") {
		t.Errorf("dispatch to verify did not report archive OK:\n%s", out)
	}
}
