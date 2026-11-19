package ingest

import (
	"bytes"
	"fmt"
	"path"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/hivecommons/hive/pkg/workflowmodel"
	"github.com/hivecommons/hive/pkg/workflowmodel/hivedefault"
)

// issueTemplate is one issue form (YAML) or legacy template (Markdown
// with front matter) as GitHub reads it: the fields the intake needs.
// Title prefixes and assignees are not read; the schema has no slot for
// them.
type issueTemplate struct {
	Path string
	// Line is the line the template's name is declared on (1 for a file
	// with no name key).
	Line   int
	Name   string
	Labels []string
	// LabelLines are the lines each label is declared on, parallel to
	// Labels.
	LabelLines []int
	Form       bool
}

func isIssueTemplatePath(rel string) bool {
	if isIssueTemplateConfig(rel) {
		return false
	}
	if pathIs(rel, ".github/issue_template.md", "issue_template.md") {
		return true
	}
	rest, ok := strings.CutPrefix(strings.ToLower(rel), strings.ToLower(intakeDir))
	if !ok || rest == "" || strings.Contains(rest, "/") {
		return false
	}
	return isYAMLPath(rest) || path.Ext(rest) == ".md"
}

func isIssueTemplateConfig(rel string) bool {
	return pathIs(rel, ".github/issue_template/config.yml", ".github/issue_template/config.yaml")
}

func isPRTemplatePath(rel string) bool {
	if pathIs(rel, ".github/pull_request_template.md", "pull_request_template.md", "docs/pull_request_template.md") {
		return true
	}
	rest, ok := strings.CutPrefix(strings.ToLower(rel), ".github/pull_request_template/")
	return ok && rest != "" && !strings.Contains(rest, "/") && path.Ext(rest) == ".md"
}

// parseTemplateConfig reads blank_issues_enabled from
// .github/ISSUE_TEMPLATE/config.yml; nil when the key is absent.
func parseTemplateConfig(data []byte) *bool {
	var cfg struct {
		BlankIssuesEnabled *bool `yaml:"blank_issues_enabled"`
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil
	}
	return cfg.BlankIssuesEnabled
}

// parseIssueTemplate reads a form or front-matter template. A template
// GitHub would not load (no YAML, no name) is reported as a finding.
func parseIssueTemplate(p string, data []byte) (issueTemplate, *workflowmodel.Finding) {
	t := issueTemplate{Path: p, Line: 1}
	src := data
	offset := 0
	if path.Ext(strings.ToLower(p)) == ".md" {
		fm, off, ok := frontMatter(data)
		if !ok {
			return t, &workflowmodel.Finding{Path: p, Code: CodeTemplateUnparsable, Detail: "no front matter", Severity: workflowmodel.SeverityWarning}
		}
		src, offset = fm, off
	} else {
		t.Form = true
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil || doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		detail := "not a YAML mapping"
		if err != nil {
			detail = err.Error()
		}
		return t, &workflowmodel.Finding{Path: p, Code: CodeTemplateUnparsable, Detail: detail, Severity: workflowmodel.SeverityWarning}
	}
	root := doc.Content[0]
	for i := 0; i+1 < len(root.Content); i += 2 {
		k, v := root.Content[i], root.Content[i+1]
		switch strings.ToLower(k.Value) {
		case "name":
			t.Name = strings.TrimSpace(v.Value)
			t.Line = k.Line + offset
		case "labels":
			t.Labels, t.LabelLines = stringList(v, offset)
		}
	}
	if t.Name == "" {
		t.Name = strings.TrimSuffix(path.Base(p), path.Ext(p))
	}
	return t, nil
}

// stringList reads a YAML sequence of scalars or a comma-separated
// scalar (both are valid for labels and assignees) with the line each
// item is on.
func stringList(n *yaml.Node, offset int) ([]string, []int) {
	var out []string
	var lines []int
	add := func(s string, line int) {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
			lines = append(lines, line+offset)
		}
	}
	switch n.Kind {
	case yaml.SequenceNode:
		for _, c := range n.Content {
			if c.Kind == yaml.ScalarNode {
				add(c.Value, c.Line)
			}
		}
	case yaml.ScalarNode:
		for _, s := range strings.Split(n.Value, ",") {
			add(s, n.Line)
		}
	}
	return out, lines
}

// frontMatter returns the YAML between the leading --- fences and the
// line offset to add to YAML line numbers.
func frontMatter(data []byte) ([]byte, int, bool) {
	data = bytes.TrimLeft(data, "\ufeff")
	lines := bytes.Split(data, []byte("\n"))
	if len(lines) == 0 || strings.TrimSpace(string(lines[0])) != "---" {
		return nil, 0, false
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(string(lines[i])) == "---" {
			return bytes.Join(lines[1:i], []byte("\n")), 1, true
		}
	}
	return nil, 0, false
}

// parseChecklist turns a pull-request template's checklist items into
// manual checks, one per `- [ ]` line, cited by line.
func parseChecklist(p string, data []byte) []workflowmodel.Check {
	var out []workflowmodel.Check
	seen := map[string]bool{}
	for i, line := range strings.Split(string(data), "\n") {
		s := strings.TrimSpace(line)
		var text string
		for _, marker := range []string{"- [ ] ", "* [ ] ", "- [x] ", "* [x] ", "- [X] ", "* [X] "} {
			if rest, ok := strings.CutPrefix(s, marker); ok {
				text = strings.TrimSpace(rest)
				break
			}
		}
		if text == "" {
			continue
		}
		id := "pr-template-" + truncate(workflowmodel.Slug(text), maxCheckSlug)
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, workflowmodel.Check{ID: id, Name: text, Kind: workflowmodel.CheckManual, Source: fmt.Sprintf("%s:%d", p, i+1)})
	}
	return out
}

// maxCheckSlug bounds a checklist-derived check id.
const maxCheckSlug = 60

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.TrimRight(s[:n], "-")
}

// intakeState is one intake state: the templates that share a label set.
type intakeState struct {
	id, label string
	// bind is the label whose presence fires the intake event.
	bind      string
	labels    []string
	citations []workflowmodel.Citation
}

// planIntake groups templates by label set into intake states. The event
// label is the first template label that is not a triage label and is not
// already bound by another intake state; a template without such a label
// is merged into the state that bound it or reported, never guessed.
func planIntake(templates []issueTemplate, triage triageLabels) ([]intakeState, []workflowmodel.Finding) {
	var states []intakeState
	var findings []workflowmodel.Finding
	bySet := map[string]int{}
	byBind := map[string]int{}
	ids := map[string]bool{}
	for _, t := range templates {
		if len(t.Labels) == 0 {
			findings = append(findings, workflowmodel.Finding{Path: t.Path, Code: CodeTemplateNoLabels, Detail: fmt.Sprintf("template %q sets no labels; an issue filed with it cannot be told apart", t.Name), Severity: workflowmodel.SeverityUnknown})
			continue
		}
		key := labelSetKey(t.Labels)
		c := cite(t.Path, t.Line, t.Name)
		if i, ok := bySet[key]; ok {
			states[i].citations = append(states[i].citations, c)
			continue
		}
		bind := ""
		for _, l := range t.Labels {
			if triage.is(l) {
				continue
			}
			if _, used := byBind[normalizeLabel(l)]; used {
				continue
			}
			bind = l
			break
		}
		if bind == "" {
			// Every label is a triage label or already fires another
			// intake state: fold into the state sharing the first label.
			merged := false
			for _, l := range t.Labels {
				if i, ok := byBind[normalizeLabel(l)]; ok {
					states[i].citations = append(states[i].citations, c)
					findings = append(findings, workflowmodel.Finding{Path: t.Path, Code: CodeTemplateMerged, Detail: fmt.Sprintf("template %q shares label %q with %q", t.Name, l, states[i].label), Severity: workflowmodel.SeverityUnknown})
					merged = true
					break
				}
			}
			if !merged {
				findings = append(findings, workflowmodel.Finding{Path: t.Path, Code: CodeTemplateNoLabels, Detail: fmt.Sprintf("template %q sets only triage labels", t.Name), Severity: workflowmodel.SeverityUnknown})
			}
			continue
		}
		id := StateRequestedPrefix + workflowmodel.Slug(t.Name)
		for n := 2; ids[id]; n++ {
			id = fmt.Sprintf("%s%s-%d", StateRequestedPrefix, workflowmodel.Slug(t.Name), n)
		}
		ids[id] = true
		bySet[key] = len(states)
		byBind[normalizeLabel(bind)] = len(states)
		states = append(states, intakeState{id: id, label: "Requested: " + t.Name, bind: bind, labels: t.Labels, citations: []workflowmodel.Citation{c}})
	}
	return states, findings
}

func labelSetKey(labels []string) string {
	set := map[string]bool{}
	for _, l := range labels {
		set[normalizeLabel(l)] = true
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, "\x00")
}

// applyTemplates adds the Intake column and one state per intake plan,
// each entered from Open on its template event and left for Triage (when
// the repository has a triage label) or Actionable (when a stage label
// is applied).
func applyTemplates(m *workflowmodel.Model, templates []issueTemplate, triage triageLabels) {
	plan, findings := planIntake(templates, triage)
	m.Spec.Coverage.Findings = append(m.Spec.Coverage.Findings, findings...)
	if len(plan) == 0 {
		return
	}
	col := workflowmodel.Column{ID: ColumnIntake, Label: "Intake", Hint: "Filed through an issue template; not yet triaged"}
	prev := hivedefault.StateOpen
	for _, s := range plan {
		st := workflowmodel.State{
			ID: s.id, Label: s.label, Owner: workflowmodel.OwnerHuman, Origin: workflowmodel.OriginIngested,
			Lifecycle: workflowmodel.StateLifecycle{Column: ColumnIntake, Kinds: []workflowmodel.ItemKind{workflowmodel.KindIssue}, Confidence: workflowmodel.ConfidenceInferred, Citations: s.citations},
		}
		insertStateAfter(m, prev, st)
		prev = s.id
		col.States = append(col.States, s.id)
		ev := EventTemplatePrefix + strings.TrimPrefix(s.id, StateRequestedPrefix)
		m.Spec.Events = append(m.Spec.Events, workflowmodel.Event{
			ID: ev, Label: "Issue opened with template " + strings.TrimPrefix(s.label, "Requested: "),
			Fact:       workflowmodel.Predicate{Fact: "labels", Op: workflowmodel.OpHas, Value: s.bind},
			Confidence: workflowmodel.ConfidenceInferred, Citations: s.citations,
		})
		m.Spec.Transitions = append(m.Spec.Transitions, workflowmodel.Transition{From: hivedefault.StateOpen, To: s.id, On: ev, Note: "issue template applied label " + s.bind})
		if triage.needs != "" {
			m.Spec.Transitions = append(m.Spec.Transitions, workflowmodel.Transition{From: s.id, To: StateTriage, On: EventTriageRequested, Note: "template carries the triage label"})
		} else {
			m.Spec.Transitions = append(m.Spec.Transitions, workflowmodel.Transition{From: s.id, To: hivedefault.StateID(stageActionable), On: hivedefault.EventStageChange, Note: "staged by hive"})
		}
		m.Spec.Transitions = append(m.Spec.Transitions, workflowmodel.Transition{From: s.id, To: hivedefault.StateClosed, On: hivedefault.EventClosed})
		for _, l := range s.labels {
			e := workflowmodel.LabelEntry{Name: l}
			if normalizeLabel(l) == normalizeLabel(s.bind) {
				e.State = s.id
			}
			addLabel(m, e)
		}
	}
	insertColumnAfter(m, hivedefault.ColumnOpen, col)
}
