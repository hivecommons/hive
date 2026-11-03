package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/hivecommons/hive/pkg/config"
)

// runConfigDiff starts nothing: the entrypoint calls it before snapshot writes.
// Positional paths override the conventional prior layers for operator use.
func runConfigDiff(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("config diff", flag.ContinueOnError)
	fs.SetOutput(stderr)
	defaultPath := os.Getenv("HIVE_CONFIG")
	if defaultPath == "" {
		defaultPath = "/etc/hive/hive.yaml"
	}
	path := fs.String("config", defaultPath, "effective config path")
	asJSON := fs.Bool("json", false, "emit boot warnings as JSON")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	previous := fs.Args()
	if len(previous) == 0 {
		previous = []string{config.RuntimeConfigFile, config.RuntimeConfigFile + ".bak", config.RuntimeConfigFileLegacy, config.DashboardOverlayFile}
	}
	warnings, strict, err := config.SafetyGateDiff(*path, previous)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	for _, warning := range warnings {
		fmt.Fprintln(stderr, "WARN", warning)
	}
	if *asJSON {
		if err := json.NewEncoder(stdout).Encode(warnings); err != nil {
			return 1
		}
	} else if len(warnings) == 0 {
		fmt.Fprintln(stdout, "No safety gate regressions")
	}
	if strict && len(warnings) > 0 {
		fmt.Fprintln(stderr, "refusing to start: config.refuse_gate_regression is enabled; restore the safety gates or explicitly disable strict mode")
		return 1
	}
	return 0
}
