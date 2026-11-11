package spec

import (
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/workflowmodel"
)

// Mark enforces the spec-ingest guarantees on a model regardless of which
// stage produced it: identity fields, provenance, inferred confidence on
// every state, event and gate, ingested origin, citations that point at a
// real section, and no elements a spec cannot vouch for (pipelines,
// sources, agent grants). It returns the number of citations it had to
// replace with the whole-document reference.
func Mark(m *workflowmodel.Model, origin, name string, scope workflowmodel.Scope, secs []Section) int {
	m.APIVersion = workflowmodel.APIVersion
	m.Kind = workflowmodel.KindWorkflowModel
	if name != "" {
		m.Metadata.Name = name
	}
	if m.Metadata.Name == "" {
		m.Metadata.Name = workflowmodel.Slug(firstSection(secs).Heading)
	}
	if m.Metadata.Annotations == nil {
		m.Metadata.Annotations = map[string]string{}
	}
	m.Metadata.Annotations[AnnotationSource] = "spec:" + origin
	m.Spec.Scope = scope
	if m.Spec.Meta.Version == "" {
		m.Spec.Meta.Version = "1"
	}
	m.Spec.Meta.Source = workflowmodel.SourceSpec
	m.Spec.Meta.Provenance = workflowmodel.ProvenanceIngestedUnreviewed
	m.Spec.Pipelines = nil
	m.Spec.Sources = nil
	m.Spec.Agents = nil
	for i := range m.Spec.States {
		m.Spec.States[i].Pipelines = nil
	}
	if m.Spec.Derivation.Mode == "" {
		m.Spec.Derivation.Mode = workflowmodel.DerivationBuiltin
	}

	known := headings(secs)
	whole := workflowmodel.Citation{Path: firstSection(secs).Ref(), Line: firstSection(secs).StartLine}
	if len(secs) > 1 {
		whole = workflowmodel.Citation{Path: "spec:" + origin + "#L1-L" + strconv.Itoa(secs[len(secs)-1].EndLine)}
	}
	replaced := 0
	fix := func(cs []workflowmodel.Citation) []workflowmodel.Citation {
		var kept []workflowmodel.Citation
		for _, c := range cs {
			if sec, ok := citedSection(c.Path, known); ok {
				if c.Line < sec.StartLine || c.Line > sec.EndLine {
					c.Line = sec.StartLine
				}
				kept = append(kept, c)
			}
		}
		if len(kept) == 0 {
			replaced++
			kept = []workflowmodel.Citation{whole}
		}
		return kept
	}
	var walk func(states []workflowmodel.State)
	walk = func(states []workflowmodel.State) {
		for i := range states {
			s := &states[i]
			s.Origin = workflowmodel.OriginIngested
			s.Lifecycle.Confidence = workflowmodel.ConfidenceInferred
			s.Lifecycle.Citations = fix(s.Lifecycle.Citations)
			walk(s.Regions)
		}
	}
	walk(m.Spec.States)
	for i := range m.Spec.Events {
		m.Spec.Events[i].Confidence = workflowmodel.ConfidenceInferred
		m.Spec.Events[i].Citations = fix(m.Spec.Events[i].Citations)
	}
	for i := range m.Spec.Gates {
		m.Spec.Gates[i].Confidence = workflowmodel.ConfidenceInferred
		m.Spec.Gates[i].Citations = fix(m.Spec.Gates[i].Citations)
	}
	for i := range m.Spec.Guards {
		if _, ok := citedSection(m.Spec.Guards[i].Source, known); !ok {
			m.Spec.Guards[i].Source = whole.Path
		}
	}
	if m.Spec.Coverage.Files == 0 {
		m.Spec.Coverage.Files = 1
		m.Spec.Coverage.Parsed = 1
	}
	return replaced
}

// citedSection resolves a spec:<heading>#L.. path to its section.
func citedSection(path string, known map[string]Section) (Section, bool) {
	if !strings.HasPrefix(path, "spec:") {
		return Section{}, false
	}
	heading := strings.TrimPrefix(path, "spec:")
	if i := strings.LastIndex(heading, "#L"); i >= 0 {
		heading = heading[:i]
	}
	sec, ok := known[heading]
	return sec, ok
}
