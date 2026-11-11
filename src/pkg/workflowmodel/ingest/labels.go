package ingest

import (
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/workflowmodel"
	"github.com/hivecommons/hive/pkg/workflowmodel/hivedefault"
)

// stageActionable is the hive-default stage intake states hand over to.
const stageActionable = stagelabels.Actionable

// knownLabel is a label the repository is known to use and where it was
// first seen.
type knownLabel struct {
	Name        string
	Description string
	Color       string
	Cite        workflowmodel.Citation
}

// labelIndex collects labels from every source, keyed by normalized name,
// first citation wins (files are read in sorted path order).
type labelIndex struct {
	byName map[string]knownLabel
}

func newLabelIndex() *labelIndex { return &labelIndex{byName: map[string]knownLabel{}} }

func (x *labelIndex) add(name, description, color string, c workflowmodel.Citation) {
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	k := normalizeLabel(name)
	if prev, ok := x.byName[k]; ok {
		// First sighting keeps the citation; a later label-sync entry
		// may still supply the description and colour.
		if prev.Description == "" {
			prev.Description = description
		}
		if prev.Color == "" {
			prev.Color = color
		}
		x.byName[k] = prev
		return
	}
	x.byName[k] = knownLabel{Name: name, Description: description, Color: color, Cite: c}
}

func (x *labelIndex) empty() bool { return len(x.byName) == 0 }

// get returns the known label for any of the normalized names, in the
// order the names are listed (the table's preferred spelling first).
func (x *labelIndex) get(names ...string) (knownLabel, bool) {
	for _, n := range names {
		if l, ok := x.byName[n]; ok {
			return l, true
		}
	}
	return knownLabel{}, false
}

// normalizeLabel lowercases and unifies separators so `Needs Triage`,
// `needs_triage` and `needs-triage` compare equal.
func normalizeLabel(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, ": ", "/")
	s = strings.ReplaceAll(s, ":", "/")
	s = strings.NewReplacer(" ", "-", "_", "-").Replace(s)
	return s
}

// Triage label table. Matching is exact on the normalized name; a label
// outside the table is not a triage label. Spellings are those seen on
// GitHub's own, Kubernetes', CNCF's and the common label-sync presets.
var (
	triageNeedsNames    = []string{"needs-triage", "needs/triage", "triage-needed", "triage/needed", "status/needs-triage", "status/triage", "triage/needs-triage", "untriaged", "triage"}
	triageAcceptedNames = []string{"triage/accepted", "triage-accepted", "triaged", "status/triaged"}
	triageDeclinedNames = []string{"triage/declined", "triage-declined", "triage/rejected", "triage/not-planned"}
	// conventionNames are community labels worth carrying in the label
	// set but that mark no state.
	conventionNames = []string{"good-first-issue", "help-wanted"}
	// classificationPrefixes are the label families triage applies to
	// classify an issue (kind/bug, priority/P1, area/docs ...). They mark
	// no state either; the model carries them so the triage gate's
	// vocabulary is visible in the editor.
	classificationPrefixes = []string{"kind/", "priority/", "type/", "area/", "severity/", "size/"}
)

// isClassificationLabel reports whether the normalized label n belongs
// to one of the classification families.
func isClassificationLabel(n string) bool {
	for _, p := range classificationPrefixes {
		if rest, ok := strings.CutPrefix(n, p); ok && rest != "" {
			return true
		}
	}
	return false
}

// triageLabels are the repository's triage labels, if any (empty names
// when absent), with their citations.
type triageLabels struct {
	needs, accepted, declined string
	cites                     []workflowmodel.Citation
	cite                      map[string][]workflowmodel.Citation
	descriptions              map[string]string
}

// is reports whether l is one of the repository's triage labels.
func (t triageLabels) is(l string) bool {
	n := normalizeLabel(l)
	return n != "" && (n == normalizeLabel(t.needs) || n == normalizeLabel(t.accepted) || n == normalizeLabel(t.declined))
}

// triage finds the triage labels in the index.
func (x *labelIndex) triage() triageLabels {
	var t triageLabels
	t.descriptions = map[string]string{}
	t.cite = map[string][]workflowmodel.Citation{}
	pick := func(names []string) string {
		l, ok := x.get(names...)
		if !ok {
			return ""
		}
		t.cites = append(t.cites, l.Cite)
		t.cite[l.Name] = []workflowmodel.Citation{l.Cite}
		t.descriptions[l.Name] = l.Description
		return l.Name
	}
	t.needs = pick(triageNeedsNames)
	if t.needs == "" {
		return triageLabels{}
	}
	t.accepted = pick(triageAcceptedNames)
	t.declined = pick(triageDeclinedNames)
	return t
}

// parseSettingsLabels reads labels[].name from a probot settings.yml.
func parseSettingsLabels(p string, data []byte, x *labelIndex) {
	root := yamlRoot(data)
	if root == nil {
		return
	}
	if labels := mappingValue(root, "labels"); labels != nil {
		addLabelSequence(p, labels, x)
	}
}

// parseLabelsFile reads a label-sync file: either a sequence of
// {name, description, color} or a mapping of name -> {description, color}
// (github-label-sync, EndBug/label-sync and crazy-max/ghaction-github-labeler
// all use one of the two).
func parseLabelsFile(p string, data []byte, x *labelIndex) {
	root := yamlRoot(data)
	if root == nil {
		return
	}
	switch root.Kind {
	case yaml.SequenceNode:
		addLabelSequence(p, root, x)
	case yaml.MappingNode:
		if labels := mappingValue(root, "labels"); labels != nil {
			addLabelSequence(p, labels, x)
			return
		}
		for i := 0; i+1 < len(root.Content); i += 2 {
			k, v := root.Content[i], root.Content[i+1]
			desc, color := "", ""
			if v.Kind == yaml.MappingNode {
				desc = scalar(mappingValue(v, "description"))
				color = scalar(mappingValue(v, "color"))
			}
			x.add(k.Value, desc, color, cite(p, k.Line, k.Value))
		}
	}
}

// parseLabelerFile reads actions/labeler's config: top-level keys are
// the labels it applies.
func parseLabelerFile(p string, data []byte, x *labelIndex) {
	root := yamlRoot(data)
	if root == nil || root.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		k := root.Content[i]
		x.add(k.Value, "", "", cite(p, k.Line, k.Value))
	}
}

// addLabelSequence reads a sequence of label objects or bare names.
func addLabelSequence(p string, seq *yaml.Node, x *labelIndex) {
	if seq.Kind != yaml.SequenceNode {
		return
	}
	for _, it := range seq.Content {
		switch it.Kind {
		case yaml.ScalarNode:
			x.add(it.Value, "", "", cite(p, it.Line, it.Value))
		case yaml.MappingNode:
			name := mappingValue(it, "name")
			if name == nil {
				continue
			}
			x.add(name.Value, scalar(mappingValue(it, "description")), scalar(mappingValue(it, "color")), cite(p, name.Line, name.Value))
		}
	}
}

// backticked matches `label` mentions in prose.
var backticked = regexp.MustCompile("`([^`\n]{1,60})`")

// parseContributing reads label names a CONTRIBUTING file mentions: any
// backticked token that is a table label, or a hyphenated/slashed table
// label written plainly. Plain single words (triage, triaged) are not
// matched: prose uses them without naming a label.
func parseContributing(p string, data []byte, x *labelIndex) {
	table := append(append(append(append([]string(nil), triageNeedsNames...), triageAcceptedNames...), triageDeclinedNames...), conventionNames...)
	for i, line := range strings.Split(string(data), "\n") {
		for _, m := range backticked.FindAllStringSubmatch(line, -1) {
			n := normalizeLabel(m[1])
			if isClassificationLabel(n) && !strings.ContainsAny(m[1], " *<>") {
				x.add(m[1], "", "", cite(p, i+1, m[0]))
			}
			for _, t := range table {
				if n == t {
					x.add(m[1], "", "", cite(p, i+1, m[0]))
				}
			}
		}
		low := strings.ToLower(line)
		for _, t := range table {
			if !strings.ContainsAny(t, "-/") {
				continue
			}
			if idx := strings.Index(low, t); idx >= 0 && boundary(low, idx, len(t)) {
				x.add(line[idx:idx+len(t)], "", "", cite(p, i+1, t))
			}
		}
	}
}

// boundary reports whether s[i:i+n] is delimited by non-word characters.
func boundary(s string, i, n int) bool {
	word := func(c byte) bool {
		return c == '-' || c == '/' || c == '_' || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
	}
	return (i == 0 || !word(s[i-1])) && (i+n >= len(s) || !word(s[i+n]))
}

// applyTriage adds the Triage column and state when the repository has a
// triage label: Open and every intake state enter it when the label is
// applied; a triage decision (gate) accepts it to Actionable or declines
// it to Closed.
func applyTriage(m *workflowmodel.Model, t triageLabels, roles map[string]bool) {
	if t.needs == "" {
		return
	}
	hint := "Carries " + t.needs
	if t.accepted != "" {
		hint += "; accepted with " + t.accepted
	}
	if t.declined != "" {
		hint += ", declined with " + t.declined
	}
	st := workflowmodel.State{
		ID: StateTriage, Label: "Triage", Owner: workflowmodel.OwnerHuman, Origin: workflowmodel.OriginIngested,
		Lifecycle: workflowmodel.StateLifecycle{Column: ColumnTriage, Kinds: []workflowmodel.ItemKind{workflowmodel.KindIssue}, Waiting: []string{"awaiting triage"}, Confidence: workflowmodel.ConfidenceInferred, Citations: t.cites},
	}
	insertStateAfter(m, hivedefault.StateOpen, st)
	insertColumnAfter(m, hivedefault.ColumnOpen, workflowmodel.Column{ID: ColumnTriage, Label: "Triage", Hint: hint, States: []string{StateTriage}})

	m.Spec.Events = append(m.Spec.Events, workflowmodel.Event{
		ID: EventTriageRequested, Label: "Triage label applied",
		Fact:       workflowmodel.Predicate{Fact: "labels", Op: workflowmodel.OpHas, Value: t.needs},
		Confidence: workflowmodel.ConfidenceInferred, Citations: t.cite[t.needs],
	})
	accepted := workflowmodel.Event{ID: EventTriageAccepted, Label: "Triage label removed", Fact: workflowmodel.Predicate{Fact: "labels", Op: workflowmodel.OpLacks, Value: t.needs}, Confidence: workflowmodel.ConfidenceInferred, Citations: t.cite[t.needs]}
	if t.accepted != "" {
		accepted.Label = "Triage accepted"
		accepted.Fact = workflowmodel.Predicate{Fact: "labels", Op: workflowmodel.OpHas, Value: t.accepted}
		accepted.Citations = t.cite[t.accepted]
	}
	m.Spec.Events = append(m.Spec.Events, accepted)
	gate := workflowmodel.Gate{ID: GateTriageDecision, ReleasedBy: releaser(roles), Confidence: workflowmodel.ConfidenceInferred, Citations: t.cites}
	if t.accepted != "" {
		gate.Reply = "/label " + t.accepted
	}
	m.Spec.Gates = append(m.Spec.Gates, gate)

	m.Spec.Transitions = append(m.Spec.Transitions,
		workflowmodel.Transition{From: hivedefault.StateOpen, To: StateTriage, On: EventTriageRequested, Note: "label-added " + t.needs},
		workflowmodel.Transition{From: StateTriage, To: hivedefault.StateID(stageActionable), On: EventTriageAccepted, GateRef: GateTriageDecision, Note: "triage accepted"},
		workflowmodel.Transition{From: StateTriage, To: hivedefault.StateClosed, On: hivedefault.EventClosed},
	)
	addLabel(m, workflowmodel.LabelEntry{Name: t.needs, Description: t.descriptions[t.needs], State: StateTriage})
	if t.accepted != "" {
		addLabel(m, workflowmodel.LabelEntry{Name: t.accepted, Description: t.descriptions[t.accepted], State: hivedefault.StateID(stageActionable)})
	}
	if t.declined != "" {
		m.Spec.Events = append(m.Spec.Events, workflowmodel.Event{
			ID: EventTriageDeclined, Label: "Triage declined",
			Fact:       workflowmodel.Predicate{Fact: "labels", Op: workflowmodel.OpHas, Value: t.declined},
			Confidence: workflowmodel.ConfidenceInferred, Citations: t.cite[t.declined],
		})
		m.Spec.Transitions = append(m.Spec.Transitions, workflowmodel.Transition{From: StateTriage, To: hivedefault.StateClosed, On: EventTriageDeclined, GateRef: GateTriageDecision, Note: "triage declined"})
		addLabel(m, workflowmodel.LabelEntry{Name: t.declined, Description: t.descriptions[t.declined], State: hivedefault.StateClosed})
	}
}

// applyConventionLabels carries good-first-issue / help-wanted style
// labels into the label set (no state) when the repository declares them.
func applyConventionLabels(m *workflowmodel.Model, x *labelIndex) {
	for _, n := range conventionNames {
		if l, ok := x.get(n); ok {
			addLabel(m, workflowmodel.LabelEntry{Name: l.Name, Description: l.Description, Color: l.Color})
		}
	}
}

// applyClassificationLabels carries the kind/*, priority/* ... labels the
// repository declares into the label set (no state), sorted by name.
// Labels an intake state already bound keep that state (addLabel).
func applyClassificationLabels(m *workflowmodel.Model, x *labelIndex) {
	var names []string
	for n := range x.byName {
		if isClassificationLabel(n) {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		l := x.byName[n]
		addLabel(m, workflowmodel.LabelEntry{Name: l.Name, Description: l.Description, Color: l.Color})
	}
}

// yamlRoot parses data and returns the document's root node, or nil.
func yamlRoot(data []byte) *yaml.Node {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil || doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil
	}
	return doc.Content[0]
}

// mappingValue returns the value node for key in a mapping, or nil.
func mappingValue(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if strings.EqualFold(n.Content[i].Value, key) {
			return n.Content[i+1]
		}
	}
	return nil
}

func scalar(n *yaml.Node) string {
	if n == nil || n.Kind != yaml.ScalarNode {
		return ""
	}
	return strings.TrimSpace(n.Value)
}
