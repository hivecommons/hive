package main

// Tests for the bd CLI entry points that had no coverage: the main() command
// dispatcher, the cmdKB subcommand dispatcher, the dolt/init no-ops, and the
// usage printers. Success paths run in-process (matching the conventions in
// main_test.go); paths that call os.Exit re-exec the test binary as a
// subprocess and assert on exit code and stderr.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// captureStderr runs fn while capturing os.Stderr, returning what was written.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	fn()
	w.Close()
	os.Stderr = old
	var buf bytes.Buffer
	io.Copy(&buf, r)
	return buf.String()
}

// runMain invokes main() in-process with the given argv (excluding the program
// name), capturing stdout. Only safe for arguments that do not hit os.Exit.
func runMain(t *testing.T, args ...string) string {
	t.Helper()
	origArgs := os.Args
	defer func() { os.Args = origArgs }()
	os.Args = append([]string{"bd"}, args...)
	return captureStdout(t, main)
}

// --- main() dispatch: success arms ---

func TestMainDispatchHelpVariants(t *testing.T) {
	for _, arg := range []string{"help", "--help", "-h"} {
		t.Run(arg, func(t *testing.T) {
			origArgs := os.Args
			defer func() { os.Args = origArgs }()
			os.Args = []string{"bd", arg}
			errOut := captureStderr(t, main)
			if !strings.Contains(errOut, "Usage: bd <command>") {
				t.Errorf("main(%q) stderr = %q; want usage text", arg, errOut)
			}
		})
	}
}

func TestMainDispatchDoltPush(t *testing.T) {
	out := runMain(t, "dolt", "push")
	if !strings.Contains(out, "no-op") {
		t.Errorf("main(dolt push) output = %q; want no-op notice", out)
	}
}

func TestMainDispatchInit(t *testing.T) {
	t.Setenv("BD_DIR", t.TempDir())
	out := runMain(t, "init")
	if !strings.Contains(out, "store initialized") {
		t.Errorf("main(init) output = %q; want store initialized notice", out)
	}
}

func TestMainDispatchCreateListReady(t *testing.T) {
	t.Setenv("BD_DIR", t.TempDir())

	created := runMain(t, "create", "--title", "dispatch test bead", "--type", "task", "--priority", "1", "--actor", "tester")
	var bead struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(created), &bead); err != nil || bead.ID == "" {
		t.Fatalf("main(create) output = %q; want bead JSON with id (err=%v)", created, err)
	}

	listOut := runMain(t, "list", "--json")
	if !strings.Contains(listOut, bead.ID) {
		t.Errorf("main(list --json) output = %q; want it to contain %q", listOut, bead.ID)
	}

	readyOut := runMain(t, "ready", "--json")
	if !strings.Contains(readyOut, bead.ID) {
		t.Errorf("main(ready --json) output = %q; want it to contain %q", readyOut, bead.ID)
	}
}

func TestMainDispatchUpdateCloseResetRemember(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BD_DIR", dir)

	created := runMain(t, "create", "--title", "lifecycle bead", "--type", "task", "--priority", "2", "--actor", "tester")
	var bead struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(created), &bead); err != nil || bead.ID == "" {
		t.Fatalf("main(create) output = %q; want bead JSON with id (err=%v)", created, err)
	}

	claimOut := runMain(t, "update", bead.ID, "--claim")
	if !strings.Contains(claimOut, bead.ID) {
		t.Errorf("main(update --claim) output = %q; want it to mention %q", claimOut, bead.ID)
	}

	closeOut := runMain(t, "close", bead.ID)
	if !strings.Contains(closeOut, "Closed bead "+bead.ID) {
		t.Errorf("main(close) output = %q; want Closed bead %s", closeOut, bead.ID)
	}

	rememberOut := runMain(t, "remember", "dispatch", "fact")
	if !strings.Contains(rememberOut, "Remembered: dispatch fact") {
		t.Errorf("main(remember) output = %q; want Remembered: dispatch fact", rememberOut)
	}

	resetOut := runMain(t, "reset", "--reason", "test sweep")
	if !strings.Contains(resetOut, "test sweep") {
		t.Errorf("main(reset) output = %q; want reason echoed", resetOut)
	}

	store := reloadStore(t, dir)
	if got := store.Ready(""); len(got) != 0 {
		t.Errorf("after reset, Ready() returned %d beads; want 0", len(got))
	}
}

func TestMainDispatchDecomposePrintPrompt(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BD_DIR", dir)

	created := runMain(t, "create", "--title", "epic to decompose", "--type", "epic", "--priority", "1", "--actor", "architect")
	var bead struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(created), &bead); err != nil || bead.ID == "" {
		t.Fatalf("main(create) output = %q; want bead JSON with id (err=%v)", created, err)
	}

	promptOut := runMain(t, "decompose", bead.ID, "--print-prompt")
	if !strings.Contains(promptOut, "epic to decompose") {
		t.Errorf("main(decompose --print-prompt) output = %q; want epic title in prompt", promptOut)
	}
}

func TestMainDispatchKB(t *testing.T) {
	kbTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(t, w, []map[string]interface{}{})
	})
	out := runMain(t, "kb", "list-docs")
	if !strings.Contains(out, "No documents imported.") {
		t.Errorf("main(kb list-docs) output = %q; want empty-docs notice", out)
	}
}

// --- cmdKB dispatch ---

func TestCmdKBHelpVariants(t *testing.T) {
	for _, arg := range []string{"help", "--help", "-h"} {
		t.Run(arg, func(t *testing.T) {
			errOut := captureStderr(t, func() { cmdKB([]string{arg}) })
			if !strings.Contains(errOut, "Usage: bd kb <subcommand>") {
				t.Errorf("cmdKB(%q) stderr = %q; want kb usage text", arg, errOut)
			}
		})
	}
}

func TestCmdKBDispatchesSearch(t *testing.T) {
	kbTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/knowledge/search" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		jsonResponse(t, w, []map[string]interface{}{
			{"slug": "s1", "title": "Fact One", "confidence": 0.9, "type": "pattern", "body": "body text"},
		})
	})
	out := captureStdout(t, func() { cmdKB([]string{"search", "fact"}) })
	if !strings.Contains(out, "Fact One") {
		t.Errorf("cmdKB(search) output = %q; want result title", out)
	}
}

func TestCmdKBDispatchesRead(t *testing.T) {
	kbTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(t, w, []map[string]interface{}{
			{"slug": "target", "title": "Target Fact", "body": "full body"},
		})
	})
	out := captureStdout(t, func() { cmdKB([]string{"read", "target"}) })
	if !strings.Contains(out, "Target Fact") || !strings.Contains(out, "full body") {
		t.Errorf("cmdKB(read) output = %q; want title and body", out)
	}
}

func TestCmdKBDispatchesCtx7Search(t *testing.T) {
	kbTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(t, w, []map[string]interface{}{})
	})
	out := captureStdout(t, func() { cmdKB([]string{"ctx7-search", "somelib"}) })
	if !strings.Contains(out, "No libraries found.") {
		t.Errorf("cmdKB(ctx7-search) output = %q; want empty-libraries notice", out)
	}
}

// --- usage printers ---

func TestPrintUsageListsAllCommands(t *testing.T) {
	out := captureStderr(t, printUsage)
	for _, cmd := range []string{"list", "ready", "create", "update", "close", "decompose", "reset", "remember", "kb", "dolt", "init", "BD_DIR"} {
		if !strings.Contains(out, cmd) {
			t.Errorf("printUsage() missing %q", cmd)
		}
	}
}

func TestPrintKBUsageListsAllSubcommands(t *testing.T) {
	out := captureStderr(t, printKBUsage)
	for _, sub := range []string{"search", "read", "import-url", "import-file", "import-ctx7", "ctx7-search", "list-docs", "BD_DASHBOARD_URL"} {
		if !strings.Contains(out, sub) {
			t.Errorf("printKBUsage() missing %q", sub)
		}
	}
}

// --- exit paths (subprocess re-exec) ---

// TestBDHelperProcess is not a real test: it is the subprocess entry point for
// the exit-path tests below. It re-invokes main() with the argv supplied via
// BD_HELPER_ARGS so that os.Exit terminates the child, not the test run.
func TestBDHelperProcess(t *testing.T) {
	if os.Getenv("BD_HELPER_PROCESS") != "1" {
		t.Skip("helper process for exit-path tests")
	}
	args := strings.Split(os.Getenv("BD_HELPER_ARGS"), "\x1f")
	if len(args) == 1 && args[0] == "" {
		args = nil
	}
	os.Args = append([]string{"bd"}, args...)
	main()
	os.Exit(0)
}

// runMainExpectExit1 re-execs the test binary running only TestBDHelperProcess
// with the given argv, asserting the process exits 1, and returns combined output.
func runMainExpectExit1(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestBDHelperProcess")
	cmd.Env = append(os.Environ(),
		"BD_HELPER_PROCESS=1",
		"BD_HELPER_ARGS="+strings.Join(args, "\x1f"),
		"BD_DIR="+t.TempDir(),
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

// errorsAs is a tiny local wrapper to keep the import list tidy.
func errorsAs(err error, target *(*exec.ExitError)) bool {
	e, ok := err.(*exec.ExitError)
	if ok {
		*target = e
	}
	return ok
}

func TestMainNoArgsExits1WithUsage(t *testing.T) {
	out := runMainExpectExit1(t)
	if !strings.Contains(out, "Usage: bd <command>") {
		t.Errorf("bd (no args) output = %q; want usage text", out)
	}
}

func TestMainUnknownCommandExits1(t *testing.T) {
	out := runMainExpectExit1(t, "frobnicate")
	if !strings.Contains(out, `unknown command "frobnicate"`) {
		t.Errorf("bd frobnicate output = %q; want unknown command error", out)
	}
}

func TestMainDoltNonPushExits1(t *testing.T) {
	out := runMainExpectExit1(t, "dolt", "pull")
	if !strings.Contains(out, "only 'push' subcommand is supported") {
		t.Errorf("bd dolt pull output = %q; want unsupported-subcommand error", out)
	}
}

func TestMainKBNoArgsExits1WithUsage(t *testing.T) {
	out := runMainExpectExit1(t, "kb")
	if !strings.Contains(out, "Usage: bd kb <subcommand>") {
		t.Errorf("bd kb output = %q; want kb usage text", out)
	}
}

func TestMainKBUnknownSubcommandExits1(t *testing.T) {
	out := runMainExpectExit1(t, "kb", "frobnicate")
	if !strings.Contains(out, `unknown subcommand "frobnicate"`) {
		t.Errorf("bd kb frobnicate output = %q; want unknown subcommand error", out)
	}
}

func TestMainCloseNoIDExits1(t *testing.T) {
	out := runMainExpectExit1(t, "close")
	if !strings.Contains(out, "requires a bead ID") {
		t.Errorf("bd close output = %q; want missing-ID error", out)
	}
}

func TestMainCloseUnknownIDExits1(t *testing.T) {
	out := runMainExpectExit1(t, "close", "bd-nonexistent")
	if !strings.Contains(out, "bd close:") {
		t.Errorf("bd close bd-nonexistent output = %q; want store error", out)
	}
}

func TestMainRememberNoArgsExits1(t *testing.T) {
	out := runMainExpectExit1(t, "remember")
	if !strings.Contains(out, "requires a fact string") {
		t.Errorf("bd remember output = %q; want missing-fact error", out)
	}
}
