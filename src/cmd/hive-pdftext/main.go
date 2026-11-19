// Command hive-pdftext is the sandbox the Inbox reads one uploaded PDF in.
// The hive starts it as the hive-transcribe uid through su-exec, with an
// empty environment, under prlimit and nice and, where the kernel allows it,
// in an empty network namespace; the PDF arrives on stdin and the text
// leaves as JSON on stdout. See pkg/inbox/pdfbox and
// src/docs/inbox-uploads.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/hivecommons/hive/pkg/inbox/pdfbox"
)

// version is the sandbox protocol version printed by -version.
const version = "hive-pdftext 1"

func main() { os.Exit(runMain(os.Args[1:])) }

func runMain(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer stop()
	return run(ctx, args, os.Stdin, os.Stdout, os.Stderr)
}

// run parses args and reads one PDF.
func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	var o pdfbox.Options
	fs := flag.NewFlagSet("hive-pdftext", flag.ContinueOnError)
	fs.SetOutput(stderr)
	showVersion := fs.Bool("version", false, "print the version and exit")
	fs.Int64Var(&o.MaxBytes, "max-bytes", 0, "largest PDF accepted on stdin (0: no cap)")
	fs.IntVar(&o.MaxPages, "max-pages", pdfbox.DefaultMaxPages, "pages read at most")
	fs.IntVar(&o.MaxChars, "max-chars", pdfbox.DefaultMaxChars, "characters returned at most")
	fs.StringVar(&o.WorkRoot, "work-root", "", "where the private work dir is created (default: $TMPDIR or /tmp)")
	if err := fs.Parse(args); err != nil {
		return pdfbox.ExitUsage
	}
	if *showVersion {
		fmt.Fprintln(stdout, version)
		return pdfbox.ExitOK
	}
	if err := pdfbox.Run(ctx, o, stdin, stdout); err != nil {
		fmt.Fprintln(stderr, "hive-pdftext: "+err.Error())
		var pe *pdfbox.Error
		if errors.As(err, &pe) {
			return pe.Code
		}
		return pdfbox.ExitSetup
	}
	return pdfbox.ExitOK
}
