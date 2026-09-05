package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/hivecommons/hive/pkg/config"
)

// configCheckExitCode is what `hive validate` returns when the config is bad.
// Deliberately 1, the same code a boot-time config failure exits with, so a
// CI step or a pre-flight init container needs no special casing.
const configCheckExitCode = 1

// runConfigCheck implements `hive validate` (alias `hive --config-check`).
//
// It exists because #6024 had no answer to "is this config valid?" other than
// applying it and watching the pod crash-loop - and once a spoke is in that
// loop the dashboard API that is the only supported way to fix the config is
// unreachable, because it is served by the process the config kills.
//
// The load path is deliberately LoadWithDashboardOverlay, byte for byte what
// main() calls, so what this validates is what the hive would actually boot -
// including the per-agent overlay files under data.agents_dir. Validating only
// hive.yaml would have declared the #6024 spoke's config perfectly healthy,
// since the contradiction lived in /data/agent-configs/supervisor.yaml.
//
// Returns a process exit code; it never calls os.Exit itself so tests can drive
// it directly.
func runConfigCheck(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	defaultConfig := "/etc/hive/hive.yaml"
	if envCfg := os.Getenv("HIVE_CONFIG"); envCfg != "" {
		defaultConfig = envCfg
	}
	configPath := fs.String("config", defaultConfig, "path to hive.yaml config file")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "usage: hive validate [-config path]\n\nLoads and validates the config the way a real boot does, including the\nper-agent overlay files under data.agents_dir, then exits 0 when it is\nvalid and %d when it is not. Starts nothing.\n\n", configCheckExitCode)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return configCheckExitCode
	}

	cfg, err := config.LoadWithDashboardOverlay(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "config INVALID: %s\n%v\n", *configPath, err)
		return configCheckExitCode
	}

	names := make([]string, 0, len(cfg.Agents))
	for name := range cfg.Agents {
		names = append(names, name)
	}
	sort.Strings(names)
	fmt.Fprintf(stdout, "config OK: %s\n", *configPath)
	fmt.Fprintf(stdout, "  agents (%d): %v\n", len(names), names)
	// Overlays that RejectInvalidAgentOverlays dropped are gone from cfg by
	// now and were logged at ERROR during the load; surface the surviving
	// provenance so an operator can see which entries came from files they
	// might not have thought to look at.
	for _, name := range names {
		agent := cfg.Agents[name]
		if src := agent.SourceFile(); src != "" {
			fmt.Fprintf(stdout, "    %s <- %s\n", name, src)
		}
	}
	return 0
}
