package releaselines

import (
	"fmt"
	"io"
	"strings"
)

// Report writes findings for a human to read.
//
// When actions is true it also emits GitHub Actions workflow commands, so a
// failure is annotated on the offending file in the PR's Files-changed view
// rather than buried in a log, and an undecided release line surfaces as a
// notice on every run. Annotation messages are single-line by construction —
// Actions truncates at the first newline.
func Report(w io.Writer, findings []Finding, actions bool) {
	errs, notices := Errors(findings), Notices(findings)

	for _, f := range notices {
		fmt.Fprintf(w, "NOTICE  %s\n", f)
		if actions {
			fmt.Fprintf(w, "::notice file=%s,title=Release line undecided::%s\n", annotationFile(f), flatten(f.Message))
		}
	}
	for _, f := range errs {
		fmt.Fprintf(w, "ERROR   %s\n", f)
		if actions {
			fmt.Fprintf(w, "::error file=%s,title=Release line carry-forward::%s\n", annotationFile(f), flatten(f.String()))
		}
	}

	if len(errs) == 0 {
		fmt.Fprintf(w, "\nIn sync: every branch list checked against %s names the same release lines it does.\n", ConfigPath)
		return
	}
	fmt.Fprintf(w, "\n%d file(s) have fallen out of sync with %s.\n", countFiles(errs), ConfigPath)
	fmt.Fprint(w, strings.TrimSpace(fixHint)+"\n")
}

const fixHint = `
A release line was added or retired in .github/release-lines.yaml and the CI
files above still name the old set — or one of them was edited without updating
that file. Either way the fix is to make the two agree; the messages above name
the branch and the file. This is #4339's failure mode caught before it lands:
a workflow pinned to a branch that is not a release line does not fail, it
simply never runs, and reports green forever.
`

func annotationFile(f Finding) string {
	if f.File == "" {
		return ConfigPath
	}
	return f.File
}

func countFiles(findings []Finding) int {
	seen := map[string]bool{}
	for _, f := range findings {
		seen[annotationFile(f)] = true
	}
	return len(seen)
}
