package ingest

import (
	"path"
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/workflowmodel"
	"github.com/hivecommons/hive/pkg/workflowmodel/hivedefault"
)

// This file is the RFC phase 5 half of the ingest (hivecommons/hive#17620):
// a repository's issue templates, triage labels, pull-request template,
// CODEOWNERS/OWNERS and branch rules become intake and triage states,
// roles, gates and checks on top of the hive-default lifecycle. Every
// element produced here is origin ingested, confidence inferred, and
// cites the file:line or API path it was read from. The heuristics are
// table-driven and prefer precision: a convention the tables do not name
// yields nothing rather than a guess.

// Column, state, event and gate ids the intake half adds.
const (
	ColumnIntake = "intake"
	ColumnTriage = "triage"
	StateTriage  = "triage"
	// StateRequestedPrefix prefixes one intake state per issue-template
	// label set: requested-<slug of the template name>.
	StateRequestedPrefix = "requested-"
	// EventTemplatePrefix prefixes the event that moves a new issue into
	// its intake state: issue-opened-with-template:<slug>.
	EventTemplatePrefix  = "issue-opened-with-template:"
	EventTriageRequested = "triage-requested"
	EventTriageAccepted  = "triage-accepted"
	EventTriageDeclined  = "triage-declined"
	GateTriageDecision   = "triage-decision"
	GateReview           = "review"
	GateCodeownerReview  = "codeowner-review"
	GuardRequiredChecks  = "required-checks"
	RoleApprover         = "approver"
	RoleReviewer         = "reviewer"
)

// Findings the intake half reports on coverage.
const (
	CodeTemplateNoLabels   = "issue-template-no-labels"
	CodeTemplateUnparsable = "issue-template-unparsable"
	CodeTemplateMerged     = "issue-template-merged"
)

// AnnotationBlankIssues records .github/ISSUE_TEMPLATE/config.yml's
// blank_issues_enabled when it is false: every issue arrives through a
// template, so the intake states cover all intake.
const AnnotationBlankIssues = "workflow.hivecommons.dev/blank-issues"

// Options are the inputs to LifecycleOptions beyond the tree.
type Options struct {
	// BranchRules are the default branch's protection or ruleset settings,
	// read from the forge API; nil when the ingest had no client or the
	// branch is unprotected. Lifecycle also accepts them as the synthetic
	// file BranchRulesPath in the tree.
	BranchRules *BranchRules
}

// intakeDir is where GitHub reads issue forms and templates from.
const intakeDir = ".github/ISSUE_TEMPLATE/"

// intakeFiles are the exact (case-insensitive) repo-relative paths the
// intake half reads besides the issue-template directory.
var intakeFiles = map[string]bool{
	".github/issue_template.md":          true,
	"issue_template.md":                  true,
	".github/pull_request_template.md":   true,
	"pull_request_template.md":           true,
	"docs/pull_request_template.md":      true,
	"contributing.md":                    true,
	".github/contributing.md":            true,
	"docs/contributing.md":               true,
	"codeowners":                         true,
	".github/codeowners":                 true,
	"docs/codeowners":                    true,
	"owners":                             true,
	"owners_aliases":                     true,
	"maintainers":                        true,
	"maintainers.md":                     true,
	".github/maintainers.md":             true,
	"docs/maintainers.md":                true,
	".github/settings.yml":               true,
	".github/labels.yml":                 true,
	".github/labels.yaml":                true,
	".github/labeler.yml":                true,
	".github/labeler.yaml":               true,
	".github/issue_template/config.yml":  true,
	".github/issue_template/config.yaml": true,
}

// IsIntakePath reports whether rel (repo-relative slash path) is one of
// the process-convention files the intake half reads: issue and
// pull-request templates, CONTRIBUTING, CODEOWNERS/OWNERS and label-sync
// or settings files. ReadTree keeps these alongside the CI definitions.
func IsIntakePath(rel string) bool {
	l := strings.ToLower(rel)
	if intakeFiles[l] {
		return true
	}
	if rest, ok := strings.CutPrefix(l, strings.ToLower(intakeDir)); ok && rest != "" && !strings.Contains(rest, "/") {
		return isYAMLPath(rest) || path.Ext(rest) == ".md"
	}
	if rest, ok := strings.CutPrefix(l, ".github/pull_request_template/"); ok && rest != "" && !strings.Contains(rest, "/") {
		return path.Ext(rest) == ".md"
	}
	return false
}

// sortedPaths returns the file paths in order so the output is stable.
func sortedPaths(files map[string][]byte) []string {
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}

// pathIs reports whether rel matches one of the lower-cased names.
func pathIs(rel string, names ...string) bool {
	l := strings.ToLower(rel)
	for _, n := range names {
		if l == n {
			return true
		}
	}
	return false
}

// intake is everything the intake half read from the tree.
type intake struct {
	templates    []issueTemplate
	blankIssues  *bool
	labels       *labelIndex
	owners       owners
	checks       []workflowmodel.Check
	findings     []workflowmodel.Finding
	templateSeen bool
	// settingsRules are branch rules declared in .github/settings.yml,
	// used when the forge API gave none.
	settingsRules *BranchRules
}

// readIntake parses the intake files in the tree.
func readIntake(files map[string][]byte) *intake {
	in := &intake{labels: newLabelIndex()}
	for _, p := range sortedPaths(files) {
		data := files[p]
		if len(data) > MaxFileBytes {
			continue
		}
		switch {
		case isIssueTemplateConfig(p):
			in.blankIssues = parseTemplateConfig(data)
		case isIssueTemplatePath(p):
			in.templateSeen = true
			t, finding := parseIssueTemplate(p, data)
			if finding != nil {
				in.findings = append(in.findings, *finding)
				continue
			}
			in.templates = append(in.templates, t)
			for i, l := range t.Labels {
				in.labels.add(l, "", "", cite(p, t.LabelLines[i], l))
			}
		case isPRTemplatePath(p):
			in.checks = append(in.checks, parseChecklist(p, data)...)
		case pathIs(p, "codeowners", ".github/codeowners", "docs/codeowners"):
			in.owners.addCodeowners(p, data)
		case pathIs(p, "owners"):
			in.owners.addOwners(p, data)
		case pathIs(p, "owners_aliases"):
			in.owners.addAliases(p, data)
		case pathIs(p, "maintainers", "maintainers.md", ".github/maintainers.md", "docs/maintainers.md"):
			in.owners.addMaintainers(p, data)
		case pathIs(p, ".github/settings.yml"):
			parseSettingsLabels(p, data, in.labels)
			in.settingsRules = branchRulesFromSettings(p, data)
		case pathIs(p, ".github/labels.yml", ".github/labels.yaml"):
			parseLabelsFile(p, data, in.labels)
		case pathIs(p, ".github/labeler.yml", ".github/labeler.yaml"):
			parseLabelerFile(p, data, in.labels)
		case pathIs(p, "contributing.md", ".github/contributing.md", "docs/contributing.md"):
			parseContributing(p, data, in.labels)
		}
	}
	in.owners.resolve()
	return in
}

// empty reports whether nothing intake-related was found, so the
// lifecycle must stay byte-identical to the pipeline-only ingest.
func (in *intake) empty() bool {
	return !in.templateSeen && in.blankIssues == nil && in.labels.empty() && in.owners.empty() && len(in.checks) == 0 && len(in.findings) == 0 && in.settingsRules == nil
}

// applyIntake adds the intake half to m. It is a no-op when the tree has
// none of the inputs and rules is nil.
func applyIntake(m *workflowmodel.Model, in *intake, rules *BranchRules) {
	if in.empty() && rules == nil {
		return
	}
	if rules == nil {
		rules = in.settingsRules
	}
	m.Spec.Coverage.Findings = append(m.Spec.Coverage.Findings, in.findings...)
	if in.blankIssues != nil && !*in.blankIssues {
		if m.Metadata.Annotations == nil {
			m.Metadata.Annotations = map[string]string{}
		}
		m.Metadata.Annotations[AnnotationBlankIssues] = "disabled"
	}
	if in.templateSeen {
		switch m.Spec.Meta.Source {
		case "":
			m.Spec.Meta.Source = workflowmodel.SourceIssueTemplates
		default:
			m.Spec.Meta.Source += "+" + workflowmodel.SourceIssueTemplates
		}
	}

	triage := in.labels.triage()
	roles := applyOwners(m, in.owners)
	applyTriage(m, triage, roles)
	applyTemplates(m, in.templates, triage)
	applyConventionLabels(m, in.labels)
	applyClassificationLabels(m, in.labels)
	m.Spec.Checks = append(m.Spec.Checks, in.checks...)
	applyBranchRules(m, rules, roles)
}

// releaser is the human role that releases an inferred gate: the
// repository's approvers when an owners file names them, else the
// hive-default maintainer role.
func releaser(roles map[string]bool) string {
	if roles[RoleApprover] {
		return RoleApprover
	}
	return hivedefault.RoleMaintainer
}

// insertColumnAfter adds col to m right after the column with id after.
func insertColumnAfter(m *workflowmodel.Model, after string, col workflowmodel.Column) {
	for i, c := range m.Spec.Columns {
		if c.ID == after {
			cols := append([]workflowmodel.Column(nil), m.Spec.Columns[:i+1]...)
			cols = append(cols, col)
			m.Spec.Columns = append(cols, m.Spec.Columns[i+1:]...)
			return
		}
	}
	m.Spec.Columns = append(m.Spec.Columns, col)
}

// insertStateAfter adds st to m right after the state with id after.
func insertStateAfter(m *workflowmodel.Model, after string, st workflowmodel.State) {
	for i, s := range m.Spec.States {
		if s.ID == after {
			states := append([]workflowmodel.State(nil), m.Spec.States[:i+1]...)
			states = append(states, st)
			m.Spec.States = append(states, m.Spec.States[i+1:]...)
			return
		}
	}
	m.Spec.States = append(m.Spec.States, st)
}

// addLabel appends a label entry unless one with the same name exists;
// an existing entry without a state takes the new state.
func addLabel(m *workflowmodel.Model, e workflowmodel.LabelEntry) {
	for i := range m.Spec.Labels.Entries {
		if strings.EqualFold(m.Spec.Labels.Entries[i].Name, e.Name) {
			if m.Spec.Labels.Entries[i].State == "" {
				m.Spec.Labels.Entries[i].State = e.State
			}
			return
		}
	}
	m.Spec.Labels.Entries = append(m.Spec.Labels.Entries, e)
}

// admitted are the Merge queue states a PR reaches once review and checks
// are satisfied; needs-decision-pr is excluded because it parks a PR on a
// human decision, not on review.
var admitted = map[string]bool{
	hivedefault.StateID(stagelabels.AwaitingMergeAdmission): true,
	hivedefault.StateID(stagelabels.ReadyForHumanMerge):     true,
	hivedefault.StateID(stagelabels.PreMergeSoak):           true,
	hivedefault.StateID(stagelabels.MergeQueue):             true,
}

// mergeTransitions are the hive-default edges from the CI / Review column
// into the admitted Merge queue states: where a repository's review gate
// and required checks apply.
func mergeTransitions(m *workflowmodel.Model) []int {
	col := map[string]string{}
	for _, c := range m.Spec.Columns {
		for _, s := range c.States {
			col[s] = c.ID
		}
	}
	var idx []int
	for i, t := range m.Spec.Transitions {
		if col[t.From] == hivedefault.ColumnCIReview && admitted[t.To] {
			idx = append(idx, i)
		}
	}
	return idx
}

// citeAPI is the citation for a forge API reading.
func citeAPI(endpoint, text string) workflowmodel.Citation {
	return workflowmodel.Citation{Path: "api:" + endpoint, Text: text}
}

// cite is the citation for a file line.
func cite(p string, line int, text string) workflowmodel.Citation {
	return workflowmodel.Citation{Path: p, Line: line, Text: text}
}
