package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/kubestellar/hive/v2/pkg/beads"
	"github.com/kubestellar/hive/v2/pkg/visualhive"
)

type visualImportOutput struct {
	visualhive.Validation
	Applied bool `json:"applied"`
	Created int  `json:"created"`
	Skipped int  `json:"skipped"`
}

func runVisualCommand(args []string) int {
	if len(args) == 0 || args[0] != "import" || len(args) < 2 || (args[1] != "validate" && args[1] != "apply") {
		fmt.Fprintln(os.Stderr, "usage: hive visual import <validate|apply> --bundle <manifest.json> [--beads-dir <dir>] [--max-acmm 3] [--allow-local]")
		return 2
	}
	action := args[1]
	flags := flag.NewFlagSet("hive visual import "+action, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	bundlePath := flags.String("bundle", "", "path to a Visual Hive bundle manifest")
	beadsDir := flags.String("beads-dir", "/data/beads/quality", "target Hive beads directory")
	maxACMM := flags.Int("max-acmm", 3, "maximum ACMM level this Hive permits")
	allowLocal := flags.Bool("allow-local", false, "allow explicit local proof bundles (never use for an untrusted artifact)")
	if err := flags.Parse(args[2:]); err != nil {
		return 2
	}
	if *bundlePath == "" {
		fmt.Fprintln(os.Stderr, "--bundle is required")
		return 2
	}
	bundle, err := visualhive.ValidateBundle(*bundlePath, visualhive.ValidationOptions{MaxACMM: *maxACMM, AllowLocal: *allowLocal})
	if err != nil {
		fmt.Fprintln(os.Stderr, "visual evidence rejected:", err)
		return 1
	}
	output := visualImportOutput{Validation: bundle.Validation}
	if action == "apply" {
		store, err := beads.NewStore(*beadsDir)
		if err != nil {
			fmt.Fprintln(os.Stderr, "open beads store:", err)
			return 1
		}
		result, err := bundle.Import(store)
		if err != nil {
			fmt.Fprintln(os.Stderr, "import visual evidence:", err)
			return 1
		}
		output.Applied, output.Created, output.Skipped = true, result.Created, result.Skipped
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(output); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
