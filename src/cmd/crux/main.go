// Command crux runs, observes and controls agents through crux.Runtime
// (hivecommons/hive#13536; src/docs/crux-cli.md). All behaviour lives in
// pkg/crux/cli; this file is the process boundary: signals and exit code.
package main

import (
	"context"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/hivecommons/hive/pkg/crux/cli"
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
	return cli.Main(ctx, args, stdin, stdout, stderr)
}
