package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLIJSONRequested(t *testing.T) {
	for _, args := range [][]string{{"--json"}, {"-json"}, {"--json=true"}, {"-json=1"}, {"--other", "--json=invalid"}, {"--json=false", "--json=true"}, {"--json=false", "--json=invalid"}} {
		if !cliJSONRequested(args) {
			t.Fatalf("cliJSONRequested(%q) = false", args)
		}
	}
	for _, args := range [][]string{nil, {"--json=false"}, {"-json=0"}, {"value-with---json"}} {
		if cliJSONRequested(args) {
			t.Fatalf("cliJSONRequested(%q) = true", args)
		}
	}
}

func TestRunCLICommandWithJSONContract(t *testing.T) {
	tests := []struct {
		name       string
		commandOut string
		commandRC  int
		wantRC     int
		wantOK     bool
		wantCode   string
	}{
		{name: "success", commandOut: `{"ok":true}`, wantOK: true},
		{name: "structured failure", commandOut: `{"ok":false,"error":"expected"}`, commandRC: 2, wantRC: 2, wantOK: true},
		{name: "empty failure", commandRC: 2, wantRC: 2, wantCode: "command_failed_before_json"},
		{name: "empty success", wantRC: 1, wantCode: "invalid_json_stdout"},
		{name: "human prefix", commandOut: "progress\n{\"ok\":true}", wantRC: 1, wantCode: "invalid_json_stdout"},
		{name: "two documents", commandOut: "{}\n{}", wantRC: 1, wantCode: "invalid_json_stdout"},
		{name: "null is not an object", commandOut: "null", wantRC: 1, wantCode: "invalid_json_stdout"},
		{name: "array is not an object", commandOut: "[]", wantRC: 1, wantCode: "invalid_json_stdout"},
		{name: "invalid UTF-8", commandOut: string([]byte{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'}), wantRC: 1, wantCode: "invalid_json_stdout"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			output, code := captureCLIContractStdout(t, func() int {
				return runCLICommandWithJSONContract("setup", []string{"--json"}, func() int {
					fmt.Fprint(os.Stdout, test.commandOut)
					return test.commandRC
				})
			})
			if code != test.wantRC {
				t.Fatalf("exit = %d, want %d; output=%s", code, test.wantRC, output)
			}
			var document map[string]any
			if err := json.Unmarshal(output, &document); err != nil {
				t.Fatalf("stdout is not one JSON document: %v: %q", err, output)
			}
			if test.wantOK {
				if _, exists := document["error_code"]; exists {
					t.Fatalf("valid command output was replaced: %s", output)
				}
			} else if document["error_code"] != test.wantCode {
				t.Fatalf("error_code = %v, want %s: %s", document["error_code"], test.wantCode, output)
			}
		})
	}
}

func TestRunCLICommandWithJSONContractRecoversPanic(t *testing.T) {
	output, code := captureCLIContractStdout(t, func() int {
		return runCLICommandWithJSONContract("doctor", []string{"--json"}, func() int {
			panic("sensitive panic payload")
		})
	})
	if code != 1 {
		t.Fatalf("panic exit=%d output=%s", code, output)
	}
	var document map[string]any
	if err := json.Unmarshal(output, &document); err != nil || document["error_code"] != "command_panicked" {
		t.Fatalf("panic was not converted into one safe JSON object: %v: %s", err, output)
	}
	if strings.Contains(string(output), "sensitive panic payload") {
		t.Fatal("panic payload leaked into machine output")
	}
}

func TestEarlyCLIJSONSurfacesAreStructuredAndDoNotStartStreams(t *testing.T) {
	priorVersion, priorCommit := integratedVersion, gitHash
	integratedVersion, gitHash = "v0.4.1-integrated.19", strings.Repeat("a", 40)
	defer func() { integratedVersion, gitHash = priorVersion, priorCommit }()

	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantKey  string
		wantVal  any
	}{
		{name: "version", args: []string{"--version", "--json"}, wantKey: "schema_version", wantVal: "hive.version.v1"},
		{name: "mcp stream rejection", args: []string{"mcp-server", "--json"}, wantCode: 2, wantKey: "error_code", wantVal: "command_failed_before_json"},
		{name: "unknown command", args: []string{"not-a-command", "--json"}, wantCode: 2, wantKey: "error_code", wantVal: "command_failed_before_json"},
		{name: "later malformed JSON flag", args: []string{"status", "--json=false", "--json=invalid"}, wantCode: 2, wantKey: "error_code", wantVal: "command_failed_before_json"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			output, code := captureCLIContractStdout(t, func() int {
				handled, result := runEarlyCLI(test.args)
				if !handled {
					t.Fatal("JSON-bearing early command was not handled")
				}
				return result
			})
			if code != test.wantCode {
				t.Fatalf("exit=%d want=%d output=%s", code, test.wantCode, output)
			}
			var document map[string]any
			if err := json.Unmarshal(output, &document); err != nil || document[test.wantKey] != test.wantVal {
				t.Fatalf("unexpected structured result: err=%v document=%+v output=%s", err, document, output)
			}
		})
	}
}

func TestMutatingCommandRejectsTrailingPositionalBeforeJSON(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "must-not-exist")
	args := []string{"--state-dir", stateDir, "unexpected", "--json"}
	output, code := captureCLIContractStdout(t, func() int {
		return runCLICommandWithJSONContract("pause", args, func() int {
			return runIntegratedPause("pause", args)
		})
	})
	if code != 2 {
		t.Fatalf("pause exit = %d, want 2: %s", code, output)
	}
	if _, err := os.Lstat(stateDir); !os.IsNotExist(err) {
		t.Fatalf("malformed pause mutated state before rejecting its positional argument: %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(output, &document); err != nil || document["error_code"] != "command_failed_before_json" {
		t.Fatalf("malformed pause did not return one structured failure: %v: %s", err, output)
	}
}

func TestSetupJSONMissingInputsNeverPrompts(t *testing.T) {
	args := []string{"--state-dir", filepath.Join(t.TempDir(), "state"), "--json"}
	output, code := captureCLIContractStdout(t, func() int {
		return runCLICommandWithJSONContract("setup", args, func() int { return runSetupCommand(args) })
	})
	if code != 2 {
		t.Fatalf("setup exit = %d, want 2: %s", code, output)
	}
	var document map[string]any
	if err := json.Unmarshal(output, &document); err != nil || document["error_code"] != "command_failed_before_json" {
		t.Fatalf("setup missing-input failure is not structured: %v: %s", err, output)
	}
}

func TestVisualStatusAcceptsExplicitJSONContract(t *testing.T) {
	args := []string{"lifecycle", "status", "--state-dir", filepath.Join(t.TempDir(), "lifecycle"), "--json"}
	output, code := captureCLIContractStdout(t, func() int {
		return runCLICommandWithJSONContract("visual", args, func() int { return runVisualCommand(args) })
	})
	if code != 0 {
		t.Fatalf("visual status exit = %d: %s", code, output)
	}
	var document map[string]any
	if err := json.Unmarshal(output, &document); err != nil {
		t.Fatalf("visual status stdout is not one JSON document: %v: %s", err, output)
	}
}

func captureCLIContractStdout(t *testing.T, run func() int) ([]byte, int) {
	t.Helper()
	outer, err := os.CreateTemp(t.TempDir(), "stdout-*.json")
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = outer
	defer func() { os.Stdout = original }()
	code := run()
	os.Stdout = original
	if err := outer.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := os.Open(outer.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return data, code
}
