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
	"os"

	"github.com/hivecommons/hive/pkg/dashboard/webstatic"
)

func main() {
	src := flag.String("src", "../static-src", "directory holding manifest.txt and the per-area sources")
	out := flag.String("out", "../static/index.html", "generated document to write")
	check := flag.Bool("check", false, "compare only; exit 1 if -out is not up to date")
	flag.Parse()

	got, err := webstatic.ConcatIndex(os.DirFS(*src))
	if err != nil {
		fmt.Fprintln(os.Stderr, "concat:", err)
		os.Exit(2)
	}
	if *check {
		have, err := os.ReadFile(*out)
		if err != nil {
			fmt.Fprintln(os.Stderr, "concat:", err)
			os.Exit(2)
		}
		if !bytes.Equal(have, got) {
			fmt.Fprintf(os.Stderr, "%s is out of date with %s; run `make dashboard-gen`\n", *out, *src)
			os.Exit(1)
		}
		return
	}
	if err := os.WriteFile(*out, got, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "concat:", err)
		os.Exit(2)
	}
}
