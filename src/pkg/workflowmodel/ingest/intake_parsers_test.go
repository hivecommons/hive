package ingest

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/workflowmodel"
	"github.com/hivecommons/hive/pkg/workflowmodel/hivedefault"
)

func TestIsIntakePath(t *testing.T) {
	yes := []string{
		".github/ISSUE_TEMPLATE/bug.yml", ".github/issue_template/feature.yaml", ".github/ISSUE_TEMPLATE/legacy.md",
		".github/ISSUE_TEMPLATE/config.yml", ".github/ISSUE_TEMPLATE.md", ".github/PULL_REQUEST_TEMPLATE.md",
		".github/PULL_REQUEST_TEMPLATE/feature.md", "docs/pull_request_template.md", "CONTRIBUTING.md",
		".github/CODEOWNERS", "CODEOWNERS", "OWNERS", "OWNERS_ALIASES", ".github/settings.yml",
		".github/labels.yml", ".github/labeler.yaml",
	}
	no := []string{
		".github/ISSUE_TEMPLATE/", ".github/ISSUE_TEMPLATE/sub/x.yml", ".github/ISSUE_TEMPLATE/notes.txt",
		".github/PULL_REQUEST_TEMPLATE/x.yml", "pkg/OWNERS", "README.md", ".github/workflows/ci.yml",
		"vendor/CONTRIBUTING.md", ".github/dependabot.yml",
	}
	for _, p := range yes {
		if !IsIntakePath(p) {
			t.Errorf("%s: want intake", p)
		}
	}
	for _, p := range no {
		if IsIntakePath(p) {
			t.Errorf("%s: want not intake", p)
		}
	}
}

func TestReadTreeKeepsIntakeFiles(t *testing.T) {
	root := t.TempDir()
	for rel, body := range map[string]string{
		".github/workflows/ci.yml":         "on: push\njobs: {}\n",
		".github/ISSUE_TEMPLATE/b.yml":     "name: Bug\nlabels: [bug]\n",
		".github/ISSUE_TEMPLATE/n.txt":     "no\n",
		".github/PULL_REQUEST_TEMPLATE.md": "- [ ] tests\n",
		"OWNERS":                           "approvers: [a]\n",
		"CONTRIBUTING.md":                  "triage\n",
		"README.md":                        "hi\n",
	} {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	files, err := ReadTree(root)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for p := range files {
		got = append(got, p)
	}
	sort.Strings(got)
	want := ".github/ISSUE_TEMPLATE/b.yml .github/PULL_REQUEST_TEMPLATE.md .github/workflows/ci.yml CONTRIBUTING.md OWNERS"
	if strings.Join(got, " ") != want {
		t.Fatalf("got %v", got)
	}
}

func TestParseIssueTemplate(t *testing.T) {
	cases := []struct {
		name, path, body string
		wantName         string
		wantLabels       []string
		wantLines        []int
		wantLine         int
		wantFinding      string
	}{
		{name: "form list", path: ".github/ISSUE_TEMPLATE/bug.yml", body: "name: Bug\ndescription: x\nlabels:\n  - kind/bug\n  - needs-triage\nbody: []\n", wantName: "Bug", wantLabels: []string{"kind/bug", "needs-triage"}, wantLines: []int{4, 5}, wantLine: 1},
		{name: "form flow", path: ".github/ISSUE_TEMPLATE/bug.yml", body: "description: x\nname: Bug\nlabels: [a, b]\n", wantName: "Bug", wantLabels: []string{"a", "b"}, wantLines: []int{3, 3}, wantLine: 2},
		{name: "form comma string", path: ".github/ISSUE_TEMPLATE/bug.yml", body: "name: Bug\nlabels: \"a, b ,\"\n", wantName: "Bug", wantLabels: []string{"a", "b"}, wantLines: []int{2, 2}, wantLine: 1},
		{name: "md front matter", path: ".github/ISSUE_TEMPLATE/bug.md", body: "---\nname: Bug report\nabout: x\nlabels: bug\n---\n\nbody\n", wantName: "Bug report", wantLabels: []string{"bug"}, wantLines: []int{4}, wantLine: 2},
		{name: "md no front matter", path: ".github/ISSUE_TEMPLATE/bug.md", body: "# just text\n", wantFinding: CodeTemplateUnparsable},
		{name: "md unterminated front matter", path: ".github/ISSUE_TEMPLATE/bug.md", body: "---\nname: x\n", wantFinding: CodeTemplateUnparsable},
		{name: "not a mapping", path: ".github/ISSUE_TEMPLATE/bug.yml", body: "- a\n- b\n", wantFinding: CodeTemplateUnparsable},
		{name: "bad yaml", path: ".github/ISSUE_TEMPLATE/bug.yml", body: "name: [\n", wantFinding: CodeTemplateUnparsable},
		{name: "name from file", path: ".github/ISSUE_TEMPLATE/feature-request.yml", body: "labels: [x]\n", wantName: "feature-request", wantLabels: []string{"x"}, wantLines: []int{1}, wantLine: 1},
		{name: "empty document", path: ".github/ISSUE_TEMPLATE/e.yml", body: "", wantFinding: CodeTemplateUnparsable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, f := parseIssueTemplate(tc.path, []byte(tc.body))
			if tc.wantFinding != "" {
				if f == nil || f.Code != tc.wantFinding {
					t.Fatalf("finding %+v", f)
				}
				return
			}
			if f != nil {
				t.Fatalf("unexpected finding %+v", f)
			}
			if got.Name != tc.wantName || !reflect.DeepEqual(got.Labels, tc.wantLabels) || !reflect.DeepEqual(got.LabelLines, tc.wantLines) || got.Line != tc.wantLine {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestParseTemplateConfig(t *testing.T) {
	if v := parseTemplateConfig([]byte("blank_issues_enabled: false\n")); v == nil || *v {
		t.Fatalf("got %v", v)
	}
	if v := parseTemplateConfig([]byte("blank_issues_enabled: true\n")); v == nil || !*v {
		t.Fatalf("got %v", v)
	}
	if v := parseTemplateConfig([]byte("contact_links: []\n")); v != nil {
		t.Fatalf("got %v", *v)
	}
	if v := parseTemplateConfig([]byte(":[\n")); v != nil {
		t.Fatalf("got %v", *v)
	}
}

func TestParseChecklist(t *testing.T) {
	body := "## Checklist\n- [ ] Tests added\n* [x] Signed the CLA\n  - [ ] Nested: " + strings.Repeat("very long text ", 10) + "\n- not a box\n- [ ] Tests added\n"
	got := parseChecklist("PR.md", []byte(body))
	if len(got) != 3 {
		t.Fatalf("got %d checks: %+v", len(got), got)
	}
	if got[0].ID != "pr-template-tests-added" || got[0].Kind != workflowmodel.CheckManual || got[0].Source != "PR.md:2" || got[0].Name != "Tests added" {
		t.Fatalf("got %+v", got[0])
	}
	if got[1].Source != "PR.md:3" || got[1].Name != "Signed the CLA" {
		t.Fatalf("got %+v", got[1])
	}
	if len(got[2].ID) > len("pr-template-")+maxCheckSlug {
		t.Fatalf("id not truncated: %s", got[2].ID)
	}
}

func TestPlanIntake(t *testing.T) {
	triage := triageLabels{needs: "needs-triage"}
	tpl := func(name string, labels ...string) issueTemplate {
		return issueTemplate{Path: name + ".yml", Line: 1, Name: name, Labels: labels, LabelLines: make([]int, len(labels))}
	}
	states, findings := planIntake([]issueTemplate{
		tpl("Bug", "kind/bug", "needs-triage"),
		tpl("Bug again", "kind/bug", "needs-triage"), // same label set: cited on the first
		tpl("Crash", "kind/bug"),                     // only shared labels: merged with a finding
		tpl("Triage only", "needs-triage"),           // nothing to bind
		tpl("Support"),                               // no labels
		tpl("Bug", "kind/other"),                     // same name, other label: id suffixed
	}, triage)
	var ids, binds []string
	for _, s := range states {
		ids = append(ids, s.id)
		binds = append(binds, s.bind)
	}
	if strings.Join(ids, " ") != "requested-bug requested-bug-2" || strings.Join(binds, " ") != "kind/bug kind/other" {
		t.Fatalf("states %v %v", ids, binds)
	}
	if len(states[0].citations) != 3 {
		t.Fatalf("first state citations %d", len(states[0].citations))
	}
	var codes []string
	for _, f := range findings {
		codes = append(codes, f.Code)
	}
	if strings.Join(codes, " ") != "issue-template-merged issue-template-no-labels issue-template-no-labels" {
		t.Fatalf("findings %v", codes)
	}
}

func TestLabelParsers(t *testing.T) {
	cases := []struct {
		name  string
		parse func(string, []byte, *labelIndex)
		body  string
		want  []string // name@line
	}{
		{"settings", parseSettingsLabels, "labels:\n  - name: needs-triage\n    color: ff0000\n    description: New\n  - name: bug\n", []string{"bug@5", "needs-triage@2"}},
		{"settings bad", parseSettingsLabels, "labels: [\n", nil},
		{"labels sequence", parseLabelsFile, "- name: a\n- name: b\n  color: fff\n- plain\n", []string{"a@1", "b@2", "plain@4"}},
		{"labels wrapped", parseLabelsFile, "labels:\n  - name: a\n", []string{"a@2"}},
		{"labels mapping", parseLabelsFile, "a:\n  color: fff\n  description: d\nb: fff\n", []string{"a@1", "b@4"}},
		{"labels empty", parseLabelsFile, "", nil},
		{"labels scalar", parseLabelsFile, "just a string\n", nil},
		{"labeler", parseLabelerFile, "area/docs:\n  - docs/**\nkind/bug:\n  - '*'\n", []string{"area/docs@1", "kind/bug@3"}},
		{"labeler sequence", parseLabelerFile, "- x\n", nil},
		{"contributing backticks", parseContributing, "Add `needs-triage` then `triage/accepted`; `unrelated` is ignored.\n", []string{"needs-triage@1", "triage/accepted@1"}},
		{"contributing plain", parseContributing, "Issues are marked needs-triage until a maintainer looks.\nnot-needs-triage-ish\n", []string{"needs-triage@1"}},
		{"contributing plain words not matched", parseContributing, "We offer help wanted tasks.\n", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			x := newLabelIndex()
			tc.parse("f", []byte(tc.body), x)
			var got []string
			for _, l := range x.byName {
				got = append(got, l.Name+"@"+itoa(l.Cite.Line))
			}
			sort.Strings(got)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestLabelIndexFirstCitationWins(t *testing.T) {
	x := newLabelIndex()
	x.add("Needs-Triage", "", "", cite("a", 1, ""))
	x.add("needs-triage", "desc", "fff", cite("b", 2, ""))
	x.add("  ", "", "", cite("c", 3, ""))
	l, ok := x.get("needs-triage")
	if !ok || l.Cite.Path != "a" || l.Description != "desc" || l.Color != "fff" || l.Name != "Needs-Triage" {
		t.Fatalf("got %+v %v", l, ok)
	}
	if _, ok := x.get("nope"); ok {
		t.Fatal("unexpected hit")
	}
	tr := x.triage()
	if tr.needs != "Needs-Triage" || tr.accepted != "" || !tr.is("needs-triage") || tr.is("bug") {
		t.Fatalf("triage %+v", tr)
	}
}

func TestOwnersParsers(t *testing.T) {
	var o owners
	o.addCodeowners(".github/CODEOWNERS", []byte("# comment\n\n* @org/team @alice\ndocs/ @bob # trailing\n"))
	o.addOwners("OWNERS", []byte("approvers:\n  - carol\n  - sig-leads\nreviewers:\n  - dave\nfilters:\n  \".*\":\n    approvers: [erin]\n  \"\\\\.md$\":\n    reviewers: [ignored]\n"))
	o.addAliases("OWNERS_ALIASES", []byte("aliases:\n  sig-leads:\n    - frank\n    - grace\n"))
	o.resolve()
	if !o.prow {
		t.Fatal("prow not detected")
	}
	if got := strings.Join(members(o.approvers), ","); got != "alice,bob,carol,erin,frank,grace,org/team" {
		t.Fatalf("approvers %s", got)
	}
	if got := strings.Join(members(o.reviewers), ","); got != "dave" {
		t.Fatalf("reviewers %s", got)
	}
	var paths []string
	for _, c := range o.cites {
		paths = append(paths, c.Path+":"+itoa(c.Line))
	}
	if strings.Join(paths, " ") != ".github/CODEOWNERS:3 OWNERS:1 OWNERS_ALIASES:1" {
		t.Fatalf("cites %v", paths)
	}

	var bad owners
	bad.addOwners("OWNERS", []byte(":[\n"))
	bad.addAliases("OWNERS_ALIASES", []byte(":[\n"))
	bad.addCodeowners("CODEOWNERS", []byte("# only comments\n"))
	bad.resolve()
	if !bad.empty() || bad.prow {
		t.Fatalf("bad owners %+v", bad)
	}
	if keyLine([]byte("a: 1\nb: 2\n"), "zzz") != 1 || keyLine([]byte("a: 1\nb: 2\n"), "b") != 2 {
		t.Fatal("keyLine")
	}
}

func TestBranchRulesFromSettings(t *testing.T) {
	body := `repository:
  default_branch: main
branches:
  - name: release
    protection:
      enforce_admins: true
  - name: main
    protection:
      required_status_checks:
        contexts: [build, lint]
      required_pull_request_reviews:
        required_approving_review_count: 1
        require_code_owner_reviews: true
        dismiss_stale_reviews: true
      enforce_admins: false
      required_linear_history: true
`
	r := branchRulesFromSettings(".github/settings.yml", []byte(body))
	if r == nil || r.Branch != "main" || r.Source != BranchRulesSettings || r.Endpoint != ".github/settings.yml:3" || !reflect.DeepEqual(r.RequiredChecks, []string{"build", "lint"}) || r.RequiredApprovals != 1 || !r.RequireCodeOwnerReview || !r.DismissStaleReviews || r.EnforceAdmins || !r.LinearHistory {
		t.Fatalf("got %+v", r)
	}
	if branchRulesFromSettings("s", []byte("branches:\n  - name: main\n    protection: {}\n")) != nil {
		t.Fatal("empty protection should be nil")
	}
	if branchRulesFromSettings("s", []byte("branches:\n  - name: main\n")) != nil {
		t.Fatal("no protection should be nil")
	}
	if branchRulesFromSettings("s", []byte(":[\n")) != nil {
		t.Fatal("bad yaml should be nil")
	}
	if r := branchRulesFromSettings("s", []byte("branches:\n  - name: default\n    protection:\n      enforce_admins: true\n")); r == nil || r.Branch != "default" {
		t.Fatalf("default branch alias: %+v", r)
	}
}

func TestBranchRulesFromFiles(t *testing.T) {
	if BranchRulesFromFiles(nil) != nil {
		t.Fatal("nil files")
	}
	if BranchRulesFromFiles(map[string][]byte{BranchRulesPath: []byte("{")}) != nil {
		t.Fatal("bad json")
	}
	r := BranchRulesFromFiles(map[string][]byte{BranchRulesPath: []byte(`{"branch":"main","source":"protection","endpoint":"e","requiredApprovals":2}`)})
	if r == nil || r.RequiredApprovals != 2 || r.Empty() {
		t.Fatalf("got %+v", r)
	}
	var empty *BranchRules
	if !empty.Empty() || !(&BranchRules{Branch: "main"}).Empty() {
		t.Fatal("Empty")
	}
}

// TestIntakeSettingsRulesFallback: with no API rules, settings.yml branch
// protection drives the merge gate; API rules win when present.
func TestIntakeSettingsRulesFallback(t *testing.T) {
	files := map[string][]byte{
		".github/settings.yml": []byte("branches:\n  - name: main\n    protection:\n      required_status_checks:\n        contexts: [build]\n      required_pull_request_reviews:\n        required_approving_review_count: 1\n"),
	}
	m := Lifecycle(scopeFor("x/y"), files)
	if _, diags, err := workflowmodel.Compile(m); err != nil {
		t.Fatalf("%v %v", err, diags)
	}
	if len(m.Spec.Checks) != 1 || m.Spec.Checks[0].Source != ".github/settings.yml:1" || m.Spec.Checks[0].Kind != workflowmodel.CheckCI {
		t.Fatalf("checks %+v", m.Spec.Checks)
	}
	gate := findGate(m, GateReview)
	if gate == nil || gate.MinApprovals != 1 || gate.ReleasedBy != "maintainer" {
		t.Fatalf("gate %+v", gate)
	}
	api := &BranchRules{Branch: "main", Source: BranchRulesProtection, Endpoint: "repos/x/y/branches/main/protection", RequiredApprovals: 3, RequireCodeOwnerReview: true}
	m = LifecycleOptions(scopeFor("x/y"), files, Options{BranchRules: api})
	if gate = findGate(m, GateReview); gate == nil || gate.MinApprovals != 3 || len(m.Spec.Checks) != 0 {
		t.Fatalf("api rules did not win: %+v %+v", gate, m.Spec.Checks)
	}
	if g := findGate(m, GateCodeownerReview); g == nil || len(g.Citations) != 1 || g.Citations[0].Path != "api:repos/x/y/branches/main/protection" {
		t.Fatalf("codeowner gate %+v", g)
	}
	if m.Spec.Meta.Source != "" {
		t.Fatalf("source %q should stay as pipelines report when no templates", m.Spec.Meta.Source)
	}
}

func findGate(m *workflowmodel.Model, id string) *workflowmodel.Gate {
	for i := range m.Spec.Gates {
		if m.Spec.Gates[i].ID == id {
			return &m.Spec.Gates[i]
		}
	}
	return nil
}

// TestIntakeTriageWithoutTemplates: a triage label alone (labels.yml)
// proposes the triage state; templates alone without a triage label route
// intake straight to actionable.
func TestIntakeTriageWithoutTemplates(t *testing.T) {
	m := Lifecycle(scopeFor("x/y"), map[string][]byte{".github/labels.yml": []byte("- name: needs-triage\n- name: triage/accepted\n- name: triage/declined\n")})
	if _, diags, err := workflowmodel.Compile(m); err != nil {
		t.Fatalf("%v %v", err, diags)
	}
	s := shape(m)
	if !reflect.DeepEqual(s.states, []string{StateTriage}) || s.triageEdges != 1 || len(s.events) != 3 {
		t.Fatalf("shape %+v", s)
	}
	if m.Spec.Meta.Source != "" {
		t.Fatalf("source %q", m.Spec.Meta.Source)
	}

	m = Lifecycle(scopeFor("x/y"), map[string][]byte{".github/ISSUE_TEMPLATE/bug.yml": []byte("name: Bug\nlabels: [bug]\n")})
	if _, diags, err := workflowmodel.Compile(m); err != nil {
		t.Fatalf("%v %v", err, diags)
	}
	var toActionable bool
	for _, tr := range m.Spec.Transitions {
		if tr.From == "requested-bug" && tr.To == hivedefault.StateID(stageActionable) {
			toActionable = true
		}
	}
	if !toActionable {
		t.Fatal("requested-bug must reach actionable when no triage label exists")
	}
	if m.Spec.Meta.Source != workflowmodel.SourceIssueTemplates {
		t.Fatalf("source %q", m.Spec.Meta.Source)
	}
}

func TestTruncate(t *testing.T) {
	if truncate("abc", 5) != "abc" || truncate("abcdef", 3) != "abc" {
		t.Fatal("truncate")
	}
}

func TestIntakeOversizedFileSkipped(t *testing.T) {
	big := []byte("name: Big\nlabels: [x]\n" + strings.Repeat("#", MaxFileBytes))
	m := Lifecycle(scopeFor("x/y"), map[string][]byte{".github/ISSUE_TEMPLATE/big.yml": big})
	if len(shape(m).states) != 0 {
		t.Fatal("oversized template must be skipped")
	}
}
