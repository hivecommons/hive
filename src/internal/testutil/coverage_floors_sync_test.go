package testutil

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The per-package coverage gate is applied twice: by v2-tests.yml on every
// pull request and by coverage-hourly.yml on its schedule. Both carry their
// own copy of the gate's inputs (the default THRESHOLD, the PKG_THRESHOLD
// floor map and the UNTESTED_CMD_MAINS exemption list) with a "KEEP IN SYNC"
// comment and nothing enforcing it. A floor lowered, raised or added in one
// file but not the other makes the PR gate and the hourly gate disagree: a
// change merges green and the hourly run turns red (or the reverse, hiding a
// regression until the next PR touches the package). This test fails in the
// PR that introduces the drift.

// coverageGateWorkflows are the workflow files that each carry a copy of the
// gate's inputs, relative to this package's directory (src/internal/testutil).
var coverageGateWorkflows = []string{
	filepath.Join("..", "..", "..", ".github", "workflows", "v2-tests.yml"),
	filepath.Join("..", "..", "..", ".github", "workflows", "coverage-hourly.yml"),
}

// coverageGateInputs is one workflow's copy of the gate's inputs.
type coverageGateInputs struct {
	threshold        string
	pkgThreshold     map[string]int
	untestedCmdMains string
}

var (
	pkgThresholdEntryRE = regexp.MustCompile(`^\[([^\]\s]+)\]=(\d+)\s*(#.*)?$`)
	thresholdLineRE     = regexp.MustCompile(`^THRESHOLD=(\S+)\s*(#.*)?$`)
	untestedMainsLineRE = regexp.MustCompile(`^UNTESTED_CMD_MAINS=(.*?)\s*$`)
)

// TestCoverageFloorsInSync fails when the two workflows that apply the
// per-package coverage gate disagree on any of its inputs.
func TestCoverageFloorsInSync(t *testing.T) {
	inputs := make([]coverageGateInputs, len(coverageGateWorkflows))
	for i, path := range coverageGateWorkflows {
		in, err := parseCoverageGateInputs(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if len(in.pkgThreshold) == 0 {
			t.Fatalf("%s: PKG_THRESHOLD map is empty — the gate changed shape; update this test's parser", path)
		}
		inputs[i] = in
	}

	ref, refPath := inputs[0], coverageGateWorkflows[0]
	for i := 1; i < len(inputs); i++ {
		path := coverageGateWorkflows[i]
		if diff := diffCoverageGateInputs(ref, inputs[i]); diff != "" {
			t.Errorf("coverage gate inputs differ between %s and %s (KEEP IN SYNC):\n%s",
				filepath.Base(refPath), filepath.Base(path), diff)
		}
	}
}

// parseCoverageGateInputs reads one workflow's THRESHOLD, PKG_THRESHOLD and
// UNTESTED_CMD_MAINS assignments. Each must appear exactly once; a duplicate
// PKG_THRESHOLD key is an error because bash silently keeps the last one.
func parseCoverageGateInputs(path string) (coverageGateInputs, error) {
	f, err := os.Open(path)
	if err != nil {
		return coverageGateInputs{}, err
	}
	defer f.Close()

	in := coverageGateInputs{pkgThreshold: map[string]int{}}
	var (
		inMap                  bool
		sawThreshold, sawMains bool
		sawMap, closedMap      bool
		lineNo                 int
	)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		switch {
		case inMap:
			if line == ")" {
				inMap, closedMap = false, true
				continue
			}
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			m := pkgThresholdEntryRE.FindStringSubmatch(line)
			if m == nil {
				return in, fmt.Errorf("line %d: unparseable PKG_THRESHOLD entry %q", lineNo, line)
			}
			if _, dup := in.pkgThreshold[m[1]]; dup {
				return in, fmt.Errorf("line %d: duplicate PKG_THRESHOLD key %q", lineNo, m[1])
			}
			n, err := strconv.Atoi(m[2])
			if err != nil {
				return in, fmt.Errorf("line %d: %v", lineNo, err)
			}
			in.pkgThreshold[m[1]] = n
		case line == "declare -A PKG_THRESHOLD=(":
			if sawMap {
				return in, fmt.Errorf("line %d: second PKG_THRESHOLD map", lineNo)
			}
			sawMap, inMap = true, true
		case thresholdLineRE.MatchString(line):
			if sawThreshold {
				return in, fmt.Errorf("line %d: second THRESHOLD assignment", lineNo)
			}
			sawThreshold = true
			in.threshold = thresholdLineRE.FindStringSubmatch(line)[1]
		case untestedMainsLineRE.MatchString(line):
			if sawMains {
				return in, fmt.Errorf("line %d: second UNTESTED_CMD_MAINS assignment", lineNo)
			}
			sawMains = true
			in.untestedCmdMains = untestedMainsLineRE.FindStringSubmatch(line)[1]
		}
	}
	if err := sc.Err(); err != nil {
		return in, err
	}
	if !sawThreshold {
		return in, fmt.Errorf("no THRESHOLD= assignment found")
	}
	if !sawMap {
		return in, fmt.Errorf("no 'declare -A PKG_THRESHOLD=(' found")
	}
	if !closedMap {
		return in, fmt.Errorf("PKG_THRESHOLD map is never closed")
	}
	if !sawMains {
		return in, fmt.Errorf("no UNTESTED_CMD_MAINS= assignment found")
	}
	return in, nil
}

// diffCoverageGateInputs returns one line per disagreement, or "" when the
// two copies match.
func diffCoverageGateInputs(a, b coverageGateInputs) string {
	var out []string
	if a.threshold != b.threshold {
		out = append(out, fmt.Sprintf("  THRESHOLD: %s vs %s", a.threshold, b.threshold))
	}
	if a.untestedCmdMains != b.untestedCmdMains {
		out = append(out, fmt.Sprintf("  UNTESTED_CMD_MAINS: %s vs %s", a.untestedCmdMains, b.untestedCmdMains))
	}
	keys := map[string]bool{}
	for k := range a.pkgThreshold {
		keys[k] = true
	}
	for k := range b.pkgThreshold {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	for _, k := range sorted {
		av, aok := a.pkgThreshold[k]
		bv, bok := b.pkgThreshold[k]
		switch {
		case !aok:
			out = append(out, fmt.Sprintf("  PKG_THRESHOLD[%s]: missing vs %d", k, bv))
		case !bok:
			out = append(out, fmt.Sprintf("  PKG_THRESHOLD[%s]: %d vs missing", k, av))
		case av != bv:
			out = append(out, fmt.Sprintf("  PKG_THRESHOLD[%s]: %d vs %d", k, av, bv))
		}
	}
	return strings.Join(out, "\n")
}

// TestParseCoverageGateInputs pins the parser against the shapes the
// workflows actually use (indented entries, trailing comments, blank and
// comment lines inside the map) and the malformed shapes it must refuse.
func TestParseCoverageGateInputs(t *testing.T) {
	write := func(t *testing.T, body string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "wf.yml")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	good := `
      run: |
          THRESHOLD=90
          # KEEP IN SYNC
          declare -A PKG_THRESHOLD=(
            [dashboard]=84   # residue is the tmux watcher
            [crux/agent]=89

            # ── a section heading ──
            [cmd/crux]=75     # main() is uncovered; floor_key maps ./cmd/crux here
          )
          UNTESTED_CMD_MAINS=''
`
	in, err := parseCoverageGateInputs(write(t, good))
	if err != nil {
		t.Fatalf("good input: %v", err)
	}
	if in.threshold != "90" {
		t.Errorf("threshold = %q, want 90", in.threshold)
	}
	if in.untestedCmdMains != "''" {
		t.Errorf("untestedCmdMains = %q, want ''", in.untestedCmdMains)
	}
	want := map[string]int{"dashboard": 84, "crux/agent": 89, "cmd/crux": 75}
	if len(in.pkgThreshold) != len(want) {
		t.Fatalf("pkgThreshold = %v, want %v", in.pkgThreshold, want)
	}
	for k, v := range want {
		if in.pkgThreshold[k] != v {
			t.Errorf("pkgThreshold[%s] = %d, want %d", k, in.pkgThreshold[k], v)
		}
	}

	bad := map[string]string{
		"duplicate key": strings.Replace(good, "[crux/agent]=89", "[crux/agent]=89\n            [dashboard]=85", 1),
		"garbage entry": strings.Replace(good, "[crux/agent]=89", "crux/agent=89", 1),
		"unclosed map":  strings.Replace(good, "          )\n", "", 1),
		"no threshold":  strings.Replace(good, "THRESHOLD=90", "", 1),
		"no mains":      strings.Replace(good, "UNTESTED_CMD_MAINS=''", "", 1),
		"no map":        strings.Replace(good, "declare -A PKG_THRESHOLD=(", "declare -A OTHER=(", 1),
	}
	for name, body := range bad {
		if _, err := parseCoverageGateInputs(write(t, body)); err == nil {
			t.Errorf("%s: parser accepted malformed input", name)
		}
	}

	if _, err := parseCoverageGateInputs(filepath.Join(t.TempDir(), "missing.yml")); err == nil {
		t.Error("missing file: want error")
	}
}

// TestDiffCoverageGateInputs pins the report shape so a drift names the key.
func TestDiffCoverageGateInputs(t *testing.T) {
	a := coverageGateInputs{threshold: "90", untestedCmdMains: "''",
		pkgThreshold: map[string]int{"hub": 87, "crux/agent": 89, "only-a": 1}}
	b := coverageGateInputs{threshold: "85", untestedCmdMains: "'cmd/x'",
		pkgThreshold: map[string]int{"hub": 87, "crux/agent": 90, "only-b": 2}}

	if d := diffCoverageGateInputs(a, a); d != "" {
		t.Errorf("identical inputs: want no diff, got\n%s", d)
	}
	d := diffCoverageGateInputs(a, b)
	for _, want := range []string{
		"THRESHOLD: 90 vs 85",
		"UNTESTED_CMD_MAINS: '' vs 'cmd/x'",
		"PKG_THRESHOLD[crux/agent]: 89 vs 90",
		"PKG_THRESHOLD[only-a]: 1 vs missing",
		"PKG_THRESHOLD[only-b]: missing vs 2",
	} {
		if !strings.Contains(d, want) {
			t.Errorf("diff missing %q:\n%s", want, d)
		}
	}
	if strings.Contains(d, "[hub]") {
		t.Errorf("diff reports an equal key:\n%s", d)
	}
}
