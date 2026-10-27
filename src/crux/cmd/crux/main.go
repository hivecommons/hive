// Command crux is the Crux agent runtime's own binary (hivecommons/hive#14138):
// run, observe and control agents through crux.Runtime (`crux run/ps/logs/
// kick/stop`), serve that runtime over the Crux API (`crux serve`) and, with
// --server or CRUX_SERVER, drive such a server instead of an in-process
// runtime. All behaviour lives in crux/cli; this package is the process
// boundary: signals, exit code, the version string and the module's own
// file-based definition loader (crux/defs, selected with --defs / CRUX_DEFS).
//
// It builds from the nested module alone:
//
//	cd src/crux && GOWORK=off go build ./cmd/crux
//
// The root module's cmd/crux is the same host wired to Hive's hive.yaml
// loader instead; the verbs and flags are identical.
package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/hivecommons/hive/crux/cli"
	"github.com/hivecommons/hive/crux/defs"
	"github.com/hivecommons/hive/crux/telemetry"
)

// version is what --version reports. Release builds set it with
// -ldflags "-X main.version=<tag>"; a plain `go build` reports cli.DefaultVersion.
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
	env := &cli.Env{Stdin: stdin, Stdout: stdout, Stderr: stderr, Loader: defs.Loader{}, Version: version}
	return env.Run(ctx, args)
}
