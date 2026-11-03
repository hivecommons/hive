package writeback_test

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Ratchet over GitHub write calls that bypass the write-back gate.
//
// Tier 1 — raw go-github write calls (c.client.Issues.CreateComment, …).
// Each is a hard bypass of the Client wrappers and so of any gate the
// wrappers consult. Tier 2 — Client wrapper write calls (CreateIssueComment,
// AddLabels, …) outside pkg/writeback and the wrappers themselves: every one
// is a path that must either be solicited or consult writeback.Gate.Allowed
// before it. Both are baselined in testdata/direct-writes-baseline.txt; a
// file whose count grows, or a new file, fails here. Lower a baseline line
// when you migrate a call to the gate — never raise one.
//
// Regenerate the baseline (after a deliberate migration) with:
//
//	WRITEBACK_RATCHET_UPDATE=1 go test ./pkg/writeback -run TestDirectWriteRatchet

var (
	rawWriteRe  = regexp.MustCompile(`\.(Issues\.(CreateComment|AddLabelsToIssue|RemoveLabelForIssue|RemoveLabelsForIssue|ReplaceLabelsForIssue|Create|Edit|EditComment|DeleteComment|AddAssignees|Lock)|PullRequests\.(CreateReview|Edit|RequestReviewers|CreateComment|UpdateBranch|Merge)|Repositories\.CreateStatus|Checks\.(CreateCheckRun|UpdateCheckRun))\(`)
	wrapWriteRe = regexp.MustCompile(`\.(CreateIssueComment|AddLabels|RemoveLabel|CreateIssue|CreateReview|SubmitReview|AddLabelsFresh|RemoveLabelsFresh|CloseIssue|ClosePR|EditIssueBody|UpdateBranchExpectedHead)\(`)
)

func srcRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

// countWrites walks src/{pkg,cmd,internal} non-test .go files and counts
// matches per tier per file (paths relative to src/).
func countWrites(t *testing.T, root string) map[string]map[string]int {
	t.Helper()
	out := map[string]map[string]int{"raw": {}, "wrap": {}}
	for _, dir := range []string{"pkg", "cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			if strings.HasPrefix(rel, "pkg/writeback/") {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			raw, wrap := countLines(string(data))
			if raw > 0 {
				out["raw"][rel] = raw
			}
			// The wrappers themselves live in client.go; their bodies are
			// the choke point, not a bypass.
			if rel != "pkg/github/client.go" && wrap > 0 {
				out["wrap"][rel] = wrap
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	return out
}

// countLines counts matches per tier, skipping lines a reviewer annotated
// as going through the gate (`// writeback:gated`) or as solicited by a
// human/agent on that item (`// writeback:solicited <why>`). The annotation
// is how a migrated call leaves the baseline.
func countLines(src string) (raw, wrap int) {
	for _, line := range strings.Split(src, "\n") {
		if strings.Contains(line, "writeback:gated") || strings.Contains(line, "writeback:solicited") {
			continue
		}
		raw += len(rawWriteRe.FindAllStringIndex(line, -1))
		wrap += len(wrapWriteRe.FindAllStringIndex(line, -1))
	}
	return raw, wrap
}

func readBaseline(t *testing.T, path string) map[string]map[string]int {
	t.Helper()
	out := map[string]map[string]int{"raw": {}, "wrap": {}}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			t.Fatalf("baseline: bad line %q", line)
		}
		n, err := strconv.Atoi(fields[2])
		if err != nil {
			t.Fatalf("baseline: bad count in %q", line)
		}
		out[fields[0]][fields[1]] = n
	}
	return out
}

func writeBaseline(t *testing.T, path string, counts map[string]map[string]int) {
	t.Helper()
	var b strings.Builder
	b.WriteString("# GitHub write calls that do not go through pkg/writeback (ratchet; see ratchet_test.go).\n")
	b.WriteString("# tier path count — lower a line when you migrate a path to the gate; never raise one.\n")
	for _, tier := range []string{"raw", "wrap"} {
		files := make([]string, 0, len(counts[tier]))
		for f := range counts[tier] {
			files = append(files, f)
		}
		sort.Strings(files)
		for _, f := range files {
			fmt.Fprintf(&b, "%s %s %d\n", tier, f, counts[tier][f])
		}
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDirectWriteRatchet(t *testing.T) {
	root := srcRoot(t)
	baselinePath := filepath.Join(root, "pkg", "writeback", "testdata", "direct-writes-baseline.txt")
	counts := countWrites(t, root)
	if os.Getenv("WRITEBACK_RATCHET_UPDATE") == "1" {
		writeBaseline(t, baselinePath, counts)
		t.Logf("baseline rewritten at %s", baselinePath)
		return
	}
	baseline := readBaseline(t, baselinePath)
	var failures, improvements []string
	for _, tier := range []string{"raw", "wrap"} {
		for file, n := range counts[tier] {
			base, ok := baseline[tier][file]
			switch {
			case !ok:
				failures = append(failures, fmt.Sprintf("%s %s: %d direct write call(s) in a file not in the baseline — route them through writeback.Gate.Allowed (or a solicited relay) instead", tier, file, n))
			case n > base:
				failures = append(failures, fmt.Sprintf("%s %s: %d direct write call(s), baseline %d — new unsolicited GitHub writes must consult writeback.Gate.Allowed", tier, file, n, base))
			case n < base:
				improvements = append(improvements, fmt.Sprintf("%s %s %d", tier, file, n))
			}
		}
		for file, base := range baseline[tier] {
			if _, ok := counts[tier][file]; !ok && base > 0 {
				improvements = append(improvements, fmt.Sprintf("%s %s 0", tier, file))
			}
		}
	}
	sort.Strings(failures)
	sort.Strings(improvements)
	for _, f := range failures {
		t.Error(f)
	}
	if len(improvements) > 0 {
		t.Logf("ratchet can be tightened (set WRITEBACK_RATCHET_UPDATE=1 to rewrite the baseline):\n  %s", strings.Join(improvements, "\n  "))
	}
}
