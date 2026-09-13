package main

// Exit-path and filter-branch tests for the top-level bd commands in main.go.
// Every failure arm here terminates with os.Exit(1), so — exactly like the
// kb exit-path tests — each one re-execs the test binary through the
// TestBDHelperProcess harness (main_dispatch_test.go). The in-process tests
// at the bottom cover the cmdList filter branches, cmdReady's table output,
// and printTable's long-title truncation, none of which exit.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/beads"
)

// runMainExpectExit1InDir is runMainExpectExit1 with the store directory under
// the caller's control instead of a fresh t.TempDir(). The default helper
// always appends a working BD_DIR, which makes openStore's failure arm
// unreachable through it; this variant strips any inherited BD_DIR and sets
// exactly the one supplied, so a test can point the store at an unopenable
// path. glibc getenv returns the FIRST matching environ entry, so filtering —
// not appending a duplicate — is the only reliable override.
func runMainExpectExit1InDir(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestBDHelperProcess")
	env := make([]string, 0, len(os.Environ())+3)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "BD_DIR=") {
			continue
		}
		env = append(env, kv)
	}
	cmd.Env = append(env,
		"BD_HELPER_PROCESS=1",
		"BD_HELPER_ARGS="+strings.Join(args, "\x1f"),
		"BD_DIR="+dir,
	)
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if err == nil {
		t.Fatalf("bd %v exited 0; want exit 1\noutput: %s", args, out)
	} else if !errorsAs(err, &exitErr) {
		t.Fatalf("bd %v failed to run: %v\noutput: %s", args, err, out)
	} else if exitErr.ExitCode() != 1 {
		t.Fatalf("bd %v exit code = %d; want 1\noutput: %s", args, exitErr.ExitCode(), out)
	}
	return string(out)
}

// --- openStore ---

// A store rooted under a regular file cannot be created (MkdirAll fails), and
// every command that opens the store must report the path and exit 1 rather
// than proceed against nothing.
func TestOpenStoreUnopenableDirExits1(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}
	badDir := filepath.Join(file, "beads")
	out := runMainExpectExit1InDir(t, badDir, "list")
	if !strings.Contains(out, "failed to open store at "+badDir) {
		t.Errorf("bd list output = %q; want failed-to-open error naming %q", out, badDir)
	}
}

// --- cmdCreate validation ---

func TestCreateMissingTitleExits1(t *testing.T) {
	out := runMainExpectExit1(t, "create", "--actor", "quality")
	if !strings.Contains(out, "--title and --actor are required") {
		t.Errorf("bd create output = %q; want required-flags error", out)
	}
}

func TestCreateMissingActorExits1(t *testing.T) {
	out := runMainExpectExit1(t, "create", "--title", "a finding")
	if !strings.Contains(out, "--title and --actor are required") {
		t.Errorf("bd create output = %q; want required-flags error", out)
	}
}

func TestCreatePriorityOutOfRangeExits1(t *testing.T) {
	for _, pri := range []string{"-1", "5"} {
		t.Run(pri, func(t *testing.T) {
			out := runMainExpectExit1(t, "create", "--title", "x", "--actor", "quality", "--priority", pri)
			if !strings.Contains(out, "--priority must be 0-4") {
				t.Errorf("bd create --priority %s output = %q; want range error", pri, out)
			}
		})
	}
}

// --- cmdUpdate validation and store-error arms ---

func TestUpdateNoIDExits1(t *testing.T) {
	out := runMainExpectExit1(t, "update")
	if !strings.Contains(out, "requires a bead ID") {
		t.Errorf("bd update output = %q; want bead-ID-required error", out)
	}
}

func TestUpdateClaimMissingBeadExits1(t *testing.T) {
	out := runMainExpectExit1(t, "update", "no-such-bead", "--claim")
	if !strings.Contains(out, "bd update:") || !strings.Contains(out, "no-such-bead") {
		t.Errorf("bd update --claim output = %q; want store error naming the bead", out)
	}
}

func TestUpdateInvalidStatusExits1(t *testing.T) {
	out := runMainExpectExit1(t, "update", "some-id", "--status", "bogus")
	if !strings.Contains(out, `invalid status "bogus"`) {
		t.Errorf("bd update --status bogus output = %q; want invalid-status error", out)
	}
}

func TestUpdateStatusMissingBeadExits1(t *testing.T) {
	out := runMainExpectExit1(t, "update", "no-such-bead", "--status", "open")
	if !strings.Contains(out, "bd update:") || !strings.Contains(out, "no-such-bead") {
		t.Errorf("bd update --status output = %q; want store error naming the bead", out)
	}
}

func TestUpdateSetMetadataBadFormatExits1(t *testing.T) {
	out := runMainExpectExit1(t, "update", "some-id", "--set-metadata", "no-equals-sign")
	if !strings.Contains(out, "--set-metadata requires key=value format") {
		t.Errorf("bd update --set-metadata output = %q; want key=value format error", out)
	}
}

func TestUpdateSetMetadataMissingBeadExits1(t *testing.T) {
	out := runMainExpectExit1(t, "update", "no-such-bead", "--set-metadata", "k=v")
	if !strings.Contains(out, "bd update:") || !strings.Contains(out, "no-such-bead") {
		t.Errorf("bd update --set-metadata output = %q; want store error naming the bead", out)
	}
}

func TestUpdateUnsetMetadataMissingBeadExits1(t *testing.T) {
	out := runMainExpectExit1(t, "update", "no-such-bead", "--unset-metadata", "k")
	if !strings.Contains(out, "bd update:") || !strings.Contains(out, "no-such-bead") {
		t.Errorf("bd update --unset-metadata output = %q; want store error naming the bead", out)
	}
}

func TestUpdateNoActionFlagExits1(t *testing.T) {
	out := runMainExpectExit1(t, "update", "some-id")
	if !strings.Contains(out, "specify --claim, --status, --set-metadata, or --unset-metadata") {
		t.Errorf("bd update (no action) output = %q; want action-required error", out)
	}
}

// --- cmdList filter branches (in-process; these paths do not exit) ---

// The --status and --actor flags must narrow the listing: both filters are
// applied together, and a bead matching only one of them stays out.
func TestCmdListStatusAndActorFilters(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BD_DIR", dir)
	store, err := beads.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	match, err := store.Create("matching bead", beads.TypeAdvisory, beads.Priority(2), "quality", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create("other actor", beads.TypeAdvisory, beads.Priority(2), "scanner", ""); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		cmdList([]string{"--json", "--status", "open", "--actor", "quality"})
	})
	if !strings.Contains(out, match.ID) {
		t.Errorf("cmdList filtered output missing matching bead %s:\n%s", match.ID, out)
	}
	if strings.Contains(out, "other actor") {
		t.Errorf("cmdList --actor quality leaked another actor's bead:\n%s", out)
	}

	out = captureStdout(t, func() {
		cmdList([]string{"--json", "--status", "done"})
	})
	if strings.Contains(out, match.ID) {
		t.Errorf("cmdList --status done leaked an open bead:\n%s", out)
	}
}

// --- cmdReady table output (in-process) ---

// cmdReady without --json must render the human-readable table, the branch
// the JSON-focused ready tests never take.
func TestCmdReadyTableOutput(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BD_DIR", dir)
	store, err := beads.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := store.Create("ready bead", beads.TypeAdvisory, beads.Priority(1), "quality", "")
	if err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		cmdReady([]string{})
	})
	if !strings.Contains(out, "ID") || !strings.Contains(out, "STATUS") {
		t.Errorf("cmdReady table missing header:\n%s", out)
	}
	if !strings.Contains(out, b.ID) {
		t.Errorf("cmdReady table missing bead %s:\n%s", b.ID, out)
	}
}

// --- printTable long-title truncation (in-process) ---

// A title longer than titleColWidth must be truncated with a "..." suffix so
// the table stays aligned; the full title must not appear.
func TestPrintTableTruncatesLongTitles(t *testing.T) {
	longTitle := strings.Repeat("x", titleColWidth+10)
	items := []*beads.Bead{{
		ID:     "long-title-bead",
		Title:  longTitle,
		Status: beads.StatusOpen,
		Actor:  "quality",
	}}

	out := captureStdout(t, func() {
		printTable(items)
	})
	if strings.Contains(out, longTitle) {
		t.Errorf("printTable printed the untruncated %d-rune title:\n%s", len(longTitle), out)
	}
	want := strings.Repeat("x", titleColWidth-3) + "..."
	if !strings.Contains(out, want) {
		t.Errorf("printTable output missing truncated title %q:\n%s", want, out)
	}
}
