// Command crux is the Hive-hosted build of the crux binary: it runs,
// observes and controls agents through crux.Runtime (hivecommons/hive#13536;
// src/docs/crux-cli.md), serves that runtime over the Crux API (`crux serve`;
// src/docs/crux-serve.md) and, with --server or CRUX_SERVER, drives such a
// server instead of an in-process runtime. All behaviour lives in crux/cli;
// this package is a thin host in the root module whose one job is the
// Hive-backed definition loader (definitions.go: hive.yaml through pkg/config,
// the portable AgentDefinition through pkg/defsrc) the CLI programs against
// through cli.DefinitionLoader. The same binary built from the nested module
// alone is crux/cmd/crux, which wires crux/defs (plain definition files)
// instead; verbs and flags are identical.
package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/hivecommons/hive/crux/cli"
	"github.com/hivecommons/hive/crux/telemetry"
)

// version is what --version reports; set with -ldflags "-X main.version=…".
var version = cli.DefaultVersion

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, cli.SignalContext))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer, ctxFn func() (context.Context, context.CancelFunc)) int {
	ctx, stop := ctxFn()
	defer stop()
	// OTLP export when OTEL_EXPORTER_OTLP_* names an endpoint; a no-op
	// otherwise (hivecommons/crux#19). A bad configuration costs telemetry,
	// never the command.
	flush, err := telemetry.Init(ctx, version)
	if err != nil {
		fmt.Fprintln(stderr, "crux: telemetry disabled:", err)
	}
	defer flush()
	env := &cli.Env{Stdin: stdin, Stdout: stdout, Stderr: stderr, Loader: hiveLoader{}, Version: version}
	return env.Run(ctx, args)
}
