package releaselines

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// repoRoot is this package's path back to the repository root: src/pkg/releaselines.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(ConfigPath))); err != nil {
		t.Fatalf("%s not found from %s: %v", ConfigPath, root, err)
	}
	return root
}

func report(t *testing.T, findings []Finding) string {
	t.Helper()
	var sb strings.Builder
	Report(&sb, findings, os.Getenv("GITHUB_ACTIONS") == "true")
	return sb.String()
}

// ── The check itself, against the real repository ────────────────────────────

// TestRepositoryIsInSync is the gate. Every hand-written branch list in CI must
// name exactly the release lines declared in .github/release-lines.yaml.
func TestRepositoryIsInSync(t *testing.T) {
	findings, err := Check(repoRoot(t))
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	// Notices are printed on every run, pass or fail: an undecided release line
	// is an open maintainer question, and staying visible is the whole point.
	t.Log("\n" + report(t, findings))
	if errs := Errors(findings); len(errs) > 0 {
		t.Fatalf("%d branch list(s) out of sync with %s", len(errs), ConfigPath)
	}
}

// TestGuardIsReachableFromEveryBranch is acceptance criterion 2: the check must
// not be subject to the bug it guards against. #4339 was a guard pinned to a
// branch name that went away; a guard against that pinned the same way would be
// #4339 one level up, so its own workflow may not carry a branch pin — or a
// path filter, which is the other way a guard scopes itself into never running.
func TestGuardIsReachableFromEveryBranch(t *testing.T) {
	const gate = "release-lines-gate.yml"
	path := filepath.Join(repoRoot(t), filepath.FromSlash(WorkflowDir), gate)

	filters, err := branchFilters(path)
	if err != nil {
		t.Fatalf("parse %s: %v", gate, err)
	}
	seen := map[string]bool{}
	for _, f := range filters {
		seen[f.path] = true
		for _, v := range f.values {
			if v != "**" {
				t.Errorf("%s %s names %q; the guard must run on every branch", gate, f.path, v)
			}
		}
	}
	for _, want := range []string{"on.push.branches", "on.pull_request.branches"} {
		if !seen[want] {
			t.Errorf("%s has no %s; expected an explicit '**'", gate, want)
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	on := mapValue(documentRoot(&doc), "on")
	for _, event := range triggerEvents {
		ev := mapValue(on, event)
		if ev == nil {
			continue
		}
		for _, key := range []string{"paths", "paths-ignore"} {
			if mapValue(ev, key) != nil {
				t.Errorf("%s on.%s.%s: the guard must have no path filter, or it reports green by never running", gate, event, key)
			}
		}
	}
}

// ── Demonstrating the failure this exists to catch ───────────────────────────

// TestCuttingANewReleaseLineFailsUntilWorkflowsCatchUp is acceptance criterion
// 1, run against the repository's REAL workflows: declare a plausible future
// release line and change nothing else, and the check must name every file that
// has not caught up.
func TestCuttingANewReleaseLineFailsUntilWorkflowsCatchUp(t *testing.T) {
	root := repoRoot(t)
	cfg, err := Load(root)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cfg.ReleaseLines = append(cfg.ReleaseLines, Line{
		Branch: "v5",
		Status: StatusCurrent,
		Note:   "hypothetical, added by this test only",
	})

	findings := CheckConfig(root, cfg)
	t.Log("\nWhat cutting v5 without touching CI looks like:\n" + report(t, findings))

	errs := Errors(findings)
	if len(errs) == 0 {
		t.Fatal("adding a release line changed nothing; the guard does not guard")
	}

	// Every pinned workflow in #4405's table, and docker.yml's push policy —
	// which is the entry the table's own "docker.yml is the outlier" note does
	// NOT cover, because it is a second hand-written list in the same file.
	want := []string{
		"v2-ci.yml", "v2-tests.yml", "scorecard.yml",
		"podman-rootful-lane.yml", "podman-rootless-lane.yml", "podman-arm64-lane.yml",
		"podman-contract.yml", "quadlet-gate.yml", "suid-contract.yml",
		"docker.yml",
	}
	for _, file := range want {
		if !mentions(errs, workflowPath(file), "v5") {
			t.Errorf("cutting v5 did not flag %s", file)
		}
	}
	// docker.yml's TRIGGER is deliberate ('**' with bot branches excluded) and
	// must stay unflagged even though its LONG_LIVED list is reported.
	for _, f := range errs {
		if f.File == workflowPath("docker.yml") && strings.HasPrefix(f.Where, "on.") {
			t.Errorf("docker.yml's deliberate '**' trigger was flagged: %s", f)
		}
	}
}

// TestRetiringAReleaseLineNamesEveryWorkflowStillPinnedToIt is the other
// direction, and the reason the pins were asserted rather than replaced by a
// `v*` glob: a glob can only ever ADD, so it has no way to notice that a line
// has been retired and should stop being built.
func TestRetiringAReleaseLineNamesEveryWorkflowStillPinnedToIt(t *testing.T) {
	root := repoRoot(t)
	cfg, err := Load(root)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	var kept []Line
	for _, l := range cfg.ReleaseLines {
		if l.Branch != "v2" {
			kept = append(kept, l)
		}
	}
	if len(kept) == len(cfg.ReleaseLines) {
		t.Skip("v2 is no longer a declared release line; the maintainer decision has been taken")
	}
	cfg.ReleaseLines = kept

	findings := CheckConfig(root, cfg)
	t.Log("\nWhat retiring v2 without touching CI looks like:\n" + report(t, findings))

	errs := Errors(findings)
	for _, file := range []string{"v2-ci.yml", "v2-tests.yml", "podman-contract.yml", "suid-contract.yml", "quadlet-gate.yml", "docker.yml"} {
		if !mentions(errs, workflowPath(file), "v2") {
			t.Errorf("retiring v2 did not flag %s, which still names it", file)
		}
	}
	// scorecard.yml declares `omits: [v2]`; once v2 is not a release line that
	// declaration is stale and must be reported too, or the escape hatches rot.
	if !mentions(errs, workflowPath("scorecard.yml"), "omits") {
		t.Error("retiring v2 left scorecard.yml's stale `omits: [v2]` declaration unreported")
	}
}

func mentions(findings []Finding, file, needle string) bool {
	for _, f := range findings {
		if f.File == file && strings.Contains(f.Message, needle) {
			return true
		}
	}
	return false
}

// ── Unit tests on synthetic fixtures ─────────────────────────────────────────

// fixture builds a throwaway repository root holding a config and workflows.
func fixture(t *testing.T, config string, workflows map[string]string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, filepath.FromSlash(WorkflowDir))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(ConfigPath)), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, body := range workflows {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

const twoLines = `
release_lines:
  - branch: v4
    status: current
  - branch: v2
    status: supported
`

func checkFixture(t *testing.T, config string, workflows map[string]string) []Finding {
	t.Helper()
	findings, err := Check(fixture(t, config, workflows))
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	return findings
}

func assertClean(t *testing.T, findings []Finding) {
	t.Helper()
	if errs := Errors(findings); len(errs) > 0 {
		t.Fatalf("expected no errors, got:\n%s", report(t, findings))
	}
}

func assertError(t *testing.T, findings []Finding, needle string) {
	t.Helper()
	for _, f := range Errors(findings) {
		if strings.Contains(f.String(), needle) {
			return
		}
	}
	t.Fatalf("no error mentioning %q; got:\n%s", needle, report(t, findings))
}

// TestBothYAMLSpellingsAreHandled is acceptance criterion 3. `branches: [v2, v4]`
// is the flow sequence v2-ci.yml uses — the highest-value entry in #4405's table
// — and a guard that only understood the block form would miss it. Parsing the
// YAML rather than the text makes the two indistinguishable, and this pins that.
func TestBothYAMLSpellingsAreHandled(t *testing.T) {
	const flow = "on:\n  push:\n    branches: [v2, v4]\n"
	const block = "on:\n  push:\n    branches:\n      - v2\n      - v4\n"
	const quotedFlow = "on:\n  push:\n    branches: [ \"v2\", \"v4\" ]\n"

	for name, body := range map[string]string{"flow": flow, "block": block, "quoted-flow": quotedFlow} {
		t.Run(name, func(t *testing.T) {
			cfg := twoLines + "\nworkflows:\n  - file: a.yml\n    pinned: true\n"
			assertClean(t, checkFixture(t, cfg, map[string]string{"a.yml": body}))

			// ... and each spelling must FAIL identically when a line is missing.
			short := strings.NewReplacer("[v2, v4]", "[v4]", "      - v2\n", "", "[ \"v2\", \"v4\" ]", "[ \"v4\" ]").Replace(body)
			assertError(t, checkFixture(t, cfg, map[string]string{"a.yml": short}), `release line "v2" is missing`)
		})
	}
}

// TestGlobTriggerIsRecognisedAsDeliberate is acceptance criterion 4: docker.yml's
// '**' trigger, bot exclusions and all, is not a stale pin and is not flagged.
func TestGlobTriggerIsRecognisedAsDeliberate(t *testing.T) {
	const dockerish = `
on:
  push:
    branches:
      - '**'
      - '!dependabot/**'
      - '!copilot/**'
  workflow_dispatch:
`
	cfg := twoLines + "\nworkflows:\n  - file: docker.yml\n    pinned: false\n    reason: builds on every branch on purpose\n"
	assertClean(t, checkFixture(t, cfg, map[string]string{"docker.yml": dockerish}))
}

func TestUnpinnedDeclarationRequiresAReason(t *testing.T) {
	const dockerish = "on:\n  push:\n    branches: ['**']\n"
	cfg := twoLines + "\nworkflows:\n  - file: docker.yml\n    pinned: false\n"
	assertError(t, checkFixture(t, cfg, map[string]string{"docker.yml": dockerish}), "no `reason`")
}

// A file declared unpinned that has since been narrowed to a literal list is
// back inside the failure mode, and saying `pinned: false` must not hide it.
func TestUnpinnedDeclarationThatBecameLiteralIsFlagged(t *testing.T) {
	cfg := twoLines + "\nworkflows:\n  - file: docker.yml\n    pinned: false\n    reason: was a glob once\n"
	body := "on:\n  push:\n    branches: [v2, v4]\n"
	assertError(t, checkFixture(t, cfg, map[string]string{"docker.yml": body}), "names only literal branches")
}

func TestPinnedDeclarationThatContainsAPatternIsFlagged(t *testing.T) {
	cfg := twoLines + "\nworkflows:\n  - file: a.yml\n    pinned: true\n"
	body := "on:\n  push:\n    branches: [v2, 'v*']\n"
	assertError(t, checkFixture(t, cfg, map[string]string{"a.yml": body}), `pattern "v*"`)
}

func TestUnknownBranchMustBeDeclaredAsExtra(t *testing.T) {
	cfg := twoLines + "\nworkflows:\n  - file: scorecard.yml\n    pinned: true\n"
	body := "on:\n  push:\n    branches: [ \"main\", \"v4\", \"v2\" ]\n"
	findings := checkFixture(t, cfg, map[string]string{"scorecard.yml": body})
	assertError(t, findings, `names "main", which is not a release line`)

	withExtra := twoLines + "\nworkflows:\n  - file: scorecard.yml\n    pinned: true\n    extra: [main]\n"
	assertClean(t, checkFixture(t, withExtra, map[string]string{"scorecard.yml": body}))
}

func TestOmittedReleaseLineMustBeDeclared(t *testing.T) {
	body := "on:\n  push:\n    branches: [v4]\n"
	assertError(t, checkFixture(t, twoLines+"\nworkflows:\n  - file: a.yml\n    pinned: true\n",
		map[string]string{"a.yml": body}), `release line "v2" is missing`)

	assertClean(t, checkFixture(t, twoLines+"\nworkflows:\n  - file: a.yml\n    pinned: true\n    omits: [v2]\n",
		map[string]string{"a.yml": body}))
}

// The escape hatches are themselves carry-forward hazards: an `omits` or `extra`
// that stops describing the file is a stale declaration, and stale declarations
// are how the original lists got here.
func TestStaleEscapeHatchesAreFlagged(t *testing.T) {
	body := "on:\n  push:\n    branches: [v2, v4]\n"
	assertError(t, checkFixture(t, twoLines+"\nworkflows:\n  - file: a.yml\n    pinned: true\n    omits: [v2]\n",
		map[string]string{"a.yml": body}), "but the filter does name v2")
	assertError(t, checkFixture(t, twoLines+"\nworkflows:\n  - file: a.yml\n    pinned: true\n    extra: [main]\n",
		map[string]string{"a.yml": body}), "but the filter does not name it")
	assertError(t, checkFixture(t, twoLines+"\nworkflows:\n  - file: a.yml\n    pinned: true\n    omits: [nope]\n",
		map[string]string{"a.yml": body}), `"nope" is not a release line`)
}

// The next instance of this bug is a workflow added later that pins branches and
// is never declared. It would be invisible to a check that only looks at what it
// was told about, so the check looks at the directory too.
func TestUndeclaredPinnedWorkflowIsFlagged(t *testing.T) {
	cfg := twoLines + "\nworkflows: []\n"
	findings := checkFixture(t, cfg, map[string]string{"newcomer.yml": "on:\n  push:\n    branches: [v4]\n"})
	assertError(t, findings, "newcomer.yml")

	// A workflow with no branch filter at all runs everywhere and needs no entry.
	assertClean(t, checkFixture(t, cfg, map[string]string{"free.yml": "on:\n  schedule:\n    - cron: '0 6 * * 1'\n"}))
	assertClean(t, checkFixture(t, cfg, map[string]string{"free.yml": "on: [push, pull_request]\n"}))
}

func TestDeclaredWorkflowThatLostItsFilterIsFlagged(t *testing.T) {
	cfg := twoLines + "\nworkflows:\n  - file: a.yml\n    pinned: true\n"
	assertError(t, checkFixture(t, cfg, map[string]string{"a.yml": "on:\n  workflow_dispatch:\n"}), "has no branch filter")
}

func TestDeclaredWorkflowThatIsGoneIsFlagged(t *testing.T) {
	cfg := twoLines + "\nworkflows:\n  - file: deleted.yml\n    pinned: true\n"
	assertError(t, checkFixture(t, cfg, nil), "does not exist")
}

func TestPullRequestTargetIsChecked(t *testing.T) {
	cfg := twoLines + "\nworkflows:\n  - file: a.yml\n    pinned: true\n"
	body := "on:\n  pull_request_target:\n    branches: [v4]\n"
	assertError(t, checkFixture(t, cfg, map[string]string{"a.yml": body}), "on.pull_request_target.branches")
}

// Every trigger block is checked independently: a workflow whose push filter has
// been carried forward but whose pull_request filter has not still leaves the
// new line ungated on PRs, which is where a gate matters most.
func TestEachTriggerBlockIsCheckedSeparately(t *testing.T) {
	cfg := twoLines + "\nworkflows:\n  - file: a.yml\n    pinned: true\n"
	body := "on:\n  push:\n    branches: [v2, v4]\n  pull_request:\n    branches: [v4]\n"
	findings := checkFixture(t, cfg, map[string]string{"a.yml": body})
	assertError(t, findings, "on.pull_request.branches")
	for _, f := range Errors(findings) {
		if f.Where == "on.push.branches" {
			t.Errorf("push filter is in sync but was flagged: %s", f)
		}
	}
}

// ── branch_lists: the same names, hand-written a second time ─────────────────

const dockerPolicy = `
on:
  push:
    branches: ['**']
jobs:
  gate:
    steps:
      - name: Decide push policy
        env:
          LONG_LIVED: "v2 v4 mk dd"
`

func TestBranchListIsCheckedAgainstReleaseLines(t *testing.T) {
	cfg := twoLines + `
workflows:
  - file: docker.yml
    pinned: false
    reason: every branch builds on purpose
branch_lists:
  - file: docker.yml
    env: LONG_LIVED
    extra: [mk, dd]
`
	assertClean(t, checkFixture(t, cfg, map[string]string{"docker.yml": dockerPolicy}))

	// A release line missing from the push policy is worse than a skipped guard:
	// the image builds and is never published, so no hive can be assigned to the
	// new line at all, and the image looks healthy the whole time.
	short := strings.Replace(dockerPolicy, `"v2 v4 mk dd"`, `"v2 v4 mk"`, 1)
	assertError(t, checkFixture(t, cfg, map[string]string{"docker.yml": short}), "declares `extra: [dd]`")

	undeclared := strings.Replace(dockerPolicy, `"v2 v4 mk dd"`, `"v2 v4 mk dd zz"`, 1)
	assertError(t, checkFixture(t, cfg, map[string]string{"docker.yml": undeclared}), `names "zz"`)

	renamed := strings.Replace(dockerPolicy, "LONG_LIVED", "LONG_LIVED_BRANCHES", 1)
	assertError(t, checkFixture(t, cfg, map[string]string{"docker.yml": renamed}), "no `env:` block sets LONG_LIVED")
}

func TestBranchListMissingAReleaseLineIsFlagged(t *testing.T) {
	cfg := twoLines + `
workflows:
  - file: docker.yml
    pinned: false
    reason: every branch builds on purpose
branch_lists:
  - file: docker.yml
    env: LONG_LIVED
    extra: [mk, dd]
`
	short := strings.Replace(dockerPolicy, `"v2 v4 mk dd"`, `"v4 mk dd"`, 1)
	assertError(t, checkFixture(t, cfg, map[string]string{"docker.yml": short}), `release line "v2" is missing from the list`)
}

// ── The source of truth is checked against itself ────────────────────────────

func TestUndecidedStatusIsANoticeNotAFailure(t *testing.T) {
	cfg := `
release_lines:
  - branch: v4
    status: current
  - branch: v2
    status: undecided
    note: >
      MAINTAINER DECISION NEEDED. Whether v2 is still supported.
workflows:
  - file: a.yml
    pinned: true
`
	findings := checkFixture(t, cfg, map[string]string{"a.yml": "on:\n  push:\n    branches: [v2, v4]\n"})
	assertClean(t, findings)
	notices := Notices(findings)
	if len(notices) != 1 || !strings.Contains(notices[0].Message, "MAINTAINER DECISION NEEDED") {
		t.Fatalf("expected one notice carrying the open question, got %v", notices)
	}
}

func TestConfigIsValidatedAgainstItself(t *testing.T) {
	cases := map[string]struct{ cfg, want string }{
		"unknown status": {"release_lines:\n  - branch: v4\n    status: probably\n", `status "probably"`},
		"duplicate line": {"release_lines:\n  - branch: v4\n    status: current\n  - branch: v4\n    status: current\n", "declared twice"},
		"empty branch":   {"release_lines:\n  - branch: \"\"\n    status: current\n", "empty branch"},
		"no lines":       {"release_lines: []\n", "no release_lines declared"},
		"duplicate file": {twoLines + "\nworkflows:\n  - file: a.yml\n    pinned: true\n  - file: a.yml\n    pinned: true\n", `workflow "a.yml" is declared twice`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			assertError(t, checkFixture(t, tc.cfg, map[string]string{"a.yml": "on:\n  push:\n    branches: [v2, v4]\n"}), tc.want)
		})
	}
}

func TestMissingConfigIsAnError(t *testing.T) {
	if _, err := Check(t.TempDir()); err == nil {
		t.Fatal("expected an error when the source of truth is absent")
	}
}

func TestReportNamesTheFilesAndSaysWhatToDo(t *testing.T) {
	findings := []Finding{
		{Severity: Notice, Message: "an open question"},
		{Severity: Error, File: workflowPath("a.yml"), Where: "on.push.branches", Message: `release line "v5" is missing`},
	}
	var sb strings.Builder
	Report(&sb, findings, true)
	out := sb.String()
	for _, want := range []string{
		"NOTICE",
		"ERROR   .github/workflows/a.yml (on.push.branches)",
		"::notice file=.github/release-lines.yaml",
		"::error file=.github/workflows/a.yml",
		"1 file(s) have fallen out of sync",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report is missing %q:\n%s", want, out)
		}
	}
	// Actions truncates an annotation at the first newline, so a folded note has
	// to arrive flattened or the useful half of it is lost.
	var multiline strings.Builder
	Report(&multiline, []Finding{{Severity: Notice, Message: "first line\nsecond line"}}, true)
	for _, line := range strings.Split(multiline.String(), "\n") {
		if strings.HasPrefix(line, "::") && !strings.Contains(line, "first line second line") {
			t.Errorf("annotation was not flattened: %q", line)
		}
	}
}

func TestReportSaysSoWhenNothingIsWrong(t *testing.T) {
	var sb strings.Builder
	Report(&sb, nil, false)
	if !strings.Contains(sb.String(), "In sync") {
		t.Errorf("expected an in-sync line, got %q", sb.String())
	}
}

// ── Malformed input is reported, not swallowed ───────────────────────────────

func TestMalformedInputIsReported(t *testing.T) {
	if _, err := Check(fixture(t, "release_lines: [oh: no\n", nil)); err == nil {
		t.Error("expected an error for unparseable " + ConfigPath)
	}

	cfg := twoLines + "\nworkflows:\n  - file: a.yml\n    pinned: true\n"
	assertError(t, checkFixture(t, cfg, map[string]string{"a.yml": "on: [oh: no\n"}), "parse")

	// A workflow file that is not a mapping at all carries no trigger block; it
	// is malformed for other reasons, and this check is not the place to say so.
	assertClean(t, checkFixture(t, twoLines+"\nworkflows: []\n", map[string]string{"a.yml": "- just\n- a list\n"}))
	assertClean(t, checkFixture(t, twoLines+"\nworkflows: []\n", map[string]string{"a.yml": ""}))
}

// GitHub wants a list, but a lone scalar is legal YAML and must not be read as
// "no filter" — that would make the workflow invisible to the check.
func TestScalarBranchFilterIsRead(t *testing.T) {
	cfg := twoLines + "\nworkflows:\n  - file: a.yml\n    pinned: true\n"
	assertError(t, checkFixture(t, cfg, map[string]string{"a.yml": "on:\n  push:\n    branches: v4\n"}), `release line "v2" is missing`)

	unpinned := twoLines + "\nworkflows:\n  - file: a.yml\n    pinned: false\n    reason: everywhere, on purpose\n"
	assertClean(t, checkFixture(t, unpinned, map[string]string{"a.yml": "on:\n  push:\n    branches: '**'\n"}))
}

func TestBranchesIgnoreIsChecked(t *testing.T) {
	cfg := twoLines + "\nworkflows:\n  - file: a.yml\n    pinned: true\n"
	assertError(t, checkFixture(t, cfg, map[string]string{"a.yml": "on:\n  push:\n    branches-ignore: [v4]\n"}),
		"on.push.branches-ignore")
}

func TestMissingWorkflowDirectoryIsReported(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".github"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(ConfigPath)), []byte(twoLines), 0o644); err != nil {
		t.Fatal(err)
	}
	findings, err := Check(root)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	assertError(t, findings, "read workflow directory")
}

func TestBranchListOnAMissingFileIsReported(t *testing.T) {
	cfg := twoLines + "\nbranch_lists:\n  - file: gone.yml\n    env: LONG_LIVED\n"
	assertError(t, checkFixture(t, cfg, nil), "does not exist")

	empty := twoLines + "\nbranch_lists:\n  - file: a.yml\n    env: LONG_LIVED\n"
	assertError(t, checkFixture(t, empty, map[string]string{"a.yml": ""}), "the file is empty")
	assertError(t, checkFixture(t, empty, map[string]string{"a.yml": "on: [oh: no\n"}), "parse")
}

func TestIsPattern(t *testing.T) {
	for _, s := range []string{"**", "v*", "!dependabot/**", "releases/**", "v[12]", "v+"} {
		if !isPattern(s) {
			t.Errorf("%q should be a pattern", s)
		}
	}
	for _, s := range []string{"v2", "v4", "main", "mk", "release-v2"} {
		if isPattern(s) {
			t.Errorf("%q should be a literal branch name", s)
		}
	}
}
