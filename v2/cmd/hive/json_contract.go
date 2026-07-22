package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

var cliJSONOutputMu sync.Mutex

type boundedJSONCapture struct {
	data  bytes.Buffer
	total int64
}

func (capture *boundedJSONCapture) Write(value []byte) (int, error) {
	capture.total += int64(len(value))
	remaining := maxCLIJSONOutputBytes + 1 - capture.data.Len()
	if remaining > 0 {
		if remaining < len(value) {
			_, _ = capture.data.Write(value[:remaining])
		} else {
			_, _ = capture.data.Write(value)
		}
	}
	return len(value), nil
}

const (
	cliJSONContractSchema = "hive.cli-json-result.v1"
	maxCLIJSONOutputBytes = 16 << 20
)

func parseExactFlags(flags *flag.FlagSet, args []string) error {
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		err := fmt.Errorf("unexpected positional arguments: %s", strings.Join(flags.Args(), " "))
		fmt.Fprintln(flags.Output(), err)
		return err
	}
	return nil
}

// runCLICommandWithJSONContract makes --json a process-level contract instead
// of relying on every early return in every subcommand to remember to emit a
// document. Human diagnostics remain on stderr. Stdout is released only after
// it has been proven to contain exactly one bounded JSON value.
func runCLICommandWithJSONContract(command string, args []string, run func() int) int {
	if !cliJSONRequested(args) {
		return run()
	}
	cliJSONOutputMu.Lock()
	defer cliJSONOutputMu.Unlock()

	originalStdout := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		return emitCLIJSONContractFailure(originalStdout, command, 1, "stdout_capture_failed", err)
	}
	captured := &boundedJSONCapture{}
	readDone := make(chan error, 1)
	go func() {
		_, copyErr := io.Copy(captured, reader)
		_ = reader.Close()
		readDone <- copyErr
	}()
	var panicValue any
	code := func() (code int) {
		os.Stdout = writer
		defer func() {
			os.Stdout = originalStdout
			_ = writer.Close()
		}()
		defer func() {
			if recovered := recover(); recovered != nil {
				panicValue = recovered
				code = 1
			}
		}()
		return run()
	}()
	if readErr := <-readDone; readErr != nil {
		return emitCLIJSONContractFailure(originalStdout, command, nonzeroExit(code), "stdout_capture_failed", readErr)
	}
	if panicValue != nil {
		fmt.Fprintf(os.Stderr, "%s --json stopped after an internal panic; no panic payload was written to machine output\n", command)
		return emitCLIJSONContractFailure(originalStdout, command, nonzeroExit(code), "command_panicked", nil)
	}
	if captured.total > maxCLIJSONOutputBytes {
		fmt.Fprintf(os.Stderr, "%s --json produced %d bytes, exceeding the %d-byte machine-output limit\n", command, captured.total, maxCLIJSONOutputBytes)
		return emitCLIJSONContractFailure(originalStdout, command, nonzeroExit(code), "stdout_too_large", nil)
	}

	trimmed := bytes.TrimSpace(captured.data.Bytes())
	var document map[string]any
	if len(trimmed) == 0 || !utf8.Valid(trimmed) || json.Unmarshal(trimmed, &document) != nil || document == nil {
		reason := "command_failed_before_json"
		if code == 0 || len(trimmed) > 0 {
			reason = "invalid_json_stdout"
		}
		fmt.Fprintf(os.Stderr, "%s --json did not produce exactly one valid JSON document\n", command)
		return emitCLIJSONContractFailure(originalStdout, command, nonzeroExit(code), reason, nil)
	}
	if _, err := originalStdout.Write(append(trimmed, '\n')); err != nil {
		fmt.Fprintln(os.Stderr, "write validated JSON stdout:", err)
		return 1
	}
	return code
}

func cliJSONRequested(args []string) bool {
	for _, arg := range args {
		trimmed := strings.TrimSpace(arg)
		switch trimmed {
		case "--json", "-json":
			return true
		}
		for _, prefix := range []string{"--json=", "-json="} {
			if strings.HasPrefix(trimmed, prefix) {
				value, err := strconv.ParseBool(strings.TrimPrefix(trimmed, prefix))
				if err != nil || value {
					return true
				}
				break
			}
		}
	}
	return false
}

func nonzeroExit(code int) int {
	if code == 0 {
		return 1
	}
	return code
}

func emitCLIJSONContractFailure(writer io.Writer, command string, exitCode int, errorCode string, cause error) int {
	message := "command failed before it emitted a machine-readable result; see stderr for diagnostics"
	if errorCode == "invalid_json_stdout" {
		message = "command violated the machine-readable stdout contract; see stderr for diagnostics"
	} else if errorCode == "stdout_too_large" {
		message = "command exceeded the bounded machine-readable stdout contract; see stderr for diagnostics"
	} else if errorCode == "stdout_capture_failed" {
		message = "Hive could not enforce the machine-readable stdout contract"
	} else if errorCode == "command_panicked" {
		message = "command stopped after an internal failure; see stderr for diagnostics"
	}
	if cause != nil {
		fmt.Fprintln(os.Stderr, message+":", cause)
	}
	payload := map[string]any{
		"schema_version": cliJSONContractSchema,
		"ok":             false,
		"command":        command,
		"exit_code":      exitCode,
		"error_code":     errorCode,
		"error":          message,
	}
	if err := json.NewEncoder(writer).Encode(payload); err != nil {
		fmt.Fprintln(os.Stderr, "encode JSON contract failure:", err)
		return 1
	}
	return exitCode
}
