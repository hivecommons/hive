// Command concat regenerates the dashboard's static/index.html from the
// per-area sources in static-src/ (see static-src/manifest.txt). It is wired
// to `go generate` in package webstatic and to `make dashboard-gen`.
//
// With -check it writes nothing and exits non-zero when the committed artifact
// differs from the concatenation, which is the same comparison
// TestIndexHTMLIsGenerated performs in CI.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/hivecommons/hive/pkg/dashboard/webstatic"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

// run is main without the process exit so it can be tested: 0 on success,
// 1 when -check finds the artifact stale, 2 on any other error.
func run(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("concat", flag.ContinueOnError)
	fs.SetOutput(stderr)
	src := fs.String("src", "../static-src", "directory holding manifest.txt and the per-area sources")
	out := fs.String("out", "../static/index.html", "generated document to write")
	check := fs.Bool("check", false, "compare only; exit 1 if -out is not up to date")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	got, err := webstatic.ConcatIndex(os.DirFS(*src))
	if err != nil {
		fmt.Fprintln(stderr, "concat:", err)
		return 2
	}
	if *check {
		have, err := os.ReadFile(*out)
		if err != nil {
			fmt.Fprintln(stderr, "concat:", err)
			return 2
		}
		if !bytes.Equal(have, got) {
			fmt.Fprintf(stderr, "%s is out of date with %s; run `make dashboard-gen`\n", *out, *src)
			return 1
		}
		return 0
	}
	if err := os.WriteFile(*out, got, 0o644); err != nil {
		fmt.Fprintln(stderr, "concat:", err)
		return 2
	}
	return 0
}
