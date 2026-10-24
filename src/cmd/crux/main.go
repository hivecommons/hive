// Command crux runs, observes and controls agents through crux.Runtime
// (hivecommons/hive#13536; src/docs/crux-cli.md), serves that runtime over
// the Crux API (`crux serve`; src/docs/crux-serve.md) and, with --server or
// CRUX_SERVER, drives such a server instead of an in-process runtime. All
// behaviour lives in crux/cli; this package is the process boundary:
// signals, exit code and the Hive-backed definition loader (definitions.go)
// the CLI programs against through cli.DefinitionLoader.
package main

import (
	"context"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/hivecommons/hive/crux/cli"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, signalContext))
}

// signalContext ends on SIGINT or SIGTERM so a foreground `crux run` stops
// its agent before exiting.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer, ctxFn func() (context.Context, context.CancelFunc)) int {
	ctx, stop := ctxFn()
	defer stop()
	return cli.Main(ctx, args, stdin, stdout, stderr, hiveLoader{})
}
