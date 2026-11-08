package codescanning

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Alert-table delimiters. The sweep rewrites only the marker and the text
// between these on later passes, so the summary, code excerpt and upstream
// note written at filing time survive.
const (
	alertsStart = "<!-- hive:code-scanning-alerts -->"
	alertsEnd   = "<!-- /hive:code-scanning-alerts -->"
)

// maxAlertRows bounds the table so a large group cannot outgrow GitHub's
// 65,536-character body limit; the marker still lists every alert.
const maxAlertRows = 100

// workflowsDocURL is the hive doc on handing workflow-file work to the lane
// that can push it.
const workflowsDocURL = "https://github.com/hivecommons/hive/blob/v6/src/docs/hive-open-issue.md"

// AlertStatus is a tracked alert's state on GitHub as the sweep last saw it.
type AlertStatus string

// Alert states the table shows.
const (
	StatusOpen      AlertStatus = "open"
	StatusFixed     AlertStatus = "fixed"
	StatusDismissed AlertStatus = "dismissed"
)

// Title is the issue title, before the filer tag ("[governor] ") the forge
// adds.
func Title(g Group) string {
	n := len(g.Alerts)
	plural := "s"
	if n == 1 {
		plural = ""
	}
	if g.Package != "" {
		return fmt.Sprintf("Code scanning: %s has %d known vulnerabilit%s (%s)", g.Package, len(g.Rules), map[bool]string{true: "y", false: "ies"}[len(g.Rules) == 1], g.Level())
	}
	rule := strings.Join(g.Rules, ", ")
	return fmt.Sprintf("Code scanning: %s in %s (%d %s alert%s)", rule, where(g.Paths), n, g.Level(), plural)
}

func where(paths []string) string {
	switch {
	case len(paths) == 0:
		return "repository settings"
	case len(paths) == 1:
		return paths[0]
	}
	dir := path.Dir(paths[0])
	for _, p := range paths[1:] {
		if path.Dir(p) != dir {
			return fmt.Sprintf("%d files", len(paths))
		}
	}
	return fmt.Sprintf("%s/ (%d files)", dir, len(paths))
}

// BodyInput is what Body needs beyond the group.
type BodyInput struct {
	Repo  string
	Group Group
	// Excerpt is the source around the first alert's lines at the analysed
	// commit ("" when it could not be read or the group has no source file).
	Excerpt     string
	ExcerptPath string
	// Upstream is the fork-parent note ("" when the repo is not a fork or
	// the check found nothing to say).
	Upstream string
	// WorkflowsLane is the lane that can push .github/workflows/** in this
	// repo ("" when none can).
	WorkflowsLane string
}

// Body renders a new issue: executive summary, evidence, wanted behaviour,
// acceptance criteria, test plan, false-positive handling and scope, so a
// lane can do it cold.
func Body(in BodyInput) string {
	g := in.Group
	var b strings.Builder
	b.WriteString(markerFor(g, nil).String() + "\n\n")
	b.WriteString("## Summary\n\n")
	if g.Package != "" {
		d, _ := g.Alerts[0].Dependency()
		fmt.Fprintf(&b, "GitHub code scanning (%s) reports that `%s` (installed `%s`) has %d known vulnerabilit%s, highest **%s**. ", g.Tool, g.Package, orUnknown(d.Installed), len(g.Rules), map[bool]string{true: "y", false: "ies"}[len(g.Rules) == 1], g.Level())
		if d.Fixed != "" {
			fmt.Fprintf(&b, "The scanner says it is fixed in `%s`. ", d.Fixed)
		}
		b.WriteString("The flagged paths below are where the scanner found the package, often a built binary rather than a source file.\n\n")
	} else {
		fmt.Fprintf(&b, "GitHub code scanning (%s) reports %d alert%s for `%s` (%s) in %s, highest **%s**. ", g.Tool, len(g.Alerts), map[bool]string{true: "", false: "s"}[len(g.Alerts) == 1], strings.Join(g.Rules, "`, `"), ruleTitle(g), where(g.Paths), g.Level())
		b.WriteString("The hive grouped them into one issue sized for one PR.\n\n")
	}
	b.WriteString("**These alerts may be false positives.** Verify each one against the code before changing anything; a static analyser does not know which inputs can reach a line.\n\n")
	if g.WorkflowOnly() {
		b.WriteString(workflowNote(in.WorkflowsLane))
	}

	b.WriteString("## Evidence\n\n")
	if sha := g.Alerts[0].Instance.CommitSHA; sha != "" {
		fmt.Fprintf(&b, "Analysed commit: `%s`", shortSHA(sha))
		if ref := g.Alerts[0].Instance.Ref; ref != "" {
			fmt.Fprintf(&b, " on `%s`", ref)
		}
		b.WriteString(". Line numbers are at that commit.\n\n")
	}
	b.WriteString(alertsSection(in.Repo, g.Alerts, nil, nil) + "\n")
	if in.Excerpt != "" {
		fmt.Fprintf(&b, "\nCode at the analysed commit (`%s`):\n\n```%s\n%s\n```\n", in.ExcerptPath, fenceLang(in.ExcerptPath), strings.TrimRight(in.Excerpt, "\n"))
	}
	if help := g.Alerts[0].Rule.HelpURI; help != "" {
		fmt.Fprintf(&b, "\nRule documentation: %s\n", help)
	} else if u := g.Alerts[0].HTMLURL; u != "" {
		fmt.Fprintf(&b, "\nThe rule's explanation and recommended fix are on each alert page (for example %s).\n", u)
	}
	if in.Upstream != "" {
		b.WriteString("\n### Upstream\n\n" + in.Upstream + "\n")
	}

	b.WriteString("\n## Wanted behaviour\n\n")
	switch {
	case g.Package != "":
		d, _ := g.Alerts[0].Dependency()
		fmt.Fprintf(&b, "`%s` is at a version with none of the listed vulnerabilities", g.Package)
		if d.Fixed != "" {
			fmt.Fprintf(&b, " (`%s` or later)", strings.TrimSpace(strings.Split(d.Fixed, ",")[0]))
		}
		b.WriteString(". Find where the flagged artifact gets the package: the `go.mod`/lockfile of the program that builds it, or the `Containerfile`/`Dockerfile` that installs it. Bump it there, not in the built output.")
		if g.Package == "stdlib" {
			b.WriteString(" For Go's `stdlib` that means building with a newer Go toolchain: the `go`/`toolchain` line in `go.mod`, or the Go version of the builder image.")
		}
		b.WriteString("\n")
	case len(g.Paths) == 0:
		b.WriteString("The repository-level check passes. This usually needs a settings or policy change by a maintainer; if so, say so here instead of changing code.\n")
	default:
		b.WriteString("Each listed alert is fixed in the code, so the next code scanning analysis of the default branch no longer reports it, or is shown on this issue to be a false positive. The behaviour of the code is otherwise unchanged.\n")
	}

	b.WriteString("\n## Acceptance criteria\n\n")
	b.WriteString("- [ ] Every alert listed above is fixed at the cited location, or reported on this issue as a false positive with the reason.\n")
	b.WriteString("- [ ] The change touches only what the alerts need; no unrelated refactoring.\n")
	b.WriteString("- [ ] No check is weakened to silence an alert: no suppression comments, no edits to the code scanning workflow or its config, no alert dismissed on GitHub.\n")
	b.WriteString("- [ ] The repository's build and tests pass.\n")

	b.WriteString("\n## Test plan\n\n")
	b.WriteString("- Build the project and run its existing tests.\n")
	if g.Package != "" {
		b.WriteString("- Rebuild the flagged artifact and confirm it now carries the fixed version (for Go: `go version -m <binary>`).\n")
	} else if len(g.Paths) > 0 {
		b.WriteString("- Where practical, add or extend a test that drives the flagged input through the fixed code.\n")
	}
	b.WriteString("- After the PR merges, the next code scanning run closes the alerts. The hive then closes this issue on its own; it is not closed by the PR.\n")

	b.WriteString("\n## If an alert is a false positive\n\n")
	b.WriteString("Comment on this issue with the alert number and why it cannot happen (which input cannot reach the line, or why the pattern is safe here). Do not change code just to make the analyser quiet. A maintainer decides whether to dismiss the alert on GitHub; the hive closes this issue once every listed alert is fixed or dismissed.\n")

	b.WriteString("\n## Out of scope\n\n")
	b.WriteString("- Other code scanning alerts, other rules and files not listed here; the hive files those separately.\n")
	b.WriteString("- The code scanning setup itself (its workflow, query suites and config).\n")
	b.WriteString("- Dismissing alerts on GitHub, which is a maintainer's call.\n")
	return b.String()
}

func workflowNote(lane string) string {
	var b strings.Builder
	b.WriteString("**This fix edits GitHub Actions workflow files** (`.github/workflows/`). Pushing them needs the GitHub App's Workflows permission, and the hive's sentinel flags workflow permission changes for maintainer review. ")
	if lane != "" {
		fmt.Fprintf(&b, "This issue carries `hive/workflows-lane`, so it goes to `%s`, the lane that can push workflow files.", lane)
	} else {
		fmt.Fprintf(&b, "No lane serving this repository can push workflow files. A lane that cannot push them should not try: park the issue with the workflows blocker as described in [hive-open-issue](%s).", workflowsDocURL)
	}
	b.WriteString("\n\n")
	return b.String()
}

func ruleTitle(g Group) string {
	if d := strings.TrimSpace(g.Alerts[0].Rule.Description); d != "" {
		return d
	}
	return g.Alerts[0].Rule.Name
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func fenceLang(p string) string {
	switch strings.ToLower(path.Ext(p)) {
	case ".c", ".h":
		return "c"
	case ".cc", ".cpp", ".cxx", ".hpp", ".hh", ".hxx":
		return "cpp"
	case ".go":
		return "go"
	case ".py":
		return "python"
	case ".js", ".mjs", ".cjs":
		return "javascript"
	case ".ts", ".tsx":
		return "typescript"
	case ".yml", ".yaml":
		return "yaml"
	case ".sh", ".bash":
		return "bash"
	case ".java":
		return "java"
	case ".rs":
		return "rust"
	case ".rb":
		return "ruby"
	}
	return ""
}

// markerFor builds the marker for a group plus alerts appended later.
func markerFor(g Group, extra []int) Marker {
	m := Marker{Tool: g.Tool, Rules: g.Rules, Package: g.Package, Paths: g.Paths}
	for _, a := range g.Alerts {
		m.Alerts = append(m.Alerts, a.Number)
	}
	m.Alerts = mergeInts(m.Alerts, extra)
	return m
}

func mergeInts(a, b []int) []int {
	seen := map[int]bool{}
	var out []int
	for _, v := range append(append([]int(nil), a...), b...) {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Ints(out)
	return out
}

var alertRowRE = regexp.MustCompile(`(?m)^\| \[#(\d+)\]\([^)]*\) \| [a-z]+ \|.*$`)

// alertsSection renders the delimited alert table. open holds the alerts
// still open (full rows); gone maps tracked alerts that are no longer open to
// their status, re-using the row from prev (the old section) when there is
// one so the location written at filing time is kept.
func alertsSection(repo string, open []Alert, gone map[int]AlertStatus, prev map[int]string) string {
	rows := map[int]string{}
	for _, a := range open {
		loc := a.Path()
		if loc == "" {
			loc = "(repository)"
		} else if l := a.Instance.Location.StartLine; l > 0 {
			loc += ":" + strconv.Itoa(l)
		}
		rows[a.Number] = fmt.Sprintf("| [#%d](%s) | %s | `%s` | %s | %s |", a.Number, alertURL(repo, a), StatusOpen, loc, a.Level(), cell(a.Rule.ID+": "+a.Instance.Message.Text))
	}
	for n, st := range gone {
		if old, ok := prev[n]; ok {
			rows[n] = alertRowRE.ReplaceAllStringFunc(old, func(r string) string {
				i := strings.Index(r, ") | ")
				j := strings.Index(r[i+4:], " |")
				return r[:i+4] + string(st) + r[i+4+j:]
			})
			continue
		}
		rows[n] = fmt.Sprintf("| [#%d](%s) | %s | | | |", n, alertURL(repo, Alert{Number: n}), st)
	}
	nums := make([]int, 0, len(rows))
	for n := range rows {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	var b strings.Builder
	b.WriteString(alertsStart + "\n")
	fmt.Fprintf(&b, "%d alert%s, %d still open.\n\n", len(nums), map[bool]string{true: "", false: "s"}[len(nums) == 1], len(open))
	b.WriteString("| Alert | Status | Location | Severity | Rule and message |\n| --- | --- | --- | --- | --- |\n")
	for i, n := range nums {
		if i == maxAlertRows {
			fmt.Fprintf(&b, "\n%d more alerts are tracked; the marker at the top lists every number.\n", len(nums)-i)
			break
		}
		b.WriteString(rows[n] + "\n")
	}
	b.WriteString(alertsEnd)
	return b.String()
}

func alertURL(repo string, a Alert) string {
	if a.HTMLURL != "" {
		return a.HTMLURL
	}
	return "https://github.com/" + repo + "/security/code-scanning/" + strconv.Itoa(a.Number)
}

// cell flattens analyser text into one table cell.
func cell(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	s = strings.ReplaceAll(s, "|", `\|`)
	s = strings.ReplaceAll(s, "<!--", "&lt;!--")
	if len(s) > 200 {
		s = s[:197] + "..."
	}
	return s
}

// previousRows reads the rows of the existing alert table by alert number.
func previousRows(body string) map[int]string {
	out := map[int]string{}
	for _, m := range alertRowRE.FindAllStringSubmatch(body, -1) {
		n, _ := strconv.Atoi(m[1])
		out[n] = m[0]
	}
	return out
}

// rewriteBody replaces the marker and the alert table in an existing body.
// It returns the body unchanged when either is missing.
func rewriteBody(body string, m Marker, section string) string {
	i, j := strings.Index(body, alertsStart), strings.Index(body, alertsEnd)
	if i < 0 || j < i || !markerRE.MatchString(body) {
		return body
	}
	body = body[:i] + section + body[j+len(alertsEnd):]
	return markerRE.ReplaceAllLiteralString(body, m.String())
}

// CloseMarker tags the closing note, so a retry after a failed close (the
// note posted, the close refused) closes without posting the note again.
const CloseMarker = "<!-- hive:code-scanning-closed -->"

// CloseComment is the note posted when every tracked alert is gone.
func CloseComment(gone map[int]AlertStatus) string {
	nums := make([]int, 0, len(gone))
	for n := range gone {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	var fixed, dismissed []string
	for _, n := range nums {
		if gone[n] == StatusDismissed {
			dismissed = append(dismissed, "#"+strconv.Itoa(n))
		} else {
			fixed = append(fixed, "#"+strconv.Itoa(n))
		}
	}
	var b strings.Builder
	b.WriteString(CloseMarker + "\n")
	b.WriteString("Every code scanning alert this issue tracked is no longer open on GitHub")
	if len(fixed) > 0 {
		b.WriteString("; fixed: " + strings.Join(fixed, ", "))
	}
	if len(dismissed) > 0 {
		b.WriteString("; dismissed by a maintainer: " + strings.Join(dismissed, ", "))
	}
	b.WriteString(". Closing. The hive never reopens this issue and never files these alert numbers again.")
	return b.String()
}
