package agent

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestCavemanPinIsNotBareSHA guards the caveman skill pin format in
// installCavemanForAgent (manager.go).
//
// The `skills` CLI (and npx's git resolution for `skills add`) clones with
// `git clone --depth 1 --branch <ref>`, and git cannot clone a bare commit
// SHA as a branch ("Could not find remote branch <sha>"). A raw-SHA pin
// therefore silently breaks every caveman install on a live hive — this
// happened with the 0d95a81d35a9 pin (fixed by re-pinning to the equivalent
// tag v1.9.1 in PR #4518). Pins must be branch or tag names.
//
// This test scans manager.go for caveman refs and fails if any ref after '#'
// is a bare hex SHA (7-40 hex chars), so the regression cannot be
// reintroduced.
func TestCavemanPinIsNotBareSHA(t *testing.T) {
	src, err := os.ReadFile("manager.go")
	if err != nil {
		t.Fatalf("reading manager.go: %v", err)
	}

	// Match both pin forms: "github:JuliusBrussee/caveman#<ref>" (npx) and
	// "JuliusBrussee/caveman#<ref>" (skills add).
	refRe := regexp.MustCompile(`(?:github:)?JuliusBrussee/caveman#([^"\s]+)`)
	bareSHARe := regexp.MustCompile(`^[0-9a-f]{7,40}$`)

	matches := refRe.FindAllStringSubmatch(string(src), -1)
	if len(matches) == 0 {
		t.Fatal("no caveman refs found in manager.go; if the pin moved, update this test to scan the new location")
	}

	for _, m := range matches {
		ref := m[1]
		if bareSHARe.MatchString(ref) {
			t.Errorf("caveman ref %q is pinned to a bare commit SHA; `git clone --branch <sha>` cannot clone it, so every install fails. Pin a tag (or branch) that resolves to the wanted commit instead — see PR #4518", m[0])
		}
	}
}

// TestCavemanSkillsAddIsHeadless guards the other silent-failure mode of the
// same install path: without the skills CLI's own `-y`, `skills add <pkg> -a
// <agent>` renders an interactive confirmation that auto-cancels in the
// hive's non-TTY captured-output context — the command exits 0 but installs
// NOTHING (npx's `-y` only confirms the npx package fetch, not the skills
// prompts). See PR #4548. Every `skills add` invocation in manager.go must
// therefore carry a `-y` argument.
func TestCavemanSkillsAddIsHeadless(t *testing.T) {
	src, err := os.ReadFile("manager.go")
	if err != nil {
		t.Fatalf("reading manager.go: %v", err)
	}

	addLineRe := regexp.MustCompile(`(?m)^.*"skills",\s*"add".*$`)
	// Only args AFTER "add" count: npx's own leading `-y` confirms the npx
	// package fetch, not the skills CLI's prompts.
	yesRe := regexp.MustCompile(`"(-y|--yes)"`)

	lines := addLineRe.FindAllString(string(src), -1)
	if len(lines) == 0 {
		t.Fatal("no `skills add` invocations found in manager.go; if the install moved, update this test to scan the new location")
	}

	for _, line := range lines {
		_, afterAdd, _ := strings.Cut(line, `"add"`)
		if !yesRe.MatchString(afterAdd) {
			t.Errorf("`skills add` invocation lacks the skills CLI's own -y/--yes flag after `add` (npx's leading -y does not count), so it auto-cancels headlessly and installs nothing (exit 0): %s — see PR #4548", strings.TrimSpace(line))
		}
	}
}
