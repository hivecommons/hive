package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/kubestellar/hive/v2/pkg/integrated"
)

func main() {
	hiveBinary := flag.String("hive", "", "path to the Hive binary")
	hiveCommit := flag.String("hive-commit", "", "immutable Hive commit SHA")
	hiveVersion := flag.String("hive-version", "", "integrated Hive release version embedded in the binary and manifest")
	visualHiveDir := flag.String("visual-hive", "", "path to an unpacked Visual Hive release bundle")
	visualCommit := flag.String("visual-hive-commit", "", "immutable Visual Hive commit SHA")
	nodeBinary := flag.String("node", "", "path to the pinned Node 22 binary")
	nodeRuntime := flag.String("node-runtime", "", "path to the complete pinned Node 22 runtime with npm and corepack")
	nodeLicense := flag.String("node-license", "", "path to the Node license")
	skillDir := flag.String("skill", "", "path to the Hive Codex skill")
	targetOS := flag.String("target-os", "", "distribution operating system (linux or windows)")
	targetArch := flag.String("target-arch", "amd64", "distribution architecture")
	nodeVersion := flag.String("node-version", "", "verified Node runtime version for cross-packaging")
	output := flag.String("output", "", "new distribution output directory")
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	manifest, err := integrated.BuildDistribution(ctx, integrated.DistributionOptions{
		HiveBinary: *hiveBinary, HiveCommit: *hiveCommit, HiveVersion: *hiveVersion, VisualHiveDir: *visualHiveDir, VisualCommit: *visualCommit,
		NodeBinary: *nodeBinary, NodeRuntimeDir: *nodeRuntime, NodeLicense: *nodeLicense, NodeVersion: *nodeVersion, SkillDir: *skillDir,
		TargetOS: *targetOS, TargetArch: *targetArch, OutputDir: *output,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "build Hive distribution:", err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(manifest); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
