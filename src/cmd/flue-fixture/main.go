// Command flue-fixture runs the deterministic Flue stand-in from
// pkg/extwork/flue/fixture as a standalone process, for the runnable example
// under examples/flue/ and for manual probing. It links the fixture only,
// never the Flue adapter or any hive package that holds credentials.
package main

import (
	"context"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/hivecommons/hive/pkg/extwork/flue/fixture"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, signalContext))
}

// signalContext ends on SIGINT or SIGTERM.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// run serves the fixture until the context from ctxFn ends and returns the
// process exit code.
func run(args []string, stdout, stderr io.Writer, ctxFn func() (context.Context, context.CancelFunc)) int {
	ctx, stop := ctxFn()
	defer stop()
	return fixture.Run(ctx, args, stdout, stderr)
}
