package prosefilter

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// emitDirs are the source trees (relative to src/) whose non-test Go files
// build text that hive sends to a forge or an agent: PR, issue and comment
// templates, status lines, notices and kick prompts. Docs, tests and
// third-party text are deliberately out of scope.
var emitDirs = []string{
	"pkg/github",
	"pkg/priorart",
	"pkg/dupsweep",
	"pkg/hotspot",
	"pkg/advisory",
	"pkg/escalation",
	"pkg/prfollowup",
	"pkg/scheduler",
	"pkg/review",
	// Outbound send sites classed as templates (outbound_sites_test.go).
	"pkg/forge",
	"pkg/feedbackbundle",
	"pkg/fleetreport",
}

// TestNoNewDashesInEmittedText is a ratchet: the em-dash and en-dash count of
// each emitter file may only go down. testdata/emit_dash_baseline.json holds
// the lines that predate the rule; a file not listed there must have none.
// When you remove dashes from a listed file, lower its number (or delete the
// entry) in the same change.
func TestNoNewDashesInEmittedText(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	srcRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")

	raw, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "testdata", "emit_dash_baseline.json"))
	if err != nil {
		t.Fatalf("read baseline: %v", err)
	}
	baseline := map[string]int{}
	if err := json.Unmarshal(raw, &baseline); err != nil {
		t.Fatalf("parse baseline: %v", err)
	}

	var failures []string
	for _, dir := range emitDirs {
		root := filepath.Join(srcRoot, filepath.FromSlash(dir))
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" {
					return fs.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(srcRoot, path)
			rel = filepath.ToSlash(rel)
			if got, allowed := countDashCodeLines(string(data)), baseline[rel]; got > allowed {
				failures = append(failures, rel)
				t.Errorf("%s has %d lines with an em-dash or en-dash outside comments (allowed %d). Hive must not emit them: use a hyphen or reword the string", rel, got, allowed)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	sort.Strings(failures)
}

// countDashCodeLines counts lines containing an em-dash or en-dash that are
// not whole-line comments (// lines and /* */ blocks).
func countDashCodeLines(src string) int {
	n := 0
	inBlock := false
	for _, line := range strings.Split(src, "\n") {
		s := strings.TrimSpace(line)
		if inBlock {
			if strings.Contains(s, "*/") {
				inBlock = false
			}
			continue
		}
		if strings.HasPrefix(s, "/*") {
			if !strings.Contains(s, "*/") {
				inBlock = true
			}
			continue
		}
		if strings.HasPrefix(s, "//") {
			continue
		}
		if HasDash(line) {
			n++
		}
	}
	return n
}

func TestCountDashCodeLines(t *testing.T) {
	src := "// a " + emDash + "\n/* x " + emDash + "\n y " + emDash + " */\ns := \"a " + emDash + " b\"\nt := 1 // " + enDash + "\nu := 2\n"
	if got := countDashCodeLines(src); got != 2 {
		t.Fatalf("countDashCodeLines = %d, want 2", got)
	}
}
